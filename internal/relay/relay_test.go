package relay

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"carteiro/internal/config"
	"carteiro/internal/metrics"
	"carteiro/internal/storage"
)

// fakeMX is a minimal in-memory SMTP server standing in for the remote MX.
type fakeMX struct {
	ln       net.Listener
	rcptCode int
	dataCode int

	mu        sync.Mutex
	from      string
	rcpts     []string
	received  []string
	messagesN int
}

func newFakeMX(t *testing.T, rcptCode, dataCode int) *fakeMX {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mx := &fakeMX{ln: ln, rcptCode: rcptCode, dataCode: dataCode}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go mx.handle(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return mx
}

func (m *fakeMX) port() string {
	_, port, _ := net.SplitHostPort(m.ln.Addr().String())
	return port
}

func (m *fakeMX) handle(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	bw := bufio.NewWriter(conn)
	reply := func(s string) {
		bw.WriteString(s + "\r\n")
		bw.Flush()
	}
	reply("220 fake.mx ESMTP")

	inData := false
	var msg strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				m.mu.Lock()
				m.received = append(m.received, msg.String())
				m.messagesN++
				m.mu.Unlock()
				inData = false
				reply(fmt.Sprintf("%d 2.0.0 Ok", m.dataCode))
				continue
			}
			if strings.HasPrefix(line, ".") {
				line = line[1:]
			}
			msg.WriteString(line + "\n")
			continue
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			reply("250-fake.mx\r\n250-8BITMIME\r\n250 OK")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			m.mu.Lock()
			m.from = line
			m.mu.Unlock()
			reply("250 2.1.0 Ok")
		case strings.HasPrefix(upper, "RCPT TO:"):
			m.mu.Lock()
			m.rcpts = append(m.rcpts, line)
			m.mu.Unlock()
			reply(fmt.Sprintf("%d 2.1.5 Ok", m.rcptCode))
		case strings.HasPrefix(upper, "DATA"):
			msg.Reset()
			reply("354 End data with <CR><LF>.<CR><LF>")
			inData = true
		case strings.HasPrefix(upper, "QUIT"):
			reply("221 2.0.0 Bye")
			return
		case strings.HasPrefix(upper, "RSET"):
			reply("250 2.0.0 Ok")
		default:
			reply("250 2.0.0 Ok")
		}
	}
}

