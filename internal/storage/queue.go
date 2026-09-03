package storage

import (
	"context"
	"fmt"
	"time"
)

// Enqueue writes a new message to the queue and returns its ID.
func (s *Store) Enqueue(sender string, to []string, data []byte) (string, error) {
	return s.EnqueueWithID(NewID(time.Now()), sender, to, data)
}

// EnqueueWithID writes a new message with a caller-provided ID (used so the
// Received header can carry the queue ID).
func (s *Store) EnqueueWithID(id, sender string, to []string, data []byte) (string, error) {
	if len(to) == 0 {
		return "", fmt.Errorf("message %s: no recipients", id)
	}
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO queue_messages(id, sender, to_json, data, attempts, next_attempt, status, created_at)
		 VALUES(?, ?, ?, ?, 0, 0, ?, ?)`,
		id, sender, toJSON(to), data, StatusQueued, nowMS())
	if err != nil {
		return "", fmt.Errorf("enqueueing %s: %w", id, err)
	}
	s.wake()
	return id, nil
}

// NextDue returns up to limit messages ready for delivery. Each returned
// message is claimed with a lease (lease_until = now + LeaseTimeout) so a
// crash mid-delivery does not lose or duplicate beyond the at-least-once
// window: after the lease expires the message becomes due again.
func (s *Store) NextDue(now time.Time, limit int) ([]*Message, error) {
	ctx := context.Background()
	if limit <= 0 {
		limit = 100
	}
	nowMS := now.UnixMilli()

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, sender, to_json, data, attempts, next_attempt, created_at, last_error, permanent_json
		 FROM queue_messages
		 WHERE status = ? AND (next_attempt = 0 OR next_attempt <= ?)
		 ORDER BY created_at ASC
		 LIMIT ?`, StatusQueued, nowMS, limit)
	if err != nil {
		return nil, err
	}
	var msgs []*Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		msgs = append(msgs, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Claim each candidate atomically.
	lease := now.Add(LeaseTimeout).UnixMilli()
	var ready []*Message
	for _, m := range msgs {
		res, err := s.db.ExecContext(ctx,
			`UPDATE queue_messages SET lease_until = ?, worker_id = ?
			 WHERE id = ? AND status = ?
			   AND (lease_until = 0 OR lease_until <= ?)`,
			lease, s.instance, m.ID, StatusQueued, nowMS)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n == 1 {
			ready = append(ready, m)
		}
	}
	return ready, nil
}

func scanMessage(row interface{ Scan(...any) error }) (*Message, error) {
	var (
		m           Message
		toRaw       string
		permRaw     string
		nextAttempt int64
		createdAt   int64
	)
	if err := row.Scan(&m.ID, &m.From, &toRaw, &m.Data, &m.Attempts,
		&nextAttempt, &createdAt, &m.LastError, &permRaw); err != nil {
		return nil, err
	}
	to, err := fromJSON[[]string](toRaw)
	if err != nil {
		return nil, fmt.Errorf("message %s: invalid recipients: %w", m.ID, err)
	}
	perm, err := fromJSON[map[string]string](permRaw)
	if err != nil {
		return nil, fmt.Errorf("message %s: invalid permanent map: %w", m.ID, err)
	}
	m.To = to
	m.Permanent = perm
	m.NextAttempt = timeFromMS(nextAttempt)
	m.CreatedAt = timeFromMS(createdAt)
	return &m, nil
}

// Persist saves the current metadata of a claimed message (remaining
// recipients, permanent failures, attempts and next attempt).
func (s *Store) Persist(m *Message) error {
	if len(m.To) == 0 {
		// Nothing left to deliver: the final Succeed/DeadLetter handles the row.
		return nil
	}
	var next int64
	if !m.NextAttempt.IsZero() {
		next = m.NextAttempt.UnixMilli()
	}
	_, err := s.db.ExecContext(context.Background(),
		`UPDATE queue_messages SET to_json = ?, permanent_json = ?, attempts = ?,
		 next_attempt = ?, last_error = ?, lease_until = 0, worker_id = ''
		 WHERE id = ?`,
		toJSON(m.To), toJSON(m.Permanent), m.Attempts, next, m.LastError, m.ID)
	if err != nil {
		return fmt.Errorf("persisting %s: %w", m.ID, err)
	}
	return nil
}

