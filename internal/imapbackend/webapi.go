package imapbackend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-message/mail"

	"github.com/sociolytik/mailserver/internal/store"
)

// Account is the webmail-facing handle for one authenticated user.
//
// Unlike the IMAP Session (built around the wire protocol's sequence
// numbers, NumSets and response writers), Account exposes what a web UI
// actually needs in plain Go terms: folders with counts, a message list
// addressed by UID, single-message fetch, flag changes, move/delete, and
// append. It operates on the exact same in-memory Mailbox objects the IMAP
// server itself uses (see Backend.userFor), so a webmail session and an
// IMAP client looking at the same mailbox at the same time never disagree
// about its state, and there's exactly one place that reconciles Maildir
// with the UID index (Mailbox.reloadLocked).
type Account struct {
	user *User
}

// Authenticate verifies email/password against the user directory.
func (b *Backend) Authenticate(ctx context.Context, email, password string) (*Account, error) {
	dbUser, err := b.Store.GetUserByEmail(ctx, strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		return nil, fmt.Errorf("invalid credentials")
	}
	if !store.VerifyPassword(dbUser, password) {
		return nil, fmt.Errorf("invalid credentials")
	}
	return &Account{user: b.userFor(dbUser)}, nil
}

func (a *Account) Email() string { return a.user.dbUser.Email }

// IsAdmin reports whether this account has administrative privileges
// (mailbox management, via the webmail Admin page).
func (a *Account) IsAdmin() bool { return a.user.dbUser.IsAdmin }

// ChangePassword updates the account's own password after verifying
// currentPassword against the live database record. It deliberately
// re-reads the user rather than trusting a.user.dbUser: Backend.userFor
// caches one *User per email for the life of the process (shared by every
// session and IMAP connection for that address), so the cached copy can be
// stale relative to the database — login already works around this by
// checking a freshly-fetched record (see Authenticate/Login), and this does
// the same rather than adding cache-invalidation machinery for one rarely
// used mutation.
func (a *Account) ChangePassword(ctx context.Context, currentPassword, newPassword string) error {
	fresh, err := a.user.backend.Store.GetUserByID(ctx, a.user.dbUser.ID)
	if err != nil {
		return fmt.Errorf("looking up account: %w", err)
	}
	if !store.VerifyPassword(fresh, currentPassword) {
		return fmt.Errorf("current password is incorrect")
	}
	return a.user.backend.Store.SetPassword(ctx, fresh.ID, newPassword)
}

// FolderInfo summarizes one IMAP folder for a sidebar.
type FolderInfo struct {
	Name   string
	Total  int
	Unseen int
}

// Folders lists every folder for the account, INBOX first then
// alphabetical.
func (a *Account) Folders(ctx context.Context) ([]FolderInfo, error) {
	dbMboxes, err := a.user.backend.Store.ListMailboxes(ctx, a.user.dbUser.ID)
	if err != nil {
		return nil, err
	}

	infos := make([]FolderInfo, 0, len(dbMboxes))
	for i := range dbMboxes {
		mbox, err := a.user.mailbox(dbMboxes[i].Name)
		if err != nil {
			return nil, err
		}

		mbox.mu.Lock()
		if err := mbox.reloadLocked(ctx); err != nil {
			mbox.mu.Unlock()
			return nil, err
		}
		total := len(mbox.l)
		unseen := total - int(mbox.countByFlagLocked(imap.FlagSeen))
		mbox.mu.Unlock()

		infos = append(infos, FolderInfo{Name: dbMboxes[i].Name, Total: total, Unseen: unseen})
	}

	sortFoldersInboxFirst(infos)
	return infos, nil
}

func sortFoldersInboxFirst(infos []FolderInfo) {
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].Name == "INBOX" {
			return true
		}
		if infos[j].Name == "INBOX" {
			return false
		}
		return infos[i].Name < infos[j].Name
	})
}

// MessageSummary is what a message-list view needs, without the body.
type MessageSummary struct {
	UID     uint32
	Subject string
	From    string
	To      string
	Date    time.Time
	Flags   []string
	Size    int64
}

func (s MessageSummary) HasFlag(flag string) bool {
	for _, f := range s.Flags {
		if strings.EqualFold(f, flag) {
			return true
		}
	}
	return false
}

func summarize(msg *message) MessageSummary {
	sum := MessageSummary{
		UID:   uint32(msg.uid),
		Date:  msg.t,
		Flags: flagsToStrings(msg.flagList()),
		Size:  msg.size,
	}
	if buf, err := msg.data(); err == nil {
		if env := envelopeFromBuf(buf); env != nil {
			sum.Subject = env.Subject
			sum.From = formatAddressList(env.From)
			sum.To = formatAddressList(env.To)
			if !env.Date.IsZero() {
				sum.Date = env.Date
			}
		}
	}
	return sum
}

