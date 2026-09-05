package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"carteiro/internal/webui"
)

// Panel serves the web dashboard on its OWN listener: the SPA at "/" plus an
// in-process reverse proxy of every /api/* request to the admin API listener.
// The browser keeps a single origin (no CORS, bearer header forwarded as-is),
// while the API remains independently reachable and bindable on its own port.
type Panel struct {
	log     *slog.Logger
	handler http.Handler
	http    *http.Server
}

// NewPanel builds the panel server. listen is the web address (e.g. ":8080");
// apiTarget is the HTTP base URL of the admin API listener.
func NewPanel(listen, apiTarget string, log *slog.Logger) *Panel {
	proxy := &httputil.ReverseProxy{}
	if u, err := url.Parse(apiTarget); err == nil && u.Scheme != "" && u.Host != "" {
		proxy = httputil.NewSingleHostReverseProxy(u)
	} else {
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			log.Error("web panel: api proxy unavailable", "err", err)
			http.Error(w, `{"error":"admin api proxy unavailable"}`, http.StatusBadGateway)
		}
	}

	mux := http.NewServeMux()
	// /api/* goes to the admin API listener (never to the SPA).
	mux.HandleFunc("/api/", proxy.ServeHTTP)
	mux.HandleFunc("/api", proxy.ServeHTTP)
	// Everything else is the dashboard.
	mux.Handle("/", webui.Handler())

	return &Panel{
		log:     log,
		handler: mux,
		http: &http.Server{
			Addr:              listen,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		},
	}
}

// Handler exposes the panel routes (used by tests).
func (p *Panel) Handler() http.Handler { return p.handler }

// Serve blocks serving until Shutdown is called.
func (p *Panel) Serve() error {
	p.log.Info("web panel listening", "addr", p.http.Addr)
	if err := p.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown gracefully stops the panel server.
func (p *Panel) Shutdown(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := p.http.Shutdown(ctx); err != nil {
		p.log.Warn("web panel shutdown", "err", err)
	}
}

// APITargetURL derives the HTTP base URL of the admin API from its listen
// address. An empty or wildcard host ("", "0.0.0.0", "::") dials loopback,
// which is always correct for the in-process proxy.
func APITargetURL(apiListen string) string {
	host, port, err := net.SplitHostPort(apiListen)
	if err != nil {
		host, port = "127.0.0.1", "9090"
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	host = strings.Trim(host, "[]")
	return "http://" + net.JoinHostPort(host, port)
}
