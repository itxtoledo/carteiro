// Package api implements the HTTP surface of Carteiro: the administrative
// REST API (bearer token from config/env only, never stored), queue
// monitoring, Prometheus metrics and the embedded web dashboard. Through it,
// accounts, allowed senders and DKIM domains are added and removed over the
// air, and messages can be composed straight into the queue. The API and the
// dashboard share one listener and one origin; the canonical endpoint paths
// live under /api/* (a few pre-dashboard routes keep their legacy root
// alias).
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"carteiro/internal/config"
	"carteiro/internal/dkim"
	"carteiro/internal/metrics"
	"carteiro/internal/sends"
	"carteiro/internal/storage"
	"carteiro/internal/webui"
)

// Server is the API + dashboard server.
type Server struct {
	store          *storage.Store
	token          string
	log            *slog.Logger
	metrics        *metrics.Metrics
	rec            *sends.Recorder
	version        string
	started        time.Time
	maxMessageSize int64
	maxRecipients  int

	handler http.Handler
	http    *http.Server
}

// New creates the API server. rec is the recent-sends recorder (may be nil);
// version is reported by /api/stats; zero limits fall back to the defaults
// (25 MiB / 100 recipients).
func New(cfg *config.API, store *storage.Store, log *slog.Logger, m *metrics.Metrics, rec *sends.Recorder, version string, maxMessageSize int64, maxRecipients int) *Server {
	s := &Server{
		store: store, token: cfg.Token, log: log, metrics: m,
		rec: rec, version: version, started: time.Now().UTC(),
		maxMessageSize: maxMessageSize, maxRecipients: maxRecipients,
	}
	if s.maxMessageSize <= 0 {
		s.maxMessageSize = 25 << 20
	}
	if s.maxRecipients <= 0 {
		s.maxRecipients = 100
	}

	mux := http.NewServeMux()
	// Legacy root aliases (only for paths that do not shadow a React route)
	// plus the canonical /api/* set the dashboard uses, then the SPA
	// fallback: ServeMux gives the specific API routes precedence over "/",
	// so /api/* is never swallowed by the panel.
	s.registerRoutes(mux, "", true)
	s.registerRoutes(mux, "/api", false)
	mux.Handle("/", webui.Handler())

	s.handler = mux
	s.http = &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// route is one API endpoint. Public routes are reachable without a token
// (liveness, metrics, the OpenAPI document); everything else requires the
// bearer token, including every dashboard endpoint.
//
// legacy controls whether the endpoint is also mounted at its root path (no
// prefix). That alias exists only for pre-dashboard routes whose root path is
// NOT a React client route: accounts and the dashboard endpoints are
// /api-only, because their root paths (/accounts, /sends, /sends/{id},
// /stats, /send) are SPA pages and would swallow a direct reload.
type route struct {
	method string
	path   string
	public bool
	legacy bool
	h      http.HandlerFunc
}

// registerRoutes mounts all endpoints under the given path prefix.
func (s *Server) registerRoutes(mux *http.ServeMux, prefix string, legacy bool) {
	routes := []route{
		{method: "GET", path: "/health", public: true, legacy: true, h: s.handleHealth},
		{method: "GET", path: "/metrics", public: true, legacy: true, h: s.handleMetrics},
		{method: "GET", path: "/openapi.json", public: true, legacy: true, h: s.handleOpenAPI},

		{method: "GET", path: "/accounts", h: s.handleListAccounts},
		{method: "POST", path: "/accounts", h: s.handleCreateAccount},
		{method: "PATCH", path: "/accounts/{email}", h: s.handleUpdateAccount},
		{method: "DELETE", path: "/accounts/{email}", h: s.handleDeleteAccount},
		{method: "GET", path: "/dkim", legacy: true, h: s.handleListDKIM},
		{method: "POST", path: "/dkim", legacy: true, h: s.handleCreateDKIM},
		{method: "DELETE", path: "/dkim/{domain}", legacy: true, h: s.handleDeleteDKIM},
		{method: "GET", path: "/queue/stats", legacy: true, h: s.handleQueueStats},
		{method: "GET", path: "/queue", legacy: true, h: s.handleListMessages},
		{method: "POST", path: "/queue/{id}/retry", legacy: true, h: s.handleRequeue},

		// Web dashboard endpoints (/api/* only; their root paths are SPA pages).
		{method: "GET", path: "/stats", h: s.handleStats},
		{method: "GET", path: "/sends", h: s.handleListSends},
		{method: "GET", path: "/sends/{id}", h: s.handleGetSend},
		{method: "POST", path: "/send", h: s.handleSend},
	}
	for _, rt := range routes {
		if legacy && !rt.legacy {
			continue
		}
		h := rt.h
		if !rt.public {
			h = s.requireAuth(rt.h)
		}
		mux.HandleFunc(rt.method+" "+prefix+rt.path, h)
	}
}

// Handler exposes the HTTP routes (used by tests).
func (s *Server) Handler() http.Handler { return s.handler }

// Serve blocks serving until Shutdown is called.
func (s *Server) Serve() error {
	s.log.Info("web panel and admin api listening", "addr", s.http.Addr)
	if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully stops the API server.
func (s *Server) Shutdown(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := s.http.Shutdown(ctx); err != nil {
		s.log.Warn("api shutdown", "err", err)
	}
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || !s.validToken(strings.TrimSpace(token)) {
			writeError(w, http.StatusUnauthorized, "invalid or missing bearer token")
			return
		}
		next(w, r)
	}
}

func (s *Server) validToken(given string) bool {
	return subtle.ConstantTimeCompare([]byte(s.token), []byte(given)) == 1
}

// --- system handlers ---------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	st, err := s.store.Stats(time.Now())
	if err == nil {
		fmt.Fprintf(w, "# HELP carteiro_queue_queued Messages currently queued.\n# TYPE carteiro_queue_queued gauge\ncarteiro_queue_queued %d\n", st.Queued)
		fmt.Fprintf(w, "# HELP carteiro_queue_due Messages currently due for delivery.\n# TYPE carteiro_queue_due gauge\ncarteiro_queue_due %d\n", st.Due)
		fmt.Fprintf(w, "# HELP carteiro_queue_dead Messages in dead-letter.\n# TYPE carteiro_queue_dead gauge\ncarteiro_queue_dead %d\n", st.Dead)
	}
	s.metrics.WritePrometheus(w)
}