func flagsToStrings(flags []imap.Flag) []string {
	out := make([]string, len(flags))
	for i, f := range flags {
		out[i] = string(f)
	}
	return out
}

func formatAddressList(addrs []imap.Address) string {
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		addr := a.Addr()
		if addr == "" {
			continue
		}
		if a.Name != "" {
			parts = append(parts, fmt.Sprintf("%s <%s>", a.Name, addr))
		} else {
			parts = append(parts, addr)
		}
	}
	return strings.Join(parts, ", ")
}

// ListMessages returns a page of messages in folder, newest first. Page is
// 1-indexed.
func (a *Account) ListMessages(folder string, page, pageSize int) (summaries []MessageSummary, total int, err error) {
	mbox, err := a.user.mailbox(folder)
	if err != nil {
		return nil, 0, err
	}

	mbox.mu.Lock()
	defer mbox.mu.Unlock()
	if err := mbox.reloadLocked(context.Background()); err != nil {
		return nil, 0, err
	}

	total = len(mbox.l)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 25
	}

	end := total - (page-1)*pageSize
	if end <= 0 {
		return nil, total, nil
	}
	start := end - pageSize
	if start < 0 {
		start = 0
	}

	summaries = make([]MessageSummary, 0, end-start)
	for i := end - 1; i >= start; i-- {
		summaries = append(summaries, summarize(mbox.l[i]))
	}
	return summaries, total, nil
}

// GetSummary fetches one message's current summary without the side effect
// GetMessage has (marking it \Seen) — for actions like toggling a flag,
// which need to read the current flag set first without altering it.
func (a *Account) GetSummary(folder string, uid uint32) (*MessageSummary, error) {
	mbox, err := a.user.mailbox(folder)
	if err != nil {
		return nil, err
	}

	mbox.mu.Lock()
	defer mbox.mu.Unlock()

	msg := mbox.findByUIDLocked(imap.UID(uid))
	if msg == nil {
		return nil, fmt.Errorf("message not found")
	}
	sum := summarize(msg)
	return &sum, nil
}

// Search does a basic full-text search (headers + text body, like IMAP's
// SEARCH TEXT) within one folder, newest first.
func (a *Account) Search(folder, query string) ([]MessageSummary, error) {
	mbox, err := a.user.mailbox(folder)
	if err != nil {
		return nil, err
	}

	mbox.mu.Lock()
	defer mbox.mu.Unlock()
	if err := mbox.reloadLocked(context.Background()); err != nil {
		return nil, err
	}

	criteria := &imap.SearchCriteria{Text: []string{query}}

	var summaries []MessageSummary
	for i := len(mbox.l) - 1; i >= 0; i-- {
		if mbox.l[i].search(0, criteria) {
			summaries = append(summaries, summarize(mbox.l[i]))
		}
	}
	return summaries, nil
}

// FullMessage is what the reading pane and compose (for reply/forward)
// need.
type FullMessage struct {
	MessageSummary
	Cc         string
	MessageID  string
	InReplyTo  string
	References string
	TextBody   string
	HTMLBody   string
}

// GetMessage fetches one message by UID, marking it \Seen (matching the
// IMAP FETCH-without-PEEK behavior a mail client gets when opening a
// message).
func (a *Account) GetMessage(folder string, uid uint32) (*FullMessage, error) {
	mbox, err := a.user.mailbox(folder)
	if err != nil {
		return nil, err
	}

	mbox.mu.Lock()
	msg := mbox.findByUIDLocked(imap.UID(uid))
	if msg == nil {
		mbox.mu.Unlock()
		return nil, fmt.Errorf("message not found")
	}
	buf, err := msg.data()
	if err != nil {
		mbox.mu.Unlock()
		return nil, fmt.Errorf("reading message: %w", err)
	}
	if !msg.hasFlag(imap.FlagSeen) {
		msg.flags[imap.FlagSeen] = struct{}{}
		if err := mbox.persistFlagsLocked(context.Background(), msg); err != nil {
			mbox.mu.Unlock()
			return nil, err
		}
	}
	sum := summarize(msg)
	mbox.mu.Unlock()

	full := &FullMessage{MessageSummary: sum}

	env := envelopeFromBuf(buf)
	if env != nil {
		full.Cc = formatAddressList(env.Cc)
		full.MessageID = env.MessageID
		if len(env.InReplyTo) > 0 {
			full.InReplyTo = env.InReplyTo[0]
		}
	}

	textBody, htmlBody, references := extractBody(buf)
	full.TextBody = textBody
	full.HTMLBody = htmlBody
	full.References = strings.Join(references, " ")

	return full, nil
}

