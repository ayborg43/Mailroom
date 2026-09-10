// Command mailserver runs the SMTP/IMAP mail server, or performs one-off
// administrative tasks (creating mailboxes) against the same config/database.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-smtp"

	"github.com/sociolytik/mailserver/internal/acmedns"
	"github.com/sociolytik/mailserver/internal/address"
	"github.com/sociolytik/mailserver/internal/config"
	"github.com/sociolytik/mailserver/internal/dkimsign"
	"github.com/sociolytik/mailserver/internal/imapbackend"
	"github.com/sociolytik/mailserver/internal/maildir"
	"github.com/sociolytik/mailserver/internal/mailsend"
	"github.com/sociolytik/mailserver/internal/metrics"
	"github.com/sociolytik/mailserver/internal/queue"
	"github.com/sociolytik/mailserver/internal/ratelimit"
	"github.com/sociolytik/mailserver/internal/realip"
	"github.com/sociolytik/mailserver/internal/smtpserver"
	"github.com/sociolytik/mailserver/internal/store"
	"github.com/sociolytik/mailserver/internal/tlsutil"
	"github.com/sociolytik/mailserver/internal/webmail"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "user":
		cmdUser(os.Args[2:])
	case "dkim":
		cmdDKIM(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `mailserver - self-hosted mail server

Usage:
  mailserver serve --config <path>
  mailserver user create --config <path> --email user@domain.com --password secret [--admin]
  mailserver user delete --config <path> --email user@domain.com

Commands:
  serve         Run the SMTP, IMAP, webmail, and outbound relay services.
  user create   Create a mailbox user (and its local domain, if new).
  user delete   Delete a mailbox user, its messages, and its Maildir.
  dkim show     Print the DNS TXT record to publish for DKIM signing.

Mailboxes can also be created and deleted from the webmail UI's Admin
page (/admin), available to any user created with --admin.`)
}

func cmdDKIM(args []string) {
	if len(args) < 1 || args[0] != "show" {
		fmt.Fprintln(os.Stderr, "usage: mailserver dkim show --config <path>")
		os.Exit(2)
	}

	fs := flag.NewFlagSet("dkim show", flag.ExitOnError)
	cfg := loadConfig(fs, args[1:])

	key, _, err := dkimsign.LoadOrGenerateKey(cfg.DKIM.PrivateKeyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading/generating DKIM key: %v\n", err)
		os.Exit(1)
	}
	record, err := dkimsign.DNSRecord(key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encoding DKIM public key: %v\n", err)
		os.Exit(1)
	}

	for _, d := range cfg.Domains {
		fmt.Printf("%s._domainkey.%s.  IN TXT  %q\n", cfg.DKIM.Selector, d, record)
	}
}

