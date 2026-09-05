package sends

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"carteiro/internal/storage"
)

func TestBuildAndParseRoundTrip(t *testing.T) {
	raw, err := BuildMessage("20260903T100000Z-abc123", "sender@example.com",
		[]string{"a@example.com", "b@example.com"},
		"Café com pão", "Olá, mundo!\nLinha dois", "<html><body><h1>Olá</h1></body></html>")
	if err != nil {
		t.Fatal(err)
	}
	msg := string(raw)
	for _, want := range []string{
		"From: sender@example.com",
		"To: a@example.com, b@example.com",
		"Message-ID: <20260903T100000Z-abc123@carteiro>",
		"Content-Type: multipart/alternative",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q", want)
		}
	}
	if strings.Contains(msg, "\n\n\n") {
		t.Error("message contains an empty separator line")
	}

	p := parseMsg(raw)
	if p.subject != "Café com pão" {
		t.Errorf("subject = %q", p.subject)
	}
	if !strings.Contains(p.text, "Olá, mundo!") || !strings.Contains(p.text, "Linha dois") {
		t.Errorf("text body = %q", p.text)
	}
	if !strings.Contains(p.html, "<h1>Olá</h1>") {
		t.Errorf("html body = %q", p.html)
	}
}

func TestBuildRequiresContent(t *testing.T) {
	if _, err := BuildMessage("id", "a@example.com", []string{"b@example.com"}, "s", "", ""); err == nil {
		t.Error("empty bodies should be rejected")
	}
	if _, err := BuildMessage("id", "a@example.com", nil, "s", "text", ""); err == nil {
		t.Error("missing recipients should be rejected")
	}
	if _, err := BuildMessage("id", "a\r\nb@example.com", []string{"x@example.com"}, "s", "t", ""); err == nil {
		t.Error("header injection should be rejected")
	}
}

func TestParsePlainAndNoHeaderFallbacks(t *testing.T) {
	plain := []byte("Subject: Hi\nContent-Type: text/plain; charset=\"utf-8\"\n\nJust text body\nsecond line")
	p := parseMsg(plain)
	if p.subject != "Hi" || !strings.Contains(p.text, "Just text body") {
		t.Errorf("plain parse = %+v", p)
	}

	// Bare message without any headers: whole payload becomes the text.
	crlf := []byte("From: a@example.com\r\nTo: b@example.com\r\nSubject: X\r\n\r\nbody")
	if end, _ := headerEnd(crlf); end == -1 {
		t.Error("CRLF header separator not detected")
	}
	noHeaders := []byte("totally not a message with headers")
	p2 := parseMsg(noHeaders)
	if p2.text != "totally not a message with headers" {
		t.Errorf("no-header fallback = %q", p2.text)
	}
}

func TestParseQuotedPrintableLatin1(t *testing.T) {
	// =E1 is "á" in latin-1, wrapped with a soft line break.
	qp := "Subject: =?iso-8859-1?q?Ol=E1?=\nContent-Type: text/plain; charset=\"iso-8859-1\"\n" +
		"Content-Transfer-Encoding: quoted-printable\n\ncora=E7=\r\n\xc3o"
	p := parseMsg([]byte(qp))
	if p.subject != "Olá" {
		t.Errorf("encoded-word subject = %q", p.subject)
	}
	if !strings.Contains(p.text, "coraç") {
		t.Errorf("latin-1 quoted-printable body = %q", p.text)
	}
}

