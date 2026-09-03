package dkim

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSignerRejectsGarbage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "chave.pem")
	if err := os.WriteFile(p, []byte("não é pem"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigner(p); err == nil {
		t.Fatal("expected an error for a file without PEM")
	}
}

func TestSignAddsDKIMHeader(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("From: contact@example.com\r\nTo: someone@example.net\r\nSubject: hi\r\n\r\nmessage body\r\n")
	signed, err := Sign(msg, "example.com", "mail", key)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	head := string(signed)
	if !strings.HasPrefix(head, "DKIM-Signature:") {
		t.Fatalf("signature was not prefixed: %.80q", head)
	}
	for _, want := range []string{"d=example.com", "s=mail", "bh=", "a=rsa-sha256"} {
		if !strings.Contains(head, want) {
			t.Errorf("signature missing %q: %s", want, head)
		}
	}
	if !strings.Contains(string(signed), "\r\nFrom: contact@example.com") {
		t.Error("the original message should remain intact after the DKIM header")
	}
}

func TestLoadSignerPKCS1RoundTrip(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "dkim.key")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(p, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSigner(p)
	if err != nil {
		t.Fatalf("LoadSigner: %v", err)
	}
	if loaded.Public() == nil {
		t.Fatal("loaded key without a public key")
	}
}
