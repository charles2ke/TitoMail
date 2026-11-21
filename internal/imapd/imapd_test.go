package imapd

import (
	"bufio"
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
	"math/big"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/charles2ke/TitoMail/internal/store"
)

func init() {
	store.PasswordCost = 4
	loginFailureDelay = 10 * time.Millisecond
}

const (
	testUser     = "alice@example.com"
	testPassword = "correct horse battery"
	waitTimeout  = 5 * time.Second
)

type testEnv struct {
	st   *store.Store
	srv  *Server
	addr string
	user *store.User
}

// newTestEnv starts a server allowing plaintext authentication.
func newTestEnv(t *testing.T, opts Options) *testEnv {
	t.Helper()
	opts.AllowInsecureAuth = true
	return startEnv(t, opts)
}

func startEnv(t *testing.T, opts Options) *testEnv {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.CreateDomain(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}
	env := &testEnv{st: st}
	env.user = env.createUser(t, testUser, 0)

	env.srv = New(st, opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	env.addr = ln.Addr().String()
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.srv.Serve(ln)
	}()
	t.Cleanup(func() {
		if err := env.srv.Close(); err != nil {
			t.Errorf("Server.Close: %v", err)
		}
		select {
		case <-done:
		case <-time.After(waitTimeout):
			t.Error("Serve did not return after Close")
		}
	})
	return env
}

