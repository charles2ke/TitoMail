package imapd

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/charles2ke/TitoMail/internal/auth"
	"github.com/charles2ke/TitoMail/internal/store"
)

// loginFailureDelay slows down password guessing.
var loginFailureDelay = time.Second

var (
	systemFlags = []imap.Flag{imap.FlagSeen, imap.FlagAnswered, imap.FlagFlagged, imap.FlagDeleted, imap.FlagDraft}
	permFlags   = append(append([]imap.Flag(nil), systemFlags...), imap.FlagWildcard)
)

type session struct {
	srv    *Server
	conn   *imapserver.Conn
	remote string
	ctx    context.Context
	cancel context.CancelFunc

	// mu serializes command execution against IDLE polling and Close.
	// Lock order: mu -> store write lock -> Server.mu -> mailboxView.qmu.
	mu     sync.Mutex
	closed bool
	user   *store.User
	view   *mailboxView
}

var (
	_ imapserver.SessionIMAP4rev2   = (*session)(nil)
	_ imapserver.SessionAppendLimit = (*session)(nil)
)

func (s *session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.releaseView()
	s.cancel()
	s.srv.sessions.Done()
	return nil
}

func (s *session) releaseView() {
	if s.view != nil {
		s.srv.unregister(s.view)
		s.view = nil
	}
}

func (s *session) Login(username, password string) error {
	u, err := s.srv.opts.Auth.Login(s.ctx, s.remote, username, password)
	if errors.Is(err, auth.ErrThrottled) {
		s.srv.log.Warn("imap: login throttled", "user", username, "remote", s.remote)
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeUnavailable,
			Text: "Too many failed login attempts, try again later"}
	}
	if errors.Is(err, store.ErrInvalidCredentials) {
		s.srv.log.Info("imap: login failed", "user", username, "remote", s.remote)
		t := time.NewTimer(loginFailureDelay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-s.ctx.Done():
		}
		return imapserver.ErrAuthFailed
	} else if err != nil {
		return s.storeErr(err)
	}
	s.mu.Lock()
	s.user = u
	s.mu.Unlock()
	s.srv.log.Debug("imap: login", "user", u.Email, "remote", s.remote)
	return nil
}

func (s *session) userID() int64 {
	return s.user.ID
}

func (s *session) Select(name string, options *imap.SelectOptions) (*imap.SelectData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseView()
	readOnly := options != nil && options.ReadOnly

	var (
		mb     *store.Mailbox
		status *store.MailboxStatus
		view   *mailboxView
	)
	err := s.srv.st.Locked(func() error {
		var err error
		mb, err = s.mailboxByName(name)
		if err != nil {
			return err
		}
		uids, err := s.srv.st.MailboxUIDs(s.ctx, mb.ID)
		if err != nil {
			return err
		}
		status, err = s.srv.st.GetMailboxStatus(s.ctx, s.userID(), mb.ID)
		if err != nil {
			return err
		}
		view = newMailboxView(s.srv, mb, status.UIDValidity, uids, readOnly)
		s.srv.register(view)
		return nil
	})
	if err != nil {
		return nil, s.storeErr(err)
	}
	s.view = view

	data := &imap.SelectData{
		Flags:       systemFlags,
		NumMessages: uint32(len(view.uids)),
		UIDNext:     imap.UID(status.UIDNext),
		UIDValidity: status.UIDValidity,
	}
	if readOnly {
		data.PermanentFlags = []imap.Flag{}
	} else {
		data.PermanentFlags = permFlags
	}
	if s.conn.EnabledCaps().Has(imap.CapIMAP4rev2) {
		attrs, err := s.mailboxAttrs(mb, nil)
		if err != nil {
			s.releaseView()
			return nil, s.storeErr(err)
		}
		data.List = &imap.ListData{
			Attrs:   removeAttr(attrs, imap.MailboxAttrSubscribed),
			Delim:   store.MailboxDelimiter,
			Mailbox: mb.Name,
		}
	}
	return data, nil
}

func (s *session) Unselect() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseView()
	return nil
}

func (s *session) Create(name string, options *imap.CreateOptions) error {
	if options != nil && len(options.SpecialUse) > 0 {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: "USEATTR", Text: "Special-use attributes cannot be assigned"}
	}
	name = normName(strings.TrimSuffix(name, string(store.MailboxDelimiter)))
	if _, err := s.srv.st.CreateMailbox(s.ctx, s.userID(), name); err != nil {
		return s.storeErr(err)
	}
	return nil
}

func (s *session) Delete(name string) error {
	mb, err := s.mailboxByName(name)
	if err != nil {
		return s.storeErr(err)
	}
	if mb.SpecialUse != "" {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeCannot, Text: "System mailboxes cannot be deleted"}
	}
	hasChildren, err := s.srv.st.HasChildren(s.ctx, s.userID(), mb.Name)
	if err != nil {
		return s.storeErr(err)
	}
	if hasChildren {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeHasChildren, Text: "Mailbox has children"}
	}
	return s.storeErr(s.srv.st.DeleteMailbox(s.ctx, s.userID(), mb.ID, false))
}

func (s *session) Rename(name, newName string, options *imap.RenameOptions) error {
	mb, err := s.mailboxByName(name)
	if err != nil {
		return s.storeErr(err)
	}
	newName = normName(strings.TrimSuffix(newName, string(store.MailboxDelimiter)))
	if mb.SpecialUse == store.SpecialInbox {
		return s.renameInbox(mb, newName)
	}
	return s.storeErr(s.srv.st.RenameMailbox(s.ctx, s.userID(), mb.ID, newName))
}

