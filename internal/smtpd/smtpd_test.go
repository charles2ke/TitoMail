package smtpd

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"

	"github.com/charles2ke/TitoMail/internal/auth"
	"github.com/charles2ke/TitoMail/internal/delivery"
	"github.com/charles2ke/TitoMail/internal/store"
)

func init() { store.PasswordCost = 4 }

type fakeResolver struct{ txt map[string][]string }

func notFound(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (r *fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if v, ok := r.txt[strings.TrimSuffix(name, ".")]; ok {
		return v, nil
	}
	return nil, notFound(name)
}
func (r *fakeResolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	return nil, notFound(name)
}
func (r *fakeResolver) LookupIPAddr(_ context.Context, name string) ([]net.IPAddr, error) {
	return nil, notFound(name)
}
func (r *fakeResolver) LookupAddr(_ context.Context, name string) ([]string, error) {
	return nil, notFound(name)
}

type env struct {
	st         *store.Store
	d          *delivery.Deliverer
	res        *fakeResolver
	alice, bob *store.User
	mx, sub    string
}

func selfSignedTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mx.example.test"},
		DNSNames: []string{"mx.example.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

func setup(t *testing.T, insecureAuth bool) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.CreateDomain(ctx, "example.test"); err != nil {
		t.Fatal(err)
	}
	e := &env{st: st, res: &fakeResolver{txt: map[string][]string{}}}
	if e.alice, err = st.CreateUser(ctx, store.NewUser{Email: "alice@example.test", Password: "correct horse"}); err != nil {
		t.Fatal(err)
	}
	if e.bob, err = st.CreateUser(ctx, store.NewUser{Email: "bob@example.test", Password: "battery staple"}); err != nil {
		t.Fatal(err)
	}
	e.d = delivery.New(st, delivery.Options{Hostname: "mx.example.test", Resolver: e.res, DisableOutbound: true})
	srv := New(st, e.d, auth.New(st), Options{Hostname: "mx.example.test", TLSConfig: selfSignedTLS(t),
		AllowInsecureAuth: insecureAuth, AuthChecks: true})
	t.Cleanup(func() { srv.Close() })
	for _, x := range []struct {
		addr  *string
		serve func(net.Listener) error
	}{{&e.mx, srv.ServeMX}, {&e.sub, srv.ServeSubmission}} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		*x.addr = ln.Addr().String()
		go x.serve(ln)
	}
	return e
}

func message(from, to, subject, extra string) []byte {
	return []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: <%d@test>\r\n"+
		"Date: Mon, 02 Jan 2006 15:04:05 +0000\r\n%sContent-Type: text/plain\r\n\r\nHello there.\r\n",
		from, to, subject, time.Now().UnixNano(), extra))
}

// transact runs one SMTP transaction. auth may be nil.
func transact(addr string, a sasl.Client, from string, rcpts []string, data []byte) error {
	c, err := gosmtp.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Hello("client.sender.test"); err != nil {
		return err
	}
	if a != nil {
		if err := c.Auth(a); err != nil {
			return err
		}
	}
	if err := c.Mail(from, nil); err != nil {
		return err
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r, nil); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	return w.Close()
}

func wantCode(t *testing.T, err error, code int, enh string) {
	t.Helper()
	var se *gosmtp.SMTPError
	if !errors.As(err, &se) {
		t.Fatalf("got error %v, want SMTP %d %s", err, code, enh)
	}
	got := fmt.Sprintf("%d.%d.%d", se.EnhancedCode[0], se.EnhancedCode[1], se.EnhancedCode[2])
	if se.Code != code || (enh != "" && got != enh) {
		t.Fatalf("got %d %s %q, want %d %s", se.Code, got, se.Message, code, enh)
	}
}

