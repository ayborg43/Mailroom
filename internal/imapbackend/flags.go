package imapbackend

import (
	"strings"

	"github.com/emersion/go-imap/v2"
)

// The database is the source of truth for a message's full flag set
// (including custom/keyword flags, e.g. "$Forwarded", which Maildir's
// tmp/new/cur naming has no room for). The on-disk ":2,FLAGS" suffix is kept
// in sync on a best-effort basis, using only the standard letters, so other
// Maildir-aware tools see a reasonable approximation.
var maildirLetterToFlag = map[byte]imap.Flag{
	'S': imap.FlagSeen,
	'R': imap.FlagAnswered,
	'F': imap.FlagFlagged,
	'T': imap.FlagDeleted,
	'D': imap.FlagDraft,
}

var flagToMaildirLetter = map[imap.Flag]byte{
	imap.FlagSeen:     'S',
	imap.FlagAnswered: 'R',
	imap.FlagFlagged:  'F',
	imap.FlagDeleted:  'T',
	imap.FlagDraft:    'D',
}

func flagSetFromMaildirLetters(letters string) map[imap.Flag]struct{} {
	set := make(map[imap.Flag]struct{})
	for i := 0; i < len(letters); i++ {
		if flag, ok := maildirLetterToFlag[letters[i]]; ok {
			set[flag] = struct{}{}
		}
	}
	return set
}

func maildirLettersFromFlagList(flags []imap.Flag) string {
	var b strings.Builder
	for _, flag := range flags {
		if letter, ok := flagToMaildirLetter[canonicalFlag(flag)]; ok {
			b.WriteByte(letter)
		}
	}
	return b.String()
}

// encodeFlags/decodeFlags serialize the full IMAP flag set (including
// keywords) into the database's "flags" text column as a space-separated
// list.
func encodeFlags(flags []imap.Flag) string {
	strs := make([]string, len(flags))
	for i, f := range flags {
		strs[i] = string(f)
	}
	return strings.Join(strs, " ")
}

func decodeFlags(s string) map[imap.Flag]struct{} {
	set := make(map[imap.Flag]struct{})
	for _, f := range strings.Fields(s) {
		set[canonicalFlag(imap.Flag(f))] = struct{}{}
	}
	return set
}
