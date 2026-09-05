// Command carteiro is the SMTP relay: it accepts authenticated messages,
// stores them in the database queue (SQLite or MySQL) and delivers via MX
// with retry. Accounts and DKIM domains are seeded at startup (upsert with
// clear logs) and can be managed over the air through the admin REST API. It
// runs as a daemon (systemd, launchd, nohup) or in a container.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"carteiro/internal/acme"
	"carteiro/internal/api"
	"carteiro/internal/config"
	"carteiro/internal/dkim"
	"carteiro/internal/logmask"
	"carteiro/internal/metrics"
	"carteiro/internal/relay"
	"carteiro/internal/sends"
	"carteiro/internal/smtpd"
	"carteiro/internal/storage"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "carteiro:", err)
		os.Exit(1)
	}
}

func run() error {
	var configPath string
	showVersion := flag.Bool("version", false, "print the version and exit")
	manageACME := flag.Bool("acme", false, "manage a Let's Encrypt certificate for the SMTP listener (overrides CARTEIRO_ACME)")
	flag.StringVar(&configPath, "config", "", "path to the config file (default: looks in /etc/carteiro and the user config dirs)")
	flag.Parse()

	if *showVersion {
		fmt.Println("carteiro", version)
		return nil
	}

	acmeFlagSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "acme" {
			acmeFlagSet = true
		}
	})

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	// The CLI flag wins over CARTEIRO_ACME; re-validate because the override
	// happens after Load.
	if acmeFlagSet {
		if cfg.ACME == nil {
			cfg.ACME = &config.ACME{}
		}
		cfg.ACME.Enabled = *manageACME
		if cfg.ACME.Enabled {
			if err := cfg.ACME.Validate(cfg.Hostname); err != nil {
				return err
			}
		}
	}

	logger := logmask.NewLogger(newLogger(cfg.LogLevel), cfg.LogMaskEmails)

	dbPath, err := resolveDBPath(cfg)
	if err != nil {
		return err
	}
	store, err := storage.Open(cfg.Storage.Type, dbPath)
	if err != nil {
		return fmt.Errorf("opening %s database: %w", cfg.Storage.Type, err)
	}
	defer store.Close()
	store.SetDeadMax(cfg.Queue.DeadMax)

	// Seed: upsert accounts and DKIM keys declared in YAML/env. Idempotent -
	// runs on every start, logs exactly what changed (created/updated/
	// unchanged). Later changes belong to the admin API.
	if err := seed(logger, cfg, store); err != nil {
		return err
	}

	counters := &metrics.Metrics{}
	// rec powers the web panel's recent-sends feed (list, rendered bodies and
	// live delivery status). It is an in-memory ring: the database queue
	// remains the source of truth for delivery.
	rec := sends.New(200, 512<<10)
	server, err := smtpd.New(cfg, store, logger, counters, rec)
	if err != nil {
		return err
	}
	deliverer, err := relay.New(cfg, store, logger, counters, rec)
	if err != nil {
		return err
	}

	// Managed TLS (ACME): when enabled, Carteiro obtains and renews a
	// Let's Encrypt certificate for the SMTP hostname itself and the SMTP
	// listener resolves it dynamically (no restart on renewal). When
	// disabled, an external proxy (or the base64 tls.* settings) keep
	// handling certificates.
	var acmeMgr *acme.Manager
	if cfg.ACME != nil && cfg.ACME.Enabled {
		dir := acme.DirectoryProduction
		if cfg.ACME.Staging {
			dir = acme.DirectoryStaging
		}
		mgr, err := acme.New(cfg.ACME, cfg.Hostname, store, logger, dir)
		if err != nil {
			return err
		}
		initCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		mgr.Init(initCtx)
		cancel()
		tlsMode := "starttls"
		if cfg.TLS != nil && cfg.TLS.Mode == "implicit" {
			tlsMode = "implicit"
		}
		server.UseManagedTLS(tlsMode, mgr.GetCertificate)
		acmeMgr = mgr
		logger.Info("managed tls (acme) enabled",
			"domain", cfg.Hostname, "provider", cfg.ACME.Provider, "directory", dir, "tls_mode", tlsMode)
	}

	logger.Info("carteiro starting",
		"version", version,
		"config", cfg.ConfigFilePath(),
		"storage", cfg.Storage.Type,
		"db", dbPath,
		"accounts_in_db", lenOr(logger, store),
		"log_level", cfg.LogLevel,
	)
	if cfg.API != nil {
		logger.Info("admin api enabled", "listen", cfg.API.Listen)
		logger.Info("web panel enabled", "listen", cfg.Web.Listen, "api_proxy", api.APITargetURL(cfg.API.Listen))
	}
	if cfg.InsecureAuthMsg != "" {
		logger.Warn(cfg.InsecureAuthMsg)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if acmeMgr != nil {
		go acmeMgr.Run(ctx)
	}

	serveErr := make(chan error, 3)
	go func() {
		serveErr <- server.Serve(ctx)
	}()

	var apiServer *api.Server
	var panel *api.Panel
	if cfg.API != nil {
		apiServer = api.New(cfg.API, store, logger, counters, rec, version, cfg.MaxMessageSize, cfg.MaxRecipients)
		go func() {
			if err := apiServer.Serve(); err != nil {
				serveErr <- fmt.Errorf("admin api: %w", err)
			}
		}()
		// The web panel runs on its own listener (cfg.Web.Listen) and proxies
		// /api/* to the admin API listener above.
		panel = api.NewPanel(cfg.Web.Listen, api.APITargetURL(cfg.API.Listen), logger)
		go func() {
			if err := panel.Serve(); err != nil {
				serveErr <- fmt.Errorf("web panel: %w", err)
			}
		}()
	}

	workerCtx, cancelWorker := context.WithCancel(ctx)
	go deliverer.Run(workerCtx)

	select {
	case err := <-serveErr:
		if err != nil {
			cancelWorker()
			return err
		}
	case <-ctx.Done():
		logger.Info("signal received, shutting down")
	}

	cancelWorker()
	if apiServer != nil {
		apiServer.Shutdown(10 * time.Second)
	}
	if panel != nil {
		panel.Shutdown(10 * time.Second)
	}
	server.Shutdown(10 * time.Second)

	st, err := store.Stats(time.Now())
	if err == nil && st.Dead > 0 {
		logger.Warn("dead-letter messages waiting for review", "count", st.Dead, "hint", "see the admin API: GET /queue?status=dead and POST /queue/{id}/retry")
	}
	logger.Info("carteiro stopped")
	return nil
}