// statsResponse is the dashboard summary: process counters, queue gauges and
// process metadata in one JSON document.
type statsResponse struct {
	Version       string         `json:"version"`
	UptimeSeconds int64          `json:"uptime_seconds"`
	Counters      map[string]int `json:"counters"`
	Queue         storage.Stats  `json:"queue"`
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.Stats(time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, statsResponse{
		Version:       s.version,
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
		Counters: map[string]int{
			"auth_success_total":       int(s.metrics.AuthSuccess.Load()),
			"auth_failure_total":       int(s.metrics.AuthFailure.Load()),
			"messages_queued_total":    int(s.metrics.MessagesQueued.Load()),
			"delivery_attempts_total":  int(s.metrics.DeliveryAttempts.Load()),
			"messages_delivered_total": int(s.metrics.MessagesDelivered.Load()),
			"messages_dead_total":      int(s.metrics.MessagesDead.Load()),
			"messages_requeued_total":  int(s.metrics.Requeued.Load()),
		},
		Queue: st,
	})
}

// --- accounts ----------------------------------------------------------------

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := s.store.ListAccounts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type view struct {
		Email       string   `json:"email"`
		AllowedFrom []string `json:"allowed_from"`
	}
	out := make([]view, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, view{Email: a.Email, AllowedFrom: a.AllowedFrom})
	}
	writeJSON(w, http.StatusOK, out)
}

