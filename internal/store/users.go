package store

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/charles2ke/TitoMail/internal/email"
)

// PasswordCost is the bcrypt cost used for new password hashes. Tests may
// lower it to speed things up.
var PasswordCost = 12

// MinPasswordLength is the minimum accepted password length.
const MinPasswordLength = 8

// DKIMSelector is the selector used for generated DKIM keys.
const DKIMSelector = "titomail"

// ErrInvalidCredentials is returned by Authenticate on any failure.
var ErrInvalidCredentials = errors.New("invalid email or password")

// Domain is a mail domain hosted by this server.
type Domain struct {
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	DKIMSelector   string    `json:"dkim_selector"`
	DKIMPrivateKey string    `json:"-"`
	CreatedAt      time.Time `json:"created_at"`
	Users          int       `json:"users"`
}

// DKIMSigner parses the domain's private key.
func (d *Domain) DKIMSigner() (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(d.DKIMPrivateKey))
	if block == nil {
		return nil, errors.New("no DKIM key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		if k, err2 := x509.ParsePKCS1PrivateKey(block.Bytes); err2 == nil {
			return k, nil
		}
		return nil, err
	}
	rk, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("DKIM key is not RSA")
	}
	return rk, nil
}

// DKIMPublicKey returns the base64 public key for the DNS TXT record.
func (d *Domain) DKIMPublicKey() (string, error) {
	k, err := d.DKIMSigner()
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		return "", err
	}
	return base64Std(der), nil
}

