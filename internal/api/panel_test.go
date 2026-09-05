package api

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"carteiro/internal/config"
	"carteiro/internal/metrics"
)

func TestPanelServesSPAAndProxiesAPI(t *testing.T) {
	store := openTestStore(t)
	cfg := &config.API{Listen: "127.0.0.1:9090", Token: "super-secret-token"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(cfg, store, logger, &metrics.Metrics{}, nil, "test", 0, 0)
	apiTs := httptest.NewServer(srv.Handler())
	t.Cleanup(apiTs.Close)

	panelH := NewPanel("127.0.0.1:8080", apiTs.URL, logger)
	panelTs := httptest.NewServer(panelH.Handler())
	t.Cleanup(panelTs.Close)

	tok := "super-secret-token"

	// The panel serves the SPA shell at "/" and on every React route.
	for _, page := range []string{"/", "/sends", "/accounts", "/compose"} {
		r := do(t, "GET", panelTs.URL+page, "", "")
		if r.StatusCode != 200 {
			t.Fatalf("panel GET %s status = %d, want 200", page, r.StatusCode)
		}
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(strings.ToLower(string(raw)), "<!doctype html") {
			t.Errorf("panel GET %s did not return the shell", page)
		}
	}

	// /api/* is proxied to the admin API listener: public routes work and
	// protected routes still require the token.
	if r := do(t, "GET", panelTs.URL+"/api/health", "", ""); r.StatusCode != 200 {
		t.Errorf("proxied health status = %d", r.StatusCode)
	}
	if r := do(t, "GET", panelTs.URL+"/api/accounts", "", ""); r.StatusCode != 401 {
		t.Errorf("proxied accounts without token status = %d, want 401", r.StatusCode)
	}
	if r := do(t, "GET", panelTs.URL+"/api/sends?limit=5", tok, ""); r.StatusCode != 200 {
		t.Errorf("proxied sends with token status = %d, want 200", r.StatusCode)
	}
	// Unknown /api paths reach the API and 404 there (not the SPA).
	if r := do(t, "GET", panelTs.URL+"/api/nope", tok, ""); r.StatusCode != 404 {
		t.Errorf("proxied unknown api status = %d, want 404", r.StatusCode)
	}

	// The API listener itself has no SPA.
	if r := do(t, "GET", apiTs.URL+"/sends", "", ""); r.StatusCode != 404 {
		t.Errorf("api GET /sends status = %d, want 404", r.StatusCode)
	}
}

func TestAPITargetURL(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:9090": "http://127.0.0.1:9090",
		":9090":          "http://127.0.0.1:9090",
		"0.0.0.0:9091":   "http://127.0.0.1:9091",
		"[::1]:9092":     "http://[::1]:9092",
		"::1:9093":       "http://127.0.0.1:9090", // malformed: falls back to defaults
		"":               "http://127.0.0.1:9090", // malformed: falls back
	}
	for in, want := range cases {
		if got := APITargetURL(in); got != want {
			t.Errorf("APITargetURL(%q) = %q, want %q", in, got, want)
		}
	}
}
