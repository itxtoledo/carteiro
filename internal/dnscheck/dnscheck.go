// Package dnscheck inspects the public DNS records of a sending domain
// (SPF, DKIM, DMARC, MX and reverse DNS) and reports whether they are
// configured correctly for outbound mail, so the dashboard can warn about
// setups that would land in spam. Dependencies are standard library only.
package dnscheck

import (
	"context"
	"fmt"
	"net"
	"strings"

	"carteiro/internal/dkim"
)

// Resolver is the subset of net.Resolver the checks need. It is an interface
// so tests can supply canned answers; production passes net.DefaultResolver.
type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
	LookupAddr(ctx context.Context, addr string) ([]string, error)
}

// Status grades a single check.
type Status string

const (
	// StatusPass means the record is present and correct.
	StatusPass Status = "pass"
	// StatusWarn means something is present but suboptimal, or absent while
	// not strictly required.
	StatusWarn Status = "warn"
	// StatusFail means the setup is broken and mail is likely to be flagged.
	StatusFail Status = "fail"
	// StatusInfo means the check could not be evaluated (nothing to do).
	StatusInfo Status = "info"
)

// Result is one DNS diagnostic with a human-readable verdict.
type Result struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Status   Status   `json:"status"`
	Message  string   `json:"message"`
	Found    []string `json:"found,omitempty"`
	Expected string   `json:"expected,omitempty"`
}

// Report aggregates every check for one sending domain.
type Report struct {
	Domain   string   `json:"domain"`
	Selector string   `json:"selector,omitempty"`
	Hostname string   `json:"hostname,omitempty"`
	OK       bool     `json:"ok"`
	Passed   int      `json:"passed"`
	Warnings int      `json:"warnings"`
	Failed   int      `json:"failed"`
	Checks   []Result `json:"checks"`
}

// Request describes what to inspect. PrivateKeyPEM is the PEM text of the
// DKIM private key stored in Carteiro (empty skips the key comparison);
// Hostname enables the reverse-DNS (PTR) check.
type Request struct {
	Domain        string
	Selector      string
	PrivateKeyPEM string
	Hostname      string
}

// Check runs every diagnostic against the resolver and returns the report.
// A nil resolver falls back to the system resolver.
func Check(ctx context.Context, r Resolver, req Request) Report {
	if r == nil {
		r = net.DefaultResolver
	}
	domain := normalizeDomain(req.Domain)
	rep := Report{
		Domain:   domain,
		Selector: strings.TrimSpace(req.Selector),
		Hostname: strings.TrimSpace(req.Hostname),
	}
	rep.Checks = append(rep.Checks,
		checkSPF(ctx, r, domain),
		checkDKIM(ctx, r, domain, rep.Selector, req.PrivateKeyPEM),
		checkDMARC(ctx, r, domain),
		checkMX(ctx, r, domain),
		checkReverse(ctx, r, rep.Hostname),
	)
	for _, c := range rep.Checks {
		switch c.Status {
		case StatusPass:
			rep.Passed++
		case StatusWarn:
			rep.Warnings++
		case StatusFail:
			rep.Failed++
		}
	}
	rep.OK = rep.Failed == 0
	return rep
}

