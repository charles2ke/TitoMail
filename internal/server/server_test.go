package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"

	"github.com/charles2ke/TitoMail/internal/api"
	"github.com/charles2ke/TitoMail/internal/config"
	"github.com/charles2ke/TitoMail/internal/store"
)

func init() { store.PasswordCost = 4 }

const (
	testHost      = "mail.example.test"
	adminAddr     = "admin@example.test"
	adminPassword = "correct horse battery"
	bobAddr       = "bob@example.test"
	bobPassword   = "battery staple horse"
	// remoteMXIP is the address of remote.test's mail exchanger; the test
	// dialer connects it to the fake remote server.
	remoteMXIP = "192.0.2.25"
)

// fakeDNS keeps the tests hermetic: remote.test has an MX record, TXT
// records can be published, and every other name does not exist.
type fakeDNS struct {
	mu  sync.Mutex
	txt map[string][]string
}

func notFound(name string) error {
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (r *fakeDNS) publishTXT(name, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.txt[name] = []string{value}
}

func (r *fakeDNS) LookupTXT(_ context.Context, name string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.txt[strings.TrimSuffix(name, ".")]; ok {
		return v, nil
	}
	return nil, notFound(name)
}

func (r *fakeDNS) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	if strings.TrimSuffix(name, ".") == "remote.test" {
		return []*net.MX{{Host: "mx.remote.test.", Pref: 10}}, nil
	}
	return nil, notFound(name)
}

func (r *fakeDNS) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if strings.TrimSuffix(host, ".") == "mx.remote.test" {
		return []net.IPAddr{{IP: net.ParseIP(remoteMXIP)}}, nil
	}
	return nil, notFound(host)
}

func (r *fakeDNS) LookupAddr(_ context.Context, addr string) ([]string, error) {
	return nil, notFound(addr)
}

// remoteMX is the mail server of remote.test. It records what it receives.
type remoteMX struct {
	addr     string
	received chan remoteDelivery
}

type remoteDelivery struct {
	from  string
	rcpts []string
	data  []byte
}

type remoteSession struct {
	mx    *remoteMX
	from  string
	rcpts []string
}

func (s *remoteSession) Reset()        { s.from, s.rcpts = "", nil }
func (s *remoteSession) Logout() error { return nil }
func (s *remoteSession) Mail(from string, _ *gosmtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *remoteSession) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	s.rcpts = append(s.rcpts, to)
	return nil
}

func (s *remoteSession) Data(r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mx.received <- remoteDelivery{from: s.from, rcpts: s.rcpts, data: b}
	return nil
}

func startRemoteMX(t *testing.T) *remoteMX {
	t.Helper()
	mx := &remoteMX{received: make(chan remoteDelivery, 10)}
	srv := gosmtp.NewServer(gosmtp.BackendFunc(func(*gosmtp.Conn) (gosmtp.Session, error) {
		return &remoteSession{mx: mx}, nil
	}))
	srv.Domain = "mx.remote.test"
	srv.ErrorLog = log.New(io.Discard, "", 0)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mx.addr = ln.Addr().String()
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return mx
}

// writeCert writes a self-signed certificate for testHost to dir and
// returns a client configuration that trusts it.
func writeCert(t *testing.T, dir string) (certFile, keyFile string, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: testHost},
		DNSNames:     []string{testHost},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, &tls.Config{RootCAs: pool, ServerName: testHost}
}

// logBuffer collects server logs; they are printed when a test fails.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// running is a server started with start.
type running struct {
	addrs  map[string]string
	logs   *logBuffer
	cancel context.CancelFunc
	done   chan error

	once sync.Once
	took time.Duration
	err  error
}

