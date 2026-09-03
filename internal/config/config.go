// Package config loads and validates the Carteiro configuration.
//
// Precedence: system defaults < YAML file < CARTEIRO_* environment variables
// < command line flags (handled by main).
package config

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"carteiro/internal/dkim"
)

// Duration accepts YAML durations as text ("30s", "5m") or as an integer
// (nanoseconds).
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("invalid duration at line %d", node.Line)
	}
	if node.Tag == "!!int" {
		var ns int64
		if err := node.Decode(&ns); err != nil {
			return err
		}
		*d = Duration(ns)
		return nil
	}
	var raw string
	if err := node.Decode(&raw); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	*d = Duration(parsed)
	return nil
}

// D converts back to a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// TLS configures the optional TLS layer of the submission server.
//
// The certificate and key are provided ONLY as base64 of the PEM files (one
// line each), never as filesystem paths: cert_data/key_data in YAML or
// CARTEIRO_TLS_CERT/CARTEIRO_TLS_KEY in the environment. They are decoded at
// load time into CertPEM/KeyPEM.
type TLS struct {
	CertData string `yaml:"cert_data"`
	KeyData  string `yaml:"key_data"`
	// Mode is "starttls" (default) or "implicit" (465).
	Mode string `yaml:"mode"`

	// Decoded PEM texts (never read from YAML directly).
	CertPEM string `yaml:"-"`
	KeyPEM  string `yaml:"-"`
}

// decodeYAMLBase64 converts cert_data/key_data (base64 of the PEM files)
// into PEM text. The base64 strings may contain line breaks or spaces.
func (t *TLS) decodeYAMLBase64() error {
	decode := func(field, name string) (string, error) {
		if strings.TrimSpace(field) == "" {
			return "", fmt.Errorf("missing %s (base64 of the PEM file)", name)
		}
		compact := strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, field)
		decoded, err := base64.StdEncoding.DecodeString(compact)
		if err != nil {
			return "", fmt.Errorf("%s must be base64-encoded PEM: %v", name, err)
		}
		if !strings.Contains(string(decoded), "-----BEGIN") {
			return "", fmt.Errorf("%s decodes to something that is not a PEM file", name)
		}
		return string(decoded), nil
	}
	if strings.TrimSpace(t.CertData) == "" && strings.TrimSpace(t.KeyData) == "" {
		return nil // nothing set yet; validation below reports it
	}
	cert, err := decode(t.CertData, "cert_data")
	if err != nil {
		return err
	}
	key, err := decode(t.KeyData, "key_data")
	if err != nil {
		return err
	}
	t.CertPEM = cert
	t.KeyPEM = key
	return nil
}

// Delivery controls MX delivery.
type Delivery struct {
	ConnectTimeout Duration `yaml:"connect_timeout"`
	IOTimeout      Duration `yaml:"io_timeout"`
	RetryBase      Duration `yaml:"retry_base"`
	RetryMax       Duration `yaml:"retry_max"`
	MaxAttempts    int      `yaml:"max_attempts"`
	PollInterval   Duration `yaml:"poll_interval"`
	Concurrency    int      `yaml:"concurrency"`
}

// Storage selects the database backend. Type is "sqlite" (default) or
// "mysql". For sqlite, SQLitePath is the database file (empty picks the
// system default data directory). For mysql, DSN is the connection string.
type Storage struct {
	Type       string `yaml:"type"`
	SQLitePath string `yaml:"sqlite_path"`
	DSN        string `yaml:"dsn"`
}

// QueueCfg tunes the queue behavior.
type QueueCfg struct {
	// DeadMax bounds the dead-letter table (oldest rows are pruned beyond
	// this). 0 disables the limit.
	DeadMax int `yaml:"dead_max"`
}

// API configures the administrative REST API. Token is a single plain-text
// bearer token; it is intentionally NOT stored in the database - only the
// server owner sees it (config file or environment).
type API struct {
	Listen string `yaml:"listen"`
	Token  string `yaml:"token"`
}

// Account is a seed account (login + password in plain text). Seeds are
// upserted into the database at startup with clear logs; passwords are
// hashed there. Subsequent runtime changes go through the admin API.
type Account struct {
	Email    string `yaml:"email"`
	Password string `yaml:"password"`
	// AllowedFrom lists extra envelope addresses the account may use. Empty
	// means only the account's own Email.
	AllowedFrom []string `yaml:"allowed_from"`
}

