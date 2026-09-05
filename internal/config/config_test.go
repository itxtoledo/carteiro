package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// configViaEnvIsolada points CARTEIRO_CONFIG to an empty temp file so that a
// real system config cannot interfere with the env tests.
func configViaEnvIsolada(t *testing.T) {
	t.Helper()
	t.Setenv("CARTEIRO_CONFIG", writeTemp(t, ""))
}

func TestLoadFromFileWithDefaults(t *testing.T) {
	configViaEnvIsolada(t)
	p := writeTemp(t, `
listen: "127.0.0.1:2525"
hostname: "smtp.example.com"
accounts:
  - email: "Sender@Example.com"
    password: "secret1"
    allowed_from:
      - "news@Example.com"
delivery:
  max_attempts: 3
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:2525" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	if cfg.Hostname != "smtp.example.com" {
		t.Errorf("hostname = %q", cfg.Hostname)
	}
	if cfg.Storage == nil || cfg.Storage.Type != "sqlite" {
		t.Errorf("default storage = %+v", cfg.Storage)
	}
	if cfg.MaxMessageSize != 25<<20 {
		t.Errorf("default max_message_size = %d", cfg.MaxMessageSize)
	}
	if cfg.Delivery.MaxAttempts != 3 {
		t.Errorf("max_attempts = %d", cfg.Delivery.MaxAttempts)
	}
	if cfg.Delivery.PollInterval.D() == 0 {
		t.Error("poll_interval default lost")
	}
	if len(cfg.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(cfg.Accounts))
	}
	if cfg.Accounts[0].Email != "sender@example.com" {
		t.Errorf("account email not normalized: %q", cfg.Accounts[0].Email)
	}
	if len(cfg.Accounts[0].AllowedFrom) != 1 || cfg.Accounts[0].AllowedFrom[0] != "news@example.com" {
		t.Errorf("allowed_from not normalized: %v", cfg.Accounts[0].AllowedFrom)
	}
}

func TestLoadEnvAccounts(t *testing.T) {
	configViaEnvIsolada(t)
	t.Setenv("CARTEIRO_LISTEN", ":9999")
	t.Setenv("CARTEIRO_ACCOUNTS", "a@x.com:p1;b@x.com:p2")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":9999" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	if len(cfg.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(cfg.Accounts))
	}
}

func TestEnvAccountOverridesFile(t *testing.T) {
	configViaEnvIsolada(t)
	t.Setenv("CARTEIRO_ACCOUNTS", "duplicated@x.com:new-password")
	p := writeTemp(t, `
accounts:
  - email: "duplicated@x.com"
    password: "old-password"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(cfg.Accounts))
	}
	if cfg.Accounts[0].Password != "new-password" {
		t.Error("the env account should override the file one")
	}
}

func TestLoadAllowsZeroAccounts(t *testing.T) {
	configViaEnvIsolada(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("accounts are optional (admin API), Load: %v", err)
	}
	if len(cfg.Accounts) != 0 {
		t.Errorf("accounts = %d", len(cfg.Accounts))
	}
}

func TestLoadRejectsBadEnvAccounts(t *testing.T) {
	configViaEnvIsolada(t)
	t.Setenv("CARTEIRO_ACCOUNTS", "no-separator")
	if _, err := Load(""); err == nil {
		t.Fatal("expected an error for an entry without ':'")
	}
}

func TestTLSModeValidation(t *testing.T) {
	configViaEnvIsolada(t)
	cert, key := selfSignedTLS(t)
	yaml := "tls:\n  mode: errado\n  cert_data: \"" + b64(cert) + "\"\n  key_data: \"" + b64(key) + "\"\n"
	if _, err := Load(writeTemp(t, yaml)); err == nil {
		t.Fatal("expected an error for an invalid tls.mode")
	}
}

