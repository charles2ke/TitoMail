package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/charles2ke/TitoMail/internal/email"
)

// IMAP system flags.
const (
	FlagSeen     = `\Seen`
	FlagFlagged  = `\Flagged`
	FlagAnswered = `\Answered`
	FlagDraft    = `\Draft`
	FlagDeleted  = `\Deleted`
)

// Message is the stored metadata of a message.
type Message struct {
	ID             int64              `json:"id"`
	UserID         int64              `json:"-"`
	MailboxID      int64              `json:"mailbox_id"`
	UID            uint32             `json:"-"`
	ThreadID       int64              `json:"thread_id"`
	BlobID         int64              `json:"-"`
	MessageID      string             `json:"message_id"`
	InReplyTo      string             `json:"in_reply_to"`
	References     []string           `json:"references"`
	Subject        string             `json:"subject"`
	From           email.Address      `json:"from"`
	To             []email.Address    `json:"to"`
	Cc             []email.Address    `json:"cc"`
	Bcc            []email.Address    `json:"bcc"`
	ReplyTo        []email.Address    `json:"reply_to"`
	Date           time.Time          `json:"date"`
	ReceivedAt     time.Time          `json:"received_at"`
	Size           int64              `json:"size"`
	Snippet        string             `json:"snippet"`
	HasAttachments bool               `json:"has_attachments"`
	Attachments    []email.Attachment `json:"attachments"`
	Seen           bool               `json:"seen"`
	Flagged        bool               `json:"flagged"`
	Answered       bool               `json:"answered"`
	Draft          bool               `json:"draft"`
	Deleted        bool               `json:"deleted"`
	Keywords       []string           `json:"keywords"`
	Labels         []int64            `json:"labels"`
}

// Flags returns the IMAP flags of the message.
func (m *Message) Flags() []string { return m.flagState().list() }

func (m *Message) flagState() flagState {
	return flagState{m.Seen, m.Flagged, m.Answered, m.Draft, m.Deleted, m.Keywords}
}

type flagState struct {
	seen, flagged, answered, draft, deleted bool
	keywords                                []string
}

func (f flagState) list() []string {
	var out []string
	if f.seen {
		out = append(out, FlagSeen)
	}
	if f.flagged {
		out = append(out, FlagFlagged)
	}
	if f.answered {
		out = append(out, FlagAnswered)
	}
	if f.draft {
		out = append(out, FlagDraft)
	}
	if f.deleted {
		out = append(out, FlagDeleted)
	}
	return append(out, f.keywords...)
}

func (f flagState) equal(o flagState) bool {
	if f.seen != o.seen || f.flagged != o.flagged || f.answered != o.answered || f.draft != o.draft ||
		f.deleted != o.deleted || len(f.keywords) != len(o.keywords) {
		return false
	}
	for i := range f.keywords {
		if f.keywords[i] != o.keywords[i] {
			return false
		}
	}
	return true
}

