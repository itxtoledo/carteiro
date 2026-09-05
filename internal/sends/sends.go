// Package sends records recently queued messages (the "sent e-mails" history
// of the web panel) and provides small RFC 5322 build/parse helpers used by
// the compose endpoint. The history is PERSISTED in the database (sends_log
// table): it survives restarts, unlike the earlier in-memory ring. Bodies are
// stored capped so old messages keep rendering.
package sends

import (
	"time"

	"carteiro/internal/storage"
)

// Message lifecycle states (kept in sync with storage.Status* for the queue).
const (
	StatusQueued    = "queued"
	StatusDelivered = "delivered"
	StatusDead      = "dead"
)

// Summary is the list view of a send (no bodies).
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

// Recorder persists and reads the send history. A nil *Recorder is valid:
// every method becomes a no-op, which keeps optional wiring simple.
type Recorder struct {
	store   *storage.Store
	maxBody int
}

// New creates a recorder over the given store; message bodies stored in the
// history are capped at maxBodyBytes (older content is dropped and the entry
// flagged truncated). Non-positive caps fall back to 512 KiB.
func New(store *storage.Store, maxBodyBytes int) *Recorder {
	if maxBodyBytes <= 0 {
		maxBodyBytes = 512 << 10
	}
	return &Recorder{store: store, maxBody: maxBodyBytes}
}

func fromLog(l storage.SendLog) Summary {
	return Summary{
		ID: l.ID, From: l.From, To: l.To, Subject: l.Subject,
		Status: l.Status, Attempts: l.Attempts, LastError: l.LastError,
		QueuedAt: l.QueuedAt, UpdatedAt: l.UpdatedAt,
		Size: l.Size, Truncated: l.Truncated,
	}
}

// Add records a message the moment it is queued. data is the raw message
// (headers + body); the subject and the renderable bodies are parsed once
// here and persisted, so the panel never has to re-parse after restarts.
func (r *Recorder) Add(id, from string, to []string, data []byte) {
	if r == nil || r.store == nil {
		return
	}
	truncated := len(data) > r.maxBody
	raw := data
	if truncated {
		raw = data[:r.maxBody]
	}
	p := parseMsg(raw)
	now := time.Now().UTC()
	_ = r.store.InsertSendLog(storage.SendLog{
		ID: id, From: from, To: to, Subject: p.subject,
		Status: StatusQueued, Size: len(data), Truncated: truncated,
		QueuedAt: now, UpdatedAt: now,
		HTML: p.html, Text: p.text, Raw: string(raw),
	})
}

// MarkDelivered flags a recorded send as fully delivered (attempts kept).
func (r *Recorder) MarkDelivered(id string) {
	if r == nil || r.store == nil {
		return
	}
	_ = r.store.SetSendStatus(id, StatusDelivered, "")
}

// MarkDead flags a recorded send as dead-lettered with the given reason
// (attempts kept).
func (r *Recorder) MarkDead(id, reason string) {
	if r == nil || r.store == nil {
		return
	}
	_ = r.store.SetSendStatus(id, StatusDead, reason)
}

// MarkQueued requeues a recorded send (dead -> queued) and resets its attempt
// counter, mirroring what the retry endpoint does in the database.
func (r *Recorder) MarkQueued(id string) {
	if r == nil || r.store == nil {
		return
	}
	_ = r.store.UpdateSendLogStatus(id, StatusQueued, 0, "")
}

// BumpAttempt increments the recorded attempt counter of a send.
func (r *Recorder) BumpAttempt(id string) {
	if r == nil || r.store == nil {
		return
	}
	r.store.BumpSendAttempt(id)
}

// Drop removes a recorded send (used when the enqueue that created it fails).
func (r *Recorder) Drop(id string) {
	if r == nil || r.store == nil {
		return
	}
	r.store.DeleteSendLog(id)
}

// List returns the most recent sends from the persisted history, newest
// first.
func (r *Recorder) List(limit int) []Summary {
	if r == nil || r.store == nil {
		return []Summary{}
	}
	logs, err := r.store.ListSendLogs(limit)
	if err != nil {
		return []Summary{}
	}
	out := make([]Summary, 0, len(logs))
	for _, l := range logs {
		out = append(out, fromLog(l))
	}
	return out
}

// Get returns the full detail of one recorded send.
func (r *Recorder) Get(id string) (Detail, bool) {
	if r == nil || r.store == nil {
		return Detail{}, false
	}
	l, ok, err := r.store.GetSendLog(id)
	if err != nil || !ok {
		return Detail{}, false
	}
	return Detail{Summary: fromLog(l), HTML: l.HTML, Text: l.Text, Raw: l.Raw}, true
}
