package imapbackend

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
)

// message is one Maildir file as seen by IMAP: stable UID, current flags,
// and enough metadata to answer FETCH/SEARCH without touching disk. The
// message body itself is loaded from the Maildir on demand (see data()),
// not cached here, so a mailbox with many large messages doesn't need to
// hold them all in memory at once.
type message struct {
	mbox *Mailbox // immutable

	// immutable
	uid  imap.UID
	key  string
	t    time.Time
	size int64

	// mutable, protected by Mailbox.mu
	flags map[imap.Flag]struct{}
}

func (msg *message) data() ([]byte, error) {
	return msg.mbox.md.Open(msg.key)
}

func (msg *message) flagList() []imap.Flag {
	var flags []imap.Flag
	for flag := range msg.flags {
		flags = append(flags, flag)
	}
	return flags
}

func (msg *message) hasFlag(flag imap.Flag) bool {
	_, ok := msg.flags[canonicalFlag(flag)]
	return ok
}

func (msg *message) fetch(w *imapserver.FetchResponseWriter, options *imap.FetchOptions) error {
	w.WriteUID(msg.uid)

	if options.Flags {
		w.WriteFlags(msg.flagList())
	}
	if options.InternalDate {
		w.WriteInternalDate(msg.t)
	}
	if options.RFC822Size {
		w.WriteRFC822Size(msg.size)
	}

	needBuf := options.Envelope || options.BodyStructure != nil ||
		len(options.BodySection) > 0 || len(options.BinarySection) > 0 || len(options.BinarySectionSize) > 0
	var buf []byte
	if needBuf {
		var err error
		buf, err = msg.data()
		if err != nil {
			return fmt.Errorf("reading message %s: %w", msg.key, err)
		}
	}

	if options.Envelope {
		w.WriteEnvelope(envelopeFromBuf(buf))
	}
	if options.BodyStructure != nil {
		w.WriteBodyStructure(imapserver.ExtractBodyStructure(bytes.NewReader(buf)))
	}

	for _, bs := range options.BodySection {
		section := imapserver.ExtractBodySection(bytes.NewReader(buf), bs)
		wc := w.WriteBodySection(bs, int64(len(section)))
		_, writeErr := wc.Write(section)
		closeErr := wc.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}

	for _, bs := range options.BinarySection {
		section := imapserver.ExtractBinarySection(bytes.NewReader(buf), bs)
		wc := w.WriteBinarySection(bs, int64(len(section)))
		_, writeErr := wc.Write(section)
		closeErr := wc.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}

	for _, bss := range options.BinarySectionSize {
		n := imapserver.ExtractBinarySectionSize(bytes.NewReader(buf), bss)
		w.WriteBinarySectionSize(bss, n)
	}

	return w.Close()
}

func envelopeFromBuf(buf []byte) *imap.Envelope {
	br := bufio.NewReader(bytes.NewReader(buf))
	header, err := textproto.ReadHeader(br)
	if err != nil {
		return nil
	}
	return imapserver.ExtractEnvelope(header)
}

func (msg *message) store(store *imap.StoreFlags) {
	switch store.Op {
	case imap.StoreFlagsSet:
		msg.flags = make(map[imap.Flag]struct{})
		fallthrough
	case imap.StoreFlagsAdd:
		for _, flag := range store.Flags {
			msg.flags[canonicalFlag(flag)] = struct{}{}
		}
	case imap.StoreFlagsDel:
		for _, flag := range store.Flags {
			delete(msg.flags, canonicalFlag(flag))
		}
	default:
		panic(fmt.Errorf("unknown STORE flag operation: %v", store.Op))
	}
}

func (msg *message) reader() *gomessage.Entity {
	buf, err := msg.data()
	if err != nil {
		e, _ := gomessage.New(gomessage.Header{}, bytes.NewReader(nil))
		return e
	}
	r, _ := gomessage.Read(bytes.NewReader(buf))
	if r == nil {
		r, _ = gomessage.New(gomessage.Header{}, bytes.NewReader(nil))
	}
	return r
}

// search implements the subset of RFC 3501 SEARCH criteria go-imap parses
// for us; the matching logic mirrors emersion/go-imap's own imapmemserver
// reference implementation.
func (msg *message) search(seqNum uint32, criteria *imap.SearchCriteria) bool {
	for _, seqSet := range criteria.SeqNum {
		if seqNum == 0 || !seqSet.Contains(seqNum) {
			return false
		}
	}
	for _, uidSet := range criteria.UID {
		if !uidSet.Contains(msg.uid) {
			return false
		}
	}
	if !matchDate(msg.t, criteria.Since, criteria.Before) {
		return false
	}

	for _, flag := range criteria.Flag {
		if !msg.hasFlag(flag) {
			return false
		}
	}
	for _, flag := range criteria.NotFlag {
		if msg.hasFlag(flag) {
			return false
		}
	}

	if criteria.Larger != 0 && msg.size <= criteria.Larger {
		return false
	}
	if criteria.Smaller != 0 && msg.size >= criteria.Smaller {
		return false
	}

	header := mail.Header{Header: msg.reader().Header}

	for _, fieldCriteria := range criteria.Header {
		if !matchHeaderFields(header.FieldsByKey(fieldCriteria.Key), fieldCriteria.Value) {
			return false
		}
	}

	if !criteria.SentSince.IsZero() || !criteria.SentBefore.IsZero() {
		t, err := header.Date()
		if err != nil {
			return false
		} else if !matchDate(t, criteria.SentSince, criteria.SentBefore) {
			return false
		}
	}

	for _, text := range criteria.Text {
		if !matchEntity(msg.reader(), text, true) {
			return false
		}
	}
	for _, body := range criteria.Body {
		if !matchEntity(msg.reader(), body, false) {
			return false
		}
	}

	for _, not := range criteria.Not {
		if msg.search(seqNum, &not) {
			return false
		}
	}
	for _, or := range criteria.Or {
		if !msg.search(seqNum, &or[0]) && !msg.search(seqNum, &or[1]) {
			return false
		}
	}

	return true
}

func matchDate(t, since, before time.Time) bool {
	// RFC 3501 explicitly requires zone-unaware date comparison.
	t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)

	if !since.IsZero() && t.Before(since) {
		return false
	}
	if !before.IsZero() && !t.Before(before) {
		return false
	}
	return true
}

func matchHeaderFields(fields gomessage.HeaderFields, pattern string) bool {
	if pattern == "" {
		return fields.Len() > 0
	}

	pattern = strings.ToLower(pattern)
	for fields.Next() {
		v, _ := fields.Text()
		if strings.Contains(strings.ToLower(v), pattern) {
			return true
		}
	}
	return false
}

func matchEntity(e *gomessage.Entity, pattern string, includeHeader bool) bool {
	if pattern == "" {
		return true
	}

	if includeHeader && matchHeaderFields(e.Header.Fields(), pattern) {
		return true
	}

	if mr := e.MultipartReader(); mr != nil {
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			} else if err != nil {
				return false
			}
			if matchEntity(part, pattern, includeHeader) {
				return true
			}
		}
		return false
	}

	t, _, err := e.Header.ContentType()
	if err != nil {
		return false
	}
	if !strings.HasPrefix(t, "text/") && !strings.HasPrefix(t, "message/") {
		return false
	}

	buf, err := io.ReadAll(e.Body)
	if err != nil {
		return false
	}
	return bytes.Contains(bytes.ToLower(buf), bytes.ToLower([]byte(pattern)))
}

func canonicalFlag(flag imap.Flag) imap.Flag {
	return imap.Flag(strings.ToLower(string(flag)))
}