func (f *flagState) set(flag string, v bool) {
	switch strings.ToLower(flag) {
	case `\seen`:
		f.seen = v
	case `\flagged`:
		f.flagged = v
	case `\answered`:
		f.answered = v
	case `\draft`:
		f.draft = v
	case `\deleted`:
		f.deleted = v
	case `\recent`, "":
	default:
		if strings.HasPrefix(flag, `\`) || strings.ContainsAny(flag, " \t\r\n") {
			return
		}
		idx := -1
		for i, k := range f.keywords {
			if strings.EqualFold(k, flag) {
				idx = i
				break
			}
		}
		if v && idx < 0 {
			f.keywords = append(append([]string(nil), f.keywords...), flag)
		} else if !v && idx >= 0 {
			kw := append([]string(nil), f.keywords[:idx]...)
			f.keywords = append(kw, f.keywords[idx+1:]...)
		}
	}
}

func flagsFromList(flags []string) flagState {
	var f flagState
	for _, fl := range flags {
		f.set(fl, true)
	}
	return f
}

// FlagOp is the kind of flag modification.
type FlagOp int

// Flag operations.
const (
	FlagsAdd FlagOp = iota
	FlagsRemove
	FlagsSet
)

func (f flagState) apply(op FlagOp, flags []string) flagState {
	switch op {
	case FlagsSet:
		n := flagsFromList(flags)
		return n
	case FlagsAdd:
		for _, fl := range flags {
			f.set(fl, true)
		}
	case FlagsRemove:
		for _, fl := range flags {
			f.set(fl, false)
		}
	}
	return f
}

const msgCols = `m.id, m.user_id, m.mailbox_id, m.uid, m.thread_id, m.blob_id, m.message_id, m.in_reply_to, m.refs,
	m.subject, m.from_name, m.from_addr, m.to_json, m.cc_json, m.bcc_json, m.reply_to_json, m.sent_at, m.received_at,
	m.size, m.snippet, m.has_attachments, m.attachments_json, m.seen, m.flagged, m.answered, m.draft, m.deleted, m.keywords`

func scanMessage(row interface{ Scan(...any) error }) (*Message, error) {
	var m Message
	var uid, sent, received int64
	var refs, to, cc, bcc, replyTo, atts, keywords string
	err := row.Scan(&m.ID, &m.UserID, &m.MailboxID, &uid, &m.ThreadID, &m.BlobID, &m.MessageID, &m.InReplyTo, &refs,
		&m.Subject, &m.From.Name, &m.From.Address, &to, &cc, &bcc, &replyTo, &sent, &received,
		&m.Size, &m.Snippet, &m.HasAttachments, &atts, &m.Seen, &m.Flagged, &m.Answered, &m.Draft, &m.Deleted, &keywords)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	m.UID = uint32(uid)
	m.References = strings.Fields(refs)
	_ = json.Unmarshal([]byte(to), &m.To)
	_ = json.Unmarshal([]byte(cc), &m.Cc)
	_ = json.Unmarshal([]byte(bcc), &m.Bcc)
	_ = json.Unmarshal([]byte(replyTo), &m.ReplyTo)
	_ = json.Unmarshal([]byte(atts), &m.Attachments)
	m.Date = fromUnix(sent)
	m.ReceivedAt = fromUnixMilli(received)
	m.Keywords = strings.Fields(keywords)
	if m.References == nil {
		m.References = []string{}
	}
	if m.Keywords == nil {
		m.Keywords = []string{}
	}
	for _, p := range []*[]email.Address{&m.To, &m.Cc, &m.Bcc, &m.ReplyTo} {
		if *p == nil {
			*p = []email.Address{}
		}
	}
	if m.Attachments == nil {
		m.Attachments = []email.Attachment{}
	}
	m.Labels = []int64{}
	return &m, nil
}

func queryMessages(ctx context.Context, q queryer, query string, args ...any) ([]*Message, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func loadLabels(ctx context.Context, q queryer, msgs []*Message) error {
	if len(msgs) == 0 {
		return nil
	}
	byID := make(map[int64]*Message, len(msgs))
	ids := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		byID[m.ID] = m
		ids = append(ids, m.ID)
	}
	for _, part := range chunk(ids, 500) {
		rows, err := q.QueryContext(ctx, `SELECT message_id, label_id FROM message_labels WHERE message_id IN (`+placeholders(len(part))+`) ORDER BY label_id`, int64Args(part)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var mid, lid int64
			if err := rows.Scan(&mid, &lid); err != nil {
				rows.Close()
				return err
			}
			byID[mid].Labels = append(byID[mid].Labels, lid)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func addrs(list []email.Address) []email.Address {
	if list == nil {
		return []email.Address{}
	}
	return list
}

type ftsDoc struct {
	subject, sender, recipients, body, attachments string
}

const maxFTSBody = 200 * 1024

func buildFTS(p *email.Message) ftsDoc {
	var d ftsDoc
	d.subject = p.Subject
	var b strings.Builder
	for _, a := range p.From {
		b.WriteString(a.Name + " " + a.Address + " ")
	}
	d.sender = b.String()
	b.Reset()
	for _, a := range p.AllRecipients() {
		b.WriteString(a.Name + " " + a.Address + " ")
	}
	d.recipients = b.String()
	body := p.BodyText()
	if len(body) > maxFTSBody {
		body = body[:maxFTSBody]
	}
	d.body = body
	b.Reset()
	for _, a := range p.Attachments {
		if a.Filename != "" {
			b.WriteString(a.Filename + " ")
		}
	}
	d.attachments = b.String()
	return d
}

// NewMessage describes a message to store.
type NewMessage struct {
	UserID     int64
	MailboxID  int64
	Raw        []byte
	Parsed     *email.Message
	Flags      []string
	ReceivedAt time.Time
	LabelIDs   []int64
	Source     any
}

type preparedMessage struct {
	nm   *NewMessage
	p    *email.Message
	hash string
	fts  ftsDoc
}

func prepare(nm *NewMessage) *preparedMessage {
	p := nm.Parsed
	if p == nil {
		p = email.Parse(nm.Raw)
	}
	sum := sha256.Sum256(nm.Raw)
	if nm.ReceivedAt.IsZero() {
		nm.ReceivedAt = time.Now()
	}
	return &preparedMessage{nm: nm, p: p, hash: hex.EncodeToString(sum[:]), fts: buildFTS(p)}
}

// InsertMessage stores a new message in a mailbox.
func (s *Store) InsertMessage(ctx context.Context, nm NewMessage) (*Message, error) {
	pm := prepare(&nm)
	var m *Message
	err := s.write(ctx, func(tx *sql.Tx, emit emitFunc) error {
		var err error
		m, err = insertMessageTx(ctx, tx, pm)
		if err != nil {
			return err
		}
		emit(Change{Kind: MessagesAdded, UserID: nm.UserID, MailboxID: nm.MailboxID, UIDs: []uint32{m.UID}, Source: nm.Source})
		return nil
	})
	return m, err
}

// ReplaceMessage atomically deletes oldID and stores a new message (used
// for saving drafts).
func (s *Store) ReplaceMessage(ctx context.Context, oldID int64, nm NewMessage) (*Message, error) {
	pm := prepare(&nm)
	var m *Message
	err := s.write(ctx, func(tx *sql.Tx, emit emitFunc) error {
		removed, err := deleteByIDTx(ctx, tx, nm.UserID, []int64{oldID})
		if err != nil {
			return err
		}
		if len(removed) == 0 {
			return ErrNotFound
		}
		emitRemoved(emit, nm.UserID, removed, nil)
		m, err = insertMessageTx(ctx, tx, pm)
		if err != nil {
			return err
		}
		emit(Change{Kind: MessagesAdded, UserID: nm.UserID, MailboxID: nm.MailboxID, UIDs: []uint32{m.UID}, Source: nm.Source})
		return nil
	})
	return m, err
}

func insertMessageTx(ctx context.Context, tx *sql.Tx, pm *preparedMessage) (*Message, error) {
	nm, p := pm.nm, pm.p
	var owner, uidNext int64
	err := tx.QueryRowContext(ctx, `SELECT user_id, uid_next FROM mailboxes WHERE id = ?`, nm.MailboxID).Scan(&owner, &uidNext)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != nm.UserID) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	blobID, err := ensureBlob(ctx, tx, pm)
	if err != nil {
		return nil, err
	}
	threadID, err := resolveThread(ctx, tx, nm.UserID, p)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET uid_next = uid_next + 1 WHERE id = ?`, nm.MailboxID); err != nil {
		return nil, err
	}
	f := flagsFromList(nm.Flags)
	from := p.PrimaryFrom()
	m := &Message{
		UserID: nm.UserID, MailboxID: nm.MailboxID, UID: uint32(uidNext), ThreadID: threadID, BlobID: blobID,
		MessageID: p.MessageID, References: p.References, Subject: p.Subject, From: from,
		To: addrs(p.To), Cc: addrs(p.Cc), Bcc: addrs(p.Bcc), ReplyTo: addrs(p.ReplyTo),
		Date: p.Date, ReceivedAt: nm.ReceivedAt.UTC().Truncate(time.Millisecond), Size: int64(len(nm.Raw)),
		Snippet: p.Snippet(), Attachments: p.Attachments,
		Seen: f.seen, Flagged: f.flagged, Answered: f.answered, Draft: f.draft, Deleted: f.deleted, Keywords: f.keywords,
	}
	if len(p.InReplyTo) > 0 {
		m.InReplyTo = p.InReplyTo[0]
	}
	if m.Attachments == nil {
		m.Attachments = []email.Attachment{}
	}
	if m.References == nil {
		m.References = []string{}
	}
	if m.Keywords == nil {
		m.Keywords = []string{}
	}
	for _, a := range m.Attachments {
		if !a.Inline {
			m.HasAttachments = true
		}
	}
	var sent int64
	if !m.Date.IsZero() {
		sent = m.Date.Unix()
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO messages(user_id, mailbox_id, uid, thread_id, blob_id, message_id, in_reply_to, refs,
		subject, from_name, from_addr, to_json, cc_json, bcc_json, reply_to_json, sent_at, received_at, mailbox_since, size, snippet,
		has_attachments, attachments_json, seen, flagged, answered, draft, deleted, keywords)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.UserID, m.MailboxID, int64(m.UID), m.ThreadID, m.BlobID, m.MessageID, m.InReplyTo, strings.Join(m.References, " "),
		m.Subject, m.From.Name, m.From.Address, jsonString(m.To), jsonString(m.Cc), jsonString(m.Bcc), jsonString(m.ReplyTo),
		sent, m.ReceivedAt.UnixMilli(), time.Now().UnixMilli(), m.Size, m.Snippet,
		boolInt(m.HasAttachments), jsonString(m.Attachments), boolInt(f.seen), boolInt(f.flagged), boolInt(f.answered),
		boolInt(f.draft), boolInt(f.deleted), strings.Join(f.keywords, " "))
	if err != nil {
		return nil, err
	}
	m.ID, _ = res.LastInsertId()
	m.Labels = []int64{}
	for _, lid := range dedupeInt64(nm.LabelIDs) {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO message_labels(message_id, label_id)
			SELECT ?, id FROM labels WHERE id = ? AND user_id = ?`, m.ID, lid, m.UserID)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			m.Labels = append(m.Labels, lid)
		}
	}
	return m, nil
}

func ensureBlob(ctx context.Context, tx *sql.Tx, pm *preparedMessage) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM blobs WHERE hash = ?`, pm.hash).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO blobs(hash, size, data) VALUES (?, ?, ?)`, pm.hash, len(pm.nm.Raw), pm.nm.Raw)
	if err != nil {
		return 0, err
	}
	id, _ = res.LastInsertId()
	d := pm.fts
	_, err = tx.ExecContext(ctx, `INSERT INTO messages_fts(rowid, subject, sender, recipients, body, attachments) VALUES (?, ?, ?, ?, ?, ?)`,
		id, d.subject, d.sender, d.recipients, d.body, d.attachments)
	return id, err
}

const maxThreadRefs = 50

func resolveThread(ctx context.Context, tx *sql.Tx, userID int64, p *email.Message) (int64, error) {
	seen := map[string]bool{}
	var refs []string
	add := func(r string) {
		r = strings.TrimSpace(r)
		if r != "" && !seen[r] {
			seen[r] = true
			refs = append(refs, r)
		}
	}
	references := p.References
	if len(references) > maxThreadRefs {
		references = references[len(references)-maxThreadRefs:]
	}
	for _, r := range references {
		add(r)
	}
	for _, r := range p.InReplyTo {
		add(r)
	}
	add(p.MessageID)

	var found []int64
	if len(refs) > 0 {
		args := append([]any{userID}, stringArgs(refs)...)
		var err error
		found, err = queryInt64s(ctx, tx, `SELECT DISTINCT thread_id FROM thread_refs WHERE user_id = ? AND ref IN (`+placeholders(len(refs))+`)`, args...)
		if err != nil {
			return 0, err
		}
	}
	var tid int64
	if len(found) == 0 {
		res, err := tx.ExecContext(ctx, `INSERT INTO threads DEFAULT VALUES`)
		if err != nil {
			return 0, err
		}
		tid, _ = res.LastInsertId()
	} else {
		sort.Slice(found, func(i, j int) bool { return found[i] < found[j] })
		tid = found[0]
		if others := found[1:]; len(others) > 0 {
			args := append([]any{tid, userID}, int64Args(others)...)
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET thread_id = ? WHERE user_id = ? AND thread_id IN (`+placeholders(len(others))+`)`, args...); err != nil {
				return 0, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE thread_refs SET thread_id = ? WHERE user_id = ? AND thread_id IN (`+placeholders(len(others))+`)`, args...); err != nil {
				return 0, err
			}
		}
	}
	for _, r := range refs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO thread_refs(user_id, ref, thread_id) VALUES (?, ?, ?)
			ON CONFLICT(user_id, ref) DO UPDATE SET thread_id = excluded.thread_id`, userID, r, tid); err != nil {
			return 0, err
		}
	}
	return tid, nil
}

