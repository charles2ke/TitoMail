package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const schemaV1 = `
CREATE TABLE domains (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE COLLATE NOCASE,
	dkim_selector TEXT NOT NULL DEFAULT '',
	dkim_private_key TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);

CREATE TABLE users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	domain_id INTEGER NOT NULL REFERENCES domains(id) ON DELETE RESTRICT,
	email TEXT NOT NULL UNIQUE COLLATE NOCASE,
	display_name TEXT NOT NULL DEFAULT '',
	password_hash TEXT NOT NULL,
	is_admin INTEGER NOT NULL DEFAULT 0,
	disabled INTEGER NOT NULL DEFAULT 0,
	quota_bytes INTEGER NOT NULL DEFAULT 0,
	settings TEXT NOT NULL DEFAULT '{}',
	created_at INTEGER NOT NULL
);

CREATE TABLE aliases (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	address TEXT NOT NULL UNIQUE COLLATE NOCASE,
	domain_id INTEGER NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL
);
CREATE INDEX aliases_user ON aliases(user_id);

CREATE TABLE mailboxes (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	special_use TEXT NOT NULL DEFAULT '',
	uid_validity INTEGER NOT NULL,
	uid_next INTEGER NOT NULL DEFAULT 1,
	subscribed INTEGER NOT NULL DEFAULT 1,
	created_at INTEGER NOT NULL,
	UNIQUE (user_id, name)
);

CREATE TABLE blobs (
	id INTEGER PRIMARY KEY,
	hash TEXT NOT NULL UNIQUE,
	size INTEGER NOT NULL,
	data BLOB NOT NULL
);

CREATE TABLE threads (id INTEGER PRIMARY KEY AUTOINCREMENT);

CREATE TABLE messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	mailbox_id INTEGER NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
	uid INTEGER NOT NULL,
	thread_id INTEGER NOT NULL,
	blob_id INTEGER NOT NULL REFERENCES blobs(id),
	message_id TEXT NOT NULL DEFAULT '',
	in_reply_to TEXT NOT NULL DEFAULT '',
	refs TEXT NOT NULL DEFAULT '',
	subject TEXT NOT NULL DEFAULT '',
	from_name TEXT NOT NULL DEFAULT '',
	from_addr TEXT NOT NULL DEFAULT '',
	to_json TEXT NOT NULL DEFAULT '[]',
	cc_json TEXT NOT NULL DEFAULT '[]',
	bcc_json TEXT NOT NULL DEFAULT '[]',
	reply_to_json TEXT NOT NULL DEFAULT '[]',
	sent_at INTEGER NOT NULL DEFAULT 0,
	received_at INTEGER NOT NULL,
	-- When the message entered its current mailbox; Trash and Junk expire by this.
	mailbox_since INTEGER NOT NULL DEFAULT 0,
	size INTEGER NOT NULL,
	snippet TEXT NOT NULL DEFAULT '',
	has_attachments INTEGER NOT NULL DEFAULT 0,
	attachments_json TEXT NOT NULL DEFAULT '[]',
	seen INTEGER NOT NULL DEFAULT 0,
	flagged INTEGER NOT NULL DEFAULT 0,
	answered INTEGER NOT NULL DEFAULT 0,
	draft INTEGER NOT NULL DEFAULT 0,
	deleted INTEGER NOT NULL DEFAULT 0,
	keywords TEXT NOT NULL DEFAULT '',
	UNIQUE (mailbox_id, uid)
);
CREATE INDEX messages_mailbox_date ON messages(mailbox_id, received_at);
CREATE INDEX messages_thread ON messages(user_id, thread_id);
CREATE INDEX messages_msgid ON messages(user_id, message_id);
CREATE INDEX messages_blob ON messages(blob_id);
CREATE INDEX messages_user_date ON messages(user_id, received_at);

CREATE TABLE thread_refs (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	ref TEXT NOT NULL,
	thread_id INTEGER NOT NULL,
	PRIMARY KEY (user_id, ref)
) WITHOUT ROWID;
CREATE INDEX thread_refs_thread ON thread_refs(user_id, thread_id);

-- Full-text index keyed by blob id (copies of a message share a blob).
CREATE VIRTUAL TABLE messages_fts USING fts5(
	subject, sender, recipients, body, attachments,
	content='', contentless_delete=1,
	tokenize='unicode61 remove_diacritics 2'
);

CREATE TRIGGER messages_after_delete AFTER DELETE ON messages BEGIN
	DELETE FROM blobs WHERE id = old.blob_id
		AND NOT EXISTS (SELECT 1 FROM messages WHERE blob_id = old.blob_id);
END;

CREATE TRIGGER blobs_after_delete AFTER DELETE ON blobs BEGIN
	DELETE FROM messages_fts WHERE rowid = old.id;
END;

CREATE TABLE labels (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL COLLATE NOCASE,
	color TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	UNIQUE (user_id, name)
);

CREATE TABLE message_labels (
	message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	label_id INTEGER NOT NULL REFERENCES labels(id) ON DELETE CASCADE,
	PRIMARY KEY (message_id, label_id)
) WITHOUT ROWID;
CREATE INDEX message_labels_label ON message_labels(label_id);

CREATE TABLE contacts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	email TEXT NOT NULL COLLATE NOCASE,
	name TEXT NOT NULL DEFAULT '',
	phone TEXT NOT NULL DEFAULT '',
	notes TEXT NOT NULL DEFAULT '',
	saved INTEGER NOT NULL DEFAULT 0,
	use_count INTEGER NOT NULL DEFAULT 0,
	last_used_at INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	UNIQUE (user_id, email)
);

CREATE TABLE sessions (
	id TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	csrf_token TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	ip TEXT NOT NULL DEFAULT '',
	user_agent TEXT NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user ON sessions(user_id);

CREATE TABLE uploads (
	id TEXT PRIMARY KEY,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	filename TEXT NOT NULL,
	content_type TEXT NOT NULL,
	size INTEGER NOT NULL,
	data BLOB NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE filters (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL DEFAULT '',
	criteria TEXT NOT NULL,
	actions TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 1,
	position INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);

CREATE TABLE sender_prefs (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	address TEXT NOT NULL COLLATE NOCASE,
	kind TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (user_id, address)
) WITHOUT ROWID;

CREATE TABLE vacation_log (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	sender TEXT NOT NULL COLLATE NOCASE,
	sent_at INTEGER NOT NULL,
	PRIMARY KEY (user_id, sender)
) WITHOUT ROWID;

CREATE TABLE queue (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
	mail_from TEXT NOT NULL,
	message_id TEXT NOT NULL DEFAULT '',
	subject TEXT NOT NULL DEFAULT '',
	sent_copy_id INTEGER NOT NULL DEFAULT 0,
	data BLOB NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE queue_rcpts (
	id INTEGER PRIMARY KEY,
	queue_id INTEGER NOT NULL REFERENCES queue(id) ON DELETE CASCADE,
	rcpt TEXT NOT NULL,
	domain TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending',
	attempts INTEGER NOT NULL DEFAULT 0,
	next_attempt_at INTEGER NOT NULL,
	last_attempt_at INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX queue_rcpts_queue ON queue_rcpts(queue_id);
CREATE INDEX queue_rcpts_due ON queue_rcpts(status, next_attempt_at);
`

