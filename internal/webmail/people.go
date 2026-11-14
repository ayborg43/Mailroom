package webmail

import (
	"hash/fnv"
	"net/mail"
	"strconv"
	"strings"
	"unicode"
)

// Helpers for showing the people on a message. The mailbox layer hands the
// templates raw From/To header text ("NRS REV360 <rev360donotreply@nrs.example>");
// the list reads far better with just the name.

// parseAddrs parses a header value into addresses, tolerating junk (spam
// and mailing-list mail has plenty) by returning nil instead of failing.
func parseAddrs(header string) []*mail.Address {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil
	}
	if list, err := mail.ParseAddressList(header); err == nil {
		return list
	}
	if one, err := mail.ParseAddress(header); err == nil {
		return []*mail.Address{one}
	}
	return nil
}

// displayName is the human-readable form of a From/To header: each
// recipient's name, or its address when it has none, comma-separated. Text it
// can't parse is returned as-is.
func displayName(header string) string {
	addrs := parseAddrs(header)
	if len(addrs) == 0 {
		return strings.TrimSpace(header)
	}
	names := make([]string, len(addrs))
	for i, a := range addrs {
		if a.Name != "" {
			names[i] = a.Name
		} else {
			names[i] = a.Address
		}
	}
	return strings.Join(names, ", ")
}

// displayAddr is the bare address(es) behind displayName, for tooltips.
func displayAddr(header string) string {
	addrs := parseAddrs(header)
	if len(addrs) == 0 {
		return strings.TrimSpace(header)
	}
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = a.Address
	}
	return strings.Join(out, ", ")
}

// avatarInitial is the first letter or digit of the first person's name (or,
// failing that, address), upper-cased, for the avatar circle.
func avatarInitial(header string) string {
	candidates := []string{displayName(header)}
	if addrs := parseAddrs(header); len(addrs) > 0 {
		candidates = append(candidates, addrs[0].Address) // name was all punctuation
	}
	for _, c := range candidates {
		for _, r := range c {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				return string(unicode.ToUpper(r))
			}
		}
	}
	return "?"
}

// avatarClass picks one of eight stable palette classes (av0..av7) for a
// sender, hashed from the address so the same sender is always the same
// colour.
func avatarClass(header string) string {
	key := strings.ToLower(displayAddr(header))
	if addrs := parseAddrs(header); len(addrs) > 0 {
		key = strings.ToLower(addrs[0].Address)
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return "av" + strconv.Itoa(int(h.Sum32()%8))
}
