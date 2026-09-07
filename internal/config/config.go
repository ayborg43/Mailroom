// Package config loads and validates the mail server's YAML configuration.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Hostname string   `yaml:"hostname"` // FQDN used in HELO/EHLO, DKIM signing, TLS SNI default
	Domains  []string `yaml:"domains"`  // local domains this server accepts mail for

	Listen ListenConfig `yaml:"listen"`
	TLS    TLSConfig    `yaml:"tls"`

	Database DatabaseConfig `yaml:"database"`
	Storage  StorageConfig  `yaml:"storage"`

	DKIM     DKIMConfig     `yaml:"dkim"`
	Queue    QueueConfig    `yaml:"queue"`
	Admin    AdminConfig    `yaml:"admin"`
	Security SecurityConfig `yaml:"security"`
}

type ListenConfig struct {
	SMTP       string `yaml:"smtp"`       // e.g. ":25"
	Submission string `yaml:"submission"` // e.g. ":587"
	IMAP       string `yaml:"imap"`       // e.g. ":993"
	Webmail    string `yaml:"webmail"`    // e.g. ":8080"; plain HTTP, TLS terminated by a reverse proxy in front of it
	Metrics    string `yaml:"metrics"`    // e.g. ":9090"; /healthz + /metrics, unauthenticated — keep this off any publicly reachable interface
}