type createAccountReq struct {
	Email       string   `json:"email"`
	Password    string   `json:"password"`
	AllowedFrom []string `json:"allowed_from,omitempty"`
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var req createAccountReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if req.Email == "" || !strings.Contains(req.Email, "@") {
		writeError(w, http.StatusBadRequest, "email must be a valid address")
		return
	}
	if req.Password == "" {
		writeError(w, http.StatusBadRequest, "password must not be empty")
		return
	}
	sum, err := s.store.UpsertAccounts([]storage.AccountSeed{
		{Email: req.Email, Password: req.Password, AllowedFrom: req.AllowedFrom},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	created := len(sum.Created) == 1
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	s.log.Info("api: account upserted", "email", req.Email, "created", created)
	writeJSON(w, code, map[string]any{
		"email": req.Email, "created": created,
		"note": "password hashed with bcrypt before storage",
	})
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.PathValue("email")))
	if err := s.store.DeleteAccount(email); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("api: account deleted", "email", email)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": email})
}

// updateAccountReq edits an existing account: allowed_from replaces the whole
// list (omitting it keeps the current one); password sets a new one (omitting
// it or sending "" keeps the current hash).
type updateAccountReq struct {
	AllowedFrom *[]string `json:"allowed_from"`
	Password    *string   `json:"password,omitempty"`
}

func (s *Server) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.PathValue("email")))

	var req updateAccountReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	acc, ok, err := s.store.GetAccount(email)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}

	newPassword := ""
	if req.Password != nil {
		newPassword = *req.Password
	}
	if req.AllowedFrom == nil && newPassword == "" {
		writeError(w, http.StatusBadRequest, "nothing to update (send allowed_from, a new password, or both)")
		return
	}

	allowed := acc.AllowedFrom
	if req.AllowedFrom != nil {
		allowed = *req.AllowedFrom
	}
	for _, f := range allowed {
		if strings.TrimSpace(f) == "" {
			writeError(w, http.StatusBadRequest, "allowed_from must not contain empty addresses")
			return
		}
	}
	if err := s.store.UpdateAccount(email, allowed, newPassword); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("api: account updated", "email", email, "password_changed", newPassword != "")
	writeJSON(w, http.StatusOK, map[string]any{
		"email": email, "updated": true,
		"allowed_from": allowed, "password_changed": newPassword != "",
	})
}

// --- dkim --------------------------------------------------------------------

func (s *Server) handleListDKIM(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListDKIM()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type view struct {
		Domain   string `json:"domain"`
		Selector string `json:"selector"`
	}
	out := make([]view, 0, len(keys))
	for _, k := range keys {
		out = append(out, view{Domain: k.Domain, Selector: k.Selector})
	}
	writeJSON(w, http.StatusOK, out)
}

type createDKIMReq struct {
	Domain   string `json:"domain"`
	Selector string `json:"selector"`
	KeyData  string `json:"key_data"`
}

