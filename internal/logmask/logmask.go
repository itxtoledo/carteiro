// Package logmask masks e-mail addresses before they reach the logs.
//
// When CARTEIRO_LOG_MASK_EMAILS is enabled the server hides the addresses
// that appear in log lines (authentication, queueing and delivery) so a log
// file can be shared without leaking who the e-mails go to or come from.
//
// Masking rule: the local part keeps only its first character (plus "***"),
// and the domain keeps the public suffix (com, com.br, co.uk, ...) with the
// registrable label masked to its first character; subdomain labels are
// dropped. Examples:
//
//	joao.silva@mx.example.com.br  ->  j***@e***.com.br
//	noreply@example.com           ->  n***@e***.com
//	user@mail.gmail.com           ->  u***@g***.com
package logmask

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
)

// publicSuffixes contains multi-label public suffixes that must stay
// visible. Anything else uses its last label as the visible suffix.
var publicSuffixes = map[string]bool{
	"com.br": true, "net.br": true, "org.br": true, "gov.br": true,
	"edu.br": true, "co.uk": true, "org.uk": true, "ac.uk": true,
	"co.jp": true, "co.nz": true, "com.au": true, "net.au": true,
	"com.mx": true, "com.ar": true, "com.co": true, "com.ve": true,
	"co.za": true, "com.pt": true, "com.uy": true, "com.pe": true,
}

// emailRe matches addresses embedded anywhere in a log message or attribute.
var emailRe = regexp.MustCompile(`[A-Za-z0-9.!#$%&'*+/=?^_{|}~-]+@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+`)

// Email returns the masked form of an e-mail address. Strings that do not
// look like an address (no '@') are returned unchanged so that unrelated log
// values (domains, message ids, hosts) are never altered.
func Email(s string) string {
	at := strings.IndexByte(s, '@')
	if at < 0 {
		return s
	}
	local := s[:at]
	domain := s[at+1:]

	ml := maskLabel(local)
	md := maskDomain(domain)
	return ml + "@" + md
}

// MaskText replaces every e-mail address found anywhere in s with its masked
// form (used for free-form log messages and string attribute values).
func MaskText(s string) string {
	if !strings.Contains(s, "@") {
		return s
	}
	return emailRe.ReplaceAllStringFunc(s, Email)
}

// List masks every e-mail in a slice (used for recipient lists).
func List(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = Email(s)
	}
	return out
}

// maskDomain keeps the public suffix visible and masks the registrable label
// (the one right before the suffix) to its first character; deeper subdomain
// labels are dropped entirely.
func maskDomain(d string) string {
	if d == "" {
		return "***"
	}
	labels := strings.Split(d, ".")
	suffix := 1
	if len(labels) >= 2 {
		lastTwo := labels[len(labels)-2] + "." + labels[len(labels)-1]
		if publicSuffixes[lastTwo] {
			suffix = 2
		}
	}
	// The registrable label is the one immediately before the suffix.
	regIdx := len(labels) - suffix - 1
	if regIdx < 0 {
		// No registrable label (e.g. a bare TLD): mask the suffix itself.
		return maskLabel(d)
	}
	reg := maskLabel(labels[regIdx])
	visible := strings.Join(labels[len(labels)-suffix:], ".")
	return reg + "." + visible
}

// maskLabel keeps the first character of the label followed by "***" when
// there is more than one character, and returns "***" for empty or
// single-character labels (a single char on its own reveals nothing useful).
func maskLabel(l string) string {
	r := []rune(l)
	if len(r) < 2 {
		return "***"
	}
	return string(r[0]) + "***"
}

// maskHandler wraps a slog handler and masks e-mail addresses present in the
// log message and in string/slice attributes before they are emitted.
type maskHandler struct {
	next slog.Handler
}

// NewLogger returns a logger whose output has e-mail addresses masked when
// enabled is true; otherwise it returns the original logger untouched.
func NewLogger(base *slog.Logger, enabled bool) *slog.Logger {
	if !enabled {
		return base
	}
	return slog.New(&maskHandler{next: base.Handler()})
}

func (h *maskHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *maskHandler) Handle(ctx context.Context, r slog.Record) error {
	r2 := slog.NewRecord(r.Time, r.Level, MaskText(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		r2.AddAttrs(maskAttr(a))
		return true
	})
	return h.next.Handle(ctx, r2)
}

func (h *maskHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	masked := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		masked = append(masked, maskAttr(a))
	}
	return &maskHandler{next: h.next.WithAttrs(masked)}
}

func (h *maskHandler) WithGroup(name string) slog.Handler {
	return &maskHandler{next: h.next.WithGroup(name)}
}

// maskAttr recursively masks e-mail addresses in a string, slice or map
// attribute value (and inside nested groups).
func maskAttr(a slog.Attr) slog.Attr {
	v := a.Value
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, MaskText(v.String()))
	case slog.KindAny:
		return slog.Any(a.Key, maskAny(v.Any()))
	case slog.KindGroup:
		child := make([]slog.Attr, 0, len(v.Group()))
		for _, c := range v.Group() {
			child = append(child, maskAttr(c))
		}
		args := make([]any, len(child))
		for i, c := range child {
			args[i] = c
		}
		return slog.Group(a.Key, args...)
	}
	return a
}

func maskAny(x any) any {
	switch t := x.(type) {
	case string:
		return MaskText(t)
	case []string:
		out := make([]string, len(t))
		for i, s := range t {
			out[i] = MaskText(s)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = maskAny(s)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, s := range t {
			out[k] = maskAny(s)
		}
		return out
	}
	return x
}
