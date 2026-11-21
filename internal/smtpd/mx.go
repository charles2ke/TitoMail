package smtpd

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/charles2ke/TitoMail/internal/delivery"
	"github.com/charles2ke/TitoMail/internal/email"
	"github.com/charles2ke/TitoMail/internal/store"
)

// mxSession receives mail from other servers for local users.
type mxSession struct {
	s    *Server
	conn *smtp.Conn

	from  string
	size  int64
	rcpts []string
	users []*store.User
}

func (m *mxSession) Reset() {
	m.from, m.size, m.rcpts, m.users = "", 0, nil, nil
}

func (m *mxSession) Logout() error { return nil }

func (m *mxSession) Mail(from string, opts *smtp.MailOptions) error {
	m.Reset()
	m.from = email.NormalizeAddress(from)
	if from != "" && !email.ValidAddress(m.from) {
		return smtpErr(553, smtp.EnhancedCode{5, 1, 7}, "Bad sender address syntax")
	}
	if opts != nil {
		m.size = opts.Size
	}
	return nil
}

func (m *mxSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	addr := email.NormalizeAddress(to)
	if !email.ValidAddress(addr) {
		return errBadAddress
	}
	if !m.s.st.IsLocalDomain(ctx, email.DomainOf(addr)) {
		return smtpErr(554, smtp.EnhancedCode{5, 7, 1}, "Relay access denied")
	}
	u, err := m.s.d.ResolveRecipient(ctx, addr)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return smtpErr(550, smtp.EnhancedCode{5, 1, 1}, "The email account that you tried to reach does not exist")
	case err != nil:
		m.s.log.Error("resolving recipient failed", "rcpt", addr, "err", err)
		return errTempFailure
	case u.Disabled:
		return smtpErr(550, smtp.EnhancedCode{5, 2, 1}, "The email account that you tried to reach is disabled")
	}
	if err := m.s.st.CheckQuota(ctx, u, m.size); errors.Is(err, store.ErrQuotaExceeded) {
		return smtpErr(452, smtp.EnhancedCode{4, 2, 2}, "The email account that you tried to reach is over quota")
	}
	m.rcpts = append(m.rcpts, addr)
	for _, have := range m.users {
		if have.ID == u.ID {
			return nil
		}
	}
	m.users = append(m.users, u)
	return nil
}

func (m *mxSession) Data(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	s := m.s
	if countHeader(raw, "Received") > maxHops {
		return smtpErr(554, smtp.EnhancedCode{5, 4, 6}, "Too many hops, possible mail loop")
	}
	ip := remoteIP(m.conn)
	helo := cleanHelo(m.conn.Hostname())
	p := email.Parse(raw)
	logger := s.log.With("remote", ip.String(), "helo", helo, "from", m.from, "rcpts", m.rcpts, "size", len(raw))

	var prepend []string
	spam := false
	if s.opts.AuthChecks {
		fromDomain := email.DomainOf(p.PrimaryFrom().Address)
		v := s.checkAuth(ctx, ip, helo, m.from, raw, p, fromDomain != "" && s.st.IsLocalDomain(ctx, fromDomain))
		logger = logger.With("spf", v.spf, "dmarc", v.dmarc)
		if v.reject {
			logger.Info("message rejected", "reason", v.reason)
			return smtpErr(550, smtp.EnhancedCode{5, 7, 1}, "Message rejected due to the "+v.reason)
		}
		spam = v.spam
		if spam {
			logger = logger.With("spam", v.reason)
		}
		prepend = append(prepend, v.header(s.opts.Hostname))
	}

	id := newTraceID()
	rdns := "unknown"
	lctx, lcancel := context.WithTimeout(ctx, 3*time.Second)
	if names, err := s.res.LookupAddr(lctx, ip.String()); err == nil && len(names) > 0 {
		rdns = cleanHelo(strings.TrimSuffix(names[0], "."))
	}
	lcancel()
	with := "ESMTP"
	if _, ok := m.conn.TLSConnectionState(); ok {
		with = "ESMTPS"
	}
	clauses := []string{"from " + helo + " (" + rdns + " [" + ip.String() + "])", "by " + s.opts.Hostname + " (TitoMail) with " + with + " id " + id}
	if t := tlsDescription(m.conn); t != "" {
		clauses = append(clauses, t)
	}
	if len(m.rcpts) == 1 {
		clauses = append(clauses, "for <"+m.rcpts[0]+">")
	}
	clauses[len(clauses)-1] += ";"
	clauses = append(clauses, receivedDate(s.now()))
	prepend = append([]string{foldHeader("Received", clauses...)}, prepend...)

	// Drop Authentication-Results that claim to come from this server
	// (RFC 8601 section 5) so senders cannot forge them.
	final := rewriteHeader(raw, prepend, func(f headerField) bool {
		if !strings.EqualFold(f.name, "Authentication-Results") {
			return false
		}
		servID, _, _ := strings.Cut(f.value(), ";")
		return strings.EqualFold(strings.TrimSpace(servID), s.opts.Hostname)
	})

	var delivered int
	var failed []string
	quotaOnly := true
	for _, u := range m.users {
		_, err := s.d.DeliverLocal(ctx, u, &delivery.Inbound{Raw: final, MailFrom: m.from, Spam: spam})
		if err == nil {
			delivered++
			continue
		}
		if !errors.Is(err, store.ErrQuotaExceeded) {
			quotaOnly = false
		}
		logger.Warn("local delivery failed", "user", u.Email, "err", err)
		failed = append(failed, u.Email)
	}
	if delivered == 0 {
		if quotaOnly {
			return smtpErr(552, smtp.EnhancedCode{5, 2, 2}, "The email account that you tried to reach is over quota")
		}
		return errTempFailure
	}
	if len(failed) > 0 {
		// The message was accepted for some recipients, so it cannot be
		// refused any more; retry the rest from the queue, which bounces
		// the message if delivery keeps failing.
		_, err := s.st.Enqueue(ctx, &store.QueueEntry{MailFrom: m.from, MessageID: p.MessageID, Subject: p.Subject, Data: final},
			failed, s.now().Add(time.Minute))
		if err != nil {
			logger.Error("queueing failed local deliveries failed", "err", err)
			return errTempFailure
		}
		s.d.Kick()
	}
	logger.Info("message received", "id", id, "delivered", delivered, "deferred", len(failed))
	return nil
}
