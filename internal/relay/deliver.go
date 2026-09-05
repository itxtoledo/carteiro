// Package relay delivers the queued messages to the recipients' MX servers,
// with persistent retry/backoff and dead-letter. Queue state lives in the
// database (storage package); nothing is kept in memory between batches.
package relay

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"net/textproto"
	"sort"
	"strings"
	"sync"
	"time"

	"carteiro/internal/config"
	"carteiro/internal/dkim"
	"carteiro/internal/metrics"
	"carteiro/internal/sends"
	"carteiro/internal/storage"
)

// Test seams: lookupMX and smtpPort can be replaced in tests to point to a
// local fake MX.
var lookupMX = func(domain string) ([]*net.MX, error) { return net.LookupMX(domain) }
var smtpPort = "25"

// Deliverer processes the database queue and delivers messages.
type Deliverer struct {
	cfg     *config.Config
	store   *storage.Store
	log     *slog.Logger
	metrics *metrics.Metrics
	rec     *sends.Recorder
}

// New creates the deliverer. rec receives delivery-state updates for the web
// panel's recent-sends feed (may be nil).
func New(cfg *config.Config, store *storage.Store, log *slog.Logger, m *metrics.Metrics, rec *sends.Recorder) (*Deliverer, error) {
	return &Deliverer{cfg: cfg, store: store, log: log, metrics: m, rec: rec}, nil
}

// Run processes the queue until the context is cancelled.
func (d *Deliverer) Run(ctx context.Context) {
	interval := d.cfg.Delivery.PollInterval.D()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			d.processBatch()
			return
		case <-ticker.C:
			d.processBatch()
		case <-d.store.Notify():
			d.processBatch()
		}
	}
}

func (d *Deliverer) processBatch() {
	batch := d.cfg.Delivery.Concurrency * 2
	if batch < 2 {
		batch = 2
	}
	msgs, err := d.store.NextDue(time.Now(), batch)
	if err != nil {
		d.log.Error("failed to read the queue", "err", err)
		return
	}
	if len(msgs) == 0 {
		return
	}
	sem := make(chan struct{}, d.cfg.Delivery.Concurrency)
	var wg sync.WaitGroup
	for _, m := range msgs {
		wg.Add(1)
		go func(m *storage.Message) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			d.deliver(m)
		}(m)
	}
	wg.Wait()
}

func (d *Deliverer) deliver(m *storage.Message) {
	start := time.Now()
	id := m.ID
	d.rec.BumpAttempt(id)
	d.log.Info("delivering message", "id", id, "from", m.From, "to", m.To, "attempt", m.Attempts+1)

	body, err := d.prepareBody(m)
	if err != nil {
		d.log.Error("failed to prepare the message for delivery", "id", id, "err", err)
		d.deadLetter(id, "preparation: "+err.Error())
		return
	}

	remaining := append([]string(nil), m.To...)
	deliveredAny := false
	permCount := 0
	var retryErr error

	for len(remaining) > 0 {
		domain := domainOf(remaining[0])

		group, keep := splitByDomain(remaining, domain)
		remaining = keep

		delivered, perm, gErr := d.deliverGroup(domain, group, m.From, body)
		if gErr != nil {
			retryErr = gErr
			// The whole group (including recipients accepted before the DATA
			// failure) stays queued: nothing was delivered successfully to
			// this domain.
			remaining = append(remaining, group...)
			break
		}

		deliveredAny = deliveredAny || len(delivered) > 0
		left := make(map[string]bool, len(group))
		for _, a := range group {
			left[a] = true
		}
		for _, a := range delivered {
			delete(left, a)
		}
		permCount += len(perm)
		for a, reason := range perm {
			delete(left, a)
			if m.Permanent == nil {
				m.Permanent = map[string]string{}
			}
			m.Permanent[a] = reason
			d.log.Warn("recipient permanently rejected", "id", id, "to", a, "reason", reason)
		}
		for a := range left {
			remaining = append(remaining, a)
		}
		m.To = remaining
		if err := d.store.Persist(m); err != nil {
			d.log.Error("failed to persist progress", "id", id, "err", err)
		}
	}

	m.To = remaining

	if retryErr != nil {
		m.LastError = retryErr.Error()
		m.Attempts++
		if m.Attempts >= d.cfg.Delivery.MaxAttempts {
			d.log.Error("attempts exhausted, moving to dead-letter", "id", id, "attempts", m.Attempts, "err", retryErr)
			d.deadLetter(id, fmt.Sprintf("exhausted %d attempts: %v", m.Attempts, retryErr))
			return
		}
		m.NextAttempt = time.Now().Add(d.backoff(m.Attempts))
		if err := d.store.Persist(m); err != nil {
			d.log.Error("failed to persist retry", "id", id, "err", err)
		}
		d.store.Release(id)
		d.log.Warn("delivery postponed", "id", id, "err", retryErr, "attempt", m.Attempts, "next", m.NextAttempt.Format(time.RFC3339))
		return
	}

	if !deliveredAny && len(m.To) == 0 && permCount > 0 {
		d.log.Warn("no recipient accepted the message", "id", id)
		d.deadLetter(id, "all recipients rejected the message permanently: "+permSummary(m.Permanent))
		return
	}
	if err := d.store.Succeed(id); err != nil {
		d.log.Error("failed to remove the message from the queue", "id", id, "err", err)
		return
	}
	d.rec.MarkDelivered(id)
	if permCount > 0 {
		d.log.Warn("message partially delivered", "id", id, "duration", time.Since(start).Round(time.Millisecond), "permanent_failures", permCount)
		return
	}
	d.metrics.MessagesDelivered.Add(1)
	d.log.Info("message delivered", "id", id, "duration", time.Since(start).Round(time.Millisecond))
}