func stringArgs(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

// GetMessage returns one of the user's messages by ID, including labels.
func (s *Store) GetMessage(ctx context.Context, userID, id int64) (*Message, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx, `SELECT `+msgCols+` FROM messages m WHERE m.user_id = ? AND m.id = ?`, userID, id))
	if err != nil {
		return nil, err
	}
	return m, loadLabels(ctx, s.db, []*Message{m})
}

// GetMessages returns the user's messages with the given IDs (missing IDs
// are skipped), ordered by received time.
func (s *Store) GetMessages(ctx context.Context, userID int64, ids []int64) ([]*Message, error) {
	var out []*Message
	for _, part := range chunk(dedupeInt64(ids), 500) {
		args := append([]any{userID}, int64Args(part)...)
		msgs, err := queryMessages(ctx, s.db, `SELECT `+msgCols+` FROM messages m WHERE m.user_id = ? AND m.id IN (`+placeholders(len(part))+`)`, args...)
		if err != nil {
			return nil, err
		}
		out = append(out, msgs...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReceivedAt.Before(out[j].ReceivedAt) })
	return out, loadLabels(ctx, s.db, out)
}

// MessageRaw returns the raw RFC 5322 bytes of one of the user's messages.
func (s *Store) MessageRaw(ctx context.Context, userID, id int64) ([]byte, *Message, error) {
	m, err := s.GetMessage(ctx, userID, id)
	if err != nil {
		return nil, nil, err
	}
	raw, err := s.ReadBlob(ctx, m.BlobID)
	return raw, m, err
}

