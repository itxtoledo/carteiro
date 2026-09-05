package storage

import (
	"testing"
	"time"
)

func TestLifetimeCounters(t *testing.T) {
	s := openStore(t)
	got, err := s.GetCounters()
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range got {
		if v != 0 {
			t.Errorf("initial %s = %d, want 0", name, v)
		}
	}
	if err := s.AddCounter("messages_queued", 3); err != nil {
		t.Fatal(err)
	}
	if err := s.AddCounter("auth_success", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.AddCounter("messages_queued", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.AddCounter("nope", 1); err == nil {
		t.Fatal("unknown counter should error")
	}
	got, _ = s.GetCounters()
	if got["messages_queued"] != 4 || got["auth_success"] != 2 {
		t.Errorf("counters wrong: %v", got)
	}
}

func TestPruneSendLogs(t *testing.T) {
	s := openStore(t)
	add := func(id string, age time.Duration) {
		q := time.Now().Add(-age).UnixMilli()
		if _, err := s.db.Exec(`INSERT INTO sends_log(id, sender, to_json, subject, status, attempts, size, truncated, last_error, html, text, raw, queued_at, updated_at)
			VALUES(?, 'a@example.com', '["b@example.com"]', 's', 'queued', 0, 1, 0, '', '', '', '', ?, ?)`, id, q, q); err != nil {
			t.Fatal(err)
		}
	}
	add("old-40d", 40*24*time.Hour)
	add("old-10d", 10*24*time.Hour)
	add("fresh", time.Hour)

	n, err := s.PruneSendLogs(30)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("pruned = %d, want 1", n)
	}
	logs, err := s.ListSendLogs(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 {
		t.Errorf("remaining = %d, want 2", len(logs))
	}
	// 0 disables pruning.
	if _, err := s.PruneSendLogs(0); err != nil {
		t.Fatal(err)
	}
}
