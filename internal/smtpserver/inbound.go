// Package smtpserver implements the inbound (port 25) and submission
// (port 587) SMTP backends on top of github.com/emersion/go-smtp.
package smtpserver

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/sociolytik/mailserver/internal/address"
	"github.com/sociolytik/mailserver/internal/config"
	"github.com/sociolytik/mailserver/internal/inboundauth"
	"github.com/sociolytik/mailserver/internal/maildir"
	"github.com/sociolytik/mailserver/internal/metrics"
	"github.com/sociolytik/mailserver/internal/store"
)

// InboundBackend accepts unauthenticated mail addressed to local domains
// only (port 25). It never relays: any recipient outside Config.Domains, or
// any local address without a matching mailbox, is rejected at RCPT time.
type InboundBackend struct {
	Config *config.Config
	Store  *store.Store
	Logger *slog.Logger
}

func (b *InboundBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &inboundSession{backend: b, conn: c}, nil
}

type inboundSession struct {
	backend *InboundBackend
	conn    *smtp.Conn

	from  string
	rcpts []string
}

func (s *inboundSession) Mail(from string, opts *smtp.MailOptions) error {
	s.from = from
	s.rcpts = nil
	return nil
}

func (s *inboundSession) Rcpt(to string, opts *smtp.RcptOptions) error {
	addr := strings.ToLower(strings.TrimSpace(to))
	_, domain, ok := address.Split(addr)
	if !ok {
		return &smtp.SMTPError{Code: 501, EnhancedCode: smtp.EnhancedCode{5, 1, 3}, Message: "Malformed recipient address"}
	}

	if !s.backend.Config.IsLocalDomain(domain) {
		metrics.SMTPMessages.WithLabelValues("inbound", "rejected_relay").Inc()
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "Relay access denied"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.backend.Store.GetUserByEmail(ctx, addr); err != nil {
		metrics.SMTPMessages.WithLabelValues("inbound", "rejected_unknown_user").Inc()
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "No such user here"}
	}

	s.rcpts = append(s.rcpts, addr)
	return nil
}

func (s *inboundSession) Data(r io.Reader) error {
	if len(s.rcpts) == 0 {
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "No valid recipients"}
	}

	body, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("reading message body: %w", err)
	}

	verifyResult := inboundauth.Verify(s.backend.Config.Hostname, s.clientIP(), s.helo(), s.from, body)

	folder := "INBOX"
	if verifyResult.ShouldQuarantine {
		folder = "Junk"
	}

	for _, rcpt := range s.rcpts {
		local, domain, _ := address.Split(rcpt)

		var full []byte
		full = append(full, []byte(s.receivedHeader(rcpt))...)
		full = append(full, []byte(fmt.Sprintf("Authentication-Results: %s\r\n", verifyResult.AuthenticationResults))...)
		full = append(full, body...)

		root := maildir.UserRoot(s.backend.Config.Storage.MaildirPath, domain, local)
		md := maildir.New(maildir.FolderPath(root, folder))
		if _, err := md.Deliver(full); err != nil {
			s.backend.Logger.Error("maildir delivery failed", "recipient", rcpt, "folder", folder, "error", err)
			metrics.SMTPMessages.WithLabelValues("inbound", "delivery_failed").Inc()
			return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary delivery failure, please retry"}
		}
		s.backend.Logger.Info("delivered inbound message", "from", s.from, "to", rcpt, "folder", folder, "bytes", len(full))
		if verifyResult.ShouldQuarantine {
			metrics.SMTPMessages.WithLabelValues("inbound", "quarantined").Inc()
		} else {
			metrics.SMTPMessages.WithLabelValues("inbound", "accepted").Inc()
		}
	}

	return nil
}

func (s *inboundSession) receivedHeader(rcpt string) string {
	remoteHost := "unknown"
	if ip := s.clientIP(); ip != nil {
		remoteHost = ip.String()
	}
	return fmt.Sprintf(
		"Received: from %s ([%s])\r\n\tby %s (mailserver) with ESMTP\r\n\tfor <%s>; %s\r\n",
		s.helo(), remoteHost, s.backend.Config.Hostname, rcpt, time.Now().Format(time.RFC1123Z),
	)
}

func (s *inboundSession) helo() string {
	if s.conn == nil {
		return ""
	}
	return s.conn.Hostname()
}

// clientIP returns the connecting MTA's IP address, for SPF checking and
// the Received trace header. This is the raw TCP peer, not a
// reverse-proxy-forwarded address: Traefik can't proxy SMTP, so nothing
// sits between this listener and the real client (unlike the webmail HTTP
// server — see internal/realip for that distinction).
func (s *inboundSession) clientIP() net.IP {
	if s.conn == nil || s.conn.Conn() == nil {
		return nil
	}
	host, _, err := net.SplitHostPort(s.conn.Conn().RemoteAddr().String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

func (s *inboundSession) Reset() {
	s.from = ""
	s.rcpts = nil
}

func (s *inboundSession) Logout() error {
	return nil
}