// ReadBlob returns raw message bytes by blob ID.
func (s *Store) ReadBlob(ctx context.Context, blobID int64) ([]byte, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM blobs WHERE id = ?`, blobID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return data, err
}

// ThreadMessages returns the messages of a conversation ordered by time.
// Messages in Trash and Junk are only included if includeTrash is set.
// Messages flagged \Deleted (pending IMAP expunge) are never included.
func (s *Store) ThreadMessages(ctx context.Context, userID, threadID int64, includeTrash bool) ([]*Message, error) {
	q := `SELECT ` + msgCols + ` FROM messages m WHERE m.user_id = ? AND m.thread_id = ? AND m.deleted = 0`
	args := []any{userID, threadID}
	if !includeTrash {
		q += ` AND ` + notTrashCond
		args = append(args, userID)
	}
	msgs, err := queryMessages(ctx, s.db, q+` ORDER BY m.received_at, m.id`, args...)
	if err != nil {
		return nil, err
	}
	return msgs, loadLabels(ctx, s.db, msgs)
}

// FindByMessageID returns the user's messages with the given Message-ID.
func (s *Store) FindByMessageID(ctx context.Context, userID int64, messageID string) ([]*Message, error) {
	return queryMessages(ctx, s.db, `SELECT `+msgCols+` FROM messages m WHERE m.user_id = ? AND m.message_id = ? ORDER BY m.id`, userID, messageID)
}

// MailboxUIDs returns all UIDs of a mailbox in ascending order.
func (s *Store) MailboxUIDs(ctx context.Context, mailboxID int64) ([]uint32, error) {
	return mailboxUIDsTx(ctx, s.db, mailboxID)
}

// MailboxStatus summarizes a mailbox for IMAP STATUS/SELECT.
type MailboxStatus struct {
	Messages    int
	Unseen      int
	Deleted     int
	Size        int64
	UIDNext     uint32
	UIDValidity uint32
}

// GetMailboxStatus returns counters for one of the user's mailboxes.
func (s *Store) GetMailboxStatus(ctx context.Context, userID, mailboxID int64) (*MailboxStatus, error) {
	var st MailboxStatus
	var un, uv int64
	err := s.db.QueryRowContext(ctx, `SELECT mb.uid_next, mb.uid_validity,
		(SELECT COUNT(*) FROM messages m WHERE m.mailbox_id = mb.id),
		(SELECT COUNT(*) FROM messages m WHERE m.mailbox_id = mb.id AND m.seen = 0),
		(SELECT COUNT(*) FROM messages m WHERE m.mailbox_id = mb.id AND m.deleted = 1),
		(SELECT COALESCE(SUM(size), 0) FROM messages m WHERE m.mailbox_id = mb.id)
		FROM mailboxes mb WHERE mb.user_id = ? AND mb.id = ?`, userID, mailboxID).
		Scan(&un, &uv, &st.Messages, &st.Unseen, &st.Deleted, &st.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	st.UIDNext, st.UIDValidity = uint32(un), uint32(uv)
	return &st, err
}

// MessagesByUID returns messages of a mailbox with the given UIDs, ordered
// by UID. Unknown UIDs are skipped.
func (s *Store) MessagesByUID(ctx context.Context, mailboxID int64, uids []uint32) ([]*Message, error) {
	return messagesByUIDTx(ctx, s.db, mailboxID, uids)
}

func messagesByUIDTx(ctx context.Context, q queryer, mailboxID int64, uids []uint32) ([]*Message, error) {
	var out []*Message
	for _, part := range chunk(uids, 500) {
		args := append([]any{mailboxID}, uint32Args(part)...)
		msgs, err := queryMessages(ctx, q, `SELECT `+msgCols+` FROM messages m WHERE m.mailbox_id = ? AND m.uid IN (`+placeholders(len(part))+`)`, args...)
		if err != nil {
			return nil, err
		}
		out = append(out, msgs...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

// AllMessagesInMailbox returns every message of a mailbox ordered by UID.
func (s *Store) AllMessagesInMailbox(ctx context.Context, mailboxID int64) ([]*Message, error) {
	return queryMessages(ctx, s.db, `SELECT `+msgCols+` FROM messages m WHERE m.mailbox_id = ? ORDER BY m.uid`, mailboxID)
}

func messagesByIDTx(ctx context.Context, q queryer, userID int64, ids []int64) ([]*Message, error) {
	var out []*Message
	for _, part := range chunk(dedupeInt64(ids), 500) {
		args := append([]any{userID}, int64Args(part)...)
		msgs, err := queryMessages(ctx, q, `SELECT `+msgCols+` FROM messages m WHERE m.user_id = ? AND m.id IN (`+placeholders(len(part))+`)`, args...)
		if err != nil {
			return nil, err
		}
		out = append(out, msgs...)
	}
	return out, nil
}

// updateFlagsTx applies a flag operation to msgs and returns, per mailbox,
// the new flags of every message (all) and of changed messages only.
func updateFlagsTx(ctx context.Context, tx *sql.Tx, msgs []*Message, op FlagOp, flags []string) (all, changed map[int64]map[uint32][]string, err error) {
	all = map[int64]map[uint32][]string{}
	changed = map[int64]map[uint32][]string{}
	for _, m := range msgs {
		old := m.flagState()
		nf := old.apply(op, flags)
		if all[m.MailboxID] == nil {
			all[m.MailboxID] = map[uint32][]string{}
		}
		all[m.MailboxID][m.UID] = nf.list()
		if nf.equal(old) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET seen = ?, flagged = ?, answered = ?, draft = ?, deleted = ?, keywords = ? WHERE id = ?`,
			boolInt(nf.seen), boolInt(nf.flagged), boolInt(nf.answered), boolInt(nf.draft), boolInt(nf.deleted),
			strings.Join(nf.keywords, " "), m.ID); err != nil {
			return nil, nil, err
		}
		m.Seen, m.Flagged, m.Answered, m.Draft, m.Deleted, m.Keywords = nf.seen, nf.flagged, nf.answered, nf.draft, nf.deleted, nf.keywords
		if changed[m.MailboxID] == nil {
			changed[m.MailboxID] = map[uint32][]string{}
		}
		changed[m.MailboxID][m.UID] = nf.list()
	}
	return all, changed, nil
}

