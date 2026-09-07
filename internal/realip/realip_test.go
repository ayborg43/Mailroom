package realip

import (
	"net"
	"net/http"
	"testing"
)

func mustTrusted(t *testing.T, entries ...string) TrustedProxies {
	t.Helper()
	tp, err := Parse(entries)
	if err != nil {
		t.Fatalf("Parse(%v): %v", entries, err)
	}
	return tp
}

func TestFromHTTPUntrustedPeerIgnoresHeader(t *testing.T) {
	// The core "production incident" scenario: a direct, untrusted client
	// forges X-Forwarded-For to look like someone (or something) else. It
	// must never be believed — the real TCP peer is the only fact we know.
	trusted := mustTrusted(t, "10.0.0.0/24")
	r := &http.Request{
		RemoteAddr: "203.0.113.7:54321",
		Header:     http.Header{"X-Forwarded-For": []string{"10.0.0.5"}}, // forged: claims to BE the trusted proxy
	}
	got := FromHTTP(r, trusted)
	if got != "203.0.113.7" {
		t.Fatalf("FromHTTP = %q, want the untrusted peer's real address 203.0.113.7 (header must be ignored)", got)
	}
}

func TestFromHTTPTrustedProxySingleHop(t *testing.T) {
	trusted := mustTrusted(t, "10.0.0.0/24")
	r := &http.Request{
		RemoteAddr: "10.0.0.5:443",
		Header:     http.Header{"X-Forwarded-For": []string{"203.0.113.7"}},
	}
	got := FromHTTP(r, trusted)
	if got != "203.0.113.7" {
		t.Fatalf("FromHTTP = %q, want the real client 203.0.113.7", got)
	}
}

func TestFromHTTPNoHeaderFallsBackToPeer(t *testing.T) {
	trusted := mustTrusted(t, "10.0.0.0/24")
	r := &http.Request{RemoteAddr: "10.0.0.5:443", Header: http.Header{}}
	got := FromHTTP(r, trusted)
	if got != "10.0.0.5" {
		t.Fatalf("FromHTTP = %q, want 10.0.0.5 when no header is present", got)
	}
}

func TestFromHTTPMultiHopChain(t *testing.T) {
	// CDN -> Traefik -> us. Both are trusted; walk right-to-left past both
	// trusted hops to find the real client.
	trusted := mustTrusted(t, "10.0.0.0/24", "192.168.1.0/24")
	r := &http.Request{
		RemoteAddr: "10.0.0.5:443", // Traefik
		Header:     http.Header{"X-Forwarded-For": []string{"203.0.113.7, 192.168.1.9"}},
	}
	got := FromHTTP(r, trusted)
	if got != "203.0.113.7" {
		t.Fatalf("FromHTTP = %q, want the real client 203.0.113.7 past both trusted hops", got)
	}
}

func TestFromHTTPAllHopsClaimTrusted(t *testing.T) {
	// Degenerate/adversarial case: every entry in the header (including a
	// forged prefix) looks like a trusted IP. Fall back to the real peer
	// rather than picking an attacker-chosen entry.
	trusted := mustTrusted(t, "10.0.0.0/24")
	r := &http.Request{
		RemoteAddr: "10.0.0.5:443",
		Header:     http.Header{"X-Forwarded-For": []string{"10.0.0.9, 10.0.0.8"}},
	}
	got := FromHTTP(r, trusted)
	if got != "10.0.0.5" {
		t.Fatalf("FromHTTP = %q, want fallback to the real peer 10.0.0.5", got)
	}
}

func TestFromHTTPMalformedRemoteAddr(t *testing.T) {
	trusted := mustTrusted(t, "10.0.0.0/24")
	r := &http.Request{RemoteAddr: "not-an-address", Header: http.Header{}}
	got := FromHTTP(r, trusted)
	if got != "not-an-address" {
		t.Fatalf("FromHTTP = %q, want raw RemoteAddr passthrough on parse failure", got)
	}
}

func TestTrustedProxiesContainsBareIP(t *testing.T) {
	tp := mustTrusted(t, "10.0.0.5")
	if !tp.Contains(mustParseIP(t, "10.0.0.5")) {
		t.Fatal("bare IP entry should match itself")
	}
	if tp.Contains(mustParseIP(t, "10.0.0.6")) {
		t.Fatal("bare IP entry should not match a different address")
	}
}

func mustParseIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("ParseIP(%q) failed", s)
	}
	return ip
}
