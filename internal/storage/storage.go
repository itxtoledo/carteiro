// Package storage persists Carteiro's state in a relational database
// (SQLite by default, or MySQL through a DSN). It replaces the old on-disk
// JSON-file queue and the in-memory configuration snapshot: accounts, DKIM
// keys and the message queue all live in the database, so accounts and
// domains can be added over the air through the admin API without restarts.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"
)

// Account is an authorized SMTP sender. PasswordHash is a bcrypt hash;
// AllowedFrom is the full list of envelope addresses the account may use
// (always includes Email).
type Account struct {
	Email        string
	PasswordHash string
	AllowedFrom  []string
}

// AccountSeed is an account to upsert with a plain-text password (from the
// YAML/env seed or the admin API). It is hashed before being stored.
type AccountSeed struct {
	Email       string
	Password    string
	AllowedFrom []string
}

// DKIMKey is a signing key stored in the database. KeyData holds the PEM
// private key text.
type DKIMKey struct {
	Domain   string
	Selector string
	KeyData  string
}

// Message is a queued message (metadata + raw content).
type Message struct {
	ID          string
	From        string
	To          []string
	Attempts    int
	NextAttempt time.Time
	CreatedAt   time.Time
	LastError   string
	Permanent   map[string]string
	Data        []byte
}

// QueueStatus is the queue message lifecycle status.
const (
	StatusQueued = "queued"
	StatusDead   = "dead"
)

// UpsertSummary describes what a seed upsert changed.
type UpsertSummary struct {
	Created   []string
	Updated   []string
	Unchanged []string
}

func (u UpsertSummary) Total() int { return len(u.Created) + len(u.Updated) + len(u.Unchanged) }

// Stats is a queue health snapshot used by the monitoring API.
type Stats struct {
	Queued int `json:"queued"`
	Due    int `json:"due"`
	Dead   int `json:"dead"`
}

// Store is the database handle for the whole server state.
type Store struct {
	db       *sql.DB
	dialect  string // "sqlite" or "mysql"
	instance string // random worker id used for lease claims
	deadMax  int    // dead-letter cap (0 = unlimited)
	notify   chan struct{}
}

// SetDeadMax bounds the dead-letter table: whenever a message is moved to the
// dead state beyond this cap, the oldest dead rows are pruned. 0 disables the
// limit.
func (s *Store) SetDeadMax(n int) { s.deadMax = n }

// Open opens the configured store. kind is "sqlite" (path is a file path) or
// "mysql" (path is a DSN).
func Open(kind, path string) (*Store, error) {
	if kind == "" {
		kind = "sqlite"
	}
	var driver, dialect string
	switch kind {
	case "sqlite":
		driver, dialect = "sqlite", "sqlite"
	case "mysql":
		driver, dialect = "mysql", "mysql"
	default:
		return nil, fmt.Errorf("unsupported storage type %q (use sqlite or mysql)", kind)
	}
	if path == "" {
		return nil, fmt.Errorf("storage %s requires a path/DSN", kind)
	}

	db, err := sql.Open(driver, path)
	if err != nil {
		return nil, fmt.Errorf("opening %s database: %w", kind, err)
	}
	db.SetMaxOpenConns(5)
	db.SetConnMaxLifetime(10 * time.Minute)

	if kind == "sqlite" {
		// A single connection plus WAL keeps the queue lock-free in practice;
		// busy_timeout protects against transient contention.
		db.SetMaxOpenConns(1)
		conn, err := db.Conn(context.Background())
		if err != nil {
			db.Close()
			return nil, err
		}
		for _, pragma := range []string{
			"PRAGMA journal_mode=WAL",
			"PRAGMA synchronous=NORMAL",
			"PRAGMA busy_timeout=10000",
			"PRAGMA foreign_keys=ON",
		} {
			if _, err := conn.ExecContext(context.Background(), pragma); err != nil {
				conn.Close()
				db.Close()
				return nil, fmt.Errorf("sqlite pragma %q: %w", pragma, err)
			}
		}
		conn.Close()
	} else {
		db.SetConnMaxIdleTime(5 * time.Minute)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging %s database: %w", kind, err)
	}

	s := &Store{db: db, dialect: dialect, instance: newInstanceID(), notify: make(chan struct{}, 1)}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// Dialect returns "sqlite" or "mysql".
func (s *Store) Dialect() string { return s.dialect }

// Notify is woken when a message is enqueued in this process.
func (s *Store) Notify() <-chan struct{} { return s.notify }

func (s *Store) wake() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// nowMS returns the current time in Unix milliseconds.
func nowMS() int64 { return time.Now().UnixMilli() }

func timeFromMS(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// NewID generates a unique message identifier.
func NewID(now time.Time) string {
	var b [4]byte
	randBytes(b[:])
	return now.UTC().Format("20060102T150405.000000000Z") + "-" + toHex(b[:])
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func fromJSON[T any](raw string) (T, error) {
	var v T
	if raw == "" {
		return v, nil
	}
	err := json.Unmarshal([]byte(raw), &v)
	return v, err
}
