package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// counterColumns maps metric names to their stats_counters column.
var counterColumns = map[string]string{
	"auth_success":       "auth_success",
	"auth_failure":       "auth_failure",
	"messages_queued":    "messages_queued",
	"delivery_attempts":  "delivery_attempts",
	"messages_delivered": "messages_delivered",
	"messages_dead":      "messages_dead",
	"messages_requeued":  "messages_requeued",
}

func ensureCountersRow(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO stats_counters(id, updated_at) VALUES(1, ?)
		 ON CONFLICT(id) DO NOTHING`, nowMS())
	return err
}

// AddCounter increments one lifetime counter (idempotent row creation).
func (s *Store) AddCounter(name string, delta int64) error {
	col, ok := counterColumns[name]
	if !ok {
		return fmt.Errorf("unknown counter %q", name)
	}
	if delta == 0 {
		return nil
	}
	ctx := context.Background()
	if err := ensureCountersRow(ctx, s.db); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE stats_counters SET `+col+` = `+col+` + ?, updated_at = ? WHERE id = 1`,
		delta, nowMS())
	return err
}

// GetCounters returns every lifetime counter (zeros when none were recorded).
func (s *Store) GetCounters() (map[string]int64, error) {
	ctx := context.Background()
	if err := ensureCountersRow(ctx, s.db); err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(counterColumns))
	row := s.db.QueryRowContext(ctx,
		`SELECT auth_success, auth_failure, messages_queued, delivery_attempts,
		        messages_delivered, messages_dead, messages_requeued
		 FROM stats_counters WHERE id = 1`)
	var vals [7]int64
	if err := row.Scan(&vals[0], &vals[1], &vals[2], &vals[3], &vals[4], &vals[5], &vals[6]); err != nil {
		return nil, err
	}
	names := []string{"auth_success", "auth_failure", "messages_queued", "delivery_attempts",
		"messages_delivered", "messages_dead", "messages_requeued"}
	for i, n := range names {
		out[n] = vals[i]
	}
	return out, nil
}

// PruneSendLogs deletes send history rows older than retentionDays (0 keeps
// everything) and returns how many were removed.
func (s *Store) PruneSendLogs(retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour).UnixMilli()
	res, err := s.db.ExecContext(context.Background(),
		`DELETE FROM sends_log WHERE queued_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("pruning send history: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