// DKIMKey is a seed DKIM key. The private key is provided ONLY as the base64
// encoding of the whole PEM file (a single line): in YAML through key_data,
// in the environment through CARTEIRO_DKIM_KEYS. The base64 is decoded at
// load time into KeyData (PEM text).
type DKIMKey struct {
	Domain   string `yaml:"domain"`
	Selector string `yaml:"selector"`
	KeyData  string `yaml:"key_data"`
}

// Config is the effective configuration (defaults and validation applied).
type Config struct {
	Listen   string `yaml:"listen"`
	Hostname string `yaml:"hostname"`
	// Storage holds the database settings.
	Storage         *Storage `yaml:"storage"`
	MaxMessageSize  int64    `yaml:"max_message_size"`
	MaxRecipients   int      `yaml:"max_recipients"`
	RequireTLS      bool     `yaml:"require_tls"`
	InsecureAuthMsg string   `yaml:"-"`
	LogLevel        string   `yaml:"log_level"`

	API      *API      `yaml:"api"`
	Queue    QueueCfg  `yaml:"queue"`
	TLS      *TLS      `yaml:"tls"`
	Delivery Delivery  `yaml:"delivery"`
	DKIM     []DKIMKey `yaml:"dkim"`
	Accounts []Account `yaml:"accounts"`

	// dkimEnv holds DKIM keys configured through CARTEIRO_DKIM_*; they
	// override the YAML entries for the same domains.
	dkimEnv []DKIMKey
}

// ConfigFile is the path of the loaded file (or "" if none).
var ConfigFile = ""

func defaults() *Config {
	host, _ := os.Hostname()
	return &Config{
		Listen:         ":587",
		Hostname:       host,
		Storage:        &Storage{Type: "sqlite"},
		MaxMessageSize: 25 << 20, // 25 MiB
		MaxRecipients:  100,
		RequireTLS:     false,
		LogLevel:       "info",
		Delivery:       Delivery{ConnectTimeout: Duration(30 * time.Second), IOTimeout: Duration(2 * time.Minute), RetryBase: Duration(time.Minute), RetryMax: Duration(4 * time.Hour), MaxAttempts: 10, PollInterval: Duration(5 * time.Second), Concurrency: 4},
		Queue:          QueueCfg{DeadMax: 1000},
	}
}

// Load assembles the configuration: defaults -> YAML file -> CARTEIRO_* env.
// flagPath takes precedence when provided.
func Load(flagPath string) (*Config, error) {
	cfg := defaults()

	path, err := locateFile(flagPath)
	if err != nil {
		return nil, err
	}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading config %s: %w", path, err)
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing config %s: %w", path, err)
		}
		// YAML dkim keys and TLS certificates/keys are base64-encoded PEM;
		// decode them to PEM text now (environment entries are appended
		// later, already normalized).
		for i := range cfg.DKIM {
			if err := cfg.DKIM[i].decodeYAMLKey(); err != nil {
				return nil, fmt.Errorf("config %s: dkim for %s: %w", path, cfg.DKIM[i].Domain, err)
			}
		}
		if cfg.TLS != nil {
			if err := cfg.TLS.decodeYAMLBase64(); err != nil {
				return nil, fmt.Errorf("config %s: tls: %w", path, err)
			}
		}
		ConfigFile = path
	}
	if err := applyEnv(cfg); err != nil {
		return nil, err
	}
	if cfg.Storage == nil {
		cfg.Storage = &Storage{Type: "sqlite"}
	}
	if len(cfg.dkimEnv) > 0 {
		merged := make([]DKIMKey, 0, len(cfg.DKIM)+len(cfg.dkimEnv))
		for _, k := range cfg.DKIM {
			dupe := false
			for _, e := range cfg.dkimEnv {
				if e.Domain == k.Domain {
					dupe = true
					break
				}
			}
			if !dupe {
				merged = append(merged, k)
			}
		}
		merged = append(merged, cfg.dkimEnv...)
		cfg.DKIM = merged
	}
	if cfg.Hostname == "" {
		cfg.Hostname, _ = os.Hostname()
	}
	if err := cfg.normalizeAndValidate(); err != nil {
		return nil, err
	}
	if !cfg.RequireTLS {
		cfg.InsecureAuthMsg = "plain-text AUTH is enabled: use TLS or a private network (require_tls: true in public production)"
	}
	return cfg, nil
}

// ConfigFilePath returns the path of the loaded config file.
func (c *Config) ConfigFilePath() string { return ConfigFile }

