package store

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/charles2ke/TitoMail/internal/email"
)

// View kinds.
const (
	ViewMailbox = "mailbox"
	ViewStarred = "starred"
	ViewAll     = "all"
	ViewLabel   = "label"
)

// View selects which messages a listing covers.
type View struct {
	Kind      string
	MailboxID int64
	LabelID   int64
}

// ListOptions controls ListConversations.
type ListOptions struct {
	View     View
	Query    string
	Offset   int
	Limit    int
	Threaded bool
}

// Participant is a sender in a conversation.
type Participant struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Unread  bool   `json:"unread"`
}

// Conversation is one row of a message list. In non-threaded mode it
// represents a single message.
type Conversation struct {
	ThreadID       int64           `json:"thread_id"`
	MessageIDs     []int64         `json:"message_ids"`
	LatestID       int64           `json:"latest_id"`
	Subject        string          `json:"subject"`
	Snippet        string          `json:"snippet"`
	Participants   []Participant   `json:"participants"`
	To             []email.Address `json:"to"`
	Count          int             `json:"count"`
	Unread         bool            `json:"unread"`
	Starred        bool            `json:"starred"`
	HasAttachments bool            `json:"has_attachments"`
	HasDraft       bool            `json:"has_draft"`
	Labels         []int64         `json:"labels"`
	Mailboxes      []int64         `json:"mailboxes"`
	Date           time.Time       `json:"date"`
}

// ListResult is a page of conversations.
type ListResult struct {
	Conversations []*Conversation `json:"conversations"`
	Total         int             `json:"total"`
	Offset        int             `json:"offset"`
	Limit         int             `json:"limit"`
}

type scopeSQL struct {
	where     string
	args      []any
	withTrash bool
}

const notTrashCond = `m.mailbox_id NOT IN (SELECT id FROM mailboxes WHERE user_id = ? AND special_use IN ('trash', 'junk'))`

func (s *Store) buildScope(ctx context.Context, userID int64, view View, query string) (*scopeSQL, error) {
	sq, err := parseSearch(query, time.Now())
	if err != nil {
		return nil, err
	}
	conds := []string{"m.user_id = ?", "m.deleted = 0"}
	args := []any{userID}
	sc := &scopeSQL{}
	if sq.hasIn {
		switch sq.in {
		case "anywhere":
			sc.withTrash = true
		default:
			mb, err := s.resolveMailboxRef(ctx, userID, sq.in)
			if err != nil {
				conds = append(conds, "0")
			} else {
				conds = append(conds, "m.mailbox_id = ?")
				args = append(args, mb.ID)
				sc.withTrash = mb.SpecialUse == SpecialTrash || mb.SpecialUse == SpecialJunk
			}
		}
	} else {
		switch view.Kind {
		case ViewMailbox:
			mb, err := s.GetMailbox(ctx, userID, view.MailboxID)
			if err != nil {
				return nil, err
			}
			conds = append(conds, "m.mailbox_id = ?")
			args = append(args, mb.ID)
			sc.withTrash = mb.SpecialUse == SpecialTrash || mb.SpecialUse == SpecialJunk
		case ViewStarred:
			conds = append(conds, "m.flagged = 1", notTrashCond)
			args = append(args, userID)
		case ViewLabel:
			conds = append(conds, "EXISTS (SELECT 1 FROM message_labels ml WHERE ml.message_id = m.id AND ml.label_id = ?)", notTrashCond)
			args = append(args, view.LabelID, userID)
		default:
			conds = append(conds, notTrashCond)
			args = append(args, userID)
		}
	}
	conds = append(conds, sq.conds...)
	args = append(args, sq.args...)
	sc.where = strings.Join(conds, " AND ")
	sc.args = args
	return sc, nil
}

var mailboxAliases = map[string]string{
	"inbox": SpecialInbox, "sent": SpecialSent, "drafts": SpecialDrafts, "draft": SpecialDrafts,
	"archive": SpecialArchive, "junk": SpecialJunk, "spam": SpecialJunk, "trash": SpecialTrash, "bin": SpecialTrash,
}

func (s *Store) resolveMailboxRef(ctx context.Context, userID int64, ref string) (*Mailbox, error) {
	if sp, ok := mailboxAliases[strings.ToLower(ref)]; ok {
		return s.MailboxBySpecial(ctx, userID, sp)
	}
	return s.MailboxByName(ctx, userID, ref)
}

