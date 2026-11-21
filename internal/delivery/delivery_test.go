package delivery

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	gosmtp "github.com/emersion/go-smtp"

	"github.com/charles2ke/TitoMail/internal/email"
	"github.com/charles2ke/TitoMail/internal/store"
)

func init() { store.PasswordCost = 4 }

type fakeResolver struct {
	mx  map[string][]*net.MX
	ips map[string][]net.IPAddr
	txt map[string][]string
}

func notFound(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (r *fakeResolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	if v, ok := r.mx[strings.TrimSuffix(name, ".")]; ok {
		return v, nil
	}
	return nil, notFound(name)
}

func (r *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if v, ok := r.ips[strings.TrimSuffix(host, ".")]; ok {
		return v, nil
	}
	return nil, notFound(host)
}

func (r *fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if v, ok := r.txt[strings.TrimSuffix(name, ".")]; ok {
		return v, nil
	}
	return nil, notFound(name)
}

func (r *fakeResolver) LookupAddr(context.Context, string) ([]string, error) {
	return nil, notFound("addr")
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newUser(t *testing.T, st *store.Store, addr string) *store.User {
	t.Helper()
	ctx := context.Background()
	if d := email.DomainOf(addr); !st.IsLocalDomain(ctx, d) {
		if _, err := st.CreateDomain(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	u, err := st.CreateUser(ctx, store.NewUser{Email: addr, Password: "correct horse", DisplayName: strings.Split(addr, "@")[0]})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func msg(from, to, subject, extra, body string) []byte {
	return []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: <%d@test>\r\nDate: Mon, 02 Jan 2006 15:04:05 +0000\r\n%s"+
		"Content-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", from, to, subject, time.Now().UnixNano(), extra, body))
}

func mailboxOf(t *testing.T, st *store.Store, u *store.User, m *store.Message) string {
	t.Helper()
	mb, err := st.GetMailbox(context.Background(), u.ID, m.MailboxID)
	if err != nil {
		t.Fatal(err)
	}
	return mb.SpecialUse
}

func TestDeliverLocalRules(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	d := New(st, Options{Hostname: "mx.example.test"})
	u := newUser(t, st, "alice@example.test")

	label, err := st.CreateLabel(ctx, u.ID, "News", "#336699")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveFilter(ctx, u.ID, &store.Filter{Name: "news", Enabled: true,
		Criteria: store.FilterCriteria{From: "news@"},
		Actions:  store.FilterActions{SkipInbox: true, MarkRead: true, LabelID: label.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSenderPref(ctx, u.ID, "spammer@bad.test", store.SenderBlocked); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSenderPref(ctx, u.ID, "@friends.test", store.SenderTrusted); err != nil {
		t.Fatal(err)
	}

	deliver := func(from string, spam bool) *store.Message {
		t.Helper()
		m, err := d.DeliverLocal(ctx, u, &Inbound{Raw: msg(from, "alice@example.test", "hi", "", "hello"), MailFrom: from, Spam: spam})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	if m := deliver("bob@other.test", false); mailboxOf(t, st, u, m) != store.SpecialInbox || m.Seen {
		t.Errorf("plain message: mailbox %q seen %v", mailboxOf(t, st, u, m), m.Seen)
	}
	m := deliver("news@shop.test", false)
	if mailboxOf(t, st, u, m) != store.SpecialArchive || !m.Seen || len(m.Labels) != 1 || m.Labels[0] != label.ID {
		t.Errorf("filtered message: mailbox %q seen %v labels %v", mailboxOf(t, st, u, m), m.Seen, m.Labels)
	}
	if m := deliver("spammer@bad.test", false); mailboxOf(t, st, u, m) != store.SpecialJunk {
		t.Errorf("blocked sender delivered to %q", mailboxOf(t, st, u, m))
	}
	if m := deliver("x@unknown.test", true); mailboxOf(t, st, u, m) != store.SpecialJunk {
		t.Errorf("spam delivered to %q", mailboxOf(t, st, u, m))
	}
	if m := deliver("pal@friends.test", true); mailboxOf(t, st, u, m) != store.SpecialInbox {
		t.Errorf("trusted sender's message delivered to %q", mailboxOf(t, st, u, m))
	}

	quota := int64(1)
	if err := st.UpdateUser(ctx, u.ID, store.UserUpdate{QuotaBytes: &quota}); err != nil {
		t.Fatal(err)
	}
	u, _ = st.GetUser(ctx, u.ID)
	if _, err := d.DeliverLocal(ctx, u, &Inbound{Raw: msg("bob@other.test", "alice@example.test", "big", "", "x")}); !errors.Is(err, store.ErrQuotaExceeded) {
		t.Errorf("over-quota delivery: got %v", err)
	}
}

func TestVacationReply(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	d := New(st, Options{Hostname: "mx.example.test"})
	u := newUser(t, st, "alice@example.test")
	settings := u.Settings
	settings.Vacation = store.Vacation{Enabled: true, Subject: "Away", Body: "Back next week."}
	if _, err := st.UpdateSettings(ctx, u.ID, settings); err != nil {
		t.Fatal(err)
	}
	u, _ = st.GetUser(ctx, u.ID)

	deliver := func(from, to, extra string) {
		t.Helper()
		if _, err := d.DeliverLocal(ctx, u, &Inbound{Raw: msg(from, to, "Question", extra, "hi"), MailFrom: from}); err != nil {
			t.Fatal(err)
		}
	}
	deliver("bob@other.test", "alice@example.test", "")
	deliver("bob@other.test", "alice@example.test", "")                             // already answered
	deliver("list@other.test", "alice@example.test", "List-Id: <x.other.test>\r\n") // mailing list
	deliver("carol@other.test", "someone@else.test", "")                            // not addressed directly
	deliver("noreply@other.test", "alice@example.test", "")                         // automated sender

	q, err := st.ListQueue(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 1 {
		t.Fatalf("expected exactly one auto-reply, got %d", len(q))
	}
	e, _ := st.GetQueueEntry(ctx, q[0].ID)
	if e.MailFrom != "" || len(e.Rcpts) != 1 || e.Rcpts[0].Rcpt != "bob@other.test" {
		t.Errorf("auto-reply envelope: from %q rcpts %+v", e.MailFrom, e.Rcpts)
	}
	p := email.Parse(e.Data)
	if p.Subject != "Away" || p.Header.Get("Auto-Submitted") != "auto-replied" || len(p.InReplyTo) != 1 {
		t.Errorf("auto-reply: subject %q auto-submitted %q in-reply-to %v", p.Subject, p.Header.Get("Auto-Submitted"), p.InReplyTo)
	}
}

// fakeMX is an SMTP server that records received messages.
type fakeMX struct {
	mu       sync.Mutex
	received []fakeDelivery
}

type fakeDelivery struct {
	from  string
	rcpts []string
	data  []byte
	tls   bool
	helo  string
}

type fakeSession struct {
	mx    *fakeMX
	conn  *gosmtp.Conn
	from  string
	rcpts []string
}

func (s *fakeSession) Reset()        { s.from, s.rcpts = "", nil }
func (s *fakeSession) Logout() error { return nil }
func (s *fakeSession) Mail(from string, _ *gosmtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *fakeSession) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	switch {
	case strings.HasPrefix(to, "dave@"):
		return &gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 1, 1}, Message: "No such user"}
	case strings.HasSuffix(to, "@temp.test"):
		return &gosmtp.SMTPError{Code: 451, EnhancedCode: gosmtp.EnhancedCode{4, 3, 0}, Message: "Try again later"}
	}
	s.rcpts = append(s.rcpts, to)
	return nil
}

func (s *fakeSession) Data(r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	_, isTLS := s.conn.TLSConnectionState()
	s.mx.mu.Lock()
	s.mx.received = append(s.mx.received, fakeDelivery{from: s.from, rcpts: s.rcpts, data: b, tls: isTLS, helo: s.conn.Hostname()})
	s.mx.mu.Unlock()
	return nil
}

func selfSignedTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mx.remote.test"},
		DNSNames: []string{"mx.remote.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

func startFakeMX(t *testing.T) (*fakeMX, int) {
	t.Helper()
	mx := &fakeMX{}
	srv := gosmtp.NewServer(gosmtp.BackendFunc(func(c *gosmtp.Conn) (gosmtp.Session, error) {
		return &fakeSession{mx: mx, conn: c}, nil
	}))
	srv.Domain = "mx.remote.test"
	srv.TLSConfig = selfSignedTLS(t)
	srv.ErrorLog = log.New(io.Discard, "", 0)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return mx, ln.Addr().(*net.TCPAddr).Port
}

func TestSendLocalRemoteBounceAndRetry(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	alice := newUser(t, st, "alice@example.test")
	bob := newUser(t, st, "bob@example.test")
	mx, port := startFakeMX(t)
	dom, err := st.DomainByName(ctx, "example.test")
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := dom.DKIMPublicKey()
	loopback := []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}
	res := &fakeResolver{
		mx: map[string][]*net.MX{
			"remote.test": {{Host: "mx.remote.test.", Pref: 10}},
			"temp.test":   {{Host: "mx.remote.test.", Pref: 10}},
			"nullmx.test": {{Host: ".", Pref: 0}},
		},
		ips: map[string][]net.IPAddr{"mx.remote.test": loopback},
		txt: map[string][]string{"titomail._domainkey.example.test": {"v=DKIM1; k=rsa; p=" + pub}},
	}
	d := New(st, Options{Hostname: "mail.example.test", Resolver: res, OutboundPort: port})

	out := &email.Outgoing{
		From:    alice.Address(),
		To:      []email.Address{{Name: "Bob", Address: "bob@example.test"}, {Address: "carol@remote.test"}},
		Cc:      []email.Address{{Address: "dave@remote.test"}, {Address: "erin@temp.test"}},
		Bcc:     []email.Address{{Address: "frank@nullmx.test"}},
		Subject: "Quarterly report",
		HTML:    "<p>Numbers attached.</p>",
	}
	r, err := d.Send(ctx, SendRequest{User: alice, Message: out})
	if err != nil {
		t.Fatal(err)
	}
	d.ProcessQueue(ctx)

	// Local recipient.
	inbox, _ := st.MailboxBySpecial(ctx, bob.ID, store.SpecialInbox)
	msgs, _ := st.AllMessagesInMailbox(ctx, inbox.ID)
	if len(msgs) != 1 || msgs[0].Subject != "Quarterly report" {
		t.Fatalf("bob's inbox: %+v", msgs)
	}
	if len(msgs[0].Bcc) != 0 {
		t.Errorf("Bcc header leaked to recipient: %v", msgs[0].Bcc)
	}

	// Remote recipient: delivered over STARTTLS, DKIM-signed.
	mx.mu.Lock()
	received := append([]fakeDelivery(nil), mx.received...)
	mx.mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("fake MX received %d messages", len(received))
	}
	got := received[0]
	if got.from != "alice@example.test" || strings.Join(got.rcpts, ",") != "carol@remote.test" {
		t.Errorf("envelope from %q rcpts %v", got.from, got.rcpts)
	}
	if !got.tls || got.helo != "mail.example.test" {
		t.Errorf("tls %v helo %q", got.tls, got.helo)
	}
	verifs, err := dkim.VerifyWithOptions(bytes.NewReader(got.data), &dkim.VerifyOptions{LookupTXT: func(name string) ([]string, error) {
		return res.LookupTXT(ctx, name)
	}})
	if err != nil || len(verifs) != 1 || verifs[0].Err != nil || verifs[0].Domain != "example.test" {
		t.Errorf("DKIM verification: %v %+v", err, verifs)
	}
	if bytes.Contains(got.data, []byte("frank@nullmx.test")) {
		t.Error("Bcc recipient visible in transmitted message")
	}

	// erin@temp.test was deferred; dave and frank failed permanently.
	q, _ := st.GetQueueEntry(ctx, r.QueueID)
	if q == nil {
		t.Fatal("queue entry removed although a recipient is pending")
	}
	states := map[string]*store.QueueRcpt{}
	for _, rc := range q.Rcpts {
		states[rc.Rcpt] = rc
	}
	if e := states["erin@temp.test"]; e.Status != store.RcptPending || e.Attempts != 1 || !strings.Contains(e.LastError, "451") {
		t.Errorf("erin: %+v", e)
	}
	if e := states["dave@remote.test"]; e.Status != store.RcptFailed || !strings.Contains(e.LastError, "550") {
		t.Errorf("dave: %+v", e)
	}
	if e := states["frank@nullmx.test"]; e.Status != store.RcptFailed || !strings.Contains(e.LastError, "null MX") {
		t.Errorf("frank: %+v", e)
	}

	// Give up on erin: the entry completes and a single bounce lists both
	// failures, threaded with the sent message.
	d.opts.QueueMaxAge = time.Second
	d.now = func() time.Time { return time.Now().Add(time.Hour) }
	d.ProcessQueue(ctx)
	if _, err := st.GetQueueEntry(ctx, r.QueueID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("queue entry not removed after completion: %v", err)
	}
	aliceInbox, _ := st.MailboxBySpecial(ctx, alice.ID, store.SpecialInbox)
	bounces, _ := st.AllMessagesInMailbox(ctx, aliceInbox.ID)
	if len(bounces) != 1 {
		t.Fatalf("expected one bounce, got %d", len(bounces))
	}
	raw, _, _ := st.MessageRaw(ctx, alice.ID, bounces[0].ID)
	for _, want := range []string{"dave@remote.test", "erin@temp.test", "frank@nullmx.test", "Status: 5.1.1", "Status: 4.4.7", "multipart/report"} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("bounce lacks %q", want)
		}
	}
	if bytes.Contains(raw, []byte("carol@remote.test\r\n    ")) {
		t.Error("bounce lists a delivered recipient")
	}
	if bounces[0].ThreadID != r.Sent.ThreadID {
		t.Errorf("bounce thread %d, sent thread %d", bounces[0].ThreadID, r.Sent.ThreadID)
	}
}

func TestUndoSend(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	alice := newUser(t, st, "alice@example.test")
	newUser(t, st, "bob@example.test")
	d := New(st, Options{Hostname: "mail.example.test", DisableOutbound: true})
	r, err := d.Send(ctx, SendRequest{User: alice, Delay: time.Minute, Message: &email.Outgoing{
		From: alice.Address(), To: []email.Address{{Address: "bob@example.test"}}, Subject: "oops", Text: "draft",
	}})
	if err != nil {
		t.Fatal(err)
	}
	d.ProcessQueue(ctx) // not due yet
	if _, err := st.CancelQueued(ctx, alice.ID, r.QueueID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := st.GetMessage(ctx, alice.ID, r.Sent.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("sent copy still exists after undo: %v", err)
	}

	// Once delivery started, undo is no longer possible.
	r, err = d.Send(ctx, SendRequest{User: alice, Message: &email.Outgoing{
		From: alice.Address(), To: []email.Address{{Address: "bob@example.test"}}, Subject: "sent", Text: "x",
	}})
	if err != nil {
		t.Fatal(err)
	}
	d.ProcessQueue(ctx)
	if _, err := st.CancelQueued(ctx, alice.ID, r.QueueID); err == nil {
		t.Error("cancelled an already delivered message")
	}
}

func TestSendChecksCurrentQuota(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	alice := newUser(t, st, "alice@example.test")
	quota := int64(1)
	if err := st.UpdateUser(ctx, alice.ID, store.UserUpdate{QuotaBytes: &quota}); err != nil {
		t.Fatal(err)
	}
	d := New(st, Options{})
	if _, err := d.Send(ctx, SendRequest{User: alice, Message: &email.Outgoing{
		From: alice.Address(), To: []email.Address{{Address: "bob@example.test"}}, Subject: "over quota", Text: "body",
	}}); !errors.Is(err, store.ErrQuotaExceeded) {
		t.Fatalf("Send() error = %v, want ErrQuotaExceeded", err)
	}
	sent, err := st.MailboxBySpecial(ctx, alice.ID, store.SpecialSent)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := st.AllMessagesInMailbox(ctx, sent.ID)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := st.ListQueue(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 || len(queue) != 0 {
		t.Errorf("over-quota send left %d sent messages and %d queue entries", len(messages), len(queue))
	}
}
