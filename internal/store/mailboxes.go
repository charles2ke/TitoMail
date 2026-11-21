package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"unicode"
)

// Special-use mailbox identifiers (RFC 6154 without the backslash).
const (
	SpecialInbox   = "inbox"
	SpecialDrafts  = "drafts"
	SpecialSent    = "sent"
	SpecialArchive = "archive"
	SpecialJunk    = "junk"
	SpecialTrash   = "trash"
)

// MailboxDelimiter separates hierarchy levels in mailbox names.
const MailboxDelimiter = '/'

var defaultMailboxes = []struct{ name, special string }{
	{"INBOX", SpecialInbox},
	{"Drafts", SpecialDrafts},
	{"Sent", SpecialSent},
	{"Archive", SpecialArchive},
	{"Junk", SpecialJunk},
	{"Trash", SpecialTrash},
}

var specialOrder = map[string]int{
	SpecialInbox: 0, SpecialDrafts: 1, SpecialSent: 2, SpecialArchive: 3, SpecialJunk: 4, SpecialTrash: 5,
}

// Mailbox is a folder of messages.
type Mailbox struct {
	ID          int64  `json:"id"`
	UserID      int64  `json:"-"`
	Name        string `json:"name"`
	SpecialUse  string `json:"special_use,omitempty"`
	UIDValidity uint32 `json:"-"`
	UIDNext     uint32 `json:"-"`
	Subscribed  bool   `json:"subscribed"`
	Total       int    `json:"total"`
	Unread      int    `json:"unread"`
	Size        int64  `json:"size"`
}

// ValidateMailboxName normalizes and checks a mailbox name.
func ValidateMailboxName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", invalid("folder name must not be empty")
	}
	if len(name) > 200 {
		return "", invalid("folder name is too long")
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") {
		return "", invalid("folder name has an empty path segment")
	}
	for _, r := range name {
		if r == '*' || r == '%' || unicode.IsControl(r) {
			return "", invalid("folder name contains an invalid character")
		}
	}
	for _, seg := range strings.Split(name, "/") {
		if strings.TrimSpace(seg) != seg {
			return "", invalid("folder name segments must not start or end with spaces")
		}
	}
	if strings.EqualFold(name, "INBOX") {
		return "INBOX", nil
	}
	return name, nil
}

func createDefaultMailboxes(ctx context.Context, tx *sql.Tx, userID int64) error {
	for _, d := range defaultMailboxes {
		if _, err := createMailboxTx(ctx, tx, userID, d.name, d.special); err != nil {
			return err
		}
	}
	return nil
}