// locateFile returns the first existing config file.
func locateFile(flagPath string) (string, error) {
	if flagPath != "" {
		if _, err := os.Stat(flagPath); err != nil {
			return "", fmt.Errorf("given config does not exist: %s", flagPath)
		}
		return flagPath, nil
	}
	if p := os.Getenv("CARTEIRO_CONFIG"); p != "" {
		return p, nil
	}
	userDir, err := os.UserConfigDir()
	if err != nil {
		userDir = ""
	}
	candidates := []string{"/etc/carteiro/config.yaml", "/etc/carteiro/config.yml"}
	if userDir != "" {
		candidates = append(candidates, filepath.Join(userDir, "carteiro", "config.yaml"), filepath.Join(userDir, "carteiro", "config.yml"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", nil
}

// applyEnv overlays fields from CARTEIRO_* environment variables.
func applyEnv(c *Config) error {
	setStr := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
	setInt := func(key string, dst *int64) error {
		if v, ok := os.LookupEnv(key); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("invalid %s: %w", key, err)
			}
			*dst = n
		}
		return nil
	}
	setBool := func(key string, dst *bool) error {
		if v, ok := os.LookupEnv(key); ok {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("invalid %s: %w", key, err)
			}
			*dst = b
		}
		return nil
	}
	setIntField := func(key string, dst *int) error {
		if v, ok := os.LookupEnv(key); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("invalid %s: %w", key, err)
			}
			*dst = n
		}
		return nil
	}
	setDur := func(key string, dst *Duration) error {
		if v, ok := os.LookupEnv(key); ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("invalid %s (use Go durations like 30s, 5m): %w", key, err)
			}
			*dst = Duration(d)
		}
		return nil
	}
	setStr("CARTEIRO_LISTEN", &c.Listen)
	setStr("CARTEIRO_HOSTNAME", &c.Hostname)
	setStr("CARTEIRO_LOG_LEVEL", &c.LogLevel)
	setStr("CARTEIRO_SQLITE_PATH", &c.Storage.SQLitePath)
	if err := setDur("CARTEIRO_DELIVERY_CONNECT_TIMEOUT", &c.Delivery.ConnectTimeout); err != nil {
		return err
	}
	if err := setDur("CARTEIRO_DELIVERY_IO_TIMEOUT", &c.Delivery.IOTimeout); err != nil {
		return err
	}
	if err := setDur("CARTEIRO_DELIVERY_RETRY_BASE", &c.Delivery.RetryBase); err != nil {
		return err
	}
	if err := setDur("CARTEIRO_DELIVERY_RETRY_MAX", &c.Delivery.RetryMax); err != nil {
		return err
	}
	if err := setDur("CARTEIRO_DELIVERY_POLL_INTERVAL", &c.Delivery.PollInterval); err != nil {
		return err
	}
	if err := setIntField("CARTEIRO_DELIVERY_MAX_ATTEMPTS", &c.Delivery.MaxAttempts); err != nil {
		return err
	}
	if err := setIntField("CARTEIRO_DELIVERY_CONCURRENCY", &c.Delivery.Concurrency); err != nil {
		return err
	}
	if err := setIntField("CARTEIRO_QUEUE_DEAD_MAX", &c.Queue.DeadMax); err != nil {
		return err
	}
	if err := setInt("CARTEIRO_MAX_MESSAGE_SIZE", &c.MaxMessageSize); err != nil {
		return err
	}
	if v, ok := os.LookupEnv("CARTEIRO_MAX_RECIPIENTS"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid CARTEIRO_MAX_RECIPIENTS: %w", err)
		}
		c.MaxRecipients = n
	}
	if err := setBool("CARTEIRO_REQUIRE_TLS", &c.RequireTLS); err != nil {
		return err
	}
	if v := os.Getenv("CARTEIRO_STORAGE_TYPE"); v != "" {
		c.Storage.Type = v
	}
	if v := os.Getenv("CARTEIRO_DB_DSN"); v != "" {
		c.Storage.DSN = v
		c.Storage.Type = "mysql"
	}

	// TLS from inline base64 (the only supported way; no certificate files).
	if certData, keyData := os.Getenv("CARTEIRO_TLS_CERT"), os.Getenv("CARTEIRO_TLS_KEY"); certData != "" || keyData != "" {
		if certData == "" || keyData == "" {
			return fmt.Errorf("CARTEIRO_TLS_CERT and CARTEIRO_TLS_KEY must be set together")
		}
		normalized := &TLS{CertData: certData, KeyData: keyData, Mode: os.Getenv("CARTEIRO_TLS_MODE")}
		if err := normalized.decodeYAMLBase64(); err != nil {
			return err
		}
		normalized.CertData, normalized.KeyData = "", ""
		c.TLS = normalized
	}
	if v := os.Getenv("CARTEIRO_ACCOUNTS"); v != "" {
		for _, entry := range strings.Split(v, ";") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			email, password, ok := strings.Cut(entry, ":")
			if !ok || strings.TrimSpace(email) == "" {
				return fmt.Errorf("CARTEIRO_ACCOUNTS: entry %q must be email:password", entry)
			}
			c.Accounts = append(c.Accounts, Account{Email: strings.TrimSpace(email), Password: password})
		}
	}
	if v := os.Getenv("CARTEIRO_API_LISTEN"); v != "" {
		if c.API == nil {
			c.API = &API{}
		}
		c.API.Listen = v
	}
	if v := os.Getenv("CARTEIRO_API_TOKEN"); v != "" {
		if c.API == nil {
			c.API = &API{}
		}
		c.API.Token = v
	}
	if err := applyDKIMEnv(c); err != nil {
		return err
	}
	return nil
}

