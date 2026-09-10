package store

import (
	"context"
	"database/sql"
)

// RelaySettings configures an optional outbound SMTP smart host — an
// authenticated relay (Brevo, Postmark, SES, Mailgun, ...) that all
// non-local mail is handed to instead of being delivered directly to each
// recipient's MX. Improves deliverability for a fresh sending IP with no
// reputation of its own; the protocol is identical regardless of which
// provider you point it at.
//
// This is a singleton row (id is always 1) rather than a table per
// provider, since only one smart host can be active at a time.
type RelaySettings struct {
	Enabled  bool   `db:"enabled"`
	Host     string `db:"host"`
	Port     int    `db:"port"`
	Username string `db:"username"`
	// Stored in plaintext: it must be recoverable as-is to authenticate
	// with the relay (unlike user login passwords, which only ever need
	// verifying, so those are bcrypt-hashed). This credential's exposure
	// is therefore bounded by the same trust boundary as the database
	// itself — keep DB access as tightly scoped as you would the
	// config.yaml file's own secrets.
	Password string `db:"password"`
}

const relaySettingsSchemaSQLite = `
CREATE TABLE IF NOT EXISTS relay_settings (
	id       INTEGER PRIMARY KEY CHECK (id = 1),
	enabled  INTEGER NOT NULL DEFAULT 0,
	host     TEXT NOT NULL DEFAULT '',
	port     INTEGER NOT NULL DEFAULT 587,
	username TEXT NOT NULL DEFAULT '',
	password TEXT NOT NULL DEFAULT ''
);
`

const relaySettingsSchemaPostgres = `
CREATE TABLE IF NOT EXISTS relay_settings (
	id       BIGINT PRIMARY KEY CHECK (id = 1),
	enabled  BOOLEAN NOT NULL DEFAULT false,
	host     TEXT NOT NULL DEFAULT '',
	port     INTEGER NOT NULL DEFAULT 587,
	username TEXT NOT NULL DEFAULT '',
	password TEXT NOT NULL DEFAULT ''
);
`

// GetRelaySettings returns the current smart-host configuration, or a
// zero-value (disabled) RelaySettings if none has been saved yet.
func (s *Store) GetRelaySettings(ctx context.Context) (*RelaySettings, error) {
	var rs RelaySettings
	err := s.db.GetContext(ctx, &rs, s.db.Rebind(
		`SELECT enabled, host, port, username, password FROM relay_settings WHERE id = 1`))
	if err == sql.ErrNoRows {
		return &RelaySettings{Port: 587}, nil
	}
	if err != nil {
		return nil, err
	}
	return &rs, nil
}

// SetRelaySettings upserts the singleton relay settings row. If password is
// empty, the previously stored password is preserved — the admin form
// leaves it blank to mean "unchanged" rather than round-tripping the
// secret through the browser on every save.
func (s *Store) SetRelaySettings(ctx context.Context, rs RelaySettings) error {
	if rs.Password == "" {
		existing, err := s.GetRelaySettings(ctx)
		if err != nil {
			return err
		}
		rs.Password = existing.Password
	}

	if s.driver == "postgres" {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO relay_settings (id, enabled, host, port, username, password)
			VALUES (1, $1, $2, $3, $4, $5)
			ON CONFLICT (id) DO UPDATE SET
				enabled = EXCLUDED.enabled, host = EXCLUDED.host, port = EXCLUDED.port,
				username = EXCLUDED.username, password = EXCLUDED.password`,
			rs.Enabled, rs.Host, rs.Port, rs.Username, rs.Password)
		return err
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO relay_settings (id, enabled, host, port, username, password)
		VALUES (1, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			enabled = excluded.enabled, host = excluded.host, port = excluded.port,
			username = excluded.username, password = excluded.password`,
		rs.Enabled, rs.Host, rs.Port, rs.Username, rs.Password)
	return err
}
