package queue

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/sociolytik/mailserver/internal/address"
)

// SmartHostConfig points outbound relay at a single authenticated upstream
// (Brevo, Postmark, SES, Mailgun, ...) instead of delivering directly to
// each recipient's MX. The protocol is identical for any such provider —
// authenticated SMTP with STARTTLS — so nothing here is provider-specific.
type SmartHostConfig struct {
	Host     string
	Port     int
	Username string
	Password string
}

// deliver relays data to to's mailbox. If q.SmartHost is set and returns an
// enabled configuration, every message goes through that one relay —
// mixing direct delivery and a smart host for different messages would
// mean half your outbound mail carries the smart host's reputation and
// half carries your own, which defeats the point of configuring one.
// Otherwise it falls back to direct MX delivery, trying each in
// preference order until one accepts the message or permanently rejects
// it.
func (q *Queue) deliver(from, to string, data []byte) error {
	if q.SmartHost != nil {
		cfg, enabled, err := q.SmartHost(context.Background())
		if err != nil {
			return fmt.Errorf("loading smart host settings: %w", err)
		}
		if enabled {
			return q.trySmartHost(cfg, from, to, data)
		}
	}

	_, domain, ok := address.Split(to)
	if !ok {
		return fmt.Errorf("invalid recipient address %q", to)
	}

	hosts, err := mxHosts(domain)
	if err != nil {
		return fmt.Errorf("no mail exchanger for %s: %w", domain, err)
	}

	var lastErr error
	for _, host := range hosts {
		err := q.tryHost(host, from, to, data)
		if err == nil {
			return nil
		}
		lastErr = err
		if isPermanent(err) {
			return err
		}
	}
	return lastErr
}

// trySmartHost delivers via a configured authenticated relay. Unlike
// tryAddr's opportunistic TLS for direct-to-MX delivery, STARTTLS here is
// mandatory, the certificate is fully verified (no InsecureSkipVerify),
// and there's no plaintext fallback: real login credentials are about to
// go over this connection, so failing the delivery attempt (it'll retry)
// is the only acceptable response to a TLS problem.
func (q *Queue) trySmartHost(cfg *SmartHostConfig, from, to string, data []byte) error {
	return q.trySmartHostWithTLS(cfg, &tls.Config{ServerName: cfg.Host}, from, to, data)
}

// trySmartHostWithTLS is split out from trySmartHost so tests can supply a
// tls.Config trusting a locally-generated test certificate, the same way
// tryAddr is split from tryHost for testing against a fake MTA.
func (q *Queue) trySmartHostWithTLS(cfg *SmartHostConfig, tlsConfig *tls.Config, from, to string, data []byte) error {
	addr := net.JoinHostPort(cfg.Host, portOrDefault(cfg.Port))

	client, err := smtp.DialStartTLS(addr, tlsConfig)
	if err != nil {
		return fmt.Errorf("connecting to smart host %s: %w", addr, err)
	}
	defer client.Close()

	if err := client.Hello(q.Hostname); err != nil {
		return err
	}
	if err := client.Auth(sasl.NewPlainClient("", cfg.Username, cfg.Password)); err != nil {
		return fmt.Errorf("authenticating to smart host: %w", err)
	}
	if err := client.Mail(from, nil); err != nil {
		return err
	}
	if err := client.Rcpt(to, nil); err != nil {
		return err
	}
	wc, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := wc.Write(data); err != nil {
		return fmt.Errorf("writing message data: %w", err)
	}
	if err := wc.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func portOrDefault(port int) string {
	if port <= 0 {
		return "587"
	}
	return fmt.Sprintf("%d", port)
}

// mxHosts returns domain's mail exchangers in preference order, falling
// back to the domain's own address record per RFC 5321 5.1 when it
// publishes no MX records at all.
func mxHosts(domain string) ([]string, error) {
	mxs, err := net.LookupMX(domain)
	if err == nil && len(mxs) > 0 {
		hosts := make([]string, len(mxs))
		for i, mx := range mxs {
			hosts[i] = strings.TrimSuffix(mx.Host, ".")
		}
		return hosts, nil
	}

	if _, aerr := net.LookupHost(domain); aerr == nil {
		return []string{domain}, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("domain has no MX or address records")
}

// tryHost delivers to a single MX host, opportunistically upgrading to TLS
// (RFC 3207) but falling back to plaintext if the remote doesn't support or
// complete STARTTLS, matching standard "best-effort" outbound relay TLS
// behavior for unauthenticated MTA-to-MTA delivery.
func (q *Queue) tryHost(host, from, to string, data []byte) error {
	return q.tryAddr(net.JoinHostPort(host, "25"), host, from, to, data)
}

// tryAddr does the actual delivery to a resolved address; split out from
// tryHost so tests can point it at a local fake MTA on a non-privileged
// port instead of always dialing :25.
func (q *Queue) tryAddr(addr, tlsServerName, from, to string, data []byte) error {
	client, err := smtp.DialStartTLS(addr, &tls.Config{ServerName: tlsServerName, InsecureSkipVerify: true})
	if err != nil {
		client, err = smtp.Dial(addr)
		if err != nil {
			return fmt.Errorf("connecting to %s: %w", addr, err)
		}
	}
	defer client.Close()

	if err := client.Hello(q.Hostname); err != nil {
		return err
	}
	if err := client.Mail(from, nil); err != nil {
		return err
	}
	if err := client.Rcpt(to, nil); err != nil {
		return err
	}
	wc, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := wc.Write(data); err != nil {
		return fmt.Errorf("writing message data: %w", err)
	}
	if err := wc.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// isPermanent reports whether err represents a definitive rejection (5xx)
// that no amount of retrying will fix. Anything else (network errors,
// timeouts, 4xx) is treated as worth retrying.
func isPermanent(err error) bool {
	var smtpErr *smtp.SMTPError
	if errors.As(err, &smtpErr) {
		return !smtpErr.Temporary()
	}
	return false
}