func (d *Deliverer) deadLetter(id, reason string) {
	if err := d.store.DeadLetter(id, reason); err != nil {
		d.log.Error("dead-letter failed", "id", id, "err", err)
		return
	}
	d.rec.MarkDead(id, reason)
	d.metrics.MessagesDead.Add(1)
}

// prepareBody normalizes the message to CRLF and applies the DKIM signature of
// the sender domain, reading the key from the database. Signing failures do
// not block delivery: the message goes out unsigned with a log alert.
func (d *Deliverer) prepareBody(m *storage.Message) ([]byte, error) {
	body := normalizeCRLF(m.Data)
	fromDomain := domainOf(m.From)
	key, ok, err := d.store.GetDKIM(fromDomain)
	if err != nil {
		d.log.Error("dkim lookup failed", "id", m.ID, "domain", fromDomain, "err", err)
		return body, nil
	}
	if !ok {
		return body, nil
	}
	signer, err := dkim.ParseSigner([]byte(key.KeyData))
	if err != nil {
		d.log.Error("dkim key in the database is invalid; delivering unsigned", "id", m.ID, "domain", fromDomain, "err", err)
		return body, nil
	}
	signed, err := dkim.Sign(body, fromDomain, key.Selector, signer)
	if err != nil {
		d.log.Error("DKIM signing failed; delivering unsigned", "id", m.ID, "domain", fromDomain, "err", err)
		return body, nil
	}
	d.log.Debug("message signed with DKIM", "id", m.ID, "domain", fromDomain, "selector", key.Selector)
	return signed, nil
}

// deliverGroup delivers to a single MX domain. It returns the delivered
// recipients, the permanently rejected ones (5xx) and a retry error for
// transient failures (4xx/network).
func (d *Deliverer) deliverGroup(domain string, rcpts []string, from string, body []byte) ([]string, map[string]string, error) {
	d.metrics.DeliveryAttempts.Add(1)
	perm := map[string]string{}

	host, err := d.resolveHost(domain)
	if err != nil {
		if isPermanent(err) {
			for _, r := range rcpts {
				perm[r] = err.Error()
			}
			return nil, perm, nil
		}
		return nil, perm, fmt.Errorf("resolving MX of %s: %w", domain, err)
	}

	client, conn, err := d.connect(host)
	if err != nil {
		return nil, perm, fmt.Errorf("connecting to %s (MX of %s): %w", host, domain, err)
	}
	defer conn.Close()

	if err := client.Hello(d.cfg.Hostname); err != nil {
		return nil, perm, fmt.Errorf("EHLO to %s: %w", host, err)
	}

	// Opportunistic TLS: use STARTTLS when available and the handshake
	// validates the certificate; on handshake failure fall back to plain text
	// (the standard MTA behavior).
	if ok, _ := client.Extension("STARTTLS"); ok {
		if tlsErr := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); tlsErr == nil {
			d.log.Debug("starttls with MX established", "mx", host, "domain", domain)
		} else {
			d.log.Debug("starttls failed, using plain text", "mx", host, "err", tlsErr)
			conn.Close()
			client, conn, err = d.connect(host)
			if err != nil {
				return nil, perm, fmt.Errorf("reconnecting to %s: %w", host, err)
			}
			defer conn.Close()
			if err := client.Hello(d.cfg.Hostname); err != nil {
				return nil, perm, fmt.Errorf("EHLO (plain) to %s: %w", host, err)
			}
		}
	}

	if err := client.Mail(from); err != nil {
		if isPermanent(err) {
			for _, r := range rcpts {
				perm[r] = fmt.Sprintf("MAIL FROM: %v", err)
			}
			return nil, perm, nil
		}
		return nil, perm, fmt.Errorf("MAIL FROM: %w", err)
	}

	var delivered []string
	var connErr error
	for _, rcp := range rcpts {
		if err := client.Rcpt(rcp); err != nil {
			if isPermanent(err) {
				perm[rcp] = fmt.Sprintf("RCPT: %v", err)
				continue
			}
			connErr = fmt.Errorf("RCPT %s: %w", rcp, err)
			break
		}
		delivered = append(delivered, rcp)
	}
	if connErr != nil {
		return nil, perm, connErr
	}
	if len(delivered) == 0 {
		return nil, perm, nil
	}

	w, err := client.Data()
	if err != nil {
		return nil, perm, fmt.Errorf("DATA: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		w.Close()
		return nil, perm, fmt.Errorf("writing data: %w", err)
	}
	if err := w.Close(); err != nil {
		if isPermanent(err) {
			for _, r := range delivered {
				perm[r] = fmt.Sprintf("DATA: %v", err)
			}
			return nil, perm, nil
		}
		return nil, perm, fmt.Errorf("DATA: %w", err)
	}
	client.Quit()
	return delivered, perm, nil
}

