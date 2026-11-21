package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/charles2ke/TitoMail/internal/auth"
	"github.com/charles2ke/TitoMail/internal/delivery"
	"github.com/charles2ke/TitoMail/internal/email"
	"github.com/charles2ke/TitoMail/internal/events"
	"github.com/charles2ke/TitoMail/internal/store"
)

func init() { store.PasswordCost = 4 }

const testHost = "mail.example.test"

type fakeResolver struct {
	mx  map[string][]*net.MX
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

func (r *fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if v, ok := r.txt[strings.TrimSuffix(name, ".")]; ok {
		return v, nil
	}
	return nil, notFound(name)
}

func (r *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	return nil, notFound(host)
}

func (r *fakeResolver) LookupAddr(_ context.Context, addr string) ([]string, error) {
	return nil, notFound(addr)
}

type env struct {
	t   *testing.T
	st  *store.Store
	d   *delivery.Deliverer
	api *API
	srv *httptest.Server
	res *fakeResolver
}

func newEnv(t *testing.T, opts Options) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	res := &fakeResolver{mx: map[string][]*net.MX{}, txt: map[string][]string{}}
	d := delivery.New(st, delivery.Options{Hostname: testHost, DisableOutbound: true, Resolver: res})
	hub := events.New(st)
	t.Cleanup(hub.Close)
	opts.Hostname = testHost
	opts.Resolver = res
	a := New(st, d, auth.New(st), hub, opts)
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	t.Cleanup(a.Shutdown) // runs first: ends event streams so srv.Close does not block
	return &env{t: t, st: st, d: d, api: a, srv: srv, res: res}
}

func (e *env) user(addr string, admin bool) *store.User {
	e.t.Helper()
	ctx := context.Background()
	if d := email.DomainOf(addr); !e.st.IsLocalDomain(ctx, d) {
		if _, err := e.st.CreateDomain(ctx, d); err != nil {
			e.t.Fatal(err)
		}
	}
	u, err := e.st.CreateUser(ctx, store.NewUser{Email: addr, Password: "correct horse", DisplayName: strings.Split(addr, "@")[0], IsAdmin: admin})
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

// client is a browser-like API client with a cookie jar.
type client struct {
	e    *env
	hc   *http.Client
	csrf string
}

func (e *env) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, hc: &http.Client{Jar: jar}}
}

func (e *env) login(addr string) *client {
	e.t.Helper()
	c := e.client()
	var info sessionInfo
	if code := c.do("POST", "/api/login", map[string]string{"email": addr, "password": "correct horse"}, &info); code != 200 {
		e.t.Fatalf("login %s: status %d", addr, code)
	}
	c.csrf = info.CSRFToken
	return c
}

func (c *client) request(method, path string, body io.Reader, contentType string) *http.Response {
	c.e.t.Helper()
	req, err := http.NewRequest(method, c.e.srv.URL+path, body)
	if err != nil {
		c.e.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.csrf != "" {
		req.Header.Set(csrfHeader, c.csrf)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	return resp
}

// do sends a JSON request and decodes the JSON response into out.
func (c *client) do(method, path string, body, out any) int {
	c.e.t.Helper()
	var rd io.Reader
	ct := ""
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.e.t.Fatal(err)
		}
		rd, ct = bytes.NewReader(b), "application/json"
	}
	resp := c.request(method, path, rd, ct)
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(data, out); err != nil {
			c.e.t.Fatalf("%s %s: decoding %q: %v", method, path, data, err)
		}
	}
	if resp.StatusCode >= 300 && testing.Verbose() {
		c.e.t.Logf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return resp.StatusCode
}

func (c *client) must(method, path string, body, out any) {
	c.e.t.Helper()
	if code := c.do(method, path, body, out); code >= 300 {
		c.e.t.Fatalf("%s %s: status %d", method, path, code)
	}
}

