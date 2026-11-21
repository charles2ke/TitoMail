package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charles2ke/TitoMail/internal/email"
)

// Queue recipient states.
const (
	RcptPending = "pending"
	RcptSent    = "sent"
	RcptFailed  = "failed"
)

// QueueEntry is an outgoing message awaiting delivery.
type QueueEntry struct {
	ID         int64        `json:"id"`
	UserID     int64        `json:"user_id"`
	MailFrom   string       `json:"mail_from"`
	MessageID  string       `json:"message_id"`
	Subject    string       `json:"subject"`
	SentCopyID int64        `json:"sent_copy_id"`
	Scheduled  bool         `json:"scheduled"`
	Size       int64        `json:"size"`
	Data       []byte       `json:"-"`
	CreatedAt  time.Time    `json:"created_at"`
	Rcpts      []*QueueRcpt `json:"recipients"`
}

// QueueRcpt is the delivery state of one recipient of a QueueEntry.
type QueueRcpt struct {
	ID            int64     `json:"id"`
	QueueID       int64     `json:"-"`
	Rcpt          string    `json:"rcpt"`
	Domain        string    `json:"domain"`
	Status        string    `json:"status"`
	Attempts      int       `json:"attempts"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	LastAttemptAt time.Time `json:"last_attempt_at"`
	LastError     string    `json:"last_error"`
}

// QueueBatch is a set of recipients at one domain for one queue entry,
// claimed for a delivery attempt.
type QueueBatch struct {
	Entry  *QueueEntry
	Domain string
	Rcpts  []*QueueRcpt
}

// Enqueue adds a message for delivery to rcpts, not before notBefore.
func (s *Store) Enqueue(ctx context.Context, e *QueueEntry, rcpts []string, notBefore time.Time) (int64, error) {
	if len(rcpts) == 0 {
		return 0, invalid("no recipients")
	}
	err := s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		var userID any
		if e.UserID > 0 {
			userID = e.UserID
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO queue(user_id, mail_from, message_id, subject, sent_copy_id, data, created_at, scheduled)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, userID, e.MailFrom, e.MessageID, e.Subject, e.SentCopyID, e.Data, nowUnix(), e.Scheduled)
		if err != nil {
			return err
		}
		e.ID, _ = res.LastInsertId()
		seen := map[string]bool{}
		for _, r := range rcpts {
			r = email.NormalizeAddress(r)
			if seen[r] {
				continue
			}
			seen[r] = true
			if _, err := tx.ExecContext(ctx, `INSERT INTO queue_rcpts(queue_id, rcpt, domain, next_attempt_at) VALUES (?, ?, ?, ?)`,
				e.ID, r, email.DomainOf(r), notBefore.Unix()); err != nil {
				return err
			}
		}
		return nil
	})
	return e.ID, err
}

const rcptCols = `id, queue_id, rcpt, domain, status, attempts, next_attempt_at, last_attempt_at, last_error`

func scanRcpt(row interface{ Scan(...any) error }) (*QueueRcpt, error) {
	var r QueueRcpt
	var next, last int64
	if err := row.Scan(&r.ID, &r.QueueID, &r.Rcpt, &r.Domain, &r.Status, &r.Attempts, &next, &last, &r.LastError); err != nil {
		return nil, err
	}
	r.NextAttemptAt, r.LastAttemptAt = fromUnix(next), fromUnix(last)
	return &r, nil
}

func getQueueEntryTx(ctx context.Context, q queryer, id int64, withData bool) (*QueueEntry, error) {
	var e QueueEntry
	var userID sql.NullInt64
	var created int64
	dataCol := "x''"
	if withData {
		dataCol = "data"
	}
	err := q.QueryRowContext(ctx, `SELECT id, user_id, mail_from, message_id, subject, sent_copy_id, length(data), `+dataCol+`, created_at, scheduled
		FROM queue WHERE id = ?`, id).Scan(&e.ID, &userID, &e.MailFrom, &e.MessageID, &e.Subject, &e.SentCopyID, &e.Size, &e.Data, &created, &e.Scheduled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	e.UserID = userID.Int64
	e.CreatedAt = fromUnix(created)
	rows, err := q.QueryContext(ctx, `SELECT `+rcptCols+` FROM queue_rcpts WHERE queue_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanRcpt(rows)
		if err != nil {
			return nil, err
		}
		e.Rcpts = append(e.Rcpts, r)
	}
	return &e, rows.Err()
}

// GetQueueEntry returns a queue entry including its message data.
func (s *Store) GetQueueEntry(ctx context.Context, id int64) (*QueueEntry, error) {
	return getQueueEntryTx(ctx, s.db, id, true)
}

// MaxBatchRcpts is the maximum number of recipients in one QueueBatch
// (RFC 5321 requires servers to accept at least 100).
const MaxBatchRcpts = 100

