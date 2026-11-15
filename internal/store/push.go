package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Web Push storage: the server's VAPID key pair (a singleton row, generated
// on first start) and one row per browser/device subscription.
//
// The VAPID private key is stored in plaintext for the same reason the relay
// password is (see relay_settings.go): it has to be usable as-is to sign
// push requests. Treat database access with the same care as config.yaml.

const pushSchemaSQLite = `
CREATE TABLE IF NOT EXISTS push_vapid (
	id          INTEGER PRIMARY KEY CHECK (id = 1),
	public_key  TEXT NOT NULL,
	private_key TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS push_subscriptions (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	endpoint   TEXT NOT NULL UNIQUE,
	p256dh     TEXT NOT NULL,
	auth       TEXT NOT NULL,
	user_agent TEXT NOT NULL DEFAULT '',
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS push_subscriptions_user ON push_subscriptions(user_id);
`

const pushSchemaPostgres = `
CREATE TABLE IF NOT EXISTS push_vapid (
	id          BIGINT PRIMARY KEY CHECK (id = 1),
	public_key  TEXT NOT NULL,
	private_key TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS push_subscriptions (
	id         BIGSERIAL PRIMARY KEY,
	user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	endpoint   TEXT NOT NULL UNIQUE,
	p256dh     TEXT NOT NULL,
	auth       TEXT NOT NULL,
	user_agent TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS push_subscriptions_user ON push_subscriptions(user_id);
`

// VAPIDKeys is the server's application-server key pair (base64url, as
// produced by webpush.GenerateVAPIDKeys).
type VAPIDKeys struct {
	PublicKey  string `db:"public_key"`
	PrivateKey string `db:"private_key"`
}

// PushSubscription is one browser's push endpoint plus the keys needed to
// encrypt a payload for it.
type PushSubscription struct {
	ID        int64     `db:"id"`
	UserID    int64     `db:"user_id"`
	Endpoint  string    `db:"endpoint"`
	P256dh    string    `db:"p256dh"`
	Auth      string    `db:"auth"`
	UserAgent string    `db:"user_agent"`
	CreatedAt time.Time `db:"created_at"`
}

// GetVAPIDKeys returns the stored key pair, or ErrNotFound if none has been
// saved yet.
func (s *Store) GetVAPIDKeys(ctx context.Context) (*VAPIDKeys, error) {
	var k VAPIDKeys
	err := s.db.GetContext(ctx, &k, `SELECT public_key, private_key FROM push_vapid WHERE id = 1`)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// SaveVAPIDKeysIfAbsent stores k unless a pair already exists (two servers
// starting at once must end up agreeing on one key), and returns whichever
// pair is now stored.
func (s *Store) SaveVAPIDKeysIfAbsent(ctx context.Context, k VAPIDKeys) (*VAPIDKeys, error) {
	_, err := s.db.ExecContext(ctx, s.db.Rebind(
		`INSERT INTO push_vapid (id, public_key, private_key) VALUES (1, ?, ?) ON CONFLICT (id) DO NOTHING`),
		k.PublicKey, k.PrivateKey)
	if err != nil {
		return nil, err
	}
	return s.GetVAPIDKeys(ctx)
}

// UpsertPushSubscription records a device's subscription for userID. An
// endpoint identifies one browser profile, so if the same endpoint was
// previously subscribed by a different user (a shared computer), it is
// reassigned to the current one — a device only ever notifies whoever last
// enabled notifications on it.
func (s *Store) UpsertPushSubscription(ctx context.Context, userID int64, endpoint, p256dh, auth, userAgent string) error {
	_, err := s.db.ExecContext(ctx, s.db.Rebind(`
INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth, user_agent) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (endpoint) DO UPDATE SET user_id = excluded.user_id, p256dh = excluded.p256dh, auth = excluded.auth, user_agent = excluded.user_agent`),
		userID, endpoint, p256dh, auth, userAgent)
	return err
}

// DeletePushSubscription removes userID's subscription for endpoint. It
// reports whether a row was removed (a user can't remove someone else's).
func (s *Store) DeletePushSubscription(ctx context.Context, userID int64, endpoint string) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.db.Rebind(`DELETE FROM push_subscriptions WHERE user_id = ? AND endpoint = ?`), userID, endpoint)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeletePushSubscriptionByEndpoint drops a subscription the push service
// reported as gone (HTTP 404/410), regardless of owner.
func (s *Store) DeletePushSubscriptionByEndpoint(ctx context.Context, endpoint string) error {
	_, err := s.db.ExecContext(ctx, s.db.Rebind(`DELETE FROM push_subscriptions WHERE endpoint = ?`), endpoint)
	return err
}

// ListPushSubscriptionsByEmail returns every subscription belonging to the
// mailbox with the given address (case-insensitive), in a single query so
// the new-mail hot path costs one lookup.
func (s *Store) ListPushSubscriptionsByEmail(ctx context.Context, email string) (userID int64, subs []PushSubscription, err error) {
	err = s.db.SelectContext(ctx, &subs, s.db.Rebind(`
SELECT s.id, s.user_id, s.endpoint, s.p256dh, s.auth, s.user_agent, s.created_at
FROM push_subscriptions s JOIN users u ON u.id = s.user_id
WHERE u.email = ? ORDER BY s.id`), strings.ToLower(email))
	if err != nil {
		return 0, nil, err
	}
	if len(subs) > 0 {
		userID = subs[0].UserID
	}
	return userID, subs, nil
}

// ListPushSubscriptions returns every subscription of userID.
func (s *Store) ListPushSubscriptions(ctx context.Context, userID int64) ([]PushSubscription, error) {
	var subs []PushSubscription
	err := s.db.SelectContext(ctx, &subs, s.db.Rebind(`
SELECT id, user_id, endpoint, p256dh, auth, user_agent, created_at
FROM push_subscriptions WHERE user_id = ? ORDER BY id`), userID)
	return subs, err
}

// CountPushSubscriptions is how many devices userID has notifications on.
func (s *Store) CountPushSubscriptions(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.db.GetContext(ctx, &n, s.db.Rebind(`SELECT COUNT(1) FROM push_subscriptions WHERE user_id = ?`), userID)
	return n, err
}