// inbox returns the raw messages in a user's mailbox.
func (e *env) mailbox(t *testing.T, u *store.User, special string) []string {
	t.Helper()
	ctx := context.Background()
	mb, err := e.st.MailboxBySpecial(ctx, u.ID, special)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := e.st.AllMessagesInMailbox(ctx, mb.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range msgs {
		raw, _, err := e.st.MessageRaw(ctx, u.ID, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(raw))
	}
	return out
}

func TestMXRecipientChecks(t *testing.T) {
	e := setup(t, true)
	data := message("carol@sender.test", "x@other.test", "hi", "")
	wantCode(t, transact(e.mx, nil, "carol@sender.test", []string{"x@other.test"}, data), 554, "5.7.1")
	wantCode(t, transact(e.mx, nil, "carol@sender.test", []string{"nobody@example.test"}, data), 550, "5.1.1")
	// AUTH must not be offered on the MX port.
	if err := transact(e.mx, sasl.NewPlainClient("", "alice@example.test", "correct horse"), "alice@example.test",
		[]string{"bob@example.test"}, data); err == nil {
		t.Fatal("AUTH accepted on the MX port")
	}
}

func TestMXAuthenticationResults(t *testing.T) {
	e := setup(t, true)
	ctx := context.Background()
	e.res.txt["sender.test"] = []string{"v=spf1 ip4:127.0.0.1 -all"}
	if _, err := e.st.CreateAlias(ctx, "info@example.test", e.alice.ID); err != nil {
		t.Fatal(err)
	}
	forged := "Authentication-Results: mx.example.test; dkim=pass header.d=bank.test\r\n" +
		"Authentication-Results: elsewhere.test; spf=pass\r\n"
	err := transact(e.mx, nil, "carol@sender.test", []string{"alice@example.test", "info@example.test"},
		message("Carol <carol@sender.test>", "alice@example.test", "Lunch", forged))
	if err != nil {
		t.Fatal(err)
	}
	inbox := e.mailbox(t, e.alice, store.SpecialInbox)
	if len(inbox) != 1 {
		t.Fatalf("got %d messages, want 1 (alias and address are the same user)", len(inbox))
	}
	raw := inbox[0]
	if !strings.HasPrefix(raw, "Received: from client.sender.test (unknown [127.0.0.1])\r\n\tby mx.example.test (TitoMail) with ESMTP id ") {
		t.Errorf("unexpected Received field:\n%s", raw[:200])
	}
	if strings.Contains(raw, "bank.test") || !strings.Contains(raw, "Authentication-Results: elsewhere.test; spf=pass") {
		t.Errorf("forged Authentication-Results not removed or foreign one dropped:\n%s", raw)
	}
	if n := strings.Count(raw, "Authentication-Results: mx.example.test;"); n != 1 {
		t.Errorf("got %d of our Authentication-Results fields, want 1", n)
	}
	for _, want := range []string{"Authentication-Results: mx.example.test;\r\n\tspf=pass ", "smtp.mailfrom=carol@sender.test;\r\n\tdkim=none;\r\n\tdmarc=none header.from=sender.test\r\n"} {
		if !strings.Contains(raw, want) {
			t.Errorf("missing %q in:\n%s", want, raw)
		}
	}
}

func TestMXDMARC(t *testing.T) {
	e := setup(t, true)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	e.res.txt = map[string][]string{
		"strict.test":                {"v=spf1 -all"},
		"_dmarc.strict.test":         {"v=DMARC1; p=reject"},
		"quarantine.test":            {"v=spf1 -all"},
		"_dmarc.quarantine.test":     {"v=DMARC1; p=quarantine"},
		"signed.test":                {"v=spf1 -all"},
		"_dmarc.signed.test":         {"v=DMARC1; p=reject; adkim=s"},
		"sel._domainkey.signed.test": {"v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub)},
	}
	rcpt := []string{"alice@example.test"}

	err = transact(e.mx, nil, "ceo@strict.test", rcpt, message("ceo@strict.test", "alice@example.test", "Wire money", ""))
	wantCode(t, err, 550, "5.7.1")

	if err := transact(e.mx, nil, "news@quarantine.test", rcpt, message("news@quarantine.test", "alice@example.test", "Deal", "")); err != nil {
		t.Fatal(err)
	}
	// The DKIM signature aligns with the From domain, so DMARC passes even
	// though SPF fails (as it does for forwarded mail).
	var signed bytes.Buffer
	err = dkim.Sign(&signed, bytes.NewReader(message("team@signed.test", "alice@example.test", "Signed", "")),
		&dkim.SignOptions{Domain: "signed.test", Selector: "sel", Signer: priv})
	if err != nil {
		t.Fatal(err)
	}
	if err := transact(e.mx, nil, "team@signed.test", rcpt, signed.Bytes()); err != nil {
		t.Fatal(err)
	}
	// Mail claiming to be from a local domain without authentication.
	if err := transact(e.mx, nil, "", rcpt, message("bob@example.test", "alice@example.test", "Spoof", "")); err != nil {
		t.Fatal(err)
	}

	inbox := e.mailbox(t, e.alice, store.SpecialInbox)
	junk := e.mailbox(t, e.alice, store.SpecialJunk)
	if len(inbox) != 1 || !strings.Contains(inbox[0], "Subject: Signed") {
		t.Fatalf("inbox = %d messages, want only the signed one", len(inbox))
	}
	for _, want := range []string{"spf=fail", "dkim=pass header.d=signed.test", "dmarc=pass header.from=signed.test"} {
		if !strings.Contains(inbox[0], want) {
			t.Errorf("missing %q in signed message", want)
		}
	}
	if len(junk) != 2 || !strings.Contains(junk[0]+junk[1], "Subject: Deal") || !strings.Contains(junk[0]+junk[1], "Subject: Spoof") {
		t.Fatalf("junk has %d messages, want the quarantined and the spoofed one", len(junk))
	}
}

func TestMXPartialFailureIsQueued(t *testing.T) {
	e := setup(t, true)
	ctx := context.Background()
	small, err := e.st.CreateUser(ctx, store.NewUser{Email: "small@example.test", Password: "correct horse", QuotaBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	err = transact(e.mx, nil, "carol@sender.test", []string{"alice@example.test", "small@example.test"},
		message("carol@sender.test", "alice@example.test, small@example.test", "Big", ""))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(e.mailbox(t, e.alice, store.SpecialInbox)); n != 1 {
		t.Fatalf("alice has %d messages, want 1", n)
	}
	if n := len(e.mailbox(t, small, store.SpecialInbox)); n != 0 {
		t.Fatalf("small has %d messages, want 0", n)
	}
	q, err := e.st.ListQueue(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 1 || len(q[0].Rcpts) != 1 || q[0].Rcpts[0].Rcpt != "small@example.test" || q[0].MailFrom != "carol@sender.test" {
		t.Fatalf("unexpected queue: %+v", q)
	}

	// When nobody can take the message, the sender is told so directly.
	err = transact(e.mx, nil, "carol@sender.test", []string{"small@example.test"}, message("carol@sender.test", "small@example.test", "Big", ""))
	wantCode(t, err, 552, "5.2.2")
}

func TestSubmission(t *testing.T) {
	e := setup(t, true)
	ctx := context.Background()
	login := sasl.NewLoginClient("alice@example.test", "correct horse")
	data := message("Alice <alice@example.test>", "bob@example.test", "Hi", "")

	wantCode(t, transact(e.sub, nil, "alice@example.test", []string{"bob@example.test"}, data), 530, "5.7.0")
	wantCode(t, transact(e.sub, sasl.NewPlainClient("", "alice@example.test", "wrong"), "alice@example.test", nil, data), 535, "5.7.8")
	wantCode(t, transact(e.sub, login, "bob@example.test", []string{"bob@example.test"}, data), 553, "5.7.1")
	wantCode(t, transact(e.sub, login, "alice@example.test", []string{"bob@example.test"},
		message("bob@example.test", "dave@remote.test", "Spoof", "")), 553, "5.7.1")

	raw := "From: Alice <alice@example.test>\r\nTo: bob@example.test\r\nBcc: dave@remote.test\r\n" +
		"Authentication-Results: mx.example.test; spf=pass\r\n" +
		"authentication-results: elsewhere.test; dkim=pass\r\n" +
		"Subject: Plan\r\n\r\nSee you.\r\n"
	err := transact(e.sub, sasl.NewPlainClient("", "alice@example.test", "correct horse"), "alice@example.test",
		[]string{"bob@example.test", "dave@remote.test"}, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	q, err := e.st.ListQueue(ctx, e.alice.ID)
	if err != nil || len(q) != 1 {
		t.Fatalf("queue = %v, %v; want one entry", q, err)
	}
	entry, err := e.st.GetQueueEntry(ctx, q[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	queued := string(entry.Data)
	if strings.Contains(queued, "Bcc:") || countHeader(entry.Data, "Authentication-Results") != 0 ||
		!strings.Contains(queued, "\r\nMessage-ID: <") || !strings.Contains(queued, "\r\nDate: ") {
		t.Fatalf("queued message not fixed up:\n%s", queued)
	}
	if !strings.HasPrefix(queued, "Received: from client.sender.test\r\n\tby mx.example.test (TitoMail) with ESMTPA id ") {
		t.Fatalf("unexpected Received field:\n%s", queued)
	}
	if entry.MessageID == "" || !strings.Contains(queued, "<"+entry.MessageID+">") {
		t.Fatalf("queue entry Message-ID %q does not match the message", entry.MessageID)
	}

	e.d.ProcessQueue(ctx)
	inbox := e.mailbox(t, e.bob, store.SpecialInbox)
	if len(inbox) != 1 || strings.Contains(inbox[0], "dave@remote.test") {
		t.Fatalf("bob's inbox = %d messages (Bcc must not leak)", len(inbox))
	}
	c, err := e.st.ListContacts(ctx, e.alice.ID, "", false, 10)
	if err != nil || len(c) != 2 {
		t.Fatalf("contacts = %d, %v; want both recipients", len(c), err)
	}
}

func TestSubmissionThrottlesAndRequiresTLS(t *testing.T) {
	e := setup(t, false)
	data := message("alice@example.test", "bob@example.test", "Hi", "")
	good := sasl.NewPlainClient("", "alice@example.test", "correct horse")
	if err := transact(e.sub, good, "alice@example.test", []string{"bob@example.test"}, data); err == nil {
		t.Fatal("authentication allowed without TLS")
	}

	tlsSend := func(a sasl.Client) error {
		c, err := gosmtp.DialStartTLS(e.sub, &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			return err
		}
		defer c.Close()
		if err := c.Auth(a); err != nil {
			return err
		}
		if err := c.Mail("alice@example.test", nil); err != nil {
			return err
		}
		if err := c.Rcpt("bob@example.test", nil); err != nil {
			return err
		}
		w, err := c.Data()
		if err != nil {
			return err
		}
		w.Write(data)
		return w.Close()
	}
	if err := tlsSend(good); err != nil {
		t.Fatalf("submission over STARTTLS failed: %v", err)
	}
	for range 10 {
		wantCode(t, tlsSend(sasl.NewPlainClient("", "alice@example.test", "guess")), 535, "5.7.8")
	}
	wantCode(t, tlsSend(good), 454, "4.7.0")
}

func TestHeaderRewrite(t *testing.T) {
	raw := []byte("A: 1\r\nBcc: x@y,\r\n z@w\r\nC: 3\n\r\nBody\r\nBcc: not a header\r\n")
	got := string(rewriteHeader(raw, []string{"X: new"}, func(f headerField) bool { return f.name == "Bcc" }))
	want := "X: new\r\nA: 1\r\nC: 3\n\r\nBody\r\nBcc: not a header\r\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := string(rewriteHeader([]byte("Subject: no body"), nil, nil)); got != "Subject: no body\r\n" {
		t.Fatalf("got %q", got)
	}
}
