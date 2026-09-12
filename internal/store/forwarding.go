package store

import (
	"context"
	"database/sql"
)

// Forwarding is one mailbox's own forwarding rule: when Enabled, every
// message delivered to that mailbox is also copied to ForwardTo, in
// addition to (never instead of) keeping the original in the mailbox
// itself — losing mail because a forward silently failed (recipient
// mailbox full, remote server down) is worse than an occasional duplicate.
type Forwarding struct {
	UserID    int64  `db:"user_id"`
	Enabled   bool   `db:"enabled"`
	ForwardTo string `db:"forward_to"`
}

const forwardingSchemaSQLite = `
CREATE TABLE IF NOT EXISTS forwarding (
	user_id    INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	enabled    INTEGER NOT NULL DEFAULT 0,
	forward_to TEXT NOT NULL DEFAULT ''
);
`

const forwardingSchemaPostgres = `
CREATE TABLE IF NOT EXISTS forwarding (
	user_id    BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	enabled    BOOLEAN NOT NULL DEFAULT false,
	forward_to TEXT NOT NULL DEFAULT ''
);
`

// GetForwarding returns userID's forwarding rule, or a zero-value
// (disabled) Forwarding if none has been saved yet.
func (s *Store) GetForwarding(ctx context.Context, userID int64) (*Forwarding, error) {
	var f Forwarding
	err := s.db.GetContext(ctx, &f, s.db.Rebind(
		`SELECT user_id, enabled, forward_to FROM forwarding WHERE user_id = ?`), userID)
	if err == sql.ErrNoRows {
		return &Forwarding{UserID: userID}, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// SetForwarding upserts userID's forwarding rule.
func (s *Store) SetForwarding(ctx context.Context, userID int64, enabled bool, forwardTo string) error {
	if s.driver == "postgres" {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO forwarding (user_id, enabled, forward_to)
			VALUES ($1, $2, $3)
			ON CONFLICT (user_id) DO UPDATE SET
				enabled = EXCLUDED.enabled, forward_to = EXCLUDED.forward_to`,
			userID, enabled, forwardTo)
		return err
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO forwarding (user_id, enabled, forward_to)
		VALUES (?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET
			enabled = excluded.enabled, forward_to = excluded.forward_to`,
		userID, enabled, forwardTo)
	return err
}