// ClaimDue returns up to limit batches of due pending recipients, grouped
// by entry and domain. The claimed recipients are leased (their next
// attempt is pushed back) so a crash doesn't lose them and they aren't
// claimed twice.
func (s *Store) ClaimDue(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]*QueueBatch, error) {
	var batches []*QueueBatch
	err := s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+rcptCols+` FROM queue_rcpts WHERE status = 'pending' AND next_attempt_at <= ?
			ORDER BY next_attempt_at, id LIMIT ?`, now.Unix(), limit*MaxBatchRcpts)
		if err != nil {
			return err
		}
		var rcpts []*QueueRcpt
		for rows.Next() {
			r, err := scanRcpt(rows)
			if err != nil {
				rows.Close()
				return err
			}
			rcpts = append(rcpts, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		entries := map[int64]*QueueEntry{}
		groups := map[[2]string]*QueueBatch{}
		var order [][2]string
		for _, r := range rcpts {
			key := [2]string{strconv.FormatInt(r.QueueID, 10), strings.ToLower(r.Domain)}
			b, ok := groups[key]
			if ok && len(b.Rcpts) >= MaxBatchRcpts {
				continue
			}
			if !ok {
				if len(order) >= limit {
					continue
				}
				e, ok := entries[r.QueueID]
				if !ok {
					e, err = getQueueEntryTx(ctx, tx, r.QueueID, true)
					if err != nil {
						return err
					}
					entries[r.QueueID] = e
				}
				b = &QueueBatch{Entry: e, Domain: strings.ToLower(r.Domain)}
				groups[key] = b
				order = append(order, key)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE queue_rcpts SET next_attempt_at = ?, last_attempt_at = ? WHERE id = ?`,
				now.Add(lease).Unix(), now.Unix(), r.ID); err != nil {
				return err
			}
			b.Rcpts = append(b.Rcpts, r)
		}
		for _, k := range order {
			batches = append(batches, groups[k])
		}
		return nil
	})
	return batches, err
}

// RcptResult records the outcome of a delivery attempt.
type RcptResult struct {
	ID        int64
	Status    string
	NextRetry time.Time
	Error     string
}

// RecordAttempts stores delivery attempt outcomes.
func (s *Store) RecordAttempts(ctx context.Context, results []RcptResult) error {
	return s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		for _, r := range results {
			errMsg := r.Error
			if len(errMsg) > 1000 {
				errMsg = errMsg[:1000]
			}
			if _, err := tx.ExecContext(ctx, `UPDATE queue_rcpts SET status = ?, attempts = attempts + 1, next_attempt_at = ?,
				last_error = ? WHERE id = ?`, r.Status, r.NextRetry.Unix(), errMsg, r.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

// CompletedEntries returns entries whose recipients have all been
// delivered or permanently failed.
func (s *Store) CompletedEntries(ctx context.Context) ([]*QueueEntry, error) {
	ids, err := queryInt64s(ctx, s.db, `SELECT q.id FROM queue q WHERE NOT EXISTS
		(SELECT 1 FROM queue_rcpts r WHERE r.queue_id = q.id AND r.status = 'pending') LIMIT 100`)
	if err != nil {
		return nil, err
	}
	var out []*QueueEntry
	for _, id := range ids {
		e, err := getQueueEntryTx(ctx, s.db, id, true)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// DeleteQueueEntry removes a queue entry.
func (s *Store) DeleteQueueEntry(ctx context.Context, id int64) error {
	return s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM queue WHERE id = ?`, id)
		return err
	})
}

// ListQueue returns queue entries (without message data), newest first.
func (s *Store) ListQueue(ctx context.Context, userID int64) ([]*QueueEntry, error) {
	q := `SELECT id FROM queue`
	var args []any
	if userID > 0 {
		q += ` WHERE user_id = ?`
		args = append(args, userID)
	}
	ids, err := queryInt64s(ctx, s.db, q+` ORDER BY id DESC LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	out := []*QueueEntry{}
	for _, id := range ids {
		e, err := getQueueEntryTx(ctx, s.db, id, false)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		e.Data = nil
		out = append(out, e)
	}
	return out, nil
}

// NextDue returns when the next pending recipient is due.
func (s *Store) NextDue(ctx context.Context) (time.Time, bool) {
	var next sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(next_attempt_at) FROM queue_rcpts WHERE status = 'pending'`).Scan(&next); err != nil || !next.Valid {
		return time.Time{}, false
	}
	return time.Unix(next.Int64, 0), true
}

// ErrTooLate is returned when a queued message has already been (or is
// being) delivered and can no longer be cancelled.
var ErrTooLate = errors.New("message has already been sent")

// CancelQueued cancels delivery of one of the user's queued messages
// (undo send) if no delivery attempt has started, and deletes its copy in
// Sent. The cancelled entry is returned.
func (s *Store) CancelQueued(ctx context.Context, userID, id int64) (*QueueEntry, error) {
	var e *QueueEntry
	err := s.write(ctx, func(tx *sql.Tx, emit emitFunc) error {
		var err error
		e, err = getQueueEntryTx(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if e.UserID != userID {
			return ErrNotFound
		}
		for _, r := range e.Rcpts {
			if r.Status != RcptPending || r.Attempts > 0 || !r.LastAttemptAt.IsZero() {
				return ErrTooLate
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM queue WHERE id = ?`, id); err != nil {
			return err
		}
		if e.SentCopyID > 0 {
			removed, err := deleteByIDTx(ctx, tx, userID, []int64{e.SentCopyID})
			if err != nil {
				return err
			}
			emitRemoved(emit, userID, removed, nil)
		}
		return nil
	})
	return e, err
}

// RetryQueueEntry makes all pending recipients of an entry due now.
func (s *Store) RetryQueueEntry(ctx context.Context, id int64) error {
	return s.write(ctx, func(tx *sql.Tx, _ emitFunc) error {
		res, err := tx.ExecContext(ctx, `UPDATE queue_rcpts SET next_attempt_at = ? WHERE queue_id = ? AND status = 'pending'`, nowUnix(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SortRcpts orders recipients by address (for stable output).
func SortRcpts(list []*QueueRcpt) {
	sort.Slice(list, func(i, j int) bool { return list[i].Rcpt < list[j].Rcpt })
}