// resolveDBPath computes the sqlite file path or returns the mysql DSN.
func resolveDBPath(cfg *config.Config) (string, error) {
	if cfg.Storage.Type == "mysql" {
		return cfg.Storage.DSN, nil
	}
	if cfg.Storage.SQLitePath != "" {
		return cfg.Storage.SQLitePath, nil
	}
	dir, err := config.DefaultDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "carteiro.db"), nil
}

// seed upserts the accounts and DKIM keys declared in the YAML/env config,
// logging exactly what happened for every entry. DKIM keys are always PEM
// text (already decoded from base64 by the config loader).
func seed(logger *slog.Logger, cfg *config.Config, store *storage.Store) error {
	if len(cfg.Accounts) > 0 {
		seeds := make([]storage.AccountSeed, 0, len(cfg.Accounts))
		for _, a := range cfg.Accounts {
			seeds = append(seeds, storage.AccountSeed{Email: a.Email, Password: a.Password, AllowedFrom: a.AllowedFrom})
		}
		sum, err := store.UpsertAccounts(seeds)
		if err != nil {
			return fmt.Errorf("seeding accounts: %w", err)
		}
		logSeed(logger, "accounts", sum)
	} else {
		logger.Info("seed: no accounts declared in config (add them later via the admin API)")
	}

	if len(cfg.DKIM) > 0 {
		keys := make([]storage.DKIMKey, 0, len(cfg.DKIM))
		for _, k := range cfg.DKIM {
			if _, err := dkim.ParseSigner([]byte(k.KeyData)); err != nil {
				return fmt.Errorf("seeding dkim for %s: %w", k.Domain, err)
			}
			keys = append(keys, storage.DKIMKey{Domain: k.Domain, Selector: k.Selector, KeyData: k.KeyData})
		}
		sum, err := store.UpsertDKIM(keys)
		if err != nil {
			return fmt.Errorf("seeding dkim keys: %w", err)
		}
		logSeed(logger, "dkim keys", sum)
	} else {
		logger.Info("seed: no dkim keys declared in config (add them later via the admin API)")
	}
	return nil
}

func logSeed(logger *slog.Logger, what string, sum storage.UpsertSummary) {
	logger.Info(fmt.Sprintf("seed: upserting %s done", what),
		"created", sum.Created, "updated", sum.Updated, "unchanged", sum.Unchanged)
	for _, email := range sum.Created {
		logger.Info(fmt.Sprintf("seed: %s -> created in the database", email), "kind", what)
	}
	for _, email := range sum.Updated {
		logger.Info(fmt.Sprintf("seed: %s -> updated (new password or allowed_from)", email), "kind", what)
	}
	for _, email := range sum.Unchanged {
		logger.Debug(fmt.Sprintf("seed: %s -> unchanged, left as is", email), "kind", what)
	}
}

func lenOr(logger *slog.Logger, store *storage.Store) int {
	accs, err := store.ListAccounts()
	if err != nil {
		logger.Warn("counting accounts failed", "err", err)
		return -1
	}
	return len(accs)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