// connect tries each address of the host and returns the first one that
// accepts a connection and an SMTP greeting.
func (d *Deliverer) connect(host string) (*smtp.Client, net.Conn, error) {
	timeout := d.cfg.Delivery.ConnectTimeout.D()
	var lastErr error
	for _, addr := range hostAddrs(host) {
		conn, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			lastErr = err
			continue
		}
		conn.SetDeadline(time.Now().Add(d.cfg.Delivery.IOTimeout.D()))
		client, err := smtp.NewClient(conn, host)
		if err != nil {
			conn.Close()
			lastErr = err
			continue
		}
		return client, conn, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no address resolved for %s", host)
	}
	return nil, nil, lastErr
}

// resolveHost returns the MX with the lowest preference (or the A/AAAA when
// the domain has no MX, per RFC 5321 §5.1).
func (d *Deliverer) resolveHost(domain string) (string, error) {
	domain = strings.TrimSuffix(domain, ".")
	mxs, err := lookupMX(domain)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return "", fmt.Errorf("domain %s does not exist: %w", domain, err)
		}
		return "", fmt.Errorf("MX lookup for %s: %w", domain, err)
	}
	if len(mxs) > 0 {
		sort.SliceStable(mxs, func(i, j int) bool { return mxs[i].Pref < mxs[j].Pref })
		return mxs[0].Host, nil
	}
	hosts, err := net.LookupHost(domain)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return "", fmt.Errorf("domain %s does not exist (no MX or A): %w", domain, err)
		}
		return "", fmt.Errorf("resolving %s: %w", domain, err)
	}
	if len(hosts) == 0 {
		return "", fmt.Errorf("domain %s has no MX or A/AAAA records", domain)
	}
	return hosts[0], nil
}

func hostAddrs(host string) []string {
	addr := net.JoinHostPort(host, smtpPort)
	if ip := net.ParseIP(host); ip != nil {
		return []string{addr}
	}
	ips, err := net.LookupHost(host)
	if err != nil || len(ips) == 0 {
		return []string{addr}
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, net.JoinHostPort(ip, smtpPort))
	}
	return out
}

func isPermanent(err error) bool {
	var tpErr *textproto.Error
	if errors.As(err, &tpErr) {
		return tpErr.Code >= 500 && tpErr.Code != 421
	}
	return false
}

// permSummary joins the permanent rejection reasons for the dead-letter
// reason (bounded to keep the record readable).
func permSummary(perm map[string]string) string {
	if len(perm) == 0 {
		return ""
	}
	parts := make([]string, 0, len(perm))
	for addr, reason := range perm {
		parts = append(parts, addr+": "+reason)
	}
	if len(parts) > 3 {
		parts = parts[:3]
		parts = append(parts, "...")
	}
	return strings.Join(parts, "; ")
}

// backoff computes the exponential delay: base * 2^(n-1), capped at
// retry_max.
func (d *Deliverer) backoff(attempt int) time.Duration {
	base := d.cfg.Delivery.RetryBase.D()
	max := d.cfg.Delivery.RetryMax.D()
	shift := attempt - 1
	if shift > 62 {
		shift = 62
	}
	delay := base << uint(shift)
	if delay <= 0 || delay > max {
		return max
	}
	return delay
}

// normalizeCRLF converts the message to CRLF lines and guarantees the final
// terminator.
func normalizeCRLF(data []byte) []byte {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	lines := bytes.Split(data, []byte("\n"))
	if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
		lines = lines[:n-1]
	}
	out := make([]byte, 0, len(data)+8)
	for i, l := range lines {
		l = bytes.TrimSuffix(l, []byte("\r"))
		out = append(out, l...)
		if i < len(lines)-1 {
			out = append(out, '\r', '\n')
		}
	}
	return append(out, '\r', '\n')
}

func domainOf(addr string) string {
	at := strings.LastIndexByte(addr, '@')
	if at < 0 {
		return addr
	}
	return strings.ToLower(strings.TrimSuffix(addr[at+1:], "."))
}

func splitByDomain(addrs []string, domain string) (group, rest []string) {
	for _, a := range addrs {
		if domainOf(a) == domain {
			group = append(group, a)
		} else {
			rest = append(rest, a)
		}
	}
	return group, rest
}
