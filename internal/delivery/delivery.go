// Package delivery stores incoming mail in local mailboxes and delivers
// queued outgoing mail, both to local users and to remote mail servers.
package delivery

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charles2ke/TitoMail/internal/email"
	"github.com/charles2ke/TitoMail/internal/store"
)

// Resolver performs the DNS lookups needed for delivery and for inbound
// SPF/DKIM/DMARC checks. *net.Resolver implements it.
type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupAddr(ctx context.Context, addr string) ([]string, error)
}

// Relay configures a smarthost that receives all outbound mail.
type Relay struct {
	Host     string
	Port     int
	Username string
	Password string
	// TLS is "starttls", "tls" or "none".
	TLS string
}

// Options configures a Deliverer.
type Options struct {
	// Hostname is used in EHLO and in bounce messages.
	Hostname string
	// QueueMaxAge is how long temporary failures are retried before the
	// message is bounced.
	QueueMaxAge time.Duration
	// DisableOutbound keeps remote mail in the queue.
	DisableOutbound bool
	// OutboundPort is the port used for direct-to-MX delivery (25).
	OutboundPort int
	// Relay, if set, receives all remote mail.
	Relay *Relay
	// RelayTLSConfig overrides the TLS configuration used to verify the relay.
	RelayTLSConfig *tls.Config
	Resolver       Resolver
	// Dial opens outbound connections. Defaults to a net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Concurrency limits simultaneous delivery attempts.
	Concurrency int
	Logger      *slog.Logger
}

// Deliverer delivers messages. Create it with New and start the queue
// worker with Run.
type Deliverer struct {
	st   *store.Store
	opts Options
	log  *slog.Logger
	res  Resolver

	kick     chan struct{}
	active   atomic.Int32
	wg       sync.WaitGroup
	finishMu sync.Mutex
	now      func() time.Time
}

// New returns a Deliverer.
func New(st *store.Store, opts Options) *Deliverer {
	if opts.Hostname == "" {
		opts.Hostname = "localhost"
	}
	if opts.QueueMaxAge <= 0 {
		opts.QueueMaxAge = 5 * 24 * time.Hour
	}
	if opts.OutboundPort == 0 {
		opts.OutboundPort = 25
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 8
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Dial == nil {
		d := &net.Dialer{Timeout: 30 * time.Second}
		opts.Dial = d.DialContext
	}
	res := opts.Resolver
	if res == nil {
		res = net.DefaultResolver
	}
	return &Deliverer{
		st:   st,
		opts: opts,
		log:  opts.Logger,
		res:  res,
		kick: make(chan struct{}, 1),
		now:  time.Now,
	}
}

// Resolver returns the DNS resolver in use.
func (d *Deliverer) Resolver() Resolver { return d.res }

// ResolveRecipient finds the local user that receives mail for addr. Mail
// to postmaster@ and abuse@ any local domain goes to the first
// administrator unless an explicit alias exists.
func (d *Deliverer) ResolveRecipient(ctx context.Context, addr string) (*store.User, error) {
	u, err := d.st.ResolveAddress(ctx, addr)
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		return u, err
	}
	local := strings.ToLower(email.LocalPart(addr))
	if (local == "postmaster" || local == "abuse") && d.st.IsLocalDomain(ctx, email.DomainOf(addr)) {
		users, err := d.st.ListUsers(ctx)
		if err != nil {
			return nil, err
		}
		for _, u := range users {
			if u.IsAdmin && !u.Disabled {
				return u, nil
			}
		}
	}
	return nil, store.ErrNotFound
}

// Inbound is a message being delivered to a local mailbox.
type Inbound struct {
	Raw []byte
	// Parsed is optional; it is computed from Raw when nil.
	Parsed *email.Message
	// MailFrom is the envelope sender; empty for bounces.
	MailFrom string
	// Spam marks the message as suspicious (e.g. failed DMARC).
	Spam bool
}

// DeliverLocal stores a message in the mailbox of u, applying blocked and
// trusted senders, the user's filters and the vacation responder. It
// returns store.ErrQuotaExceeded if the user's mailbox is full.
func (d *Deliverer) DeliverLocal(ctx context.Context, u *store.User, in *Inbound) (*store.Message, error) {
	p := in.Parsed
	if p == nil {
		p = email.Parse(in.Raw)
	}
	from := p.PrimaryFrom().Address
	pref := ""
	if from != "" {
		pref = d.st.GetSenderPref(ctx, u.ID, from)
	}
	spam := in.Spam
	if spam && (pref == store.SenderTrusted || (from != "" && d.st.IsContact(ctx, u.ID, from))) {
		spam = false
	}

	var flags []string
	var labels []int64
	var moveTo int64
	trash, skipInbox, neverSpam := false, false, false
	filters, err := d.st.ListFilters(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	for _, f := range filters {
		if !f.Enabled || !f.Criteria.Matches(p) {
			continue
		}
		a := f.Actions
		if a.MarkRead {
			flags = append(flags, store.FlagSeen)
		}
		if a.Star {
			flags = append(flags, store.FlagFlagged)
		}
		if a.LabelID > 0 {
			labels = append(labels, a.LabelID)
		}
		if a.MoveTo > 0 && moveTo == 0 {
			moveTo = a.MoveTo
		}
		trash = trash || a.Trash
		skipInbox = skipInbox || a.SkipInbox
		neverSpam = neverSpam || a.NeverSpam
	}

	special := store.SpecialInbox
	switch {
	case pref == store.SenderBlocked:
		special = store.SpecialJunk
	case spam && !neverSpam:
		special = store.SpecialJunk
	case trash:
		special = store.SpecialTrash
	case moveTo > 0:
		special = ""
	case skipInbox:
		special = store.SpecialArchive
	}
	var mb *store.Mailbox
	if special == "" {
		mb, err = d.st.GetMailbox(ctx, u.ID, moveTo)
		if errors.Is(err, store.ErrNotFound) {
			special = store.SpecialInbox
		} else if err != nil {
			return nil, err
		}
	}
	if mb == nil {
		mb, err = d.st.MailboxBySpecial(ctx, u.ID, special)
		if err != nil {
			return nil, err
		}
	}
	m, err := d.st.InsertMessageWithQuota(ctx, store.NewMessage{
		UserID: u.ID, MailboxID: mb.ID, Raw: in.Raw, Parsed: p, Flags: flags, LabelIDs: labels,
	})
	if err != nil {
		return nil, err
	}
	if mb.SpecialUse != store.SpecialJunk && mb.SpecialUse != store.SpecialTrash {
		if err := d.maybeVacationReply(ctx, u, p, in.MailFrom); err != nil {
			d.log.Warn("vacation reply failed", "user", u.Email, "err", err)
		}
	}
	return m, nil
}