func emitFlags(emit emitFunc, userID int64, changed map[int64]map[uint32][]string, source any) {
	for mbID, flags := range changed {
		uids := make([]uint32, 0, len(flags))
		for uid := range flags {
			uids = append(uids, uid)
		}
		sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
		emit(Change{Kind: FlagsChanged, UserID: userID, MailboxID: mbID, UIDs: uids, Flags: flags, Source: source})
	}
}

// UpdateFlags modifies the flags of messages in a mailbox by UID and
// returns the resulting flags of every matched message.
func (s *Store) UpdateFlags(ctx context.Context, userID, mailboxID int64, uids []uint32, op FlagOp, flags []string, source any) (map[uint32][]string, error) {
	var result map[uint32][]string
	err := s.write(ctx, func(tx *sql.Tx, emit emitFunc) error {
		if err := checkMailboxOwner(ctx, tx, userID, mailboxID); err != nil {
			return err
		}
		msgs, err := messagesByUIDTx(ctx, tx, mailboxID, uids)
		if err != nil {
			return err
		}
		all, changed, err := updateFlagsTx(ctx, tx, msgs, op, flags)
		if err != nil {
			return err
		}
		result = all[mailboxID]
		if result == nil {
			result = map[uint32][]string{}
		}
		emitFlags(emit, userID, changed, source)
		return nil
	})
	return result, err
}