// applyDKIMEnv builds DKIM key seeds from the CARTEIRO_DKIM_KEYS environment
// variable. It is the ONLY environment surface for DKIM keys: one variable
// listing several domains, entries separated by ';':
//
//	CARTEIRO_DKIM_KEYS = "doma.com:mail:<b64-a>;domb.com:selector-b:<b64-b>"
//
// where <b64> is the base64 of the whole PEM private key file (a single
// line). Env entries override the YAML dkim: entries with the same domain.
func applyDKIMEnv(c *Config) error {
	if v := os.Getenv("CARTEIRO_DKIM_KEYS"); v != "" {
		for _, entry := range strings.Split(v, ";") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			parts := strings.SplitN(entry, ":", 3)
			if len(parts) != 3 || strings.TrimSpace(parts[0]) == "" {
				return fmt.Errorf("CARTEIRO_DKIM_KEYS: entry %q must be domain:selector:base64", entry)
			}
			domain := strings.TrimSpace(parts[0])
			selector := strings.TrimSpace(parts[1])
			if selector == "" {
				selector = "mail"
			}
			key, err := decodeBase64Key(parts[2])
			if err != nil {
				return fmt.Errorf("CARTEIRO_DKIM_KEYS: entry %q: %w", entry, err)
			}
			c.dkimEnv = append(c.dkimEnv, DKIMKey{Domain: domain, Selector: selector, KeyData: key})
		}
	}
	return nil
}

// decodeBase64Key decodes the base64 of a PEM private key back into PEM text.
func decodeBase64Key(v string) (string, error) {
	compact := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, v)
	decoded, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return "", fmt.Errorf("key must be base64 of the PEM private key: %v", err)
	}
	if !strings.Contains(string(decoded), "-----BEGIN") {
		return "", fmt.Errorf("key decodes to something that is not a PEM private key")
	}
	return string(decoded), nil
}

