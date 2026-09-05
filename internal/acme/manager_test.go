package acme

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"carteiro/internal/config"
	"carteiro/internal/storage"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// selfSigned returns a certificate for the given domain valid until notAfter.
func selfSigned(t *testing.T, domain string, notAfter time.Time) (certPEM, keyPEM string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{domain},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	return certPEM, keyPEM
}

type fakeObtainer struct {
	calls    int
	notAfter time.Time
	fail     bool
}

func (f *fakeObtainer) Obtain(ctx context.Context, email, accountKeyPEM, domain string) (string, string, string, time.Time, error) {
	f.calls++
	if f.fail {
		return "", "", "", time.Time{}, io.ErrUnexpectedEOF
	}
	cert, key, err := certPair(domain, f.notAfter)
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	return cert, key, "account-key-pem", f.notAfter, nil
}

// certPair generates a self-signed certificate valid until notAfter.
func certPair(domain string, notAfter time.Time) (string, string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{domain},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	return certPEM, keyPEM, nil
}

func openStore(t *testing.T) *storage.Store {
	t.Helper()
	st, err := storage.Open("sqlite", t.TempDir()+"/acme.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newTestManager(t *testing.T, st *storage.Store, fake *fakeObtainer) *Manager {
	m, err := New(&config.ACME{Email: "ops@example.com", HTTPAddr: ":80"},
		"smtp.example.com", st, testLogger(), DirectoryStaging)
	if err != nil {
		t.Fatal(err)
	}
	m.SetObtainer(fake)
	return m
}

func TestInitObtainsWhenNothingStored(t *testing.T) {
	st := openStore(t)
	fake := &fakeObtainer{notAfter: time.Now().Add(60 * 24 * time.Hour)}
	m := newTestManager(t, st, fake)

	m.Init(context.Background())

	if fake.calls != 1 {
		t.Fatalf("obtain calls = %d, want 1", fake.calls)
	}
	if m.Certificate() == nil {
		t.Fatal("no certificate in memory after Init")
	}
	// Persisted for the next restart.
	cert, ok, err := st.GetManagedCert()
	if err != nil || !ok {
		t.Fatalf("stored cert: ok=%v err=%v", ok, err)
	}
	if cert.Domain != "smtp.example.com" {
		t.Errorf("stored domain = %q", cert.Domain)
	}
	acc, ok, err := st.GetACMEAccount()
	if err != nil || !ok || acc.AccountKey != "account-key-pem" {
		t.Errorf("stored account: ok=%v acc=%+v err=%v", ok, acc, err)
	}
}

func TestInitReusesFreshStoredCert(t *testing.T) {
	st := openStore(t)
	fake := &fakeObtainer{notAfter: time.Now().Add(60 * 24 * time.Hour)}
	cert, key := selfSigned(t, "smtp.example.com", time.Now().Add(50*24*time.Hour))
	if err := st.SaveManagedCert(storage.ManagedCert{
		Domain: "smtp.example.com", CertPEM: cert, KeyPEM: key, NotAfter: time.Now().Add(50 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t, st, fake)

	m.Init(context.Background())

	if fake.calls != 0 {
		t.Errorf("obtain calls = %d, want 0 (fresh stored cert)", fake.calls)
	}
	if m.Certificate() == nil {
		t.Fatal("stored certificate was not loaded")
	}
}

func TestInitRenewsCertNearExpiry(t *testing.T) {
	st := openStore(t)
	fake := &fakeObtainer{notAfter: time.Now().Add(60 * 24 * time.Hour)}
	cert, key := selfSigned(t, "smtp.example.com", time.Now().Add(10*24*time.Hour)) // < 30d lead
	if err := st.SaveManagedCert(storage.ManagedCert{
		Domain: "smtp.example.com", CertPEM: cert, KeyPEM: key, NotAfter: time.Now().Add(10 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t, st, fake)

	m.Init(context.Background())

	if fake.calls != 1 {
		t.Errorf("obtain calls = %d, want 1 (cert expiring soon)", fake.calls)
	}
}

func TestInitFailureLoggedAndRetriedByRun(t *testing.T) {
	st := openStore(t)
	fake := &fakeObtainer{notAfter: time.Now().Add(60 * 24 * time.Hour)}
	m := newTestManager(t, st, fake)

	// First attempt fails, second succeeds (Run tick).
	fake.fail = true
	m.Init(context.Background())
	if m.Certificate() != nil {
		t.Fatal("certificate should not be ready after a failed obtain")
	}
	fake.fail = false
	m.checkEvery = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for m.Certificate() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if m.Certificate() == nil {
		t.Fatal("certificate was never obtained by the background loop")
	}
}

func TestGetCertificateBeforeReady(t *testing.T) {
	st := openStore(t)
	fake := &fakeObtainer{fail: true}
	m := newTestManager(t, st, fake)
	m.Init(context.Background())

	if _, err := m.GetCertificate(nil); err == nil {
		t.Error("expected an error before the certificate is ready")
	}
}