func openRecorderStore(t *testing.T) *storage.Store {
	t.Helper()
	st, err := storage.Open("sqlite", t.TempDir()+"/sends.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestRecorderLifecycle(t *testing.T) {
	store := openRecorderStore(t)
	r := New(store, 1<<20)
	data, _ := BuildMessage("id-1", "from@example.com", []string{"to@example.com"}, "Sub", "Hello", "")
	r.Add("id-1", "from@example.com", []string{"to@example.com"}, data)
	r.Add("id-2", "from@example.com", []string{"to@example.com"}, []byte("Subject: Two\n\nplain"))

	if got := len(r.List(0)); got != 2 {
		t.Fatalf("list len = %d", got)
	}
	// Newest first.
	if r.List(1)[0].ID != "id-2" {
		t.Errorf("newest-first ordering broken: %+v", r.List(2))
	}

	r.BumpAttempt("id-1")
	r.MarkDead("id-1", "connection refused")
	d, ok := r.Get("id-1")
	if !ok {
		t.Fatal("id-1 missing")
	}
	if d.Status != StatusDead || d.LastError != "connection refused" || d.Attempts != 1 {
		t.Errorf("dead entry wrong: %+v", d.Summary)
	}
	if d.Subject != "Sub" || d.Text != "Hello" {
		t.Errorf("detail bodies wrong: subject=%q text=%q", d.Subject, d.Text)
	}

	r.MarkQueued("id-1")
	d, _ = r.Get("id-1")
	if d.Status != StatusQueued || d.Attempts != 0 {
		t.Errorf("requeue reset wrong: %+v", d.Summary)
	}

	r.MarkDelivered("id-2")
	d2, _ := r.Get("id-2")
	if d2.Status != StatusDelivered {
		t.Errorf("delivered status wrong: %+v", d2.Summary)
	}
}

func TestHistoryPersistsAcrossRecorders(t *testing.T) {
	store := openRecorderStore(t)
	r1 := New(store, 1<<20)
	for i := 0; i < 4; i++ {
		r1.Add(fmt.Sprintf("id-%d", i), "f@example.com", []string{"t@example.com"}, []byte("Subject: s\n\nbody"))
	}
	r1.MarkDelivered("id-1")

	// A brand-new recorder over the same database sees the whole history:
	// this is what makes the panel survive restarts.
	r2 := New(store, 1<<20)
	sums := r2.List(0)
	if len(sums) != 4 {
		t.Fatalf("history size after new recorder = %d, want 4", len(sums))
	}
	if sums[0].ID != "id-3" || sums[3].ID != "id-0" {
		t.Errorf("ordering wrong after reload: %+v", sums)
	}
	d, ok := r2.Get("id-1")
	if !ok || d.Status != StatusDelivered {
		t.Errorf("status did not survive reload: ok=%v d=%+v", ok, d)
	}
}

func TestRecorderCapsBody(t *testing.T) {
	store := openRecorderStore(t)
	r := New(store, 64) // tiny cap
	big := []byte("Subject: big\n\n" + strings.Repeat("x", 500))
	r.Add("b", "f@example.com", []string{"t@example.com"}, big)
	sums := r.List(1)
	if len(sums) == 0 || !sums[0].Truncated || sums[0].Size != len(big) {
		t.Fatalf("truncation not reported: %+v", sums)
	}
	d, _ := r.Get("b")
	if len(d.Raw) > 128 {
		t.Errorf("raw not capped: %d bytes", len(d.Raw))
	}
}

func TestNilRecorderIsNoop(t *testing.T) {
	var r *Recorder
	r.Add("id", "f", []string{"t"}, []byte("x"))
	r.MarkDelivered("id")
	if got := r.List(5); len(got) != 0 {
		t.Errorf("nil recorder should list nothing, got %d", len(got))
	}
	if _, ok := r.Get("id"); ok {
		t.Error("nil recorder should not find entries")
	}
}

func TestRecorderListLimit(t *testing.T) {
	store := openRecorderStore(t)
	r := New(store, 1<<20)
	for i := 0; i < 5; i++ {
		r.Add(fmt.Sprintf("id-%d", i), "f@example.com", []string{"t@example.com"}, []byte("Subject: s\n\nbody"))
	}
	sums := r.List(3)
	if len(sums) != 3 {
		t.Fatalf("limited list = %d, want 3", len(sums))
	}
	if sums[0].ID != "id-4" {
		t.Errorf("limit did not keep the newest first: %+v", sums)
	}
	// The older entries remain stored (history is not evicted).
	if got := r.List(0); len(got) != 5 {
		t.Errorf("history size = %d, want 5 (no eviction)", len(got))
	}
}

func TestQPEncoder(t *testing.T) {
	// Line-wrapping must stay below the 76 char limit (plus the trailing
	// "=" soft-break marker) and keep content.
	out := qpEncode(bytes.Repeat([]byte("a"), 200))
	lines := bytes.Split(out, []byte("\r\n"))
	for _, l := range lines[:len(lines)-1] {
		content := strings.TrimSuffix(string(l), "=")
		if len(content) > 76 {
			t.Errorf("qp line too long: %d", len(content))
		}
	}
	// Trailing spaces are encoded.
	if !bytes.Contains(qpEncode([]byte("a \n")), []byte("a=20\r\n")) {
		t.Errorf("trailing space not encoded: %q", qpEncode([]byte("a \n")))
	}
	if !bytes.Contains(qpEncode([]byte("olá")), []byte("=C3=A1")) {
		t.Errorf("non-ascii not encoded: %q", qpEncode([]byte("olá")))
	}
}
