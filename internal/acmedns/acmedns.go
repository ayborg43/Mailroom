// Package acmedns obtains and renews a TLS certificate via ACME DNS-01
// (RFC 8555 + RFC 8555 8.4), using github.com/go-acme/lego. DNS-01 is used
// instead of HTTP-01/TLS-ALPN-01 because ports 80/443 belong to the reverse
// proxy (Traefik) in front of the admin/webmail UI, not this process.
package acmedns

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/dns/cloudflare"
	"github.com/go-acme/lego/v4/registration"

	"github.com/sociolytik/mailserver/internal/config"
)

// renewBefore triggers renewal once the certificate has this long left
// before expiry (Let's Encrypt certs are valid 90 days; this leaves ample
// margin for retries).
const renewBefore = 30 * 24 * time.Hour

type acmeUser struct {
	Email string
	Reg   *registration.Resource
	key   crypto.PrivateKey
}

func (u *acmeUser) GetEmail() string                        { return u.Email }
func (u *acmeUser) GetRegistration() *registration.Resource { return u.Reg }
func (u *acmeUser) GetPrivateKey() crypto.PrivateKey        { return u.key }

type accountFile struct {
	Email        string                 `json:"email"`
	PrivateKey   []byte                 `json:"private_key"` // PEM-encoded EC private key
	Registration *registration.Resource `json:"registration"`
}

// Manager obtains and keeps a DNS-01 issued certificate up to date,
// persisting the ACME account and certificate under CacheDir so restarts
// don't needlessly re-issue.
type Manager struct {
	cfg     config.ACMEConfig
	domains []string
	logger  *slog.Logger

	mu   sync.RWMutex
	cert *tls.Certificate
}

func New(acmeCfg config.ACMEConfig, domains []string, logger *slog.Logger) (*Manager, error) {
	if acmeCfg.Email == "" {
		return nil, fmt.Errorf("tls.acme.email is required")
	}
	if len(domains) == 0 {
		return nil, fmt.Errorf("no domains configured for ACME")
	}
	if err := os.MkdirAll(acmeCfg.CacheDir, 0700); err != nil {
		return nil, fmt.Errorf("creating ACME cache directory: %w", err)
	}

	// lego's DNS providers read credentials from the process environment;
	// tls.acme.options is passed through as exactly that, per the documented
	// env vars for the chosen provider (e.g. CLOUDFLARE_DNS_API_TOKEN).
	for k, v := range acmeCfg.Options {
		if err := os.Setenv(k, v); err != nil {
			return nil, fmt.Errorf("setting %s: %w", k, err)
		}
	}

	return &Manager{cfg: acmeCfg, domains: domains, logger: logger}, nil
}

// TLSConfig returns a *tls.Config that always serves the manager's current
// certificate, so renewals in the background take effect without needing
// to restart the listener.
func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			m.mu.RLock()
			defer m.mu.RUnlock()
			if m.cert == nil {
				return nil, fmt.Errorf("acmedns: no certificate obtained yet")
			}
			return m.cert, nil
		},
	}
}

func (m *Manager) certPath() string    { return filepath.Join(m.cfg.CacheDir, "fullchain.pem") }
func (m *Manager) keyPath() string     { return filepath.Join(m.cfg.CacheDir, "privkey.pem") }
func (m *Manager) accountPath() string { return filepath.Join(m.cfg.CacheDir, "account.json") }

// Init loads a cached certificate or obtains a new one synchronously. Call
// this before starting any listener that needs TLSConfig().
func (m *Manager) Init() error {
	return m.ensureCertificate()
}

// Run checks every interval whether the certificate needs renewing, until
// ctx is canceled. Call Init first to obtain the initial certificate.
func (m *Manager) Run(ctx context.Context, checkInterval time.Duration) {
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if m.needsRenewal() {
				m.logger.Info("renewing ACME certificate")
				if err := m.obtain(); err != nil {
					m.logger.Error("renewing ACME certificate", "error", err)
				}
			}
		}
	}
}