// TLS is base64-only: certificate files are not supported anymore.
func TestTLSFileFieldsIgnored(t *testing.T) {
	configViaEnvIsolada(t)
	t.Setenv("CARTEIRO_TLS_CERT_FILE", "/tmp/x.crt")
	t.Setenv("CARTEIRO_TLS_KEY_FILE", "/tmp/x.key")
	// Only file vars => TLS stays off (files are ignored, not supported).
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TLS != nil {
		t.Error("file-based TLS vars should be ignored")
	}
	cert, key := selfSignedTLS(t)
	t.Setenv("CARTEIRO_TLS_CERT", b64(cert))
	t.Setenv("CARTEIRO_TLS_KEY", b64(key))
	cfg2, err := Load("")
	if err != nil {
		t.Fatalf("Load with base64: %v", err)
	}
	if cfg2.TLS == nil || cfg2.TLS.CertPEM == "" {
		t.Error("base64 TLS vars should enable TLS")
	}
}

func TestConfigFlagDoesNotExist(t *testing.T) {
	if _, err := Load("/does/not/exist.yaml"); err == nil {
		t.Fatal("expected an error for a nonexistent config")
	}
}

func TestStorageMySQLRequiresDSN(t *testing.T) {
	configViaEnvIsolada(t)
	p := writeTemp(t, "storage:\n  type: mysql\n")
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "dsn") {
		t.Fatalf("expected a DSN error for mysql, got %v", err)
	}
	t.Setenv("CARTEIRO_DB_DSN", "user:pass@tcp(db:3306)/carteiro")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load with CARTEIRO_DB_DSN: %v", err)
	}
	if cfg.Storage.Type != "mysql" || cfg.Storage.DSN == "" {
		t.Errorf("env dsn did not select mysql: %+v", cfg.Storage)
	}
}

func genTestKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return string(pemBytes)
}

func TestLoadDKIMFromEnvKeys(t *testing.T) {
	configViaEnvIsolada(t)
	key := base64.StdEncoding.EncodeToString([]byte(genTestKey(t)))
	t.Setenv("CARTEIRO_DKIM_KEYS", "x.com:sel:"+key)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.DKIM) != 1 {
		t.Fatalf("DKIM entries = %d, want 1", len(cfg.DKIM))
	}
	k := cfg.DKIM[0]
	if k.Domain != "x.com" || k.Selector != "sel" {
		t.Errorf("dkim key metadata wrong: %+v", k)
	}
	if k.KeyData == "" || !strings.Contains(k.KeyData, "-----BEGIN RSA PRIVATE KEY-----") {
		t.Error("inline key data missing or malformed")
	}
}

func TestLoadDKIMFromEnvKeysDefaultSelector(t *testing.T) {
	configViaEnvIsolada(t)
	key := base64.StdEncoding.EncodeToString([]byte(genTestKey(t)))
	t.Setenv("CARTEIRO_DKIM_KEYS", "x.com::"+key)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.DKIM) != 1 || cfg.DKIM[0].Selector != "mail" {
		t.Fatalf("selector should default to mail: %+v", cfg.DKIM)
	}
}

