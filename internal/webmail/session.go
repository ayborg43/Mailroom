package webmail

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sync"
	"time"

	"github.com/sociolytik/mailserver/internal/imapbackend"
)

const (
	sessionCookieName = "mailserver_session"
	sessionTTL        = 24 * time.Hour
)

type session struct {
	account *imapbackend.Account
	expires time.Time
}

// sessionStore is a plain in-memory session table. Since the webmail UI
// talks to the mail server in-process (no separate network hop), sessions
// only need to survive for the life of this one process — losing them on
// restart just means signing back in, same as any other web app's server
// restart with in-memory sessions.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*session
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]*session)}
}

func (s *sessionStore) create(account *imapbackend.Account) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.sessions[token] = &session{account: account, expires: time.Now().Add(sessionTTL)}
	s.mu.Unlock()
	return token, nil
}

func (s *sessionStore) get(token string) *imapbackend.Account {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[token]
	if !ok {
		return nil
	}
	if time.Now().After(sess.expires) {
		delete(s.sessions, token)
		return nil
	}
	return sess.account
}

func (s *sessionStore) delete(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}
