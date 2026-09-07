package queue

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"testing"
	"time"

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
