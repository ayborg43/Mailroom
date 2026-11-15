// Package push sends Web Push notifications ("you have new mail") to the
// browsers and installed PWAs users have enabled them on.
//
// The server holds a VAPID key pair (generated on first start, stored in the
// database); each device registers a subscription (endpoint + keys) through
// the webmail UI. When mail lands in a mailbox's INBOX, NewMail wakes that
// user's devices. Payloads are end-to-end encrypted to the device by
// github.com/SherClockHolmes/webpush-go and deliberately carry only the
// sender's name — never the subject or body.
package push

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/sociolytik/mailserver/internal/metrics"
	"github.com/sociolytik/mailserver/internal/store"
)

// Notifier is what the delivery paths (SMTP inbound, local submission
// delivery) call after writing a message to a mailbox's INBOX. rcpt is the
// mailbox address and msg the full RFC 5322 message.
type Notifier interface {
	NewMail(rcpt string, msg []byte)
}

// Payload is the JSON the service worker receives (see sw.js).
type Payload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Tag   string `json:"tag"`
	URL   string `json:"url"`
	Count int    `json:"count,omitempty"`
}

const (
	// pushTTL is how long the push service holds a notification for a device
	// that's offline. Stale "new mail" alerts aren't worth delivering days
	// later.
	pushTTL = 6 * 3600

	defaultCooldown = 10 * time.Second
	sendTimeout     = 15 * time.Second
)

// Service sends push notifications. Create one with New.
type Service struct {
	st      *store.Store
	logger  *slog.Logger
	keys    store.VAPIDKeys
	subject string
	client  webpush.HTTPClient

	cooldown time.Duration

	mu     sync.Mutex
	bursts map[string]*burst // keyed by mailbox address
}

// burst coalesces a flurry of mail for one mailbox into a single follow-up
// notification, so a mailing-list dump doesn't buzz the phone fifty times.
type burst struct {
	pending  int
	lastFrom string
}

// New loads the server's VAPID key pair from the database, generating and
// storing one on first use. subject is the contact the push services may use
// to reach the operator, as a mailto: or https: URL.
func New(ctx context.Context, st *store.Store, subject string, logger *slog.Logger) (*Service, error) {
	keys, err := st.GetVAPIDKeys(ctx)
	if errors.Is(err, store.ErrNotFound) {
		priv, pub, genErr := webpush.GenerateVAPIDKeys()
		if genErr != nil {
			return nil, fmt.Errorf("generating VAPID keys: %w", genErr)
		}
		keys, err = st.SaveVAPIDKeysIfAbsent(ctx, store.VAPIDKeys{PublicKey: pub, PrivateKey: priv})
		if err == nil {
			logger.Info("generated a new Web Push (VAPID) key pair")
		}
	}
	if err != nil {
		return nil, fmt.Errorf("loading VAPID keys: %w", err)
	}
	return &Service{
		st:       st,
		logger:   logger,
		keys:     *keys,
		subject:  subject,
		client:   &http.Client{Timeout: sendTimeout, Transport: safeTransport()},
		cooldown: defaultCooldown,
		bursts:   make(map[string]*burst),
	}, nil
}

// PublicKey is the application server key browsers need to subscribe
// (base64url, as passed to PushManager.subscribe).
func (s *Service) PublicKey() string { return s.keys.PublicKey }

// NewMail implements Notifier. It returns immediately; delivery happens in
// the background and never affects mail handling.
func (s *Service) NewMail(rcpt string, msg []byte) {
	from := SenderName(msg)
	key := strings.ToLower(rcpt)

	s.mu.Lock()
	if b, cooling := s.bursts[key]; cooling {
		b.pending++
		b.lastFrom = from
		s.mu.Unlock()
		return
	}
	s.bursts[key] = &burst{}
	s.mu.Unlock()

	time.AfterFunc(s.cooldown, func() { s.endCooldown(key) })
	go s.deliver(key, Payload{Title: from, Body: "New message", Tag: "new-mail", URL: "/mail/INBOX"})
}

// endCooldown runs when a mailbox's quiet period ends: if more mail arrived
// meanwhile, send one summary and start another quiet period.
func (s *Service) endCooldown(key string) {
	s.mu.Lock()
	b := s.bursts[key]
	if b == nil || b.pending == 0 {
		delete(s.bursts, key)
		s.mu.Unlock()
		return
	}
	n, from := b.pending, b.lastFrom
	b.pending = 0
	s.mu.Unlock()

	time.AfterFunc(s.cooldown, func() { s.endCooldown(key) })
	title := fmt.Sprintf("%d new messages", n)
	go s.deliver(key, Payload{Title: title, Body: "Latest from " + from, Tag: "new-mail", URL: "/mail/INBOX", Count: n})
}

// deliver pushes p to every device subscribed for the mailbox.
func (s *Service) deliver(email string, p Payload) {
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()

	_, subs, err := s.st.ListPushSubscriptionsByEmail(ctx, email)
	if err != nil {
		s.logger.Error("listing push subscriptions", "mailbox", email, "error", err)
		return
	}
	s.sendAll(ctx, subs, p)
}

