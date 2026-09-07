package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}

const minimalYAML = `
hostname: mail.example.com
domains: [example.com]
`

func TestLoadAppliesDefaults(t *testing.T) {
	path := writeTestConfig(t, minimalYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.Driver != "sqlite" {
		t.Errorf("Database.Driver = %q, want sqlite default", cfg.Database.Driver)
	}
	if cfg.Listen.SMTP != ":25" {
		t.Errorf("Listen.SMTP = %q, want :25 default", cfg.Listen.SMTP)
	}
}

// TestEnvOverrideSwitchesDatabase is the original Phase 1 requirement:
// "Must support running with an external Postgres OR embedded SQLite
// (env-var configurable)" — switching databases must not require editing
// the mounted config file.
func TestEnvOverrideSwitchesDatabase(t *testing.T) {
	path := writeTestConfig(t, minimalYAML)

	t.Setenv("MAILSERVER_DATABASE_DRIVER", "postgres")
	t.Setenv("MAILSERVER_DATABASE_DSN", "postgres://user:pass@db:5432/mailserver?sslmode=disable")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.Driver != "postgres" {
		t.Errorf("Database.Driver = %q, want postgres", cfg.Database.Driver)
	}
	if cfg.Database.DSN != "postgres://user:pass@db:5432/mailserver?sslmode=disable" {
		t.Errorf("Database.DSN = %q, want the env-supplied DSN", cfg.Database.DSN)
	}
}

func TestEnvOverrideTakesPrecedenceOverYAML(t *testing.T) {
	path := writeTestConfig(t, minimalYAML+"\nadmin:\n  email: from-yaml@example.com\n  password: yaml-password\n")

	t.Setenv("MAILSERVER_ADMIN_EMAIL", "from-env@example.com")
	t.Setenv("MAILSERVER_ADMIN_PASSWORD", "env-password")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Admin.Email != "from-env@example.com" {
		t.Errorf("Admin.Email = %q, want env override to win", cfg.Admin.Email)
	}
	if cfg.Admin.Password != "env-password" {
		t.Errorf("Admin.Password = %q, want env override to win", cfg.Admin.Password)
	}
}

func TestEnvOverrideDomainsAndTrustedProxiesSplitOnComma(t *testing.T) {
	path := writeTestConfig(t, minimalYAML)

	t.Setenv("MAILSERVER_DOMAINS", "a.example.com, b.example.com ,c.example.com")
	t.Setenv("MAILSERVER_TRUSTED_PROXIES", "10.0.0.0/24, 192.168.1.5")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wantDomains := []string{"a.example.com", "b.example.com", "c.example.com"}
	if len(cfg.Domains) != len(wantDomains) {
		t.Fatalf("Domains = %v, want %v", cfg.Domains, wantDomains)
	}
	for i, d := range wantDomains {
		if cfg.Domains[i] != d {
			t.Errorf("Domains[%d] = %q, want %q", i, cfg.Domains[i], d)
		}
	}

	wantProxies := []string{"10.0.0.0/24", "192.168.1.5"}
	if len(cfg.Security.TrustedProxies) != len(wantProxies) {
		t.Fatalf("TrustedProxies = %v, want %v", cfg.Security.TrustedProxies, wantProxies)
	}
	for i, p := range wantProxies {
		if cfg.Security.TrustedProxies[i] != p {
			t.Errorf("TrustedProxies[%d] = %q, want %q", i, cfg.Security.TrustedProxies[i], p)
		}
	}
}

func TestEnvOverrideBooleans(t *testing.T) {
	path := writeTestConfig(t, minimalYAML)

	t.Setenv("MAILSERVER_ACME_ENABLED", "true")
	t.Setenv("MAILSERVER_ACME_STAGING", "false")
	t.Setenv("MAILSERVER_ACME_EMAIL", "admin@example.com")
	t.Setenv("MAILSERVER_ACME_PROVIDER", "cloudflare")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.TLS.ACME.Enabled {
		t.Error("ACME.Enabled should be true from env override")
	}
	if cfg.TLS.ACME.Staging {
		t.Error("ACME.Staging should be false from env override")
	}
	if cfg.TLS.ACME.Email != "admin@example.com" || cfg.TLS.ACME.Provider != "cloudflare" {
		t.Errorf("ACME.Email/Provider = %q/%q, want overrides applied", cfg.TLS.ACME.Email, cfg.TLS.ACME.Provider)
	}
}

func TestEnvOverrideInvalidBooleanIgnored(t *testing.T) {
	path := writeTestConfig(t, minimalYAML)
	t.Setenv("MAILSERVER_ACME_ENABLED", "not-a-bool")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TLS.ACME.Enabled {
		t.Error("an unparseable boolean override should be ignored, not silently treated as true")
	}
}

func TestNoEnvVarsLeavesYAMLUntouched(t *testing.T) {
	path := writeTestConfig(t, minimalYAML+"\ndatabase:\n  driver: postgres\n  dsn: postgres://yaml-only\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.Driver != "postgres" || cfg.Database.DSN != "postgres://yaml-only" {
		t.Errorf("expected YAML values preserved when no env vars set, got %q/%q", cfg.Database.Driver, cfg.Database.DSN)
	}
}
