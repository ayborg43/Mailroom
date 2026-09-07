package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// Mailbox is an IMAP folder (INBOX, Sent, Drafts, ...) belonging to a user.
//
// The message bytes themselves live in a Maildir directory on disk; this
// table only tracks the IMAP-level bookkeeping (UIDVALIDITY/UIDNEXT and the
// UID assigned to each Maildir file) that the Maildir format itself has no
// place for.
type Mailbox struct {
	ID          int64  `db:"id"`
	UserID      int64  `db:"user_id"`
	Name        string `db:"name"`
	UIDValidity uint32 `db:"uid_validity"`
	UIDNext     uint32 `db:"uid_next"`
	Subscribed  bool   `db:"subscribed"`
}

// MailboxMessage maps one Maildir file to its assigned IMAP UID.
type MailboxMessage struct {
	ID           int64     `db:"id"`
	MailboxID    int64     `db:"mailbox_id"`
	UID          uint32    `db:"uid"`
	MaildirKey   string    `db:"maildir_key"`
	Flags        string    `db:"flags"`
	InternalDate time.Time `db:"internal_date"`
	Size         int64     `db:"size"`
}

const mailboxSchemaSQLite = `
CREATE TABLE IF NOT EXISTS mailboxes (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name         TEXT NOT NULL,
	uid_validity INTEGER NOT NULL,
	uid_next     INTEGER NOT NULL DEFAULT 1,
	subscribed   INTEGER NOT NULL DEFAULT 1,
	created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(user_id, name)
);

CREATE TABLE IF NOT EXISTS mailbox_messages (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	mailbox_id    INTEGER NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
	uid           INTEGER NOT NULL,
	maildir_key   TEXT NOT NULL,
	flags         TEXT NOT NULL DEFAULT '',
	internal_date DATETIME NOT NULL,
	size          INTEGER NOT NULL DEFAULT 0,
	UNIQUE(mailbox_id, uid),
	UNIQUE(mailbox_id, maildir_key)
);
`

const mailboxSchemaPostgres = `
CREATE TABLE IF NOT EXISTS mailboxes (
	id           BIGSERIAL PRIMARY KEY,
	user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name         TEXT NOT NULL,
	uid_validity BIGINT NOT NULL,
	uid_next     BIGINT NOT NULL DEFAULT 1,
	subscribed   BOOLEAN NOT NULL DEFAULT true,
	created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE(user_id, name)
);

CREATE TABLE IF NOT EXISTS mailbox_messages (
	id            BIGSERIAL PRIMARY KEY,
	mailbox_id    BIGINT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
	uid           BIGINT NOT NULL,
	maildir_key   TEXT NOT NULL,
	flags         TEXT NOT NULL DEFAULT '',
	internal_date TIMESTAMPTZ NOT NULL,
	size          BIGINT NOT NULL DEFAULT 0,
	UNIQUE(mailbox_id, uid),
	UNIQUE(mailbox_id, maildir_key)
);
`

