// Package maildir implements delivery and reading of messages in the
// standard Maildir format (tmp/new/cur), including Maildir++ style flag
// suffixes ("filename:2,FLAGS") so IMAP flags map directly onto disk.
package maildir

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

var counter uint64

// Standard Maildir++ flag letters, which must stay in ASCII sort order in the
// filename per the spec.
const flagLetters = "DFRST" // Draft, Flagged, Replied, Seen, Trashed (\Deleted)

type Maildir struct {
	root string
}

func New(root string) *Maildir {
	return &Maildir{root: root}
}

func (m *Maildir) Root() string { return m.root }

// Init creates the tmp/new/cur directory structure if it doesn't exist.
func (m *Maildir) Init() error {
	for _, sub := range []string{"tmp", "new", "cur"} {
		if err := os.MkdirAll(filepath.Join(m.root, sub), 0700); err != nil {
			return fmt.Errorf("creating maildir %s/%s: %w", m.root, sub, err)
		}
	}
	return nil
}

func uniqueName() (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "localhost"
	}
	hostname = strings.NewReplacer("/", "\\057", ":", "\\072").Replace(hostname)

	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	n := atomic.AddUint64(&counter, 1)

	// Zero-padded nanosecond epoch as the leading component: List() sorts
	// keys lexicographically and callers (the IMAP backend's UID
	// assignment) depend on that matching delivery order. Unix-seconds
	// resolution isn't fine enough for that — multiple messages delivered
	// within the same second would then sort by the random suffix instead,
	// scrambling UID order. 20 digits covers int64 nanoseconds-since-epoch
	// (which itself overflows around year 2262) with room to spare, so the
	// field width never changes and the sort stays purely chronological;
	// the counter is only a tiebreaker for same-nanosecond deliveries.
	return fmt.Sprintf("%020d.%s%d_%d.%s", time.Now().UnixNano(), hex.EncodeToString(buf[:]), os.Getpid(), n, hostname), nil
}

// Deliver writes data as a brand new message into new/ (undelivered/unread).
// Returns the base filename (no ":2," flags suffix).
func (m *Maildir) Deliver(data []byte) (string, error) {
	if err := m.Init(); err != nil {
		return "", err
	}
	name, err := uniqueName()
	if err != nil {
		return "", err
	}

	tmpPath := filepath.Join(m.root, "tmp", name)
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", fmt.Errorf("creating tmp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("writing message: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("syncing message: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("closing message: %w", err)
	}

	newPath := filepath.Join(m.root, "new", name)
	if err := os.Rename(tmpPath, newPath); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("moving message to new: %w", err)
	}
	return name, nil
}

// Message describes one message file as it exists on disk right now.
type Message struct {
	Key   string // stable identifier: the base filename before any ":2," suffix
	Flags string // flag letters present, e.g. "FS"
	InNew bool   // true if currently sitting in new/ (not yet moved to cur/)
}

func splitFlags(entryName string) (key, flags string) {
	idx := strings.Index(entryName, ":2,")
	if idx == -1 {
		return entryName, ""
	}
	return entryName[:idx], entryName[idx+3:]
}

func hasFlag(flags string, letter byte) bool {
	return strings.IndexByte(flags, letter) != -1
}

// List returns every message currently in new/ and cur/, sorted by key
// (which embeds a delivery timestamp, so this is delivery order).
func (m *Maildir) List() ([]Message, error) {
	var msgs []Message

	newEntries, err := os.ReadDir(filepath.Join(m.root, "new"))
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading new/: %w", err)
	}
	for _, e := range newEntries {
		if e.IsDir() {
			continue
		}
		key, flags := splitFlags(e.Name())
		msgs = append(msgs, Message{Key: key, Flags: flags, InNew: true})
	}

	curEntries, err := os.ReadDir(filepath.Join(m.root, "cur"))
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading cur/: %w", err)
	}
	for _, e := range curEntries {
		if e.IsDir() {
			continue
		}
		key, flags := splitFlags(e.Name())
		msgs = append(msgs, Message{Key: key, Flags: flags, InNew: false})
	}

	sort.Slice(msgs, func(i, j int) bool { return msgs[i].Key < msgs[j].Key })
	return msgs, nil
}

// find locates the on-disk filename (and directory: "new" or "cur") for a
// message key, since STORE/EXPUNGE need to know where it currently lives.
func (m *Maildir) find(key string) (dir, filename string, err error) {
	for _, d := range []string{"new", "cur"} {
		entries, readErr := os.ReadDir(filepath.Join(m.root, d))
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue
			}
			return "", "", readErr
		}
		for _, e := range entries {
			k, _ := splitFlags(e.Name())
			if k == key {
				return d, e.Name(), nil
			}
		}
	}
	return "", "", os.ErrNotExist
}

// Open returns the full message contents (headers + body) for key.
func (m *Maildir) Open(key string) ([]byte, error) {
	dir, filename, err := m.find(key)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(m.root, dir, filename))
}

// Stat returns file metadata (size, mtime) for key.
func (m *Maildir) Stat(key string) (os.FileInfo, error) {
	dir, filename, err := m.find(key)
	if err != nil {
		return nil, err
	}
	return os.Stat(filepath.Join(m.root, dir, filename))
}

// SetFlags overwrites a message's flag set and, if it's still in new/, moves
// it into cur/ (the standard Maildir transition: once flags/state have been
// touched by a client, the message is no longer "new").
func (m *Maildir) SetFlags(key string, flags string) error {
	dir, filename, err := m.find(key)
	if err != nil {
		return err
	}

	// Normalize to the required ASCII sort order.
	var b strings.Builder
	for i := 0; i < len(flagLetters); i++ {
		if hasFlag(flags, flagLetters[i]) {
			b.WriteByte(flagLetters[i])
		}
	}
	newName := key + ":2," + b.String()

	oldPath := filepath.Join(m.root, dir, filename)
	newPath := filepath.Join(m.root, "cur", newName)
	if err := os.Rename(oldPath, newPath); err != nil {
		return fmt.Errorf("updating flags: %w", err)
	}
	return nil
}

// Remove permanently deletes a message (IMAP EXPUNGE of a \Deleted message).
func (m *Maildir) Remove(key string) error {
	dir, filename, err := m.find(key)
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(m.root, dir, filename))
}
