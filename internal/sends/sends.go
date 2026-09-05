// Package sends records recently queued messages (the "sent e-mails" feed of
// the web panel) and provides small RFC 5322 build/parse helpers used by the
// compose endpoint. It is an in-memory ring buffer: nothing is persisted and
// the recorder is only a convenience view over the database queue, which
// remains the source of truth for delivery.
package sends

import (
	"sync"
	"time"
)

// Message lifecycle states (kept in sync with storage.Status* for the DB).
const (
	StatusQueued    = "queued"
	StatusDelivered = "delivered"
	StatusDead      = "dead"
)

// Summary is the list view of a recorded send (no bodies).
type Summary struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        []string  `json:"to"`
	Subject   string    `json:"subject"`
	Status    string    `json:"status"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error,omitempty"`
	QueuedAt  time.Time `json:"queued_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Size      int       `json:"size"`
	Truncated bool      `json:"truncated,omitempty"`
}

// Detail is the single-send view: summary plus the parsed bodies used to
// render the message in the panel.
type Detail struct {
	Summary
	HTML string `json:"html,omitempty"`
	Text string `json:"text,omitempty"`
	Raw  string `json:"raw,omitempty"`
}

type entry struct {
	sum     Summary
	html    string
	text    string
	raw     []byte
	hasBody bool
	hasHTML bool
	hasText bool
}

// Recorder is a thread-safe ring of recently queued messages. A nil *Recorder
// is valid: every method becomes a no-op, which keeps optional wiring simple.
type Recorder struct {
	mu      sync.Mutex
	max     int
	maxBody int
	entries []entry
}

// New creates a recorder bounded to maxEntries entries; each stored message
// body is capped at maxBodyBytes (older content is dropped and the summary
// flagged truncated). Non-positive values fall back to 200 entries and
// 512 KiB per body.
func New(maxEntries, maxBodyBytes int) *Recorder {
	if maxEntries <= 0 {
		maxEntries = 200
	}
	if maxBodyBytes <= 0 {
		maxBodyBytes = 512 << 10
	}
	return &Recorder{max: maxEntries, maxBody: maxBodyBytes}
}

// Add records a message the moment it is queued. data is the raw message
// (headers + body); the subject and the renderable bodies are extracted once
// here so the panel never has to re-parse.
func (r *Recorder) Add(id, from string, to []string, data []byte) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().UTC()
	truncated := len(data) > r.maxBody
	raw := data
	if truncated {
		raw = data[:r.maxBody]
	}
	p := parseMsg(raw)
	e := entry{
		sum: Summary{
			ID: id, From: from, To: to, Subject: p.subject,
			Status: StatusQueued, QueuedAt: now, UpdatedAt: now,
			Size: len(data), Truncated: truncated,
		},
		raw: raw, hasBody: len(raw) > 0,
	}
	if p.html != "" {
		e.html, e.hasHTML = p.html, true
	}
	if p.text != "" {
		e.text, e.hasText = p.text, true
	}
	r.entries = append(r.entries, e)
	if len(r.entries) > r.max {
		r.entries = r.entries[len(r.entries)-r.max:]
	}
}

func (r *Recorder) update(id string, fn func(*entry)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.entries) - 1; i >= 0; i-- {
		e := &r.entries[i]
		if e.sum.ID == id {
			fn(e)
			e.sum.UpdatedAt = time.Now().UTC()
			return
		}
	}
}

// MarkDelivered flags a recorded send as fully delivered.
func (r *Recorder) MarkDelivered(id string) {
	r.update(id, func(e *entry) {
		e.sum.Status = StatusDelivered
		e.sum.LastError = ""
	})
}

// MarkDead flags a recorded send as dead-lettered with the given reason.
func (r *Recorder) MarkDead(id, reason string) {
	r.update(id, func(e *entry) {
		e.sum.Status = StatusDead
		e.sum.LastError = reason
	})
}

// MarkQueued requeues a recorded send (dead -> queued) and resets its
// attempt counter, mirroring what the retry endpoint does in the database.
func (r *Recorder) MarkQueued(id string) {
	r.update(id, func(e *entry) {
		e.sum.Status = StatusQueued
		e.sum.LastError = ""
		e.sum.Attempts = 0
	})
}

// BumpAttempt increments the recorded attempt counter of a send.
func (r *Recorder) BumpAttempt(id string) {
	r.update(id, func(e *entry) { e.sum.Attempts++ })
}

// Drop removes a recorded send. Callers record the entry before enqueueing
// (so a fast delivery cannot race ahead of the recorder) and drop it again if
// the enqueue fails.
func (r *Recorder) Drop(id string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.entries {
		if r.entries[i].sum.ID == id {
			r.entries = append(r.entries[:i], r.entries[i+1:]...)
			return
		}
	}
}

// List returns the most recent sends, newest first.
func (r *Recorder) List(limit int) []Summary {
	if r == nil {
		return []Summary{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 || limit > len(r.entries) {
		limit = len(r.entries)
	}
	out := make([]Summary, 0, limit)
	for i := len(r.entries) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, r.entries[i].sum)
	}
	return out
}

// Get returns the full detail of one recorded send.
func (r *Recorder) Get(id string) (Detail, bool) {
	if r == nil {
		return Detail{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.entries) - 1; i >= 0; i-- {
		e := &r.entries[i]
		if e.sum.ID == id {
			d := Detail{Summary: e.sum}
			if e.hasHTML {
				d.HTML = e.html
			}
			if e.hasText {
				d.Text = e.text
			}
			if e.hasBody {
				d.Raw = string(e.raw)
			}
			return d, true
		}
	}
	return Detail{}, false
}