// ListConversations returns a page of conversations (or messages).
func (s *Store) ListConversations(ctx context.Context, userID int64, opts ListOptions) (*ListResult, error) {
	if opts.Limit <= 0 || opts.Limit > 500 {
		opts.Limit = 50
	}
	if opts.Offset < 0 {
		opts.Offset = 0
	}
	sc, err := s.buildScope(ctx, userID, opts.View, opts.Query)
	if err != nil {
		return nil, err
	}
	res := &ListResult{Offset: opts.Offset, Limit: opts.Limit, Conversations: []*Conversation{}}
	group := "m.thread_id"
	if !opts.Threaded {
		group = "m.id"
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT `+group+`) FROM messages m WHERE `+sc.where, sc.args...).Scan(&res.Total); err != nil {
		return nil, wrapSearchErr(err)
	}
	pageArgs := append(append([]any{}, sc.args...), opts.Limit, opts.Offset)
	keys, err := queryInt64s(ctx, s.db, `SELECT `+group+` FROM messages m WHERE `+sc.where+`
		GROUP BY `+group+` ORDER BY MAX(m.received_at) DESC, `+group+` DESC LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		return nil, wrapSearchErr(err)
	}
	if len(keys) == 0 {
		return res, nil
	}
	inArgs := append(append([]any{}, sc.args...), int64Args(keys)...)
	scoped, err := queryMessages(ctx, s.db, `SELECT `+msgCols+` FROM messages m WHERE `+sc.where+` AND `+group+` IN (`+placeholders(len(keys))+`)`, inArgs...)
	if err != nil {
		return nil, wrapSearchErr(err)
	}
	var wide []*Message
	if opts.Threaded {
		q := `SELECT ` + msgCols + ` FROM messages m WHERE m.user_id = ? AND m.deleted = 0 AND m.thread_id IN (` + placeholders(len(keys)) + `)`
		args := append([]any{userID}, int64Args(keys)...)
		if !sc.withTrash {
			q += ` AND ` + notTrashCond
			args = append(args, userID)
		}
		if wide, err = queryMessages(ctx, s.db, q, args...); err != nil {
			return nil, err
		}
	}
	all := map[int64]*Message{}
	for _, m := range append(append([]*Message{}, scoped...), wide...) {
		all[m.ID] = m
	}
	allList := make([]*Message, 0, len(all))
	for _, m := range all {
		allList = append(allList, m)
	}
	if err := loadLabels(ctx, s.db, allList); err != nil {
		return nil, err
	}
	key := func(m *Message) int64 {
		if opts.Threaded {
			return m.ThreadID
		}
		return m.ID
	}
	scopedBy := map[int64][]*Message{}
	for _, m := range scoped {
		scopedBy[key(m)] = append(scopedBy[key(m)], all[m.ID])
	}
	unionBy := map[int64][]*Message{}
	for _, m := range allList {
		unionBy[key(m)] = append(unionBy[key(m)], m)
	}
	for _, k := range keys {
		sm := scopedBy[k]
		if len(sm) == 0 {
			continue
		}
		res.Conversations = append(res.Conversations, buildConversation(sm, unionBy[k]))
	}
	return res, nil
}

func byTime(msgs []*Message) {
	sort.Slice(msgs, func(i, j int) bool {
		if msgs[i].ReceivedAt.Equal(msgs[j].ReceivedAt) {
			return msgs[i].ID < msgs[j].ID
		}
		return msgs[i].ReceivedAt.Before(msgs[j].ReceivedAt)
	})
}

