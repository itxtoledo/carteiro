// Package acme implements managed TLS for the SMTP listener: when enabled,
// Carteiro obtains and renews a Let's Encrypt certificate itself (via the
// lego ACME client) and serves it dynamically, so renewals never require a
// restart. The ACME registration and the last certificate are persisted in
// the database. When the feature is disabled, nothing changes and an external
// proxy can keep terminating TLS.
package acme

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"sync"
	"time"

	"carteiro/internal/config"
	"carteiro/internal/storage"
)

// errNotReady is returned by GetCertificate before the first obtain succeeds.
var errNotReady = errors.New("managed certificate not ready yet")

// Let's Encrypt ACME directory endpoints.
const (
	DirectoryProduction = "https://acme-v02.api.letsencrypt.org/directory"
	DirectoryStaging    = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

// store is the subset of storage.Store used by the manager (kept small so
// tests can stub persistence).
type Store interface {
	GetACMEAccount() (storage.ACMEAccount, bool, error)
	SaveACMEAccount(storage.ACMEAccount) error
	GetManagedCert() (storage.ManagedCert, bool, error)
	SaveManagedCert(storage.ManagedCert) error
}

// obtainer is the ACME interaction seam: production uses lego (lego.go),
// tests inject a fake to exercise the renewal logic offline.
type obtainer interface {
	// Obtain ensures an ACME account for email (reusing accountKeyPEM when
	// non-empty) and returns the certificate for domain as PEM texts, the
	// account key to persist, and the certificate expiry.
	Obtain(ctx context.Context, email, accountKeyPEM, domain string) (certPEM, keyPEM, outAccountKey string, notAfter time.Time, err error)
}

// Manager runs the ACME lifecycle: it keeps the current certificate in
// memory for tls.GetCertificate, persists account + certificate, and renews
// in the background well before expiry.
type Manager struct {
	cfg    *config.ACME
	domain string
	store  Store
	log    *slog.Logger
	ob     obtainer
	dirURL string

	// renewalLead is how long before notAfter a renewal is triggered.
	renewalLead time.Duration
	// checkEvery is how often the expiry is re-evaluated in the background.
	checkEvery time.Duration

	mu   sync.RWMutex
	cert *tls.Certificate
}

// New creates the manager. dirURL is the ACME directory (production or
// staging, chosen from the config).
func New(cfg *config.ACME, domain string, store Store, log *slog.Logger, dirURL string) (*Manager, error) {
	return &Manager{
		cfg:         cfg,
		domain:      domain,
		store:       store,
		log:         log,
		dirURL:      dirURL,
		renewalLead: 30 * 24 * time.Hour,
		checkEvery:  12 * time.Hour,
	}, nil
}

// SetObtainer replaces the ACME backend (tests only).
func (m *Manager) SetObtainer(o obtainer) { m.ob = o }

// DirectoryURL reports the configured ACME directory.
func (m *Manager) DirectoryURL() string { return m.dirURL }

func (m *Manager) obtainer() obtainer {
	if m.ob != nil {
		return m.ob
	}
	m.ob = &legoObtainer{httpAddr: m.cfg.HTTPAddr, dirURL: m.dirURL, log: m.log}
	return m.ob
}

// loadPersisted loads the stored certificate into memory when it is still
// fresh for this hostname.
func (m *Manager) loadPersisted() {
	cert, ok, err := m.store.GetManagedCert()
	if err != nil {
		m.log.Error("acme: reading stored certificate failed", "err", err)
		return
	}
	if !ok {
		return
	}
	if cert.Domain != m.domain || cert.NotAfter.Before(time.Now().Add(m.renewalLead)) {
		m.log.Info("acme: stored certificate is stale, will obtain a new one", "domain", cert.Domain)
		return
	}
	tlsCert, err := tls.X509KeyPair([]byte(cert.CertPEM), []byte(cert.KeyPEM))
	if err != nil {
		m.log.Error("acme: stored certificate is invalid, will re-obtain", "err", err)
		return
	}
	m.mu.Lock()
	m.cert = &tlsCert
	m.mu.Unlock()
	m.log.Info("acme: using stored managed certificate", "domain", m.domain, "expires", cert.NotAfter.Format(time.RFC3339))
}

// Init loads a persisted certificate synchronously and, when none is usable,
// performs one obtain attempt with the given context (so a fresh install gets
// a certificate before the listener serves TLS). Failures are logged, not
// fatal: the background loop keeps retrying.
func (m *Manager) Init(ctx context.Context) {
	m.loadPersisted()
	if m.Certificate() != nil {
		return
	}
	m.log.Info("acme: no usable certificate yet, obtaining one", "domain", m.domain, "directory", m.dirURL)
	if err := m.ensure(ctx); err != nil {
		m.log.Error("acme: initial obtain failed (retrying in the background)", "domain", m.domain, "err", err)
	}
}

// Run renews the certificate in the background until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(m.checkEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.ensure(ctx); err != nil {
				m.log.Warn("acme: renewal attempt failed", "domain", m.domain, "err", err)
			}
		}
	}
}

// needsRenew reports whether the persisted certificate is missing or closer
// to expiry than the renewal lead.
func (m *Manager) needsRenew() (bool, error) {
	cert, ok, err := m.store.GetManagedCert()
	if err != nil {
		return false, err
	}
	if !ok {
		return true, nil
	}
	if cert.Domain != m.domain || !cert.NotAfter.After(time.Now().Add(m.renewalLead)) {
		return true, nil
	}
	return false, nil
}

// ensure obtains a certificate when needed (missing or expiring) and swaps it
// into memory plus the database.
func (m *Manager) ensure(ctx context.Context) error {
	need, err := m.needsRenew()
	if err != nil {
		return err
	}
	if !need {
		return nil
	}
	return m.obtainFresh(ctx)
}

func (m *Manager) obtainFresh(ctx context.Context) error {
	acc, has, err := m.store.GetACMEAccount()
	if err != nil {
		return err
	}
	email := m.cfg.Email
	accountKey := ""
	if has {
		email = acc.Email
		accountKey = acc.AccountKey
	}
	certPEM, keyPEM, outKey, notAfter, err := m.obtainer().Obtain(ctx, email, accountKey, m.domain)
	if err != nil {
		return err
	}
	tlsCert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return err
	}
	if err := m.store.SaveManagedCert(storage.ManagedCert{
		Domain: m.domain, CertPEM: certPEM, KeyPEM: keyPEM, NotAfter: notAfter,
	}); err != nil {
		return err
	}
	if outKey != "" {
		if err := m.store.SaveACMEAccount(storage.ACMEAccount{Email: email, AccountKey: outKey}); err != nil {
			m.log.Warn("acme: certificate stored but the account could not be persisted", "err", err)
		}
	}
	m.mu.Lock()
	m.cert = &tlsCert
	m.mu.Unlock()
	m.log.Info("acme: managed certificate ready", "domain", m.domain, "expires", notAfter.Format(time.RFC3339))
	return nil
}

// Certificate returns the current managed certificate (nil before the first
// successful obtain). Safe for concurrent use; intended for
// tls.Config.GetCertificate.
func (m *Manager) Certificate() *tls.Certificate {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cert
}

// GetCertificate satisfies tls.Config.GetCertificate.
func (m *Manager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c := m.Certificate()
	if c == nil {
		return nil, errNotReady
	}
	return c, nil
}
