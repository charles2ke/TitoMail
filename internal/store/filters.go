package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/charles2ke/TitoMail/internal/email"
)

// FilterCriteria selects incoming messages. All non-empty fields must
// match.
type FilterCriteria struct {
	From          string `json:"from"`
	To            string `json:"to"`
	Subject       string `json:"subject"`
	HasWords      string `json:"has_words"`
	HasAttachment bool   `json:"has_attachment"`
}

// Empty reports whether no criterion is set.
func (c FilterCriteria) Empty() bool {
	return strings.TrimSpace(c.From) == "" && strings.TrimSpace(c.To) == "" && strings.TrimSpace(c.Subject) == "" &&
		strings.TrimSpace(c.HasWords) == "" && !c.HasAttachment
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(strings.TrimSpace(sub)))
}

func anyAddrContains(list []email.Address, sub string) bool {
	for _, a := range list {
		if containsFold(a.Address, sub) || containsFold(a.Name, sub) {
			return true
		}
	}
	return false
}

// Matches reports whether a parsed message satisfies the criteria.
func (c FilterCriteria) Matches(p *email.Message) bool {
	if c.Empty() {
		return false
	}
	if strings.TrimSpace(c.From) != "" && !anyAddrContains(p.From, c.From) {
		return false
	}
	if strings.TrimSpace(c.To) != "" && !anyAddrContains(append(append([]email.Address{}, p.To...), p.Cc...), c.To) {
		return false
	}
	if strings.TrimSpace(c.Subject) != "" && !containsFold(p.Subject, c.Subject) {
		return false
	}
	if words := strings.Fields(c.HasWords); len(words) > 0 {
		text := strings.ToLower(p.Subject + "\n" + p.BodyText())
		for _, w := range words {
			if !strings.Contains(text, strings.ToLower(w)) {
				return false
			}
		}
	}
	if c.HasAttachment {
		found := false
		for _, a := range p.Attachments {
			if !a.Inline {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// FilterActions are applied to matching messages.
type FilterActions struct {
	SkipInbox bool  `json:"skip_inbox"`
	MarkRead  bool  `json:"mark_read"`
	Star      bool  `json:"star"`
	LabelID   int64 `json:"label_id"`
	MoveTo    int64 `json:"move_to"`
	Trash     bool  `json:"trash"`
	NeverSpam bool  `json:"never_spam"`
}

// Filter is a user's incoming-mail rule.
type Filter struct {
	ID       int64          `json:"id"`
	Name     string         `json:"name"`
	Criteria FilterCriteria `json:"criteria"`
	Actions  FilterActions  `json:"actions"`
	Enabled  bool           `json:"enabled"`
}

// ListFilters returns the user's filters in evaluation order.
func (s *Store) ListFilters(ctx context.Context, userID int64) ([]*Filter, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, criteria, actions, enabled FROM filters WHERE user_id = ? ORDER BY position, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Filter{}
	for rows.Next() {
		var f Filter
		var crit, act string
		if err := rows.Scan(&f.ID, &f.Name, &crit, &act, &f.Enabled); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(crit), &f.Criteria)
		_ = json.Unmarshal([]byte(act), &f.Actions)
		out = append(out, &f)
	}
	return out, rows.Err()
}

func (s *Store) validateFilter(ctx context.Context, userID int64, f *Filter) error {
	f.Name = strings.TrimSpace(f.Name)
	if len(f.Name) > 100 {
		return invalid("filter name too long")
	}
	if f.Criteria.Empty() {
		return invalid("a filter needs at least one criterion")
	}
	if f.Actions.LabelID > 0 {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM labels WHERE id = ? AND user_id = ?`, f.Actions.LabelID, userID).Scan(&n); err != nil || n == 0 {
			return invalid("unknown label")
		}
	}
	if f.Actions.MoveTo > 0 {
		if _, err := s.GetMailbox(ctx, userID, f.Actions.MoveTo); err != nil {
			return invalid("unknown folder")
		}
	}
	return nil
}

// SaveFilter creates (ID 0) or updates a filter.
func (s *Store) SaveFilter(ctx context.Context, userID int64, f *Filter) (*Filter, error) {
	if err := s.validateFilter(ctx, userID, f); err != nil {
		return nil, err
	}
	crit, act := jsonString(f.Criteria), jsonString(f.Actions)
	err := s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		if f.ID > 0 {
			res, err := tx.ExecContext(ctx, `UPDATE filters SET name = ?, criteria = ?, actions = ?, enabled = ? WHERE id = ? AND user_id = ?`,
				f.Name, crit, act, boolInt(f.Enabled), f.ID, userID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return ErrNotFound
			}
			return nil
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO filters(user_id, name, criteria, actions, enabled, position, created_at)
			VALUES (?, ?, ?, ?, ?, (SELECT COALESCE(MAX(position), 0) + 1 FROM filters WHERE user_id = ?), ?)`,
			userID, f.Name, crit, act, boolInt(f.Enabled), userID, nowUnix())
		if err != nil {
			return err
		}
		f.ID, _ = res.LastInsertId()
		return nil
	})
	return f, err
}

// DeleteFilter removes a filter.
func (s *Store) DeleteFilter(ctx context.Context, userID, id int64) error {
	return s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM filters WHERE id = ? AND user_id = ?`, id, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// Sender preference kinds.
const (
	SenderBlocked = "blocked"
	SenderTrusted = "trusted"
)

// SenderPref is a per-sender preference ("@domain" applies to a domain).
type SenderPref struct {
	Address   string    `json:"address"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
}

func normalizeSenderPattern(addr string) (string, error) {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if strings.HasPrefix(addr, "@") {
		if !ValidDomain(addr[1:]) {
			return "", invalid("invalid domain")
		}
		return addr, nil
	}
	addr = email.NormalizeAddress(addr)
	if !email.ValidAddress(addr) {
		return "", invalid("invalid email address")
	}
	return addr, nil
}

// SetSenderPref blocks or trusts a sender (address or "@domain").
func (s *Store) SetSenderPref(ctx context.Context, userID int64, addr, kind string) error {
	if kind != SenderBlocked && kind != SenderTrusted {
		return invalid("invalid sender preference")
	}
	addr, err := normalizeSenderPattern(addr)
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO sender_prefs(user_id, address, kind, created_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(user_id, address) DO UPDATE SET kind = excluded.kind`, userID, addr, kind, nowUnix())
		return err
	})
}

// DeleteSenderPref removes a sender preference.
func (s *Store) DeleteSenderPref(ctx context.Context, userID int64, addr string) error {
	addr = strings.ToLower(strings.TrimSpace(addr))
	return s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM sender_prefs WHERE user_id = ? AND address = ?`, userID, addr)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// GetSenderPref returns the preference kind for an address ("" if none),
// checking the exact address before its domain.
func (s *Store) GetSenderPref(ctx context.Context, userID int64, addr string) string {
	addr = email.NormalizeAddress(addr)
	var kind string
	err := s.db.QueryRowContext(ctx, `SELECT kind FROM sender_prefs WHERE user_id = ? AND address IN (?, ?)
		ORDER BY CASE WHEN address LIKE '@%' THEN 1 ELSE 0 END LIMIT 1`, userID, addr, "@"+email.DomainOf(addr)).Scan(&kind)
	if err != nil {
		return ""
	}
	return kind
}

// ListSenderPrefs returns the user's sender preferences.
func (s *Store) ListSenderPrefs(ctx context.Context, userID int64) ([]*SenderPref, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT address, kind, created_at FROM sender_prefs WHERE user_id = ? ORDER BY kind, address`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*SenderPref{}
	for rows.Next() {
		var p SenderPref
		var created int64
		if err := rows.Scan(&p.Address, &p.Kind, &created); err != nil {
			return nil, err
		}
		p.CreatedAt = fromUnix(created)
		out = append(out, &p)
	}
	return out, rows.Err()
}

// ClaimVacationReply reports whether an auto-reply should be sent to sender
// (none sent within interval) and, if so, records it.
func (s *Store) ClaimVacationReply(ctx context.Context, userID int64, sender string, interval time.Duration) (bool, error) {
	sender = email.NormalizeAddress(sender)
	ok := false
	err := s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		now := time.Now()
		var last int64
		err := tx.QueryRowContext(ctx, `SELECT sent_at FROM vacation_log WHERE user_id = ? AND sender = ?`, userID, sender).Scan(&last)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && now.Sub(time.Unix(last, 0)) < interval {
			return nil
		}
		ok = true
		_, err = tx.ExecContext(ctx, `INSERT INTO vacation_log(user_id, sender, sent_at) VALUES (?, ?, ?)
			ON CONFLICT(user_id, sender) DO UPDATE SET sent_at = excluded.sent_at`, userID, sender, now.Unix())
		return err
	})
	return ok, err
}

// ResetVacationLog forgets which senders received an auto-reply.
func (s *Store) ResetVacationLog(ctx context.Context, userID int64) error {
	return s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM vacation_log WHERE user_id = ?`, userID)
		return err
	})
}