// SendTest pushes a test notification to all of userID's devices and
// reports how many accepted it.
func (s *Service) SendTest(ctx context.Context, userID int64) (sent, total int, err error) {
	subs, err := s.st.ListPushSubscriptions(ctx, userID)
	if err != nil {
		return 0, 0, err
	}
	p := Payload{Title: "Notifications are on", Body: "You'll be alerted here when new mail arrives.", Tag: "test", URL: "/mail/INBOX"}
	return s.sendAll(ctx, subs, p), len(subs), nil
}

func (s *Service) sendAll(ctx context.Context, subs []store.PushSubscription, p Payload) int {
	if len(subs) == 0 {
		return 0
	}
	body, err := json.Marshal(p)
	if err != nil {
		return 0
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	sent := 0
	for _, sub := range subs {
		wg.Add(1)
		go func(sub store.PushSubscription) {
			defer wg.Done()
			if s.sendOne(ctx, sub, body) {
				mu.Lock()
				sent++
				mu.Unlock()
			}
		}(sub)
	}
	wg.Wait()
	return sent
}

func (s *Service) sendOne(ctx context.Context, sub store.PushSubscription, body []byte) bool {
	resp, err := webpush.SendNotificationWithContext(ctx, body, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth},
	}, &webpush.Options{
		HTTPClient:      s.client,
		Subscriber:      s.subject,
		VAPIDPublicKey:  s.keys.PublicKey,
		VAPIDPrivateKey: s.keys.PrivateKey,
		TTL:             pushTTL,
		Topic:           "newmail",
	})
	if err != nil {
		metrics.PushNotifications.WithLabelValues("failed").Inc()
		s.logger.Warn("push send failed", "host", hostOf(sub.Endpoint), "error", err)
		return false
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		metrics.PushNotifications.WithLabelValues("sent").Inc()
		return true
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		// The browser unsubscribed or the subscription expired.
		metrics.PushNotifications.WithLabelValues("expired").Inc()
		if err := s.st.DeletePushSubscriptionByEndpoint(context.Background(), sub.Endpoint); err != nil {
			s.logger.Error("removing expired push subscription", "error", err)
		}
		return false
	default:
		metrics.PushNotifications.WithLabelValues("failed").Inc()
		s.logger.Warn("push service rejected notification", "host", hostOf(sub.Endpoint), "status", resp.StatusCode)
		return false
	}
}

func hostOf(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil {
		return u.Host
	}
	return ""
}

// SenderName returns a short, display-safe name for who a message is from:
// the From header's display name, or its address if there is no name.
func SenderName(msg []byte) string {
	m, err := mail.ReadMessage(bytes.NewReader(msg))
	if err != nil {
		return "New message"
	}
	addrs, err := m.Header.AddressList("From")
	if err != nil || len(addrs) == 0 {
		return "New message"
	}
	name := addrs[0].Name
	if name == "" {
		name = addrs[0].Address
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" {
		return "New message"
	}
	if r := []rune(name); len(r) > 60 {
		name = string(r[:59]) + "…"
	}
	return name
}

// ValidateSubscription checks what a browser sent before we store it. The
// server will later POST to this endpoint, so it has to look like a real
// push service: https on the default port, no credentials in the URL. (The
// dialer additionally refuses to connect to private or loopback addresses —
// see safeTransport.)
func ValidateSubscription(endpoint, p256dh, auth string) error {
	if len(endpoint) > 2048 {
		return errors.New("endpoint too long")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return errors.New("invalid endpoint")
	}
	if u.Scheme != "https" {
		return errors.New("endpoint must be https")
	}
	if u.User != nil {
		return errors.New("endpoint must not contain credentials")
	}
	if port := u.Port(); port != "" && port != "443" {
		return errors.New("endpoint must use the default https port")
	}
	if key, err := b64(p256dh); err != nil || len(key) != 65 || key[0] != 4 {
		return errors.New("invalid p256dh key")
	}
	if a, err := b64(auth); err != nil || len(a) != 16 {
		return errors.New("invalid auth secret")
	}
	return nil
}

func b64(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	return base64.RawURLEncoding.DecodeString(s)
}

// safeTransport is an HTTP transport that only connects to public
// addresses. Subscription endpoints come from users' browsers, so without
// this an authenticated user could point one at an internal service and have
// the server POST to it. The check runs on the resolved address at connect
// time (not on the hostname), so DNS tricks don't get around it.
func safeTransport() *http.Transport {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: refuseNonPublic}
	return &http.Transport{
		DialContext:         d.DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		// No proxy: a proxy would make the dial-time check meaningless.
		Proxy: nil,
	}
}

func refuseNonPublic(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !isPublicIP(ip) {
		return fmt.Errorf("refusing to connect to non-public address %s", host)
	}
	return nil
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func isPublicIP(ip net.IP) bool {
	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsUnspecified(), ip.IsMulticast(),
		ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast():
		return false
	}
	return !cgnat.Contains(ip)
}
