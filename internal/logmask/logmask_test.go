package logmask

import (
	"log/slog"
	"strings"
	"testing"
)

func TestEmail(t *testing.T) {
	cases := []struct{ in, want string }{
		{"joao.silva@mx.example.com.br", "j***@e***.com.br"},
		{"noreply@example.com", "n***@e***.com"},
		{"user@mail.gmail.com", "u***@g***.com"},
		{"ana@example.com.br", "a***@e***.com.br"},
		{"abc@example.co.uk", "a***@e***.co.uk"},
		{"a@b.io", "***@***.io"},
		{"not-an-email", "not-an-email"},         // no @ -> unchanged
		{"host.example.com", "host.example.com"}, // domains stay untouched
		{"", ""},
	}
	for _, c := range cases {
		if got := Email(c.in); got != c.want {
			t.Errorf("Email(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEmailNeverLeaksFullAddress(t *testing.T) {
	addrs := []string{
		"joao.silva@mx.example.com.br",
		"contato@exemplo.com.br",
		"alguem@sub.gmail.com",
		"fulano@deep.sub.domain.co.uk",
	}
	for _, a := range addrs {
		m := Email(a)
		if strings.Contains(m, a) {
			t.Errorf("masked %q still contains the full address: %q", a, m)
		}
		// The full local part (after its first char) must not appear.
		local := strings.SplitN(a, "@", 2)[0]
		if len(local) > 1 && strings.Contains(m, local[1:]) {
			t.Errorf("masked %q leaks the local part tail %q", a, local[1:])
		}
	}
}

func TestList(t *testing.T) {
	in := []string{"ana@example.com", "bia@example.com"}
	out := List(in)
	if len(out) != 2 || out[0] != "a***@e***.com" || out[1] != "b***@e***.com" {
		t.Errorf("List(%v) = %v", in, out)
	}
	if List(nil) != nil {
		t.Error("List(nil) should stay nil")
	}
}

func TestMaskText(t *testing.T) {
	in := "delivering to contato@exemplo.com.br from no-reply@example.com"
	want := "delivering to c***@e***.com.br from n***@e***.com"
	if got := MaskText(in); got != want {
		t.Errorf("MaskText = %q, want %q", got, want)
	}
	if MaskText("sem emails aqui") != "sem emails aqui" {
		t.Error("MaskText changed a string without emails")
	}
}

func TestNewLoggerMasksOutput(t *testing.T) {
	var buf strings.Builder
	base := slog.New(slog.NewTextHandler(&buf, nil))
	masked := NewLogger(base, true)
	masked.Info("auth ok", "user", "joao@example.com", "to", []string{"ana@mx.example.com.br", "bia@example.com"}, "plain", "value")

	out := buf.String()
	for _, leak := range []string{"joao@example.com", "ana@mx.example.com.br", "bia@example.com"} {
		if strings.Contains(out, leak) {
			t.Errorf("output leaks %q:\n%s", leak, out)
		}
	}
	for _, want := range []string{"j***@e***.com", "a***@e***.com.br", "b***@e***.com", "plain=value"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestNewLoggerDisabledIsOriginal(t *testing.T) {
	var buf strings.Builder
	base := slog.New(slog.NewTextHandler(&buf, nil))
	if NewLogger(base, false) != base {
		t.Error("NewLogger(..., false) should return the original logger")
	}
}
