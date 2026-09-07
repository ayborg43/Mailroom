// Package address provides the one address-splitting helper shared by the
// SMTP servers and the admin CLI.
package address

import "strings"

// Split splits "local@domain" on the last '@'. It doesn't handle quoted
// local parts containing '@', which is an accepted limitation for this
// server (as it is for most non-purist MTAs).
func Split(addr string) (local, domain string, ok bool) {
	i := strings.LastIndexByte(addr, '@')
	if i <= 0 || i == len(addr)-1 {
		return "", "", false
	}
	return addr[:i], addr[i+1:], true
}