func checkSPF(ctx context.Context, r Resolver, domain string) Result {
	c := Result{Name: "spf", Label: "SPF", Status: StatusFail}
	recs, err := r.LookupTXT(ctx, domain)
	if err != nil {
		c.Status = StatusWarn
		c.Message = "cannot query TXT for " + domain + ": " + err.Error()
		return c
	}
	var spf []string
	for _, rec := range recs {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(rec)), "v=spf1") {
			spf = append(spf, strings.TrimSpace(rec))
		}
	}
	switch len(spf) {
	case 0:
		c.Message = "no SPF record on the sending domain; publish one authorizing this server"
		c.Expected = "v=spf1 ip4:<server-ip> -all"
		return c
	case 1:
		c.Found = spf
		qualifier, lookups := spfTerms(spf[0])
		switch qualifier {
		case "-all":
			c.Status = StatusPass
			c.Message = "SPF record found with a hard fail ('-all')"
		case "~all":
			c.Status = StatusWarn
			c.Message = "SPF ends with '~all' (soft fail); switch to '-all' once you confirm the record is right"
		case "+all":
			c.Status = StatusFail
			c.Message = "SPF ends with '+all' (or a bare 'all'), authorizing the whole internet; use '-all'"
		case "?all":
			c.Status = StatusFail
			c.Message = "SPF ends with '?all' (neutral), which gives no protection; use '-all'"
		default:
			c.Status = StatusWarn
			c.Message = "SPF record has no 'all' mechanism, so unlisted senders are neither authorized nor rejected"
		}
		if lookups > 10 {
			c.Status = StatusFail
			c.Message = fmt.Sprintf("SPF uses %d DNS lookups; the limit is 10, so receivers treat it as a permanent error", lookups)
		}
		return c
	default:
		c.Status = StatusFail
		c.Found = spf
		c.Message = "multiple v=spf1 records on the same name; receivers treat this as a permanent error (merge them into one)"
		return c
	}
}

// spfTerms returns the trailing "all" qualifier and the number of DNS-lookup
// mechanisms (a, mx, ptr, include, exists, redirect), per RFC 7208.
func spfTerms(record string) (qualifier string, lookups int) {
	for _, f := range strings.Fields(record) {
		tok := strings.ToLower(f)
		name := strings.TrimLeft(tok, "+-~?")
		if name == "all" {
			q := strings.TrimSuffix(tok, "all")
			if q == "" {
				q = "+"
			}
			qualifier = q + "all"
			continue
		}
		switch {
		case name == "a" || strings.HasPrefix(name, "a:") || strings.HasPrefix(name, "a/"),
			name == "mx" || strings.HasPrefix(name, "mx:"),
			name == "ptr" || strings.HasPrefix(name, "ptr:"),
			strings.HasPrefix(name, "include:"),
			strings.HasPrefix(name, "exists:"),
			strings.HasPrefix(name, "redirect="):
			lookups++
		}
	}
	return qualifier, lookups
}

func checkDKIM(ctx context.Context, r Resolver, domain, selector, keyPEM string) Result {
	c := Result{Name: "dkim", Label: "DKIM", Status: StatusFail}
	if selector == "" {
		c.Status = StatusInfo
		c.Message = "no selector for this domain; add a DKIM key so outbound mail is signed"
		return c
	}
	name := selector + "._domainkey." + domain

	expected := ""
	if strings.TrimSpace(keyPEM) != "" {
		signer, err := dkim.ParseSigner([]byte(keyPEM))
		if err != nil {
			c.Status = StatusWarn
			c.Message = "the DKIM private key stored in Carteiro is invalid: " + err.Error()
			return c
		}
		rec, err := dkim.PublicRecord(signer)
		if err != nil {
			c.Status = StatusWarn
			c.Message = "cannot derive the public key: " + err.Error()
			return c
		}
		expected = rec
		c.Expected = rec
	}

	recs, err := r.LookupTXT(ctx, name)
	if err != nil {
		c.Message = "no DKIM record at " + name + "; publish the record below"
		return c
	}
	found := ""
	for _, rec := range recs {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(rec)), "V=DKIM1") || strings.Contains(rec, "p=") {
			found = strings.TrimSpace(rec)
			break
		}
	}
	if found == "" {
		c.Message = "no DKIM record at " + name + "; publish the record below"
		return c
	}
	c.Found = []string{found}

	if expected == "" {
		c.Status = StatusWarn
		c.Message = "DKIM record found, but there is no private key stored in Carteiro to compare it with"
		return c
	}
	dnsKey := tagValue(found, "p")
	if dnsKey == "" {
		c.Message = "the DKIM record has an empty p= (the key is revoked)"
		return c
	}
	if normalizeB64(dnsKey) != normalizeB64(tagValue(expected, "p")) {
		c.Message = "the p= published in DNS does not match the private key stored in Carteiro; re-publish the record below"
		return c
	}
	c.Status = StatusPass
	c.Message = "the DKIM record matches the private key stored in Carteiro"
	return c
}