func (c *Config) normalizeAndValidate() error {
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("invalid listen %q (use host:port, e.g. \":587\"): %w", c.Listen, err)
	}
	if c.TLS != nil {
		switch c.TLS.Mode {
		case "", "starttls":
			c.TLS.Mode = "starttls"
		case "implicit":
		default:
			return fmt.Errorf("tls.mode must be \"starttls\" or \"implicit\", got %q", c.TLS.Mode)
		}
		if c.TLS.CertPEM == "" || c.TLS.KeyPEM == "" {
			return fmt.Errorf("tls: cert_data and key_data are required (base64 of the PEM files)")
		}
		if _, err := tls.X509KeyPair([]byte(c.TLS.CertPEM), []byte(c.TLS.KeyPEM)); err != nil {
			return fmt.Errorf("tls: certificate/key pair is invalid: %w", err)
		}
	}
	if c.MaxMessageSize <= 0 {
		return fmt.Errorf("max_message_size must be positive")
	}
	if c.MaxRecipients <= 0 {
		return fmt.Errorf("max_recipients must be positive")
	}
	if c.Queue.DeadMax < 0 {
		return fmt.Errorf("queue.dead_max must be >= 0 (0 disables the limit)")
	}
	switch c.Storage.Type {
	case "", "sqlite":
		c.Storage.Type = "sqlite"
	case "mysql":
		if strings.TrimSpace(c.Storage.DSN) == "" {
			return fmt.Errorf("storage type mysql requires storage.dsn (or CARTEIRO_DB_DSN)")
		}
	default:
		return fmt.Errorf("storage.type must be \"sqlite\" or \"mysql\", got %q", c.Storage.Type)
	}
	if c.API != nil {
		if strings.TrimSpace(c.API.Token) == "" {
			return fmt.Errorf("api requires a token (api.token or CARTEIRO_API_TOKEN)")
		}
		c.API.Token = strings.TrimSpace(c.API.Token)
		if strings.TrimSpace(c.API.Listen) == "" {
			c.API.Listen = "127.0.0.1:9090"
		}
		if _, _, err := net.SplitHostPort(c.API.Listen); err != nil {
			return fmt.Errorf("invalid api.listen %q: %w", c.API.Listen, err)
		}
	}

	// Normalize seed accounts (emails lowercased, last duplicate wins so env
	// overrides YAML) but do not require any: accounts can also be created
	// later through the admin API.
	index := map[string]int{}
	uniq := make([]Account, 0, len(c.Accounts))
	for _, acc := range c.Accounts {
		acc.Email = strings.ToLower(strings.TrimSpace(acc.Email))
		if i, ok := index[acc.Email]; ok {
			uniq[i] = acc
			continue
		}
		index[acc.Email] = len(uniq)
		uniq = append(uniq, acc)
	}
	c.Accounts = uniq
	for i := range c.Accounts {
		acc := &c.Accounts[i]
		if acc.Email == "" || !validEmail(acc.Email) {
			return fmt.Errorf("account %q: email must be a valid address", acc.Email)
		}
		if acc.Password == "" {
			return fmt.Errorf("account %q: password must not be empty", acc.Email)
		}
		acc.AllowedFrom = normalizeExtraAllowed(acc.AllowedFrom)
	}

	// Deduplicate DKIM seeds by domain keeping the last occurrence, so an
	// entry from the environment overrides the one from YAML.
	dkimIdx := map[string]int{}
	uniqDKIM := make([]DKIMKey, 0, len(c.DKIM))
	for _, k := range c.DKIM {
		k.Domain = strings.ToLower(strings.TrimSpace(k.Domain))
		if i, ok := dkimIdx[k.Domain]; ok {
			uniqDKIM[i] = k
			continue
		}
		dkimIdx[k.Domain] = len(uniqDKIM)
		uniqDKIM = append(uniqDKIM, k)
	}
	c.DKIM = uniqDKIM
	for i := range c.DKIM {
		k := &c.DKIM[i]
		if k.Domain == "" || k.Selector == "" {
			return fmt.Errorf("dkim: domain and selector are required")
		}
		if k.KeyData == "" {
			return fmt.Errorf("dkim for %s: provide the key as key_data (base64 of the PEM file)", k.Domain)
		}
		k.Selector = strings.TrimSpace(k.Selector)
		if _, err := dkim.ParseSigner([]byte(k.KeyData)); err != nil {
			return fmt.Errorf("dkim for %s: invalid inline key: %w", k.Domain, err)
		}
	}
	if c.Delivery.MaxAttempts < 1 {
		return fmt.Errorf("delivery.max_attempts must be >= 1")
	}
	if c.Delivery.Concurrency < 1 {
		return fmt.Errorf("delivery.concurrency must be >= 1")
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be one of debug|info|warn|error")
	}
	return nil
}

// decodeYAMLKey converts the YAML key_data (base64 of the PEM file) into PEM
// text. The base64 string may contain line breaks or spaces.
func (k *DKIMKey) decodeYAMLKey() error {
	if strings.TrimSpace(k.KeyData) == "" {
		return fmt.Errorf("missing key_data (base64 of the private key)")
	}
	decoded, err := decodeBase64Key(k.KeyData)
	if err != nil {
		return fmt.Errorf("key_data: %v", err)
	}
	k.KeyData = decoded
	return nil
}

func normalizeExtraAllowed(extra []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range extra {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// UserConfigFile returns the canonical user config path.
func UserConfigFile() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "carteiro", "config.yaml"), nil
}

func validEmail(s string) bool {
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return false
	}
	return !strings.ContainsAny(s, " \t<>()")
}

// DataDir returns the user data directory (without creating it).
func DataDir() string {
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support")
		}
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return x
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share")
	}
	return "."
}

// DefaultDataDir creates and returns the default data directory:
// /var/lib/carteiro (daemon/system) with a fallback to the user data
// directory.
func DefaultDataDir() (string, error) {
	candidates := []string{filepath.Join(string(os.PathSeparator), "var", "lib", "carteiro")}
	dataDir := filepath.Join(DataDir(), "carteiro")
	if dataDir != candidates[0] {
		candidates = append(candidates, dataDir)
	}
	for _, dir := range candidates {
		if err := os.MkdirAll(dir, 0o755); err == nil {
			return dir, nil
		}
	}
	return "", fmt.Errorf("could not create the data directory at %s (use storage.sqlite_path or CARTEIRO_SQLITE_PATH)", strings.Join(candidates, ", "))
}
