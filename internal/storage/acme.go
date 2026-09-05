package storage

import (
	"context"
	"fmt"
	"time"
)

// ACMEAccount is the ACME registration stored by the managed-TLS feature: the
// private account key is what lets renewals reuse the same ACME account.
type ACMEAccount struct {
	Email      string
	AccountKey string // PEM of the ACME account private key
}

// ManagedCert is the last certificate obtained for the SMTP listener (PEM
// texts, exactly what tls.X509KeyPair needs).
type ManagedCert struct {
	Domain   string
	CertPEM  string
	KeyPEM   string
	NotAfter time.Time
}

// GetACMEAccount returns the stored ACME registration, if any.
func (s *Store) GetACMEAccount() (ACMEAccount, bool, error) {
	var a ACMEAccount
	var updated int64
	err := s.db.QueryRowContext(context.Background(),
		`SELECT email, account_key, updated_at FROM acme_account WHERE id = 1`).
		Scan(&a.Email, &a.AccountKey, &updated)
	if err != nil {
		if err == sqlNoRows() {
			return ACMEAccount{}, false, nil
		}
		return ACMEAccount{}, false, err
	}
	return a, true, nil
}

// SaveACMEAccount stores (or replaces) the ACME registration.
func (s *Store) SaveACMEAccount(a ACMEAccount) error {
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO acme_account(id, email, account_key, updated_at) VALUES(1, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET email = excluded.email,
		   account_key = excluded.account_key, updated_at = excluded.updated_at`,
		a.Email, a.AccountKey, nowMS())
	if err != nil {
		return fmt.Errorf("saving acme account: %w", err)
	}
	return nil
}

// GetManagedCert returns the stored managed certificate, if any.
func (s *Store) GetManagedCert() (ManagedCert, bool, error) {
	var c ManagedCert
	var updated, notAfter int64
	err := s.db.QueryRowContext(context.Background(),
		`SELECT domain, cert_pem, key_pem, not_after, updated_at FROM managed_cert WHERE id = 1`).
		Scan(&c.Domain, &c.CertPEM, &c.KeyPEM, &notAfter, &updated)
	if err != nil {
		if err == sqlNoRows() {
			return ManagedCert{}, false, nil
		}
		return ManagedCert{}, false, err
	}
	c.NotAfter = timeFromMS(notAfter)
	return c, true, nil
}

// SaveManagedCert stores (or replaces) the managed certificate.
func (s *Store) SaveManagedCert(c ManagedCert) error {
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO managed_cert(id, domain, cert_pem, key_pem, not_after, updated_at)
		 VALUES(1, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET domain = excluded.domain,
		   cert_pem = excluded.cert_pem, key_pem = excluded.key_pem,
		   not_after = excluded.not_after, updated_at = excluded.updated_at`,
		c.Domain, c.CertPEM, c.KeyPEM, c.NotAfter.UnixMilli(), nowMS())
	if err != nil {
		return fmt.Errorf("saving managed cert: %w", err)
	}
	return nil
}
