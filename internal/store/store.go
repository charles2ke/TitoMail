// Package store persists all TitoMail data in a single SQLite database.
//
// All write transactions are serialized through a mutex. Change
// notifications are dispatched synchronously while that mutex is held, so
// listeners (such as the IMAP server's mailbox trackers) observe changes in
// exactly the order they were committed. Listeners must therefore be fast and
// must never call back into the store's write methods.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a record would violate a uniqueness rule.
var ErrConflict = errors.New("already exists")

// ValidationError describes invalid user input.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}

// ChangeKind identifies the type of a Change.
type ChangeKind int

const (
	// MessagesAdded: UIDs were appended to MailboxID (ascending order).
	MessagesAdded ChangeKind = iota + 1
	// MessagesRemoved: UIDs were removed from MailboxID.
	MessagesRemoved
	// FlagsChanged: Flags holds the complete new flag list for each UID.
	FlagsChanged
	// MessagesUpdated: web-only metadata (labels, threads) changed.
	MessagesUpdated
	// MailboxesChanged: mailboxes were created, renamed, deleted or (un)subscribed.
	MailboxesChanged
)

// Change describes a committed modification of a user's mail data.
type Change struct {
	Kind      ChangeKind
	UserID    int64
	MailboxID int64
	UIDs      []uint32
	Flags     map[uint32][]string
	// Source identifies the originator (e.g. an IMAP session tracker) so it
	// can skip echoing its own updates. May be nil.
	Source any
}

// Store is the TitoMail database.
type Store struct {
	db  *sql.DB
	wmu sync.Mutex

	lmu       sync.RWMutex
	listeners map[int]func(Change)
	nextID    int
}

// Open opens (and creates or migrates) the database at path.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(15000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetConnMaxIdleTime(5 * time.Minute)
	s := &Store{db: db, listeners: map[int]func(Change){}}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying database for read-only diagnostics.
func (s *Store) DB() *sql.DB { return s.db }

// Subscribe registers a change listener. It returns a function that removes
// the listener.
func (s *Store) Subscribe(fn func(Change)) func() {
	s.lmu.Lock()
	id := s.nextID
	s.nextID++
	s.listeners[id] = fn
	s.lmu.Unlock()
	return func() {
		s.lmu.Lock()
		delete(s.listeners, id)
		s.lmu.Unlock()
	}
}

func (s *Store) dispatch(c Change) {
	s.lmu.RLock()
	fns := make([]func(Change), 0, len(s.listeners))
	for _, fn := range s.listeners {
		fns = append(fns, fn)
	}
	s.lmu.RUnlock()
	for _, fn := range fns {
		fn(c)
	}
}

// Locked runs fn while holding the write lock. fn may read from the store
// but must not write. It is used to take consistent snapshots that line up
// exactly with the change notification stream.
func (s *Store) Locked(fn func() error) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return fn()
}

type emitFunc func(Change)

// write runs fn in a serialized write transaction and dispatches the
// collected changes after a successful commit.
func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx, emit emitFunc) error) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	var changes []Change
	if err := fn(tx, func(c Change) { changes = append(changes, c) }); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, c := range changes {
		s.dispatch(c)
	}
	return nil
}

func nowUnix() int64 { return time.Now().Unix() }

func fromUnix(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

func fromUnixMilli(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// placeholders returns "?,?,?" for n parameters.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, 0, n*2)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}

func int64Args(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func uint32Args(ids []uint32) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = int64(id)
	}
	return out
}

// queryer is satisfied by *sql.DB and *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
