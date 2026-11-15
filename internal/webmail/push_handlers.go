package webmail

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/sociolytik/mailserver/internal/imapbackend"
	"github.com/sociolytik/mailserver/internal/push"
)

// maxPushDevicesPerUser bounds how many devices one mailbox can register, so
// a runaway script can't fill the table.
const maxPushDevicesPerUser = 20

type pushSubscribeRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

type pushUnsubscribeRequest struct {
	Endpoint string `json:"endpoint"`
}

// requireAuthAPI is requireAuth for JSON endpoints: an expired session gets a
// 401 the page's script can act on, instead of a redirect to the login page
// that fetch() would silently follow.
func (s *Server) requireAuthAPI(h accountHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "not signed in"})
			return
		}
		account := s.sessions.get(cookie.Value)
		if account == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "session expired"})
			return
		}
		h(w, r, account)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeJSONBody reads a small JSON request body. Requiring the JSON content
// type means a cross-site HTML form (which can only send form encodings)
// can't reach these state-changing endpoints, on top of the session cookie
// being SameSite=Strict.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{"error": "expected application/json"})
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return false
	}
	return true
}

func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request, account *imapbackend.Account) {
	var req pushSubscribeRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if err := push.ValidateSubscription(req.Endpoint, req.Keys.P256dh, req.Keys.Auth); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	ctx := r.Context()
	st := s.Backend.Store
	existing, err := st.ListPushSubscriptions(ctx, account.ID())
	if err != nil {
		s.Logger.Error("listing push subscriptions", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	known := false
	for _, sub := range existing {
		if sub.Endpoint == req.Endpoint {
			known = true
			break
		}
	}
	if !known && len(existing) >= maxPushDevicesPerUser {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "too many devices have notifications enabled; turn some off first"})
		return
	}

	ua := r.UserAgent()
	if len(ua) > 300 {
		ua = ua[:300]
	}
	if err := st.UpsertPushSubscription(ctx, account.ID(), req.Endpoint, req.Keys.P256dh, req.Keys.Auth, ua); err != nil {
		s.Logger.Error("saving push subscription", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request, account *imapbackend.Account) {
	var req pushUnsubscribeRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if _, err := s.Backend.Store.DeletePushSubscription(r.Context(), account.ID(), req.Endpoint); err != nil {
		s.Logger.Error("removing push subscription", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	// Removing one that's already gone is fine: the end state is the same.
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request, account *imapbackend.Account) {
	ctx, cancel := contextWithTimeout(r, 20*time.Second)
	defer cancel()
	sent, total, err := s.Push.SendTest(ctx, account.ID())
	if err != nil {
		s.Logger.Error("sending test push", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": sent, "total": total})
}

func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}