func loadConfig(fs *flag.FlagSet, args []string) *config.Config {
	path := fs.String("config", "/etc/mailserver/config.yaml", "path to config YAML file")
	fs.Parse(args)

	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading config: %v\n", err)
		os.Exit(1)
	}
	return cfg
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfg := loadConfig(fs, args)

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if err := os.MkdirAll(cfg.Storage.MaildirPath, 0700); err != nil {
		logger.Error("creating maildir base path", "error", err)
		os.Exit(1)
	}

	db, err := store.Open(cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		logger.Error("opening store", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	for _, d := range cfg.Domains {
		if _, err := db.EnsureDomain(context.Background(), d); err != nil {
			logger.Error("ensuring domain", "domain", d, "error", err)
			os.Exit(1)
		}
	}

	if cfg.Admin.Email != "" && cfg.Admin.Password != "" {
		if _, err := db.GetUserByEmail(context.Background(), cfg.Admin.Email); errors.Is(err, store.ErrNotFound) {
			local, domain, ok := address.Split(cfg.Admin.Email)
			if !ok {
				logger.Error("invalid admin.email in config", "email", cfg.Admin.Email)
				os.Exit(1)
			}
			admin, err := db.CreateUser(context.Background(), domain, local, cfg.Admin.Password, true)
			if err != nil {
				logger.Error("bootstrapping admin user", "error", err)
				os.Exit(1)
			}
			if err := imapbackend.ProvisionMailboxes(context.Background(), cfg, db, admin); err != nil {
				logger.Error("provisioning admin mailboxes", "error", err)
				os.Exit(1)
			}
			logger.Info("bootstrapped admin user", "email", cfg.Admin.Email)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	inboundBackend := &smtpserver.InboundBackend{Config: cfg, Store: db, Logger: logger}
	inboundServer := smtp.NewServer(inboundBackend)
	inboundServer.Addr = cfg.Listen.SMTP
	inboundServer.Domain = cfg.Hostname
	inboundServer.AllowInsecureAuth = true // this listener never authenticates; irrelevant either way
	inboundServer.ReadTimeout = 5 * 60 * 1e9
	inboundServer.WriteTimeout = 5 * 60 * 1e9
	inboundServer.MaxMessageBytes = 32 * 1024 * 1024
	inboundServer.MaxRecipients = 100

	tlsConfig, err := setupTLS(ctx, cfg, logger)
	if err != nil {
		logger.Error("setting up TLS", "error", err)
		os.Exit(1)
	}

	retryIntervals := make([]time.Duration, len(cfg.Queue.RetryIntervalMinutes))
	for i, m := range cfg.Queue.RetryIntervalMinutes {
		retryIntervals[i] = time.Duration(m) * time.Minute
	}
	mailQueue, err := queue.New(cfg.Storage.QueuePath, cfg.Hostname, retryIntervals, cfg.Queue.MaxRetries, logger)
	if err != nil {
		logger.Error("opening outbound queue", "error", err)
		os.Exit(1)
	}
	mailQueue.OnPermanentFailure = func(item queue.Item, data []byte, reason string) {
		if err := bounce(cfg, db, item, data, reason); err != nil {
			logger.Error("bouncing undeliverable message", "to", item.To, "from", item.From, "error", err)
		}
	}
	mailQueue.SmartHost = func(ctx context.Context) (*queue.SmartHostConfig, bool, error) {
		rs, err := db.GetRelaySettings(ctx)
		if err != nil {
			return nil, false, err
		}
		if !rs.Enabled {
			return nil, false, nil
		}
		return &queue.SmartHostConfig{Host: rs.Host, Port: rs.Port, Username: rs.Username, Password: rs.Password}, true, nil
	}
	go mailQueue.Run(ctx, 30*time.Second)

	dkimKey, generated, err := dkimsign.LoadOrGenerateKey(cfg.DKIM.PrivateKeyPath)
	if err != nil {
		logger.Error("loading/generating DKIM key", "error", err)
		os.Exit(1)
	}
	if generated {
		record, _ := dkimsign.DNSRecord(dkimKey)
		logger.Warn("generated a new DKIM key; add its DNS TXT record before sending mail (run 'mailserver dkim show' to print it again)",
			"selector", cfg.DKIM.Selector, "path", cfg.DKIM.PrivateKeyPath)
		for _, d := range cfg.Domains {
			logger.Info("DKIM DNS record", "host", fmt.Sprintf("%s._domainkey.%s", cfg.DKIM.Selector, d), "type", "TXT", "value", record)
		}
	}
	dkimSigner := dkimsign.Signer(cfg.DKIM.Selector, dkimKey)

	sender := &mailsend.Sender{Config: cfg, Store: db, Queue: mailQueue, Logger: logger, Sign: dkimSigner}

	// Brute-force protection. All three surfaces (SMTP AUTH, IMAP LOGIN,
	// webmail login) get their own limiter instance so a lockout on one
	// never affects the others. Real-IP resolution differs by surface: the
	// webmail server sits behind Traefik and needs X-Forwarded-For (only
	// trusted from security.trusted_proxies); SMTP/IMAP are reached
	// directly, so the raw TCP peer is authoritative unless a trusted
	// PROXY-protocol load balancer is configured (same trusted_proxies
	// list, applied via realip.WrapProxyProto below).
	rateLimitCfg := cfg.Security.AuthRateLimit
	newLimiter := func() *ratelimit.Limiter {
		return ratelimit.New(rateLimitCfg.MaxAttempts,
			time.Duration(rateLimitCfg.WindowMinutes)*time.Minute,
			time.Duration(rateLimitCfg.BanMinutes)*time.Minute)
	}
	submissionLimiter, imapLimiter, webmailLimiter := newLimiter(), newLimiter(), newLimiter()
	go submissionLimiter.Run(ctx, time.Minute)
	go imapLimiter.Run(ctx, time.Minute)
	go webmailLimiter.Run(ctx, time.Minute)

	trustedProxies, err := realip.Parse(cfg.Security.TrustedProxies)
	if err != nil {
		logger.Error("parsing security.trusted_proxies", "error", err)
		os.Exit(1)
	}

	submissionBackend := &smtpserver.SubmissionBackend{Config: cfg, Store: db, Logger: logger, Sender: sender, RateLimiter: submissionLimiter}
	submissionServer := smtp.NewServer(submissionBackend)
	submissionServer.Domain = cfg.Hostname
	submissionServer.TLSConfig = tlsConfig
	submissionServer.AllowInsecureAuth = false // AUTH requires STARTTLS first
	submissionServer.ReadTimeout = 5 * 60 * 1e9
	submissionServer.WriteTimeout = 5 * 60 * 1e9
	submissionServer.MaxMessageBytes = 32 * 1024 * 1024
	submissionServer.MaxRecipients = 100

	imapBackend := imapbackend.New(cfg, db, logger)
	imapBackend.RateLimiter = imapLimiter
	imapServer := imapserver.New(&imapserver.Options{
		NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return imapBackend.NewSession(c), nil, nil
		},
		Caps:      imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIMAP4rev2: {}},
		TLSConfig: tlsConfig,
	})

	// The webmail UI talks to imapBackend/sender directly, in-process — not
	// over a network connection to the listeners above — per the Phase 2
	// requirement that it not go over the wire to reach IMAP/SMTP.
	webmailServer := &http.Server{
		Addr:              cfg.Listen.Webmail,
		Handler:           webmail.New(imapBackend, sender, cfg.Hostname, logger, webmailLimiter, trustedProxies),
		ReadHeaderTimeout: 10 * time.Second,
	}

	submissionListener, err := net.Listen("tcp", cfg.Listen.Submission)
	if err != nil {
		logger.Error("listening for submission", "addr", cfg.Listen.Submission, "error", err)
		os.Exit(1)
	}
	submissionListener, err = realip.WrapProxyProto(submissionListener, cfg.Security.TrustedProxies)
	if err != nil {
		logger.Error("configuring PROXY protocol for submission", "error", err)
		os.Exit(1)
	}

	imapListener, err := net.Listen("tcp", cfg.Listen.IMAP)
	if err != nil {
		logger.Error("listening for IMAP", "addr", cfg.Listen.IMAP, "error", err)
		os.Exit(1)
	}
	imapListener, err = realip.WrapProxyProto(imapListener, cfg.Security.TrustedProxies)
	if err != nil {
		logger.Error("configuring PROXY protocol for IMAP", "error", err)
		os.Exit(1)
	}
	imapListener = tls.NewListener(imapListener, tlsConfig)

	metricsServer := &http.Server{
		Addr: cfg.Listen.Metrics,
		Handler: metrics.NewHandler(func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return db.Ping(ctx)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 5)
	go func() {
		logger.Info("SMTP (inbound) listening", "addr", cfg.Listen.SMTP)
		errCh <- inboundServer.ListenAndServe()
	}()
	go func() {
		logger.Info("SMTP (submission) listening", "addr", cfg.Listen.Submission)
		errCh <- submissionServer.Serve(submissionListener)
	}()
	go func() {
		logger.Info("IMAP listening", "addr", cfg.Listen.IMAP)
		errCh <- imapServer.Serve(imapListener)
	}()
	go func() {
		logger.Info("webmail listening", "addr", cfg.Listen.Webmail)
		errCh <- webmailServer.ListenAndServe()
	}()
	go func() {
		logger.Info("metrics/health listening", "addr", cfg.Listen.Metrics)
		errCh <- metricsServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, smtp.ErrServerClosed) && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server exited", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10e9)
		defer cancel()
		_ = inboundServer.Shutdown(shutdownCtx)
		_ = submissionServer.Shutdown(shutdownCtx)
		_ = imapServer.Close()
		_ = webmailServer.Shutdown(shutdownCtx)
		_ = metricsServer.Shutdown(shutdownCtx)
	}
}

// bounce delivers a non-delivery notification into the original sender's
// own INBOX, if they're a local user. There is nowhere to send it otherwise
// (no return-path relay is attempted for a bounce of a bounce).
func bounce(cfg *config.Config, db *store.Store, item queue.Item, original []byte, reason string) error {
	local, domain, ok := address.Split(item.From)
	if !ok || !cfg.IsLocalDomain(domain) {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "From: Mail Delivery System <mailer-daemon@%s>\r\n", cfg.Hostname)
	fmt.Fprintf(&b, "To: %s\r\n", item.From)
	fmt.Fprintf(&b, "Subject: Undelivered Mail Returned to Sender\r\n")
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Content-Type: text/plain; charset=utf-8\r\n\r\n")
	fmt.Fprintf(&b, "The following message could not be delivered to %s after %d attempt(s):\r\n\r\n%s\r\n\r\n", item.To, item.Attempts, reason)
	fmt.Fprintf(&b, "--- Original message follows ---\r\n\r\n")
	b.Write(original)

	root := maildir.UserRoot(cfg.Storage.MaildirPath, domain, local)
	md := maildir.New(maildir.FolderPath(root, "INBOX"))
	_, err := md.Deliver([]byte(b.String()))
	return err
}

// setupTLS picks a certificate source in priority order: a mounted
// cert/key pair, then DNS-01 ACME, then (development only) a self-signed
// certificate. For ACME, the initial certificate is obtained synchronously
// before returning so listeners never start without one; renewal continues
// in the background until ctx is canceled.
func setupTLS(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*tls.Config, error) {
	if cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != "" {
		return tlsutil.Load(cfg.TLS.CertFile, cfg.TLS.KeyFile, cfg.Hostname, logger)
	}

	if cfg.TLS.ACME.Enabled {
		mgr, err := acmedns.New(cfg.TLS.ACME, cfg.Domains, logger)
		if err != nil {
			return nil, fmt.Errorf("configuring ACME: %w", err)
		}
		if err := mgr.Init(); err != nil {
			return nil, fmt.Errorf("obtaining initial ACME certificate: %w", err)
		}
		go mgr.Run(ctx, 12*time.Hour)
		return mgr.TLSConfig(), nil
	}

	return tlsutil.Load("", "", cfg.Hostname, logger)
}

func cmdUser(args []string) {
	if len(args) < 1 {
		usageUser()
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		cmdUserCreate(args[1:])
	case "delete":
		cmdUserDelete(args[1:])
	default:
		usageUser()
		os.Exit(2)
	}
}

func usageUser() {
	fmt.Fprintln(os.Stderr, `usage:
  mailserver user create --config <path> --email user@domain.com --password secret [--admin]
  mailserver user delete --config <path> --email user@domain.com`)
}

func cmdUserCreate(args []string) {
	fs := flag.NewFlagSet("user create", flag.ExitOnError)
	path := fs.String("config", "/etc/mailserver/config.yaml", "path to config YAML file")
	email := fs.String("email", "", "mailbox email address, e.g. alice@example.com")
	password := fs.String("password", "", "mailbox password")
	admin := fs.Bool("admin", false, "grant admin privileges")
	fs.Parse(args)

	if *email == "" || *password == "" {
		fmt.Fprintln(os.Stderr, "--email and --password are required")
		os.Exit(2)
	}

	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading config: %v\n", err)
		os.Exit(1)
	}

	local, domain, ok := address.Split(*email)
	if !ok {
		fmt.Fprintf(os.Stderr, "invalid email address %q\n", *email)
		os.Exit(1)
	}
	if !cfg.IsLocalDomain(domain) {
		fmt.Fprintf(os.Stderr, "domain %q is not listed in config.domains\n", domain)
		os.Exit(1)
	}

	db, err := store.Open(cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opening store: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	u, err := db.CreateUser(context.Background(), domain, local, *password, *admin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating user: %v\n", err)
		os.Exit(1)
	}
	if err := imapbackend.ProvisionMailboxes(context.Background(), cfg, db, u); err != nil {
		fmt.Fprintf(os.Stderr, "provisioning mailboxes: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("created mailbox %s (id=%d, admin=%v)\n", u.Email, u.ID, u.IsAdmin)
}

func cmdUserDelete(args []string) {
	fs := flag.NewFlagSet("user delete", flag.ExitOnError)
	path := fs.String("config", "/etc/mailserver/config.yaml", "path to config YAML file")
	email := fs.String("email", "", "mailbox email address, e.g. alice@example.com")
	fs.Parse(args)

	if *email == "" {
		fmt.Fprintln(os.Stderr, "--email is required")
		os.Exit(2)
	}

	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading config: %v\n", err)
		os.Exit(1)
	}

	db, err := store.Open(cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opening store: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	u, err := db.GetUserByEmail(context.Background(), *email)
	if err != nil {
		fmt.Fprintf(os.Stderr, "looking up %q: %v\n", *email, err)
		os.Exit(1)
	}

	if err := db.DeleteUser(context.Background(), u.ID); err != nil {
		fmt.Fprintf(os.Stderr, "deleting user: %v\n", err)
		os.Exit(1)
	}

	local, domain, _ := address.Split(u.Email)
	root := maildir.UserRoot(cfg.Storage.MaildirPath, domain, local)
	if err := os.RemoveAll(root); err != nil {
		fmt.Fprintf(os.Stderr, "warning: deleted %s from the directory, but removing its Maildir at %s failed: %v\n", u.Email, root, err)
		os.Exit(1)
	}
	fmt.Printf("deleted mailbox %s\n", u.Email)
}
