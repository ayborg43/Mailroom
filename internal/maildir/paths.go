package maildir

import (
	"path/filepath"
	"strings"
)

// UserRoot returns the INBOX Maildir root for a mailbox user.
func UserRoot(basePath, domain, localPart string) string {
	return filepath.Join(basePath, domain, localPart)
}

// Maildir++ subfolders live as dot-prefixed directories alongside INBOX's
// tmp/new/cur, e.g. <UserRoot>/.Sent, so a folder hierarchy of Sent/2024
// becomes ".Sent.2024".
var defaultFolders = []string{"Sent", "Drafts", "Trash", "Junk"}

// FolderPath returns the Maildir root for a named folder under a user's
// mailbox. folder == "" or "INBOX" (case-insensitive) refers to the INBOX
// itself; anything else is a Maildir++ dot-prefixed subfolder.
func FolderPath(userRoot, folder string) string {
	if folder == "" || strings.EqualFold(folder, "INBOX") {
		return userRoot
	}
	return filepath.Join(userRoot, "."+folder)
}

// DefaultFolders lists the subfolders created for every new mailbox.
func DefaultFolders() []string {
	return append([]string(nil), defaultFolders...)
}