func (e *testEnv) createUser(t *testing.T, addr string, quota int64) *store.User {
	t.Helper()
	u, err := e.st.CreateUser(context.Background(), store.NewUser{
		Email: addr, Password: testPassword, DisplayName: "Test", QuotaBytes: quota,
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (e *testEnv) dial(t *testing.T, h *imapclient.UnilateralDataHandler) *imapclient.Client {
	t.Helper()
	c, err := imapclient.DialInsecure(e.addr, &imapclient.Options{UnilateralDataHandler: h})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func (e *testEnv) login(t *testing.T, h *imapclient.UnilateralDataHandler) *imapclient.Client {
	t.Helper()
	c := e.dial(t, h)
	if err := c.Login(testUser, testPassword).Wait(); err != nil {
		t.Fatalf("LOGIN: %v", err)
	}
	return c
}

func (e *testEnv) mailbox(t *testing.T, name string) *store.Mailbox {
	t.Helper()
	mb, err := e.st.MailboxByName(context.Background(), e.user.ID, name)
	if err != nil {
		t.Fatalf("mailbox %q: %v", name, err)
	}
	return mb
}

// insert delivers a message through the store, as the SMTP server or the
// web UI would.
func (e *testEnv) insert(t *testing.T, mailbox string, raw []byte, received time.Time, flags ...string) *store.Message {
	t.Helper()
	if received.IsZero() {
		received = time.Now()
	}
	m, err := e.st.InsertMessage(context.Background(), store.NewMessage{
		UserID:     e.user.ID,
		MailboxID:  e.mailbox(t, mailbox).ID,
		Raw:        raw,
		Flags:      flags,
		ReceivedAt: received,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (e *testEnv) storedMessage(t *testing.T, m *store.Message) *store.Message {
	t.Helper()
	msgs, err := e.st.MessagesByUID(context.Background(), m.MailboxID, []uint32{m.UID})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("MessagesByUID(%d): %v, %d messages", m.UID, err, len(msgs))
	}
	return msgs[0]
}

var msgCounter atomic.Int64

func testMessage(from, subject, body string) []byte {
	return fmt.Appendf(nil, "From: %s\r\n"+
		"To: Alice <alice@example.com>\r\n"+
		"Subject: %s\r\n"+
		"Date: Mon, 02 Jan 2006 15:04:05 +0000\r\n"+
		"Message-ID: <msg%d@example.com>\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n"+
		"\r\n"+
		"%s\r\n", from, subject, msgCounter.Add(1), body)
}

func mustSelect(t *testing.T, c *imapclient.Client, name string, readOnly bool) *imap.SelectData {
	t.Helper()
	data, err := c.Select(name, &imap.SelectOptions{ReadOnly: readOnly}).Wait()
	if err != nil {
		t.Fatalf("SELECT %s: %v", name, err)
	}
	return data
}

func appendMsg(c *imapclient.Client, mailbox string, raw []byte, opts *imap.AppendOptions) (*imap.AppendData, error) {
	cmd := c.Append(mailbox, int64(len(raw)), opts)
	_, werr := cmd.Write(raw)
	cerr := cmd.Close()
	data, err := cmd.Wait()
	if err == nil {
		err = errors.Join(werr, cerr)
	}
	return data, err
}

func fetch(t *testing.T, c *imapclient.Client, numSet imap.NumSet, opts *imap.FetchOptions) []*imapclient.FetchMessageBuffer {
	t.Helper()
	msgs, err := c.Fetch(numSet, opts).Collect()
	if err != nil {
		t.Fatalf("FETCH %v: %v", numSet, err)
	}
	return msgs
}

func addFlags(t *testing.T, c *imapclient.Client, numSet imap.NumSet, flags ...imap.Flag) {
	t.Helper()
	_, err := c.Store(numSet, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: flags}, nil).Collect()
	if err != nil {
		t.Fatalf("STORE %v +FLAGS.SILENT %v: %v", numSet, flags, err)
	}
}

func status(t *testing.T, c *imapclient.Client, mailbox string) *imap.StatusData {
	t.Helper()
	data, err := c.Status(mailbox, &imap.StatusOptions{
		NumMessages: true, UIDNext: true, UIDValidity: true, NumUnseen: true, NumDeleted: true, Size: true,
	}).Wait()
	if err != nil {
		t.Fatalf("STATUS %s: %v", mailbox, err)
	}
	return data
}

func fetchUIDs(t *testing.T, c *imapclient.Client) []imap.UID {
	t.Helper()
	if c.Mailbox().NumMessages == 0 {
		return nil
	}
	var uids []imap.UID
	for _, msg := range fetch(t, c, imap.SeqSet{{Start: 1, Stop: 0}}, &imap.FetchOptions{UID: true}) {
		uids = append(uids, msg.UID)
	}
	return uids
}

func expectCode(t *testing.T, what string, err error, code imap.ResponseCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: unexpected success", what)
	}
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) || imapErr.Code != code {
		t.Fatalf("%s: got error %v, want response code %s", what, err, code)
	}
}

func hasFlag(flags []imap.Flag, f imap.Flag) bool {
	for _, x := range flags {
		if strings.EqualFold(string(x), string(f)) {
			return true
		}
	}
	return false
}

func sameFlags(got []imap.Flag, want ...imap.Flag) bool {
	if len(got) != len(want) {
		return false
	}
	for _, f := range want {
		if !hasFlag(got, f) {
			return false
		}
	}
	return true
}

func hasAttr(data *imap.ListData, attr imap.MailboxAttr) bool {
	for _, a := range data.Attrs {
		if strings.EqualFold(string(a), string(attr)) {
			return true
		}
	}
	return false
}

// updates records unilateral data received by a client.
type updates struct {
	mu       sync.Mutex
	exists   []uint32
	expunges []uint32
	fetches  []*imapclient.FetchMessageBuffer
	notify   chan struct{}
}

func newUpdates() *updates {
	return &updates{notify: make(chan struct{}, 1)}
}

func (u *updates) handler() *imapclient.UnilateralDataHandler {
	return &imapclient.UnilateralDataHandler{
		Mailbox: func(data *imapclient.UnilateralDataMailbox) {
			if data.NumMessages != nil {
				u.record(func() { u.exists = append(u.exists, *data.NumMessages) })
			}
		},
		Expunge: func(seqNum uint32) {
			u.record(func() { u.expunges = append(u.expunges, seqNum) })
		},
		Fetch: func(msg *imapclient.FetchMessageData) {
			buf, err := msg.Collect()
			if err != nil {
				return
			}
			u.record(func() { u.fetches = append(u.fetches, buf) })
		},
	}
}

func (u *updates) record(f func()) {
	u.mu.Lock()
	f()
	u.mu.Unlock()
	select {
	case u.notify <- struct{}{}:
	default:
	}
}

// take returns and clears the recorded updates.
func (u *updates) take() (exists, expunges []uint32, fetches []*imapclient.FetchMessageBuffer) {
	u.mu.Lock()
	defer u.mu.Unlock()
	exists, expunges, fetches = u.exists, u.expunges, u.fetches
	u.exists, u.expunges, u.fetches = nil, nil, nil
	return
}

// waitFor waits until cond, evaluated with the updates locked, is true.
func (u *updates) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(waitTimeout)
	for {
		u.mu.Lock()
		ok := cond()
		u.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-u.notify:
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestLogin(t *testing.T) {
	env := newTestEnv(t, Options{})
	c := env.dial(t, nil)

	expectCode(t, "LOGIN with wrong password", c.Login(testUser, "wrong password").Wait(), imap.ResponseCodeAuthenticationFailed)
	expectCode(t, "LOGIN with unknown user", c.Login("nobody@example.com", testPassword).Wait(), imap.ResponseCodeAuthenticationFailed)
	if err := c.Login(testUser, testPassword).Wait(); err != nil {
		t.Fatalf("LOGIN: %v", err)
	}

	caps, err := c.Capability().Wait()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []imap.Cap{
		imap.CapIMAP4rev1, imap.CapIMAP4rev2, imap.CapIdle, imap.CapMove, imap.CapUIDPlus,
		imap.CapNamespace, imap.CapSpecialUse, imap.CapListStatus, imap.CapESearch, imap.CapUnselect,
	} {
		if !caps.Has(name) {
			t.Errorf("capability %s not advertised", name)
		}
	}
	if limit, ok := caps.AppendLimit(); !ok || limit == nil || *limit != DefaultMaxMessageBytes {
		t.Errorf("APPENDLIMIT = %v (%v), want %d", limit, ok, DefaultMaxMessageBytes)
	}
}

func TestLoginThrottled(t *testing.T) {
	env := newTestEnv(t, Options{})
	c := env.dial(t, nil)
	for range 10 {
		expectCode(t, "LOGIN with wrong password", c.Login(testUser, "wrong password").Wait(), imap.ResponseCodeAuthenticationFailed)
	}
	expectCode(t, "LOGIN after too many failures", c.Login(testUser, testPassword).Wait(), imap.ResponseCodeUnavailable)
}

func TestTLS(t *testing.T) {
	serverConf, clientConf := testTLSConfigs(t)
	env := startEnv(t, Options{TLSConfig: serverConf})

	plain := env.dial(t, nil)
	if err := plain.Login(testUser, testPassword).Wait(); err == nil {
		t.Fatal("LOGIN without TLS succeeded")
	}

	c, err := imapclient.DialStartTLS(env.addr, &imapclient.Options{TLSConfig: clientConf})
	if err != nil {
		t.Fatalf("STARTTLS: %v", err)
	}
	defer c.Close()
	if err := c.Login(testUser, testPassword).Wait(); err != nil {
		t.Fatalf("LOGIN after STARTTLS: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- env.srv.ServeTLS(ln) }()
	tc, err := imapclient.DialTLS(ln.Addr().String(), &imapclient.Options{TLSConfig: clientConf})
	if err != nil {
		t.Fatalf("implicit TLS: %v", err)
	}
	defer tc.Close()
	if err := tc.Login(testUser, testPassword).Wait(); err != nil {
		t.Fatalf("LOGIN over implicit TLS: %v", err)
	}
	if _, err := tc.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT over implicit TLS: %v", err)
	}

	if err := env.srv.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-served:
	case <-time.After(waitTimeout):
		t.Fatal("ServeTLS did not return after Close")
	}
}

func testTLSConfigs(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	server = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	client = &tls.Config{RootCAs: pool, ServerName: "localhost"}
	return server, client
}

func TestList(t *testing.T) {
	env := newTestEnv(t, Options{})
	c := env.login(t, nil)

	list := func(ref, pattern string, opts *imap.ListOptions) map[string]*imap.ListData {
		t.Helper()
		items, err := c.List(ref, pattern, opts).Collect()
		if err != nil {
			t.Fatalf("LIST %q %q: %v", ref, pattern, err)
		}
		m := make(map[string]*imap.ListData)
		for _, item := range items {
			m[item.Mailbox] = item
		}
		return m
	}

	want := map[string]imap.MailboxAttr{
		"INBOX":   "",
		"Drafts":  imap.MailboxAttrDrafts,
		"Sent":    imap.MailboxAttrSent,
		"Archive": imap.MailboxAttrArchive,
		"Junk":    imap.MailboxAttrJunk,
		"Trash":   imap.MailboxAttrTrash,
	}
	all := list("", "*", nil)
	if len(all) != len(want) {
		t.Errorf("LIST returned %d mailboxes, want %d", len(all), len(want))
	}
	for name, attr := range want {
		data := all[name]
		if data == nil {
			t.Errorf("mailbox %s not listed", name)
			continue
		}
		if data.Delim != '/' {
			t.Errorf("%s: delimiter %q", name, data.Delim)
		}
		if !hasAttr(data, imap.MailboxAttrHasNoChildren) {
			t.Errorf("%s: attributes %v lack \\HasNoChildren", name, data.Attrs)
		}
		if attr != "" && !hasAttr(data, attr) {
			t.Errorf("%s: attributes %v lack %s", name, data.Attrs, attr)
		}
	}

	if err := c.Create("Projects/2024", nil).Wait(); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	all = list("", "*", nil)
	if d := all["Projects"]; d == nil || !hasAttr(d, imap.MailboxAttrHasChildren) {
		t.Errorf("Projects: %+v, want \\HasChildren", d)
	}
	if d := all["Projects/2024"]; d == nil || !hasAttr(d, imap.MailboxAttrHasNoChildren) {
		t.Errorf("Projects/2024: %+v, want \\HasNoChildren", d)
	}
	if top := list("", "%", nil); top["Projects/2024"] != nil || top["Projects"] == nil || len(top) != 7 {
		t.Errorf(`LIST "" "%%" returned %d mailboxes`, len(top))
	}
	if sub := list("Projects/", "%", nil); len(sub) != 1 || sub["Projects/2024"] == nil {
		t.Errorf(`LIST "Projects/" "%%" returned %v`, sub)
	}
	if m := list("", "inbox", nil); len(m) != 1 || m["INBOX"] == nil {
		t.Errorf(`LIST "" "inbox" returned %v`, m)
	}

	special := list("", "*", &imap.ListOptions{SelectSpecialUse: true})
	if len(special) != 5 || special["INBOX"] != nil || special["Trash"] == nil {
		t.Errorf("LIST (SPECIAL-USE) returned %d mailboxes", len(special))
	}

	items, err := c.List("", "", nil).Collect()
	if err != nil || len(items) != 1 || items[0].Delim != '/' {
		t.Errorf(`LIST "" "": %v, %+v`, err, items)
	}

	env.insert(t, "INBOX", testMessage("bob@example.com", "Hi", "hello"), time.Time{})
	withStatus := list("", "*", &imap.ListOptions{ReturnStatus: &imap.StatusOptions{NumMessages: true, NumUnseen: true}})
	if st := withStatus["INBOX"].Status; st == nil || st.NumMessages == nil || *st.NumMessages != 1 || *st.NumUnseen != 1 {
		t.Errorf("LIST RETURN (STATUS) for INBOX: %+v", st)
	}
	if st := withStatus["Trash"].Status; st == nil || st.NumMessages == nil || *st.NumMessages != 0 {
		t.Errorf("LIST RETURN (STATUS) for Trash: %+v", st)
	}
}

func TestMailboxManagement(t *testing.T) {
	env := newTestEnv(t, Options{})
	c := env.login(t, nil)

	if err := c.Create("Work", nil).Wait(); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	expectCode(t, "CREATE existing mailbox", c.Create("Work", nil).Wait(), imap.ResponseCodeAlreadyExists)

	subscribed := func() map[string]bool {
		t.Helper()
		items, err := c.List("", "*", &imap.ListOptions{SelectSubscribed: true}).Collect()
		if err != nil {
			t.Fatalf("LIST (SUBSCRIBED): %v", err)
		}
		m := make(map[string]bool)
		for _, item := range items {
			if !hasAttr(item, imap.MailboxAttrSubscribed) {
				t.Errorf("LIST (SUBSCRIBED): %s lacks \\Subscribed", item.Mailbox)
			}
			m[item.Mailbox] = true
		}
		return m
	}
	if err := c.Subscribe("Work").Wait(); err != nil {
		t.Fatalf("SUBSCRIBE: %v", err)
	}
	if !subscribed()["Work"] {
		t.Error("Work not listed as subscribed")
	}
	if err := c.Unsubscribe("Work").Wait(); err != nil {
		t.Fatalf("UNSUBSCRIBE: %v", err)
	}
	if subscribed()["Work"] {
		t.Error("Work still listed as subscribed")
	}

	env.insert(t, "Work", testMessage("bob@example.com", "Report", "numbers"), time.Time{})
	if err := c.Rename("Work", "Projects", nil).Wait(); err != nil {
		t.Fatalf("RENAME: %v", err)
	}
	if _, err := c.Status("Work", &imap.StatusOptions{NumMessages: true}).Wait(); err == nil {
		t.Error("STATUS of renamed mailbox succeeded")
	}
	if st := status(t, c, "Projects"); *st.NumMessages != 1 {
		t.Errorf("Projects has %d messages, want 1", *st.NumMessages)
	}

	// Renaming INBOX moves its messages to a new mailbox.
	env.insert(t, "INBOX", testMessage("bob@example.com", "Old", "old"), time.Time{})
	if err := c.Rename("INBOX", "Old Inbox", nil).Wait(); err != nil {
		t.Fatalf("RENAME INBOX: %v", err)
	}
	if st := status(t, c, "INBOX"); *st.NumMessages != 0 {
		t.Errorf("INBOX has %d messages after RENAME, want 0", *st.NumMessages)
	}
	if st := status(t, c, "Old Inbox"); *st.NumMessages != 1 {
		t.Errorf("Old Inbox has %d messages, want 1", *st.NumMessages)
	}

	expectCode(t, "DELETE Trash", c.Delete("Trash").Wait(), imap.ResponseCodeCannot)
	expectCode(t, "DELETE missing mailbox", c.Delete("Nope").Wait(), imap.ResponseCodeNonExistent)
	if err := c.Create("Projects/Q1", nil).Wait(); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	expectCode(t, "DELETE mailbox with children", c.Delete("Projects").Wait(), imap.ResponseCodeHasChildren)
	if err := c.Delete("Projects/Q1").Wait(); err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	if err := c.Delete("Projects").Wait(); err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	if _, err := env.st.MailboxByName(context.Background(), env.user.ID, "Projects"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Projects still exists: %v", err)
	}

	ns, err := c.Namespace().Wait()
	if err != nil {
		t.Fatalf("NAMESPACE: %v", err)
	}
	if len(ns.Personal) != 1 || ns.Personal[0].Prefix != "" || ns.Personal[0].Delim != '/' || len(ns.Other) != 0 || len(ns.Shared) != 0 {
		t.Errorf("NAMESPACE: %+v", ns)
	}
}

func TestAppendAndSelect(t *testing.T) {
	env := newTestEnv(t, Options{})
	c := env.login(t, nil)
	inbox := env.mailbox(t, "INBOX")

	date := time.Date(2023, 5, 17, 10, 30, 0, 0, time.FixedZone("", 2*60*60))
	raw1 := testMessage("bob@example.com", "First", "one")
	data, err := appendMsg(c, "INBOX", raw1, &imap.AppendOptions{Flags: []imap.Flag{imap.FlagSeen, "$Important"}, Time: date})
	if err != nil {
		t.Fatalf("APPEND: %v", err)
	}
	if data.UID != 1 || data.UIDValidity != inbox.UIDValidity {
		t.Errorf("APPENDUID = %d %d, want %d 1", data.UIDValidity, data.UID, inbox.UIDValidity)
	}
	raw2 := testMessage("carol@example.org", "Second", "two")
	if data, err := appendMsg(c, "inbox", raw2, nil); err != nil || data.UID != 2 {
		t.Fatalf("APPEND: %v, %+v", err, data)
	}
	_, err = appendMsg(c, "Nope", raw1, nil)
	expectCode(t, "APPEND to missing mailbox", err, imap.ResponseCodeTryCreate)

	sel := mustSelect(t, c, "INBOX", false)
	if sel.NumMessages != 2 || sel.UIDNext != 3 || sel.UIDValidity != inbox.UIDValidity {
		t.Errorf("SELECT: %d messages, UIDNEXT %d, UIDVALIDITY %d", sel.NumMessages, sel.UIDNext, sel.UIDValidity)
	}
	if !hasFlag(sel.PermanentFlags, imap.FlagWildcard) || !hasFlag(sel.PermanentFlags, imap.FlagDeleted) {
		t.Errorf("PERMANENTFLAGS %v", sel.PermanentFlags)
	}

	msgs := fetch(t, c, imap.SeqSetNum(1, 2), &imap.FetchOptions{UID: true, Flags: true, InternalDate: true, RFC822Size: true})
	if len(msgs) != 2 {
		t.Fatalf("FETCH returned %d messages", len(msgs))
	}
	if !msgs[0].InternalDate.Equal(date) {
		t.Errorf("INTERNALDATE %v, want %v", msgs[0].InternalDate, date)
	}
	if !sameFlags(msgs[0].Flags, imap.FlagSeen, "$Important") || len(msgs[1].Flags) != 0 {
		t.Errorf("FLAGS %v, %v", msgs[0].Flags, msgs[1].Flags)
	}
	if msgs[0].RFC822Size != int64(len(raw1)) || msgs[1].UID != 2 {
		t.Errorf("RFC822.SIZE %d (want %d), UID %d", msgs[0].RFC822Size, len(raw1), msgs[1].UID)
	}

	st := status(t, c, "INBOX")
	if *st.NumMessages != 2 || st.UIDNext != 3 || st.UIDValidity != inbox.UIDValidity ||
		*st.NumUnseen != 1 || *st.NumDeleted != 0 || *st.Size != int64(len(raw1)+len(raw2)) {
		t.Errorf("STATUS: messages %d, uidnext %d, uidvalidity %d, unseen %d, deleted %d, size %d",
			*st.NumMessages, st.UIDNext, st.UIDValidity, *st.NumUnseen, *st.NumDeleted, *st.Size)
	}
}

func TestAppendLimits(t *testing.T) {
	env := newTestEnv(t, Options{MaxMessageBytes: 5000})
	c := env.login(t, nil)

	caps, err := c.Capability().Wait()
	if err != nil {
		t.Fatal(err)
	}
	if limit, ok := caps.AppendLimit(); !ok || limit == nil || *limit != 5000 {
		t.Errorf("APPENDLIMIT = %v (%v), want 5000", limit, ok)
	}

	big := testMessage("bob@example.com", "Big", strings.Repeat(strings.Repeat("x", 70)+"\r\n", 90))
	_, err = appendMsg(c, "INBOX", big, nil)
	expectCode(t, "APPEND over APPENDLIMIT", err, imap.ResponseCodeTooBig)

	// imapclient drops its connection after a rejected synchronizing
	// literal, so check with a raw client that the session stays usable.
	rc := dialRaw(t, env.addr)
	rc.do(fmt.Sprintf("LOGIN %s %q", testUser, testPassword))
	tag := rc.nextTag()
	rc.write(fmt.Sprintf("%s APPEND INBOX {%d}\r\n", tag, len(big)))
	if line := rc.readLine(); !strings.HasPrefix(line, tag+" NO [TOOBIG]") {
		t.Errorf("APPEND over APPENDLIMIT: %q", line)
	}
	rc.do("NOOP")

	env.createUser(t, "bob@example.com", 2000)
	bob := env.dial(t, nil)
	if err := bob.Login("bob@example.com", testPassword).Wait(); err != nil {
		t.Fatal(err)
	}
	quota := testMessage("alice@example.com", "Quota", strings.Repeat(strings.Repeat("y", 70)+"\r\n", 40))
	_, err = appendMsg(bob, "INBOX", quota, nil)
	expectCode(t, "APPEND over quota", err, imap.ResponseCodeOverQuota)
	if _, err := appendMsg(bob, "INBOX", testMessage("alice@example.com", "Small", "ok"), nil); err != nil {
		t.Errorf("APPEND within quota: %v", err)
	}
}

func TestFetch(t *testing.T) {
	env := newTestEnv(t, Options{})
	raw := []byte("From: Bob Smith <bob@example.com>\r\n" +
		"To: Alice <alice@example.com>, carol@example.org\r\n" +
		"Subject: Lunch plans\r\n" +
		"Date: Tue, 03 Jan 2006 10:00:00 +0100\r\n" +
		"Message-ID: <lunch@example.com>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"Pizza at noon?\r\n")
	m1 := env.insert(t, "INBOX", raw, time.Time{})
	m2 := env.insert(t, "INBOX", testMessage("carol@example.org", "Other", "text"), time.Time{})
	c := env.login(t, nil)
	mustSelect(t, c, "INBOX", false)

	headerSec := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Subject", "From"}, Peek: true}
	textSec := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierText, Peek: true}
	fullSec := &imap.FetchItemBodySection{Peek: true}
	partialSec := &imap.FetchItemBodySection{Peek: true, Partial: &imap.SectionPartial{Offset: 6, Size: 3}}
	msgs := fetch(t, c, imap.SeqSetNum(1), &imap.FetchOptions{
		UID: true, Flags: true, Envelope: true, RFC822Size: true, InternalDate: true,
		BodySection: []*imap.FetchItemBodySection{headerSec, textSec, fullSec, partialSec},
	})
	if len(msgs) != 1 {
		t.Fatalf("FETCH returned %d messages", len(msgs))
	}
	msg := msgs[0]
	if msg.UID != imap.UID(m1.UID) || len(msg.Flags) != 0 || msg.RFC822Size != int64(len(raw)) {
		t.Errorf("UID %d, FLAGS %v, RFC822.SIZE %d", msg.UID, msg.Flags, msg.RFC822Size)
	}
	if msg.InternalDate.Unix() != m1.ReceivedAt.Unix() {
		t.Errorf("INTERNALDATE %v, want %v", msg.InternalDate, m1.ReceivedAt)
	}
	envl := msg.Envelope
	if envl == nil {
		t.Fatal("no ENVELOPE")
	}
	if envl.Subject != "Lunch plans" || envl.MessageID != "lunch@example.com" || len(envl.To) != 2 ||
		len(envl.From) != 1 || envl.From[0].Name != "Bob Smith" || envl.From[0].Mailbox != "bob" || envl.From[0].Host != "example.com" {
		t.Errorf("ENVELOPE %+v", envl)
	}
	if !envl.Date.Equal(time.Date(2006, 1, 3, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("ENVELOPE date %v", envl.Date)
	}
	if got := string(msg.FindBodySection(fullSec)); got != string(raw) {
		t.Errorf("BODY[] = %q", got)
	}
	if got := string(msg.FindBodySection(textSec)); got != "Pizza at noon?\r\n" {
		t.Errorf("BODY[TEXT] = %q", got)
	}
	if got := string(msg.FindBodySection(headerSec)); got != "From: Bob Smith <bob@example.com>\r\nSubject: Lunch plans\r\n\r\n" {
		t.Errorf("BODY[HEADER.FIELDS (Subject From)] = %q", got)
	}
	if got := string(msg.FindBodySection(partialSec)); got != "Bob" {
		t.Errorf("BODY[]<6.3> = %q", got)
	}

	// BODY.PEEK doesn't set \Seen; BODY[] does, and reports it.
	if env.storedMessage(t, m1).Seen {
		t.Fatal("BODY.PEEK[] set \\Seen")
	}
	bodySec := &imap.FetchItemBodySection{}
	msgs = fetch(t, c, imap.SeqSetNum(1), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{bodySec}})
	if len(msgs) != 1 || !hasFlag(msgs[0].Flags, imap.FlagSeen) || string(msgs[0].FindBodySection(bodySec)) != string(raw) {
		t.Errorf("FETCH BODY[]: %d messages, flags %v", len(msgs), msgs[0].Flags)
	}
	if !env.storedMessage(t, m1).Seen {
		t.Error("BODY[] did not set \\Seen")
	}

	// "*" refers to the last message.
	msgs = fetch(t, c, imap.SeqSet{{Start: 2, Stop: 0}}, &imap.FetchOptions{UID: true})
	if len(msgs) != 1 || msgs[0].SeqNum != 2 || msgs[0].UID != imap.UID(m2.UID) {
		t.Errorf("FETCH 2:* returned %+v", msgs)
	}

	// EXAMINE never sets \Seen.
	sel := mustSelect(t, c, "INBOX", true)
	if len(sel.PermanentFlags) != 0 {
		t.Errorf("EXAMINE PERMANENTFLAGS %v", sel.PermanentFlags)
	}
	msgs = fetch(t, c, imap.SeqSetNum(2), &imap.FetchOptions{Flags: true, BodySection: []*imap.FetchItemBodySection{bodySec}})
	if len(msgs) != 1 || hasFlag(msgs[0].Flags, imap.FlagSeen) {
		t.Errorf("FETCH BODY[] in EXAMINE: %+v", msgs)
	}
	if env.storedMessage(t, m2).Seen {
		t.Error("BODY[] in EXAMINE set \\Seen")
	}
}

const multipartMessage = "From: Alice <alice@example.com>\r\n" +
	"To: Bob <bob@example.com>\r\n" +
	"Subject: Report\r\n" +
	"Date: Tue, 03 Jan 2006 10:00:00 +0000\r\n" +
	"Message-ID: <report@example.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"XYZ\"\r\n" +
	"\r\n" +
	"--XYZ\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"See attached.\r\n" +
	"--XYZ\r\n" +
	"Content-Type: application/octet-stream; name=\"data.bin\"\r\n" +
	"Content-Disposition: attachment; filename=\"data.bin\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"aGVsbG8gd29ybGQ=\r\n" +
	"--XYZ--\r\n"

func TestFetchMultipart(t *testing.T) {
	env := newTestEnv(t, Options{})
	env.insert(t, "INBOX", []byte(multipartMessage), time.Time{})
	c := env.login(t, nil)
	mustSelect(t, c, "INBOX", false)

	part1 := &imap.FetchItemBodySection{Part: []int{1}, Peek: true}
	mime2 := &imap.FetchItemBodySection{Part: []int{2}, Specifier: imap.PartSpecifierMIME, Peek: true}
	bin2 := &imap.FetchItemBinarySection{Part: []int{2}, Peek: true}
	msgs := fetch(t, c, imap.SeqSetNum(1), &imap.FetchOptions{
		Flags:             true,
		BodyStructure:     &imap.FetchItemBodyStructure{Extended: true},
		BodySection:       []*imap.FetchItemBodySection{part1, mime2},
		BinarySection:     []*imap.FetchItemBinarySection{bin2},
		BinarySectionSize: []*imap.FetchItemBinarySectionSize{{Part: []int{2}}},
	})
	if len(msgs) != 1 {
		t.Fatalf("FETCH returned %d messages", len(msgs))
	}
	msg := msgs[0]
	mp, ok := msg.BodyStructure.(*imap.BodyStructureMultiPart)
	if !ok || !strings.EqualFold(mp.Subtype, "mixed") || len(mp.Children) != 2 {
		t.Fatalf("BODYSTRUCTURE %#v", msg.BodyStructure)
	}
	text, ok := mp.Children[0].(*imap.BodyStructureSinglePart)
	if !ok || !strings.EqualFold(text.Type, "text") || !strings.EqualFold(text.Subtype, "plain") || text.Params["charset"] != "utf-8" {
		t.Errorf("part 1: %#v", mp.Children[0])
	}
	att, ok := mp.Children[1].(*imap.BodyStructureSinglePart)
	if !ok || !strings.EqualFold(att.Type, "application") || !strings.EqualFold(att.Encoding, "base64") || att.Size != 16 {
		t.Fatalf("part 2: %#v", mp.Children[1])
	}
	if att.Extended == nil || att.Extended.Disposition == nil ||
		!strings.EqualFold(att.Extended.Disposition.Value, "attachment") || att.Extended.Disposition.Params["filename"] != "data.bin" {
		t.Errorf("part 2 disposition: %#v", att.Extended)
	}
	if got := string(msg.FindBodySection(part1)); got != "See attached." {
		t.Errorf("BODY[1] = %q", got)
	}
	if got := string(msg.FindBodySection(mime2)); !strings.HasPrefix(got, "Content-Type: application/octet-stream") || !strings.HasSuffix(got, "base64\r\n\r\n") {
		t.Errorf("BODY[2.MIME] = %q", got)
	}
	if got := string(msg.FindBinarySection(bin2)); got != "hello world" {
		t.Errorf("BINARY[2] = %q", got)
	}
	if size, ok := msg.FindBinarySectionSize([]int{2}); !ok || size != 11 {
		t.Errorf("BINARY.SIZE[2] = %d, %v", size, ok)
	}
	if len(msg.Flags) != 0 {
		t.Errorf("peeking set flags %v", msg.Flags)
	}

	// Non-extended BODY.
	msgs = fetch(t, c, imap.SeqSetNum(1), &imap.FetchOptions{BodyStructure: &imap.FetchItemBodyStructure{}})
	if mp, ok := msgs[0].BodyStructure.(*imap.BodyStructureMultiPart); !ok || len(mp.Children) != 2 {
		t.Errorf("BODY %#v", msgs[0].BodyStructure)
	}
}

func TestSearch(t *testing.T) {
	env := newTestEnv(t, Options{})
	day := func(month time.Month, d int) time.Time { return time.Date(2024, month, d, 12, 0, 0, 0, time.UTC) }
	env.insert(t, "INBOX", testMessage("bob@example.com", "Hello world", "apples and bananas"), day(1, 10), store.FlagSeen)
	env.insert(t, "INBOX", testMessage("Carol <carol@example.org>", "Meeting notes", "quarterly numbers"), day(2, 10))
	env.insert(t, "INBOX", testMessage("Bob <bob@example.com>", "Re: Hello world", "cherry pie"), day(3, 10), store.FlagFlagged)
	c := env.login(t, nil)
	mustSelect(t, c, "INBOX", false)

	header := func(key, value string) []imap.SearchCriteriaHeaderField {
		return []imap.SearchCriteriaHeaderField{{Key: key, Value: value}}
	}
	date := func(y int, month time.Month, d int) time.Time { return time.Date(y, month, d, 0, 0, 0, 0, time.UTC) }
	tests := []struct {
		name     string
		criteria imap.SearchCriteria
		want     []imap.UID
	}{
		{"all", imap.SearchCriteria{}, []imap.UID{1, 2, 3}},
		{"subject", imap.SearchCriteria{Header: header("Subject", "hello")}, []imap.UID{1, 3}},
		{"from", imap.SearchCriteria{Header: header("From", "BOB")}, []imap.UID{1, 3}},
		{"from name", imap.SearchCriteria{Header: header("From", "carol")}, []imap.UID{2}},
		{"to", imap.SearchCriteria{Header: header("To", "alice@example.com")}, []imap.UID{1, 2, 3}},
		{"header present", imap.SearchCriteria{Header: header("Message-Id", "")}, []imap.UID{1, 2, 3}},
		{"header missing", imap.SearchCriteria{Header: header("X-Missing", "")}, nil},
		{"text body", imap.SearchCriteria{Text: []string{"banana"}}, []imap.UID{1}},
		{"text header", imap.SearchCriteria{Text: []string{"meeting"}}, []imap.UID{2}},
		{"body", imap.SearchCriteria{Body: []string{"QUARTERLY"}}, []imap.UID{2}},
		{"body excludes header", imap.SearchCriteria{Body: []string{"hello"}}, nil},
		{"unseen", imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, []imap.UID{2, 3}},
		{"flagged", imap.SearchCriteria{Flag: []imap.Flag{imap.FlagFlagged}}, []imap.UID{3}},
		{"or", imap.SearchCriteria{Or: [][2]imap.SearchCriteria{{
			{Header: header("From", "carol")}, {Flag: []imap.Flag{imap.FlagFlagged}},
		}}}, []imap.UID{2, 3}},
		{"not", imap.SearchCriteria{Not: []imap.SearchCriteria{{Header: header("From", "bob")}}}, []imap.UID{2}},
		{"since", imap.SearchCriteria{Since: date(2024, 2, 10)}, []imap.UID{2, 3}},
		{"before", imap.SearchCriteria{Before: date(2024, 2, 10)}, []imap.UID{1}},
		{"sentsince", imap.SearchCriteria{SentSince: date(2006, 1, 2)}, []imap.UID{1, 2, 3}},
		{"sentbefore", imap.SearchCriteria{SentBefore: date(2006, 1, 2)}, nil},
		{"uid range", imap.SearchCriteria{UID: []imap.UIDSet{{{Start: 2, Stop: 0}}}}, []imap.UID{2, 3}},
		{"seq and subject", imap.SearchCriteria{SeqNum: []imap.SeqSet{imap.SeqSetNum(1, 2)}, Header: header("Subject", "hello")}, []imap.UID{1}},
		{"larger", imap.SearchCriteria{Larger: 1}, []imap.UID{1, 2, 3}},
		{"smaller", imap.SearchCriteria{Smaller: 1}, nil},
	}
	for _, tc := range tests {
		data, err := c.UIDSearch(&tc.criteria, nil).Wait()
		if err != nil {
			t.Errorf("%s: UID SEARCH: %v", tc.name, err)
			continue
		}
		if got := data.AllUIDs(); !slices.Equal(got, tc.want) {
			t.Errorf("%s: UID SEARCH = %v, want %v", tc.name, got, tc.want)
		}
	}

	data, err := c.Search(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, nil).Wait()
	if err != nil || !slices.Equal(data.AllSeqNums(), []uint32{2, 3}) {
		t.Errorf("SEARCH UNSEEN = %v, %v", data.AllSeqNums(), err)
	}

	data, err = c.UIDSearch(&imap.SearchCriteria{Header: header("Subject", "hello")},
		&imap.SearchOptions{ReturnMin: true, ReturnMax: true, ReturnCount: true}).Wait()
	if err != nil || data.Min != 1 || data.Max != 3 || data.Count != 2 {
		t.Errorf("UID SEARCH RETURN (MIN MAX COUNT): %+v, %v", data, err)
	}

	// SENTSINCE uses the date in the Date field's own time zone.
	env.insert(t, "Archive", []byte("From: bob@example.com\r\nSubject: Late\r\nDate: Tue, 03 Jan 2006 01:00:00 +0200\r\n\r\nlate\r\n"), time.Time{})
	mustSelect(t, c, "Archive", true)
	for _, tc := range []struct {
		criteria imap.SearchCriteria
		want     []imap.UID
	}{
		{imap.SearchCriteria{SentSince: date(2006, 1, 3)}, []imap.UID{1}},
		{imap.SearchCriteria{SentBefore: date(2006, 1, 3)}, nil},
		{imap.SearchCriteria{SentBefore: date(2006, 1, 4)}, []imap.UID{1}},
	} {
		data, err := c.UIDSearch(&tc.criteria, nil).Wait()
		if err != nil || !slices.Equal(data.AllUIDs(), tc.want) {
			t.Errorf("UID SEARCH %+v = %v, %v; want %v", tc.criteria, data.AllUIDs(), err, tc.want)
		}
	}
}

func TestStoreFlags(t *testing.T) {
	env := newTestEnv(t, Options{})
	m1 := env.insert(t, "INBOX", testMessage("bob@example.com", "One", "1"), time.Time{})
	env.insert(t, "INBOX", testMessage("bob@example.com", "Two", "2"), time.Time{})
	u := newUpdates()
	c := env.login(t, u.handler())
	mustSelect(t, c, "INBOX", false)

	storeFlags := func(numSet imap.NumSet, op imap.StoreFlagsOp, silent bool, flags ...imap.Flag) []*imapclient.FetchMessageBuffer {
		t.Helper()
		msgs, err := c.Store(numSet, &imap.StoreFlags{Op: op, Silent: silent, Flags: flags}, nil).Collect()
		if err != nil {
			t.Fatalf("STORE: %v", err)
		}
		return msgs
	}
	if msgs := storeFlags(imap.SeqSetNum(1), imap.StoreFlagsAdd, false, imap.FlagFlagged, "$Work"); len(msgs) != 1 ||
		msgs[0].SeqNum != 1 || !sameFlags(msgs[0].Flags, imap.FlagFlagged, "$Work") {
		t.Errorf("STORE +FLAGS: %+v", msgs)
	}
	if msgs := storeFlags(imap.SeqSetNum(1), imap.StoreFlagsDel, false, imap.FlagFlagged); len(msgs) != 1 || !sameFlags(msgs[0].Flags, "$Work") {
		t.Errorf("STORE -FLAGS: %+v", msgs)
	}
	if msgs := storeFlags(imap.SeqSetNum(1), imap.StoreFlagsSet, false, imap.FlagAnswered); len(msgs) != 1 || !sameFlags(msgs[0].Flags, imap.FlagAnswered) {
		t.Errorf("STORE FLAGS: %+v", msgs)
	}
	if msgs := storeFlags(imap.SeqSet{{Start: 1, Stop: 0}}, imap.StoreFlagsAdd, true, imap.FlagSeen); len(msgs) != 0 {
		t.Errorf("STORE +FLAGS.SILENT returned %d FETCH responses", len(msgs))
	}
	if msgs := storeFlags(imap.UIDSetNum(2), imap.StoreFlagsAdd, false, imap.FlagDeleted); len(msgs) != 1 ||
		msgs[0].UID != 2 || !sameFlags(msgs[0].Flags, imap.FlagSeen, imap.FlagDeleted) {
		t.Errorf("UID STORE +FLAGS: %+v", msgs)
	}

	msgs := fetch(t, c, imap.SeqSetNum(1, 2), &imap.FetchOptions{Flags: true})
	if len(msgs) != 2 || !sameFlags(msgs[0].Flags, imap.FlagAnswered, imap.FlagSeen) || !sameFlags(msgs[1].Flags, imap.FlagSeen, imap.FlagDeleted) {
		t.Errorf("FETCH FLAGS: %+v", msgs)
	}
	if m := env.storedMessage(t, m1); !m.Answered || !m.Seen || m.Flagged || len(m.Keywords) != 0 {
		t.Errorf("stored message: %+v", m)
	}

	// The session's own changes are not echoed as unsolicited updates.
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if _, _, fetches := u.take(); len(fetches) != 0 {
		t.Errorf("got %d unsolicited FETCH responses", len(fetches))
	}
}

func TestCopy(t *testing.T) {
	env := newTestEnv(t, Options{})
	for i := 0; i < 3; i++ {
		env.insert(t, "INBOX", testMessage("bob@example.com", fmt.Sprint("Message ", i), "body"), time.Time{})
	}
	archive := env.mailbox(t, "Archive")
	c := env.login(t, nil)
	mustSelect(t, c, "INBOX", false)

	data, err := c.Copy(imap.SeqSetNum(1, 3), "Archive").Wait()
	if err != nil {
		t.Fatalf("COPY: %v", err)
	}
	if data.UIDValidity != archive.UIDValidity || data.SourceUIDs.String() != "1,3" || data.DestUIDs.String() != "1:2" {
		t.Errorf("COPYUID %d %v %v", data.UIDValidity, data.SourceUIDs, data.DestUIDs)
	}
	_, err = c.Copy(imap.SeqSetNum(1), "Nope").Wait()
	expectCode(t, "COPY to missing mailbox", err, imap.ResponseCodeTryCreate)

	if st := status(t, c, "Archive"); *st.NumMessages != 2 {
		t.Errorf("Archive has %d messages", *st.NumMessages)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if n := c.Mailbox().NumMessages; n != 3 {
		t.Errorf("INBOX has %d messages after COPY", n)
	}
}

func TestMove(t *testing.T) {
	env := newTestEnv(t, Options{})
	for i := 0; i < 4; i++ {
		env.insert(t, "INBOX", testMessage("bob@example.com", fmt.Sprint("Message ", i), "body"), time.Time{})
	}
	trash := env.mailbox(t, "Trash")
	archive := env.mailbox(t, "Archive")
	u := newUpdates()
	c := env.login(t, u.handler())
	mustSelect(t, c, "INBOX", false)

	data, err := c.Move(imap.SeqSetNum(2), "Trash").Wait()
	if err != nil {
		t.Fatalf("MOVE: %v", err)
	}
	if data.UIDValidity != trash.UIDValidity || data.SourceUIDs.String() != "2" || data.DestUIDs.String() != "1" {
		t.Errorf("COPYUID %d %v %v", data.UIDValidity, data.SourceUIDs, data.DestUIDs)
	}
	if _, expunges, _ := u.take(); !slices.Equal(expunges, []uint32{2}) {
		t.Errorf("MOVE: EXPUNGE responses %v, want [2]", expunges)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if exists, expunges, _ := u.take(); len(expunges) != 0 || len(exists) != 0 {
		t.Errorf("NOOP after MOVE: EXISTS %v, EXPUNGE %v", exists, expunges)
	}

	// UIDs 1, 3, 4 remain; UID MOVE 3:4 expunges sequence numbers 2 and 3.
	data, err = c.Move(imap.UIDSetNum(3, 4), "Archive").Wait()
	if err != nil {
		t.Fatalf("UID MOVE: %v", err)
	}
	if data.UIDValidity != archive.UIDValidity || data.SourceUIDs.String() != "3:4" || data.DestUIDs.String() != "1:2" {
		t.Errorf("COPYUID %d %v %v", data.UIDValidity, data.SourceUIDs, data.DestUIDs)
	}
	if _, expunges, _ := u.take(); !slices.Equal(expunges, []uint32{3, 2}) {
		t.Errorf("UID MOVE: EXPUNGE responses %v, want [3 2]", expunges)
	}
	if uids := fetchUIDs(t, c); !slices.Equal(uids, []imap.UID{1}) {
		t.Errorf("INBOX UIDs after MOVE: %v", uids)
	}
	if n := c.Mailbox().NumMessages; n != 1 {
		t.Errorf("client counts %d messages", n)
	}
	if st := status(t, c, "Trash"); *st.NumMessages != 1 {
		t.Errorf("Trash has %d messages", *st.NumMessages)
	}
	if st := status(t, c, "Archive"); *st.NumMessages != 2 {
		t.Errorf("Archive has %d messages", *st.NumMessages)
	}

	_, err = c.Move(imap.SeqSetNum(1), "Nope").Wait()
	expectCode(t, "MOVE to missing mailbox", err, imap.ResponseCodeTryCreate)
}

func TestExpunge(t *testing.T) {
	env := newTestEnv(t, Options{})
	inbox := env.mailbox(t, "INBOX")
	for i := 0; i < 5; i++ {
		env.insert(t, "INBOX", testMessage("bob@example.com", fmt.Sprint("Message ", i), "body"), time.Time{})
	}
	u := newUpdates()
	c := env.login(t, u.handler())
	mustSelect(t, c, "INBOX", false)

	addFlags(t, c, imap.SeqSetNum(1, 3), imap.FlagDeleted)
	seqNums, err := c.Expunge().Collect()
	if err != nil || !slices.Equal(seqNums, []uint32{3, 1}) {
		t.Errorf("EXPUNGE = %v, %v; want [3 1]", seqNums, err)
	}

	// UIDs 2, 4, 5 remain. UID EXPUNGE only removes listed \Deleted messages.
	addFlags(t, c, imap.SeqSetNum(1, 2), imap.FlagDeleted)
	seqNums, err = c.UIDExpunge(imap.UIDSetNum(4, 5)).Collect()
	if err != nil || !slices.Equal(seqNums, []uint32{2}) {
		t.Errorf("UID EXPUNGE = %v, %v; want [2]", seqNums, err)
	}
	if uids := fetchUIDs(t, c); !slices.Equal(uids, []imap.UID{2, 5}) {
		t.Errorf("UIDs after UID EXPUNGE: %v", uids)
	}

	// CLOSE expunges silently.
	if err := c.UnselectAndExpunge().Wait(); err != nil {
		t.Fatalf("CLOSE: %v", err)
	}
	if _, expunges, _ := u.take(); len(expunges) != 0 {
		t.Errorf("unexpected unsolicited EXPUNGE responses %v", expunges)
	}
	uids, err := env.st.MailboxUIDs(context.Background(), inbox.ID)
	if err != nil || !slices.Equal(uids, []uint32{5}) {
		t.Errorf("UIDs after CLOSE: %v, %v", uids, err)
	}
}

func TestExamineIsReadOnly(t *testing.T) {
	env := newTestEnv(t, Options{})
	m := env.insert(t, "INBOX", testMessage("bob@example.com", "Hi", "hello"), time.Time{}, store.FlagDeleted)
	c := env.login(t, nil)
	mustSelect(t, c, "INBOX", true)

	if _, err := c.Store(imap.SeqSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagFlagged}}, nil).Collect(); err == nil {
		t.Error("STORE succeeded in read-only mailbox")
	}
	if _, err := c.Expunge().Collect(); err == nil {
		t.Error("EXPUNGE succeeded in read-only mailbox")
	}
	if _, err := c.Move(imap.SeqSetNum(1), "Trash").Wait(); err == nil {
		t.Error("MOVE succeeded in read-only mailbox")
	}
	if _, err := c.Copy(imap.SeqSetNum(1), "Archive").Wait(); err != nil {
		t.Errorf("COPY from read-only mailbox: %v", err)
	}
	// CLOSE doesn't expunge read-only mailboxes.
	if err := c.UnselectAndExpunge().Wait(); err != nil {
		t.Fatalf("CLOSE: %v", err)
	}
	if got := env.storedMessage(t, m); got.Flagged || !got.Deleted {
		t.Errorf("message modified through EXAMINE: %+v", got)
	}
}

func TestIdleNewMessage(t *testing.T) {
	env := newTestEnv(t, Options{})
	u := newUpdates()
	c := env.login(t, u.handler())
	mustSelect(t, c, "INBOX", false)

	idle, err := c.Idle()
	if err != nil {
		t.Fatalf("IDLE: %v", err)
	}
	env.insert(t, "INBOX", testMessage("bob@example.com", "New", "fresh"), time.Time{})
	u.waitFor(t, "EXISTS", func() bool { return len(u.exists) > 0 })
	// Deliveries to other mailboxes don't concern the selected one.
	env.insert(t, "Archive", testMessage("bob@example.com", "Elsewhere", "other"), time.Time{})
	env.insert(t, "INBOX", testMessage("bob@example.com", "Newer", "fresh"), time.Time{})
	u.waitFor(t, "second EXISTS", func() bool { return len(u.exists) > 1 })
	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := idle.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if exists, _, _ := u.take(); !slices.Equal(exists, []uint32{1, 2}) {
		t.Errorf("EXISTS responses %v, want [1 2]", exists)
	}

	msgs := fetch(t, c, imap.SeqSetNum(2), &imap.FetchOptions{Envelope: true})
	if len(msgs) != 1 || msgs[0].Envelope.Subject != "Newer" {
		t.Errorf("FETCH 2 ENVELOPE: %+v", msgs)
	}
}

func TestIdleFlagChanges(t *testing.T) {
	env := newTestEnv(t, Options{})
	m := env.insert(t, "INBOX", testMessage("bob@example.com", "Hi", "hello"), time.Time{})
	u := newUpdates()
	c := env.login(t, u.handler())
	mustSelect(t, c, "INBOX", false)

	idle, err := c.Idle()
	if err != nil {
		t.Fatalf("IDLE: %v", err)
	}
	// A change made in the web UI.
	if err := env.st.UpdateFlagsByID(context.Background(), env.user.ID, []int64{m.ID}, store.FlagsAdd, []string{store.FlagFlagged, store.FlagSeen}); err != nil {
		t.Fatal(err)
	}
	u.waitFor(t, "FETCH FLAGS", func() bool { return len(u.fetches) > 0 })
	if err := idle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := idle.Wait(); err != nil {
		t.Fatal(err)
	}
	_, _, fetches := u.take()
	if len(fetches) != 1 {
		t.Fatalf("got %d FETCH responses", len(fetches))
	}
	if f := fetches[0]; f.SeqNum != 1 || f.UID != imap.UID(m.UID) || !sameFlags(f.Flags, imap.FlagFlagged, imap.FlagSeen) {
		t.Errorf("FETCH %d (UID %d FLAGS %v)", f.SeqNum, f.UID, f.Flags)
	}
}

func TestConcurrentSessions(t *testing.T) {
	env := newTestEnv(t, Options{})
	for i := 0; i < 3; i++ {
		env.insert(t, "INBOX", testMessage("bob@example.com", fmt.Sprint("Message ", i), "body"), time.Time{})
	}
	u := newUpdates()
	a := env.login(t, u.handler())
	b := env.login(t, nil)
	mustSelect(t, a, "INBOX", false)
	mustSelect(t, b, "INBOX", false)

	if _, err := b.Move(imap.SeqSetNum(2), "Trash").Wait(); err != nil {
		t.Fatalf("MOVE: %v", err)
	}
	// EXPUNGE must not be sent during FETCH; the expunged message is
	// simply missing from the results.
	msgs := fetch(t, a, imap.SeqSetNum(1, 2, 3), &imap.FetchOptions{UID: true})
	if len(msgs) != 2 || msgs[0].UID != 1 || msgs[1].SeqNum != 3 || msgs[1].UID != 3 {
		t.Errorf("FETCH after concurrent MOVE: %+v", msgs)
	}
	if _, expunges, _ := u.take(); len(expunges) != 0 {
		t.Errorf("EXPUNGE %v sent during FETCH", expunges)
	}
	if err := a.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if _, expunges, _ := u.take(); !slices.Equal(expunges, []uint32{2}) {
		t.Errorf("NOOP: EXPUNGE %v, want [2]", expunges)
	}

	// Flag changes by another session are reported, including .SILENT ones.
	addFlags(t, b, imap.SeqSetNum(2), imap.FlagFlagged)
	if err := a.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	// imapclient runs the unilateral FETCH handler in its own goroutine.
	u.waitFor(t, "unsolicited FETCH", func() bool { return len(u.fetches) > 0 })
	_, _, fetches := u.take()
	if len(fetches) != 1 || fetches[0].SeqNum != 2 || fetches[0].UID != 3 || !sameFlags(fetches[0].Flags, imap.FlagFlagged) {
		t.Errorf("unsolicited FETCH: %+v", fetches)
	}

	// Messages appended by another session.
	if _, err := appendMsg(b, "INBOX", testMessage("carol@example.org", "New", "x"), nil); err != nil {
		t.Fatal(err)
	}
	if err := a.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	if exists, _, _ := u.take(); !slices.Equal(exists, []uint32{3}) {
		t.Errorf("EXISTS %v, want [3]", exists)
	}
	if uids := fetchUIDs(t, a); !slices.Equal(uids, []imap.UID{1, 3, 4}) {
		t.Errorf("UIDs %v", uids)
	}
}

func TestSelectedMailboxDeleted(t *testing.T) {
	env := newTestEnv(t, Options{})
	a := env.login(t, nil)
	if err := a.Create("Temp", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	env.insert(t, "Temp", testMessage("bob@example.com", "Hi", "hello"), time.Time{})
	mustSelect(t, a, "Temp", false)

	b := env.login(t, nil)
	if err := b.Delete("Temp").Wait(); err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	_, err := a.Store(imap.SeqSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}}, nil).Collect()
	expectCode(t, "STORE in deleted mailbox", err, imap.ResponseCodeNonExistent)

	// Re-creating the mailbox doesn't revive the old selection.
	if err := b.Create("Temp", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := appendMsg(b, "Temp", testMessage("bob@example.com", "New", "x"), nil); err != nil {
		t.Fatal(err)
	}
	if err := a.Noop().Wait(); err != nil {
		t.Fatal(err)
	}
	mustSelect(t, a, "INBOX", false)
}

func TestViewsStayConsistent(t *testing.T) {
	env := newTestEnv(t, Options{})
	ctx := context.Background()
	inbox := env.mailbox(t, "INBOX")
	for i := 0; i < 5; i++ {
		env.insert(t, "INBOX", testMessage("bob@example.com", fmt.Sprint("Initial ", i), "body"), time.Time{})
	}

	type watcher struct {
		c    *imapclient.Client
		idle *imapclient.IdleCommand
	}
	watchers := make([]watcher, 3)
	for i := range watchers {
		c := env.login(t, nil)
		mustSelect(t, c, "INBOX", false)
		idle, err := c.Idle()
		if err != nil {
			t.Fatal(err)
		}
		watchers[i] = watcher{c, idle}
	}
	worker := env.login(t, nil)
	mustSelect(t, worker, "INBOX", false)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_, err := env.st.InsertMessage(ctx, store.NewMessage{
				UserID: env.user.ID, MailboxID: inbox.ID, ReceivedAt: time.Now(),
				Raw: testMessage("carol@example.org", fmt.Sprint("Delivery ", i), "body"),
			})
			if err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i := 0; i < 10; i++ {
		addFlags(t, worker, imap.SeqSetNum(1), imap.FlagSeen)
		if _, err := worker.Move(imap.SeqSetNum(1), "Archive").Wait(); err != nil {
			t.Fatalf("MOVE: %v", err)
		}
		if i%3 == 0 {
			if err := worker.Noop().Wait(); err != nil {
				t.Fatal(err)
			}
		}
	}
	wg.Wait()

	want, err := env.st.MailboxUIDs(ctx, inbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantUIDs := make([]imap.UID, len(want))
	for i, uid := range want {
		wantUIDs[i] = imap.UID(uid)
	}
	clients := []*imapclient.Client{worker}
	for _, w := range watchers {
		if err := w.idle.Close(); err != nil {
			t.Fatal(err)
		}
		if err := w.idle.Wait(); err != nil {
			t.Fatal(err)
		}
		clients = append(clients, w.c)
	}
	for i, c := range clients {
		if err := c.Noop().Wait(); err != nil {
			t.Fatal(err)
		}
		if n := c.Mailbox().NumMessages; n != uint32(len(want)) {
			t.Errorf("client %d counts %d messages, want %d", i, n, len(want))
		}
		if uids := fetchUIDs(t, c); !slices.Equal(uids, wantUIDs) {
			t.Errorf("client %d sees UIDs %v, want %v", i, uids, wantUIDs)
		}
	}
}

func TestCloseDisconnectsClients(t *testing.T) {
	env := newTestEnv(t, Options{})
	c := env.login(t, nil)
	mustSelect(t, c, "INBOX", false)
	idle, err := c.Idle()
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := env.srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Close took %v", d)
	}
	idle.Close()
	done := make(chan struct{})
	go func() {
		idle.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(waitTimeout):
		t.Fatal("client still connected after Close")
	}
	if c, err := imapclient.DialInsecure(env.addr, nil); err == nil {
		err = c.Login(testUser, testPassword).Wait()
		c.Close()
		if err == nil {
			t.Error("LOGIN succeeded after Close")
		}
	}
}

// rawClient speaks IMAP directly, to check the exact responses sent.
type rawClient struct {
	t   *testing.T
	nc  net.Conn
	r   *bufio.Reader
	tag int
}

func dialRaw(t *testing.T, addr string) *rawClient {
	t.Helper()
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	nc.SetDeadline(time.Now().Add(30 * time.Second))
	rc := &rawClient{t: t, nc: nc, r: bufio.NewReader(nc)}
	if line := rc.readLine(); !strings.HasPrefix(line, "* OK") {
		t.Fatalf("greeting %q", line)
	}
	return rc
}

func (rc *rawClient) readLine() string {
	rc.t.Helper()
	line, err := rc.r.ReadString('\n')
	if err != nil {
		rc.t.Fatalf("read: %v", err)
	}
	return strings.TrimSuffix(line, "\r\n")
}

func (rc *rawClient) write(s string) {
	rc.t.Helper()
	if _, err := io.WriteString(rc.nc, s); err != nil {
		rc.t.Fatalf("write: %v", err)
	}
}

func (rc *rawClient) nextTag() string {
	rc.tag++
	return fmt.Sprintf("a%d", rc.tag)
}

// do sends a command and returns its untagged responses. The command must
// succeed.
func (rc *rawClient) do(cmd string) []string {
	rc.t.Helper()
	tag := rc.nextTag()
	rc.write(tag + " " + cmd + "\r\n")
	var untagged []string
	for {
		line := rc.readLine()
		if rest, ok := strings.CutPrefix(line, tag+" "); ok {
			if !strings.HasPrefix(rest, "OK") {
				rc.t.Fatalf("%s: %s", cmd, rest)
			}
			return untagged
		}
		untagged = append(untagged, line)
	}
}

func TestProtocolTranscript(t *testing.T) {
	env := newTestEnv(t, Options{})
	ctx := context.Background()
	inbox := env.mailbox(t, "INBOX")
	trash := env.mailbox(t, "Trash")
	var msgs []*store.Message
	for i := 0; i < 3; i++ {
		msgs = append(msgs, env.insert(t, "INBOX", testMessage("bob@example.com", fmt.Sprint("Message ", i), "body"), time.Time{}))
	}

	rc := dialRaw(t, env.addr)
	rc.do(fmt.Sprintf("LOGIN %s %q", testUser, testPassword))
	lines := rc.do("SELECT INBOX")
	for _, want := range []string{
		"* 3 EXISTS",
		fmt.Sprintf("* OK [UIDVALIDITY %d]", inbox.UIDValidity),
		"* OK [UIDNEXT 4]",
		`* FLAGS (\Seen \Answered \Flagged \Deleted \Draft)`,
		`* OK [PERMANENTFLAGS (\Seen \Answered \Flagged \Deleted \Draft \*)]`,
	} {
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, want) }) {
			t.Errorf("SELECT: missing %q in %q", want, lines)
		}
	}

	// MOVE: COPYUID, then exactly one EXPUNGE.
	lines = rc.do("UID MOVE 2 Trash")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], fmt.Sprintf("* OK [COPYUID %d 2 1]", trash.UIDValidity)) || lines[1] != "* 2 EXPUNGE" {
		t.Errorf("UID MOVE: %q", lines)
	}
	if lines := rc.do("NOOP"); len(lines) != 0 {
		t.Errorf("NOOP: %q", lines)
	}

	// A UID range ending with * always includes the last message.
	if lines := rc.do("UID FETCH 100:* (FLAGS)"); !slices.Equal(lines, []string{"* 2 FETCH (UID 3 FLAGS ())"}) {
		t.Errorf("UID FETCH 100:*: %q", lines)
	}

	// The session's own flag changes are not reported again.
	if lines := rc.do(`STORE 2 +FLAGS (\Answered)`); !slices.Equal(lines, []string{`* 2 FETCH (FLAGS (\Answered))`}) {
		t.Errorf("STORE: %q", lines)
	}
	if lines := rc.do(`STORE 2 -FLAGS.SILENT (\Answered)`); len(lines) != 0 {
		t.Errorf("STORE .SILENT: %q", lines)
	}
	if lines := rc.do(`STORE 1 +FLAGS.SILENT (\Flagged)`); len(lines) != 0 {
		t.Errorf("STORE .SILENT: %q", lines)
	}
	if lines := rc.do("NOOP"); len(lines) != 0 {
		t.Errorf("NOOP after STORE: %q", lines)
	}

	// SEARCHRES.
	rc.do("UID SEARCH RETURN (SAVE) FLAGGED")
	if lines := rc.do("UID FETCH $ (FLAGS)"); !slices.Equal(lines, []string{`* 1 FETCH (UID 1 FLAGS (\Flagged))`}) {
		t.Errorf("UID FETCH $: %q", lines)
	}

	env.insert(t, "INBOX", testMessage("bob@example.com", "New", "body"), time.Time{})
	if lines := rc.do("NOOP"); !slices.Equal(lines, []string{"* 3 EXISTS"}) {
		t.Errorf("NOOP after delivery: %q", lines)
	}

	// IDLE reports deliveries, flag changes and expunges made elsewhere.
	tag := rc.nextTag()
	rc.write(tag + " IDLE\r\n")
	if line := rc.readLine(); !strings.HasPrefix(line, "+ ") {
		t.Fatalf("IDLE: %q", line)
	}
	env.insert(t, "INBOX", testMessage("bob@example.com", "Newer", "body"), time.Time{})
	if line := rc.readLine(); line != "* 4 EXISTS" {
		t.Errorf("IDLE: got %q, want EXISTS", line)
	}
	if err := env.st.UpdateFlagsByID(ctx, env.user.ID, []int64{msgs[2].ID}, store.FlagsAdd, []string{store.FlagSeen}); err != nil {
		t.Fatal(err)
	}
	if line := rc.readLine(); line != `* 2 FETCH (UID 3 FLAGS (\Seen))` {
		t.Errorf("IDLE: got %q, want FETCH", line)
	}
	if _, _, err := env.st.MoveMessages(ctx, env.user.ID, inbox.ID, []uint32{1}, trash.ID, nil); err != nil {
		t.Fatal(err)
	}
	if line := rc.readLine(); line != "* 1 EXPUNGE" {
		t.Errorf("IDLE: got %q, want EXPUNGE", line)
	}
	rc.write("DONE\r\n")
	if line := rc.readLine(); !strings.HasPrefix(line, tag+" OK") {
		t.Errorf("IDLE completion: %q", line)
	}

	if lines := rc.do("LOGOUT"); len(lines) == 0 || !strings.HasPrefix(lines[0], "* BYE") {
		t.Errorf("LOGOUT: %q", lines)
	}
}

func TestIMAP4rev2Select(t *testing.T) {
	env := newTestEnv(t, Options{})
	env.insert(t, "Sent", testMessage("alice@example.com", "Sent", "body"), time.Time{})

	rc := dialRaw(t, env.addr)
	rc.do(fmt.Sprintf("LOGIN %s %q", testUser, testPassword))
	if lines := rc.do("ENABLE IMAP4rev2"); !slices.Equal(lines, []string{"* ENABLED IMAP4rev2"}) {
		t.Errorf("ENABLE: %q", lines)
	}
	lines := rc.do("SELECT Sent")
	if !slices.Contains(lines, `* LIST (\HasNoChildren \Sent) "/" "Sent"`) {
		t.Errorf("SELECT: missing LIST response in %q", lines)
	}
	if !slices.Contains(lines, "* 1 EXISTS") || slices.ContainsFunc(lines, func(l string) bool { return strings.HasSuffix(l, " RECENT") }) {
		t.Errorf("SELECT: %q", lines)
	}
}
