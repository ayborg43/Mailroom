package imapbackend

import (
	"context"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/sociolytik/mailserver/internal/address"
	"github.com/sociolytik/mailserver/internal/maildir"
	"github.com/sociolytik/mailserver/internal/store"
)

const mailboxDelim rune = '/'

// User is one mailbox owner. It lazily loads and caches its folders
// (Mailbox) so that multiple concurrent IMAP connections for the same
// person share the same in-memory state and see each other's updates.
type User struct {
	backend *Backend
	dbUser  *store.User

	mu        sync.Mutex
	mailboxes map[string]*Mailbox
}

func newUser(b *Backend, dbUser *store.User) *User {
	return &User{backend: b, dbUser: dbUser, mailboxes: make(map[string]*Mailbox)}
}

func (u *User) root() string {
	local, domain, _ := address.Split(u.dbUser.Email)
	return maildir.UserRoot(u.backend.Config.Storage.MaildirPath, domain, local)
}

func (u *User) mailboxLocked(name string) (*Mailbox, error) {
	name = canonicalMailboxName(name)
	if mbox, ok := u.mailboxes[name]; ok {
		return mbox, nil
	}

	ctx := context.Background()
	dbmbox, err := u.backend.Store.GetMailbox(ctx, u.dbUser.ID, name)
	if err == store.ErrNotFound {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeNonExistent, Text: "No such mailbox"}
	}
	if err != nil {
		return nil, err
	}

	mbox, err := newMailbox(u, dbmbox)
	if err != nil {
		return nil, err
	}
	u.mailboxes[name] = mbox
	return mbox, nil
}

func (u *User) mailbox(name string) (*Mailbox, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.mailboxLocked(name)
}

func (u *User) Status(name string, options *imap.StatusOptions) (*imap.StatusData, error) {
	mbox, err := u.mailbox(name)
	if err != nil {
		return nil, err
	}
	return mbox.StatusData(options), nil
}

func (u *User) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	ctx := context.Background()
	dbMboxes, err := u.backend.Store.ListMailboxes(ctx, u.dbUser.ID)
	if err != nil {
		return err
	}

	u.mu.Lock()
	for _, dbmbox := range dbMboxes {
		if _, ok := u.mailboxes[dbmbox.Name]; !ok {
			mbox, err := newMailbox(u, &dbmbox)
			if err != nil {
				u.mu.Unlock()
				return err
			}
			u.mailboxes[dbmbox.Name] = mbox
		}
	}
	mailboxes := make([]*Mailbox, 0, len(u.mailboxes))
	for _, mbox := range u.mailboxes {
		mailboxes = append(mailboxes, mbox)
	}
	u.mu.Unlock()

	if len(patterns) == 0 {
		return w.WriteList(&imap.ListData{Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}, Delim: mailboxDelim})
	}

	var l []imap.ListData
	for _, mbox := range mailboxes {
		match := false
		for _, pattern := range patterns {
			if imapserver.MatchList(mbox.name, mailboxDelim, ref, pattern) {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		if data := mbox.list(options); data != nil {
			l = append(l, *data)
		}
	}

	sort.Slice(l, func(i, j int) bool { return l[i].Mailbox < l[j].Mailbox })

	for _, data := range l {
		if err := w.WriteList(&data); err != nil {
			return err
		}
	}
	return nil
}

func (u *User) Append(mailbox string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	mbox, err := u.mailbox(mailbox)
	if err != nil {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeTryCreate, Text: "No such mailbox"}
	}
	return mbox.appendLiteral(r, options)
}

func (u *User) Create(name string, options *imap.CreateOptions) error {
	name = canonicalMailboxName(name)

	u.mu.Lock()
	defer u.mu.Unlock()

	ctx := context.Background()
	if _, err := u.backend.Store.GetMailbox(ctx, u.dbUser.ID, name); err == nil {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAlreadyExists, Text: "Mailbox already exists"}
	}

	dbmbox, err := u.backend.Store.EnsureMailbox(ctx, u.dbUser.ID, name)
	if err != nil {
		return err
	}
	mbox, err := newMailbox(u, dbmbox)
	if err != nil {
		return err
	}
	u.mailboxes[name] = mbox
	return nil
}

func (u *User) Delete(name string) error {
	name = canonicalMailboxName(name)

	u.mu.Lock()
	defer u.mu.Unlock()

	mbox, err := u.mailboxLocked(name)
	if err != nil {
		return err
	}

	ctx := context.Background()
	if err := u.backend.Store.DeleteMailbox(ctx, mbox.dbID); err != nil {
		return err
	}
	if err := os.RemoveAll(mbox.root); err != nil {
		return err
	}
	delete(u.mailboxes, name)
	return nil
}

func (u *User) Rename(oldName, newName string, options *imap.RenameOptions) error {
	oldName = canonicalMailboxName(oldName)
	newName = canonicalMailboxName(newName)

	u.mu.Lock()
	defer u.mu.Unlock()

	mbox, err := u.mailboxLocked(oldName)
	if err != nil {
		return err
	}
	if _, err := u.backend.Store.GetMailbox(context.Background(), u.dbUser.ID, newName); err == nil {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAlreadyExists, Text: "Mailbox already exists"}
	}

	newRoot := maildir.FolderPath(u.root(), newName)
	if err := os.Rename(mbox.root, newRoot); err != nil {
		return err
	}
	if err := u.backend.Store.RenameMailbox(context.Background(), mbox.dbID, newName); err != nil {
		return err
	}

	mbox.rename(newName)
	mbox.mu.Lock()
	mbox.root = newRoot
	mbox.md = maildir.New(newRoot)
	mbox.mu.Unlock()

	u.mailboxes[newName] = mbox
	delete(u.mailboxes, oldName)
	return nil
}

func (u *User) Subscribe(name string) error {
	mbox, err := u.mailbox(name)
	if err != nil {
		return err
	}
	return mbox.setSubscribed(context.Background(), true)
}

func (u *User) Unsubscribe(name string) error {
	mbox, err := u.mailbox(name)
	if err != nil {
		return err
	}
	return mbox.setSubscribed(context.Background(), false)
}

func (u *User) Namespace() (*imap.NamespaceData, error) {
	return &imap.NamespaceData{Personal: []imap.NamespaceDescriptor{{Delim: mailboxDelim}}}, nil
}

func canonicalMailboxName(name string) string {
	name = strings.TrimRight(name, string(mailboxDelim))
	if strings.EqualFold(name, "INBOX") {
		return "INBOX"
	}
	return name
}
