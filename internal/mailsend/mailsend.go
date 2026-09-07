// Package mailsend holds the "send this message on behalf of an
// authenticated local user" dispatch logic — DKIM-sign once, then deliver
// each recipient locally or hand it to the outbound relay queue. It's
// shared by the SMTP submission server and the webmail compose handler, so
// both paths behave identically instead of drifting apart.
package mailsend

import (
	"fmt"
	"log/slog"

	"github.com/sociolytik/mailserver/internal/address"
	"github.com/sociolytik/mailserver/internal/config"
	"github.com/sociolytik/mailserver/internal/maildir"
	"github.com/sociolytik/mailserver/internal/queue"
	"github.com/sociolytik/mailserver/internal/store"
)

// Sender dispatches already-fully-formed RFC 822 messages (headers + body;
// mailsend doesn't add a Received header, since the two callers need
// different ones) from an authenticated local sender.
type Sender struct {
	Config *config.Config
	Store  *store.Store
	Queue  *queue.Queue
	Logger *slog.Logger

	// Sign, if set, DKIM-signs the message before delivery. It's called
	// with the sender's domain (the "d=" the signature should use).
	Sign func(domain string, data []byte) ([]byte, error)
}

// Send DKIM-signs data (if Sign is set) and, for each recipient, either
// delivers it straight to their local Maildir or enqueues it for relay.
// It returns the (possibly signed) bytes that were actually sent/queued,
// since callers with a Sent folder want to save exactly that copy.
func (s *Sender) Send(from string, rcpts []string, data []byte) ([]byte, error) {
	full := data

	if s.Sign != nil {
		_, senderDomain, ok := address.Split(from)
		if !ok {
			return nil, fmt.Errorf("invalid sender address %q", from)
		}
		signed, err := s.Sign(senderDomain, full)
		if err != nil {
			return nil, fmt.Errorf("DKIM signing: %w", err)
		}
		full = signed
	}

	for _, rcpt := range rcpts {
		local, domain, ok := address.Split(rcpt)
		if !ok {
			return nil, fmt.Errorf("invalid recipient address %q", rcpt)
		}

		if s.Config.IsLocalDomain(domain) {
			root := maildir.UserRoot(s.Config.Storage.MaildirPath, domain, local)
			md := maildir.New(maildir.FolderPath(root, "INBOX"))
			if _, err := md.Deliver(full); err != nil {
				return nil, fmt.Errorf("delivering to %s: %w", rcpt, err)
			}
			s.Logger.Info("delivered submitted message locally", "from", from, "to", rcpt, "bytes", len(full))
			continue
		}

		if _, err := s.Queue.Enqueue(from, rcpt, full); err != nil {
			return nil, fmt.Errorf("queuing for %s: %w", rcpt, err)
		}
		s.Logger.Info("queued outbound message", "from", from, "to", rcpt, "bytes", len(full))
	}

	return full, nil
}