func (m *Manager) ensureCertificate() error {
	if cert, err := tls.LoadX509KeyPair(m.certPath(), m.keyPath()); err == nil {
		m.mu.Lock()
		m.cert = &cert
		m.mu.Unlock()
		if !m.needsRenewal() {
			return nil
		}
		m.logger.Info("cached ACME certificate is near expiry, renewing")
	}
	return m.obtain()
}

func (m *Manager) needsRenewal() bool {
	m.mu.RLock()
	cert := m.cert
	m.mu.RUnlock()
	if cert == nil || len(cert.Certificate) == 0 {
		return true
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return true
	}
	return time.Until(leaf.NotAfter) < renewBefore
}

func (m *Manager) obtain() error {
	user, err := m.loadOrRegisterAccount()
	if err != nil {
		return fmt.Errorf("ACME account setup: %w", err)
	}

	legoCfg := lego.NewConfig(user)
	if m.cfg.Staging {
		legoCfg.CADirURL = lego.LEDirectoryStaging
	} else {
		legoCfg.CADirURL = lego.LEDirectoryProduction
	}

	client, err := lego.NewClient(legoCfg)
	if err != nil {
		return fmt.Errorf("creating ACME client: %w", err)
	}

	provider, err := m.dnsProvider()
	if err != nil {
		return err
	}
	if err := client.Challenge.SetDNS01Provider(provider); err != nil {
		return fmt.Errorf("configuring DNS-01 challenge: %w", err)
	}

	if user.Reg == nil {
		reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
		if err != nil {
			return fmt.Errorf("registering ACME account: %w", err)
		}
		user.Reg = reg
		if err := m.saveAccount(user); err != nil {
			return fmt.Errorf("saving ACME account: %w", err)
		}
	}

	res, err := client.Certificate.Obtain(certificate.ObtainRequest{
		Domains: m.domains,
		Bundle:  true,
	})
	if err != nil {
		return fmt.Errorf("obtaining certificate: %w", err)
	}

	if err := os.WriteFile(m.certPath(), res.Certificate, 0644); err != nil {
		return fmt.Errorf("writing certificate: %w", err)
	}
	if err := os.WriteFile(m.keyPath(), res.PrivateKey, 0600); err != nil {
		return fmt.Errorf("writing certificate key: %w", err)
	}

	cert, err := tls.X509KeyPair(res.Certificate, res.PrivateKey)
	if err != nil {
		return fmt.Errorf("parsing obtained certificate: %w", err)
	}

	m.mu.Lock()
	m.cert = &cert
	m.mu.Unlock()

	m.logger.Info("obtained ACME certificate", "domains", m.domains)
	return nil
}

func (m *Manager) dnsProvider() (*cloudflare.DNSProvider, error) {
	switch m.cfg.Provider {
	case "", "cloudflare":
		return cloudflare.NewDNSProvider()
	default:
		return nil, fmt.Errorf("unsupported ACME DNS provider %q (only \"cloudflare\" is currently supported)", m.cfg.Provider)
	}
}

func (m *Manager) loadOrRegisterAccount() (*acmeUser, error) {
	if buf, err := os.ReadFile(m.accountPath()); err == nil {
		var af accountFile
		if err := json.Unmarshal(buf, &af); err != nil {
			return nil, fmt.Errorf("parsing account file: %w", err)
		}
		block, _ := pem.Decode(af.PrivateKey)
		if block == nil {
			return nil, fmt.Errorf("no PEM data in stored account key")
		}
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing stored account key: %w", err)
		}
		return &acmeUser{Email: af.Email, Reg: af.Registration, key: key}, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating account key: %w", err)
	}
	return &acmeUser{Email: m.cfg.Email, key: key}, nil
}

func (m *Manager) saveAccount(user *acmeUser) error {
	ecKey, ok := user.key.(*ecdsa.PrivateKey)
	if !ok {
		return fmt.Errorf("unexpected account key type %T", user.key)
	}
	der, err := x509.MarshalECPrivateKey(ecKey)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})

	buf, err := json.MarshalIndent(accountFile{Email: user.Email, PrivateKey: keyPEM, Registration: user.Reg}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.accountPath(), buf, 0600)
}