func createMailboxTx(ctx context.Context, tx *sql.Tx, userID int64, name, special string) (*Mailbox, error) {
	uv, err := nextUIDValidity(ctx, tx)
	if err != nil {
		return nil, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO mailboxes(user_id, name, special_use, uid_validity, created_at)
		VALUES (?, ?, ?, ?, ?)`, userID, name, special, uv, nowUnix())
	if err != nil {
		if isUniqueErr(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Mailbox{ID: id, UserID: userID, Name: name, SpecialUse: special, UIDValidity: uv, UIDNext: 1, Subscribed: true}, nil
}

const mailboxCols = `mb.id, mb.user_id, mb.name, mb.special_use, mb.uid_validity, mb.uid_next, mb.subscribed`

func scanMailbox(row interface{ Scan(...any) error }, extra ...any) (*Mailbox, error) {
	var mb Mailbox
	var uv, un int64
	dest := append([]any{&mb.ID, &mb.UserID, &mb.Name, &mb.SpecialUse, &uv, &un, &mb.Subscribed}, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	mb.UIDValidity, mb.UIDNext = uint32(uv), uint32(un)
	return &mb, nil
}

// SortMailboxes orders special folders first, then the rest by name.
func SortMailboxes(list []*Mailbox) {
	sort.SliceStable(list, func(i, j int) bool {
		oi, iok := specialOrder[list[i].SpecialUse]
		oj, jok := specialOrder[list[j].SpecialUse]
		switch {
		case iok && jok:
			return oi < oj
		case iok != jok:
			return iok
		}
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})
}

// ListMailboxes returns the user's mailboxes with message counts.
func (s *Store) ListMailboxes(ctx context.Context, userID int64) ([]*Mailbox, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+mailboxCols+`,
		COALESCE(SUM(CASE WHEN m.deleted = 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN m.seen = 0 AND m.deleted = 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(m.size), 0)
		FROM mailboxes mb LEFT JOIN messages m ON m.mailbox_id = mb.id
		WHERE mb.user_id = ? GROUP BY mb.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Mailbox
	for rows.Next() {
		var total, unread int
		var size int64
		mb, err := scanMailbox(rows, &total, &unread, &size)
		if err != nil {
			return nil, err
		}
		mb.Total, mb.Unread, mb.Size = total, unread, size
		out = append(out, mb)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	SortMailboxes(out)
	return out, nil
}

// GetMailbox returns one of the user's mailboxes by ID.
func (s *Store) GetMailbox(ctx context.Context, userID, id int64) (*Mailbox, error) {
	return scanMailbox(s.db.QueryRowContext(ctx, `SELECT `+mailboxCols+` FROM mailboxes mb WHERE mb.user_id = ? AND mb.id = ?`, userID, id))
}

// MailboxByName returns a mailbox by name ("INBOX" is case-insensitive).
func (s *Store) MailboxByName(ctx context.Context, userID int64, name string) (*Mailbox, error) {
	if strings.EqualFold(name, "INBOX") {
		name = "INBOX"
	}
	return scanMailbox(s.db.QueryRowContext(ctx, `SELECT `+mailboxCols+` FROM mailboxes mb WHERE mb.user_id = ? AND mb.name = ?`, userID, name))
}

// MailboxBySpecial returns the mailbox with the given special use.
func (s *Store) MailboxBySpecial(ctx context.Context, userID int64, special string) (*Mailbox, error) {
	return scanMailbox(s.db.QueryRowContext(ctx, `SELECT `+mailboxCols+` FROM mailboxes mb
		WHERE mb.user_id = ? AND mb.special_use = ? ORDER BY mb.id LIMIT 1`, userID, special))
}

func mailboxBySpecialTx(ctx context.Context, tx *sql.Tx, userID int64, special string) (*Mailbox, error) {
	return scanMailbox(tx.QueryRowContext(ctx, `SELECT `+mailboxCols+` FROM mailboxes mb
		WHERE mb.user_id = ? AND mb.special_use = ? ORDER BY mb.id LIMIT 1`, userID, special))
}

// CreateMailbox creates a mailbox (and any missing parents).
func (s *Store) CreateMailbox(ctx context.Context, userID int64, name string) (*Mailbox, error) {
	name, err := ValidateMailboxName(name)
	if err != nil {
		return nil, err
	}
	if name == "INBOX" {
		return nil, ErrConflict
	}
	var mb *Mailbox
	err = s.write(ctx, func(tx *sql.Tx, emit emitFunc) error {
		parts := strings.Split(name, "/")
		for i := 1; i < len(parts); i++ {
			parent := strings.Join(parts[:i], "/")
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailboxes WHERE user_id = ? AND name = ?`, userID, parent).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				if _, err := createMailboxTx(ctx, tx, userID, parent, ""); err != nil {
					return err
				}
			}
		}
		var err error
		mb, err = createMailboxTx(ctx, tx, userID, name, "")
		if err != nil {
			return err
		}
		emit(Change{Kind: MailboxesChanged, UserID: userID})
		return nil
	})
	return mb, err
}

// RenameMailbox renames a mailbox and its children.
func (s *Store) RenameMailbox(ctx context.Context, userID, id int64, newName string) error {
	newName, err := ValidateMailboxName(newName)
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx, emit emitFunc) error {
		mb, err := scanMailbox(tx.QueryRowContext(ctx, `SELECT `+mailboxCols+` FROM mailboxes mb WHERE mb.user_id = ? AND mb.id = ?`, userID, id))
		if err != nil {
			return err
		}
		if mb.SpecialUse == SpecialInbox || newName == "INBOX" {
			return invalid("the inbox cannot be renamed")
		}
		if mb.Name == newName {
			return nil
		}
		if strings.HasPrefix(newName, mb.Name+"/") {
			return invalid("a folder cannot be moved into itself")
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailboxes WHERE user_id = ? AND (name = ? OR name LIKE ? ESCAPE '\')`,
			userID, newName, escapeLike(newName)+"/%").Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET name = ? || substr(name, ?) WHERE user_id = ? AND name LIKE ? ESCAPE '\'`,
			newName, len(mb.Name)+1, userID, escapeLike(mb.Name)+"/%"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE mailboxes SET name = ? WHERE id = ?`, newName, id); err != nil {
			return err
		}
		parts := strings.Split(newName, "/")
		for i := 1; i < len(parts); i++ {
			parent := strings.Join(parts[:i], "/")
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailboxes WHERE user_id = ? AND name = ?`, userID, parent).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				if _, err := createMailboxTx(ctx, tx, userID, parent, ""); err != nil {
					return err
				}
			}
		}
		emit(Change{Kind: MailboxesChanged, UserID: userID})
		return nil
	})
}