// renameInbox implements the special RENAME INBOX semantics: all messages
// are moved to a new mailbox and INBOX is left empty.
func (s *session) renameInbox(inbox *store.Mailbox, newName string) error {
	dst, err := s.srv.st.CreateMailbox(s.ctx, s.userID(), newName)
	if err != nil {
		return s.storeErr(err)
	}
	uids, err := s.srv.st.MailboxUIDs(s.ctx, inbox.ID)
	if err != nil {
		return s.storeErr(err)
	}
	if len(uids) == 0 {
		return nil
	}
	_, _, err = s.srv.st.MoveMessages(s.ctx, s.userID(), inbox.ID, uids, dst.ID, nil)
	return s.storeErr(err)
}

func (s *session) Subscribe(name string) error {
	mb, err := s.mailboxByName(name)
	if err != nil {
		return s.storeErr(err)
	}
	return s.storeErr(s.srv.st.SetSubscribed(s.ctx, s.userID(), mb.ID, true))
}

func (s *session) Unsubscribe(name string) error {
	mb, err := s.mailboxByName(name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil {
		return s.storeErr(err)
	}
	return s.storeErr(s.srv.st.SetSubscribed(s.ctx, s.userID(), mb.ID, false))
}

func (s *session) Namespace() (*imap.NamespaceData, error) {
	return &imap.NamespaceData{
		Personal: []imap.NamespaceDescriptor{{Prefix: "", Delim: store.MailboxDelimiter}},
	}, nil
}

func (s *session) AppendLimit() uint32 {
	return uint32(s.srv.maxBytes)
}

func (s *session) Status(name string, options *imap.StatusOptions) (*imap.StatusData, error) {
	mb, err := s.mailboxByName(name)
	if err != nil {
		return nil, s.storeErr(err)
	}
	return s.status(mb, options)
}

func (s *session) status(mb *store.Mailbox, options *imap.StatusOptions) (*imap.StatusData, error) {
	st, err := s.srv.st.GetMailboxStatus(s.ctx, s.userID(), mb.ID)
	if err != nil {
		return nil, s.storeErr(err)
	}
	num, unseen, deleted := uint32(st.Messages), uint32(st.Unseen), uint32(st.Deleted)
	var recent uint32
	var deletedStorage int64
	size := st.Size
	limit := uint32(s.srv.maxBytes)
	return &imap.StatusData{
		Mailbox:        mb.Name,
		NumMessages:    &num,
		NumRecent:      &recent,
		UIDNext:        imap.UID(st.UIDNext),
		UIDValidity:    st.UIDValidity,
		NumUnseen:      &unseen,
		NumDeleted:     &deleted,
		Size:           &size,
		AppendLimit:    &limit,
		DeletedStorage: &deletedStorage,
	}, nil
}

func (s *session) Append(name string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	mb, err := s.mailboxByName(name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeTryCreate, Text: "No such mailbox"}
	} else if err != nil {
		return nil, s.storeErr(err)
	}
	size := r.Size()
	if size > s.srv.maxBytes {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeTooBig, Text: "Message is too big"}
	}
	u, err := s.srv.st.GetUser(s.ctx, s.userID())
	if err != nil {
		return nil, s.storeErr(err)
	}
	if err := s.srv.st.CheckQuota(s.ctx, u, size); err != nil {
		return nil, s.storeErr(err)
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(r, raw); err != nil {
		return nil, err
	}
	var flags []string
	if options != nil {
		flags = fromIMAPFlags(options.Flags)
	}
	received := time.Now()
	if options != nil && !options.Time.IsZero() {
		received = options.Time
	}
	m, err := s.srv.st.InsertMessage(s.ctx, store.NewMessage{
		UserID:     s.userID(),
		MailboxID:  mb.ID,
		Raw:        raw,
		Flags:      flags,
		ReceivedAt: received,
	})
	if err != nil {
		return nil, s.storeErr(err)
	}
	return &imap.AppendData{UID: imap.UID(m.UID), UIDValidity: mb.UIDValidity}, nil
}

func (s *session) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.view == nil {
		return nil
	}
	return s.view.flush(s.ctx, w, allowExpunge)
}

func (s *session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	s.mu.Lock()
	var notify <-chan struct{}
	if s.view != nil {
		notify = s.view.notify
	}
	s.mu.Unlock()
	for {
		if err := s.Poll(w, true); err != nil {
			return err
		}
		select {
		case <-stop:
			return nil
		case <-s.ctx.Done():
			return nil
		case <-notify:
		}
	}
}

// selectedView returns the selected mailbox view after verifying it still
// exists. s.mu must be held.
func (s *session) selectedView() (*mailboxView, error) {
	v := s.view
	if v == nil {
		return nil, &imap.Error{Type: imap.StatusResponseTypeBad, Text: "No mailbox selected"}
	}
	v.check(s.ctx)
	return v, nil
}

var (
	errReadOnly = &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeCannot, Text: "Mailbox is read-only"}
	errGone     = &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNonExistent, Text: "Mailbox no longer exists"}
)

// storeErr converts a store error to an IMAP error response.
func (s *session) storeErr(err error) error {
	var verr *store.ValidationError
	var imapErr *imap.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &imapErr):
		return err
	case errors.Is(err, store.ErrNotFound):
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNonExistent, Text: "No such mailbox"}
	case errors.Is(err, store.ErrConflict):
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAlreadyExists, Text: "Mailbox already exists"}
	case errors.Is(err, store.ErrQuotaExceeded):
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeOverQuota, Text: "Storage quota exceeded"}
	case errors.As(err, &verr):
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeCannot, Text: verr.Msg}
	case errors.Is(err, context.Canceled):
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeUnavailable, Text: "Server is shutting down"}
	default:
		s.srv.log.Warn("imap: store error", "err", err, "remote", s.remote)
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeServerBug, Text: "Internal server error"}
	}
}
