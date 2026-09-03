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

	"carteiro/internal/api"
	"carteiro/internal/config"
	"carteiro/internal/dkim"
	"carteiro/internal/metrics"
	"carteiro/internal/relay"
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
	flag.StringVar(&configPath, "config", "", "path to the config file (default: looks in /etc/carteiro and the user config dirs)")
	flag.Parse()

	if *showVersion {
		fmt.Println("carteiro", version)
		return nil
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)

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
	server, err := smtpd.New(cfg, store, logger, counters)
	if err != nil {
		return err
	}
	deliverer, err := relay.New(cfg, store, logger, counters)
	if err != nil {
		return err
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
	}
	if cfg.InsecureAuthMsg != "" {
		logger.Warn(cfg.InsecureAuthMsg)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 2)
	go func() {
		serveErr <- server.Serve(ctx)
	}()

	var apiServer *api.Server
	if cfg.API != nil {
		apiServer = api.New(cfg.API, store, logger, counters)
		go func() {
			if err := apiServer.Serve(); err != nil {
				serveErr <- fmt.Errorf("admin api: %w", err)
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
// logging exactly what happened for every entry. File-based DKIM keys are
// read and stored as PEM text in the database.
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
			data := k.KeyData
			if data == "" {
				raw, err := os.ReadFile(k.KeyFile)
				if err != nil {
					return fmt.Errorf("seeding dkim for %s: reading %s: %w", k.Domain, k.KeyFile, err)
				}
				data = string(raw)
			}
			if _, err := dkim.ParseSigner([]byte(data)); err != nil {
				return fmt.Errorf("seeding dkim for %s: %w", k.Domain, err)
			}
			keys = append(keys, storage.DKIMKey{Domain: k.Domain, Selector: k.Selector, KeyData: data})
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
