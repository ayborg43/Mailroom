// Package webmail implements the Phase 2 server-rendered webmail UI
// (html/template + htmx). It talks to the mail store directly, in-process,
// via internal/imapbackend's Account facade and internal/mailsend's
// Sender — never by opening a network connection to the SMTP/IMAP
// listeners, so there's exactly one place ("the same Go objects the IMAP
// server itself uses") that owns mailbox state.
package webmail

import (
	"log/slog"
	"net/http"

	"github.com/sociolytik/mailserver/internal/imapbackend"
	"github.com/sociolytik/mailserver/internal/mailsend"
	"github.com/sociolytik/mailserver/internal/ratelimit"
	"github.com/sociolytik/mailserver/internal/realip"
)

const defaultPageSize = 25

type Server struct {
	Backend        *imapbackend.Backend
	Sender         *mailsend.Sender
	Hostname       string // used for the Message-Id domain of mail composed here
	Logger         *slog.Logger
	RateLimiter    *ratelimit.Limiter    // brute-force protection on /login
	TrustedProxies realip.TrustedProxies // who may supply X-Forwarded-For (normally just Traefik)

	sessions *sessionStore
	mux      *http.ServeMux
}

func New(backend *imapbackend.Backend, sender *mailsend.Sender, hostname string, logger *slog.Logger, rateLimiter *ratelimit.Limiter, trustedProxies realip.TrustedProxies) *Server {
	s := &Server{
		Backend:        backend,
		Sender:         sender,
		Hostname:       hostname,
		Logger:         logger,
		RateLimiter:    rateLimiter,
		TrustedProxies: trustedProxies,
		sessions:       newSessionStore(),
	}

	mux := http.NewServeMux()
	mux.Handle("/static/", staticHandler())

	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("GET /{$}", s.handleRoot)

	mux.HandleFunc("GET /mail/{folder}", s.requireAuth(s.handleFolder))
	mux.HandleFunc("GET /mail/{folder}/search", s.requireAuth(s.handleSearch))
	mux.HandleFunc("GET /mail/{folder}/{uid}", s.requireAuth(s.handleMessage))
	mux.HandleFunc("POST /mail/{folder}/{uid}/flag", s.requireAuth(s.handleFlag))
	mux.HandleFunc("POST /mail/{folder}/{uid}/delete", s.requireAuth(s.handleDelete))

	mux.HandleFunc("GET /compose", s.requireAuth(s.handleComposeForm))
	mux.HandleFunc("POST /compose", s.requireAuth(s.handleComposeSubmit))

	s.mux = mux
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/mail/INBOX", http.StatusSeeOther)
}

// accountHandler is a route handler for an endpoint that requires an
// authenticated session.
type accountHandler func(w http.ResponseWriter, r *http.Request, account *imapbackend.Account)

func (s *Server) requireAuth(h accountHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		account := s.sessions.get(cookie.Value)
		if account == nil {
			clearSessionCookie(w)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h(w, r, account)
	}
}
