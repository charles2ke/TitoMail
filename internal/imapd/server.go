// Package imapd implements the TitoMail IMAP server on top of go-imap's
// imapserver package and the TitoMail store.
package imapd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/charles2ke/TitoMail/internal/auth"
	"github.com/charles2ke/TitoMail/internal/store"
)

// DefaultMaxMessageBytes is the APPEND size limit used when
// Options.MaxMessageBytes is not set.
const DefaultMaxMessageBytes = 50 << 20

// Options configures a Server.
type Options struct {
	// TLSConfig enables STARTTLS on plain listeners and is required by
	// ServeTLS. nil disables STARTTLS.
	TLSConfig *tls.Config
	// AllowInsecureAuth allows LOGIN/AUTHENTICATE over unencrypted
	// connections (development and tests only). Without it, plaintext
	// connections must use STARTTLS before authenticating.
	AllowInsecureAuth bool
	// Logger receives diagnostics. nil discards them.
	Logger *slog.Logger
	// MaxMessageBytes limits the size of appended messages (advertised as
	// APPENDLIMIT). 0 means DefaultMaxMessageBytes.
	MaxMessageBytes int64
	// Auth verifies passwords and throttles guessing. It should be shared
	// with the other protocols so that limits apply across all of them.
	// nil creates a private one.
	Auth *auth.Authenticator
}

// Server is an IMAP server backed by a store.Store.
type Server struct {
	st          *store.Store
	opts        Options
	maxBytes    int64
	log         *slog.Logger
	imap        *imapserver.Server
	unsubscribe func()

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	closed   bool
	selected map[int64]map[*mailboxView]struct{} // mailbox ID -> views
	sessions sync.WaitGroup
}

// New creates a Server. It subscribes to the store's change stream so that
// selected mailboxes are kept up to date; call Close to release it.
func New(st *store.Store, opts Options) *Server {
	maxBytes := opts.MaxMessageBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxMessageBytes
	}
	if maxBytes > math.MaxUint32 {
		maxBytes = math.MaxUint32
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if opts.Auth == nil {
		opts.Auth = auth.New(st)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		st:       st,
		opts:     opts,
		maxBytes: maxBytes,
		log:      logger,
		ctx:      ctx,
		cancel:   cancel,
		selected: make(map[int64]map[*mailboxView]struct{}),
	}
	s.imap = imapserver.New(&imapserver.Options{
		NewSession: s.newSession,
		Caps: imap.CapSet{
			imap.CapIMAP4rev1:    {},
			imap.CapIMAP4rev2:    {},
			imap.CapNamespace:    {},
			imap.CapUIDPlus:      {},
			imap.CapESearch:      {},
			imap.CapSearchRes:    {},
			imap.CapListExtended: {},
			imap.CapListStatus:   {},
			imap.CapMove:         {},
			imap.CapStatusSize:   {},
			imap.CapBinary:       {},
			imap.CapChildren:     {},
			imap.CapSpecialUse:   {},
		},
		Logger:       logAdapter{logger},
		TLSConfig:    opts.TLSConfig,
		InsecureAuth: opts.AllowInsecureAuth,
	})
	s.unsubscribe = st.Subscribe(s.onChange)
	return s
}

// Serve accepts plaintext connections (with STARTTLS if a TLS config was
// provided) on ln until Close is called.
func (s *Server) Serve(ln net.Listener) error {
	return s.imap.Serve(ln)
}

// ServeTLS accepts implicit-TLS connections (IMAPS) on ln, which must be a
// plain TCP listener; it is wrapped using Options.TLSConfig.
func (s *Server) ServeTLS(ln net.Listener) error {
	if s.opts.TLSConfig == nil {
		return errors.New("imapd: ServeTLS requires Options.TLSConfig")
	}
	return s.imap.Serve(tls.NewListener(ln, s.opts.TLSConfig))
}

