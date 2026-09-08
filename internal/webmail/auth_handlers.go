package webmail

import (
	"net/http"

	"github.com/sociolytik/mailserver/internal/metrics"
	"github.com/sociolytik/mailserver/internal/realip"
)

type loginData struct {
	Error  string
	Notice string
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil && s.sessions.get(cookie.Value) != nil {
		http.Redirect(w, r, "/mail/INBOX", http.StatusSeeOther)
		return
	}
	data := loginData{}
	if r.URL.Query().Get("password_changed") == "1" {
		data.Notice = "Password changed. Please sign in again."
	}
	render(w, "login_page", data)
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	email := r.FormValue("email")
	password := r.FormValue("password")

	clientIP := realip.FromHTTP(r, s.TrustedProxies)
	if s.RateLimiter != nil && !s.RateLimiter.Allowed(clientIP) {
		s.Logger.Warn("webmail login blocked by rate limiter", "ip", clientIP)
		metrics.AuthAttempts.WithLabelValues("webmail", "rate_limited").Inc()
		w.WriteHeader(http.StatusTooManyRequests)
		render(w, "login_page", loginData{Error: "Too many failed attempts. Try again later."})
		return
	}

	account, err := s.Backend.Authenticate(r.Context(), email, password)
	if err != nil {
		if s.RateLimiter != nil {
			s.RateLimiter.RecordFailure(clientIP)
		}
		s.Logger.Warn("webmail login failed", "ip", clientIP, "email", email)
		metrics.AuthAttempts.WithLabelValues("webmail", "failure").Inc()
		w.WriteHeader(http.StatusUnauthorized)
		render(w, "login_page", loginData{Error: "Invalid email or password"})
		return
	}
	if s.RateLimiter != nil {
		s.RateLimiter.RecordSuccess(clientIP)
	}
	metrics.AuthAttempts.WithLabelValues("webmail", "success").Inc()

	token, err := s.sessions.create(account)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	setSessionCookie(w, token)
	http.Redirect(w, r, "/mail/INBOX", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		s.sessions.delete(cookie.Value)
	}
	clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