func TestLoadDKIMEnvOverridesYAML(t *testing.T) {
	configViaEnvIsolada(t)
	envKey := base64.StdEncoding.EncodeToString([]byte(genTestKey(t)))
	t.Setenv("CARTEIRO_DKIM_KEYS", "x.com:env-sel:"+envKey)
	p := writeTemp(t, `
dkim:
  - domain: "x.com"
    selector: "yaml"
    key_data: "`+base64.StdEncoding.EncodeToString([]byte(genTestKey(t)))+`"
  - domain: "y.com"
    selector: "yaml2"
    key_data: "`+base64.StdEncoding.EncodeToString([]byte(genTestKey(t)))+`"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.DKIM) != 2 {
		t.Fatalf("DKIM entries = %d, want 2", len(cfg.DKIM))
	}
	for _, k := range cfg.DKIM {
		if k.Domain == "x.com" {
			if k.Selector != "env-sel" || !strings.Contains(k.KeyData, "-----BEGIN") {
				t.Errorf("env should override YAML for x.com: %+v", k)
			}
			continue
		}
		if k.Domain != "y.com" {
			t.Errorf("unexpected dkim domain: %+v", k)
		}
	}
}

// YAML key_data must be base64 of a PEM key; direct PEM or garbage is
// rejected.
func TestLoadDKIMYAMLKeyDataMustBeBase64(t *testing.T) {
	configViaEnvIsolada(t)
	pem := genTestKey(t)
	bad := writeTemp(t, `
dkim:
  - domain: "x.com"
    selector: "mail"
    key_data: "`+pem+`"
`)
	if _, err := Load(bad); err == nil {
		t.Fatal("expected an error when key_data is raw PEM instead of base64")
	}
	garbage := writeTemp(t, `
dkim:
  - domain: "x.com"
    selector: "mail"
    key_data: "not-base64-!!"
`)
	if _, err := Load(garbage); err == nil {
		t.Fatal("expected an error for non-base64 key_data")
	}
	missing := writeTemp(t, `
dkim:
  - domain: "x.com"
    selector: "mail"
`)
	if _, err := Load(missing); err == nil || !strings.Contains(err.Error(), "key_data") {
		t.Fatalf("expected missing key_data error, got %v", err)
	}
}

func TestLoadDKIMEnvErrors(t *testing.T) {
	configViaEnvIsolada(t)
	// Missing key material entirely.
	t.Setenv("CARTEIRO_DKIM_KEYS", "x.com:mail")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "domain:selector:base64") {
		t.Fatalf("expected format error, got %v", err)
	}
	// Key must be base64 of a PEM; raw PEM or garbage is rejected.
	os.Unsetenv("CARTEIRO_DKIM_KEYS")
	t.Setenv("CARTEIRO_DKIM_KEYS", "x.com:mail:this-is-not-a-key")
	if _, err := Load(""); err == nil {
		t.Fatal("expected an error for an invalid base64 key")
	}
	t.Setenv("CARTEIRO_DKIM_KEYS", "x.com:mail:"+base64.StdEncoding.EncodeToString([]byte("not a pem at all")))
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "PEM") {
		t.Fatalf("expected a PEM error, got %v", err)
	}
}

func TestAPIValidation(t *testing.T) {
	configViaEnvIsolada(t)
	// Listen without a token is rejected.
	p := writeTemp(t, `api:
  listen: "127.0.0.1:9090"
`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("expected missing-token error, got %v", err)
	}
	// The API defaults to loopback :9090; the web panel is enabled alongside
	// it and defaults to :8080 (all interfaces, token-protected).
	p2 := writeTemp(t, "api:\n  token: \"tok-1\"\n")
	cfg, err := Load(p2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.Listen != "127.0.0.1:9090" {
		t.Errorf("default api listen = %q", cfg.API.Listen)
	}
	if cfg.Web == nil || cfg.Web.Listen != ":8080" {
		t.Errorf("default web listen = %+v", cfg.Web)
	}
	if cfg.API.Token != "tok-1" {
		t.Errorf("token wrong: %q", cfg.API.Token)
	}
	// The web listener is separately configurable (YAML web.listen).
	p3 := writeTemp(t, `api:
  token: "tok-1"
web:
  listen: ":9095"
`)
	cfgW, err := Load(p3)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfgW.Web.Listen != ":9095" || cfgW.API.Listen != "127.0.0.1:9090" {
		t.Errorf("separate web/api listens wrong: %+v / %+v", cfgW.Web, cfgW.API)
	}
	// Env token + api listen.
	t.Setenv("CARTEIRO_API_TOKEN", "env-tok")
	t.Setenv("CARTEIRO_API_LISTEN", "127.0.0.1:9191")
	cfg2, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.API.Listen != "127.0.0.1:9191" || cfg2.API.Token != "env-tok" {
		t.Errorf("env api config wrong: %+v", cfg2.API)
	}
	// HTTP_ADDR configures the WEB panel, not the API (the token still comes
	// from CARTEIRO_API_TOKEN).
	os.Unsetenv("CARTEIRO_API_LISTEN")
	t.Setenv("HTTP_ADDR", ":9090")
	cfg3, err := Load("")
	if err != nil {
		t.Fatalf("Load (HTTP_ADDR): %v", err)
	}
	if cfg3.API.Listen != "127.0.0.1:9090" || cfg3.Web.Listen != ":9090" || cfg3.API.Token != "env-tok" {
		t.Errorf("HTTP_ADDR alias wrong: api=%+v web=%+v", cfg3.API, cfg3.Web)
	}
	os.Unsetenv("HTTP_ADDR")
	t.Setenv("CARTEIRO_WEB_LISTEN", ":9091")
	cfg4, err := Load("")
	if err != nil {
		t.Fatalf("Load (CARTEIRO_WEB_LISTEN): %v", err)
	}
	if cfg4.Web.Listen != ":9091" {
		t.Errorf("CARTEIRO_WEB_LISTEN wrong: %+v", cfg4.Web)
	}
	// A web listener without an API token is rejected.
	os.Unsetenv("CARTEIRO_API_TOKEN")
	os.Unsetenv("CARTEIRO_WEB_LISTEN")
	t.Setenv("CARTEIRO_WEB_LISTEN", ":8080")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("expected missing-token error for web-only, got %v", err)
	}
}

func TestDeliveryAndQueueEnvOverrides(t *testing.T) {
	configViaEnvIsolada(t)
	t.Setenv("CARTEIRO_DELIVERY_MAX_ATTEMPTS", "7")
	t.Setenv("CARTEIRO_DELIVERY_CONCURRENCY", "3")
	t.Setenv("CARTEIRO_DELIVERY_RETRY_BASE", "42s")
	t.Setenv("CARTEIRO_QUEUE_DEAD_MAX", "25")
	t.Setenv("CARTEIRO_LOG_MASK_EMAILS", "true")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Delivery.MaxAttempts != 7 || cfg.Delivery.Concurrency != 3 {
		t.Errorf("delivery env wrong: %+v", cfg.Delivery)
	}
	if cfg.Delivery.RetryBase.D() != 42*time.Second {
		t.Errorf("retry_base env = %v", cfg.Delivery.RetryBase.D())
	}
	if cfg.Queue.DeadMax != 25 {
		t.Errorf("queue dead_max env = %d", cfg.Queue.DeadMax)
	}
	if !cfg.LogMaskEmails {
		t.Error("log_mask_emails env should be true")
	}
	// Defaults when nothing is set.
	for _, k := range []string{
		"CARTEIRO_DELIVERY_MAX_ATTEMPTS", "CARTEIRO_DELIVERY_CONCURRENCY",
		"CARTEIRO_DELIVERY_RETRY_BASE", "CARTEIRO_QUEUE_DEAD_MAX",
		"CARTEIRO_LOG_MASK_EMAILS",
	} {
		os.Unsetenv(k)
	}
	cfgD, _ := Load("")
	if cfgD.Delivery.MaxAttempts != 10 || cfgD.Queue.DeadMax != 1000 {
		t.Errorf("delivery/queue defaults wrong: attempts=%d dead_max=%d", cfgD.Delivery.MaxAttempts, cfgD.Queue.DeadMax)
	}
	if !cfgD.LogMaskEmails {
		t.Error("log_mask_emails should default to true (emails hidden)")
	}
	// Explicit false disables the mask (full addresses in logs).
	t.Setenv("CARTEIRO_LOG_MASK_EMAILS", "false")
	cfgF, _ := Load("")
	if cfgF.LogMaskEmails {
		t.Error("log_mask_emails=false should disable the mask")
	}
	// Invalid durations are rejected.
	t.Setenv("CARTEIRO_DELIVERY_RETRY_BASE", "abc")
	if _, err := Load(""); err == nil {
		t.Fatal("expected an error for an invalid retry_base")
	}
}

func TestLoadDKIMKeysMultiDomainEnv(t *testing.T) {
	configViaEnvIsolada(t)
	keyA := genTestKey(t)
	keyB := genTestKey(t)
	// Two domains in one list, both as base64 (the only accepted form).
	list := "doma.com:mail:" + base64.StdEncoding.EncodeToString([]byte(keyA)) +
		";domb.com:selector-b:" + base64.StdEncoding.EncodeToString([]byte(keyB))
	t.Setenv("CARTEIRO_DKIM_KEYS", list)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.DKIM) != 2 {
		t.Fatalf("DKIM entries = %d, want 2 (%+v)", len(cfg.DKIM), cfg.DKIM)
	}
	byDomain := map[string]DKIMKey{}
	for _, k := range cfg.DKIM {
		byDomain[k.Domain] = k
	}
	a, ok := byDomain["doma.com"]
	if !ok || a.Selector != "mail" || !strings.Contains(a.KeyData, "-----BEGIN") {
		t.Errorf("doma.com key wrong: %+v", byDomain)
	}
	b, ok := byDomain["domb.com"]
	if !ok || b.Selector != "selector-b" || !strings.Contains(b.KeyData, "-----BEGIN") {
		t.Errorf("domb.com key wrong: %+v", byDomain)
	}
}

func TestLoadDKIMKeysInvalidEntry(t *testing.T) {
	configViaEnvIsolada(t)
	t.Setenv("CARTEIRO_DKIM_KEYS", "doma.com:mail")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "domain:selector:base64") {
		t.Fatalf("expected format error, got %v", err)
	}
	t.Setenv("CARTEIRO_DKIM_KEYS", "doma.com:mail:not-a-key")
	if _, err := Load(""); err == nil {
		t.Fatal("expected an error for a bad key material")
	}
}

// selfSignedTLS generates a throwaway self-signed certificate + key pair and
// returns (certPEM, keyPEM).
func selfSignedTLS(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	return certPEM, keyPEM
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestTLSFromYAMLBase64(t *testing.T) {
	configViaEnvIsolada(t)
	cert, key := selfSignedTLS(t)
	p := writeTemp(t, "tls:\n  mode: \"implicit\"\n  cert_data: \""+b64(cert)+"\"\n  key_data: \""+b64(key)+"\"\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TLS.Mode != "implicit" {
		t.Errorf("mode = %q", cfg.TLS.Mode)
	}
	if !strings.Contains(cfg.TLS.CertPEM, "-----BEGIN CERTIFICATE-----") || !strings.Contains(cfg.TLS.KeyPEM, "-----BEGIN RSA PRIVATE KEY-----") {
		t.Error("base64 was not decoded into PEM texts")
	}
}

func TestTLSEnvBase64(t *testing.T) {
	configViaEnvIsolada(t)
	cert, key := selfSignedTLS(t)
	t.Setenv("CARTEIRO_TLS_CERT", b64(cert))
	t.Setenv("CARTEIRO_TLS_KEY", b64(key))
	t.Setenv("CARTEIRO_TLS_MODE", "implicit")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TLS.Mode != "implicit" || cfg.TLS.CertPEM == "" || cfg.TLS.KeyPEM == "" {
		t.Errorf("env tls wrong: %+v", cfg.TLS)
	}
}

func TestTLSValidationErrors(t *testing.T) {
	configViaEnvIsolada(t)
	// cert_data without key_data.
	p := writeTemp(t, "tls:\n  cert_data: \""+b64("not really")+"\"\n")
	if _, err := Load(p); err == nil {
		t.Fatal("expected an error for cert_data without key_data")
	}
	// Raw PEM (not base64) is rejected.
	cert, key := selfSignedTLS(t)
	p2 := writeTemp(t, "tls:\n  cert_data: \"-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\"\n  key_data: \""+b64(key)+"\"\n")
	_ = cert
	if _, err := Load(p2); err == nil {
		t.Fatal("expected an error when cert_data is raw PEM instead of base64")
	}
	// A pair that does not match is rejected at parse time.
	otherCert, _ := selfSignedTLS(t)
	p3 := writeTemp(t, "tls:\n  cert_data: \""+b64(otherCert)+"\"\n  key_data: \""+b64(key)+"\"\n")
	if _, err := Load(p3); err == nil {
		t.Fatal("expected an error for a mismatched certificate/key pair")
	}

}

func TestBarePortListenIsNormalized(t *testing.T) {
	configViaEnvIsolada(t)
	// A bare port in the SMTP listener env is normalized to host:port.
	t.Setenv("CARTEIRO_LISTEN", "2525")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":2525" {
		t.Errorf("bare SMTP port = %q, want \":2525\"", cfg.Listen)
	}

	// Bare ports for the admin API listener env.
	os.Unsetenv("CARTEIRO_LISTEN")
	t.Setenv("CARTEIRO_API_TOKEN", "tok")
	t.Setenv("CARTEIRO_API_LISTEN", "8080")
	cfg2, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.API.Listen != ":8080" {
		t.Errorf("bare api port = %q, want \":8080\"", cfg2.API.Listen)
	}

	// Bare ports for the web panel env (CARTEIRO_WEB_LISTEN and HTTP_ADDR).
	os.Unsetenv("CARTEIRO_API_LISTEN")
	t.Setenv("CARTEIRO_WEB_LISTEN", "9091")
	cfg3, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg3.Web.Listen != ":9091" {
		t.Errorf("bare web port via CARTEIRO_WEB_LISTEN = %q", cfg3.Web.Listen)
	}
	os.Unsetenv("CARTEIRO_WEB_LISTEN")
	t.Setenv("HTTP_ADDR", "9092")
	cfg4, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg4.Web.Listen != ":9092" {
		t.Errorf("bare web port via HTTP_ADDR = %q", cfg4.Web.Listen)
	}
}

func TestACMEConfigAndValidation(t *testing.T) {
	configViaEnvIsolada(t)
	// Enabled without an email is rejected.
	t.Setenv("CARTEIRO_HOSTNAME", "smtp.example.com")
	t.Setenv("CARTEIRO_ACME", "true")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "email") {
		t.Fatalf("expected missing-email error, got %v", err)
	}
	// Hostname must be a domain, not an IP.
	t.Setenv("CARTEIRO_ACME_EMAIL", "ops@example.com")
	t.Setenv("CARTEIRO_HOSTNAME", "127.0.0.1")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "hostname") {
		t.Fatalf("expected hostname error, got %v", err)
	}
	t.Setenv("CARTEIRO_HOSTNAME", "smtp.example.com")

	// Defaults: provider http01, challenge listener :80.
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.ACME.Enabled || cfg.ACME.Provider != "http01" || cfg.ACME.HTTPAddr != ":80" {
		t.Errorf("acme defaults wrong: %+v", cfg.ACME)
	}
	// Provider override and a bare HTTP port number.
	t.Setenv("CARTEIRO_ACME_PROVIDER", "cloudflare")
	t.Setenv("CARTEIRO_ACME_HTTP_ADDR", "8080")
	cfg2, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg2.ACME.Provider != "cloudflare" || cfg2.ACME.HTTPAddr != ":8080" {
		t.Errorf("acme env overrides wrong: %+v", cfg2.ACME)
	}
	// Invalid provider rejected.
	t.Setenv("CARTEIRO_ACME_PROVIDER", "nonsense")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "provider") {
		t.Fatalf("expected provider error, got %v", err)
	}
}
