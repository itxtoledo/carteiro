// Package smtpd implements Carteiro's SMTP submission server.
//
// Clients authenticate with AUTH PLAIN/LOGIN using the account email as the
// login, and the MAIL FROM must be a sender allowed by the authenticated
// account (accounts.allowed_from). Accepted messages are stored in the
// database queue and answered with 250 ... queued as <id>.
package smtpd

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"carteiro/internal/config"
	"carteiro/internal/metrics"
	"carteiro/internal/storage"
)

// Server serves SMTP connections on the configured port.
type Server struct {
	cfg     *config.Config
	store   *storage.Store
	log     *slog.Logger
	metrics *metrics.Metrics
	tlsCfg  *tls.Config

	ln net.Listener

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// New validates the configured TLS and prepares the server.
func New(cfg *config.Config, store *storage.Store, log *slog.Logger, m *metrics.Metrics) (*Server, error) {
	s := &Server{cfg: cfg, store: store, log: log, metrics: m}
	if cfg.TLS != nil {
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("loading TLS certificate: %w", err)
		}
		s.tlsCfg = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}
	return s, nil
}

// Serve opens the listener and serves until the context is cancelled or
// Shutdown is called.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.cfg.Listen, err)
	}
	mode := "no TLS"
	if s.tlsCfg != nil {
		mode = "starttls"
		if s.cfg.TLS.Mode == "implicit" {
			mode = "implicit"
		}
	}
	s.log.Info("smtp listening", "addr", ln.Addr().String(), "tls", mode, "auth_plaintext", !s.cfg.RequireTLS)
	return s.ServeWithListener(ctx, ln)
}

// ServeWithListener serves on an already-created listener (used by tests).
func (s *Server) ServeWithListener(ctx context.Context, ln net.Listener) error {
	s.ln = ln
	go func() {
		<-ctx.Done()
		s.closeListener()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.isClosed() {
				break
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			s.log.Warn("accept failed", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveConn(conn)
		}()
	}
	s.wg.Wait()
	return nil
}

// Shutdown closes the listener and waits up to timeout for active
// connections.
func (s *Server) Shutdown(timeout time.Duration) {
	s.closeListener()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		s.log.Warn("smtp: shutdown exceeded the timeout, forcing connections closed")
	}
}

func (s *Server) closeListener() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed && s.ln != nil {
		s.closed = true
		s.ln.Close()
	}
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}
