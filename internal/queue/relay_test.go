package queue

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/sociolytik/mailserver/internal/tlsutil"
)

// recordingBackend is a minimal go-smtp backend that accepts one message
// and records its envelope + body, standing in for a remote MTA.
type recordingBackend struct {
	mu       sync.Mutex
	from     string
	to       []string
	body     []byte
	received bool
}

func (b *recordingBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &recordingSession{backend: b}, nil
}

type recordingSession struct {
	backend *recordingBackend
	from    string
	to      []string
}

func (s *recordingSession) Mail(from string, opts *smtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *recordingSession) Rcpt(to string, opts *smtp.RcptOptions) error {
	s.to = append(s.to, to)
	return nil
}

func (s *recordingSession) Data(r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.backend.mu.Lock()
	s.backend.from = s.from
	s.backend.to = s.to
	s.backend.body = body
	s.backend.received = true
	s.backend.mu.Unlock()
	return nil
}

func (s *recordingSession) Reset()        {}
func (s *recordingSession) Logout() error { return nil }

// TestTryAddrPlaintextFallback exercises the exact path tryHost takes when
// the remote doesn't support STARTTLS: DialStartTLS fails, so it falls back
// to a plaintext Dial, then must still call Hello with our configured
// hostname (not the library's "localhost" default) before MAIL/RCPT/DATA.
func TestTryAddrPlaintextFallback(t *testing.T) {
	backend := &recordingBackend{}
	server := smtp.NewServer(backend)
	server.Domain = "fake-remote.test"
	server.AllowInsecureAuth = true

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go server.Serve(ln)
	defer server.Close()

	q := &Queue{Hostname: "mail.sender.test", Logger: slog.Default()}

	data := []byte("Subject: test\r\n\r\nbody\r\n")
	err = q.tryAddr(ln.Addr().String(), "fake-remote.test", "alice@sender.test", "bob@remote.test", data)
	if err != nil {
		t.Fatalf("tryAddr: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		backend.mu.Lock()
		received := backend.received
		backend.mu.Unlock()
		if received || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if !backend.received {
		t.Fatal("remote MTA never received the message")
	}
	if backend.from != "alice@sender.test" {
		t.Errorf("MAIL FROM = %q, want alice@sender.test", backend.from)
	}
	if len(backend.to) != 1 || backend.to[0] != "bob@remote.test" {
		t.Errorf("RCPT TO = %v, want [bob@remote.test]", backend.to)
	}
	if !bytes.Equal(backend.body, data) {
		t.Errorf("body = %q, want %q", backend.body, data)
	}
}

// TestTryAddrOpportunisticTLS exercises the primary real-world path: the
// remote advertises STARTTLS, so DialStartTLS should succeed and the
// post-TLS EHLO should use our configured hostname.
func TestTryAddrOpportunisticTLS(t *testing.T) {
	tlsConfig, err := tlsutil.Load("", "", "fake-remote.test", slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("generating test TLS config: %v", err)
	}

	backend := &recordingBackend{}
	server := smtp.NewServer(backend)
	server.Domain = "fake-remote.test"
	server.TLSConfig = tlsConfig
	server.AllowInsecureAuth = true

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go server.Serve(ln)
	defer server.Close()

	q := &Queue{Hostname: "mail.sender.test", Logger: slog.Default()}

	data := []byte("Subject: test over tls\r\n\r\nbody\r\n")
	err = q.tryAddr(ln.Addr().String(), "fake-remote.test", "alice@sender.test", "bob@remote.test", data)
	if err != nil {
		t.Fatalf("tryAddr: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		backend.mu.Lock()
		received := backend.received
		backend.mu.Unlock()
		if received || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if !backend.received {
		t.Fatal("remote MTA never received the message over TLS")
	}
	if !bytes.Equal(backend.body, data) {
		t.Errorf("body = %q, want %q", backend.body, data)
	}
}

// authBackend is like recordingBackend but requires AUTH PLAIN with a
// specific username/password before accepting MAIL/RCPT/DATA — standing in
// for a real smart host (Brevo, etc.), which always requires auth.
type authBackend struct {
	recordingBackend
	wantUsername, wantPassword string
}

func (b *authBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &authSession{backend: b}, nil
}

type authSession struct {
	backend       *authBackend
	from          string
	to            []string
	authenticated bool
}

var _ smtp.AuthSession = (*authSession)(nil)

func (s *authSession) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *authSession) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		if username != s.backend.wantUsername || password != s.backend.wantPassword {
			return smtp.ErrAuthFailed
		}
		s.authenticated = true
		return nil
	}), nil
}

func (s *authSession) requireAuth() error {
	if !s.authenticated {
		return &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "Authentication required"}
	}
	return nil
}

func (s *authSession) Mail(from string, opts *smtp.MailOptions) error {
	if err := s.requireAuth(); err != nil {
		return err
	}
	s.from = from
	return nil
}

func (s *authSession) Rcpt(to string, opts *smtp.RcptOptions) error {
	if err := s.requireAuth(); err != nil {
		return err
	}
	s.to = append(s.to, to)
	return nil
}

