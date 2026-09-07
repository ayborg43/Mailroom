// Package realip resolves the real client address for a request that may
// have passed through a reverse proxy, without ever trusting a forwarded
// address from a source that isn't a configured trusted proxy.
//
// This distinction matters because of a real production incident: naive
// IP-blocking that trusted whatever a client claimed (or blocked based on
// the proxy's own peer address instead of resolving the real client)
// ended up banning the reverse proxy itself, taking down the entire
// service for every user behind it. The one invariant every function here
// preserves is: an untrusted peer's claims about "who I'm forwarding for"
// are never believed, no matter what header or protocol preamble it sends.
package realip

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// TrustedProxies is a set of IP ranges permitted to supply a forwarded
// client address on their own connection's behalf.
type TrustedProxies []*net.IPNet

// Parse builds a TrustedProxies set from config strings, each either a bare
// IP ("10.0.0.5") or a CIDR range ("10.0.0.0/24").
func Parse(entries []string) (TrustedProxies, error) {
	out := make(TrustedProxies, 0, len(entries))
	for _, e := range entries {
		_, ipnet, err := net.ParseCIDR(normalizeCIDR(e))
		if err != nil {
			return nil, fmt.Errorf("invalid trusted_proxies entry %q: %w", e, err)
		}
		out = append(out, ipnet)
	}
	return out, nil
}

func normalizeCIDR(s string) string {
	if strings.Contains(s, "/") {
		return s
	}
	if strings.Contains(s, ":") {
		return s + "/128"
	}
	return s + "/32"
}

// Contains reports whether ip falls within any trusted range.
func (t TrustedProxies) Contains(ip net.IP) bool {
	for _, n := range t {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// FromHTTP resolves the real client IP for r. If the immediate TCP peer
// isn't a trusted proxy, its address is authoritative and any
// X-Forwarded-For header is ignored outright — an untrusted party's claims
// about who it's forwarding for are worthless. Only when the peer is
// trusted do we walk X-Forwarded-For from the right (the end each hop
// appends its own observed peer to) and return the first entry that isn't
// itself a trusted proxy, which is the real client regardless of how many
// trusted hops sit in the chain.
func FromHTTP(r *http.Request, trusted TrustedProxies) string {
	peerHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peerHost = r.RemoteAddr
	}

	peerIP := net.ParseIP(peerHost)
	if peerIP == nil || !trusted.Contains(peerIP) {
		return peerHost
	}

	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return peerHost
	}

	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(parts[i])
		ip := net.ParseIP(candidate)
		if ip == nil {
			continue
		}
		if !trusted.Contains(ip) {
			return candidate
		}
	}

	// Every entry in the chain, including the client-supplied one, claims
	// to be a trusted proxy. Fall back to the immediate peer rather than
	// trusting a header value an attacker could have fully controlled.
	return peerHost
}