// Close stops all listeners, disconnects clients and waits (briefly) for
// their sessions to finish.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	s.cancel()
	err := s.imap.Close()
	if errors.Is(err, net.ErrClosed) {
		err = nil
	}
	done := make(chan struct{})
	go func() {
		s.sessions.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		s.log.Warn("imap: timed out waiting for sessions to close")
	}
	s.unsubscribe()
	return err
}

func (s *Server) newSession(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, &imap.Error{Type: imap.StatusResponseTypeBye, Text: "Server is shutting down"}
	}
	s.sessions.Add(1)
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(s.ctx)
	sess := &session{srv: s, conn: conn, ctx: ctx, cancel: cancel}
	if nc := conn.NetConn(); nc != nil {
		sess.remote = nc.RemoteAddr().String()
	}
	return sess, nil, nil
}

// register makes v receive change notifications. It must be called while
// holding the store write lock (inside store.Locked) so that the snapshot
// the view was initialized from lines up with the change stream.
func (s *Server) register(v *mailboxView) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.selected[v.id]
	if m == nil {
		m = make(map[*mailboxView]struct{})
		s.selected[v.id] = m
	}
	m[v] = struct{}{}
}

func (s *Server) unregister(v *mailboxView) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.selected[v.id]; m != nil {
		delete(m, v)
		if len(m) == 0 {
			delete(s.selected, v.id)
		}
	}
}

// onChange is the store change listener. It runs synchronously while the
// store holds its write lock, so it must be fast and must not call into
// the store.
func (s *Server) onChange(c store.Change) {
	var kind updateKind
	switch c.Kind {
	case store.MessagesAdded:
		kind = updAdded
	case store.MessagesRemoved:
		kind = updRemoved
	case store.FlagsChanged:
		kind = updFlags
	case store.MailboxesChanged:
		s.mu.Lock()
		for _, views := range s.selected {
			for v := range views {
				if v.userID == c.UserID {
					v.markRecheck()
				}
			}
		}
		s.mu.Unlock()
		return
	default:
		return
	}
	if len(c.UIDs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	views := s.selected[c.MailboxID]
	if len(views) == 0 {
		return
	}
	tok, _ := c.Source.(*sourceToken)
	var u *update
	for v := range views {
		if v.userID != c.UserID {
			continue
		}
		if u == nil {
			u = newUpdate(kind, c)
		}
		o := originOther
		if tok != nil && tok.view == v {
			o = originOwnReported
			if tok.silent {
				o = originOwnSilent
			}
		}
		v.push(queuedUpdate{u: u, origin: o})
	}
}

func newUpdate(kind updateKind, c store.Change) *update {
	u := &update{kind: kind, uids: append([]uint32(nil), c.UIDs...)}
	if !sort.SliceIsSorted(u.uids, func(i, j int) bool { return u.uids[i] < u.uids[j] }) {
		sort.Slice(u.uids, func(i, j int) bool { return u.uids[i] < u.uids[j] })
	}
	if kind == updFlags {
		u.flags = make(map[uint32][]imap.Flag, len(c.Flags))
		for uid, fl := range c.Flags {
			u.flags[uid] = toIMAPFlags(fl)
		}
	}
	return u
}

// logAdapter routes imapserver's Printf-style logging to slog.
type logAdapter struct{ l *slog.Logger }

func (a logAdapter) Printf(format string, args ...any) {
	msg := strings.TrimSpace(fmt.Sprintf(format, args...))
	level := slog.LevelDebug
	switch {
	case strings.HasPrefix(format, "panic"):
		level = slog.LevelError
	case strings.HasPrefix(format, "handling "),
		strings.HasPrefix(format, "accept error"),
		strings.HasPrefix(format, "failed to create session"),
		strings.HasPrefix(format, "failed to close session"):
		level = slog.LevelWarn
	}
	a.l.Log(context.Background(), level, "imap: "+msg)
}
