package delivery

import (
	"context"
	"errors"
	"time"

	"github.com/charles2ke/TitoMail/internal/email"
	"github.com/charles2ke/TitoMail/internal/store"
)

// ErrNoRecipients is returned when a message has no valid recipients.
var ErrNoRecipients = errors.New("add at least one recipient")

// SendRequest is a message composed in the web client.
type SendRequest struct {
	User    *store.User
	Message *email.Outgoing
	// DraftID is the draft being sent; it is deleted afterwards.
	DraftID int64
	// ReplyToID is the message being answered; it gets the \Answered flag.
	ReplyToID int64
	// ForwardOfID is the message being forwarded; it gets $Forwarded.
	ForwardOfID int64
	// Delay holds the message in the queue so the user can undo the send.
	Delay time.Duration
}

// SendResult describes a queued message.
type SendResult struct {
	QueueID int64          `json:"queue_id"`
	Sent    *store.Message `json:"message"`
	SendAt  time.Time      `json:"send_at"`
}

// Recipients returns the normalized, de-duplicated envelope recipients of a
// composed message.
func Recipients(o *email.Outgoing) []string {
	var out []string
	seen := map[string]bool{}
	for _, list := range [][]email.Address{o.To, o.Cc, o.Bcc} {
		for _, a := range list {
			addr := email.NormalizeAddress(a.Address)
			if addr == "" || seen[addr] {
				continue
			}
			seen[addr] = true
			out = append(out, addr)
		}
	}
	return out
}

// Send stores a copy of the message in the user's Sent folder and queues
// it for delivery after req.Delay. The caller must have verified that the
// user may use the From address.
func (d *Deliverer) Send(ctx context.Context, req SendRequest) (*SendResult, error) {
	u, o := req.User, req.Message
	rcpts := Recipients(o)
	if len(rcpts) == 0 {
		return nil, ErrNoRecipients
	}
	for _, r := range rcpts {
		if !email.ValidAddress(r) {
			return nil, &store.ValidationError{Msg: "invalid recipient address: " + r}
		}
	}
	sentRaw, err := o.Build(true)
	if err != nil {
		return nil, err
	}
	wireRaw, err := o.Build(false)
	if err != nil {
		return nil, err
	}
	sentBox, err := d.st.MailboxBySpecial(ctx, u.ID, store.SpecialSent)
	if err != nil {
		return nil, err
	}
	sent, err := d.st.InsertMessageWithQuota(ctx, store.NewMessage{UserID: u.ID, MailboxID: sentBox.ID, Raw: sentRaw, Flags: []string{store.FlagSeen}})
	if err != nil {
		return nil, err
	}
	sendAt := d.now().Add(req.Delay)
	qid, err := d.st.Enqueue(ctx, &store.QueueEntry{
		UserID: u.ID, MailFrom: email.NormalizeAddress(o.From.Address), MessageID: o.MessageID,
		Subject: o.Subject, SentCopyID: sent.ID, Data: wireRaw,
	}, rcpts, sendAt)
	if err != nil {
		_, _ = d.st.DeleteByID(ctx, u.ID, []int64{sent.ID})
		return nil, err
	}
	if req.DraftID > 0 {
		if _, err := d.st.DeleteByID(ctx, u.ID, []int64{req.DraftID}); err != nil {
			d.log.Warn("deleting sent draft failed", "err", err)
		}
	}
	if req.ReplyToID > 0 {
		_ = d.st.UpdateFlagsByID(ctx, u.ID, []int64{req.ReplyToID}, store.FlagsAdd, []string{store.FlagAnswered})
	}
	if req.ForwardOfID > 0 {
		_ = d.st.UpdateFlagsByID(ctx, u.ID, []int64{req.ForwardOfID}, store.FlagsAdd, []string{"$Forwarded"})
	}
	var touched []email.Address
	touched = append(append(append(touched, o.To...), o.Cc...), o.Bcc...)
	if err := d.st.TouchContacts(ctx, u.ID, touched); err != nil {
		d.log.Warn("updating contacts failed", "err", err)
	}
	d.Kick()
	return &SendResult{QueueID: qid, Sent: sent, SendAt: sendAt}, nil
}

// Submit queues a message received over authenticated SMTP submission.
func (d *Deliverer) Submit(ctx context.Context, u *store.User, mailFrom string, rcpts []string, raw []byte, p *email.Message) (int64, error) {
	if len(rcpts) == 0 {
		return 0, ErrNoRecipients
	}
	if p == nil {
		p = email.Parse(raw)
	}
	id, err := d.st.Enqueue(ctx, &store.QueueEntry{
		UserID: u.ID, MailFrom: email.NormalizeAddress(mailFrom), MessageID: p.MessageID, Subject: p.Subject, Data: raw,
	}, rcpts, d.now())
	if err != nil {
		return 0, err
	}
	var touched []email.Address
	for _, r := range rcpts {
		touched = append(touched, email.Address{Address: r})
	}
	for _, a := range append(append([]email.Address{}, p.To...), p.Cc...) {
		for i := range touched {
			if email.NormalizeAddress(a.Address) == touched[i].Address && a.Name != "" {
				touched[i].Name = a.Name
			}
		}
	}
	if err := d.st.TouchContacts(ctx, u.ID, touched); err != nil {
		d.log.Warn("updating contacts failed", "err", err)
	}
	d.Kick()
	return id, nil
}
