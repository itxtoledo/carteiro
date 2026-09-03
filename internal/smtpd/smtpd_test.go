package smtpd

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"carteiro/internal/config"
	"carteiro/internal/metrics"
	"carteiro/internal/storage"
)

func startServer(t *testing.T, mutate func(c *config.Config)) (*Server, *storage.Store, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open("sqlite", t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err := store.UpsertAccounts([]storage.AccountSeed{
		{Email: "sender@example.com", Password: "secret", AllowedFrom: []string{"news@example.com"}},
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Hostname:       "relay.test",
		MaxMessageSize: 1 << 20,
		MaxRecipients:  10,
		RequireTLS:     false,
		Delivery:       config.Delivery{MaxAttempts: 5},
	}
	if mutate != nil {
		mutate(cfg)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(cfg, store, logger, &metrics.Metrics{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { srv.ServeWithListener(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		srv.Shutdown(2 * time.Second)
	})
	return srv, store, ln.Addr().String()
}

// dial opens an SMTP client whose ServerInfo.Name is 127.0.0.1, which is
// required for net/smtp's PlainAuth to accept plaintext on loopback.
func dial(t *testing.T, addr string) *smtp.Client {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c, err := smtp.NewClient(conn, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func auth(c *smtp.Client, user, pass string) error {
	return c.Auth(smtp.PlainAuth("", user, pass, "127.0.0.1"))
}

func TestSmtpRoundTripQueuesMessage(t *testing.T) {
	_, store, addr := startServer(t, nil)

	c := dial(t, addr)
	defer c.Quit()
	if err := auth(c, "sender@example.com", "secret"); err != nil {
		t.Fatalf("auth: %v", err)
	}
	if err := c.Mail("sender@example.com"); err != nil {
		t.Fatalf("mail: %v", err)
	}
	if err := c.Rcpt("dest@example.net"); err != nil {
		t.Fatalf("rcpt: %v", err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatalf("data: %v", err)
	}
	if _, err := w.Write([]byte("Subject: test\r\n\r\nbody of the message\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("end of data: %v", err)
	}

	msgs, err := store.ListMessages(storage.StatusQueued, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(msgs))
	}
	if msgs[0].Sender != "sender@example.com" || len(msgs[0].To) != 1 {
		t.Errorf("envelope wrong: %+v", msgs[0])
	}

	// Read the raw body through NextDue to assert the content/headers.
	due, err := store.NextDue(time.Now(), 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("NextDue: %v len=%d", err, len(due))
	}
	eml := string(due[0].Data)
	for _, want := range []string{"Subject: test", "Received: from", "relay.test", "body of the message"} {
		if !strings.Contains(eml, want) {
			t.Errorf("message missing %q: %.120q", want, eml)
		}
	}
}

func TestSmtpRejectsMailWithoutAuth(t *testing.T) {
	_, _, addr := startServer(t, nil)
	c := dial(t, addr)
	if err := c.Mail("sender@example.com"); err == nil || !strings.Contains(err.Error(), "530") {
		t.Fatalf("MAIL without auth should fail with 530, got %v", err)
	}
}

func TestSmtpRejectsBadPassword(t *testing.T) {
	_, _, addr := startServer(t, nil)
	c := dial(t, addr)
	err := auth(c, "sender@example.com", "wrong-password")
	if err == nil || !strings.Contains(err.Error(), "535") {
		t.Fatalf("wrong password should fail with 535, got %v", err)
	}
}

func TestSmtpRejectsForbiddenSender(t *testing.T) {
	_, _, addr := startServer(t, nil)
	c := dial(t, addr)
	if err := auth(c, "sender@example.com", "secret"); err != nil {
		t.Fatal(err)
	}
	err := c.Mail("someone@example.org")
	if err == nil || !strings.Contains(err.Error(), "553") {
		t.Fatalf("MAIL FROM from another sender should fail with 553, got %v", err)
	}
	// The allowed_from entry is accepted.
	if err := c.Mail("news@example.com"); err != nil {
		t.Fatalf("MAIL FROM with allowed_from should pass: %v", err)
	}
}

// TestRequireTLSBlocksPlaintextAuthOutsideLoopback validates the authAllowed
// rule: with require_tls, non-loopback connections may only authenticate
// after STARTTLS; loopback stays allowed for local development.
func TestRequireTLSBlocksPlaintextAuthOutsideLoopback(t *testing.T) {
	cfg := &config.Config{RequireTLS: true}
	srv := &Server{cfg: cfg}
	lan := &net.TCPAddr{IP: net.ParseIP("192.168.1.10")}

	c := &smtpConn{s: srv, nc: fakeConn{remote: lan}}
	if c.authAllowed() {
		t.Error("LAN without TLS should not authenticate with require_tls")
	}
	c.tlsOn = true
	if !c.authAllowed() {
		t.Error("with TLS active it should authenticate")
	}
	c.tlsOn = false
	c.nc = fakeConn{remote: &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}}
	if !c.authAllowed() {
		t.Error("loopback should authenticate even with require_tls")
	}
}

type fakeConn struct {
	net.Conn
	remote net.Addr
}

func (f fakeConn) RemoteAddr() net.Addr { return f.remote }

func TestSmtpSizeLimitExceeded(t *testing.T) {
	_, _, addr := startServer(t, func(c *config.Config) { c.MaxMessageSize = 100 })
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	readGreeting(t, br)
	writeLine(t, conn, "EHLO raw.test")
	drainEhlo(t, br)

	creds := base64.StdEncoding.EncodeToString([]byte("\x00sender@example.com\x00secret"))
	writeLine(t, conn, "AUTH PLAIN "+creds)
	if resp := readLine(t, br); !strings.Contains(resp, "235") {
		t.Fatalf("auth failed: %s", resp)
	}
	writeLine(t, conn, "MAIL FROM:<sender@example.com> SIZE=100000")
	if resp := readLine(t, br); !strings.Contains(resp, "552") {
		t.Fatalf("declared SIZE above the limit should give 552, got %s", resp)
	}
}

func readGreeting(t *testing.T, br *bufio.Reader) {
	t.Helper()
	if resp := readLine(t, br); !strings.Contains(resp, "220") {
		t.Fatalf("greeting: %s", resp)
	}
}

func drainEhlo(t *testing.T, br *bufio.Reader) {
	t.Helper()
	for {
		line := readLine(t, br)
		if !strings.HasPrefix(line, "250-") {
			if !strings.HasPrefix(line, "250 ") {
				t.Fatalf("invalid EHLO reply: %q", line)
			}
			return
		}
	}
}

func readLine(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading reply: %v", err)
	}
	return strings.TrimSpace(line)
}

func writeLine(t *testing.T, conn net.Conn, s string) {
	t.Helper()
	if _, err := conn.Write([]byte(s + "\r\n")); err != nil {
		t.Fatalf("writing command: %v", err)
	}
}