func (c *client) upload(name, contentType string, data []byte) store.Upload {
	c.e.t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="file"; filename="%s"`, name)}
	h["Content-Type"] = []string{contentType}
	pw, _ := mw.CreatePart(h)
	_, _ = pw.Write(data)
	_ = mw.Close()
	resp := c.request("POST", "/api/uploads", &b, mw.FormDataContentType())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		c.e.t.Fatalf("upload: status %d", resp.StatusCode)
	}
	var up store.Upload
	if err := json.NewDecoder(resp.Body).Decode(&up); err != nil {
		c.e.t.Fatal(err)
	}
	return up
}

func (c *client) mailbox(special string) *store.Mailbox {
	c.e.t.Helper()
	var list []*store.Mailbox
	c.must("GET", "/api/mailboxes", nil, &list)
	for _, mb := range list {
		if mb.SpecialUse == special {
			return mb
		}
	}
	c.e.t.Fatalf("no %s mailbox", special)
	return nil
}

func (c *client) list(query string) *store.ListResult {
	c.e.t.Helper()
	var res store.ListResult
	c.must("GET", "/api/conversations?"+query, nil, &res)
	return &res
}

func (c *client) setUndo(seconds int) {
	c.e.t.Helper()
	var st store.Settings
	c.must("GET", "/api/settings", nil, &st)
	st.UndoSendSeconds = seconds
	c.must("PUT", "/api/settings", st, &st)
}

func TestSetupLoginAndCSRF(t *testing.T) {
	e := newEnv(t, Options{SetupToken: "s3cret-token"})
	c := e.client()

	var info sessionInfo
	c.must("GET", "/api/session", nil, &info)
	if info.Authenticated || !info.SetupRequired {
		t.Fatalf("fresh server: %+v", info)
	}
	setup := map[string]string{"token": "wrong", "email": "Admin@Example.test", "password": "correct horse", "display_name": "Admin"}
	if code := c.do("POST", "/api/setup", setup, nil); code != http.StatusForbidden {
		t.Fatalf("setup with wrong token: %d", code)
	}
	// Cross-site form posts and other origins are refused.
	resp := c.request("POST", "/api/setup", strings.NewReader("token=x"), "application/x-www-form-urlencoded")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("form post: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/login", strings.NewReader(`{"email":"a@b.c","password":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.test")
	if resp, err := c.hc.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin login: %v %v", resp.StatusCode, err)
	}

	setup["token"] = "s3cret-token"
	c.must("POST", "/api/setup", setup, &info)
	if !info.Authenticated || info.User.Email != "admin@example.test" || !info.User.IsAdmin || info.CSRFToken == "" {
		t.Fatalf("after setup: %+v", info)
	}
	if !e.st.IsLocalDomain(context.Background(), "example.test") {
		t.Fatal("setup did not create the domain")
	}
	if code := c.do("POST", "/api/setup", setup, nil); code != http.StatusConflict {
		t.Fatalf("second setup: %d", code)
	}
	c.csrf = info.CSRFToken
	var again sessionInfo
	c.must("GET", "/api/session", nil, &again)
	if !again.Authenticated || again.CSRFToken != info.CSRFToken || len(again.Addresses) == 0 {
		t.Fatalf("session: %+v", again)
	}

	// State-changing requests need the CSRF token.
	c.csrf = ""
	if code := c.do("POST", "/api/logout", nil, nil); code != http.StatusForbidden {
		t.Fatalf("logout without CSRF: %d", code)
	}
	c.csrf = info.CSRFToken
	c.must("POST", "/api/logout", nil, nil)
	if code := c.do("GET", "/api/mailboxes", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("after logout: %d", code)
	}

	if code := c.do("POST", "/api/login", map[string]string{"email": "admin@example.test", "password": "wrong password"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("bad password: %d", code)
	}
	e.login("admin@example.test")
}

func TestSendReceiveReply(t *testing.T) {
	e := newEnv(t, Options{})
	ctx := context.Background()
	e.user("alice@example.test", false)
	e.user("bob@example.test", false)
	alice, bob := e.login("alice@example.test"), e.login("bob@example.test")
	alice.setUndo(0)
	bob.setUndo(0)

	up := alice.upload("notes.txt", "text/plain", []byte("attached notes"))
	var sent struct {
		QueueID  int64 `json:"queue_id"`
		ThreadID int64 `json:"thread_id"`
	}
	alice.must("POST", "/api/send", map[string]any{
		"to":          []email.Address{{Name: "Bob", Address: "BOB@example.test"}},
		"subject":     "Lunch?",
		"html":        `<p>Hello <b>Bob</b></p><script>alert(1)</script>`,
		"attachments": []map[string]string{{"upload_id": up.ID}},
	}, &sent)
	if code := alice.do("POST", "/api/send", map[string]any{"subject": "nobody"}, nil); code != http.StatusBadRequest {
		t.Fatalf("send without recipients: %d", code)
	}
	if code := alice.do("POST", "/api/send", map[string]any{"from": "bob@example.test", "to": []email.Address{{Address: "bob@example.test"}}}, nil); code != http.StatusForbidden {
		t.Fatalf("send as someone else: %d", code)
	}
	e.d.ProcessQueue(ctx)

	inbox := bob.mailbox(store.SpecialInbox)
	if inbox.Unread != 1 {
		t.Fatalf("bob's inbox: %+v", inbox)
	}
	res := bob.list(fmt.Sprintf("view=mailbox&mailbox=%d", inbox.ID))
	if res.Total != 1 || !res.Conversations[0].Unread || !res.Conversations[0].HasAttachments {
		t.Fatalf("bob's list: %+v", res)
	}
	var thread struct {
		Subject  string           `json:"subject"`
		Messages []*store.Message `json:"messages"`
	}
	bob.must("GET", fmt.Sprintf("/api/threads/%d", res.Conversations[0].ThreadID), nil, &thread)
	if thread.Subject != "Lunch?" || len(thread.Messages) != 1 {
		t.Fatalf("thread: %+v", thread)
	}
	id := thread.Messages[0].ID
	var mv struct {
		Body struct {
			Document string `json:"document"`
		} `json:"body"`
		Attachments []email.Attachment `json:"attachments"`
		Mailbox     string             `json:"mailbox_special"`
	}
	bob.must("GET", fmt.Sprintf("/api/messages/%d", id), nil, &mv)
	if !strings.Contains(mv.Body.Document, "<b>Bob</b>") || strings.Contains(mv.Body.Document, "alert(") || mv.Mailbox != "inbox" {
		t.Fatalf("rendered body: %s", mv.Body.Document)
	}
	if len(mv.Attachments) != 1 || mv.Attachments[0].Filename != "notes.txt" {
		t.Fatalf("attachments: %+v", mv.Attachments)
	}
	resp := bob.request("GET", fmt.Sprintf("/api/messages/%d/attachments/%s?download=1", id, mv.Attachments[0].PartID), nil, "")
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(data) != "attached notes" || resp.Header.Get("Content-Type") != "application/octet-stream" ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("attachment download: %q %v", data, resp.Header)
	}
	// Other users cannot read the message.
	if code := alice.do("GET", fmt.Sprintf("/api/messages/%d", id), nil, nil); code != http.StatusNotFound {
		t.Fatalf("alice reading bob's message: %d", code)
	}

	bob.must("POST", "/api/messages/actions", map[string]any{"ids": []int64{id}, "action": "read"}, nil)
	var reply composeState
	bob.must("GET", fmt.Sprintf("/api/messages/%d/compose?mode=reply", id), nil, &reply)
	if reply.Subject != "Re: Lunch?" || len(reply.To) != 1 || reply.To[0].Address != "alice@example.test" ||
		reply.InReplyToID != id || reply.From != "bob@example.test" || !strings.Contains(reply.HTML, "wrote:") {
		t.Fatalf("reply template: %+v", reply)
	}
	var fwd composeState
	bob.must("GET", fmt.Sprintf("/api/messages/%d/compose?mode=forward", id), nil, &fwd)
	if fwd.Subject != "Fwd: Lunch?" || len(fwd.To) != 0 || len(fwd.Attachments) != 1 || fwd.ForwardOfID != id {
		t.Fatalf("forward template: %+v", fwd)
	}
	reply.HTML = "<p>Sure!</p>" + reply.HTML
	bob.must("POST", "/api/send", reply, nil)
	e.d.ProcessQueue(ctx)

	// Alice sees her message and Bob's reply as one conversation.
	all := alice.list("view=all&threaded=1")
	if all.Total != 1 || all.Conversations[0].Count != 2 || !all.Conversations[0].Unread {
		t.Fatalf("alice's conversations: %+v", all)
	}
	orig, err := e.st.GetMessage(ctx, bob.userID(), id)
	if err != nil || !orig.Answered || !orig.Seen {
		t.Fatalf("original after reply: %+v %v", orig, err)
	}
	// Recipients become contacts and autocomplete finds them.
	var sugg []email.Address
	alice.must("GET", "/api/autocomplete?q=bo", nil, &sugg)
	if len(sugg) != 1 || sugg[0].Address != "bob@example.test" {
		t.Fatalf("autocomplete: %+v", sugg)
	}
	// Archiving from All Mail files Bob's reply but leaves Alice's own
	// message in Sent.
	var archived actionResult
	alice.must("POST", "/api/threads/actions", map[string]any{"thread_ids": []int64{all.Conversations[0].ThreadID},
		"view": map[string]string{"kind": "all"}, "action": "archive"}, &archived)
	if archived.Affected != 1 || alice.mailbox(store.SpecialSent).Total != 1 || alice.mailbox(store.SpecialArchive).Total != 1 {
		t.Fatalf("archive from All Mail: %+v", archived)
	}
}