// start runs the server until stop is called or the test ends.
func start(t *testing.T, opts Options) *running {
	t.Helper()
	if err := opts.Config.Validate(); err != nil {
		t.Fatal(err)
	}
	r := &running{logs: &logBuffer{}, done: make(chan error, 1)}
	opts.Logger = slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ready := make(chan map[string]string, 1)
	opts.Ready = func(addrs map[string]string) { ready <- addrs }
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { r.done <- Run(ctx, opts) }()
	t.Cleanup(func() {
		r.stop()
		if t.Failed() {
			t.Logf("server log:\n%s", r.logs)
		}
	})
	select {
	case r.addrs = <-ready:
	case err := <-r.done:
		r.once.Do(func() { r.err = err })
		t.Fatalf("Run: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("the server did not start")
	}
	return r
}

// stop shuts the server down and reports how long Run took to return.
func (r *running) stop() (time.Duration, error) {
	r.once.Do(func() {
		began := time.Now()
		r.cancel()
		select {
		case r.err = <-r.done:
		case <-time.After(30 * time.Second):
			r.err = errors.New("Run did not return after cancellation")
		}
		r.took = time.Since(began)
	})
	return r.took, r.err
}

func port(addr string) int {
	_, p, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(p)
	return n
}

// webClient is a browser-like client for the web interface.
type webClient struct {
	t    *testing.T
	base string
	hc   *http.Client
	csrf string
}

func newWebClient(t *testing.T, base string, tlsConf *tls.Config) *webClient {
	jar, _ := cookiejar.New(nil)
	return &webClient{t: t, base: base, hc: &http.Client{
		Jar:           jar,
		Transport:     &http.Transport{TLSClientConfig: tlsConf},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *webClient) send(method, path string, body io.Reader, contentType string) *http.Response {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	c.t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (c *webClient) get(path string) (*http.Response, string) {
	c.t.Helper()
	resp := c.send("GET", path, nil, "")
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("GET %s: %v", path, err)
	}
	return resp, string(data)
}

// must sends a JSON request and decodes the JSON response into out.
func (c *webClient) must(method, path string, body, out any) {
	c.t.Helper()
	var rd io.Reader
	ct := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rd, ct = bytes.NewReader(b), "application/json"
	}
	resp := c.send(method, path, rd, ct)
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		c.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, bytes.TrimSpace(data))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			c.t.Fatalf("%s %s: decoding %q: %v", method, path, data, err)
		}
	}
}

func (c *webClient) login(addr, password string) {
	c.t.Helper()
	var info struct {
		CSRFToken string `json:"csrf_token"`
	}
	c.must("POST", "/api/login", map[string]string{"email": addr, "password": password}, &info)
	c.csrf = info.CSRFToken
}

func (c *webClient) mailbox(special string) store.Mailbox {
	c.t.Helper()
	var list []store.Mailbox
	c.must("GET", "/api/mailboxes", nil, &list)
	for _, mb := range list {
		if mb.SpecialUse == special {
			return mb
		}
	}
	c.t.Fatalf("no %s mailbox", special)
	return store.Mailbox{}
}

func message(from, to, subject string) string {
	return fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%d@integration.test>\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n\r\nHello from the integration test.\r\n",
		from, to, subject, time.Now().Format(time.RFC1123Z), time.Now().UnixNano())
}