func buildConversation(scoped, union []*Message) *Conversation {
	byTime(scoped)
	byTime(union)
	latest := scoped[len(scoped)-1]
	c := &Conversation{
		ThreadID: latest.ThreadID, LatestID: latest.ID, Subject: latest.Subject, Snippet: latest.Snippet,
		To: latest.To, Date: latest.ReceivedAt, Count: len(union),
		MessageIDs: []int64{}, Participants: []Participant{}, Labels: []int64{}, Mailboxes: []int64{},
	}
	for _, m := range scoped {
		c.MessageIDs = append(c.MessageIDs, m.ID)
	}
	labels := map[int64]bool{}
	mailboxes := map[int64]bool{}
	pidx := map[string]int{}
	for _, m := range union {
		if !m.Seen {
			c.Unread = true
		}
		if m.Flagged {
			c.Starred = true
		}
		if m.HasAttachments {
			c.HasAttachments = true
		}
		if m.Draft {
			c.HasDraft = true
		}
		for _, l := range m.Labels {
			labels[l] = true
		}
		mailboxes[m.MailboxID] = true
		key := strings.ToLower(m.From.Address)
		if m.Draft {
			continue
		}
		if i, ok := pidx[key]; ok {
			if !m.Seen {
				c.Participants[i].Unread = true
			}
			if c.Participants[i].Name == "" {
				c.Participants[i].Name = m.From.Name
			}
			continue
		}
		if len(c.Participants) < 10 {
			pidx[key] = len(c.Participants)
			c.Participants = append(c.Participants, Participant{Name: m.From.Name, Address: m.From.Address, Unread: !m.Seen})
		}
	}
	for l := range labels {
		c.Labels = append(c.Labels, l)
	}
	sort.Slice(c.Labels, func(i, j int) bool { return c.Labels[i] < c.Labels[j] })
	for mb := range mailboxes {
		c.Mailboxes = append(c.Mailboxes, mb)
	}
	sort.Slice(c.Mailboxes, func(i, j int) bool { return c.Mailboxes[i] < c.Mailboxes[j] })
	if c.Subject == "" {
		for _, m := range union {
			if m.Subject != "" {
				c.Subject = m.Subject
				break
			}
		}
	}
	return c
}

// ThreadScopeIDs returns the IDs of a conversation's messages that fall in
// the given view (used to apply list actions to an open conversation).
func (s *Store) ThreadScopeIDs(ctx context.Context, userID, threadID int64, view View) ([]int64, error) {
	sc, err := s.buildScope(ctx, userID, view, "")
	if err != nil {
		return nil, err
	}
	args := append(append([]any{}, sc.args...), threadID)
	return queryInt64s(ctx, s.db, `SELECT m.id FROM messages m WHERE `+sc.where+` AND m.thread_id = ? ORDER BY m.received_at`, args...)
}

// SearchError reports an invalid search query.
type SearchError struct{ Msg string }

func (e *SearchError) Error() string { return e.Msg }

func wrapSearchErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "fts5") {
		return &SearchError{Msg: "invalid search query"}
	}
	return err
}

type searchQuery struct {
	conds []string
	args  []any
	in    string
	hasIn bool
}

