package smtpserver

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/sociolytik/mailserver/internal/address"
	"github.com/sociolytik/mailserver/internal/config"
	"github.com/sociolytik/mailserver/internal/mailsend"
	"github.com/sociolytik/mailserver/internal/metrics"
	"github.com/sociolytik/mailserver/internal/ratelimit"
	"github.com/sociolytik/mailserver/internal/store"
)

// SubmissionBackend accepts authenticated mail from local mail clients
// (port 587, STARTTLS). Local recipients are delivered directly to their
// Maildir; everything else is handed to the outbound relay Queue.
type SubmissionBackend struct {
	Config *config.Config
	Store  *store.Store
	Logger *slog.Logger
	Sender *mailsend.Sender

	// RateLimiter, if set, throttles AUTH attempts per client IP. This
	// listener is reached directly (Traefik can't proxy raw SMTP), so the
	// TCP peer address is always the real client — no forwarded-header
	// resolution is needed here, unlike the webmail login.
	RateLimiter *ratelimit.Limiter
}

func (b *SubmissionBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &submissionSession{backend: b, conn: c}, nil
}

type submissionSession struct {
	backend *SubmissionBackend
	conn    *smtp.Conn

	username string // set once AUTH succeeds; the authenticated user's email
	from     string
	rcpts    []string
}

var _ smtp.AuthSession = (*submissionSession)(nil)

func (s *submissionSession) AuthMechanisms() []string {
	return []string{sasl.Plain}
}

func (s *submissionSession) Auth(mech string) (sasl.Server, error) {
	if mech != sasl.Plain {
		return nil, fmt.Errorf("unsupported SASL mechanism %q", mech)
	}
	return sasl.NewPlainServer(func(identity, username, password string) error {
		ip := s.clientIP()

		if s.backend.RateLimiter != nil && ip != "" && !s.backend.RateLimiter.Allowed(ip) {
			s.backend.Logger.Warn("submission AUTH blocked by rate limiter", "ip", ip)
			metrics.AuthAttempts.WithLabelValues("smtp_submission", "rate_limited").Inc()
			return smtp.ErrAuthFailed
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		email := strings.ToLower(strings.TrimSpace(username))
		u, err := s.backend.Store.GetUserByEmail(ctx, email)
		if err != nil || !store.VerifyPassword(u, password) {
			if s.backend.RateLimiter != nil && ip != "" {
				s.backend.RateLimiter.RecordFailure(ip)
			}
			s.backend.Logger.Warn("submission AUTH failed", "ip", ip, "email", email)
			metrics.AuthAttempts.WithLabelValues("smtp_submission", "failure").Inc()
			return smtp.ErrAuthFailed
		}

		if s.backend.RateLimiter != nil && ip != "" {
			s.backend.RateLimiter.RecordSuccess(ip)
		}
		metrics.AuthAttempts.WithLabelValues("smtp_submission", "success").Inc()
		s.username = u.Email
		return nil
	}), nil
}

// clientIP returns the connecting client's address. Like inboundSession,
// this listener isn't behind Traefik, so the raw TCP peer is authoritative
// — unless a trusted PROXY-protocol-speaking TCP load balancer is
// configured in front of it, in which case the listener itself (see
// internal/realip.WrapProxyProto, wired in cmd/mailserver) has already
// rewritten RemoteAddr() to the real client before we ever see the
// connection.
func (s *submissionSession) clientIP() string {
	if s.conn == nil || s.conn.Conn() == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(s.conn.Conn().RemoteAddr().String())
	if err != nil {
		return ""
	}
	return host
}

func (s *submissionSession) requireAuth() error {
	if s.username == "" {
		return &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "Authentication required"}
	}
	return nil
}

func (s *submissionSession) Mail(from string, opts *smtp.MailOptions) error {
	if err := s.requireAuth(); err != nil {
		return err
	}
	s.from = from
	s.rcpts = nil
	return nil
}

func (s *submissionSession) Rcpt(to string, opts *smtp.RcptOptions) error {
	if err := s.requireAuth(); err != nil {
		return err
	}

	addr := strings.ToLower(strings.TrimSpace(to))
	_, domain, ok := address.Split(addr)
	if !ok {
		return &smtp.SMTPError{Code: 501, EnhancedCode: smtp.EnhancedCode{5, 1, 3}, Message: "Malformed recipient address"}
	}

	if s.backend.Config.IsLocalDomain(domain) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.backend.Store.GetUserByEmail(ctx, addr); err != nil {
			return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "No such user here"}
		}
	}

	s.rcpts = append(s.rcpts, addr)
	return nil
}

func (s *submissionSession) Data(r io.Reader) error {
	if err := s.requireAuth(); err != nil {
		return err
	}
	if len(s.rcpts) == 0 {
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "No valid recipients"}
	}

	body, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("reading message body: %w", err)
	}

	full := append([]byte(s.receivedHeader()), body...)

	if _, err := s.backend.Sender.Send(s.username, s.rcpts, full); err != nil {
		s.backend.Logger.Error("sending submitted message failed", "from", s.username, "error", err)
		metrics.SMTPMessages.WithLabelValues("submission", "send_failed").Inc()
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary error sending message"}
	}

	metrics.SMTPMessages.WithLabelValues("submission", "accepted").Inc()
	return nil
}

func (s *submissionSession) receivedHeader() string {
	remoteHost := s.clientIP()
	if remoteHost == "" {
		remoteHost = "unknown"
	}
	helo := ""
	if s.conn != nil {
		helo = s.conn.Hostname()
	}
	return fmt.Sprintf(
		"Received: from %s ([%s])\r\n\tby %s (mailserver submission) with ESMTPSA\r\n\tfor <%s> (authenticated as %s); %s\r\n",
		helo, remoteHost, s.backend.Config.Hostname, strings.Join(s.rcpts, ", "), s.username, time.Now().Format(time.RFC1123Z),
	)
}

func (s *submissionSession) Reset() {
	s.from = ""
	s.rcpts = nil
}

func (s *submissionSession) Logout() error {
	return nil
}
