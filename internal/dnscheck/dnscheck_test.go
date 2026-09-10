package dnscheck

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"strings"
	"testing"

	"carteiro/internal/dkim"
)

// fakeResolver answers from canned maps; an unknown name returns an error,
// mimicking NXDOMAIN.
type fakeResolver struct {
	txt  map[string][]string
	mx   map[string][]*net.MX
	host map[string][]string
	addr map[string][]string
}

func (f fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	return f.txt[name], nil
}

func (f fakeResolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	return f.mx[name], nil
}

func (f fakeResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if v, ok := f.host[host]; ok {
		return v, nil
	}
	return nil, errors.New("no such host")
}

func (f fakeResolver) LookupAddr(_ context.Context, addr string) ([]string, error) {
	return f.addr[addr], nil
}

func testKeyPEM(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemText := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	record, err := dkim.PublicRecord(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pemText), record
}

func find(t *testing.T, rep Report, name string) Result {
	t.Helper()
	for _, c := range rep.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q not present", name)
	return Result{}
}

func TestCheckAllGreen(t *testing.T) {
	pemText, dkimRecord := testKeyPEM(t)
	r := fakeResolver{
		txt: map[string][]string{
			"example.com":                 {"v=spf1 ip4:203.0.113.10 -all"},
			"mail._domainkey.example.com": {dkimRecord},
			"_dmarc.example.com":          {"v=DMARC1; p=reject; rua=mailto:dmarc@example.com"},
		},
		mx:   map[string][]*net.MX{"example.com": {{Host: "mail.example.com.", Pref: 10}}},
		host: map[string][]string{"smtp.example.com": {"203.0.113.10"}},
		addr: map[string][]string{"203.0.113.10": {"smtp.example.com."}},
	}
	rep := Check(context.Background(), r, Request{
		Domain: "example.com", Selector: "mail", PrivateKeyPEM: pemText, Hostname: "smtp.example.com",
	})
	if !rep.OK || rep.Failed != 0 {
		t.Fatalf("expected an OK report, got %+v", rep)
	}
	if rep.Passed != 5 {
		t.Errorf("passed = %d, want 5", rep.Passed)
	}
	for _, c := range rep.Checks {
		if c.Status != StatusPass {
			t.Errorf("check %s status = %s (%s)", c.Name, c.Status, c.Message)
		}
	}
}

func TestCheckBrokenSetup(t *testing.T) {
	_, otherRecord := testKeyPEM(t)
	pemText, _ := testKeyPEM(t)
	r := fakeResolver{
		txt: map[string][]string{
			"mail._domainkey.example.com": {otherRecord},
		},
		host: map[string][]string{"smtp.example.com": {"203.0.113.10"}},
		addr: map[string][]string{"203.0.113.10": {"other.example.net."}},
	}
	rep := Check(context.Background(), r, Request{
		Domain: "example.com", Selector: "mail", PrivateKeyPEM: pemText, Hostname: "smtp.example.com",
	})
	if rep.OK || rep.Failed == 0 {
		t.Fatalf("expected failures, got %+v", rep)
	}
	if got := find(t, rep, "spf"); got.Status != StatusFail {
		t.Errorf("spf status = %s, want fail", got.Status)
	}
	if got := find(t, rep, "dkim"); got.Status != StatusFail || !strings.Contains(got.Message, "does not match") {
		t.Errorf("dkim = %s (%s)", got.Status, got.Message)
	}
	if got := find(t, rep, "dmarc"); got.Status != StatusFail || got.Expected == "" {
		t.Errorf("dmarc = %s, expected a suggested record", got.Status)
	}
	if got := find(t, rep, "mx"); got.Status != StatusWarn {
		t.Errorf("mx status = %s, want warn", got.Status)
	}
	if got := find(t, rep, "ptr"); got.Status != StatusWarn {
		t.Errorf("ptr status = %s, want warn", got.Status)
	}
}