// Succeed removes a delivered message from the queue.
func (s *Store) Succeed(id string) error {
	_, err := s.db.ExecContext(context.Background(),
		`DELETE FROM queue_messages WHERE id = ? AND status = ?`, id, StatusQueued)
	return err
}

// Release clears the delivery lease of a message (used after Persist
// scheduled the next attempt).
func (s *Store) Release(id string) error {
	_, err := s.db.ExecContext(context.Background(),
		`UPDATE queue_messages SET lease_until = 0, worker_id = '' WHERE id = ?`, id)
	return err
}

// DeadLetter moves a message to the dead state with a reason and prunes the
// oldest dead rows when the dead-max cap is configured.
func (s *Store) DeadLetter(id, reason string) error {
	_, err := s.db.ExecContext(context.Background(),
		`UPDATE queue_messages SET status = ?, last_error = ?, next_attempt = 0,
		 lease_until = 0, worker_id = '' WHERE id = ?`, StatusDead, reason, id)
	if err != nil {
		return err
	}
	return s.pruneDead()
}

// pruneDead deletes the oldest dead rows beyond the deadMax cap.
func (s *Store) pruneDead() error {
	if s.deadMax <= 0 {
		return nil
	}
	ctx := context.Background()
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM queue_messages WHERE status = ?`, StatusDead).Scan(&total); err != nil {
		return err
	}
	over := total - s.deadMax
	if over <= 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM queue_messages WHERE status = ? ORDER BY created_at ASC LIMIT ?`,
		StatusDead, over)
	if err != nil {
		return err
	}
	var victims []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		victims = append(victims, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range victims {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM queue_messages WHERE id = ? AND status = ?`, id, StatusDead); err != nil {
			return err
		}
	}
	return nil
}

// RequeueDead moves a dead message back to the queue for a fresh delivery
// attempt (attempts are reset).
func (s *Store) RequeueDead(id string) error {
	res, err := s.db.ExecContext(context.Background(),
		`UPDATE queue_messages SET status = ?, attempts = 0, next_attempt = 0,
		 last_error = '', lease_until = 0, worker_id = '' WHERE id = ? AND status = ?`,
		StatusQueued, id, StatusDead)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("message %s is not in dead state", id)
	}
	s.wake()
	return nil
}

// ListMessages returns queue messages for a status, newest first, without the
// message bodies (monitoring).
func (s *Store) ListMessages(status string, limit int) ([]MessageLite, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT id, sender, to_json, attempts, next_attempt, created_at, last_error, permanent_json
		 FROM queue_messages WHERE status = ? ORDER BY created_at DESC LIMIT ?`,
		status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageLite
	for rows.Next() {
		var l MessageLite
		var toRaw, permRaw string
		var next, created int64
		if err := rows.Scan(&l.ID, &l.Sender, &toRaw, &l.Attempts, &next, &created, &l.LastError, &permRaw); err != nil {
			return nil, err
		}
		to, err := fromJSON[[]string](toRaw)
		if err != nil {
			return nil, err
		}
		perm, err := fromJSON[map[string]string](permRaw)
		if err != nil {
			return nil, err
		}
		l.To = to
		l.Permanent = perm
		l.NextAttempt = timeFromMS(next)
		l.CreatedAt = timeFromMS(created)
		out = append(out, l)
	}
	return out, rows.Err()
}

// Stats returns queue counters for monitoring.
func (s *Store) Stats(now time.Time) (Stats, error) {
	var st Stats
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM queue_messages WHERE status = ?`, StatusQueued).Scan(&st.Queued); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM queue_messages WHERE status = ? AND (next_attempt = 0 OR next_attempt <= ?)`,
		StatusQueued, now.UnixMilli()).Scan(&st.Due); err != nil {
		return st, err
	}
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM queue_messages WHERE status = ?`, StatusDead).Scan(&st.Dead); err != nil {
		return st, err
	}
	return st, nil
}

// MessageLite is the monitoring view of a queued message (no body).
type MessageLite struct {
	ID          string            `json:"id"`
	Sender      string            `json:"sender"`
	To          []string          `json:"to"`
	Attempts    int               `json:"attempts"`
	NextAttempt time.Time         `json:"next_attempt,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	LastError   string            `json:"last_error,omitempty"`
	Permanent   map[string]string `json:"permanent,omitempty"`
}
