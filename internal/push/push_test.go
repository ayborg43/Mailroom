package push

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/hkdf"

	"github.com/sociolytik/mailserver/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open("sqlite", t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// device simulates a browser's push subscription: it owns the keypair and
// auth secret, and can decrypt what the server sends (RFC 8291).
type device struct {
	priv   *ecdh.PrivateKey
	auth   []byte
	p256dh string
	authB  string
}

func newDevice(t *testing.T) *device {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return &device{
		priv:   priv,
		auth:   auth,
		p256dh: base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
		authB:  base64.RawURLEncoding.EncodeToString(auth),
	}
}

func hk(secret, salt, info []byte, n int) []byte {
	out := make([]byte, n)
	_, _ = io.ReadFull(hkdf.New(sha256.New, secret, salt, info), out)
	return out
}

func (d *device) decrypt(t *testing.T, body []byte) []byte {
	t.Helper()
	if len(body) < 21 {
		t.Fatalf("body too short: %d", len(body))
	}
	salt := body[:16]
	_ = binary.BigEndian.Uint32(body[16:20]) // record size
	idLen := int(body[20])
	asPubBytes := body[21 : 21+idLen]
	ciphertext := body[21+idLen:]

	asPub, err := ecdh.P256().NewPublicKey(asPubBytes)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := d.priv.ECDH(asPub)
	if err != nil {
		t.Fatal(err)
	}
	keyInfo := append([]byte("WebPush: info\x00"), d.priv.PublicKey().Bytes()...)
	keyInfo = append(keyInfo, asPubBytes...)
	ikm := hk(secret, d.auth, keyInfo, 32)
	cek := hk(ikm, salt, []byte("Content-Encoding: aes128gcm\x00"), 16)
	nonce := hk(ikm, salt, []byte("Content-Encoding: nonce\x00"), 12)

	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatalf("decrypting push payload: %v", err)
	}
	// Strip RFC 8188 padding: payload || 0x02 || 0x00...
	plain = bytes.TrimRight(plain, "\x00")
	return plain[:len(plain)-1]
}

type received struct {
	header http.Header
	body   []byte
}

// fakePushService records what the server POSTs and answers with status.
func fakePushService(t *testing.T, status int) (*httptest.Server, func() []received) {
	t.Helper()
	var mu sync.Mutex
	var got []received
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, received{header: r.Header.Clone(), body: b})
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []received {
		mu.Lock()
		defer mu.Unlock()
		return append([]received(nil), got...)
	}
}