func (s *authSession) Data(r io.Reader) error {
	if err := s.requireAuth(); err != nil {
		return err
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.backend.mu.Lock()
	s.backend.from = s.from
	s.backend.to = s.to
	s.backend.body = body
	s.backend.received = true
	s.backend.mu.Unlock()
	return nil
}

func (s *authSession) Reset()        {}
func (s *authSession) Logout() error { return nil }

// startAuthSmartHost starts a fake smart host requiring AUTH PLAIN, and
// returns a client-side tls.Config that trusts its self-signed certificate
// — trySmartHost verifies certificates for real (unlike direct-to-MX
// delivery's InsecureSkipVerify), so tests need to explicitly trust their
// own fake server's cert rather than relying on it being in a real CA pool.
func startAuthSmartHost(t *testing.T, username, password string) (backend *authBackend, ln net.Listener, clientTLSConfig *tls.Config) {
	t.Helper()
	serverTLSConfig, err := tlsutil.Load("", "", "smarthost.test", slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("generating test TLS config: %v", err)
	}

	leaf, err := x509.ParseCertificate(serverTLSConfig.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parsing generated test certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	backend = &authBackend{wantUsername: username, wantPassword: password}
	server := smtp.NewServer(backend)
	server.Domain = "smarthost.test"
	server.TLSConfig = serverTLSConfig
	server.AllowInsecureAuth = false

	ln, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go server.Serve(ln)
	t.Cleanup(func() { server.Close() })
	return backend, ln, &tls.Config{ServerName: "smarthost.test", RootCAs: pool}
}

func waitForReceived(backend *recordingBackend) bool {
	deadline := time.Now().Add(2 * time.Second)
	for {
		backend.mu.Lock()
		received := backend.received
		backend.mu.Unlock()
		if received || time.Now().After(deadline) {
			return received
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTrySmartHostAuthenticates confirms a smart host delivery actually
// performs AUTH with the configured credentials before sending the
// message — the whole point of a smart host is that it's authenticated,
// unlike direct-to-MX delivery.
func TestTrySmartHostAuthenticates(t *testing.T) {
	backend, ln, clientTLS := startAuthSmartHost(t, "brevo-user", "brevo-api-key")
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	q := &Queue{Hostname: "mail.sender.test", Logger: slog.Default()}
	cfg := &SmartHostConfig{Host: "127.0.0.1", Port: port, Username: "brevo-user", Password: "brevo-api-key"}

	data := []byte("Subject: via smart host\r\n\r\nbody\r\n")
	if err := q.trySmartHostWithTLS(cfg, clientTLS, "alice@sender.test", "bob@dest.test", data); err != nil {
		t.Fatalf("trySmartHostWithTLS: %v", err)
	}

	if !waitForReceived(&backend.recordingBackend) {
		t.Fatal("smart host never received the message")
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.from != "alice@sender.test" {
		t.Errorf("MAIL FROM = %q, want alice@sender.test", backend.from)
	}
	if len(backend.to) != 1 || backend.to[0] != "bob@dest.test" {
		t.Errorf("RCPT TO = %v, want [bob@dest.test]", backend.to)
	}
}

// TestTrySmartHostWrongCredentialsFails confirms bad credentials are
// actually rejected rather than the delivery silently succeeding anyway.
func TestTrySmartHostWrongCredentialsFails(t *testing.T) {
	_, ln, clientTLS := startAuthSmartHost(t, "brevo-user", "brevo-api-key")
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	q := &Queue{Hostname: "mail.sender.test", Logger: slog.Default()}
	cfg := &SmartHostConfig{Host: "127.0.0.1", Port: port, Username: "brevo-user", Password: "wrong-password"}

	err := q.trySmartHostWithTLS(cfg, clientTLS, "alice@sender.test", "bob@dest.test", []byte("Subject: x\r\n\r\nbody\r\n"))
	if err == nil {
		t.Fatal("expected an error for wrong smart host credentials, got nil")
	}
}

// TestDeliverUsesSmartHostWhenEnabled confirms deliver() routes through
// the SmartHost callback instead of attempting MX lookup when it reports
// enabled — the deciding branch an admin toggling this in the UI depends
// on. It calls the real (strict-cert) trySmartHost via deliver(), so a
// self-signed test cert is expected to fail — the point here is *which*
// error we get: a TLS/connection failure proves deliver() actually reached
// for the smart host, whereas ignoring SmartHost and trying MX lookup on
// "dest.test" (which has no real DNS records) would fail with a DNS error
// instead. Full successful delivery via trySmartHost is already covered by
// TestTrySmartHostAuthenticates.
func TestDeliverUsesSmartHostWhenEnabled(t *testing.T) {
	_, ln, _ := startAuthSmartHost(t, "brevo-user", "brevo-api-key")
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	q := &Queue{
		Hostname: "mail.sender.test",
		Logger:   slog.Default(),
		SmartHost: func(ctx context.Context) (*SmartHostConfig, bool, error) {
			return &SmartHostConfig{Host: "127.0.0.1", Port: port, Username: "brevo-user", Password: "brevo-api-key"}, true, nil
		},
	}

	err := q.deliver("alice@sender.test", "bob@dest.test", []byte("Subject: x\r\n\r\nbody\r\n"))
	if err == nil {
		t.Fatal("expected a certificate error (untrusted test cert), got nil")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("deliver() error = %q, want a certificate-verification error proving it reached the smart host (not a DNS lookup error, which would mean it ignored SmartHost)", err)
	}
}