// EnsureMailbox returns the mailbox named name for user, creating it (with a
// fresh UIDVALIDITY) if it doesn't already exist.
func (s *Store) EnsureMailbox(ctx context.Context, userID int64, name string) (*Mailbox, error) {
	mbox, err := s.GetMailbox(ctx, userID, name)
	if err == nil {
		return mbox, nil
	}
	if err != ErrNotFound {
		return nil, err
	}

	uidValidity := uint32(time.Now().Unix())

	if s.driver == "postgres" {
		var m Mailbox
		err = s.db.GetContext(ctx, &m,
			`INSERT INTO mailboxes (user_id, name, uid_validity, uid_next, subscribed)
			 VALUES ($1, $2, $3, 1, true)
			 RETURNING id, user_id, name, uid_validity, uid_next, subscribed`,
			userID, name, uidValidity)
		if err != nil {
			return nil, err
		}
		return &m, nil
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO mailboxes (user_id, name, uid_validity, uid_next, subscribed) VALUES (?, ?, ?, 1, 1)`,
		userID, name, uidValidity)
	if err != nil {
		return nil, err
	}
	return s.GetMailbox(ctx, userID, name)
}

func (s *Store) GetMailbox(ctx context.Context, userID int64, name string) (*Mailbox, error) {
	var m Mailbox
	err := s.db.GetContext(ctx, &m, s.db.Rebind(
		`SELECT id, user_id, name, uid_validity, uid_next, subscribed FROM mailboxes WHERE user_id = ? AND name = ?`),
		userID, name)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) ListMailboxes(ctx context.Context, userID int64) ([]Mailbox, error) {
	var mboxes []Mailbox
	err := s.db.SelectContext(ctx, &mboxes, s.db.Rebind(
		`SELECT id, user_id, name, uid_validity, uid_next, subscribed FROM mailboxes WHERE user_id = ? ORDER BY name`),
		userID)
	return mboxes, err
}

func (s *Store) DeleteMailbox(ctx context.Context, mailboxID int64) error {
	_, err := s.db.ExecContext(ctx, s.db.Rebind(`DELETE FROM mailboxes WHERE id = ?`), mailboxID)
	return err
}

func (s *Store) RenameMailbox(ctx context.Context, mailboxID int64, newName string) error {
	_, err := s.db.ExecContext(ctx, s.db.Rebind(`UPDATE mailboxes SET name = ? WHERE id = ?`), newName, mailboxID)
	return err
}

func (s *Store) SetMailboxSubscribed(ctx context.Context, mailboxID int64, subscribed bool) error {
	_, err := s.db.ExecContext(ctx, s.db.Rebind(`UPDATE mailboxes SET subscribed = ? WHERE id = ?`), subscribed, mailboxID)
	return err
}

// ListMessages returns every indexed message in the mailbox, ordered by UID.
func (s *Store) ListMessages(ctx context.Context, mailboxID int64) ([]MailboxMessage, error) {
	var msgs []MailboxMessage
	err := s.db.SelectContext(ctx, &msgs, s.db.Rebind(
		`SELECT id, mailbox_id, uid, maildir_key, flags, internal_date, size FROM mailbox_messages WHERE mailbox_id = ? ORDER BY uid`),
		mailboxID)
	return msgs, err
}

// InsertMessage assigns the next UID in mailboxID to a newly-discovered
// Maildir file and records it. Safe for concurrent use across processes only
// insofar as the underlying database serializes the increment; for SQLite
// that means a single-writer connection (see Store.Open), for Postgres a
// transaction.
func (s *Store) InsertMessage(ctx context.Context, mailboxID int64, key, flags string, internalDate time.Time, size int64) (uint32, error) {
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var uidNext uint32
	err = tx.GetContext(ctx, &uidNext, tx.Rebind(`SELECT uid_next FROM mailboxes WHERE id = ?`), mailboxID)
	if err != nil {
		return 0, err
	}

	_, err = tx.ExecContext(ctx, tx.Rebind(
		`INSERT INTO mailbox_messages (mailbox_id, uid, maildir_key, flags, internal_date, size) VALUES (?, ?, ?, ?, ?, ?)`),
		mailboxID, uidNext, key, flags, internalDate, size)
	if err != nil {
		return 0, err
	}

	_, err = tx.ExecContext(ctx, tx.Rebind(`UPDATE mailboxes SET uid_next = ? WHERE id = ?`), uidNext+1, mailboxID)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return uidNext, nil
}

func (s *Store) UpdateMessageFlags(ctx context.Context, mailboxID int64, key, flags string) error {
	_, err := s.db.ExecContext(ctx, s.db.Rebind(
		`UPDATE mailbox_messages SET flags = ? WHERE mailbox_id = ? AND maildir_key = ?`),
		flags, mailboxID, key)
	return err
}

func (s *Store) DeleteMessageByKey(ctx context.Context, mailboxID int64, key string) error {
	_, err := s.db.ExecContext(ctx, s.db.Rebind(
		`DELETE FROM mailbox_messages WHERE mailbox_id = ? AND maildir_key = ?`),
		mailboxID, key)
	return err
}

// DeleteMessagesNotIn removes index rows whose Maildir file is no longer on
// disk (e.g. removed by an external tool), so the index stays consistent
// with reality. keys may be empty, in which case every row is removed.
func (s *Store) DeleteMessagesNotIn(ctx context.Context, mailboxID int64, keys []string) error {
	if len(keys) == 0 {
		_, err := s.db.ExecContext(ctx, s.db.Rebind(`DELETE FROM mailbox_messages WHERE mailbox_id = ?`), mailboxID)
		return err
	}

	query, args, err := sqlx.In(`DELETE FROM mailbox_messages WHERE mailbox_id = ? AND maildir_key NOT IN (?)`, mailboxID, keys)
	if err != nil {
		return fmt.Errorf("building delete query: %w", err)
	}
	_, err = s.db.ExecContext(ctx, s.db.Rebind(query), args...)
	return err
}
