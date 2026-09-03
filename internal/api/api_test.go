package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"carteiro/internal/config"
	"carteiro/internal/metrics"
	"carteiro/internal/storage"
)

func newTestAPI(t *testing.T) *httptest.Server {
	t.Helper()
	store, err := storage.Open("sqlite", t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg := &config.API{Listen: "127.0.0.1:9090", Token: "super-secret-token"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(cfg, store, logger, &metrics.Metrics{})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func do(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestHealthAndOpenAPIArePublic(t *testing.T) {
	ts := newTestAPI(t)
	resp := do(t, "GET", ts.URL+"/health", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("health status = %d", resp.StatusCode)
	}
	doc := do(t, "GET", ts.URL+"/openapi.json", "", "")
	raw, _ := io.ReadAll(doc.Body)
	if doc.StatusCode != 200 {
		t.Fatalf("openapi status = %d", doc.StatusCode)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("openapi.json is not valid JSON: %v", err)
	}
	if spec["openapi"] == nil || spec["paths"] == nil {
		t.Error("openapi.json missing required fields")
	}
	if !strings.Contains(string(raw), "/queue/{id}/retry") {
		t.Error("openapi.json missing queue retry path")
	}
}

func TestAuthRequired(t *testing.T) {
	ts := newTestAPI(t)
	for _, tc := range []struct{ url, token string }{
		{ts.URL + "/accounts", ""},
		{ts.URL + "/accounts", "wrong-token"},
		{ts.URL + "/queue/stats", ""},
	} {
		resp := do(t, "GET", tc.url, tc.token, "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with token %q: status = %d, want 401", tc.url, tc.token, resp.StatusCode)
		}
	}
}

func TestAccountLifecycleOverAPI(t *testing.T) {
	ts := newTestAPI(t)
	tok := "super-secret-token"

	// Create.
	resp := do(t, "POST", ts.URL+"/accounts", tok,
		`{"email":"project-a@example.com","password":"secret-a","allowed_from":["news@example.com"]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}

	// The stored password must be a bcrypt hash, never plain text.
	list := do(t, "GET", ts.URL+"/accounts", tok, "")
	raw, err := io.ReadAll(list.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret-a") {
		t.Error("plain-text password leaked through the API")
	}
	var accounts []map[string]any
	if err := json.Unmarshal(raw, &accounts); err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}

	// Duplicate create = update (200).
	resp2 := do(t, "POST", ts.URL+"/accounts", tok,
		`{"email":"project-a@example.com","password":"secret-a2"}`)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d", resp2.StatusCode)
	}

	// Delete.
	del := do(t, "DELETE", ts.URL+"/accounts/project-a@example.com", tok, "")
	if del.StatusCode != 200 {
		t.Fatalf("delete status = %d", del.StatusCode)
	}
	list2 := do(t, "GET", ts.URL+"/accounts", tok, "")
	raw2, _ := io.ReadAll(list2.Body)
	var after []map[string]any
	if err := json.Unmarshal(raw2, &after); err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Errorf("accounts after delete = %d", len(after))
	}
}

func TestValidationErrors(t *testing.T) {
	ts := newTestAPI(t)
	tok := "super-secret-token"
	bad := []string{
		`{"email":"","password":"x"}`,
		`{"email":"a@example.com","password":""}`,
	}
	for _, body := range bad {
		resp := do(t, "POST", ts.URL+"/accounts", tok, body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestDKIMLifecycleOverAPI(t *testing.T) {
	ts := newTestAPI(t)
	tok := "super-secret-token"

	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	body := `{"domain":"example.com","selector":"mail","key_data":` +
		strconvQuote(string(pemBytes)) + `}`
	resp := do(t, "POST", ts.URL+"/dkim", tok, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create dkim status = %d", resp.StatusCode)
	}

	// Invalid key is rejected without touching the DB.
	bad := do(t, "POST", ts.URL+"/dkim", tok,
		`{"domain":"bad.com","selector":"mail","key_data":"not-a-key"}`)
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid dkim key: status = %d, want 400", bad.StatusCode)
	}

	// Keys are listed without the private material.
	list := do(t, "GET", ts.URL+"/dkim", tok, "")
	rawK, _ := io.ReadAll(list.Body)
	var keys []map[string]string
	if err := json.Unmarshal(rawK, &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0]["domain"] != "example.com" {
		t.Errorf("dkim list wrong: %v", keys)
	}
	if strings.Contains(string(rawK), "PRIVATE") {
		t.Error("private key leaked through the API")
	}
}

func TestQueueStatsAndRetryOverAPI(t *testing.T) {
	ts := newTestAPI(t)
	tok := "super-secret-token"

	// No queue yet.
	st := do(t, "GET", ts.URL+"/queue/stats", tok, "")
	var stats storage.Stats
	if err := json.NewDecoder(st.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if stats.Queued != 0 || stats.Dead != 0 {
		t.Errorf("empty stats wrong: %+v", stats)
	}

	// Requeue of an unknown id -> 404.
	rt := do(t, "POST", ts.URL+"/queue/whatever/retry", tok, "")
	if rt.StatusCode != http.StatusNotFound {
		t.Fatalf("requeue unknown status = %d", rt.StatusCode)
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