func newService(t *testing.T, db *store.Store) *Service {
	t.Helper()
	svc, err := New(context.Background(), db, "mailto:admin@example.com", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// Tests talk to a loopback fake, which the production transport refuses.
	svc.client = http.DefaultClient
	svc.cooldown = 50 * time.Millisecond
	return svc
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}

func subscribe(t *testing.T, db *store.Store, email, endpoint string, d *device) int64 {
	t.Helper()
	ctx := context.Background()
	u, err := db.GetUserByEmail(ctx, email)
	if err != nil {
		local, domain, _ := strings.Cut(email, "@")
		if u, err = db.CreateUser(ctx, domain, local, "password123", false); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpsertPushSubscription(ctx, u.ID, endpoint, d.p256dh, d.authB, "test"); err != nil {
		t.Fatal(err)
	}
	return u.ID
}

const sampleMail = "From: \"Grace Hopper\" <grace@example.org>\r\nTo: alice@example.com\r\nSubject: Secret subject line\r\n\r\nSecret body text\r\n"

func TestNewMailSendsEncryptedSenderOnlyPayload(t *testing.T) {
	db := newTestStore(t)
	svc := newService(t, db)
	srv, requests := fakePushService(t, http.StatusCreated)
	dev := newDevice(t)
	subscribe(t, db, "alice@example.com", srv.URL+"/push/abc", dev)

	svc.NewMail("Alice@Example.com", []byte(sampleMail))
	waitFor(t, func() bool { return len(requests()) == 1 })

	req := requests()[0]
	if auth := req.header.Get("Authorization"); !strings.HasPrefix(auth, "vapid t=") || !strings.Contains(auth, "k="+svc.PublicKey()) {
		t.Errorf("Authorization = %q, want a VAPID header with our public key", auth)
	}
	if req.header.Get("Content-Encoding") != "aes128gcm" {
		t.Errorf("Content-Encoding = %q", req.header.Get("Content-Encoding"))
	}
	if req.header.Get("TTL") == "" || req.header.Get("Topic") != "newmail" {
		t.Errorf("TTL=%q Topic=%q", req.header.Get("TTL"), req.header.Get("Topic"))
	}

	plain := dev.decrypt(t, req.body)
	var p Payload
	if err := json.Unmarshal(plain, &p); err != nil {
		t.Fatalf("payload is not JSON: %v (%q)", err, plain)
	}
	if p.Title != "Grace Hopper" || p.URL != "/mail/INBOX" || p.Tag != "new-mail" {
		t.Errorf("payload = %+v", p)
	}
	if strings.Contains(string(plain), "Secret") {
		t.Errorf("payload leaked the subject or body: %s", plain)
	}
}

func TestBurstIsCoalescedIntoOneSummary(t *testing.T) {
	db := newTestStore(t)
	svc := newService(t, db)
	srv, requests := fakePushService(t, http.StatusCreated)
	dev := newDevice(t)
	subscribe(t, db, "alice@example.com", srv.URL+"/p", dev)

	for i := 0; i < 5; i++ {
		svc.NewMail("alice@example.com", []byte(sampleMail))
	}
	// One immediately, then one summary when the quiet period ends.
	waitFor(t, func() bool { return len(requests()) == 2 })
	time.Sleep(150 * time.Millisecond)
	if n := len(requests()); n != 2 {
		t.Fatalf("got %d pushes for a burst of 5, want 2", n)
	}

	var first, second Payload
	_ = json.Unmarshal(dev.decrypt(t, requests()[0].body), &first)
	_ = json.Unmarshal(dev.decrypt(t, requests()[1].body), &second)
	if first.Title != "Grace Hopper" {
		t.Errorf("first = %+v", first)
	}
	if second.Title != "4 new messages" || second.Count != 4 {
		t.Errorf("second = %+v, want a 4-message summary", second)
	}
}

func TestExpiredSubscriptionIsRemoved(t *testing.T) {
	db := newTestStore(t)
	svc := newService(t, db)
	srv, requests := fakePushService(t, http.StatusGone)
	uid := subscribe(t, db, "alice@example.com", srv.URL+"/gone", newDevice(t))

	svc.NewMail("alice@example.com", []byte(sampleMail))
	waitFor(t, func() bool { return len(requests()) == 1 })
	waitFor(t, func() bool {
		n, _ := db.CountPushSubscriptions(context.Background(), uid)
		return n == 0
	})
}

func TestServerErrorKeepsSubscription(t *testing.T) {
	db := newTestStore(t)
	svc := newService(t, db)
	srv, requests := fakePushService(t, http.StatusInternalServerError)
	uid := subscribe(t, db, "alice@example.com", srv.URL+"/err", newDevice(t))

	svc.NewMail("alice@example.com", []byte(sampleMail))
	waitFor(t, func() bool { return len(requests()) == 1 })
	time.Sleep(50 * time.Millisecond)
	if n, _ := db.CountPushSubscriptions(context.Background(), uid); n != 1 {
		t.Errorf("a transient push-service error removed the subscription (count=%d)", n)
	}
}

func TestNoSubscriptionsNoRequests(t *testing.T) {
	db := newTestStore(t)
	svc := newService(t, db)
	srv, requests := fakePushService(t, http.StatusCreated)
	subscribe(t, db, "alice@example.com", srv.URL+"/p", newDevice(t))

	svc.NewMail("bob@example.com", []byte(sampleMail)) // bob has none
	time.Sleep(150 * time.Millisecond)
	if n := len(requests()); n != 0 {
		t.Errorf("got %d requests for a mailbox with no subscriptions", n)
	}
}

func TestSendTest(t *testing.T) {
	db := newTestStore(t)
	svc := newService(t, db)
	srv, requests := fakePushService(t, http.StatusCreated)
	dev := newDevice(t)
	uid := subscribe(t, db, "alice@example.com", srv.URL+"/p", dev)

	sent, total, err := svc.SendTest(context.Background(), uid)
	if err != nil || sent != 1 || total != 1 {
		t.Fatalf("SendTest = %d/%d, %v", sent, total, err)
	}
	var p Payload
	_ = json.Unmarshal(dev.decrypt(t, requests()[0].body), &p)
	if p.Tag != "test" {
		t.Errorf("payload = %+v", p)
	}
}

func TestVAPIDKeysPersistAcrossRestarts(t *testing.T) {
	db := newTestStore(t)
	a := newService(t, db)
	b := newService(t, db)
	if a.PublicKey() == "" || a.PublicKey() != b.PublicKey() {
		t.Errorf("keys differ across New calls: %q vs %q", a.PublicKey(), b.PublicKey())
	}
}

func TestSenderName(t *testing.T) {
	tests := []struct{ name, msg, want string }{
		{"display name", "From: Ada Lovelace <ada@x.org>\r\n\r\nhi", "Ada Lovelace"},
		{"quoted name", "From: \"Doe, Jo\" <jo@x.org>\r\n\r\nhi", "Doe, Jo"},
		{"bare address", "From: bare@x.org\r\n\r\nhi", "bare@x.org"},
		{"encoded word", "From: =?UTF-8?B?w4lsw6g=?= <e@x.org>\r\n\r\nhi", "Élè"},
		{"missing", "Subject: x\r\n\r\nhi", "New message"},
		{"garbage", "not a message", "New message"},
		{"long name", "From: " + strings.Repeat("A", 200) + " <a@x.org>\r\n\r\nhi", strings.Repeat("A", 59) + "…"},
	}
	for _, tc := range tests {
		if got := SenderName([]byte(tc.msg)); got != tc.want {
			t.Errorf("%s: SenderName = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestValidateSubscription(t *testing.T) {
	d := newDevice(t)
	ok := "https://fcm.googleapis.com/fcm/send/abc"
	if err := ValidateSubscription(ok, d.p256dh, d.authB); err != nil {
		t.Fatalf("valid subscription rejected: %v", err)
	}
	bad := map[string]string{
		"http scheme":   "http://fcm.googleapis.com/x",
		"credentials":   "https://user:pw@fcm.googleapis.com/x",
		"odd port":      "https://fcm.googleapis.com:8443/x",
		"no host":       "https:///x",
		"not a url":     "::::",
		"too long":      "https://a.example/" + strings.Repeat("x", 3000),
		"file scheme":   "file:///etc/passwd",
		"javascript:":   "javascript:alert(1)",
		"empty":         "",
		"ftp":           "ftp://a.example/x",
		"loopback http": "http://127.0.0.1/x",
	}
	for name, endpoint := range bad {
		if err := ValidateSubscription(endpoint, d.p256dh, d.authB); err == nil {
			t.Errorf("%s: endpoint %q was accepted", name, endpoint)
		}
	}
	if err := ValidateSubscription(ok, "short", d.authB); err == nil {
		t.Error("short p256dh accepted")
	}
	if err := ValidateSubscription(ok, d.p256dh, "short"); err == nil {
		t.Error("short auth accepted")
	}
}

func TestIsPublicIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.5", "172.16.9.9", "169.254.169.254", "100.64.1.1", "0.0.0.0", "::1", "fe80::1", "fc00::1", "224.0.0.1"} {
		if isPublicIP(net.ParseIP(s)) {
			t.Errorf("%s treated as public", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "142.250.80.46", "2607:f8b0:4004:c07::5f"} {
		if !isPublicIP(net.ParseIP(s)) {
			t.Errorf("%s treated as non-public", s)
		}
	}
}

func TestProductionTransportRefusesLoopback(t *testing.T) {
	srv, requests := fakePushService(t, http.StatusCreated)
	client := &http.Client{Transport: safeTransport(), Timeout: 3 * time.Second}
	if _, err := client.Post(srv.URL, "text/plain", strings.NewReader("x")); err == nil {
		t.Fatal("the production transport connected to a loopback address")
	}
	if n := len(requests()); n != 0 {
		t.Errorf("fake push service received %d requests despite the block", n)
	}
}