// submit authenticates and sends one message, then quits.
func submit(c *gosmtp.Client, user, password string, rcpts []string, msg string) error {
	defer c.Close()
	if err := c.Auth(sasl.NewPlainClient("", user, password)); err != nil {
		return fmt.Errorf("AUTH: %w", err)
	}
	if err := c.SendMail(user, rcpts, strings.NewReader(msg)); err != nil {
		return err
	}
	return c.Quit()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRunEndToEnd starts every service and exchanges mail through each of
// them the way real clients and servers would.
func TestRunEndToEnd(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, clientTLS := writeCert(t, dir)
	dns := &fakeDNS{txt: map[string][]string{}}
	remote := startRemoteMX(t)
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != net.JoinHostPort(remoteMXIP, "25") {
			return nil, fmt.Errorf("unexpected outbound connection to %s", addr)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, remote.addr)
	}

	cfg := config.Default()
	cfg.Hostname = testHost
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.PublicURL = "https://" + testHost
	for _, p := range []*string{&cfg.HTTPAddr, &cfg.HTTPSAddr, &cfg.SMTPAddr, &cfg.SubmissionAddr,
		&cfg.SubmissionsAddr, &cfg.IMAPAddr, &cfg.IMAPSAddr} {
		*p = "127.0.0.1:0"
	}
	cfg.TLSCert, cfg.TLSKey = certFile, keyFile
	cfg.AdminEmail, cfg.AdminPassword = adminAddr, adminPassword
	opts := Options{
		Config:   cfg,
		Static:   fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>TitoMail webmail</title>")}},
		Version:  "test",
		Resolver: dns,
		Dial:     dial,
	}
	srv := start(t, opts)
	addrs := srv.addrs
	for _, name := range []string{"http", "https", "smtp", "submission", "submissions", "imap", "imaps"} {
		if addrs[name] == "" {
			t.Fatalf("no %s listener in %v", name, addrs)
		}
	}

	// Plain HTTP redirects to HTTPS, except for health checks and client
	// autoconfiguration, which advertises the real ports.
	plain := newWebClient(t, "http://"+addrs["http"], nil)
	if resp, _ := plain.get("/inbox"); resp.StatusCode != http.StatusMovedPermanently ||
		resp.Header.Get("Location") != "https://"+testHost+"/inbox" {
		t.Fatalf("plain HTTP: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := plain.get("/healthz"); resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz over HTTP: %d", resp.StatusCode)
	}
	resp, body := plain.get("/mail/config-v1.1.xml?emailaddress=" + bobAddr)
	imapsEntry := fmt.Sprintf("<hostname>%s</hostname>\n      <port>%d</port>\n      <socketType>SSL</socketType>", testHost, port(addrs["imaps"]))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, imapsEntry) {
		t.Fatalf("autoconfig: %d\n%s", resp.StatusCode, body)
	}
	admin := newWebClient(t, "https://"+addrs["https"], clientTLS)
	if resp, body := admin.get("/"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "TitoMail webmail") {
		t.Fatalf("web app over HTTPS: %d %q", resp.StatusCode, body)
	}

	// The configured administrator exists and can add accounts.
	admin.login(adminAddr, adminPassword)
	admin.must("POST", "/api/admin/users", map[string]any{"email": bobAddr, "password": bobPassword, "display_name": "Bob"}, nil)

	// Publish the DKIM key the admin page asks for; SRV records point at
	// the implicit-TLS ports.
	var domains []store.Domain
	admin.must("GET", "/api/admin/domains", nil, &domains)
	if len(domains) != 1 || domains[0].Name != "example.test" {
		t.Fatalf("domains: %+v", domains)
	}
	var records struct {
		Records []api.DNSRecord `json:"records"`
	}
	admin.must("GET", fmt.Sprintf("/api/admin/domains/%d/dns", domains[0].ID), nil, &records)
	found := map[string]string{}
	for _, rec := range records.Records {
		found[rec.Name] = rec.Value
	}
	dkimName := store.DKIMSelector + "._domainkey.example.test"
	if !strings.HasPrefix(found[dkimName], "v=DKIM1; k=rsa; p=") {
		t.Fatalf("DKIM record: %+v", records.Records)
	}
	dns.publishTXT(dkimName, found[dkimName])
	if want := fmt.Sprintf("0 1 %d %s.", port(addrs["imaps"]), testHost); found["_imaps._tcp.example.test"] != want {
		t.Errorf("IMAPS SRV record %q, want %q", found["_imaps._tcp.example.test"], want)
	}
	if want := fmt.Sprintf("0 1 %d %s.", port(addrs["submissions"]), testHost); found["_submissions._tcp.example.test"] != want {
		t.Errorf("submissions SRV record %q, want %q", found["_submissions._tcp.example.test"], want)
	}

	// Submission with STARTTLS: one local and one remote recipient.
	c, err := gosmtp.DialStartTLS(addrs["submission"], clientTLS)
	if err != nil {
		t.Fatalf("submission STARTTLS: %v", err)
	}
	if err := submit(c, adminAddr, adminPassword, []string{bobAddr, "carol@remote.test"},
		message(adminAddr, "Bob <"+bobAddr+">, carol@remote.test", "Quarterly report")); err != nil {
		t.Fatalf("submission: %v", err)
	}
	// Submission over implicit TLS.
	if c, err = gosmtp.DialTLS(addrs["submissions"], clientTLS); err != nil {
		t.Fatalf("submissions: %v", err)
	}
	if err := submit(c, bobAddr, bobPassword, []string{adminAddr}, message(bobAddr, adminAddr, "Re: Quarterly report")); err != nil {
		t.Fatalf("submissions: %v", err)
	}

	// Inbound mail from the internet; relaying is refused.
	mx, err := gosmtp.Dial(addrs["smtp"])
	if err != nil {
		t.Fatal(err)
	}
	defer mx.Close()
	if err := mx.Hello("mx.remote.test"); err != nil {
		t.Fatal(err)
	}
	if err := mx.Mail("dave@remote.test", nil); err != nil {
		t.Fatal(err)
	}
	var smtpErr *gosmtp.SMTPError
	if err := mx.Rcpt("someone@elsewhere.test", nil); !errors.As(err, &smtpErr) || smtpErr.Code != 554 ||
		smtpErr.EnhancedCode != (gosmtp.EnhancedCode{5, 7, 1}) {
		t.Fatalf("relaying to elsewhere.test: %v", err)
	}
	if err := mx.Rcpt(bobAddr, nil); err != nil {
		t.Fatal(err)
	}
	w, err := mx.Data()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, message("Dave <dave@remote.test>", bobAddr, "Hello from remote.test")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("MX DATA: %v", err)
	}
	if err := mx.Quit(); err != nil {
		t.Fatal(err)
	}

	// The remote copy reaches remote.test's MX with a DKIM signature that
	// verifies against the published record.
	select {
	case got := <-remote.received:
		if got.from != adminAddr || !slices.Equal(got.rcpts, []string{"carol@remote.test"}) {
			t.Fatalf("remote delivery: from %q to %v", got.from, got.rcpts)
		}
		lookup := func(name string) ([]string, error) { return dns.LookupTXT(context.Background(), name) }
		verifs, err := dkim.VerifyWithOptions(bytes.NewReader(got.data), &dkim.VerifyOptions{LookupTXT: lookup})
		if err != nil || len(verifs) != 1 || verifs[0].Err != nil || verifs[0].Domain != "example.test" {
			t.Fatalf("DKIM verification: %v %+v\n%s", err, verifs, got.data)
		}
		if !bytes.Contains(got.data, []byte("Subject: Quarterly report")) {
			t.Fatalf("remote message:\n%s", got.data)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("remote.test did not receive the message")
	}

	// Bob reads both messages over IMAP with implicit TLS.
	ic, err := imapclient.DialTLS(addrs["imaps"], &imapclient.Options{TLSConfig: clientTLS})
	if err != nil {
		t.Fatalf("IMAPS: %v", err)
	}
	defer ic.Close()
	if err := ic.Login(bobAddr, bobPassword).Wait(); err != nil {
		t.Fatalf("IMAP LOGIN: %v", err)
	}
	waitFor(t, "two messages in Bob's inbox", func() bool {
		st, err := ic.Status("INBOX", &imap.StatusOptions{NumMessages: true}).Wait()
		return err == nil && st.NumMessages != nil && *st.NumMessages == 2
	})
	if _, err := ic.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	msgs, err := ic.Fetch(imap.SeqSetNum(1, 2), &imap.FetchOptions{Envelope: true}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, m := range msgs {
		subjects = append(subjects, m.Envelope.Subject)
	}
	slices.Sort(subjects)
	if !slices.Equal(subjects, []string{"Hello from remote.test", "Quarterly report"}) {
		t.Fatalf("Bob's inbox over IMAP: %q", subjects)
	}

	// The administrator reads Bob's reply over IMAP with STARTTLS.
	ac, err := imapclient.DialStartTLS(addrs["imap"], &imapclient.Options{TLSConfig: clientTLS})
	if err != nil {
		t.Fatalf("IMAP STARTTLS: %v", err)
	}
	defer ac.Close()
	if err := ac.Login(adminAddr, adminPassword).Wait(); err != nil {
		t.Fatalf("IMAP LOGIN after STARTTLS: %v", err)
	}
	waitFor(t, "Bob's reply in the administrator's inbox", func() bool {
		st, err := ac.Status("INBOX", &imap.StatusOptions{NumMessages: true}).Wait()
		return err == nil && st.NumMessages != nil && *st.NumMessages == 1
	})
	if err := ac.Logout().Wait(); err != nil {
		t.Fatal(err)
	}

	// The web interface shows the same mail, still unread.
	bob := newWebClient(t, "https://"+addrs["https"], clientTLS)
	bob.login(bobAddr, bobPassword)
	if inbox := bob.mailbox(store.SpecialInbox); inbox.Total != 2 || inbox.Unread != 2 {
		t.Fatalf("Bob's inbox in the web interface: %+v", inbox)
	}
	var list store.ListResult
	adminInbox := admin.mailbox(store.SpecialInbox)
	admin.must("GET", fmt.Sprintf("/api/conversations?view=mailbox&mailbox=%d", adminInbox.ID), nil, &list)
	if list.Total != 1 || list.Conversations[0].Subject != "Re: Quarterly report" {
		t.Fatalf("administrator's inbox in the web interface: %+v", list)
	}

	// Shutting down does not wait for open live-update streams or IMAP
	// sessions.
	stream := bob.send("GET", "/api/events", nil, "")
	defer stream.Body.Close()
	if line, err := bufio.NewReader(stream.Body).ReadString('\n'); stream.StatusCode != http.StatusOK || err != nil ||
		!strings.HasPrefix(line, "retry:") {
		t.Fatalf("event stream: %d %q %v", stream.StatusCode, line, err)
	}
	took, err := srv.stop()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if took > 5*time.Second {
		t.Errorf("shutdown took %v", took)
	}
	for _, name := range []string{"https", "smtp", "imaps"} {
		if conn, err := net.DialTimeout("tcp", addrs[name], time.Second); err == nil {
			conn.Close()
			t.Errorf("%s still accepts connections after shutdown", name)
		}
	}

	// A restart with the same configuration keeps all data and does not
	// create the administrator again.
	srv = start(t, opts)
	bob = newWebClient(t, "https://"+srv.addrs["https"], clientTLS)
	bob.login(bobAddr, bobPassword)
	if inbox := bob.mailbox(store.SpecialInbox); inbox.Total != 2 {
		t.Fatalf("Bob's inbox after a restart: %+v", inbox)
	}
	admin = newWebClient(t, "https://"+srv.addrs["https"], clientTLS)
	admin.login(adminAddr, adminPassword)
	if _, err := srv.stop(); err != nil {
		t.Fatalf("Run after restart: %v", err)
	}
	if strings.Contains(srv.logs.String(), "created administrator") {
		t.Error("the administrator was created again on restart")
	}
}

// TestRunFirstStartSetup covers the first start without a configured
// administrator: the log shows a one-time setup link.
func TestRunFirstStartSetup(t *testing.T) {
	cfg := config.Default()
	cfg.Hostname = testHost
	cfg.DataDir = t.TempDir()
	cfg.HTTPAddr = "127.0.0.1:0"
	cfg.SMTPAddr, cfg.SubmissionAddr, cfg.IMAPAddr = "", "", ""

	bad := cfg
	bad.AdminEmail = adminAddr
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := Run(context.Background(), Options{Config: bad, Logger: discard}); err == nil ||
		!strings.Contains(err.Error(), "admin-password is required") {
		t.Fatalf("Run with an administrator but no password: %v", err)
	}

	srv := start(t, Options{Config: cfg, Resolver: &fakeDNS{}})
	if len(srv.addrs) != 1 {
		t.Fatalf("listeners: %v", srv.addrs)
	}
	m := regexp.MustCompile(`/setup\?token=([a-z2-7]+)`).FindStringSubmatch(srv.logs.String())
	if m == nil {
		t.Fatal("the log has no setup link")
	}
	c := newWebClient(t, "http://"+srv.addrs["http"], nil)
	var info struct {
		Authenticated bool `json:"authenticated"`
		SetupRequired bool `json:"setup_required"`
		User          *struct {
			Email   string `json:"email"`
			IsAdmin bool   `json:"is_admin"`
		} `json:"user"`
	}
	c.must("GET", "/api/session", nil, &info)
	if info.Authenticated || !info.SetupRequired {
		t.Fatalf("session before setup: %+v", info)
	}
	c.must("POST", "/api/setup", map[string]string{"token": m[1], "email": adminAddr, "password": adminPassword,
		"display_name": "Admin"}, &info)
	if !info.Authenticated || info.User == nil || info.User.Email != adminAddr || !info.User.IsAdmin {
		t.Fatalf("session after setup: %+v", info)
	}
	if _, err := srv.stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestRunACMEHTTPChallenge checks that HTTP-01 challenges are answered on
// the plain HTTP listener, with and without an HTTPS listener.
func TestRunACMEHTTPChallenge(t *testing.T) {
	// The certificate prefetch talks to this directory instead of Let's
	// Encrypt and fails immediately.
	directory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(directory.Close)
	hc := &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for _, withHTTPS := range []bool{false, true} {
		t.Run(fmt.Sprintf("https=%v", withHTTPS), func(t *testing.T) {
			cfg := config.Default()
			cfg.Hostname = testHost
			cfg.DataDir = t.TempDir()
			cfg.ACME = true
			cfg.ACMEDirectory = directory.URL + "/directory"
			cfg.HTTPAddr = "127.0.0.1:0"
			if withHTTPS {
				cfg.HTTPSAddr = "127.0.0.1:0"
			}
			cfg.SMTPAddr, cfg.SubmissionAddr, cfg.IMAPAddr = "", "", ""
			srv := start(t, Options{Config: cfg, Resolver: &fakeDNS{}})

			get := func(host, path string) (int, string) {
				t.Helper()
				req, err := http.NewRequest(http.MethodGet, "http://"+srv.addrs["http"]+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Host = host
				res, err := hc.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				body, _ := io.ReadAll(res.Body)
				return res.StatusCode, string(body)
			}
			const challenge = "/.well-known/acme-challenge/token"
			if code, body := get(testHost, challenge); code != http.StatusNotFound || !strings.Contains(body, "acme/autocert") {
				t.Errorf("challenge for %s: %d %q, want a 404 from autocert", testHost, code, body)
			}
			if code, body := get("other.test", challenge); code != http.StatusForbidden || !strings.Contains(body, "not served here") {
				t.Errorf("challenge for another host: %d %q, want 403", code, body)
			}
			if code, body := get(testHost, "/healthz"); code != http.StatusOK {
				t.Errorf("/healthz: %d %q", code, body)
			}
			if code, _ := get(testHost, "/inbox"); withHTTPS != (code == http.StatusMovedPermanently) {
				t.Errorf("/inbox: %d, want a redirect only with HTTPS", code)
			}
		})
	}
}
