// Package imapbackend implements the imapserver.Session interface backed by
// Maildir-on-disk storage and the user/mailbox directory in internal/store.
//
// It's structured after github.com/emersion/go-imap/v2/imapserver/imapmemserver,
// swapping that package's purely in-memory message slice for one backed by
// a Maildir directory (message bytes) plus a small SQL-backed UID index
// (internal/store) that Maildir's on-disk format has no room for.
package imapbackend

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"

	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/sociolytik/mailserver/internal/config"
	"github.com/sociolytik/mailserver/internal/metrics"
	"github.com/sociolytik/mailserver/internal/ratelimit"
	"github.com/sociolytik/mailserver/internal/store"
)

// Backend is the top-level IMAP server backend: one per running mailserver
// process, shared by every connection.
type Backend struct {
	Config *config.Config
	Store  *store.Store
	Logger *slog.Logger

	// RateLimiter, if set, throttles LOGIN attempts per client IP. Like
	// SMTP submission, this listener is reached directly — Traefik can't
	// proxy IMAP — so the TCP peer is the real client unless a trusted
	// PROXY-protocol load balancer sits in front (see
	// internal/realip.WrapProxyProto).
	RateLimiter *ratelimit.Limiter

	mu    sync.Mutex
	users map[string]*User // keyed by lowercased email
}

func New(cfg *config.Config, db *store.Store, logger *slog.Logger) *Backend {
	return &Backend{Config: cfg, Store: db, Logger: logger, users: make(map[string]*User)}
}

// NewSession implements imapserver.Backend-like session creation; wire it
// up via imapserver.Options.NewSession.
func (b *Backend) NewSession(conn *imapserver.Conn) imapserver.Session {
	return &serverSession{backend: b, conn: conn}
}

func (b *Backend) userFor(dbUser *store.User) *User {
	b.mu.Lock()
	defer b.mu.Unlock()

	email := strings.ToLower(dbUser.Email)
	if u, ok := b.users[email]; ok {
		return u
	}
	u := newUser(b, dbUser)
	b.users[email] = u
	return u
}

type serverSession struct {
	*UserSession // nil until Login succeeds

	backend *Backend
	conn    *imapserver.Conn
}

var _ imapserver.Session = (*serverSession)(nil)

func (sess *serverSession) clientIP() string {
	if sess.conn == nil || sess.conn.NetConn() == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(sess.conn.NetConn().RemoteAddr().String())
	if err != nil {
		return ""
	}
	return host
}

func (sess *serverSession) Login(username, password string) error {
	ip := sess.clientIP()
	if sess.backend.RateLimiter != nil && ip != "" && !sess.backend.RateLimiter.Allowed(ip) {
		sess.backend.Logger.Warn("IMAP LOGIN blocked by rate limiter", "ip", ip)
		metrics.AuthAttempts.WithLabelValues("imap", "rate_limited").Inc()
		return imapserver.ErrAuthFailed
	}

	email := strings.ToLower(strings.TrimSpace(username))

	dbUser, err := sess.backend.Store.GetUserByEmail(context.Background(), email)
	if err != nil || !store.VerifyPassword(dbUser, password) {
		if sess.backend.RateLimiter != nil && ip != "" {
			sess.backend.RateLimiter.RecordFailure(ip)
		}
		sess.backend.Logger.Warn("IMAP LOGIN failed", "ip", ip, "email", email)
		metrics.AuthAttempts.WithLabelValues("imap", "failure").Inc()
		return imapserver.ErrAuthFailed
	}

	if sess.backend.RateLimiter != nil && ip != "" {
		sess.backend.RateLimiter.RecordSuccess(ip)
	}
	metrics.AuthAttempts.WithLabelValues("imap", "success").Inc()

	sess.UserSession = newUserSession(sess.backend.userFor(dbUser))
	return nil
}

func (sess *serverSession) Close() error {
	if sess.UserSession != nil {
		return sess.UserSession.Close()
	}
	return nil
}