func (c *client) userID() int64 {
	var info sessionInfo
	c.must("GET", "/api/session", nil, &info)
	return info.User.ID
}

func TestDraftsUndoAndSchedule(t *testing.T) {
	e := newEnv(t, Options{})
	e.user("alice@example.test", false)
	e.user("bob@example.test", false)
	alice := e.login("alice@example.test")
	up := alice.upload("photo.png", "image/png", []byte("\x89PNG fake"))

	draft := map[string]any{
		"to":          []email.Address{{Address: "bob@example.test"}, {Address: "not an address"}},
		"bcc":         []email.Address{{Address: "secret@example.test"}},
		"subject":     "Plans",
		"html":        "<p>draft body</p>",
		"attachments": []map[string]any{{"upload_id": up.ID, "filename": "photo.png"}},
	}
	var saved struct {
		DraftID     int64           `json:"draft_id"`
		Attachments []attachmentRef `json:"attachments"`
	}
	alice.must("POST", "/api/drafts", draft, &saved)
	if saved.DraftID == 0 || len(saved.Attachments) != 1 || saved.Attachments[0].MessageID != saved.DraftID {
		t.Fatalf("saved draft: %+v", saved)
	}
	var cs composeState
	alice.must("GET", fmt.Sprintf("/api/drafts/%d", saved.DraftID), nil, &cs)
	if len(cs.To) != 1 || cs.To[0].Address != "bob@example.test" || len(cs.Bcc) != 1 || cs.Subject != "Plans" ||
		!strings.Contains(cs.HTML, "draft body") || strings.Contains(cs.HTML, "<body") || len(cs.Attachments) != 1 {
		t.Fatalf("loaded draft: %+v", cs)
	}
	// Saving again replaces the draft; attachments now come from the old draft.
	first := saved.DraftID
	cs.HTML = "<p>draft body v2</p>"
	alice.must("POST", "/api/drafts", cs, &saved)
	if saved.DraftID == first || len(saved.Attachments) != 1 {
		t.Fatalf("resaved draft: %+v", saved)
	}
	if code := alice.do("GET", fmt.Sprintf("/api/drafts/%d", first), nil, nil); code != http.StatusNotFound {
		t.Fatalf("old draft still exists: %d", code)
	}
	if drafts := alice.mailbox(store.SpecialDrafts); drafts.Total != 1 {
		t.Fatalf("drafts folder: %+v", drafts)
	}

	// Send with the default undo delay, then undo.
	cs.DraftID = saved.DraftID
	cs.Attachments = saved.Attachments
	var res struct {
		QueueID   int64     `json:"queue_id"`
		SendAt    time.Time `json:"send_at"`
		Scheduled bool      `json:"scheduled"`
	}
	alice.must("POST", "/api/send", cs, &res)
	if res.Scheduled || time.Until(res.SendAt) < 2*time.Second {
		t.Fatalf("send result: %+v", res)
	}
	if drafts := alice.mailbox(store.SpecialDrafts); drafts.Total != 0 {
		t.Fatalf("draft not removed after sending: %+v", drafts)
	}
	var undone struct {
		DraftID int64 `json:"draft_id"`
	}
	alice.must("POST", fmt.Sprintf("/api/outbox/%d/cancel", res.QueueID), nil, &undone)
	if code := alice.do("POST", fmt.Sprintf("/api/outbox/%d/cancel", res.QueueID), nil, nil); code != http.StatusNotFound {
		t.Fatalf("second undo: %d", code)
	}
	var restored composeState
	alice.must("GET", fmt.Sprintf("/api/drafts/%d", undone.DraftID), nil, &restored)
	if restored.Subject != "Plans" || len(restored.Bcc) != 1 || len(restored.Attachments) != 1 || !strings.Contains(restored.HTML, "v2") {
		t.Fatalf("restored draft: %+v", restored)
	}
	if sent := alice.mailbox(store.SpecialSent); sent.Total != 0 {
		t.Fatalf("sent copy left after undo: %+v", sent)
	}

	// Scheduled sending.
	at := time.Now().Add(2 * time.Hour)
	restored.ScheduleAt = &at
	restored.Attachments = nil
	alice.must("POST", "/api/send", restored, &res)
	if !res.Scheduled || res.SendAt.Sub(at).Abs() > time.Second {
		t.Fatalf("scheduled: %+v", res)
	}
	var outbox []*store.QueueEntry
	alice.must("GET", "/api/outbox", nil, &outbox)
	if len(outbox) != 1 || outbox[0].Subject != "Plans" {
		t.Fatalf("outbox: %+v", outbox)
	}
	tooFar := time.Now().Add(400 * 24 * time.Hour)
	restored.ScheduleAt = &tooFar
	if code := alice.do("POST", "/api/send", restored, nil); code != http.StatusBadRequest {
		t.Fatalf("scheduling too far ahead: %d", code)
	}
}

