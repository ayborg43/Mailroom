package imapbackend

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/sociolytik/mailserver/internal/config"
	"github.com/sociolytik/mailserver/internal/maildir"
	"github.com/sociolytik/mailserver/internal/store"
)

func newTestBackend(t *testing.T) (*Backend, *store.User) {
	t.Helper()
	dir := t.TempDir()

	cfg := &config.Config{
		Hostname: "mail.example.com",
		Domains:  []string{"example.com"},
		Storage:  config.StorageConfig{MaildirPath: filepath.Join(dir, "mail")},
	}

	db, err := store.Open("sqlite", filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	u, err := db.CreateUser(context.Background(), "example.com", "alice", "password123", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	root := maildir.UserRoot(cfg.Storage.MaildirPath, "example.com", "alice")
	for _, name := range append([]string{"INBOX"}, maildir.DefaultFolders()...) {
		if _, err := db.EnsureMailbox(context.Background(), u.ID, name); err != nil {
			t.Fatalf("EnsureMailbox %s: %v", name, err)
		}
		if err := maildir.New(maildir.FolderPath(root, name)).Init(); err != nil {
			t.Fatalf("maildir Init %s: %v", name, err)
		}
	}

	return New(cfg, db, slog.Default()), u
}

func deliverTestMessage(t *testing.T, cfg *config.Config, domain, local, folder, subject, body string) {
	t.Helper()
	root := maildir.UserRoot(cfg.Storage.MaildirPath, domain, local)
	md := maildir.New(maildir.FolderPath(root, folder))
	raw := fmt.Sprintf("From: bob@other.example\r\nTo: alice@example.com\r\nSubject: %s\r\n\r\n%s\r\n", subject, body)
	if _, err := md.Deliver([]byte(raw)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
}

func TestAccountAuthenticate(t *testing.T) {
	backend, _ := newTestBackend(t)

	if _, err := backend.Authenticate(context.Background(), "alice@example.com", "wrong"); err == nil {
		t.Fatal("expected error for wrong password")
	}
	acct, err := backend.Authenticate(context.Background(), "ALICE@Example.com", "password123")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if acct.Email() != "alice@example.com" {
		t.Errorf("Email() = %q, want alice@example.com", acct.Email())
	}
}

func TestAccountListMessagesAndPagination(t *testing.T) {
	backend, _ := newTestBackend(t)
	cfg := backend.Config

	for i := 1; i <= 5; i++ {
		deliverTestMessage(t, cfg, "example.com", "alice", "INBOX", fmt.Sprintf("msg %d", i), "body")
	}

	acct, err := backend.Authenticate(context.Background(), "alice@example.com", "password123")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	page1, total, err := acct.ListMessages("INBOX", 1, 2)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if total != 5 {
		t.Fatalf("total = %d, want 5", total)
	}
	if len(page1) != 2 {
		t.Fatalf("len(page1) = %d, want 2", len(page1))
	}
	// Newest first: the last delivered message ("msg 5") should be first.
	if page1[0].Subject != "msg 5" || page1[1].Subject != "msg 4" {
		t.Errorf("page1 subjects = [%q, %q], want [msg 5, msg 4]", page1[0].Subject, page1[1].Subject)
	}

	page3, _, err := acct.ListMessages("INBOX", 3, 2)
	if err != nil {
		t.Fatalf("ListMessages page3: %v", err)
	}
	if len(page3) != 1 || page3[0].Subject != "msg 1" {
		t.Fatalf("page3 = %+v, want single [msg 1]", page3)
	}

	pageBeyond, _, err := acct.ListMessages("INBOX", 10, 2)
	if err != nil {
		t.Fatalf("ListMessages pageBeyond: %v", err)
	}
	if len(pageBeyond) != 0 {
		t.Fatalf("pageBeyond = %+v, want empty", pageBeyond)
	}
}

func TestAccountGetMessageMarksSeen(t *testing.T) {
	backend, _ := newTestBackend(t)
	cfg := backend.Config
	deliverTestMessage(t, cfg, "example.com", "alice", "INBOX", "hello", "world body")

	acct, _ := backend.Authenticate(context.Background(), "alice@example.com", "password123")
	msgs, _, err := acct.ListMessages("INBOX", 1, 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("ListMessages: %v, %+v", err, msgs)
	}
	if msgs[0].HasFlag(`\Seen`) {
		t.Fatal("message should not start as Seen")
	}

	full, err := acct.GetMessage("INBOX", msgs[0].UID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if full.Subject != "hello" {
		t.Errorf("Subject = %q, want hello", full.Subject)
	}
	if full.TextBody == "" {
		t.Error("TextBody is empty")
	}

	msgs2, _, _ := acct.ListMessages("INBOX", 1, 10)
	if !msgs2[0].HasFlag(`\Seen`) {
		t.Error("message should be \\Seen after GetMessage")
	}
}

func TestAccountSetFlagsAndDelete(t *testing.T) {
	backend, _ := newTestBackend(t)
	cfg := backend.Config
	deliverTestMessage(t, cfg, "example.com", "alice", "INBOX", "flag me", "body")

	acct, _ := backend.Authenticate(context.Background(), "alice@example.com", "password123")
	msgs, _, _ := acct.ListMessages("INBOX", 1, 10)
	uid := msgs[0].UID

	if err := acct.SetFlags("INBOX", uid, []string{`\Flagged`, `\Seen`}); err != nil {
		t.Fatalf("SetFlags: %v", err)
	}
	msgs, _, _ = acct.ListMessages("INBOX", 1, 10)
	if !msgs[0].HasFlag(`\Flagged`) || !msgs[0].HasFlag(`\Seen`) {
		t.Fatalf("flags = %v, want Flagged+Seen", msgs[0].Flags)
	}

	// Delete from INBOX moves to Trash, not a permanent expunge.
	if err := acct.Delete("INBOX", uid); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	inboxMsgs, _, _ := acct.ListMessages("INBOX", 1, 10)
	if len(inboxMsgs) != 0 {
		t.Fatalf("INBOX should be empty after delete, got %+v", inboxMsgs)
	}
	trashMsgs, _, _ := acct.ListMessages("Trash", 1, 10)
	if len(trashMsgs) != 1 || trashMsgs[0].Subject != "flag me" {
		t.Fatalf("Trash = %+v, want the moved message", trashMsgs)
	}

	// Delete from Trash permanently expunges.
	if err := acct.Delete("Trash", trashMsgs[0].UID); err != nil {
		t.Fatalf("Delete from Trash: %v", err)
	}
	trashMsgs, _, _ = acct.ListMessages("Trash", 1, 10)
	if len(trashMsgs) != 0 {
		t.Fatalf("Trash should be empty after permanent delete, got %+v", trashMsgs)
	}
}

func TestAccountSearch(t *testing.T) {
	backend, _ := newTestBackend(t)
	cfg := backend.Config
	deliverTestMessage(t, cfg, "example.com", "alice", "INBOX", "invoice attached", "please pay")
	deliverTestMessage(t, cfg, "example.com", "alice", "INBOX", "lunch tomorrow", "noon works")

	acct, _ := backend.Authenticate(context.Background(), "alice@example.com", "password123")

	results, err := acct.Search("INBOX", "invoice")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 || results[0].Subject != "invoice attached" {
		t.Fatalf("Search results = %+v, want single invoice match", results)
	}

	noResults, err := acct.Search("INBOX", "nonexistentword")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(noResults) != 0 {
		t.Fatalf("expected no matches, got %+v", noResults)
	}
}

func TestAccountAppend(t *testing.T) {
	backend, _ := newTestBackend(t)
	acct, _ := backend.Authenticate(context.Background(), "alice@example.com", "password123")

	raw := []byte("From: alice@example.com\r\nTo: bob@other.example\r\nSubject: sent copy\r\n\r\nbody\r\n")
	uid, err := acct.Append("Sent", raw, []string{`\Seen`})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if uid == 0 {
		t.Fatal("expected nonzero UID")
	}

	msgs, total, err := acct.ListMessages("Sent", 1, 10)
	if err != nil || total != 1 {
		t.Fatalf("ListMessages(Sent): err=%v total=%d", err, total)
	}
	if msgs[0].Subject != "sent copy" || !msgs[0].HasFlag(`\Seen`) {
		t.Fatalf("appended message = %+v", msgs[0])
	}
}