// extractBody walks a message's MIME structure for the first text/plain
// and text/html parts (the two "views" a mail client shows), and returns
// the References header's message-IDs (without angle brackets, matching
// Envelope.MessageID/InReplyTo's convention) for reply threading.
func extractBody(raw []byte) (textBody, htmlBody string, references []string) {
	r, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return "", "", nil
	}
	defer r.Close()

	references, _ = r.Header.MsgIDList("References")

	for {
		part, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}

		inline, ok := part.Header.(*mail.InlineHeader)
		if !ok {
			continue
		}
		ct, _, _ := inline.ContentType()
		body, _ := io.ReadAll(part.Body)

		switch {
		case strings.HasPrefix(ct, "text/html") && htmlBody == "":
			htmlBody = string(body)
		case strings.HasPrefix(ct, "text/plain") && textBody == "":
			textBody = string(body)
		}
	}
	return textBody, htmlBody, references
}

// Append saves raw as a new message in folder with the given flags (e.g.
// saving a sent or draft message).
func (a *Account) Append(folder string, raw []byte, flags []string) (uint32, error) {
	mbox, err := a.user.mailbox(folder)
	if err != nil {
		return 0, err
	}

	imapFlags := make([]imap.Flag, len(flags))
	for i, f := range flags {
		imapFlags[i] = imap.Flag(f)
	}

	data, err := mbox.appendBytes(raw, &imap.AppendOptions{Flags: imapFlags})
	if err != nil {
		return 0, err
	}
	return uint32(data.UID), nil
}

// SetFlags replaces a message's full flag set (canonical IMAP flag names,
// e.g. "\\Seen", "\\Flagged").
func (a *Account) SetFlags(folder string, uid uint32, flags []string) error {
	mbox, err := a.user.mailbox(folder)
	if err != nil {
		return err
	}

	mbox.mu.Lock()
	defer mbox.mu.Unlock()

	msg := mbox.findByUIDLocked(imap.UID(uid))
	if msg == nil {
		return fmt.Errorf("message not found")
	}

	msg.flags = make(map[imap.Flag]struct{}, len(flags))
	for _, f := range flags {
		msg.flags[canonicalFlag(imap.Flag(f))] = struct{}{}
	}
	return mbox.persistFlagsLocked(context.Background(), msg)
}

// Delete removes a message: if folder isn't already Trash, it's moved
// there (mirroring the usual mail client "delete" gesture); if it's
// already in Trash, it's permanently expunged.
func (a *Account) Delete(folder string, uid uint32) error {
	if strings.EqualFold(folder, "Trash") {
		return a.expunge(folder, uid)
	}
	return a.moveTo(folder, uid, "Trash")
}

// Move relocates a message to destFolder.
func (a *Account) Move(folder string, uid uint32, destFolder string) error {
	return a.moveTo(folder, uid, destFolder)
}

func (a *Account) moveTo(folder string, uid uint32, destFolder string) error {
	src, err := a.user.mailbox(folder)
	if err != nil {
		return err
	}
	dest, err := a.user.mailbox(destFolder)
	if err != nil {
		return err
	}

	src.mu.Lock()
	msg := src.findByUIDLocked(imap.UID(uid))
	if msg == nil {
		src.mu.Unlock()
		return fmt.Errorf("message not found")
	}
	buf, err := msg.data()
	if err != nil {
		src.mu.Unlock()
		return fmt.Errorf("reading message: %w", err)
	}
	flags := msg.flagList()
	t := msg.t
	src.mu.Unlock()

	if _, err := dest.appendBytes(buf, &imap.AppendOptions{Time: t, Flags: flags}); err != nil {
		return fmt.Errorf("copying to %s: %w", destFolder, err)
	}

	ctx := context.Background()
	if err := src.md.Remove(msg.key); err != nil {
		return fmt.Errorf("removing original: %w", err)
	}
	if err := a.user.backend.Store.DeleteMessageByKey(ctx, src.dbID, msg.key); err != nil {
		return err
	}

	src.mu.Lock()
	src.expungeLocked(map[*message]struct{}{msg: {}})
	src.mu.Unlock()
	return nil
}

func (a *Account) expunge(folder string, uid uint32) error {
	mbox, err := a.user.mailbox(folder)
	if err != nil {
		return err
	}

	mbox.mu.Lock()
	msg := mbox.findByUIDLocked(imap.UID(uid))
	mbox.mu.Unlock()
	if msg == nil {
		return fmt.Errorf("message not found")
	}

	if err := mbox.md.Remove(msg.key); err != nil {
		return fmt.Errorf("removing message: %w", err)
	}
	if err := a.user.backend.Store.DeleteMessageByKey(context.Background(), mbox.dbID, msg.key); err != nil {
		return err
	}

	mbox.mu.Lock()
	mbox.expungeLocked(map[*message]struct{}{msg: {}})
	mbox.mu.Unlock()
	return nil
}
