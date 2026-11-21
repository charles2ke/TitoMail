package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() { PasswordCost = 4 }

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newTestUser(t *testing.T, s *Store, addr string) *User {
	t.Helper()
	ctx := context.Background()
	domain := addr[strings.IndexByte(addr, '@')+1:]
	if !s.IsLocalDomain(ctx, domain) {
		if _, err := s.CreateDomain(ctx, domain); err != nil {
			t.Fatal(err)
		}
	}
	u, err := s.CreateUser(ctx, NewUser{Email: addr, Password: "correct horse", DisplayName: "Test User"})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func rawMsg(from, to, subject, msgID, inReplyTo, body string) []byte {
	h := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: Mon, 02 Jan 2006 15:04:05 +0000\r\nMessage-ID: <%s>\r\n", from, to, subject, msgID)
	if inReplyTo != "" {
		h += fmt.Sprintf("In-Reply-To: <%s>\r\nReferences: <%s>\r\n", inReplyTo, inReplyTo)
	}
	return []byte(h + "Content-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n")
}

func mustMailbox(t *testing.T, s *Store, userID int64, special string) *Mailbox {
	t.Helper()
	mb, err := s.MailboxBySpecial(context.Background(), userID, special)
	if err != nil {
		t.Fatal(err)
	}
	return mb
}

func TestUsersAndAuth(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newTestUser(t, s, "Alice@Example.com")
	if u.Email != "alice@example.com" {
		t.Errorf("email not normalized: %q", u.Email)
	}
	mbs, err := s.ListMailboxes(ctx, u.ID)
	if err != nil || len(mbs) != 6 || mbs[0].Name != "INBOX" || mbs[5].SpecialUse != SpecialTrash {
		t.Fatalf("default mailboxes = %+v, %v", mbs, err)
	}
	if _, err := s.Authenticate(ctx, "ALICE@example.com", "correct horse"); err != nil {
		t.Errorf("auth failed: %v", err)
	}
	if _, err := s.Authenticate(ctx, "alice@example.com", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("expected invalid credentials, got %v", err)
	}
	if _, err := s.CreateUser(ctx, NewUser{Email: "alice@example.com", Password: "correct horse"}); !errors.Is(err, ErrConflict) {
		t.Errorf("expected conflict, got %v", err)
	}
	if _, err := s.CreateUser(ctx, NewUser{Email: "bob@unknown.org", Password: "correct horse"}); err == nil {
		t.Error("expected error for unhosted domain")
	}
	disabled := true
	if err := s.UpdateUser(ctx, u.ID, UserUpdate{Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "alice@example.com", "correct horse"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("disabled user could log in: %v", err)
	}
	d, err := s.DomainByName(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if pk, err := d.DKIMPublicKey(); err != nil || len(pk) < 300 {
		t.Errorf("dkim public key = %q, %v", pk, err)
	}
}

func TestResolveAddress(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	alice := newTestUser(t, s, "alice@example.com")
	bob := newTestUser(t, s, "bob@example.com")
	if _, err := s.CreateAlias(ctx, "sales@example.com", alice.ID); err != nil {
		t.Fatal(err)
	}
	cases := map[string]int64{
		"alice@example.com":      alice.ID,
		"ALICE+news@example.com": alice.ID,
		"sales@example.com":      alice.ID,
		"bob@example.com":        bob.ID,
	}
	for addr, want := range cases {
		u, err := s.ResolveAddress(ctx, addr)
		if err != nil || u.ID != want {
			t.Errorf("ResolveAddress(%q) = %v, %v; want user %d", addr, u, err, want)
		}
	}
	if _, err := s.ResolveAddress(ctx, "nobody@example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected not found, got %v", err)
	}
	if _, err := s.CreateAlias(ctx, "@example.com", bob.ID); err != nil {
		t.Fatal(err)
	}
	if u, err := s.ResolveAddress(ctx, "nobody@example.com"); err != nil || u.ID != bob.ID {
		t.Errorf("catch-all not applied: %v %v", u, err)
	}
	if !s.CanSendAs(ctx, alice, "sales@example.com") || !s.CanSendAs(ctx, alice, "alice+x@example.com") {
		t.Error("alice should be able to send as her alias / plus address")
	}
	if s.CanSendAs(ctx, alice, "bob@example.com") {
		t.Error("alice must not send as bob")
	}
}

func TestThreadingSearchAndListing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newTestUser(t, s, "me@example.com")
	inbox := mustMailbox(t, s, u.ID, SpecialInbox)
	sent := mustMailbox(t, s, u.ID, SpecialSent)

	var changes []Change
	unsub := s.Subscribe(func(c Change) { changes = append(changes, c) })
	defer unsub()

	insert := func(mb *Mailbox, raw []byte, flags ...string) *Message {
		t.Helper()
		m, err := s.InsertMessage(ctx, NewMessage{UserID: u.ID, MailboxID: mb.ID, Raw: raw, Flags: flags})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	// Reply arrives before its parent: threading must still join them.
	reply := insert(inbox, rawMsg("Ann <ann@other.org>", "me@example.com", "Re: Quarterly plan", "r1@other.org", "p1@example.com", "Sounds great, the budget works"))
	parent := insert(sent, rawMsg("me@example.com", "ann@other.org", "Quarterly plan", "p1@example.com", "", "Here is the plan"), FlagSeen)
	other := insert(inbox, rawMsg("Bob <bob@else.net>", "me@example.com", "Lunch?", "l1@else.net", "", "Tacos at noon"))
	if reply.ThreadID != parent.ThreadID {
		t.Errorf("out-of-order reply not threaded: %d vs %d", reply.ThreadID, parent.ThreadID)
	}
	if other.ThreadID == parent.ThreadID {
		t.Error("unrelated message joined thread")
	}
	if reply.UID != 1 || other.UID != 2 || parent.UID != 1 {
		t.Errorf("uids = %d %d %d", reply.UID, other.UID, parent.UID)
	}
	if len(changes) != 3 || changes[0].Kind != MessagesAdded || changes[0].MailboxID != inbox.ID {
		t.Errorf("changes = %+v", changes)
	}

	res, err := s.ListConversations(ctx, u.ID, ListOptions{View: View{Kind: ViewMailbox, MailboxID: inbox.ID}, Threaded: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 2 || len(res.Conversations) != 2 {
		t.Fatalf("inbox list = %+v", res)
	}
	c := res.Conversations[1] // older: the quarterly plan thread
	if c.Subject != "Re: Quarterly plan" || c.Count != 2 || !c.Unread || len(c.MessageIDs) != 1 || len(c.Participants) != 2 {
		t.Errorf("conversation = %+v", c)
	}

	search := func(q string) []int64 {
		t.Helper()
		r, err := s.ListConversations(ctx, u.ID, ListOptions{View: View{Kind: ViewAll}, Query: q, Threaded: false})
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		var ids []int64
		for _, c := range r.Conversations {
			ids = append(ids, c.LatestID)
		}
		return ids
	}
	if ids := search("budg"); len(ids) != 1 || ids[0] != reply.ID {
		t.Errorf("prefix full-text search = %v", ids)
	}
	if ids := search(`from:bob`); len(ids) != 1 || ids[0] != other.ID {
		t.Errorf("from: search = %v", ids)
	}
	if ids := search(`is:unread -tacos`); len(ids) != 1 || ids[0] != reply.ID {
		t.Errorf("negated search = %v", ids)
	}
	if ids := search(`"the plan"`); len(ids) != 1 || ids[0] != parent.ID {
		t.Errorf("phrase search = %v", ids)
	}
	if ids := search(`in:sent`); len(ids) != 1 || ids[0] != parent.ID {
		t.Errorf("in:sent search = %v", ids)
	}
	if _, err := s.ListConversations(ctx, u.ID, ListOptions{Query: "is:bogus"}); err == nil {
		t.Error("expected error for invalid operator value")
	}

	thread, err := s.ThreadMessages(ctx, u.ID, parent.ThreadID, false)
	if err != nil || len(thread) != 2 {
		t.Fatalf("thread = %v, %v", thread, err)
	}
}

func TestFlagsMoveCopyExpungeAndBlobGC(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newTestUser(t, s, "me@example.com")
	inbox := mustMailbox(t, s, u.ID, SpecialInbox)
	archive := mustMailbox(t, s, u.ID, SpecialArchive)
	raw := rawMsg("x@y.org", "me@example.com", "Hello", "h1@y.org", "", "unique-word-zebra")
	m, err := s.InsertMessage(ctx, NewMessage{UserID: u.ID, MailboxID: inbox.ID, Raw: raw})
	if err != nil {
		t.Fatal(err)
	}

	var changes []Change
	unsub := s.Subscribe(func(c Change) { changes = append(changes, c) })
	defer unsub()

	flags, err := s.UpdateFlags(ctx, u.ID, inbox.ID, []uint32{m.UID}, FlagsAdd, []string{FlagSeen, "$Work"}, "src")
	if err != nil || strings.Join(flags[m.UID], " ") != `\Seen $Work` {
		t.Fatalf("flags = %v, %v", flags, err)
	}
	if len(changes) != 1 || changes[0].Kind != FlagsChanged || changes[0].Source != "src" {
		t.Errorf("flag change = %+v", changes)
	}
	changes = nil
	if _, err := s.UpdateFlags(ctx, u.ID, inbox.ID, []uint32{m.UID}, FlagsAdd, []string{FlagSeen}, nil); err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Errorf("no-op flag update emitted %+v", changes)
	}

	src, dst, err := s.CopyMessages(ctx, u.ID, inbox.ID, []uint32{m.UID}, archive.ID, nil)
	if err != nil || len(src) != 1 || len(dst) != 1 || dst[0] != 1 {
		t.Fatalf("copy = %v %v %v", src, dst, err)
	}
	copies, _ := s.MessagesByUID(ctx, archive.ID, dst)
	if len(copies) != 1 || copies[0].BlobID != m.BlobID || !copies[0].Seen {
		t.Fatalf("copy = %+v", copies)
	}
	// Move inbox -> archive by ID.
	if n, err := s.MoveByID(ctx, u.ID, []int64{m.ID}, archive.ID); err != nil || n != 1 {
		t.Fatalf("move = %d, %v", n, err)
	}
	if uids, _ := s.MailboxUIDs(ctx, archive.ID); len(uids) != 2 || uids[1] != 2 {
		t.Errorf("archive uids = %v", uids)
	}
	if uids, _ := s.MailboxUIDs(ctx, inbox.ID); len(uids) != 0 {
		t.Errorf("inbox uids = %v", uids)
	}
	// Expunge both: blob and FTS row must be garbage collected.
	if _, err := s.UpdateFlags(ctx, u.ID, archive.ID, []uint32{1, 2}, FlagsAdd, []string{FlagDeleted}, nil); err != nil {
		t.Fatal(err)
	}
	removed, err := s.Expunge(ctx, u.ID, archive.ID, []uint32{1}, nil)
	if err != nil || len(removed) != 1 || removed[0] != 1 {
		t.Fatalf("expunge = %v, %v", removed, err)
	}
	if _, err := s.ReadBlob(ctx, m.BlobID); err != nil {
		t.Errorf("blob deleted while still referenced: %v", err)
	}
	if _, err := s.Expunge(ctx, u.ID, archive.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadBlob(ctx, m.BlobID); !errors.Is(err, ErrNotFound) {
		t.Errorf("blob not collected: %v", err)
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'zebra'`).Scan(&n)
	if n != 0 {
		t.Errorf("fts row not removed (%d)", n)
	}
}

func TestPurgeOldCountsFromMoveIntoTrash(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newTestUser(t, s, "me@example.com")
	inbox := mustMailbox(t, s, u.ID, SpecialInbox)
	trash := mustMailbox(t, s, u.ID, SpecialTrash)
	old := time.Now().Add(-90 * 24 * time.Hour)
	m, err := s.InsertMessage(ctx, NewMessage{UserID: u.ID, MailboxID: inbox.ID, Raw: rawMsg("x@y.org", "me@example.com", "Old", "old@y.org", "", "hi"), ReceivedAt: old})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveByID(ctx, u.ID, []int64{m.ID}, trash.ID); err != nil {
		t.Fatal(err)
	}
	// A message received long ago but trashed just now must survive the 30-day purge.
	if n, err := s.PurgeOld(ctx, SpecialTrash, time.Now().Add(-30*24*time.Hour)); err != nil || n != 0 {
		t.Fatalf("purge right after trashing = %d, %v", n, err)
	}
	if n, err := s.PurgeOld(ctx, SpecialTrash, time.Now().Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("purge after retention = %d, %v", n, err)
	}
}

func TestMailboxRenameAndDelete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newTestUser(t, s, "me@example.com")
	child, err := s.CreateMailbox(ctx, u.ID, "Work/Projects")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.MailboxByName(ctx, u.ID, "Work")
	if err != nil {
		t.Fatalf("parent not auto-created: %v", err)
	}
	if err := s.RenameMailbox(ctx, u.ID, parent.ID, "Jobs"); err != nil {
		t.Fatal(err)
	}
	if mb, err := s.GetMailbox(ctx, u.ID, child.ID); err != nil || mb.Name != "Jobs/Projects" {
		t.Errorf("child after rename = %+v, %v", mb, err)
	}
	if err := s.DeleteMailbox(ctx, u.ID, parent.ID, false); err == nil {
		t.Error("expected error deleting folder with children non-recursively")
	}
	if err := s.DeleteMailbox(ctx, u.ID, parent.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetMailbox(ctx, u.ID, child.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("child survived recursive delete: %v", err)
	}
	inbox := mustMailbox(t, s, u.ID, SpecialInbox)
	if err := s.DeleteMailbox(ctx, u.ID, inbox.ID, true); err == nil {
		t.Error("inbox must not be deletable")
	}
	if _, err := s.CreateMailbox(ctx, u.ID, "bad*name"); err == nil {
		t.Error("wildcards must be rejected")
	}
}

func TestSubfoldersMatchExactly(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newTestUser(t, s, "me@example.com")
	for _, name := range []string{"Work", "work/Reports", "Données/2024", "日本語/子"} {
		if _, err := s.CreateMailbox(ctx, u.ID, name); err != nil {
			t.Fatal(err)
		}
	}
	byName := func(name string) *Mailbox {
		t.Helper()
		mb, err := s.MailboxByName(ctx, u.ID, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return mb
	}
	// Folder names are case-sensitive: "work/Reports" is not inside "Work".
	if has, err := s.HasChildren(ctx, u.ID, "Work"); err != nil || has {
		t.Errorf("HasChildren(Work) = %v, %v; want false", has, err)
	}
	if has, err := s.HasChildren(ctx, u.ID, "work"); err != nil || !has {
		t.Errorf("HasChildren(work) = %v, %v; want true", has, err)
	}
	for from, to := range map[string]string{"Work": "Projects", "Données": "Data", "日本語": "Japanese"} {
		if err := s.RenameMailbox(ctx, u.ID, byName(from).ID, to); err != nil {
			t.Fatalf("rename %s: %v", from, err)
		}
	}
	for _, name := range []string{"Projects", "work/Reports", "Data/2024", "Japanese/子"} {
		byName(name)
	}
	if err := s.DeleteMailbox(ctx, u.ID, byName("Projects").ID, false); err != nil {
		t.Fatalf("delete Projects: %v", err)
	}
	if err := s.DeleteMailbox(ctx, u.ID, byName("work").ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MailboxByName(ctx, u.ID, "work/Reports"); !errors.Is(err, ErrNotFound) {
		t.Errorf("work/Reports survived deleting work: %v", err)
	}
}

func TestQueueLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newTestUser(t, s, "me@example.com")
	sent := mustMailbox(t, s, u.ID, SpecialSent)
	copyMsg, err := s.InsertMessage(ctx, NewMessage{UserID: u.ID, MailboxID: sent.ID, Raw: rawMsg("me@example.com", "a@x.org", "Hi", "q1@example.com", "", "hi")})
	if err != nil {
		t.Fatal(err)
	}
	e := &QueueEntry{UserID: u.ID, MailFrom: "me@example.com", Data: []byte("data"), SentCopyID: copyMsg.ID}
	now := time.Now()
	if _, err := s.Enqueue(ctx, e, []string{"a@x.org", "b@x.org", "c@y.org", "A@x.org"}, now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.ClaimDue(ctx, now, time.Minute, 10); len(b) != 0 {
		t.Errorf("claimed before due: %+v", b)
	}
	// Undo send within the delay window.
	if _, err := s.CancelQueued(ctx, u.ID, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetMessage(ctx, u.ID, copyMsg.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("sent copy not removed on undo: %v", err)
	}

	e2 := &QueueEntry{UserID: u.ID, MailFrom: "me@example.com", Data: []byte("data2")}
	if _, err := s.Enqueue(ctx, e2, []string{"a@x.org", "b@x.org", "c@y.org"}, now); err != nil {
		t.Fatal(err)
	}
	batches, err := s.ClaimDue(ctx, now, time.Minute, 10)
	if err != nil || len(batches) != 2 || len(batches[0].Rcpts) != 2 || batches[0].Domain != "x.org" || string(batches[0].Entry.Data) != "data2" {
		t.Fatalf("batches = %+v, %v", batches, err)
	}
	if again, _ := s.ClaimDue(ctx, now, time.Minute, 10); len(again) != 0 {
		t.Errorf("leased recipients claimed twice")
	}
	if _, err := s.CancelQueued(ctx, u.ID, e2.ID); !errors.Is(err, ErrTooLate) {
		t.Errorf("cancel after claim = %v, want ErrTooLate", err)
	}
	var results []RcptResult
	for _, b := range batches {
		for _, r := range b.Rcpts {
			results = append(results, RcptResult{ID: r.ID, Status: RcptSent})
		}
	}
	results[2].Status, results[2].Error = RcptFailed, "550 no such user"
	if err := s.RecordAttempts(ctx, results); err != nil {
		t.Fatal(err)
	}
	done, err := s.CompletedEntries(ctx)
	if err != nil || len(done) != 1 || done[0].Rcpts[2].Status != RcptFailed {
		t.Fatalf("completed = %+v, %v", done, err)
	}
}

func TestVacationAndSenderPrefs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	u := newTestUser(t, s, "me@example.com")
	ok, _ := s.ClaimVacationReply(ctx, u.ID, "Ann@x.org", 24*time.Hour)
	again, _ := s.ClaimVacationReply(ctx, u.ID, "ann@x.org", 24*time.Hour)
	if !ok || again {
		t.Errorf("vacation claim = %v, %v", ok, again)
	}
	if err := s.SetSenderPref(ctx, u.ID, "@spam.example", SenderBlocked); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSenderPref(ctx, u.ID, "friend@spam.example", SenderTrusted); err != nil {
		t.Fatal(err)
	}
	if k := s.GetSenderPref(ctx, u.ID, "x@spam.example"); k != SenderBlocked {
		t.Errorf("domain pref = %q", k)
	}
	if k := s.GetSenderPref(ctx, u.ID, "Friend@spam.example"); k != SenderTrusted {
		t.Errorf("address pref should win over domain: %q", k)
	}
}