type TLSConfig struct {
	// Static cert/key pair mounted into the container. Takes precedence over ACME if both set.
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`

	ACME ACMEConfig `yaml:"acme"`
}

type ACMEConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Email    string `yaml:"email"`
	CacheDir string `yaml:"cache_dir"` // where certs/account keys are persisted
	// DNS-01 provider, e.g. "cloudflare", "route53", "digitalocean" (see go-acme/lego providers).
	Provider string            `yaml:"provider"`
	Options  map[string]string `yaml:"options"` // passed through as provider-specific env vars
	Staging  bool              `yaml:"staging"` // use Let's Encrypt staging endpoint
}

type DatabaseConfig struct {
	Driver string `yaml:"driver"` // "sqlite" or "postgres"
	DSN    string `yaml:"dsn"`
}

type StorageConfig struct {
	MaildirPath string `yaml:"maildir_path"` // base directory, one subdir per domain/user
	QueuePath   string `yaml:"queue_path"`   // outbound spool
}

type DKIMConfig struct {
	Selector       string `yaml:"selector"`
	PrivateKeyPath string `yaml:"private_key_path"` // generated on first run if missing
}

type QueueConfig struct {
	MaxRetries           int   `yaml:"max_retries"`
	RetryIntervalMinutes []int `yaml:"retry_interval_minutes"` // backoff schedule, e.g. [1, 5, 15, 60, 240, 1440]
}

type AdminConfig struct {
	Email    string `yaml:"email"`
	Password string `yaml:"password"` // plaintext, only used to bootstrap on first run
}

type SecurityConfig struct {
	// IPs/CIDRs allowed to supply the real client address on our behalf:
	// X-Forwarded-For for the webmail HTTP server (from Traefik), or the
	// PROXY protocol preamble for SMTP/IMAP (from a TCP-level proxy, if
	// any sits in front of those ports — by default nothing does, since
	// Traefik can't proxy raw TCP mail protocols). A connection or request
	// from any other source is never trusted to say who it's forwarding
	// for, no matter what headers it sends — this is the one property
	// that must never regress, per a past incident where naive IP-blocking
	// treated the reverse proxy's own address as the abusive client and
	// took the whole service down.
	TrustedProxies []string        `yaml:"trusted_proxies"`
	AuthRateLimit  RateLimitConfig `yaml:"auth_rate_limit"`
}

type RateLimitConfig struct {
	MaxAttempts   int `yaml:"max_attempts"`   // failed attempts allowed within the window before banning
	WindowMinutes int `yaml:"window_minutes"` // trailing window failures are counted over
	BanMinutes    int `yaml:"ban_minutes"`    // how long a ban lasts once triggered
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	cfg.applyEnvOverrides()
	cfg.applyDefaults()

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

// applyEnvOverrides layers environment variables on top of whatever the
// YAML file set, for the handful of settings a container platform's
// "Environment Variables" UI is the natural place to manage: secrets
// (admin password, database credentials) and the few values that
// routinely differ per deployment (hostname, domains) without editing the
// mounted config file. Everything else — storage paths, queue backoff,
// rate-limit thresholds — stays YAML-only, since a flat env var isn't a
// good fit for that shape of config and it changes far less often.
//
// This is also where the "SQLite or Postgres, env-var configurable"
// requirement lives: MAILSERVER_DATABASE_DRIVER/DSN let a deployment
// switch databases without touching config.yaml at all.
func (c *Config) applyEnvOverrides() {
	if v, ok := os.LookupEnv("MAILSERVER_HOSTNAME"); ok {
		c.Hostname = v
	}
	if v, ok := os.LookupEnv("MAILSERVER_DOMAINS"); ok {
		c.Domains = splitAndTrim(v)
	}
	if v, ok := os.LookupEnv("MAILSERVER_DATABASE_DRIVER"); ok {
		c.Database.Driver = v
	}
	if v, ok := os.LookupEnv("MAILSERVER_DATABASE_DSN"); ok {
		c.Database.DSN = v
	}
	if v, ok := os.LookupEnv("MAILSERVER_ADMIN_EMAIL"); ok {
		c.Admin.Email = v
	}
	if v, ok := os.LookupEnv("MAILSERVER_ADMIN_PASSWORD"); ok {
		c.Admin.Password = v
	}
	if v, ok := os.LookupEnv("MAILSERVER_TRUSTED_PROXIES"); ok {
		c.Security.TrustedProxies = splitAndTrim(v)
	}
	if v, ok := os.LookupEnv("MAILSERVER_TLS_CERT_FILE"); ok {
		c.TLS.CertFile = v
	}
	if v, ok := os.LookupEnv("MAILSERVER_TLS_KEY_FILE"); ok {
		c.TLS.KeyFile = v
	}
	if v, ok := os.LookupEnv("MAILSERVER_ACME_ENABLED"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			c.TLS.ACME.Enabled = b
		}
	}
	if v, ok := os.LookupEnv("MAILSERVER_ACME_EMAIL"); ok {
		c.TLS.ACME.Email = v
	}
	if v, ok := os.LookupEnv("MAILSERVER_ACME_PROVIDER"); ok {
		c.TLS.ACME.Provider = v
	}
	if v, ok := os.LookupEnv("MAILSERVER_ACME_STAGING"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			c.TLS.ACME.Staging = b
		}
	}
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (c *Config) applyDefaults() {
	if c.Listen.SMTP == "" {
		c.Listen.SMTP = ":25"
	}
	if c.Listen.Submission == "" {
		c.Listen.Submission = ":587"
	}
	if c.Listen.IMAP == "" {
		c.Listen.IMAP = ":993"
	}
	if c.Listen.Webmail == "" {
		c.Listen.Webmail = ":8080"
	}
	if c.Listen.Metrics == "" {
		c.Listen.Metrics = "127.0.0.1:9090"
	}
	if c.Database.Driver == "" {
		c.Database.Driver = "sqlite"
	}
	if c.Database.DSN == "" && c.Database.Driver == "sqlite" {
		c.Database.DSN = "/data/mailserver.db"
	}
	if c.Storage.MaildirPath == "" {
		c.Storage.MaildirPath = "/data/mail"
	}
	if c.Storage.QueuePath == "" {
		c.Storage.QueuePath = "/data/queue"
	}
	if c.DKIM.Selector == "" {
		c.DKIM.Selector = "default"
	}
	if c.DKIM.PrivateKeyPath == "" {
		c.DKIM.PrivateKeyPath = "/data/dkim/private.pem"
	}
	if c.Queue.MaxRetries == 0 {
		c.Queue.MaxRetries = 8
	}
	if len(c.Queue.RetryIntervalMinutes) == 0 {
		c.Queue.RetryIntervalMinutes = []int{1, 5, 15, 60, 240, 1440}
	}
	if c.TLS.ACME.CacheDir == "" {
		c.TLS.ACME.CacheDir = "/data/acme"
	}
	if c.Security.AuthRateLimit.MaxAttempts == 0 {
		c.Security.AuthRateLimit.MaxAttempts = 5
	}
	if c.Security.AuthRateLimit.WindowMinutes == 0 {
		c.Security.AuthRateLimit.WindowMinutes = 15
	}
	if c.Security.AuthRateLimit.BanMinutes == 0 {
		c.Security.AuthRateLimit.BanMinutes = 15
	}
}

func (c *Config) validate() error {
	if c.Hostname == "" {
		return fmt.Errorf("hostname is required")
	}
	if len(c.Domains) == 0 {
		return fmt.Errorf("at least one domain is required")
	}
	if c.Database.Driver != "sqlite" && c.Database.Driver != "postgres" {
		return fmt.Errorf("database.driver must be 'sqlite' or 'postgres', got %q", c.Database.Driver)
	}
	if c.Database.Driver == "postgres" && c.Database.DSN == "" {
		return fmt.Errorf("database.dsn is required for postgres")
	}
	return nil
}

// IsLocalDomain reports whether domain (case-insensitive) is configured as local.
func (c *Config) IsLocalDomain(domain string) bool {
	for _, d := range c.Domains {
		if strings.EqualFold(d, domain) {
			return true
		}
	}
	return false
}
