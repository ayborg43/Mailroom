package imapbackend

import (
	"context"
	"fmt"

	"github.com/sociolytik/mailserver/internal/address"
	"github.com/sociolytik/mailserver/internal/config"
	"github.com/sociolytik/mailserver/internal/maildir"
	"github.com/sociolytik/mailserver/internal/store"
)

// ProvisionMailboxes ensures a user has the standard IMAP folders indexed
// in the database and their Maildir directories created on disk. Called
// once right after a mailbox is created, whether from the CLI (bootstrap
// admin, `user create`) or the webmail admin UI.
func ProvisionMailboxes(ctx context.Context, cfg *config.Config, db *store.Store, user *store.User) error {
	local, domain, ok := address.Split(user.Email)
	if !ok {
		return fmt.Errorf("invalid user email %q", user.Email)
	}
	root := maildir.UserRoot(cfg.Storage.MaildirPath, domain, local)

	folders := append([]string{"INBOX"}, maildir.DefaultFolders()...)
	for _, name := range folders {
		if _, err := db.EnsureMailbox(ctx, user.ID, name); err != nil {
			return fmt.Errorf("ensuring mailbox %q: %w", name, err)
		}
		if err := maildir.New(maildir.FolderPath(root, name)).Init(); err != nil {
			return fmt.Errorf("initializing maildir %q: %w", name, err)
		}
	}
	return nil
}