func TestCheckSPFPolicies(t *testing.T) {
	cases := []struct {
		record string
		want   Status
	}{
		{"v=spf1 include:_spf.example.com -all", StatusPass},
		{"v=spf1 ip4:203.0.113.10 ~all", StatusWarn},
		{"v=spf1 +all", StatusFail},
		{"v=spf1 ?all", StatusFail},
		{"v=spf1 ip4:203.0.113.10", StatusWarn},
	}
	for _, tc := range cases {
		r := fakeResolver{txt: map[string][]string{"example.com": {tc.record}}}
		rep := Check(context.Background(), r, Request{Domain: "example.com"})
		if got := find(t, rep, "spf").Status; got != tc.want {
			t.Errorf("%q: status = %s, want %s", tc.record, got, tc.want)
		}
	}
}

func TestCheckSPFMultipleRecords(t *testing.T) {
	r := fakeResolver{txt: map[string][]string{
		"example.com": {"v=spf1 ip4:203.0.113.10 -all", "v=spf1 include:other.com -all"},
	}}
	rep := Check(context.Background(), r, Request{Domain: "example.com"})
	got := find(t, rep, "spf")
	if got.Status != StatusFail || len(got.Found) != 2 {
		t.Fatalf("spf = %s (found %d), want fail with 2 records", got.Status, len(got.Found))
	}
}

func TestCheckSPFTooManyLookups(t *testing.T) {
	rec := "v=spf1 " + strings.Repeat("include:a.example.com ", 11) + "-all"
	r := fakeResolver{txt: map[string][]string{"example.com": {rec}}}
	rep := Check(context.Background(), r, Request{Domain: "example.com"})
	if got := find(t, rep, "spf"); got.Status != StatusFail {
		t.Errorf("spf status = %s, want fail for >10 lookups", got.Status)
	}
}

func TestCheckDKIMSplitsChunks(t *testing.T) {
	// A record whose p= contains whitespace must still compare equal.
	pemText, dkimRecord := testKeyPEM(t)
	spaced := strings.Replace(dkimRecord, "p=", "p= ", 1)
	r := fakeResolver{
		txt: map[string][]string{
			"mail._domainkey.example.com": {spaced},
			"example.com":                 {"v=spf1 -all"},
			"_dmarc.example.com":          {"v=DMARC1; p=quarantine"},
		},
	}
	rep := Check(context.Background(), r, Request{Domain: "example.com", Selector: "mail", PrivateKeyPEM: pemText})
	if got := find(t, rep, "dkim"); got.Status != StatusPass {
		t.Errorf("dkim status = %s (%s), want pass", got.Status, got.Message)
	}
}

func TestCheckDKIMWithoutKey(t *testing.T) {
	r := fakeResolver{txt: map[string][]string{
		"mail._domainkey.example.com": {"v=DKIM1; k=rsa; p=abc"},
	}}
	rep := Check(context.Background(), r, Request{Domain: "example.com", Selector: "mail"})
	got := find(t, rep, "dkim")
	if got.Status != StatusWarn {
		t.Errorf("dkim status = %s, want warn when no private key is stored", got.Status)
	}
}

func TestCheckDKIMNoSelector(t *testing.T) {
	rep := Check(context.Background(), fakeResolver{}, Request{Domain: "example.com"})
	if got := find(t, rep, "dkim"); got.Status != StatusInfo {
		t.Errorf("dkim status = %s, want info when no selector is configured", got.Status)
	}
}

func TestCheckDMARCPolicy(t *testing.T) {
	cases := []struct {
		record string
		want   Status
	}{
		{"v=DMARC1; p=reject", StatusPass},
		{"v=DMARC1; p=quarantine", StatusPass},
		{"v=DMARC1; p=none", StatusWarn},
	}
	for _, tc := range cases {
		r := fakeResolver{txt: map[string][]string{"_dmarc.example.com": {tc.record}}}
		rep := Check(context.Background(), r, Request{Domain: "example.com"})
		if got := find(t, rep, "dmarc").Status; got != tc.want {
			t.Errorf("%q: status = %s, want %s", tc.record, got, tc.want)
		}
	}
}

func TestCheckHostnameNotConfigured(t *testing.T) {
	rep := Check(context.Background(), fakeResolver{}, Request{Domain: "example.com"})
	if got := find(t, rep, "ptr"); got.Status != StatusInfo {
		t.Errorf("ptr status = %s, want info without a hostname", got.Status)
	}
}
