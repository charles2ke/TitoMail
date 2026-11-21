package smtpd

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/charles2ke/TitoMail/internal/auth"
	"github.com/charles2ke/TitoMail/internal/email"
	"github.com/charles2ke/TitoMail/internal/store"
)

// subSession accepts mail from authenticated users for delivery.
type subSession struct {
	s    *Server
	conn *smtp.Conn
	user *store.User

	from  string
	rcpts []string
}

var errAuthRequired = smtpErr(530, smtp.EnhancedCode{5, 7, 0}, "Authentication required")

func (ss *subSession) Reset()        { ss.from, ss.rcpts = "", nil }
func (ss *subSession) Logout() error { return nil }

func (ss *subSession) AuthMechanisms() []string { return []string{sasl.Plain, sasl.Login} }

func (ss *subSession) Auth(mech string) (sasl.Server, error) {
	switch mech {
	case sasl.Plain:
		return sasl.NewPlainServer(ss.login), nil
	case sasl.Login:
		return &loginServer{authenticate: func(username, password string) error { return ss.login("", username, password) }}, nil
	}
	return nil, smtp.ErrAuthUnknownMechanism
}

func (ss *subSession) login(identity, username, password string) error {
	if identity != "" && !strings.EqualFold(identity, username) {
		return smtp.ErrAuthFailed
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	u, err := ss.s.auth.Login(ctx, ss.conn.Conn().RemoteAddr().String(), username, password)
	switch {
	case errors.Is(err, auth.ErrThrottled):
		return smtpErr(454, smtp.EnhancedCode{4, 7, 0}, "Too many failed login attempts, try again later")
	case errors.Is(err, store.ErrInvalidCredentials):
		ss.s.log.Info("submission login failed", "remote", ss.conn.Conn().RemoteAddr().String(), "user", username)
		return smtp.ErrAuthFailed
	case err != nil:
		ss.s.log.Error("submission login error", "err", err)
		return smtpErr(454, smtp.EnhancedCode{4, 7, 0}, "Temporary authentication failure")
	}
	ss.user = u
	return nil
}

func (ss *subSession) Mail(from string, _ *smtp.MailOptions) error {
	if ss.user == nil {
		return errAuthRequired
	}
	ss.Reset()
	addr := email.NormalizeAddress(from)
	if from != "" && !ss.s.st.CanSendAs(context.Background(), ss.user, addr) {
		return smtpErr(553, smtp.EnhancedCode{5, 7, 1}, "Sender address rejected: not owned by "+ss.user.Email)
	}
	ss.from = addr
	return nil
}

func (ss *subSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	if ss.user == nil {
		return errAuthRequired
	}
	addr := email.NormalizeAddress(to)
	if !email.ValidAddress(addr) {
		return errBadAddress
	}
	if !slices.Contains(ss.rcpts, addr) {
		ss.rcpts = append(ss.rcpts, addr)
	}
	return nil
}

func (ss *subSession) Data(r io.Reader) error {
	if ss.user == nil {
		return errAuthRequired
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p := email.Parse(raw)
	if len(p.From) == 0 {
		return smtpErr(550, smtp.EnhancedCode{5, 6, 0}, "Message has no From header")
	}
	for _, a := range p.From {
		if !ss.s.st.CanSendAs(ctx, ss.user, a.Address) {
			return smtpErr(553, smtp.EnhancedCode{5, 7, 1}, "From address rejected: not owned by "+ss.user.Email)
		}
	}
	s := ss.s
	now := s.now()
	with := "ESMTPA"
	if _, ok := ss.conn.TLSConnectionState(); ok {
		with = "ESMTPSA"
	}
	prepend := []string{foldHeader("Received",
		"from "+cleanHelo(ss.conn.Hostname()),
		"by "+s.opts.Hostname+" (TitoMail) with "+with+" id "+newTraceID()+";",
		receivedDate(now))}
	fields, _ := splitHeader(raw)
	has := func(name string) bool {
		return slices.ContainsFunc(fields, func(f headerField) bool { return strings.EqualFold(f.name, name) })
	}
	if !has("Date") {
		prepend = append(prepend, "Date: "+receivedDate(now))
	}
	if !has("Message-ID") {
		p.MessageID = email.NewMessageID(email.DomainOf(p.From[0].Address))
		prepend = append(prepend, "Message-ID: <"+p.MessageID+">")
	}
	// Remove Bcc recipients and client-supplied authentication metadata.
	final := rewriteHeader(raw, prepend, func(f headerField) bool {
		return strings.EqualFold(f.name, "Bcc") || strings.EqualFold(f.name, "Authentication-Results")
	})
	id, err := s.d.Submit(ctx, ss.user, ss.from, ss.rcpts, final, p)
	if err != nil {
		s.log.Error("queueing submitted message failed", "user", ss.user.Email, "err", err)
		return errTempFailure
	}
	s.log.Info("message submitted", "user", ss.user.Email, "queue_id", id, "rcpts", len(ss.rcpts), "size", len(final))
	return nil
}

// loginServer implements the obsolete but widely used LOGIN SASL mechanism,
// which Outlook still uses by default.
type loginServer struct {
	authenticate func(username, password string) error
	username     string
	step         int
}

func (l *loginServer) Next(response []byte) ([]byte, bool, error) {
	switch l.step {
	case 0:
		l.step = 1
		if response == nil {
			return []byte("Username:"), false, nil
		}
		fallthrough
	case 1:
		l.username = string(response)
		l.step = 2
		return []byte("Password:"), false, nil
	case 2:
		l.step = 3
		return nil, true, l.authenticate(l.username, string(response))
	}
	return nil, false, errors.New("unexpected LOGIN response")
}
