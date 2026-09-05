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
	"carteiro/internal/sends"
	"carteiro/internal/storage"
)

func openTestStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open("sqlite", t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func newTestAPI(t *testing.T) *httptest.Server {
	t.Helper()
	store := openTestStore(t)
	cfg := &config.API{Listen: "127.0.0.1:9090", Token: "super-secret-token"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(cfg, store, logger, &metrics.Metrics{}, nil, "test", 0, 0)
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
		{ts.URL + "/api/accounts", ""},
		{ts.URL + "/api/accounts", "wrong-token"},
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
	resp := do(t, "POST", ts.URL+"/api/accounts", tok,
		`{"email":"project-a@example.com","password":"secret-a","allowed_from":["news@example.com"]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}

	// The stored password must be a bcrypt hash, never plain text.
	list := do(t, "GET", ts.URL+"/api/accounts", tok, "")
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
	resp2 := do(t, "POST", ts.URL+"/api/accounts", tok,
		`{"email":"project-a@example.com","password":"secret-a2"}`)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d", resp2.StatusCode)
	}

	// Delete.
	del := do(t, "DELETE", ts.URL+"/api/accounts/project-a@example.com", tok, "")
	if del.StatusCode != 200 {
		t.Fatalf("delete status = %d", del.StatusCode)
	}
	list2 := do(t, "GET", ts.URL+"/api/accounts", tok, "")
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
		resp := do(t, "POST", ts.URL+"/api/accounts", tok, body)
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

func TestAPIRoutesPublicAndProtected(t *testing.T) {
	ts := newTestAPI(t)
	// Public endpoints exist under /api and at the legacy root.
	for _, path := range []string{"/api/health", "/health", "/api/openapi.json"} {
		if r := do(t, "GET", ts.URL+path, "", ""); r.StatusCode != 200 {
			t.Errorf("GET %s status = %d, want 200", path, r.StatusCode)
		}
	}
	// Protected endpoints require the token; the pre-dashboard /queue/* root
	// alias also keeps working.
	for _, tc := range []string{"/api/stats", "/api/sends", "/api/accounts", "/queue/stats", "/queue"} {
		if r := do(t, "GET", ts.URL+tc, "", ""); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without token: status = %d, want 401", tc, r.StatusCode)
		}
	}
	// The admin API listener is API-only: it does NOT serve the SPA. React
	// pages and unknown paths 404 here (they live on the web panel port).
	for _, page := range []string{"/", "/accounts", "/sends", "/compose", "/some/spa/route"} {
		if r := do(t, "GET", ts.URL+page, "", ""); r.StatusCode != http.StatusNotFound {
			t.Errorf("API GET %s status = %d, want 404 (SPA is on the web port)", page, r.StatusCode)
		}
	}
	// Unknown /api paths 404 as well.
	if r := do(t, "GET", ts.URL+"/api/definitely-not-a-route", "", ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown /api route status = %d, want 404", r.StatusCode)
	}
}

func TestStatsEndpoint(t *testing.T) {
	ts := newTestAPI(t)
	tok := "super-secret-token"
	resp := do(t, "GET", ts.URL+"/api/stats", tok, "")
	if resp.StatusCode != 200 {
		t.Fatalf("stats status = %d", resp.StatusCode)
	}
	var st struct {
		Version       string         `json:"version"`
		UptimeSeconds int64          `json:"uptime_seconds"`
		Counters      map[string]int `json:"counters"`
		Queue         storage.Stats  `json:"queue"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Version != "test" || st.UptimeSeconds < 0 {
		t.Errorf("stats metadata wrong: %+v", st)
	}
	if _, ok := st.Counters["messages_queued_total"]; !ok {
		t.Error("stats missing queued counter")
	}
	if _, ok := st.Counters["messages_delivered_total"]; !ok {
		t.Error("stats missing delivered counter")
	}
}

func TestComposeSendFlow(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.UpsertAccounts([]storage.AccountSeed{
		{Email: "sender@example.com", Password: "pw", AllowedFrom: []string{"news@example.com"}},
	}); err != nil {
		t.Fatal(err)
	}
	rec := sends.New(20, 1<<20)
	cfg := &config.API{Listen: "127.0.0.1:9090", Token: "tok"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(cfg, store, logger, &metrics.Metrics{}, rec, "test", 0, 0)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	tok := "tok"

	// Compose from an allowed alias with a text+html body.
	resp := do(t, "POST", ts.URL+"/api/send", tok,
		`{"from":"news@example.com","to":["a@example.com","b@example.com"],
		  "subject":"Hello <world>","text":"plain part","html":"<p>hi <b>there</b></p>"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("send status = %d body=%s", resp.StatusCode, readAll(t, resp))
	}
	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Status != "queued" {
		t.Errorf("send response wrong: %+v", created)
	}

	// The message lands in the DB queue and in the recorder.
	if got := do(t, "GET", ts.URL+"/api/queue/stats", tok, ""); got.StatusCode != 200 {
		t.Fatalf("queue/stats status = %d", got.StatusCode)
	}
	list := do(t, "GET", ts.URL+"/api/sends", tok, "")
	raw, _ := io.ReadAll(list.Body)
	if !strings.Contains(string(raw), created.ID) {
		t.Errorf("send not listed: %s", raw)
	}

	// Detail returns the parsed renderable bodies.
	det := do(t, "GET", ts.URL+"/api/sends/"+created.ID, tok, "")
	var detail sends.Detail
	if err := json.NewDecoder(det.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	if detail.Subject != "Hello <world>" || !strings.Contains(detail.HTML, "<b>there</b>") ||
		!strings.Contains(detail.Text, "plain part") || !strings.Contains(detail.Raw, "Message-ID") {
		t.Errorf("send detail wrong: %+v", detail)
	}
	if detail.From != "news@example.com" {
		t.Errorf("detail from = %q", detail.From)
	}

	// Senders that no account may use are rejected.
	forbidden := do(t, "POST", ts.URL+"/api/send", tok,
		`{"from":"other@example.com","to":["x@example.com"],"text":"hi"}`)
	if forbidden.StatusCode != http.StatusForbidden {
		t.Errorf("forbidden sender status = %d, want 403", forbidden.StatusCode)
	}
	// Missing recipients are a client error.
	bad := do(t, "POST", ts.URL+"/api/send", tok, `{"from":"news@example.com","text":"hi"}`)
	if bad.StatusCode != http.StatusBadRequest {
		t.Errorf("empty recipients status = %d, want 400", bad.StatusCode)
	}
	// Unknown send id -> 404.
	if r := do(t, "GET", ts.URL+"/api/sends/does-not-exist", tok, ""); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown send status = %d, want 404", r.StatusCode)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpdateAccountOverAPI(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.UpsertAccounts([]storage.AccountSeed{
		{Email: "team@example.com", Password: "old-pass", AllowedFrom: []string{"news@example.com"}},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.API{Listen: "127.0.0.1:9090", Token: "tok"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(cfg, store, logger, &metrics.Metrics{}, nil, "test", 0, 0)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	tok := "tok"

	// Replace the allowed list (keeps the current password).
	patch := do(t, "PATCH", ts.URL+"/api/accounts/team@example.com", tok,
		`{"allowed_from":["news@example.com","alerts@example.com"]}`)
	if patch.StatusCode != 200 {
		t.Fatalf("patch status = %d body=%s", patch.StatusCode, readAll(t, patch))
	}
	acc, ok, err := store.GetAccount("team@example.com")
	if err != nil || !ok {
		t.Fatal(err)
	}
	want := []string{"team@example.com", "news@example.com", "alerts@example.com"}
	if strings.Join(acc.AllowedFrom, ",") != strings.Join(want, ",") {
		t.Errorf("allowed_from after patch = %v, want %v", acc.AllowedFrom, want)
	}
	if !storage.VerifyPassword(acc.PasswordHash, "old-pass") {
		t.Error("password changed although the patch had no password")
	}

	// Change the password too, and verify the new one is in effect.
	p2 := do(t, "PATCH", ts.URL+"/api/accounts/team@example.com", tok,
		`{"password":"new-pass"}`)
	if p2.StatusCode != 200 {
		t.Fatalf("password patch status = %d", p2.StatusCode)
	}
	acc2, _, _ := store.GetAccount("team@example.com")
	if storage.VerifyPassword(acc2.PasswordHash, "new-pass") == false {
		t.Error("new password not in effect")
	}

	// Removing allowed senders: send only the account email.
	p3 := do(t, "PATCH", ts.URL+"/api/accounts/team@example.com", tok,
		`{"allowed_from":[]}`)
	if p3.StatusCode != 200 {
		t.Fatalf("clear allowed patch status = %d", p3.StatusCode)
	}
	acc3, _, _ := store.GetAccount("team@example.com")
	if len(acc3.AllowedFrom) != 1 || acc3.AllowedFrom[0] != "team@example.com" {
		t.Errorf("allowed_from after clear = %v", acc3.AllowedFrom)
	}

	// Unknown account, empty body -> errors.
	if r := do(t, "PATCH", ts.URL+"/api/accounts/nobody@example.com", tok,
		`{"password":"x"}`); r.StatusCode != http.StatusNotFound {
		t.Errorf("patch unknown account: status = %d, want 404", r.StatusCode)
	}
	if r := do(t, "PATCH", ts.URL+"/api/accounts/team@example.com", tok, `{}`); r.StatusCode != http.StatusBadRequest {
		t.Errorf("empty patch: status = %d, want 400", r.StatusCode)
	}
}
