package webmail

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sociolytik/mailserver/internal/config"
	"github.com/sociolytik/mailserver/internal/imapbackend"
	"github.com/sociolytik/mailserver/internal/push"
	"github.com/sociolytik/mailserver/internal/store"
)

type testEnv struct {
	cfg    *config.Config
	db     *store.Store
	server *httptest.Server
	push   *push.Service
}

// newTestEnv builds a real webmail server over a throwaway database. With
// withPush false the Web Push feature is off, as when its setup failed.
func newTestEnv(t *testing.T, withPush bool) *testEnv {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Hostname: "mail.test",
		Domains:  []string{"example.com"},
		Storage:  config.StorageConfig{MaildirPath: filepath.Join(dir, "mail"), QueuePath: filepath.Join(dir, "queue")},
	}
	db, err := store.Open("sqlite", filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var pushSvc *push.Service
	if withPush {
		if pushSvc, err = push.New(context.Background(), db, "mailto:admin@example.com", logger); err != nil {
			t.Fatal(err)
		}
	}
	srv := New(imapbackend.New(cfg, db, logger), nil, cfg.Hostname, logger, nil, nil, pushSvc)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &testEnv{cfg: cfg, db: db, server: ts, push: pushSvc}
}

func (e *testEnv) addUser(t *testing.T, local string) *store.User {
	t.Helper()
	u, err := e.db.CreateUser(context.Background(), "example.com", local, "password123", false)
	if err != nil {
		t.Fatal(err)
	}
	// A real account has its mailboxes provisioned; the mail pages need them.
	if err := imapbackend.ProvisionMailboxes(context.Background(), e.cfg, e.db, u); err != nil {
		t.Fatal(err)
	}
	return u
}

// loggedInClient returns an HTTP client holding a session for the user.
func (e *testEnv) loggedInClient(t *testing.T, local string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.PostForm(e.server.URL+"/login", url.Values{"email": {local + "@example.com"}, "password": {"password123"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login for %s: status %d, want 303", local, resp.StatusCode)
	}
	return c
}

func postJSONBody(t *testing.T, c *http.Client, url, contentType, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func validSub(t *testing.T, endpoint string) string {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	b, _ := json.Marshal(map[string]any{
		"endpoint": endpoint,
		"keys": map[string]string{
			"p256dh": base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
			"auth":   base64.RawURLEncoding.EncodeToString(auth),
		},
	})
	return string(b)
}

func (e *testEnv) subCount(t *testing.T, local string) int {
	t.Helper()
	u, err := e.db.GetUserByEmail(context.Background(), local+"@example.com")
	if err != nil {
		t.Fatal(err)
	}
	n, _ := e.db.CountPushSubscriptions(context.Background(), u.ID)
	return n
}

func TestPushEndpointsRequireSession(t *testing.T) {
	env := newTestEnv(t, true)
	c := &http.Client{}
	for _, path := range []string{"/push/subscribe", "/push/unsubscribe", "/push/test"} {
		code, body := postJSONBody(t, c, env.server.URL+path, "application/json", "{}")
		if code != http.StatusUnauthorized {
			t.Errorf("%s without a session: status %d, want 401 (body %v)", path, code, body)
		}
	}
}

func TestPushSubscribeValidation(t *testing.T) {
	env := newTestEnv(t, true)
	env.addUser(t, "alice")
	c := env.loggedInClient(t, "alice")
	url := env.server.URL + "/push/subscribe"

	if code, _ := postJSONBody(t, c, url, "application/json", validSub(t, "https://push.example.net/abc")); code != 200 {
		t.Fatalf("valid subscribe: status %d", code)
	}
	if n := env.subCount(t, "alice"); n != 1 {
		t.Fatalf("subscription count = %d, want 1", n)
	}

	cases := []struct {
		name, contentType, body string
		want                    int
	}{
		{"form content type", "application/x-www-form-urlencoded", validSub(t, "https://push.example.net/x"), 415},
		{"no content type", "", validSub(t, "https://push.example.net/x"), 415},
		{"not json", "application/json", "nope", 400},
		{"http endpoint", "application/json", validSub(t, "http://push.example.net/x"), 400},
		{"loopback endpoint", "application/json", validSub(t, "https://127.0.0.1/x"), 200}, // accepted here; the sender's dialer refuses it
		{"bad keys", "application/json", `{"endpoint":"https://push.example.net/y","keys":{"p256dh":"x","auth":"y"}}`, 400},
	}
	for _, tc := range cases {
		if code, _ := postJSONBody(t, c, url, tc.contentType, tc.body); code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, code, tc.want)
		}
	}
}

func TestPushSubscribeDeviceLimitAndResubscribe(t *testing.T) {
	env := newTestEnv(t, true)
	env.addUser(t, "alice")
	c := env.loggedInClient(t, "alice")
	url := env.server.URL + "/push/subscribe"

	for i := 0; i < maxPushDevicesPerUser; i++ {
		ep := "https://push.example.net/dev" + string(rune('a'+i))
		if code, _ := postJSONBody(t, c, url, "application/json", validSub(t, ep)); code != 200 {
			t.Fatalf("device %d: status %d", i, code)
		}
	}
	if code, _ := postJSONBody(t, c, url, "application/json", validSub(t, "https://push.example.net/one-too-many")); code != http.StatusConflict {
		t.Errorf("over the limit: status %d, want 409", code)
	}
	// An already-registered device can still refresh its keys at the limit.
	if code, _ := postJSONBody(t, c, url, "application/json", validSub(t, "https://push.example.net/deva")); code != 200 {
		t.Errorf("re-subscribing a known device at the limit: status %d, want 200", code)
	}
	if n := env.subCount(t, "alice"); n != maxPushDevicesPerUser {
		t.Errorf("count = %d, want %d", n, maxPushDevicesPerUser)
	}
}

func TestPushUnsubscribeIsScopedToTheUser(t *testing.T) {
	env := newTestEnv(t, true)
	env.addUser(t, "alice")
	env.addUser(t, "bob")
	alice := env.loggedInClient(t, "alice")
	bob := env.loggedInClient(t, "bob")

	ep := "https://push.example.net/alice-phone"
	postJSONBody(t, alice, env.server.URL+"/push/subscribe", "application/json", validSub(t, ep))

	// Bob can't remove Alice's device (the call still "succeeds" — nothing of his to remove).
	postJSONBody(t, bob, env.server.URL+"/push/unsubscribe", "application/json", `{"endpoint":"`+ep+`"}`)
	if n := env.subCount(t, "alice"); n != 1 {
		t.Fatalf("bob removed alice's subscription (count=%d)", n)
	}
	if code, _ := postJSONBody(t, alice, env.server.URL+"/push/unsubscribe", "application/json", `{"endpoint":"`+ep+`"}`); code != 200 {
		t.Fatalf("alice unsubscribe: status %d", code)
	}
	if n := env.subCount(t, "alice"); n != 0 {
		t.Errorf("count = %d after unsubscribe", n)
	}
}

func TestPushDisabledWithoutService(t *testing.T) {
	env := newTestEnv(t, false)
	env.addUser(t, "alice")
	c := env.loggedInClient(t, "alice")

	if code, _ := postJSONBody(t, c, env.server.URL+"/push/subscribe", "application/json", validSub(t, "https://push.example.net/x")); code != http.StatusNotFound {
		t.Errorf("subscribe with push disabled: status %d, want 404", code)
	}
	resp, _ := c.Get(env.server.URL + "/account")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "data-push") {
		t.Error("account page shows a notifications section while push is disabled")
	}
}