func checkDMARC(ctx context.Context, r Resolver, domain string) Result {
	c := Result{Name: "dmarc", Label: "DMARC", Status: StatusFail}
	name := "_dmarc." + domain
	recs, err := r.LookupTXT(ctx, name)
	if err != nil {
		c.Message = "no DMARC record at " + name + "; publish one so receivers know how to treat failures"
		c.Expected = "v=DMARC1; p=none; rua=mailto:postmaster@" + domain
		return c
	}
	found := ""
	for _, rec := range recs {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(rec)), "V=DMARC1") {
			found = strings.TrimSpace(rec)
			break
		}
	}
	if found == "" {
		c.Message = "no DMARC record at " + name + "; publish one so receivers know how to treat failures"
		c.Expected = "v=DMARC1; p=none; rua=mailto:postmaster@" + domain
		return c
	}
	c.Found = []string{found}
	policy := strings.ToLower(tagValue(found, "p"))
	switch policy {
	case "reject":
		c.Status = StatusPass
		c.Message = "DMARC policy is p=reject (full enforcement)"
	case "quarantine":
		c.Status = StatusPass
		c.Message = "DMARC policy is p=quarantine (failing mail goes to spam)"
	default:
		c.Status = StatusWarn
		c.Message = "DMARC policy is none/missing (monitoring only); move to quarantine or reject once reports look clean"
	}
	return c
}

func checkMX(ctx context.Context, r Resolver, domain string) Result {
	c := Result{Name: "mx", Label: "MX (bounces)", Status: StatusWarn}
	mxs, err := r.LookupMX(ctx, domain)
	if err != nil || len(mxs) == 0 {
		c.Message = "no MX records for the sending domain; mail still sends, but bounces and DMARC reports cannot be delivered"
		return c
	}
	c.Status = StatusPass
	c.Message = fmt.Sprintf("%d MX record(s); bounces and DMARC reports can be delivered", len(mxs))
	for _, mx := range mxs {
		host := strings.TrimSuffix(mx.Host, ".")
		c.Found = append(c.Found, fmt.Sprintf("%d %s", mx.Pref, host))
	}
	return c
}

func checkReverse(ctx context.Context, r Resolver, hostname string) Result {
	c := Result{Name: "ptr", Label: "Reverse DNS (PTR)", Status: StatusInfo}
	if hostname == "" {
		c.Message = "hostname is not configured; set 'hostname' to enable the PTR check"
		return c
	}
	c.Expected = hostname
	ips, err := r.LookupHost(ctx, hostname)
	if err != nil || len(ips) == 0 {
		c.Status = StatusWarn
		c.Message = "cannot resolve the configured hostname " + hostname
		return c
	}
	matched := false
	for _, ip := range ips {
		names, err := r.LookupAddr(ctx, ip)
		if err != nil {
			continue
		}
		for _, n := range names {
			n = strings.TrimSuffix(n, ".")
			c.Found = append(c.Found, n)
			if strings.EqualFold(n, hostname) {
				matched = true
			}
		}
	}
	switch {
	case len(c.Found) == 0:
		c.Status = StatusWarn
		c.Message = "no PTR record for " + ips[0] + "; set reverse DNS to " + hostname + " at the IP provider"
	case matched:
		c.Status = StatusPass
		c.Message = "PTR matches the configured hostname (forward-confirmed reverse DNS)"
	default:
		c.Status = StatusWarn
		c.Message = "the PTR does not point back to " + hostname + "; receivers treat this as a spam signal"
	}
	return c
}

// tagValue extracts a "k=v" tag from a DKIM/DMARC-style record.
func tagValue(record, key string) string {
	for _, part := range strings.Split(record, ";") {
		k, v, ok := strings.Cut(part, "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), key) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// normalizeB64 strips all whitespace so a value that DNS split across quoted
// chunks still compares equal to the single-line form.
func normalizeB64(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
}

func normalizeDomain(d string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d), "."))
}