// HasChildren reports whether the mailbox has sub-folders.
func (s *Store) HasChildren(ctx context.Context, userID int64, name string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailboxes WHERE user_id = ? AND name LIKE ? ESCAPE '\'`,
		userID, escapeLike(name)+"/%").Scan(&n)
	return n > 0, err
}

// DeleteMailbox deletes a mailbox, its messages and (if recursive) its
// sub-folders. Special-use mailboxes cannot be deleted.
func (s *Store) DeleteMailbox(ctx context.Context, userID, id int64, recursive bool) error {
	return s.write(ctx, func(tx *sql.Tx, emit emitFunc) error {
		mb, err := scanMailbox(tx.QueryRowContext(ctx, `SELECT `+mailboxCols+` FROM mailboxes mb WHERE mb.user_id = ? AND mb.id = ?`, userID, id))
		if err != nil {
			return err
		}
		if mb.SpecialUse != "" {
			return invalid("system folders cannot be deleted")
		}
		ids := []int64{id}
		children, err := queryInt64s(ctx, tx, `SELECT id FROM mailboxes WHERE user_id = ? AND name LIKE ? ESCAPE '\'`,
			userID, escapeLike(mb.Name)+"/%")
		if err != nil {
			return err
		}
		if len(children) > 0 {
			if !recursive {
				return invalid("folder has sub-folders")
			}
			var special int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mailboxes WHERE id IN (`+placeholders(len(children))+`) AND special_use != ''`,
				int64Args(children)...).Scan(&special); err != nil {
				return err
			}
			if special > 0 {
				return invalid("folder contains a system folder")
			}
			ids = append(ids, children...)
		}
		for _, mbID := range ids {
			uids, err := mailboxUIDsTx(ctx, tx, mbID)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE mailbox_id = ?`, mbID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM mailboxes WHERE id = ?`, mbID); err != nil {
				return err
			}
			if len(uids) > 0 {
				emit(Change{Kind: MessagesRemoved, UserID: userID, MailboxID: mbID, UIDs: uids})
			}
		}
		emit(Change{Kind: MailboxesChanged, UserID: userID})
		return nil
	})
}

// SetSubscribed changes a mailbox's IMAP subscription state.
func (s *Store) SetSubscribed(ctx context.Context, userID, id int64, subscribed bool) error {
	return s.write(ctx, func(tx *sql.Tx, emit emitFunc) error {
		res, err := tx.ExecContext(ctx, `UPDATE mailboxes SET subscribed = ? WHERE user_id = ? AND id = ?`, boolInt(subscribed), userID, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		emit(Change{Kind: MailboxesChanged, UserID: userID})
		return nil
	})
}

func mailboxUIDsTx(ctx context.Context, q queryer, mailboxID int64) ([]uint32, error) {
	rows, err := q.QueryContext(ctx, `SELECT uid FROM messages WHERE mailbox_id = ? ORDER BY uid`, mailboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uint32
	for rows.Next() {
		var uid int64
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		out = append(out, uint32(uid))
	}
	return out, rows.Err()
}