// UpdateFlagsByID modifies flags of the user's messages by ID.
func (s *Store) UpdateFlagsByID(ctx context.Context, userID int64, ids []int64, op FlagOp, flags []string) error {
	return s.write(ctx, func(tx *sql.Tx, emit emitFunc) error {
		msgs, err := messagesByIDTx(ctx, tx, userID, ids)
		if err != nil {
			return err
		}
		_, changed, err := updateFlagsTx(ctx, tx, msgs, op, flags)
		if err != nil {
			return err
		}
		emitFlags(emit, userID, changed, nil)
		return nil
	})
}

func checkMailboxOwner(ctx context.Context, tx *sql.Tx, userID, mailboxID int64) error {
	var owner int64
	err := tx.QueryRowContext(ctx, `SELECT user_id FROM mailboxes WHERE id = ?`, mailboxID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != userID) {
		return ErrNotFound
	}
	return err
}

func allocUIDs(ctx context.Context, tx *sql.Tx, mailboxID int64, n int) (uint32, error) {
	var next int64
	err := tx.QueryRowContext(ctx, `UPDATE mailboxes SET uid_next = uid_next + ? WHERE id = ? RETURNING uid_next - ?`, n, mailboxID, n).Scan(&next)
	return uint32(next), err
}

// CopyMessages copies messages by UID to another mailbox. It returns the
// source UIDs that were copied and the corresponding new UIDs.
func (s *Store) CopyMessages(ctx context.Context, userID, srcID int64, uids []uint32, dstID int64, source any) (src, dst []uint32, err error) {
	err = s.write(ctx, func(tx *sql.Tx, emit emitFunc) error {
		if err := checkMailboxOwner(ctx, tx, userID, srcID); err != nil {
			return err
		}
		if err := checkMailboxOwner(ctx, tx, userID, dstID); err != nil {
			return err
		}
		msgs, err := messagesByUIDTx(ctx, tx, srcID, uids)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			return nil
		}
		first, err := allocUIDs(ctx, tx, dstID, len(msgs))
		if err != nil {
			return err
		}
		for i, m := range msgs {
			newUID := first + uint32(i)
			res, err := tx.ExecContext(ctx, `INSERT INTO messages(user_id, mailbox_id, uid, thread_id, blob_id, message_id, in_reply_to, refs,
				subject, from_name, from_addr, to_json, cc_json, bcc_json, reply_to_json, sent_at, received_at, mailbox_since, size, snippet,
				has_attachments, attachments_json, seen, flagged, answered, draft, deleted, keywords)
				SELECT user_id, ?, ?, thread_id, blob_id, message_id, in_reply_to, refs,
				subject, from_name, from_addr, to_json, cc_json, bcc_json, reply_to_json, sent_at, received_at, ?, size, snippet,
				has_attachments, attachments_json, seen, flagged, answered, draft, deleted, keywords
				FROM messages WHERE id = ?`, dstID, int64(newUID), time.Now().UnixMilli(), m.ID)
			if err != nil {
				return err
			}
			newID, _ := res.LastInsertId()
			if _, err := tx.ExecContext(ctx, `INSERT INTO message_labels(message_id, label_id) SELECT ?, label_id FROM message_labels WHERE message_id = ?`, newID, m.ID); err != nil {
				return err
			}
			src = append(src, m.UID)
			dst = append(dst, newUID)
		}
		emit(Change{Kind: MessagesAdded, UserID: userID, MailboxID: dstID, UIDs: dst, Source: source})
		return nil
	})
	return src, dst, err
}