// User is a mailbox owner.
type User struct {
	ID           int64     `json:"id"`
	DomainID     int64     `json:"domain_id"`
	Email        string    `json:"email"`
	DisplayName  string    `json:"display_name"`
	PasswordHash string    `json:"-"`
	IsAdmin      bool      `json:"is_admin"`
	Disabled     bool      `json:"disabled"`
	QuotaBytes   int64     `json:"quota_bytes"`
	Settings     Settings  `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	UsedBytes    int64     `json:"used_bytes"`
}

// Address returns the user's address with display name.
func (u *User) Address() email.Address {
	return email.Address{Name: u.DisplayName, Address: u.Email}
}

// Settings are per-user preferences for the web client.
type Settings struct {
	Signature            string   `json:"signature"`
	ConversationView     bool     `json:"conversation_view"`
	ReadingPane          string   `json:"reading_pane"`
	Theme                string   `json:"theme"`
	Density              string   `json:"density"`
	PageSize             int      `json:"page_size"`
	ExternalImages       string   `json:"external_images"`
	UndoSendSeconds      int      `json:"undo_send_seconds"`
	DesktopNotifications bool     `json:"desktop_notifications"`
	Vacation             Vacation `json:"vacation"`
}

// Vacation is the out-of-office auto-reply configuration.
type Vacation struct {
	Enabled      bool   `json:"enabled"`
	Subject      string `json:"subject"`
	Body         string `json:"body"`
	Start        string `json:"start"`
	End          string `json:"end"`
	ContactsOnly bool   `json:"contacts_only"`
}

// Active reports whether the vacation responder applies at time t.
func (v Vacation) Active(t time.Time) bool {
	if !v.Enabled {
		return false
	}
	day := t.UTC().Format("2006-01-02")
	if v.Start != "" && day < v.Start {
		return false
	}
	if v.End != "" && day > v.End {
		return false
	}
	return true
}

// DefaultSettings returns the settings for new users.
func DefaultSettings() Settings {
	return Settings{
		ConversationView: true,
		ReadingPane:      "right",
		Theme:            "system",
		Density:          "comfortable",
		PageSize:         50,
		ExternalImages:   "ask",
		UndoSendSeconds:  5,
	}
}

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Normalize clamps settings to valid values.
func (s *Settings) Normalize() {
	d := DefaultSettings()
	if s.ReadingPane != "right" && s.ReadingPane != "bottom" && s.ReadingPane != "none" {
		s.ReadingPane = d.ReadingPane
	}
	if s.Theme != "system" && s.Theme != "light" && s.Theme != "dark" {
		s.Theme = d.Theme
	}
	if s.Density != "comfortable" && s.Density != "compact" {
		s.Density = d.Density
	}
	if s.PageSize < 10 || s.PageSize > 200 {
		s.PageSize = d.PageSize
	}
	if s.ExternalImages != "ask" && s.ExternalImages != "always" {
		s.ExternalImages = d.ExternalImages
	}
	if s.UndoSendSeconds < 0 {
		s.UndoSendSeconds = 0
	}
	if s.UndoSendSeconds > 30 {
		s.UndoSendSeconds = 30
	}
	if len(s.Signature) > 10000 {
		s.Signature = s.Signature[:10000]
	}
	s.Signature = email.SanitizeComposedHTML(s.Signature)
	if !dateRe.MatchString(s.Vacation.Start) {
		s.Vacation.Start = ""
	}
	if !dateRe.MatchString(s.Vacation.End) {
		s.Vacation.End = ""
	}
	if len(s.Vacation.Subject) > 200 {
		s.Vacation.Subject = s.Vacation.Subject[:200]
	}
	if len(s.Vacation.Body) > 10000 {
		s.Vacation.Body = s.Vacation.Body[:10000]
	}
}

func parseSettings(raw string) Settings {
	s := DefaultSettings()
	_ = json.Unmarshal([]byte(raw), &s)
	s.Normalize()
	return s
}

var domainRe = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// ValidDomain reports whether name is a syntactically valid domain name
// (single-label names like "localhost" are permitted for testing).
func ValidDomain(name string) bool {
	if name == "localhost" {
		return true
	}
	return len(name) <= 253 && domainRe.MatchString(name)
}

func generateDKIMKey() (string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// CreateDomain adds a hosted domain and generates its DKIM key.
func (s *Store) CreateDomain(ctx context.Context, name string) (*Domain, error) {
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	if !ValidDomain(name) {
		return nil, invalid("invalid domain name %q", name)
	}
	key, err := generateDKIMKey()
	if err != nil {
		return nil, err
	}
	var d *Domain
	err = s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		now := nowUnix()
		res, err := tx.ExecContext(ctx, `INSERT INTO domains(name, dkim_selector, dkim_private_key, created_at) VALUES (?, ?, ?, ?)`,
			name, DKIMSelector, key, now)
		if err != nil {
			if isUniqueErr(err) {
				return ErrConflict
			}
			return err
		}
		id, _ := res.LastInsertId()
		d = &Domain{ID: id, Name: name, DKIMSelector: DKIMSelector, DKIMPrivateKey: key, CreatedAt: fromUnix(now)}
		return nil
	})
	return d, err
}

const domainCols = `d.id, d.name, d.dkim_selector, d.dkim_private_key, d.created_at,
	(SELECT COUNT(*) FROM users u WHERE u.domain_id = d.id)`

func scanDomain(row interface{ Scan(...any) error }) (*Domain, error) {
	var d Domain
	var created int64
	if err := row.Scan(&d.ID, &d.Name, &d.DKIMSelector, &d.DKIMPrivateKey, &created, &d.Users); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	d.CreatedAt = fromUnix(created)
	return &d, nil
}

// ListDomains returns all hosted domains.
func (s *Store) ListDomains(ctx context.Context) ([]*Domain, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+domainCols+` FROM domains d ORDER BY d.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Domain
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDomain returns a domain by ID.
func (s *Store) GetDomain(ctx context.Context, id int64) (*Domain, error) {
	return scanDomain(s.db.QueryRowContext(ctx, `SELECT `+domainCols+` FROM domains d WHERE d.id = ?`, id))
}

// DomainByName returns a hosted domain by name.
func (s *Store) DomainByName(ctx context.Context, name string) (*Domain, error) {
	return scanDomain(s.db.QueryRowContext(ctx, `SELECT `+domainCols+` FROM domains d WHERE d.name = ?`,
		strings.TrimSuffix(strings.TrimSpace(name), ".")))
}

// IsLocalDomain reports whether mail for name is handled by this server.
func (s *Store) IsLocalDomain(ctx context.Context, name string) bool {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM domains WHERE name = ?`, strings.TrimSuffix(name, ".")).Scan(&one)
	return err == nil
}

// DeleteDomain removes a domain. Domains that still have users cannot be
// deleted.
func (s *Store) DeleteDomain(ctx context.Context, id int64) error {
	return s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE domain_id = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return invalid("domain still has %d user(s)", n)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM domains WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// HashPassword hashes a password with bcrypt.
func HashPassword(pw string) (string, error) {
	if len(pw) < MinPasswordLength {
		return "", invalid("password must be at least %d characters", MinPasswordLength)
	}
	if len(pw) > 72 {
		return "", invalid("password must be at most 72 bytes")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), PasswordCost)
	return string(h), err
}

// NewUser describes a user to create.
type NewUser struct {
	Email       string
	Password    string
	DisplayName string
	IsAdmin     bool
	QuotaBytes  int64
}

// CreateUser creates a user with the default set of mailboxes.
func (s *Store) CreateUser(ctx context.Context, nu NewUser) (*User, error) {
	addr := email.NormalizeAddress(nu.Email)
	if !email.ValidAddress(addr) {
		return nil, invalid("invalid email address %q", nu.Email)
	}
	if strings.Contains(email.LocalPart(addr), "+") {
		return nil, invalid("email address must not contain '+'")
	}
	hash, err := HashPassword(nu.Password)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(nu.DisplayName)
	if len(name) > 200 {
		return nil, invalid("display name too long")
	}
	var u *User
	err = s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		var domainID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM domains WHERE name = ?`, email.DomainOf(addr)).Scan(&domainID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return invalid("domain %q is not hosted on this server; add it first", email.DomainOf(addr))
			}
			return err
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM aliases WHERE address = ?`, addr).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			return ErrConflict
		}
		now := nowUnix()
		settings, _ := json.Marshal(DefaultSettings())
		res, err := tx.ExecContext(ctx, `INSERT INTO users(domain_id, email, display_name, password_hash, is_admin, quota_bytes, settings, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, domainID, addr, name, hash, boolInt(nu.IsAdmin), nu.QuotaBytes, string(settings), now)
		if err != nil {
			if isUniqueErr(err) {
				return ErrConflict
			}
			return err
		}
		id, _ := res.LastInsertId()
		if err := createDefaultMailboxes(ctx, tx, id); err != nil {
			return err
		}
		u = &User{ID: id, DomainID: domainID, Email: addr, DisplayName: name, PasswordHash: hash,
			IsAdmin: nu.IsAdmin, QuotaBytes: nu.QuotaBytes, Settings: DefaultSettings(), CreatedAt: fromUnix(now)}
		return nil
	})
	return u, err
}