func testDeliverer(t *testing.T, maxAttempts int) (*Deliverer, *storage.Store) {
	t.Helper()
	store, err := storage.Open("sqlite", t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := &config.Config{
		Hostname: "relay.test",
		Delivery: config.Delivery{
			ConnectTimeout: config.Duration(2 * time.Second),
			IOTimeout:      config.Duration(5 * time.Second),
			RetryBase:      config.Duration(time.Millisecond),
			RetryMax:       config.Duration(time.Second),
			MaxAttempts:    maxAttempts,
			PollInterval:   config.Duration(10 * time.Millisecond),
			Concurrency:    2,
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d, err := New(cfg, store, logger, &metrics.Metrics{})
	if err != nil {
		t.Fatal(err)
	}
	oldMX, oldPort := lookupMX, smtpPort
	t.Cleanup(func() {
		lookupMX = oldMX
		smtpPort = oldPort
	})
	return d, store
}

// pointToMX redirects the test DNS to a local fake MX.
func pointToMX(t *testing.T, d *Deliverer, mx *fakeMX) {
	t.Helper()
	lookupMX = func(domain string) ([]*net.MX, error) {
		return []*net.MX{{Host: "127.0.0.1"}}, nil
	}
	smtpPort = mx.port()
}

func enqueueMessage(t *testing.T, store *storage.Store, from string, to []string) string {
	t.Helper()
	msg := "Received: from x by relay.test with ESMTPSA id 1\r\n" +
		"Subject: test\r\nFrom: " + from + "\r\nTo: " + strings.Join(to, ", ") + "\r\n" +
		"\r\nbody\r\n"
	id, err := store.Enqueue(from, to, []byte(msg))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func queueStats(t *testing.T, store *storage.Store) storage.Stats {
	t.Helper()
	st, err := store.Stats(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestDeliverSuccess(t *testing.T) {
	d, store := testDeliverer(t, 5)
	mx := newFakeMX(t, 250, 250)
	pointToMX(t, d, mx)
	enqueueMessage(t, store, "sender@example.com", []string{"dest@example.net"})

	d.processBatch()

	st := queueStats(t, store)
	if st.Queued != 0 || st.Dead != 0 {
		t.Fatalf("message should be gone: %+v", st)
	}
	mx.mu.Lock()
	defer mx.mu.Unlock()
	if mx.messagesN != 1 {
		t.Fatalf("MX received %d messages, want 1", mx.messagesN)
	}
	got := mx.received[0]
	if !strings.Contains(got, "Subject: test") || !strings.Contains(got, "body") {
		t.Errorf("wrong received content: %q", got)
	}
	if !strings.HasPrefix(mx.from, "MAIL FROM:<sender@example.com>") {
		t.Errorf("wrong envelope from: %q", mx.from)
	}
	if len(mx.rcpts) != 1 || !strings.HasPrefix(mx.rcpts[0], "RCPT TO:<dest@example.net>") {
		t.Errorf("wrong envelope rcpt: %v", mx.rcpts)
	}
}

func TestDeliverPermanentRejectionDeadLetters(t *testing.T) {
	d, store := testDeliverer(t, 5)
	mx := newFakeMX(t, 550, 250)
	pointToMX(t, d, mx)
	enqueueMessage(t, store, "sender@example.com", []string{"dest@example.net"})

	d.processBatch()

	st := queueStats(t, store)
	if st.Dead != 1 {
		t.Fatalf("expected 1 dead-letter message, got %+v", st)
	}
	msgs, _ := store.ListMessages(storage.StatusDead, 10)
	if len(msgs) != 1 || !strings.Contains(msgs[0].LastError, "550") {
		t.Errorf("dead reason not recorded: %+v", msgs)
	}
}

func TestDeliverTransientExhaustsAttempts(t *testing.T) {
	d, store := testDeliverer(t, 2)
	mx := newFakeMX(t, 451, 250)
	pointToMX(t, d, mx)
	enqueueMessage(t, store, "sender@example.com", []string{"dest@example.net"})

	// Attempt 1: 451 is transient; schedules a retry (base of 1ms).
	d.processBatch()
	time.Sleep(60 * time.Millisecond)
	d.processBatch()

	st := queueStats(t, store)
	if st.Dead != 1 {
		t.Fatalf("expected a dead-letter after exhausted attempts, got %+v", st)
	}
}

func TestDeliverSignsWithDatabaseKey(t *testing.T) {
	d, store := testDeliverer(t, 3)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if _, err := store.UpsertDKIM([]storage.DKIMKey{
		{Domain: "example.com", Selector: "mail", KeyData: string(pemBytes)},
	}); err != nil {
		t.Fatal(err)
	}
	mx := newFakeMX(t, 250, 250)
	pointToMX(t, d, mx)
	enqueueMessage(t, store, "sender@example.com", []string{"dest@example.net"})

	d.processBatch()

	st := queueStats(t, store)
	if st.Queued != 0 || st.Dead != 0 {
		t.Fatalf("signed message should be delivered: %+v", st)
	}
	mx.mu.Lock()
	defer mx.mu.Unlock()
	if mx.messagesN != 1 {
		t.Fatalf("MX received %d messages, want 1", mx.messagesN)
	}
	if !strings.HasPrefix(mx.received[0], "DKIM-Signature:") {
		t.Errorf("message was not DKIM-signed: %.80q", mx.received[0])
	}
}

func TestDomainHelpers(t *testing.T) {
	if got := domainOf("User@Example.com"); got != "example.com" {
		t.Errorf("domainOf = %q", got)
	}
	g, r := splitByDomain([]string{"a@x.com", "b@y.com", "c@x.com"}, "x.com")
	if len(g) != 2 || len(r) != 1 {
		t.Errorf("splitByDomain wrong: g=%v r=%v", g, r)
	}
	norm := normalizeCRLF([]byte("line1\r\nline2\nline3"))
	if string(norm) != "line1\r\nline2\r\nline3\r\n" {
		t.Errorf("normalizeCRLF = %q", norm)
	}
	if isPermanent(fmt.Errorf("test")) {
		t.Error("a common error is not permanent")
	}
}

func TestBackoffGrowth(t *testing.T) {
	d := &Deliverer{cfg: &config.Config{Delivery: config.Delivery{
		RetryBase: config.Duration(time.Second),
		RetryMax:  config.Duration(4 * time.Hour),
	}}}
	if d.backoff(1) != time.Second {
		t.Error("backoff(1) should be 1s")
	}
	if d.backoff(3) != 4*time.Second {
		t.Errorf("backoff(3) = %v", d.backoff(3))
	}
	if d.backoff(60).Hours() > 4 {
		t.Errorf("backoff(60) should be capped at retry_max: %v", d.backoff(60))
	}
}
