package delivery

import (
	"context"
	"strings"
	"time"

	"github.com/charles2ke/TitoMail/internal/email"
	"github.com/charles2ke/TitoMail/internal/store"
)

// VacationInterval is the minimum time between two auto-replies to the
// same sender.
const VacationInterval = 4 * 24 * time.Hour

// isAutomated reports whether a message looks machine-generated or comes
// from a mailing list, in which case no auto-reply must be sent
// (RFC 3834 section 2).
func isAutomated(p *email.Message, mailFrom string) bool {
	h := p.Header
	if v := strings.ToLower(strings.TrimSpace(h.Get("Auto-Submitted"))); v != "" && v != "no" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(h.Get("Precedence"))) {
	case "bulk", "list", "junk":
		return true
	}
	for _, k := range []string{"List-Id", "List-Unsubscribe", "List-Post", "X-Auto-Response-Suppress", "Feedback-ID"} {
		if h.Get(k) != "" {
			return true
		}
	}
	local := strings.ToLower(email.LocalPart(mailFrom))
	for _, p := range []string{"mailer-daemon", "postmaster", "noreply", "no-reply", "donotreply", "do-not-reply", "listserv", "majordomo"} {
		if local == p {
			return true
		}
	}
	return strings.HasPrefix(local, "owner-") || strings.HasSuffix(local, "-request") || strings.HasPrefix(local, "bounce")
}

func (d *Deliverer) maybeVacationReply(ctx context.Context, u *store.User, p *email.Message, mailFrom string) error {
	v := u.Settings.Vacation
	if !v.Active(d.now()) || mailFrom == "" || !email.ValidAddress(mailFrom) || isAutomated(p, mailFrom) {
		return nil
	}
	own, err := d.st.SenderAddresses(ctx, u)
	if err != nil {
		return err
	}
	isOwn := func(addr string) bool {
		addr = email.NormalizeAddress(addr)
		for _, a := range own {
			if strings.EqualFold(a, addr) {
				return true
			}
		}
		return false
	}
	if isOwn(mailFrom) {
		return nil
	}
	// Only reply when the user is addressed directly (not via Bcc or a list).
	addressed := false
	for _, a := range append(append([]email.Address{}, p.To...), p.Cc...) {
		if isOwn(a.Address) {
			addressed = true
			break
		}
	}
	if !addressed {
		return nil
	}
	if v.ContactsOnly && !d.st.IsContact(ctx, u.ID, mailFrom) {
		return nil
	}
	ok, err := d.st.ClaimVacationReply(ctx, u.ID, mailFrom, VacationInterval)
	if err != nil || !ok {
		return err
	}
	subject := strings.TrimSpace(v.Subject)
	if subject == "" {
		subject = "Re: " + p.Subject
		if p.Subject == "" {
			subject = "Automatic reply"
		}
	}
	body := v.Body
	if strings.TrimSpace(body) == "" {
		body = "I am currently away and will reply when I return."
	}
	out := &email.Outgoing{
		From:    u.Address(),
		To:      []email.Address{{Address: email.NormalizeAddress(mailFrom)}},
		Subject: subject,
		Text:    body,
		HTML:    email.TextToHTML(body),
		Headers: [][2]string{{"Auto-Submitted", "auto-replied"}, {"X-Auto-Response-Suppress", "All"}},
	}
	if p.MessageID != "" {
		out.InReplyTo = p.MessageID
		out.References = append(append([]string{}, p.References...), p.MessageID)
	}
	raw, err := out.Build(false)
	if err != nil {
		return err
	}
	// RFC 3834 recommends a null return path for auto-replies so they can
	// never trigger bounces or further auto-replies.
	_, err = d.st.Enqueue(ctx, &store.QueueEntry{UserID: u.ID, MessageID: out.MessageID, Subject: subject, Data: raw},
		[]string{mailFrom}, d.now())
	if err == nil {
		d.Kick()
	}
	return err
}