const userCols = `u.id, u.domain_id, u.email, u.display_name, u.password_hash, u.is_admin, u.disabled,
	u.quota_bytes, u.settings, u.created_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var settings string
	var created int64
	if err := row.Scan(&u.ID, &u.DomainID, &u.Email, &u.DisplayName, &u.PasswordHash, &u.IsAdmin, &u.Disabled,
		&u.QuotaBytes, &settings, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.Settings = parseSettings(settings)
	u.CreatedAt = fromUnix(created)
	return &u, nil
}

// GetUser returns a user by ID.
func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users u WHERE u.id = ?`, id))
}

// UserByEmail returns a user by primary email address.
func (s *Store) UserByEmail(ctx context.Context, addr string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users u WHERE u.email = ?`, email.NormalizeAddress(addr)))
}

// ListUsers returns all users with their storage usage.
func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+`,
		(SELECT COALESCE(SUM(m.size), 0) FROM messages m WHERE m.user_id = u.id)
		FROM users u ORDER BY u.email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		var u User
		var settings string
		var created int64
		if err := rows.Scan(&u.ID, &u.DomainID, &u.Email, &u.DisplayName, &u.PasswordHash, &u.IsAdmin, &u.Disabled,
			&u.QuotaBytes, &settings, &created, &u.UsedBytes); err != nil {
			return nil, err
		}
		u.Settings = parseSettings(settings)
		u.CreatedAt = fromUnix(created)
		out = append(out, &u)
	}
	return out, rows.Err()
}

// CountUsers returns the number of users.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// UserUpdate holds optional changes to a user.
type UserUpdate struct {
	DisplayName *string
	IsAdmin     *bool
	Disabled    *bool
	QuotaBytes  *int64
}

