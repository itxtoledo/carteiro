package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

const (
	// LeaseTimeout is how long a claimed message stays locked to a worker
	// after a crash; after it, the message becomes due again (at-least-once).
	LeaseTimeout = 30 * time.Minute
)

func newInstanceID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102T150405Z"), hex.EncodeToString(b[:]))
}

func randBytes(dst []byte) {
	if _, err := rand.Read(dst); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
}

func toHex(b []byte) string { return hex.EncodeToString(b) }

// migrations are applied in order and tracked in schema_migrations.
var migrations = []string{
	`CREATE TABLE IF NOT EXISTS accounts (
		email         TEXT PRIMARY KEY,
		password_hash TEXT NOT NULL,
		allowed_from  TEXT NOT NULL DEFAULT '[]'
	)`,
	`CREATE TABLE IF NOT EXISTS dkim_keys (
		domain    TEXT PRIMARY KEY,
		selector  TEXT NOT NULL,
		key_data  TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS queue_messages (
		id            TEXT PRIMARY KEY,
		sender        TEXT NOT NULL,
		to_json       TEXT NOT NULL,
		data          BLOB NOT NULL,
		attempts      INTEGER NOT NULL DEFAULT 0,
		next_attempt  INTEGER NOT NULL DEFAULT 0,
		status        TEXT NOT NULL DEFAULT 'queued',
		lease_until   INTEGER NOT NULL DEFAULT 0,
		worker_id     TEXT NOT NULL DEFAULT '',
		created_at    INTEGER NOT NULL,
		last_error    TEXT NOT NULL DEFAULT '',
		permanent_json TEXT NOT NULL DEFAULT '{}'
	)`,
	`CREATE INDEX IF NOT EXISTS idx_queue_status_next ON queue_messages(status, next_attempt)`,
	// ACME-managed TLS state: the registration account (email + private key)
	// and the last certificate obtained for the SMTP listener. Single-row
	// tables (id is always 1).
	`CREATE TABLE IF NOT EXISTS acme_account (
		id          INTEGER PRIMARY KEY CHECK (id = 1),
		email       TEXT NOT NULL,
		account_key TEXT NOT NULL,
		updated_at  INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS managed_cert (
		id         INTEGER PRIMARY KEY CHECK (id = 1),
		domain     TEXT NOT NULL,
		cert_pem   TEXT NOT NULL,
		key_pem    TEXT NOT NULL,
		not_after  INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
}

func (s *Store) migrate() error {
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}
	for i, ddl := range migrations {
		version := i + 1
		var exists int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&exists); err != nil {
			return fmt.Errorf("checking migration %d: %w", version, err)
		}
		if exists > 0 {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			tx.Rollback()
			return fmt.Errorf("applying migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`,
			version, nowMS()); err != nil {
			tx.Rollback()
			return fmt.Errorf("recording migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