func (s *Server) handleCreateDKIM(w http.ResponseWriter, r *http.Request) {
	var req createDKIMReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	req.Domain = strings.ToLower(strings.TrimSpace(req.Domain))
	req.Selector = strings.TrimSpace(req.Selector)
	if req.Domain == "" || req.Selector == "" {
		writeError(w, http.StatusBadRequest, "domain and selector are required")
		return
	}
	if _, err := dkim.ParseSigner([]byte(req.KeyData)); err != nil {
		writeError(w, http.StatusBadRequest, "key_data is not a valid PEM private key: "+err.Error())
		return
	}
	sum, err := s.store.UpsertDKIM([]storage.DKIMKey{
		{Domain: req.Domain, Selector: req.Selector, KeyData: req.KeyData},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	created := len(sum.Created) == 1
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	s.log.Info("api: dkim upserted", "domain", req.Domain, "created", created)
	writeJSON(w, code, map[string]any{
		"domain": req.Domain, "selector": req.Selector, "created": created,
		"dns": fmt.Sprintf("%s._domainkey.%s TXT \"v=DKIM1; k=rsa; p=<public key>\"", req.Selector, req.Domain),
	})
}

func (s *Server) handleDeleteDKIM(w http.ResponseWriter, r *http.Request) {
	domain := strings.ToLower(strings.TrimSpace(r.PathValue("domain")))
	if err := s.store.DeleteDKIM(domain); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("api: dkim deleted", "domain", domain)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": domain})
}

// --- queue -------------------------------------------------------------------

func (s *Server) handleQueueStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.Stats(time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	status := strings.ToLower(r.URL.Query().Get("status"))
	switch status {
	case "", storage.StatusQueued:
		status = storage.StatusQueued
	case storage.StatusDead:
	default:
		writeError(w, http.StatusBadRequest, "status must be queued or dead")
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}
	msgs, err := s.store.ListMessages(status, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if msgs == nil {
		msgs = []storage.MessageLite{}
	}
	writeJSON(w, http.StatusOK, msgs)
}

func (s *Server) handleRequeue(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if err := s.store.RequeueDead(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	s.metrics.Requeued.Add(1)
	if s.rec != nil {
		s.rec.MarkQueued(id)
	}
	s.log.Info("api: dead message requeued", "id", id)
	writeJSON(w, http.StatusOK, map[string]any{"requeued": id})
}

// --- dashboard: recent sends -------------------------------------------------

func (s *Server) handleListSends(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}
	if s.rec == nil {
		writeJSON(w, http.StatusOK, []sends.Summary{})
		return
	}
	writeJSON(w, http.StatusOK, s.rec.List(limit))
}

func (s *Server) handleGetSend(w http.ResponseWriter, r *http.Request) {
	if s.rec == nil {
		writeError(w, http.StatusNotFound, "send tracking is disabled")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	d, ok := s.rec.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "send not found (it may have been evicted from the ring buffer)")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// --- dashboard: compose ------------------------------------------------------

type sendReq struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	Text    string   `json:"text"`
	HTML    string   `json:"html"`
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var req sendReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	req.From = strings.ToLower(strings.TrimSpace(req.From))
	if !strings.Contains(req.From, "@") {
		writeError(w, http.StatusBadRequest, "from must be a valid sender address")
		return
	}
	if !s.senderAllowed(req.From) {
		writeError(w, http.StatusForbidden, "sender not allowed by any account")
		return
	}

	seen := map[string]bool{}
	to := make([]string, 0, len(req.To))
	for _, a := range req.To {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if !strings.Contains(a, "@") {
			writeError(w, http.StatusBadRequest, "invalid recipient address: "+a)
			return
		}
		if !seen[a] {
			seen[a] = true
			to = append(to, a)
		}
	}
	if len(to) == 0 {
		writeError(w, http.StatusBadRequest, "at least one recipient is required")
		return
	}
	if len(to) > s.maxRecipients {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("message exceeds the limit of %d recipients", s.maxRecipients))
		return
	}
	if strings.ContainsAny(req.Subject, "\r\n") {
		writeError(w, http.StatusBadRequest, "subject must not contain line breaks")
		return
	}

	id := storage.NewID(time.Now())
	msg, err := sends.BuildMessage(id, req.From, to, req.Subject, req.Text, req.HTML)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if int64(len(msg)) > s.maxMessageSize {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("message exceeds the limit of %d bytes", s.maxMessageSize))
		return
	}

	if s.rec != nil {
		s.rec.Add(id, req.From, to, msg)
	}
	qid, err := s.store.EnqueueWithID(id, req.From, to, msg)
	if err != nil {
		if s.rec != nil {
			s.rec.Drop(id)
		}
		s.log.Error("api: failed to queue composed message", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to queue the message")
		return
	}
	s.metrics.MessagesQueued.Add(1)
	s.log.Info("api: message composed and queued", "id", qid, "from", req.From, "to", to, "bytes", len(msg))
	writeJSON(w, http.StatusCreated, map[string]any{"id": qid, "status": sends.StatusQueued})
}

// senderAllowed reports whether any account may use the envelope sender.
func (s *Server) senderAllowed(from string) bool {
	accs, err := s.store.ListAccounts()
	if err != nil {
		s.log.Error("api: listing accounts for sender check failed", "err", err)
		return false
	}
	for _, a := range accs {
		if a.AllowsFrom(from) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