// UpdateUser applies changes to a user.
func (s *Store) UpdateUser(ctx context.Context, id int64, up UserUpdate) error {
	return s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		var sets []string
		var args []any
		if up.DisplayName != nil {
			n := strings.TrimSpace(*up.DisplayName)
			if len(n) > 200 {
				return invalid("display name too long")
			}
			sets = append(sets, "display_name = ?")
			args = append(args, n)
		}
		if up.IsAdmin != nil {
			sets = append(sets, "is_admin = ?")
			args = append(args, boolInt(*up.IsAdmin))
		}
		if up.Disabled != nil {
			sets = append(sets, "disabled = ?")
			args = append(args, boolInt(*up.Disabled))
		}
		if up.QuotaBytes != nil {
			if *up.QuotaBytes < 0 {
				return invalid("quota must not be negative")
			}
			sets = append(sets, "quota_bytes = ?")
			args = append(args, *up.QuotaBytes)
		}
		if len(sets) == 0 {
			return nil
		}
		args = append(args, id)
		res, err := tx.ExecContext(ctx, `UPDATE users SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if up.Disabled != nil && *up.Disabled {
			_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id)
		}
		return err
	})
}

// SetPassword changes a user's password.
func (s *Store) SetPassword(ctx context.Context, id int64, pw string) error {
	hash, err := HashPassword(pw)
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		res, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// UpdateSettings replaces a user's settings.
func (s *Store) UpdateSettings(ctx context.Context, id int64, st Settings) (Settings, error) {
	st.Normalize()
	raw, err := json.Marshal(st)
	if err != nil {
		return st, err
	}
	err = s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		res, err := tx.ExecContext(ctx, `UPDATE users SET settings = ? WHERE id = ?`, string(raw), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
	return st, err
}

// DeleteUser removes a user and all of their data.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	return s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE user_id = ?`, id); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

var (
	dummyOnce sync.Once
	dummyHash []byte
)

// dummyPasswordHash is compared against when a user does not exist, so that
// authentication takes the same time either way. It uses PasswordCost, as
// the time a check takes doubles with every step of the cost.
func dummyPasswordHash() []byte {
	dummyOnce.Do(func() {
		dummyHash, _ = bcrypt.GenerateFromPassword([]byte("titomail-timing-equalizer"), PasswordCost)
	})
	return dummyHash
}

// Authenticate verifies credentials. Disabled users cannot log in.
func (s *Store) Authenticate(ctx context.Context, addr, password string) (*User, error) {
	u, err := s.UserByEmail(ctx, addr)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			_ = bcrypt.CompareHashAndPassword(dummyPasswordHash(), []byte(password))
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return nil, ErrInvalidCredentials
	}
	if u.Disabled {
		return nil, ErrInvalidCredentials
	}
	return u, nil
}

