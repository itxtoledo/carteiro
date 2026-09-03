// Package dkim signs outbound messages (RFC 6376) with the per-domain
// private key. RSA (PKCS#1 or PKCS#8) and Ed25519 keys are supported; the
// matching DNS record is <selector>._domainkey.<domain>.
package dkim

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	"github.com/emersion/go-msgauth/dkim"
)

// LoadSigner reads a PEM private key file and parses it.
func LoadSigner(path string) (crypto.Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	signer, err := ParseSigner(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing key %s: %w", path, err)
	}
	return signer, nil
}

// ParseSigner parses a PEM private key. RSA (PKCS#1 or PKCS#8) and Ed25519
// keys are supported.
func ParseSigner(raw []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing RSA: %w", err)
		}
		return key, nil
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing PKCS#8: %w", err)
		}
		switch key := parsed.(type) {
		case *rsa.PrivateKey:
			return key, nil
		case ed25519.PrivateKey:
			return key, nil
		default:
			return nil, fmt.Errorf("unsupported PKCS#8 key type (use RSA or Ed25519)")
		}
	case "EC PRIVATE KEY":
		return nil, fmt.Errorf("EC keys are not standard DKIM; use RSA or Ed25519")
	default:
		return nil, fmt.Errorf("unsupported PEM block type %q", block.Type)
	}
}

// Sign adds the DKIM-Signature header to msg (which must use CRLF) and
// returns the full signed message.
func Sign(msg []byte, domain, selector string, signer crypto.Signer) ([]byte, error) {
	s, err := dkim.NewSigner(&dkim.SignOptions{
		Domain:   domain,
		Selector: selector,
		Signer:   signer,
		Hash:     crypto.SHA256,
		// nil HeaderKeys = assina todos os cabeçalhos existentes.
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.Write(msg); err != nil {
		return nil, err
	}
	if err := s.Close(); err != nil {
		return nil, err
	}
	header := s.Signature()
	out := make([]byte, 0, len(header)+len(msg))
	out = append(out, header...)
	out = append(out, msg...)
	return out, nil
}
