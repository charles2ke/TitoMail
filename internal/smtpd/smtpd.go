// Package smtpd implements the inbound SMTP server (MX) that accepts mail
// for local users, and the authenticated submission service that mail
// clients such as Outlook and Thunderbird use to send mail.
package smtpd

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-smtp"
	"golang.org/x/net/netutil"

	"github.com/charles2ke/TitoMail/internal/auth"
	"github.com/charles2ke/TitoMail/internal/delivery"
	"github.com/charles2ke/TitoMail/internal/store"
)

// MaxRecipients is the maximum number of recipients per message.
const MaxRecipients = 100

// maxConnections limits simultaneous connections per listener.
const maxConnections = 500

// maxHops is the number of Received fields after which a message is
// assumed to be looping.
const maxHops = 50

// Options configures a Server.
type Options struct {
	// Hostname is announced in greetings and Received fields.
	Hostname string
	// TLSConfig enables STARTTLS and implicit TLS.
	TLSConfig *tls.Config
	// AllowInsecureAuth permits AUTH on connections without TLS.
	AllowInsecureAuth bool
	MaxMessageBytes   int64
	// AuthChecks enables SPF, DKIM and DMARC checks on inbound mail.
	AuthChecks bool
	// Resolver defaults to the deliverer's resolver.
	Resolver delivery.Resolver
	Logger   *slog.Logger
}

// Server serves SMTP.
type Server struct {
	st   *store.Store
	d    *delivery.Deliverer
	auth *auth.Authenticator
	opts Options
	log  *slog.Logger
	res  delivery.Resolver
	now  func() time.Time

	mx  *smtp.Server
	sub *smtp.Server
}

// New returns a Server.
func New(st *store.Store, d *delivery.Deliverer, a *auth.Authenticator, opts Options) *Server {
	if opts.Hostname == "" {
		opts.Hostname = "localhost"
	}
	if opts.MaxMessageBytes <= 0 {
		opts.MaxMessageBytes = 50 << 20
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Resolver == nil {
		opts.Resolver = d.Resolver()
	}
	s := &Server{st: st, d: d, auth: a, opts: opts, log: opts.Logger, res: opts.Resolver, now: time.Now}
	s.mx = s.newSMTPServer(func(c *smtp.Conn) (smtp.Session, error) { return &mxSession{s: s, conn: c}, nil })
	s.sub = s.newSMTPServer(func(c *smtp.Conn) (smtp.Session, error) { return &subSession{s: s, conn: c}, nil })
	s.sub.AllowInsecureAuth = opts.AllowInsecureAuth
	return s
}

func (s *Server) newSMTPServer(f smtp.BackendFunc) *smtp.Server {
	srv := smtp.NewServer(f)
	srv.Domain = s.opts.Hostname
	srv.TLSConfig = s.opts.TLSConfig
	srv.MaxMessageBytes = s.opts.MaxMessageBytes
	srv.MaxRecipients = MaxRecipients
	srv.ReadTimeout = 10 * time.Minute
	srv.WriteTimeout = 2 * time.Minute
	srv.EnableSMTPUTF8 = true
	srv.ErrorLog = log.New(io.Discard, "", 0)
	return srv
}

// ServeMX accepts inbound mail for local users on ln (port 25).
func (s *Server) ServeMX(ln net.Listener) error {
	return s.mx.Serve(netutil.LimitListener(ln, maxConnections))
}

// ServeSubmission serves message submission with STARTTLS on ln (port 587).
func (s *Server) ServeSubmission(ln net.Listener) error {
	return s.sub.Serve(netutil.LimitListener(ln, maxConnections))
}

// ServeSubmissions serves message submission over implicit TLS on ln
// (port 465).
func (s *Server) ServeSubmissions(ln net.Listener) error {
	if s.opts.TLSConfig == nil {
		return errors.New("smtpd: implicit TLS requires a TLS configuration")
	}
	return s.sub.Serve(tls.NewListener(netutil.LimitListener(ln, maxConnections), s.opts.TLSConfig))
}

// Shutdown stops accepting connections and waits for active ones to end.
func (s *Server) Shutdown(ctx context.Context) error {
	return errors.Join(s.mx.Shutdown(ctx), s.sub.Shutdown(ctx))
}

// Close closes all listeners and connections immediately.
func (s *Server) Close() error {
	return errors.Join(s.mx.Close(), s.sub.Close())
}

func smtpErr(code int, enh smtp.EnhancedCode, msg string) *smtp.SMTPError {
	return &smtp.SMTPError{Code: code, EnhancedCode: enh, Message: msg}
}

var (
	errTempFailure = smtpErr(451, smtp.EnhancedCode{4, 3, 0}, "Temporary server error, please try again later")
	errBadAddress  = smtpErr(501, smtp.EnhancedCode{5, 1, 3}, "Bad recipient address syntax")
)

func newTraceID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func remoteIP(c *smtp.Conn) net.IP {
	if a, ok := c.Conn().RemoteAddr().(*net.TCPAddr); ok {
		return a.IP
	}
	host, _, err := net.SplitHostPort(c.Conn().RemoteAddr().String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

// tlsDescription describes the TLS parameters of a connection for the
// Received field, or returns "" for plain connections.
func tlsDescription(c *smtp.Conn) string {
	st, ok := c.TLSConnectionState()
	if !ok {
		return ""
	}
	return fmt.Sprintf("(version=%s cipher=%s)", tls.VersionName(st.Version), tls.CipherSuiteName(st.CipherSuite))
}

// receivedDate formats the timestamp of a Received field.
func receivedDate(t time.Time) string {
	return t.Format("Mon, 02 Jan 2006 15:04:05 -0700")
}

// cleanHelo makes the HELO argument safe to include in a header.
func cleanHelo(h string) string {
	h = strings.Map(func(r rune) rune {
		if r <= ' ' || r > '~' || r == '(' || r == ')' || r == ';' {
			return -1
		}
		return r
	}, h)
	if len(h) > 255 {
		h = h[:255]
	}
	if h == "" {
		return "unknown"
	}
	return h
}