// UsedBytes returns the total size of a user's stored messages.
func (s *Store) UsedBytes(ctx context.Context, userID int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM messages WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

// ErrQuotaExceeded is returned when a user's storage quota would be exceeded.
var ErrQuotaExceeded = errors.New("mailbox storage quota exceeded")

// CheckQuota returns ErrQuotaExceeded if adding n bytes would exceed the
// user's quota.
func (s *Store) CheckQuota(ctx context.Context, u *User, n int64) error {
	if u.QuotaBytes <= 0 {
		return nil
	}
	used, err := s.UsedBytes(ctx, u.ID)
	if err != nil {
		return err
	}
	if used+n > u.QuotaBytes {
		return ErrQuotaExceeded
	}
	return nil
}

// Alias forwards an address (or, for "@domain", every unknown address in a
// domain) to a user.
type Alias struct {
	ID        int64     `json:"id"`
	Address   string    `json:"address"`
	UserID    int64     `json:"user_id"`
	UserEmail string    `json:"user_email"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateAlias creates an alias. address may be "@domain" for a catch-all.
func (s *Store) CreateAlias(ctx context.Context, address string, userID int64) (*Alias, error) {
	addr := strings.ToLower(strings.TrimSpace(address))
	catchAll := strings.HasPrefix(addr, "@")
	if catchAll {
		if !ValidDomain(addr[1:]) {
			return nil, invalid("invalid catch-all domain %q", address)
		}
	} else if !email.ValidAddress(addr) {
		return nil, invalid("invalid alias address %q", address)
	}
	var a *Alias
	err := s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		var domainID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM domains WHERE name = ?`, email.DomainOf(addr)).Scan(&domainID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return invalid("domain %q is not hosted on this server", email.DomainOf(addr))
			}
			return err
		}
		var userEmail string
		if err := tx.QueryRowContext(ctx, `SELECT email FROM users WHERE id = ?`, userID).Scan(&userEmail); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return invalid("user not found")
			}
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE email = ?`, addr).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrConflict
		}
		now := nowUnix()
		res, err := tx.ExecContext(ctx, `INSERT INTO aliases(address, domain_id, user_id, created_at) VALUES (?, ?, ?, ?)`,
			addr, domainID, userID, now)
		if err != nil {
			if isUniqueErr(err) {
				return ErrConflict
			}
			return err
		}
		id, _ := res.LastInsertId()
		a = &Alias{ID: id, Address: addr, UserID: userID, UserEmail: userEmail, CreatedAt: fromUnix(now)}
		return nil
	})
	return a, err
}

// ListAliases returns aliases, optionally restricted to one user (userID>0).
func (s *Store) ListAliases(ctx context.Context, userID int64) ([]*Alias, error) {
	q := `SELECT a.id, a.address, a.user_id, u.email, a.created_at FROM aliases a JOIN users u ON u.id = a.user_id`
	var args []any
	if userID > 0 {
		q += ` WHERE a.user_id = ?`
		args = append(args, userID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY a.address`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Alias
	for rows.Next() {
		var a Alias
		var created int64
		if err := rows.Scan(&a.ID, &a.Address, &a.UserID, &a.UserEmail, &created); err != nil {
			return nil, err
		}
		a.CreatedAt = fromUnix(created)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// DeleteAlias removes an alias.
func (s *Store) DeleteAlias(ctx context.Context, id int64) error {
	return s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM aliases WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ResolveAddress finds the user that receives mail for addr, considering
// primary addresses, aliases, plus-addressing (user+tag@domain) and
// catch-all aliases. It returns ErrNotFound if nobody does.
func (s *Store) ResolveAddress(ctx context.Context, addr string) (*User, error) {
	addr = email.NormalizeAddress(addr)
	local, domain := email.LocalPart(addr), email.DomainOf(addr)
	candidates := []string{addr}
	if i := strings.IndexByte(local, '+'); i > 0 {
		candidates = append(candidates, local[:i]+"@"+domain)
	}
	for _, c := range candidates {
		if u, err := s.UserByEmail(ctx, c); err == nil {
			return u, nil
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		if u, err := s.userByAlias(ctx, c); err == nil {
			return u, nil
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	return s.userByAlias(ctx, "@"+domain)
}

func (s *Store) userByAlias(ctx context.Context, addr string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM aliases a JOIN users u ON u.id = a.user_id
		WHERE a.address = ?`, addr))
}

// SenderAddresses returns the addresses a user may send as: their primary
// address followed by their (non catch-all) aliases.
func (s *Store) SenderAddresses(ctx context.Context, u *User) ([]string, error) {
	out := []string{u.Email}
	rows, err := s.db.QueryContext(ctx, `SELECT address FROM aliases WHERE user_id = ? AND address NOT LIKE '@%' ORDER BY address`, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CanSendAs reports whether the user may use addr as the envelope or header
// sender.
func (s *Store) CanSendAs(ctx context.Context, u *User, addr string) bool {
	addr = email.NormalizeAddress(addr)
	local, domain := email.LocalPart(addr), email.DomainOf(addr)
	if i := strings.IndexByte(local, '+'); i > 0 {
		addr = local[:i] + "@" + domain
	}
	allowed, err := s.SenderAddresses(ctx, u)
	if err != nil {
		return false
	}
	for _, a := range allowed {
		if strings.EqualFold(a, addr) {
			return true
		}
	}
	return false
}

// DirectoryEntry is a local user visible to other users for autocomplete.
type DirectoryEntry struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// SearchDirectory finds local users matching q.
func (s *Store) SearchDirectory(ctx context.Context, q string, limit int) ([]DirectoryEntry, error) {
	like := "%" + escapeLike(q) + "%"
	rows, err := s.db.QueryContext(ctx, `SELECT display_name, email FROM users
		WHERE disabled = 0 AND (email LIKE ? ESCAPE '\' OR display_name LIKE ? ESCAPE '\')
		ORDER BY email LIMIT ?`, like, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DirectoryEntry
	for rows.Next() {
		var e DirectoryEntry
		if err := rows.Scan(&e.Name, &e.Address); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
