package storage

import (
	"context"
	"fmt"
	"time"
)

// SendLog is one persistent entry of the web panel's send history. Bodies
// (html/text/raw) are stored capped so old messages keep rendering after
// restarts.
type SendLog struct {
	ID        string
	From      string
	To        []string
	Subject   string
	Status    string
	Attempts  int
	Size      int
	Truncated bool
	LastError string
	HTML      string
	Text      string
	Raw       string
	QueuedAt  time.Time
	UpdatedAt time.Time
}

// InsertSendLog appends a message to the persistent history. Re-inserting the
// same id updates it (idempotent).
func (s *Store) InsertSendLog(l SendLog) error {
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO sends_log(id, sender, to_json, subject, status, attempts, size, truncated,
		 last_error, html, text, raw, queued_at, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET sender = excluded.sender,
		   to_json = excluded.to_json, subject = excluded.subject,
		   status = excluded.status, attempts = excluded.attempts,
		   size = excluded.size, truncated = excluded.truncated,
		   last_error = excluded.last_error, html = excluded.html,
		   text = excluded.text, raw = excluded.raw,
		   updated_at = excluded.updated_at`,
		l.ID, l.From, toJSON(l.To), l.Subject, l.Status, l.Attempts, l.Size,
		b2i(l.Truncated), l.LastError, l.HTML, l.Text, l.Raw,
		l.QueuedAt.UnixMilli(), l.UpdatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("logging send %s: %w", l.ID, err)
	}
	return nil
}

// UpdateSendLogStatus advances the delivery state of a logged send. Only the
// fields that change during the lifecycle are touched.
func (s *Store) UpdateSendLogStatus(id, status string, attempts int, lastError string) error {
	res, err := s.db.ExecContext(context.Background(),
		`UPDATE sends_log SET status = ?, attempts = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		status, attempts, lastError, nowMS(), id)
	if err != nil {
		return fmt.Errorf("updating send %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("send %s not in history", id)
	}
	return nil
}

// SetSendStatus updates the delivery state without touching the attempt
// counter (used for delivered/dead transitions, where the attempts recorded
// so far must be kept).
func (s *Store) SetSendStatus(id, status, lastError string) error {
	res, err := s.db.ExecContext(context.Background(),
		`UPDATE sends_log SET status = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		status, lastError, nowMS(), id)
	if err != nil {
		return fmt.Errorf("updating send %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("send %s not in history", id)
	}
	return nil
}

// BumpSendAttempt increments the attempt counter of a logged send.
func (s *Store) BumpSendAttempt(id string) {
	_, _ = s.db.ExecContext(context.Background(),
		`UPDATE sends_log SET attempts = attempts + 1, updated_at = ? WHERE id = ?`, nowMS(), id)
}

// DeleteSendLog removes a logged send (used when queueing failed).
func (s *Store) DeleteSendLog(id string) {
	_, _ = s.db.ExecContext(context.Background(), `DELETE FROM sends_log WHERE id = ?`, id)
}

// ListSendLogs returns the most recent entries of the send history.
func (s *Store) ListSendLogs(limit int) ([]SendLog, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT id, sender, to_json, subject, status, attempts, size, truncated,
		        last_error, queued_at, updated_at
		 FROM sends_log ORDER BY queued_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SendLog
	for rows.Next() {
		var l SendLog
		var toRaw string
		var queued, updated int64
		if err := rows.Scan(&l.ID, &l.From, &toRaw, &l.Subject, &l.Status, &l.Attempts,
			&l.Size, &l.Truncated, &l.LastError, &queued, &updated); err != nil {
			return nil, err
		}
		to, err := fromJSON[[]string](toRaw)
		if err != nil {
			return nil, err
		}
		l.To = to
		l.QueuedAt = timeFromMS(queued)
		l.UpdatedAt = timeFromMS(updated)
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetSendLog returns one entry of the history including the renderable
// bodies.
func (s *Store) GetSendLog(id string) (SendLog, bool, error) {
	var l SendLog
	var toRaw string
	var queued, updated int64
	err := s.db.QueryRowContext(context.Background(),
		`SELECT id, sender, to_json, subject, status, attempts, size, truncated,
		        last_error, html, text, raw, queued_at, updated_at
		 FROM sends_log WHERE id = ?`, id).
		Scan(&l.ID, &l.From, &toRaw, &l.Subject, &l.Status, &l.Attempts,
			&l.Size, &l.Truncated, &l.LastError, &l.HTML, &l.Text, &l.Raw,
			&queued, &updated)
	if err != nil {
		if err == sqlNoRows() {
			return SendLog{}, false, nil
		}
		return SendLog{}, false, err
	}
	to, err := fromJSON[[]string](toRaw)
	if err != nil {
		return SendLog{}, false, err
	}
	l.To = to
	l.QueuedAt = timeFromMS(queued)
	l.UpdatedAt = timeFromMS(updated)
	return l, true, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
