package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/charles2ke/TitoMail/internal/email"
)

// Label is a user-defined tag that can be applied to messages.
type Label struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Color  string `json:"color"`
	Total  int    `json:"total"`
	Unread int    `json:"unread"`
}

var colorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func validateLabel(name, color string) (string, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return "", "", invalid("label name must be 1-100 characters")
	}
	if strings.ContainsAny(name, "\r\n\t") {
		return "", "", invalid("label name contains invalid characters")
	}
	if color != "" && !colorRe.MatchString(color) {
		return "", "", invalid("label color must be a hex color like #1a73e8")
	}
	return name, strings.ToLower(color), nil
}

// ListLabels returns the user's labels with message counts.
func (s *Store) ListLabels(ctx context.Context, userID int64) ([]*Label, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT l.id, l.name, l.color,
		COUNT(m.id), COALESCE(SUM(CASE WHEN m.seen = 0 THEN 1 ELSE 0 END), 0)
		FROM labels l
		LEFT JOIN message_labels ml ON ml.label_id = l.id
		LEFT JOIN messages m ON m.id = ml.message_id AND m.deleted = 0
		WHERE l.user_id = ? GROUP BY l.id ORDER BY l.name COLLATE NOCASE`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Label{}
	for rows.Next() {
		var l Label
		if err := rows.Scan(&l.ID, &l.Name, &l.Color, &l.Total, &l.Unread); err != nil {
			return nil, err
		}
		out = append(out, &l)
	}
	return out, rows.Err()
}

// CreateLabel creates a label.
func (s *Store) CreateLabel(ctx context.Context, userID int64, name, color string) (*Label, error) {
	name, color, err := validateLabel(name, color)
	if err != nil {
		return nil, err
	}
	var l *Label
	err = s.write(ctx, func(tx *sql.Tx, emit emitFunc) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO labels(user_id, name, color, created_at) VALUES (?, ?, ?, ?)`, userID, name, color, nowUnix())
		if err != nil {
			if isUniqueErr(err) {
				return ErrConflict
			}
			return err
		}
		id, _ := res.LastInsertId()
		l = &Label{ID: id, Name: name, Color: color}
		emit(Change{Kind: MessagesUpdated, UserID: userID})
		return nil
	})
	return l, err
}

// UpdateLabel renames or recolors a label.
func (s *Store) UpdateLabel(ctx context.Context, userID, id int64, name, color string) error {
	name, color, err := validateLabel(name, color)
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx, emit emitFunc) error {
		res, err := tx.ExecContext(ctx, `UPDATE labels SET name = ?, color = ? WHERE id = ? AND user_id = ?`, name, color, id, userID)
		if err != nil {
			if isUniqueErr(err) {
				return ErrConflict
			}
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		emit(Change{Kind: MessagesUpdated, UserID: userID})
		return nil
	})
}