const schemaV2 = `
ALTER TABLE queue ADD COLUMN scheduled INTEGER NOT NULL DEFAULT 0;
-- Older queues did not record this decision. Delays longer than the maximum
-- undo window identify scheduled web sends that have not yet been claimed.
UPDATE queue SET scheduled = 1
WHERE user_id IS NOT NULL AND sent_copy_id > 0
	AND EXISTS (SELECT 1 FROM queue_rcpts r WHERE r.queue_id = queue.id AND r.next_attempt_at - queue.created_at > 30)
	AND NOT EXISTS (SELECT 1 FROM queue_rcpts r WHERE r.queue_id = queue.id
		AND (r.status != 'pending' OR r.attempts != 0 OR r.last_attempt_at != 0));
`

var migrations = []string{schemaV1, schemaV2}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return err
	}
	var version int
	err := s.db.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'schema_version'`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		version = 0
	} else if err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this TitoMail build supports (%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES ('schema_version', ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	_, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO meta(key, value) VALUES ('uidvalidity', ?)`, time.Now().Unix())
	return err
}

// GetMeta returns a value from the key/value meta table.
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetMeta stores a value in the key/value meta table.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	return s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
		return err
	})
}

func nextUIDValidity(ctx context.Context, tx *sql.Tx) (uint32, error) {
	var v int64
	err := tx.QueryRowContext(ctx, `UPDATE meta SET value = CAST(value AS INTEGER) + 1
		WHERE key = 'uidvalidity' RETURNING CAST(value AS INTEGER)`).Scan(&v)
	if err != nil {
		return 0, err
	}
	return uint32(v), nil
}
