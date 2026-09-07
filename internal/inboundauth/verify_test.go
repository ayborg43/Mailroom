package inboundauth

import (
	"testing"

	"github.com/emersion/go-msgauth/dmarc"
)

func TestAlignedExactMatch(t *testing.T) {
	if !aligned("example.com", "example.com", dmarc.AlignmentStrict) {
		t.Error("exact match should align under strict mode")
	}
}

func TestAlignedStrictRejectsSubdomain(t *testing.T) {
	if aligned("example.com", "mail.example.com", dmarc.AlignmentStrict) {
		t.Error("subdomain should not align under strict mode")
	}
}

func TestAlignedRelaxedAcceptsSubdomain(t *testing.T) {
	if !aligned("example.com", "mail.example.com", dmarc.AlignmentRelaxed) {
		t.Error("subdomain should align with parent org domain under relaxed mode")
	}
	if !aligned("mail.example.com", "example.com", dmarc.AlignmentRelaxed) {
		t.Error("parent org domain should align with subdomain under relaxed mode")
	}
}

// TestAlignedRelaxedRespectsPublicSuffix is the reason this package pulls in
// golang.org/x/net/publicsuffix instead of naive suffix matching: without
// it, "attacker.co.uk" would incorrectly appear aligned with "victim.co.uk"
// (both "end with" the multi-label public suffix ".co.uk").
func TestAlignedRelaxedRespectsPublicSuffix(t *testing.T) {
	if aligned("victim.co.uk", "attacker.co.uk", dmarc.AlignmentRelaxed) {
		t.Error("two different registrants under the same public suffix must not align")
	}
	if !aligned("mail.example.co.uk", "example.co.uk", dmarc.AlignmentRelaxed) {
		t.Error("subdomain of a registrable domain under a multi-label public suffix should align")
	}
}

func TestAlignedUnrelated(t *testing.T) {
	if aligned("example.com", "evil.com", dmarc.AlignmentRelaxed) {
		t.Error("unrelated domains should not align")
	}
}

func TestExtractFromDomain(t *testing.T) {
	msg := []byte("From: Alice <alice@example.com>\r\nTo: bob@example.org\r\nSubject: hi\r\n\r\nbody\r\n")
	if got := extractFromDomain(msg); got != "example.com" {
		t.Errorf("extractFromDomain = %q, want example.com", got)
	}
}

func TestExtractFromDomainMissing(t *testing.T) {
	msg := []byte("To: bob@example.org\r\nSubject: hi\r\n\r\nbody\r\n")
	if got := extractFromDomain(msg); got != "" {
		t.Errorf("extractFromDomain = %q, want empty string for missing From header", got)
	}
}