func TestAccountPageOffersNotificationsWhenEnabled(t *testing.T) {
	env := newTestEnv(t, true)
	env.addUser(t, "alice")
	c := env.loggedInClient(t, "alice")
	resp, _ := c.Get(env.server.URL + "/account")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `data-vapid-key="`+env.push.PublicKey()+`"`) {
		t.Error("account page doesn't carry the VAPID public key")
	}
}

func TestManifest(t *testing.T) {
	env := newTestEnv(t, true)
	resp, err := http.Get(env.server.URL + "/manifest.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/manifest+json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var m struct {
		StartURL string `json:"start_url"`
		Scope    string `json:"scope"`
		Display  string `json:"display"`
		Icons    []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if m.Display != "standalone" || m.Scope != "/" || m.StartURL == "" {
		t.Errorf("manifest = %+v", m)
	}
	var maskable, any512 bool
	for _, ic := range m.Icons {
		if _, err := os.Stat(filepath.Join("static", strings.TrimPrefix(ic.Src, "/static/"))); err != nil {
			t.Errorf("manifest icon %s is missing from static/: %v", ic.Src, err)
		}
		maskable = maskable || ic.Purpose == "maskable"
		any512 = any512 || (ic.Sizes == "512x512" && ic.Purpose == "any")
	}
	if !maskable || !any512 {
		t.Errorf("icons lack a maskable or a 512px 'any' entry: %+v", m.Icons)
	}
}

func TestServiceWorkerAndOfflinePage(t *testing.T) {
	env := newTestEnv(t, true)

	resp, err := http.Get(env.server.URL + "/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	js := string(body)
	if resp.Header.Get("Service-Worker-Allowed") != "/" || !strings.Contains(resp.Header.Get("Cache-Control"), "no-cache") {
		t.Errorf("headers = %v", resp.Header)
	}
	if strings.Contains(js, "__VERSION__") || strings.Contains(js, "__PRECACHE__") {
		t.Error("service worker still has unreplaced placeholders")
	}
	if !strings.Contains(js, `"/offline"`) || !strings.Contains(js, "/static/icons/icon-192.png") {
		t.Error("precache list is missing the offline page or icons")
	}

	off, err := http.Get(env.server.URL + "/offline")
	if err != nil {
		t.Fatal(err)
	}
	offBody, _ := io.ReadAll(off.Body)
	off.Body.Close()
	if off.StatusCode != 200 || !strings.Contains(string(offBody), "offline") {
		t.Errorf("/offline: status %d", off.StatusCode)
	}
	// It must be reachable without a session (the worker fetches it at install).
	if strings.Contains(string(offBody), "Sign in") {
		t.Error("/offline redirected to or rendered the login page")
	}
}

func TestEveryPageLinksTheManifest(t *testing.T) {
	env := newTestEnv(t, true)
	env.addUser(t, "alice")
	c := env.loggedInClient(t, "alice")
	for _, path := range []string{"/login", "/offline", "/mail/INBOX", "/account", "/compose"} {
		client := c
		if path == "/login" || path == "/offline" {
			client = &http.Client{} // signed-out: /login would redirect a signed-in user away
		}
		resp, err := client.Get(env.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body), `rel="manifest"`) {
			t.Errorf("%s: status %d, links manifest: %v", path, resp.StatusCode, strings.Contains(string(body), `rel="manifest"`))
		}
	}
}
