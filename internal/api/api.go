// Package api implements the administrative REST API (bearer tokens from
// config/env only, never stored) plus queue monitoring and Prometheus
// metrics. Through it, accounts, allowed senders and DKIM domains are added
// and removed over the air, straight into the database.
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
	"carteiro/internal/storage"
)

// Server is the admin API.
type Server struct {
	store   *storage.Store
	token   string
	log     *slog.Logger
	metrics *metrics.Metrics
	handler http.Handler
	http    *http.Server
}

// New creates the API server.
func New(cfg *config.API, store *storage.Store, log *slog.Logger, m *metrics.Metrics) *Server {
	s := &Server{store: store, token: cfg.Token, log: log, metrics: m}
	mux := http.NewServeMux()

	// Health, metrics and the OpenAPI document are public; everything else
	// requires the bearer token.
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /openapi.json", s.handleOpenAPI)
	mux.HandleFunc("GET /accounts", s.requireAuth(s.handleListAccounts))
	mux.HandleFunc("POST /accounts", s.requireAuth(s.handleCreateAccount))
	mux.HandleFunc("DELETE /accounts/{email}", s.requireAuth(s.handleDeleteAccount))
	mux.HandleFunc("GET /dkim", s.requireAuth(s.handleListDKIM))
	mux.HandleFunc("POST /dkim", s.requireAuth(s.handleCreateDKIM))
	mux.HandleFunc("DELETE /dkim/{domain}", s.requireAuth(s.handleDeleteDKIM))
	mux.HandleFunc("GET /queue/stats", s.requireAuth(s.handleQueueStats))
	mux.HandleFunc("GET /queue", s.requireAuth(s.handleListMessages))
	mux.HandleFunc("POST /queue/{id}/retry", s.requireAuth(s.handleRequeue))

	s.handler = mux
	s.http = &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Handler exposes the HTTP routes (used by tests).
func (s *Server) Handler() http.Handler { return s.handler }

// Serve blocks serving until Shutdown is called.
func (s *Server) Serve() error {
	s.log.Info("admin api listening", "addr", s.http.Addr)
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
		s.log.Warn("admin api shutdown", "err", err)
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

// --- handlers ----------------------------------------------------------------

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
	s.log.Info("api: dead message requeued", "id", id)
	writeJSON(w, http.StatusOK, map[string]any{"requeued": id})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