// DeleteLabel deletes a label (messages are kept).
func (s *Store) DeleteLabel(ctx context.Context, userID, id int64) error {
	return s.write(ctx, func(tx *sql.Tx, emit emitFunc) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM labels WHERE id = ? AND user_id = ?`, id, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		emit(Change{Kind: MessagesUpdated, UserID: userID})
		return nil
	})
}

// Contact is an address-book entry.
type Contact struct {
	ID         int64     `json:"id"`
	Email      string    `json:"email"`
	Name       string    `json:"name"`
	Phone      string    `json:"phone"`
	Notes      string    `json:"notes"`
	Saved      bool      `json:"saved"`
	UseCount   int       `json:"use_count"`
	LastUsedAt time.Time `json:"last_used_at"`
}

const contactCols = `id, email, name, phone, notes, saved, use_count, last_used_at`

func scanContact(row interface{ Scan(...any) error }) (*Contact, error) {
	var c Contact
	var last int64
	if err := row.Scan(&c.ID, &c.Email, &c.Name, &c.Phone, &c.Notes, &c.Saved, &c.UseCount, &last); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.LastUsedAt = fromUnix(last)
	return &c, nil
}

// ListContacts returns the user's contacts. If savedOnly is set,
// auto-collected addresses are omitted. q filters by name or address.
func (s *Store) ListContacts(ctx context.Context, userID int64, q string, savedOnly bool, limit int) ([]*Contact, error) {
	if limit <= 0 || limit > 5000 {
		limit = 5000
	}
	query := `SELECT ` + contactCols + ` FROM contacts WHERE user_id = ?`
	args := []any{userID}
	if savedOnly {
		query += ` AND saved = 1`
	}
	if q = strings.TrimSpace(q); q != "" {
		like := "%" + escapeLike(q) + "%"
		query += ` AND (email LIKE ? ESCAPE '\' OR name LIKE ? ESCAPE '\')`
		args = append(args, like, like)
		query += ` ORDER BY (email LIKE ? ESCAPE '\' OR name LIKE ? ESCAPE '\') DESC, saved DESC, use_count DESC, last_used_at DESC LIMIT ?`
		prefix := escapeLike(q) + "%"
		args = append(args, prefix, prefix, limit)
	} else {
		query += ` ORDER BY CASE WHEN name = '' THEN email ELSE name END COLLATE NOCASE LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Contact{}
	for rows.Next() {
		c, err := scanContact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func validateContact(c *Contact) error {
	c.Email = email.NormalizeAddress(c.Email)
	if !email.ValidAddress(c.Email) {
		return invalid("invalid email address")
	}
	c.Name = strings.TrimSpace(c.Name)
	if len(c.Name) > 200 || len(c.Phone) > 50 || len(c.Notes) > 5000 {
		return invalid("contact field too long")
	}
	return nil
}

// SaveContact creates or updates a saved contact (matched by ID if set,
// otherwise by address).
func (s *Store) SaveContact(ctx context.Context, userID int64, c *Contact) (*Contact, error) {
	if err := validateContact(c); err != nil {
		return nil, err
	}
	var out *Contact
	err := s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		if c.ID > 0 {
			res, err := tx.ExecContext(ctx, `UPDATE contacts SET email = ?, name = ?, phone = ?, notes = ?, saved = 1 WHERE id = ? AND user_id = ?`,
				c.Email, c.Name, c.Phone, c.Notes, c.ID, userID)
			if err != nil {
				if isUniqueErr(err) {
					return ErrConflict
				}
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return ErrNotFound
			}
		} else {
			if _, err := tx.ExecContext(ctx, `INSERT INTO contacts(user_id, email, name, phone, notes, saved, created_at)
				VALUES (?, ?, ?, ?, ?, 1, ?)
				ON CONFLICT(user_id, email) DO UPDATE SET name = excluded.name, phone = excluded.phone, notes = excluded.notes, saved = 1`,
				userID, c.Email, c.Name, c.Phone, c.Notes, nowUnix()); err != nil {
				return err
			}
		}
		var err error
		out, err = scanContact(tx.QueryRowContext(ctx, `SELECT `+contactCols+` FROM contacts WHERE user_id = ? AND email = ?`, userID, c.Email))
		return err
	})
	return out, err
}

// DeleteContact deletes a contact.
func (s *Store) DeleteContact(ctx context.Context, userID, id int64) error {
	return s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM contacts WHERE id = ? AND user_id = ?`, id, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// TouchContacts records that the user sent mail to these addresses, adding
// them to the auto-complete list.
func (s *Store) TouchContacts(ctx context.Context, userID int64, list []email.Address) error {
	if len(list) == 0 {
		return nil
	}
	return s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		now := nowUnix()
		for _, a := range list {
			addr := email.NormalizeAddress(a.Address)
			if !email.ValidAddress(addr) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO contacts(user_id, email, name, use_count, last_used_at, created_at)
				VALUES (?, ?, ?, 1, ?, ?)
				ON CONFLICT(user_id, email) DO UPDATE SET use_count = use_count + 1, last_used_at = excluded.last_used_at,
				name = CASE WHEN contacts.name = '' THEN excluded.name ELSE contacts.name END`,
				userID, addr, strings.TrimSpace(a.Name), now, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// IsContact reports whether addr is in the user's contacts.
func (s *Store) IsContact(ctx context.Context, userID int64, addr string) bool {
	var one int
	return s.db.QueryRowContext(ctx, `SELECT 1 FROM contacts WHERE user_id = ? AND email = ?`, userID, email.NormalizeAddress(addr)).Scan(&one) == nil
}

// Session is a web login session.
type Session struct {
	UserID     int64
	CSRFToken  string
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateSession starts a session and returns its secret token and CSRF token.
func (s *Store) CreateSession(ctx context.Context, userID int64, ttl time.Duration, ip, userAgent string) (token, csrf string, err error) {
	token, csrf = randomToken(32), randomToken(24)
	if len(userAgent) > 300 {
		userAgent = userAgent[:300]
	}
	now := time.Now()
	err = s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO sessions(id, user_id, csrf_token, created_at, last_seen_at, expires_at, ip, user_agent)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, hashToken(token), userID, csrf, now.Unix(), now.Unix(), now.Add(ttl).Unix(), ip, userAgent)
		return err
	})
	return token, csrf, err
}

// SessionByToken looks up a valid session. Sessions are extended (sliding
// expiry by ttl) at most once per minute.
func (s *Store) SessionByToken(ctx context.Context, token string, ttl time.Duration) (*Session, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	id := hashToken(token)
	var sess Session
	var exp, seen int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id, csrf_token, expires_at, last_seen_at FROM sessions WHERE id = ?`, id).
		Scan(&sess.UserID, &sess.CSRFToken, &exp, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	now := time.Now()
	if now.Unix() >= exp {
		return nil, ErrNotFound
	}
	sess.ExpiresAt, sess.LastSeenAt = fromUnix(exp), fromUnix(seen)
	if now.Unix()-seen > 60 {
		newExp := now.Add(ttl).Unix()
		_ = s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
			_, err := tx.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ?, expires_at = MAX(expires_at, ?) WHERE id = ?`, now.Unix(), newExp, id)
			return err
		})
	}
	return &sess, nil
}

// DeleteSession ends a session.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	return s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, hashToken(token))
		return err
	})
}

// DeleteUserSessions ends all of a user's sessions except keepToken.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64, keepToken string) error {
	return s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND id != ?`, userID, hashToken(keepToken))
		return err
	})
}

// Upload is a file uploaded for attaching to a message.
type Upload struct {
	ID          string    `json:"id"`
	UserID      int64     `json:"-"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	Data        []byte    `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
}

// CreateUpload stores an uploaded file.
func (s *Store) CreateUpload(ctx context.Context, userID int64, filename, contentType string, data []byte) (*Upload, error) {
	u := &Upload{ID: randomToken(18), UserID: userID, Filename: filename, ContentType: contentType, Size: int64(len(data)), Data: data, CreatedAt: time.Now().UTC()}
	err := s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO uploads(id, user_id, filename, content_type, size, data, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			u.ID, userID, filename, contentType, u.Size, data, u.CreatedAt.Unix())
		return err
	})
	return u, err
}

// GetUpload returns one of the user's uploads including its data.
func (s *Store) GetUpload(ctx context.Context, userID int64, id string) (*Upload, error) {
	var u Upload
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id, user_id, filename, content_type, size, data, created_at FROM uploads WHERE id = ? AND user_id = ?`, id, userID).
		Scan(&u.ID, &u.UserID, &u.Filename, &u.ContentType, &u.Size, &u.Data, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	u.CreatedAt = fromUnix(created)
	return &u, err
}

// DeleteUploads removes uploads by ID.
func (s *Store) DeleteUploads(ctx context.Context, userID int64, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		args := append([]any{userID}, stringArgs(ids)...)
		_, err := tx.ExecContext(ctx, `DELETE FROM uploads WHERE user_id = ? AND id IN (`+placeholders(len(ids))+`)`, args...)
		return err
	})
}

// Cleanup removes expired sessions, stale uploads and old vacation records.
func (s *Store) Cleanup(ctx context.Context) error {
	now := time.Now()
	return s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, now.Unix()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM uploads WHERE created_at < ?`, now.Add(-48*time.Hour).Unix()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM vacation_log WHERE sent_at < ?`, now.Add(-30*24*time.Hour).Unix())
		return err
	})
}