func deliver(t *testing.T, e *env, to *store.User, raw string) *store.Message {
	t.Helper()
	m, err := e.d.DeliverLocal(context.Background(), to, &delivery.Inbound{Raw: []byte(strings.ReplaceAll(raw, "\n", "\r\n")), MailFrom: "x@remote.test"})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestActionsLabelsAndRendering(t *testing.T) {
	e := newEnv(t, Options{})
	u := e.user("alice@example.test", false)
	c := e.login("alice@example.test")
	m := deliver(t, e, u, `From: News <news@remote.test>
To: alice@example.test
Subject: Weekly news
Message-ID: <n1@remote.test>
List-Unsubscribe: <mailto:unsub@remote.test>, <https://remote.test/unsub?id=1>
MIME-Version: 1.0
Content-Type: multipart/related; boundary=B

--B
Content-Type: text/html; charset=utf-8

<p>Hi <img src="cid:logo@x"> <img src="https://tracker.test/p.gif"></p>
--B
Content-Type: image/png
Content-ID: <logo@x>
Content-Transfer-Encoding: base64

iVBORw0KGgo=
--B--
`)
	var mv struct {
		Body          email.Rendered `json:"body"`
		RemoteAllowed bool           `json:"remote_allowed"`
		Unsubscribe   string         `json:"unsubscribe"`
	}
	c.must("GET", fmt.Sprintf("/api/messages/%d", m.ID), nil, &mv)
	if !strings.Contains(mv.Body.Document, `src="data:image/png;base64,iVBORw0KGgo="`) || strings.Contains(mv.Body.Document, "tracker.test") ||
		!mv.Body.RemoteBlocked || mv.RemoteAllowed || mv.Unsubscribe != "https://remote.test/unsub?id=1" {
		t.Fatalf("rendering: %+v", mv)
	}
	c.must("GET", fmt.Sprintf("/api/messages/%d?remote=1", m.ID), nil, &mv)
	if !strings.Contains(mv.Body.Document, "tracker.test") || !mv.RemoteAllowed {
		t.Fatalf("remote content not shown on request: %s", mv.Body.Document)
	}
	c.must("POST", "/api/senders", map[string]string{"address": "news@remote.test", "kind": "trusted"}, nil)
	c.must("GET", fmt.Sprintf("/api/messages/%d", m.ID), nil, &mv)
	if !mv.RemoteAllowed {
		t.Fatal("remote content not shown for a trusted sender")
	}

	var label store.Label
	c.must("POST", "/api/labels", map[string]string{"name": "Reading", "color": "#336699"}, &label)
	if code := c.do("POST", "/api/labels", map[string]string{"name": "Bad", "color": "red; x"}, nil); code != http.StatusBadRequest {
		t.Fatalf("bad label color: %d", code)
	}
	inbox := c.mailbox(store.SpecialInbox)
	view := map[string]any{"kind": "mailbox", "mailbox_id": inbox.ID}
	thread := []int64{m.ThreadID}
	c.must("POST", "/api/threads/actions", map[string]any{"thread_ids": thread, "view": view, "action": "label", "label_id": label.ID}, nil)
	if res := c.list(fmt.Sprintf("view=label&label=%d", label.ID)); res.Total != 1 {
		t.Fatalf("label view: %+v", res)
	}
	c.must("POST", "/api/threads/actions", map[string]any{"thread_ids": thread, "view": view, "action": "star"}, nil)
	if res := c.list("view=starred"); res.Total != 1 {
		t.Fatalf("starred view: %+v", res)
	}
	if res := c.list("view=all&q=" + "subject:weekly"); res.Total != 1 {
		t.Fatalf("search: %+v", res)
	}
	if code := c.do("GET", "/api/conversations?q=%22unbalanced", nil, nil); code != http.StatusOK && code != http.StatusBadRequest {
		t.Fatalf("bad search query: %d", code)
	}
	var archived actionResult
	c.must("POST", "/api/threads/actions", map[string]any{"thread_ids": thread, "view": view, "action": "archive"}, &archived)
	if archived.Affected != 1 || len(archived.Undo) != 1 || archived.Undo[0].MailboxID != inbox.ID || archived.Undo[0].IDs[0] != m.ID {
		t.Fatalf("archive result: %+v", archived)
	}
	// Undo puts the messages back where they were.
	c.must("POST", "/api/messages/actions", map[string]any{"ids": archived.Undo[0].IDs, "action": "move", "mailbox_id": archived.Undo[0].MailboxID}, nil)
	if inbox := c.mailbox(store.SpecialInbox); inbox.Total != 1 {
		t.Fatalf("inbox after undo: %+v", inbox)
	}
	c.must("POST", "/api/threads/actions", map[string]any{"thread_ids": thread, "view": view, "action": "archive"}, nil)
	if inbox := c.mailbox(store.SpecialInbox); inbox.Total != 0 {
		t.Fatalf("inbox after archive: %+v", inbox)
	}
	if archive := c.mailbox(store.SpecialArchive); archive.Total != 1 {
		t.Fatalf("archive: %+v", archive)
	}
	c.must("POST", "/api/messages/actions", map[string]any{"ids": []int64{m.ID}, "action": "trash"}, nil)
	trash := c.mailbox(store.SpecialTrash)
	if res := c.list("view=all"); res.Total != 0 {
		t.Fatalf("trashed message still listed in All Mail: %+v", res)
	}
	if code := c.do("POST", fmt.Sprintf("/api/mailboxes/%d/empty", inbox.ID), nil, nil); code != http.StatusBadRequest {
		t.Fatalf("emptying the inbox: %d", code)
	}
	c.must("POST", fmt.Sprintf("/api/mailboxes/%d/empty", trash.ID), nil, nil)
	if trash := c.mailbox(store.SpecialTrash); trash.Total != 0 {
		t.Fatalf("trash after emptying: %+v", trash)
	}
	if code := c.do("POST", "/api/messages/actions", map[string]any{"ids": []int64{m.ID}, "action": "explode"}, nil); code != http.StatusBadRequest {
		t.Fatalf("unknown action: %d", code)
	}

	var mb store.Mailbox
	c.must("POST", "/api/mailboxes", map[string]string{"name": "Projects"}, &mb)
	c.must("PATCH", fmt.Sprintf("/api/mailboxes/%d", mb.ID), map[string]string{"name": "Work"}, &mb)
	if mb.Name != "Work" {
		t.Fatalf("renamed mailbox: %+v", mb)
	}
	if code := c.do("DELETE", fmt.Sprintf("/api/mailboxes/%d", inbox.ID), nil, nil); code != http.StatusBadRequest {
		t.Fatalf("deleting the inbox: %d", code)
	}
	// Deleting a folder (and its sub-folders) moves their messages to Trash.
	var sub store.Mailbox
	c.must("POST", "/api/mailboxes", map[string]string{"name": "Work/2024"}, &sub)
	kept := deliver(t, e, u, "From: a@remote.test\nTo: alice@example.test\nSubject: Keep me\n\nbody\n")
	c.must("POST", "/api/messages/actions", map[string]any{"ids": []int64{kept.ID}, "action": "move", "mailbox_id": sub.ID}, nil)
	c.must("DELETE", fmt.Sprintf("/api/mailboxes/%d", mb.ID), nil, nil)
	if trash := c.mailbox(store.SpecialTrash); trash.Total != 1 {
		t.Fatalf("trash after deleting a folder: %+v", trash)
	}
	var left []store.Mailbox
	c.must("GET", "/api/mailboxes", nil, &left)
	for _, x := range left {
		if x.ID == sub.ID || x.ID == mb.ID {
			t.Fatalf("folder still exists: %+v", x)
		}
	}
}

func TestAccountAndAdmin(t *testing.T) {
	e := newEnv(t, Options{IMAPSPort: 993, SubmissionsPort: 465, TLS: true})
	e.user("admin@example.test", true)
	e.user("alice@example.test", false)
	admin, alice := e.login("admin@example.test"), e.login("alice@example.test")

	if code := alice.do("GET", "/api/admin/users", nil, nil); code != http.StatusForbidden {
		t.Fatalf("non-admin access: %d", code)
	}
	var carol store.User
	admin.must("POST", "/api/admin/users", map[string]any{"email": "carol@example.test", "password": "correct horse"}, &carol)
	if code := admin.do("POST", "/api/admin/users", map[string]any{"email": "carol@example.test", "password": "correct horse"}, nil); code != http.StatusConflict {
		t.Fatalf("duplicate user: %d", code)
	}
	carolClient := e.login("carol@example.test")
	disabled := true
	admin.must("PATCH", fmt.Sprintf("/api/admin/users/%d", carol.ID), map[string]any{"disabled": disabled}, nil)
	if code := carolClient.do("GET", "/api/mailboxes", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("disabled user's session still works: %d", code)
	}
	if code := e.client().do("POST", "/api/login", map[string]string{"email": "carol@example.test", "password": "correct horse"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("disabled user can log in: %d", code)
	}
	var me sessionInfo
	admin.must("GET", "/api/session", nil, &me)
	if code := admin.do("DELETE", fmt.Sprintf("/api/admin/users/%d", me.User.ID), nil, nil); code != http.StatusBadRequest {
		t.Fatalf("deleting own account: %d", code)
	}
	if code := admin.do("PATCH", fmt.Sprintf("/api/admin/users/%d", me.User.ID), map[string]any{"is_admin": false}, nil); code != http.StatusBadRequest {
		t.Fatalf("removing own admin access: %d", code)
	}

	var alias store.Alias
	admin.must("POST", "/api/admin/aliases", map[string]any{"address": "hello@example.test", "user_id": alice.userID()}, &alias)
	var aliases []*store.Alias
	alice.must("GET", "/api/account/aliases", nil, &aliases)
	if len(aliases) != 1 || aliases[0].Address != "hello@example.test" {
		t.Fatalf("aliases: %+v", aliases)
	}

	var domains []*store.Domain
	admin.must("GET", "/api/admin/domains", nil, &domains)
	if len(domains) != 1 {
		t.Fatalf("domains: %+v", domains)
	}
	e.res.mx["example.test"] = []*net.MX{{Host: testHost + ".", Pref: 10}}
	e.res.txt["example.test"] = []string{"v=spf1 mx -all"}
	var dns struct {
		Records []DNSRecord `json:"records"`
	}
	admin.must("GET", fmt.Sprintf("/api/admin/domains/%d/dns?check=1", domains[0].ID), nil, &dns)
	status := map[string]string{}
	for _, r := range dns.Records {
		status[r.Type+" "+r.Name] = r.Status
	}
	if status["MX example.test"] != "ok" || status["TXT example.test"] != "ok" || status["TXT titomail._domainkey.example.test"] != "missing" ||
		status["SRV _imaps._tcp.example.test"] != "unchecked" {
		t.Fatalf("DNS check: %v", status)
	}
	if code := admin.do("DELETE", fmt.Sprintf("/api/admin/domains/%d", domains[0].ID), nil, nil); code != http.StatusBadRequest {
		t.Fatalf("deleting a domain with users: %d", code)
	}

	// Password change signs out other sessions.
	other := e.login("alice@example.test")
	if code := alice.do("POST", "/api/account/password", map[string]string{"current": "wrong", "new": "new password!"}, nil); code != http.StatusForbidden {
		t.Fatalf("password change with wrong current password: %d", code)
	}
	alice.must("POST", "/api/account/password", map[string]string{"current": "correct horse", "new": "new password!"}, nil)
	if code := other.do("GET", "/api/mailboxes", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("other session survived password change: %d", code)
	}
	alice.must("GET", "/api/mailboxes", nil, nil)

	var st store.Settings
	alice.must("GET", "/api/settings", nil, &st)
	st.Theme = "dark"
	st.Vacation = store.Vacation{Enabled: true, Subject: "Away", Body: "Back soon"}
	alice.must("PUT", "/api/settings", st, &st)
	if st.Theme != "dark" || !st.Vacation.Enabled {
		t.Fatalf("settings: %+v", st)
	}
	var f store.Filter
	alice.must("POST", "/api/filters", store.Filter{Name: "news", Enabled: true, Criteria: store.FilterCriteria{From: "news@"},
		Actions: store.FilterActions{MarkRead: true}}, &f)
	if code := alice.do("POST", "/api/filters", store.Filter{Name: "empty", Enabled: true}, nil); code != http.StatusBadRequest {
		t.Fatalf("filter without criteria: %d", code)
	}
	var contact store.Contact
	alice.must("POST", "/api/contacts", map[string]string{"email": "Dave@Remote.test", "name": "Dave"}, &contact)
	var contacts []*store.Contact
	alice.must("GET", "/api/contacts?saved=1", nil, &contacts)
	if len(contacts) != 1 || contacts[0].Email != "dave@remote.test" {
		t.Fatalf("contacts: %+v", contacts)
	}
}

func TestStaticAutoconfigAndHeaders(t *testing.T) {
	big := strings.Repeat("console.log('titomail');\n", 200)
	e := newEnv(t, Options{TLS: true, IMAPSPort: 993, IMAPPort: 143, SubmissionsPort: 465, SubmissionPort: 587,
		Static: fstest.MapFS{
			"index.html":    {Data: []byte("<!DOCTYPE html><title>TitoMail</title>")},
			"assets/app.js": {Data: []byte(big)},
		}})
	c := e.client()
	get := func(path string) (*http.Response, string) {
		t.Helper()
		resp := c.request("GET", path, nil, "")
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, body := get("/mail/inbox")
	if resp.StatusCode != 200 || !strings.Contains(body, "<title>TitoMail") || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("SPA fallback: %d %q", resp.StatusCode, body)
	}
	for _, h := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("missing %s header", h)
		}
	}
	resp, body = get("/assets/app.js")
	if body != big || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") || !strings.Contains(resp.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("asset: %v", resp.Header)
	}
	if resp, _ := get("/assets/missing.js"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing asset: %d", resp.StatusCode)
	}
	if resp, body := get("/api/nope"); resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "error") {
		t.Fatalf("unknown API endpoint: %d %s", resp.StatusCode, body)
	}

	_, body = get("/.well-known/autoconfig/mail/config-v1.1.xml?emailaddress=alice%40example.test")
	for _, want := range []string{"<domain>example.test</domain>", "<hostname>" + testHost + "</hostname>", "<port>993</port>",
		"<socketType>SSL</socketType>", "<port>587</port>", "<socketType>STARTTLS</socketType>", "%EMAILADDRESS%"} {
		if !strings.Contains(body, want) {
			t.Errorf("autoconfig lacks %q:\n%s", want, body)
		}
	}
	req := `<?xml version="1.0"?><Autodiscover xmlns="http://schemas.microsoft.com/exchange/autodiscover/outlook/requestschema/2006"><Request><EMailAddress>alice@example.test</EMailAddress></Request></Autodiscover>`
	resp = c.request("POST", "/autodiscover/autodiscover.xml", strings.NewReader(req), "text/xml")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"<Type>IMAP</Type>", "<Port>993</Port>", "<Type>SMTP</Type>", "<Port>465</Port>", "<LoginName>alice@example.test</LoginName>"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("autodiscover lacks %q:\n%s", want, b)
		}
	}
}

func TestEventStream(t *testing.T) {
	e := newEnv(t, Options{})
	u := e.user("alice@example.test", false)
	c := e.login("alice@example.test")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/api/events", nil)
	resp, err := c.hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	r := bufio.NewReader(resp.Body)
	if line, _ := r.ReadString('\n'); !strings.HasPrefix(line, "retry:") {
		t.Fatalf("first line %q", line)
	}
	m := deliver(t, e, u, "From: x@remote.test\nTo: alice@example.test\nSubject: ping\n\nhello\n")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended: %v", err)
		}
		if strings.HasPrefix(line, "data: ") {
			var ev events.Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				t.Fatal(err)
			}
			if !ev.Messages || len(ev.Added) != 1 || ev.Added[0] != m.MailboxID {
				t.Fatalf("event %+v", ev)
			}
			return
		}
	}
}