// tokenizeSearch splits a query into tokens, keeping quoted sections
// together (e.g. from:"John Smith" or "exact phrase").
func tokenizeSearch(q string) []string {
	var toks []string
	var cur strings.Builder
	inQuote := false
	for _, r := range q {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case unicode.IsSpace(r) && !inQuote:
			if cur.Len() > 0 {
				toks = append(toks, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		toks = append(toks, cur.String())
	}
	return toks
}

func unquote(s string) (string, bool) {
	if len(s) >= 2 && strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		return s[1 : len(s)-1], true
	}
	if strings.HasPrefix(s, `"`) {
		return strings.TrimPrefix(s, `"`), true
	}
	return s, false
}

var relTimeRe = regexp.MustCompile(`^(\d+)([dwmy])$`)
var sizeRe = regexp.MustCompile(`^(?i)(\d+(?:\.\d+)?)([kmg]?)b?$`)

func parseSearchDate(v string) (time.Time, bool) {
	for _, layout := range []string{"2006/01/02", "2006-01-02", "2006/1/2", "2006-1-2", "01/02/2006"} {
		if t, err := time.ParseInLocation(layout, v, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func parseSize(v string) (int64, bool) {
	m := sizeRe.FindStringSubmatch(v)
	if m == nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	switch strings.ToLower(m[2]) {
	case "k":
		f *= 1024
	case "m":
		f *= 1024 * 1024
	case "g":
		f *= 1024 * 1024 * 1024
	}
	return int64(f), true
}

func ftsQuote(term string, prefix bool) string {
	q := `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
	if prefix {
		q += "*"
	}
	return q
}

func hasWordChars(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

const ftsCond = `m.blob_id IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)`

// parseSearch translates a Gmail-style query into SQL conditions.
func parseSearch(q string, now time.Time) (searchQuery, error) {
	var sq searchQuery
	var positive []string
	add := func(neg bool, cond string, args ...any) {
		if neg {
			cond = "NOT (" + cond + ")"
		}
		sq.conds = append(sq.conds, cond)
		sq.args = append(sq.args, args...)
	}
	for _, tok := range tokenizeSearch(q) {
		neg := false
		if len(tok) > 1 && tok[0] == '-' {
			neg = true
			tok = tok[1:]
		}
		key, rawVal, hasColon := strings.Cut(tok, ":")
		key = strings.ToLower(key)
		val, _ := unquote(rawVal)
		like := "%" + escapeLike(val) + "%"
		handled := hasColon && val != ""
		if handled {
			switch key {
			case "from":
				add(neg, `(m.from_addr LIKE ? ESCAPE '\' OR m.from_name LIKE ? ESCAPE '\')`, like, like)
			case "to":
				add(neg, `(m.to_json LIKE ? ESCAPE '\' OR m.cc_json LIKE ? ESCAPE '\' OR m.bcc_json LIKE ? ESCAPE '\')`, like, like, like)
			case "cc":
				add(neg, `m.cc_json LIKE ? ESCAPE '\'`, like)
			case "bcc":
				add(neg, `m.bcc_json LIKE ? ESCAPE '\'`, like)
			case "subject":
				add(neg, `m.subject LIKE ? ESCAPE '\'`, like)
			case "filename":
				add(neg, `m.attachments_json LIKE ? ESCAPE '\'`, like)
			case "has":
				switch strings.ToLower(val) {
				case "attachment", "attachments":
					add(neg, "m.has_attachments = 1")
				case "label", "userlabels":
					add(neg, "EXISTS (SELECT 1 FROM message_labels ml WHERE ml.message_id = m.id)")
				default:
					return sq, &SearchError{Msg: fmt.Sprintf("unknown has: value %q", val)}
				}
			case "is":
				switch strings.ToLower(val) {
				case "unread":
					add(neg, "m.seen = 0")
				case "read":
					add(neg, "m.seen = 1")
				case "starred", "flagged":
					add(neg, "m.flagged = 1")
				case "unstarred":
					add(neg, "m.flagged = 0")
				case "answered", "replied":
					add(neg, "m.answered = 1")
				case "draft":
					add(neg, "m.draft = 1")
				default:
					return sq, &SearchError{Msg: fmt.Sprintf("unknown is: value %q", val)}
				}
			case "in":
				sq.in, sq.hasIn = val, true
			case "label":
				add(neg, `EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id = ml.label_id
					WHERE ml.message_id = m.id AND l.name = ?)`, val)
			case "before", "after", "older", "newer":
				t, ok := parseSearchDate(val)
				if !ok {
					return sq, &SearchError{Msg: fmt.Sprintf("invalid date %q (use YYYY/MM/DD)", val)}
				}
				if key == "before" || key == "older" {
					add(neg, "m.received_at < ?", t.UnixMilli())
				} else {
					add(neg, "m.received_at >= ?", t.UnixMilli())
				}
			case "older_than", "newer_than":
				mm := relTimeRe.FindStringSubmatch(strings.ToLower(val))
				if mm == nil {
					return sq, &SearchError{Msg: fmt.Sprintf("invalid period %q (use e.g. 7d, 2w, 3m, 1y)", val)}
				}
				n, _ := strconv.Atoi(mm[1])
				var cutoff time.Time
				switch mm[2] {
				case "d":
					cutoff = now.AddDate(0, 0, -n)
				case "w":
					cutoff = now.AddDate(0, 0, -7*n)
				case "m":
					cutoff = now.AddDate(0, -n, 0)
				case "y":
					cutoff = now.AddDate(-n, 0, 0)
				}
				if key == "older_than" {
					add(neg, "m.received_at < ?", cutoff.UnixMilli())
				} else {
					add(neg, "m.received_at >= ?", cutoff.UnixMilli())
				}
			case "larger", "smaller", "size":
				n, ok := parseSize(val)
				if !ok {
					return sq, &SearchError{Msg: fmt.Sprintf("invalid size %q (use e.g. 5M or 100K)", val)}
				}
				if key == "smaller" {
					add(neg, "m.size < ?", n)
				} else {
					add(neg, "m.size > ?", n)
				}
			default:
				handled = false
			}
		}
		if handled {
			continue
		}
		term, quoted := unquote(tok)
		if !hasWordChars(term) {
			continue
		}
		match := ftsQuote(term, !quoted)
		if neg {
			add(false, `m.blob_id NOT IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)`, match)
		} else {
			positive = append(positive, match)
		}
	}
	if len(positive) > 0 {
		add(false, ftsCond, strings.Join(positive, " "))
	}
	return sq, nil
}
