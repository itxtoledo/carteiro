package acme

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge/http01"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/registration"
)

// acmeUser adapts the registration account for lego.
type acmeUser struct {
	email string
	key   *ecdsa.PrivateKey
}

func (u *acmeUser) GetEmail() string                        { return u.email }
func (u *acmeUser) GetRegistration() *registration.Resource { return nil }
func (u *acmeUser) GetPrivateKey() crypto.PrivateKey        { return u.key }

// legoObtainer implements obtainer on top of the lego ACME client.
// legoObtainer implements obtainer on top of the lego ACME client. Only the
// http-01 challenge is used (no DNS provider or API keys): the validation
// server binds httpAddr during issuance.
type legoObtainer struct {
	httpAddr string // listener for the http-01 challenge (host:port)
	dirURL   string // ACME directory; empty = Let's Encrypt production default
	log      *slog.Logger
}

// Obtain registers (or reuses) an ACME account and obtains a certificate for
// the given domain. accountKeyPEM, when non-empty, is the persisted account
// key so renewals reuse the same account.
func (o *legoObtainer) Obtain(ctx context.Context, email, accountKeyPEM, domain string) (certPEM, keyPEM, outAccountKey string, notAfter time.Time, err error) {
	key, err := parseAccountKey(accountKeyPEM)
	if err != nil {
		return "", "", "", time.Time{}, err
	}

	user := &acmeUser{email: email, key: key}
	cfg := lego.NewConfig(user)
	if o.dirURL != "" {
		cfg.CADirURL = o.dirURL
	}
	client, err := lego.NewClient(cfg)
	if err != nil {
		return "", "", "", time.Time{}, fmt.Errorf("creating acme client: %w", err)
	}

	if err := o.configureChallenges(client); err != nil {
		return "", "", "", time.Time{}, err
	}

	if _, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true}); err != nil {
		return "", "", "", time.Time{}, fmt.Errorf("registering acme account: %w", err)
	}

	res, err := client.Certificate.Obtain(certificate.ObtainRequest{
		Domains: []string{domain},
		Bundle:  true,
	})
	if err != nil {
		return "", "", "", time.Time{}, fmt.Errorf("obtaining certificate for %s: %w", domain, err)
	}

	notAfter, err = leafNotAfter(res.Certificate)
	if err != nil {
		return "", "", "", time.Time{}, err
	}

	accountKey, err := encodeAccountKey(key)
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	return string(res.Certificate), string(res.PrivateKey), accountKey, notAfter, nil
}

func (o *legoObtainer) configureChallenges(client *lego.Client) error {
	host, port, err := net.SplitHostPort(o.httpAddr)
	if err != nil {
		host, port = "", "80"
	}
	host = strings.Trim(host, "[]")
	provider := http01.NewProviderServer(host, port)
	if err := client.Challenge.SetHTTP01Provider(provider); err != nil {
		return fmt.Errorf("acme http-01 provider: %w", err)
	}
	o.log.Info("acme: challenge provider http-01", "addr", o.httpAddr)
	return nil
}

// parseAccountKey decodes a persisted ECDSA account key, or generates a fresh
// one when none is stored yet.
func parseAccountKey(pemText string) (*ecdsa.PrivateKey, error) {
	if strings.TrimSpace(pemText) == "" {
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("acme: stored account key is not PEM")
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if pkcs8, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if ecdsaKey, ok := pkcs8.(*ecdsa.PrivateKey); ok {
			return ecdsaKey, nil
		}
	}
	return nil, errors.New("acme: stored account key is not an ECDSA key")
}

func encodeAccountKey(key *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("encoding acme account key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})), nil
}

// leafNotAfter returns the expiry of the first certificate in the PEM bundle.
func leafNotAfter(bundle []byte) (time.Time, error) {
	block, _ := pem.Decode(bundle)
	if block == nil {
		return time.Time{}, errors.New("acme: response contains no certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("acme: parsing certificate: %w", err)
	}
	return cert.NotAfter, nil
}