func moveTx(ctx context.Context, tx *sql.Tx, emit emitFunc, userID int64, msgs []*Message, dstID int64, source any) (src, dst []uint32, err error) {
	var todo []*Message
	for _, m := range msgs {
		if m.MailboxID != dstID {
			todo = append(todo, m)
		}
	}
	if len(todo) == 0 {
		return nil, nil, nil
	}
	first, err := allocUIDs(ctx, tx, dstID, len(todo))
	if err != nil {
		return nil, nil, err
	}
	removed := map[int64][]uint32{}
	now := time.Now().UnixMilli()
	for i, m := range todo {
		newUID := first + uint32(i)
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET mailbox_id = ?, uid = ?, mailbox_since = ? WHERE id = ?`, dstID, int64(newUID), now, m.ID); err != nil {
			return nil, nil, err
		}
		removed[m.MailboxID] = append(removed[m.MailboxID], m.UID)
		src = append(src, m.UID)
		dst = append(dst, newUID)
	}
	emitRemoved(emit, userID, removed, source)
	emit(Change{Kind: MessagesAdded, UserID: userID, MailboxID: dstID, UIDs: dst, Source: source})
	return src, dst, nil
}

func emitRemoved(emit emitFunc, userID int64, removed map[int64][]uint32, source any) {
	for mbID, uids := range removed {
		sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
		emit(Change{Kind: MessagesRemoved, UserID: userID, MailboxID: mbID, UIDs: uids, Source: source})
	}
}

// MoveMessages moves messages by UID to another mailbox, returning the
// moved source UIDs and their new UIDs.
func (s *Store) MoveMessages(ctx context.Context, userID, srcID int64, uids []uint32, dstID int64, source any) (src, dst []uint32, err error) {
	err = s.write(ctx, func(tx *sql.Tx, emit emitFunc) error {
		if err := checkMailboxOwner(ctx, tx, userID, srcID); err != nil {
			return err
		}
		if err := checkMailboxOwner(ctx, tx, userID, dstID); err != nil {
			return err
		}
		msgs, err := messagesByUIDTx(ctx, tx, srcID, uids)
		if err != nil {
			return err
		}
		src, dst, err = moveTx(ctx, tx, emit, userID, msgs, dstID, source)
		return err
	})
	return src, dst, err
}

// MoveByID moves the user's messages to another mailbox. It returns the
// number of messages moved.
func (s *Store) MoveByID(ctx context.Context, userID int64, ids []int64, dstID int64) (int, error) {
	var n int
	err := s.write(ctx, func(tx *sql.Tx, emit emitFunc) error {
		if err := checkMailboxOwner(ctx, tx, userID, dstID); err != nil {
			return err
		}
		msgs, err := messagesByIDTx(ctx, tx, userID, ids)
		if err != nil {
			return err
		}
		sort.Slice(msgs, func(i, j int) bool { return msgs[i].ID < msgs[j].ID })
		src, _, err := moveTx(ctx, tx, emit, userID, msgs, dstID, nil)
		n = len(src)
		return err
	})
	return n, err
}

// Expunge permanently removes messages flagged \Deleted from a mailbox. If
// uids is non-nil, only those UIDs are considered. It returns the removed
// UIDs in ascending order.
func (s *Store) Expunge(ctx context.Context, userID, mailboxID int64, uids []uint32, source any) ([]uint32, error) {
	var removed []uint32
	err := s.write(ctx, func(tx *sql.Tx, emit emitFunc) error {
		if err := checkMailboxOwner(ctx, tx, userID, mailboxID); err != nil {
			return err
		}
		var msgs []*Message
		var err error
		if uids == nil {
			msgs, err = queryMessages(ctx, tx, `SELECT `+msgCols+` FROM messages m WHERE m.mailbox_id = ? AND m.deleted = 1 ORDER BY m.uid`, mailboxID)
		} else {
			msgs, err = messagesByUIDTx(ctx, tx, mailboxID, uids)
		}
		if err != nil {
			return err
		}
		var ids []int64
		for _, m := range msgs {
			if m.Deleted {
				ids = append(ids, m.ID)
			}
		}
		res, err := deleteByIDTx(ctx, tx, userID, ids)
		if err != nil {
			return err
		}
		removed = res[mailboxID]
		emitRemoved(emit, userID, res, source)
		return nil
	})
	return removed, err
}

func deleteByIDTx(ctx context.Context, tx *sql.Tx, userID int64, ids []int64) (map[int64][]uint32, error) {
	msgs, err := messagesByIDTx(ctx, tx, userID, ids)
	if err != nil {
		return nil, err
	}
	removed := map[int64][]uint32{}
	for _, m := range msgs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, m.ID); err != nil {
			return nil, err
		}
		removed[m.MailboxID] = append(removed[m.MailboxID], m.UID)
	}
	return removed, nil
}

// DeleteByID permanently deletes the user's messages and returns how many
// were removed.
func (s *Store) DeleteByID(ctx context.Context, userID int64, ids []int64) (int, error) {
	var n int
	err := s.write(ctx, func(tx *sql.Tx, emit emitFunc) error {
		removed, err := deleteByIDTx(ctx, tx, userID, ids)
		if err != nil {
			return err
		}
		for _, uids := range removed {
			n += len(uids)
		}
		emitRemoved(emit, userID, removed, nil)
		return nil
	})
	return n, err
}

// EmptyMailbox permanently deletes every message in one of the user's
// mailboxes.
func (s *Store) EmptyMailbox(ctx context.Context, userID, mailboxID int64) (int, error) {
	var n int
	err := s.write(ctx, func(tx *sql.Tx, emit emitFunc) error {
		if err := checkMailboxOwner(ctx, tx, userID, mailboxID); err != nil {
			return err
		}
		ids, err := queryInt64s(ctx, tx, `SELECT id FROM messages WHERE mailbox_id = ?`, mailboxID)
		if err != nil {
			return err
		}
		removed, err := deleteByIDTx(ctx, tx, userID, ids)
		if err != nil {
			return err
		}
		n = len(ids)
		emitRemoved(emit, userID, removed, nil)
		return nil
	})
	return n, err
}

// PurgeOld permanently deletes messages that were put into a mailbox with
// the given special use (e.g. Trash and Junk) before cutoff, for all users.
func (s *Store) PurgeOld(ctx context.Context, special string, cutoff time.Time) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.user_id, m.id FROM messages m JOIN mailboxes mb ON mb.id = m.mailbox_id
		WHERE mb.special_use = ? AND m.mailbox_since < ? LIMIT 5000`, special, cutoff.UnixMilli())
	if err != nil {
		return 0, err
	}
	byUser := map[int64][]int64{}
	for rows.Next() {
		var uid, id int64
		if err := rows.Scan(&uid, &id); err != nil {
			rows.Close()
			return 0, err
		}
		byUser[uid] = append(byUser[uid], id)
	}
	rows.Close()
	total := 0
	for userID, ids := range byUser {
		n, err := s.DeleteByID(ctx, userID, ids)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// SetLabel adds or removes a label on the user's messages.
func (s *Store) SetLabel(ctx context.Context, userID int64, ids []int64, labelID int64, add bool) error {
	return s.write(ctx, func(tx *sql.Tx, emit emitFunc) error {
		var owner int64
		err := tx.QueryRowContext(ctx, `SELECT user_id FROM labels WHERE id = ?`, labelID).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != userID) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		for _, part := range chunk(dedupeInt64(ids), 500) {
			args := append([]any{labelID, userID}, int64Args(part)...)
			if add {
				_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO message_labels(message_id, label_id)
					SELECT id, ? FROM messages WHERE user_id = ? AND id IN (`+placeholders(len(part))+`)`, args...)
			} else {
				_, err = tx.ExecContext(ctx, `DELETE FROM message_labels WHERE label_id = ? AND message_id IN
					(SELECT id FROM messages WHERE user_id = ? AND id IN (`+placeholders(len(part))+`))`, args...)
			}
			if err != nil {
				return err
			}
		}
		emit(Change{Kind: MessagesUpdated, UserID: userID})
		return nil
	})
}
