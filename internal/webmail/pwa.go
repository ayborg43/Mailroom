package webmail

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// PWA support: a web app manifest, the service worker, and an offline page.
// See sw.js for what the service worker does (and doesn't) cache.

//go:embed sw.js
var swTemplate string

var (
	swOnce   sync.Once
	swSource string
)

// precacheURLs lists what the service worker stores at install: the built
// JS/CSS (content-hashed, so a deploy changes the list and therefore the
// worker's version), the offline page, and the icons it references.
func precacheURLs() []string {
	urls := []string{"/offline", "/static/icons/icon-192.png", "/static/icons/badge-96.png"}
	urls = append(urls, assets.files()...)
	sort.Strings(urls)
	return urls
}

// serviceWorkerSource returns sw.js with its version and precache list
// filled in. The version is a hash of the list, so the browser sees a new
// worker (and refreshes the cache) exactly when the built assets change.
func serviceWorkerSource() string {
	swOnce.Do(func() {
		urls := precacheURLs()
		list, _ := json.Marshal(urls)
		sum := sha256.Sum256([]byte(strings.Join(urls, "\n")))
		version := hex.EncodeToString(sum[:])[:12]
		r := strings.NewReplacer("__VERSION__", version, "__PRECACHE__", string(list))
		swSource = r.Replace(swTemplate)
	})
	return swSource
}

func (s *Server) handleServiceWorker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	// The browser re-checks the worker script on every navigation anyway;
	// no-cache keeps intermediaries from holding a stale one.
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Service-Worker-Allowed", "/")
	_, _ = w.Write([]byte(serviceWorkerSource()))
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	icon := func(file, size, purpose string) map[string]string {
		m := map[string]string{"src": "/static/icons/" + file, "sizes": size, "type": "image/png"}
		if purpose != "" {
			m["purpose"] = purpose
		}
		return m
	}
	manifest := map[string]any{
		"name":             "Mail",
		"short_name":       "Mail",
		"description":      "Webmail for " + s.Hostname,
		"id":               "/",
		"start_url":        "/mail/INBOX",
		"scope":            "/",
		"display":          "standalone",
		"orientation":      "any",
		"theme_color":      "#2563eb",
		"background_color": "#ffffff",
		"categories":       []string{"productivity", "communication"},
		"icons": []map[string]string{
			icon("icon-192.png", "192x192", "any"),
			icon("icon-512.png", "512x512", "any"),
			icon("icon-maskable-512.png", "512x512", "maskable"),
		},
		"shortcuts": []map[string]any{
			{"name": "Compose", "url": "/compose", "icons": []map[string]string{icon("icon-192.png", "192x192", "")}},
			{"name": "Inbox", "url": "/mail/INBOX", "icons": []map[string]string{icon("icon-192.png", "192x192", "")}},
		},
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(manifest)
}

func (s *Server) handleOffline(w http.ResponseWriter, r *http.Request) {
	render(w, "offline_page", nil)
}
