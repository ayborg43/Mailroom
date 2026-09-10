// Package store provides the domain/user directory backed by SQLite or Postgres.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite" // sqlite driver

	"golang.org/x/crypto/bcrypt"
)

type Domain struct {
	ID        int64     `db:"id"`
	Name      string    `db:"name"`
	CreatedAt time.Time `db:"created_at"`
}

type User struct {
	ID           int64     `db:"id"`
	DomainID     int64     `db:"domain_id"`
	LocalPart    string    `db:"local_part"`
	Email        string    `db:"email"` // local_part@domain, denormalized for fast lookup
	PasswordHash string    `db:"password_hash"`
	IsAdmin      bool      `db:"is_admin"`
	CreatedAt    time.Time `db:"created_at"`
}

// Store is the user/domain directory. All queries are written in "?" placeholder
// style and rebound per-driver by sqlx, so the same code path works against both
// SQLite and Postgres.
type Store struct {
	db     *sqlx.DB
	driver string
}

func Open(driver, dsn string) (*Store, error) {
	var sqlDriver string
	switch driver {
	case "sqlite":
		sqlDriver = "sqlite"
		if dsn == "" {
			return nil, fmt.Errorf("sqlite dsn (file path) is required")
		}
		// Ensure sane concurrent-access behavior for a single-file embedded DB.
		if !strings.Contains(dsn, "?") {
			dsn += "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
		}
	case "postgres":
		sqlDriver = "pgx"
	default:
		return nil, fmt.Errorf("unsupported database driver %q", driver)
	}

	db, err := sqlx.Open(sqlDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("connecting to database: %w", err)
	}
	if driver == "sqlite" {
		db.SetMaxOpenConns(1) // avoid SQLITE_BUSY under WAL with a single-file DB
	}

	s := &Store{db: db, driver: driver}
	if err := s.migrate(context.Background()); err != nil {
		return nil, fmt.Errorf("running migrations: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// Ping verifies the database connection is alive, for use by a health
// check endpoint.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *Store) migrate(ctx context.Context) error {
	schema := schemaSQLite + mailboxSchemaSQLite + relaySettingsSchemaSQLite
	if s.driver == "postgres" {
		schema = schemaPostgres + mailboxSchemaPostgres + relaySettingsSchemaPostgres
	}
	_, err := s.db.ExecContext(ctx, schema)
	return err
}

const schemaSQLite = `
CREATE TABLE IF NOT EXISTS domains (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL UNIQUE,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS users (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	domain_id     INTEGER NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
	local_part    TEXT NOT NULL,
	email         TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	is_admin      INTEGER NOT NULL DEFAULT 0,
	created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(domain_id, local_part)
);
`

const schemaPostgres = `
CREATE TABLE IF NOT EXISTS domains (
	id         BIGSERIAL PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS users (
	id            BIGSERIAL PRIMARY KEY,
	domain_id     BIGINT NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
	local_part    TEXT NOT NULL,
	email         TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	is_admin      BOOLEAN NOT NULL DEFAULT false,
	created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE(domain_id, local_part)
);
`

var ErrNotFound = fmt.Errorf("not found")

// DomainExists reports whether name is a known local domain.
func (s *Store) DomainExists(ctx context.Context, name string) (bool, error) {
	var count int
	err := s.db.GetContext(ctx, &count, s.db.Rebind(`SELECT COUNT(1) FROM domains WHERE name = ?`), strings.ToLower(name))
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// EnsureDomain inserts the domain if it doesn't already exist.
func (s *Store) EnsureDomain(ctx context.Context, name string) (*Domain, error) {
	name = strings.ToLower(name)
	var d Domain
	err := s.db.GetContext(ctx, &d, s.db.Rebind(`SELECT id, name, created_at FROM domains WHERE name = ?`), name)
	if err == nil {
		return &d, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	if s.driver == "postgres" {
		err = s.db.GetContext(ctx, &d, `INSERT INTO domains (name) VALUES ($1) RETURNING id, name, created_at`, name)
	} else {
		res, execErr := s.db.ExecContext(ctx, `INSERT INTO domains (name) VALUES (?)`, name)
		if execErr != nil {
			return nil, execErr
		}
		id, _ := res.LastInsertId()
		err = s.db.GetContext(ctx, &d, s.db.Rebind(`SELECT id, name, created_at FROM domains WHERE id = ?`), id)
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// GetUserByEmail looks up a mailbox user by full email address (case-insensitive).
func (s *Store) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	var u User
	err := s.db.GetContext(ctx, &u, s.db.Rebind(`SELECT id, domain_id, local_part, email, password_hash, is_admin, created_at FROM users WHERE email = ?`), strings.ToLower(email))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUserByID looks up a mailbox user by numeric id.
func (s *Store) GetUserByID(ctx context.Context, id int64) (*User, error) {
	var u User
	err := s.db.GetContext(ctx, &u, s.db.Rebind(`SELECT id, domain_id, local_part, email, password_hash, is_admin, created_at FROM users WHERE id = ?`), id)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// ListUsers returns every mailbox user across every domain, ordered by
// email — for an admin-facing directory listing.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	var users []User
	err := s.db.SelectContext(ctx, &users,
		`SELECT id, domain_id, local_part, email, password_hash, is_admin, created_at FROM users ORDER BY email`)
	return users, err
}

// DeleteUser removes a mailbox user from the directory. Its mailboxes and
// indexed messages cascade-delete with it (see the users/mailboxes FK), but
// the Maildir files on disk are untouched — callers must remove those
// themselves (see maildir.UserRoot) since this package doesn't know the
// storage base path.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, s.db.Rebind(`DELETE FROM users WHERE id = ?`), id)
	return err
}

// CreateUser creates a mailbox user under domain, hashing password with bcrypt.
func (s *Store) CreateUser(ctx context.Context, domainName, localPart, password string, isAdmin bool) (*User, error) {
	domainName = strings.ToLower(domainName)
	localPart = strings.ToLower(localPart)

	domain, err := s.EnsureDomain(ctx, domainName)
	if err != nil {
		return nil, fmt.Errorf("ensuring domain: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hashing password: %w", err)
	}

	email := localPart + "@" + domainName

	if s.driver == "postgres" {
		var u User
		err = s.db.GetContext(ctx, &u,
			`INSERT INTO users (domain_id, local_part, email, password_hash, is_admin)
			 VALUES ($1, $2, $3, $4, $5)
			 RETURNING id, domain_id, local_part, email, password_hash, is_admin, created_at`,
			domain.ID, localPart, email, string(hash), isAdmin)
		if err != nil {
			return nil, err
		}
		return &u, nil
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO users (domain_id, local_part, email, password_hash, is_admin) VALUES (?, ?, ?, ?, ?)`,
		domain.ID, localPart, email, string(hash), isAdmin)
	if err != nil {
		return nil, err
	}
	return s.GetUserByEmail(ctx, email)
}

// SetPassword replaces a user's password, hashing it with bcrypt.
func (s *Store) SetPassword(ctx context.Context, userID int64, newPassword string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}
	_, err = s.db.ExecContext(ctx, s.db.Rebind(`UPDATE users SET password_hash = ? WHERE id = ?`), string(hash), userID)
	return err
}

// VerifyPassword checks a plaintext password against the user's bcrypt hash.
func VerifyPassword(u *User, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) == nil
}
