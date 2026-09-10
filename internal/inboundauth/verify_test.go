package inboundauth

import (
	"net"
	"testing"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/authres"
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

// fakeDNS builds a lookupTXT function from a fixed domain -> TXT records
// map, so DMARC tests never touch real nameservers.
func fakeDNS(records map[string][]string) func(domain string) ([]string, error) {
	return func(domain string) ([]string, error) {
		if txt, ok := records[domain]; ok {
			return txt, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: domain, IsNotFound: true}
	}
}

func TestCheckDMARCFailWithRejectPolicyQuarantines(t *testing.T) {
	lookup := fakeDNS(map[string][]string{
		"_dmarc.example.com": {"v=DMARC1; p=reject"},
	})
	result, shouldQuarantine := checkDMARC(lookup, "example.com", spf.Fail, "other.com", nil)
	dr, ok := result.(*authres.DMARCResult)
	if !ok || dr.Value != authres.ResultFail {
		t.Fatalf("result = %+v, want DMARCResult{Value: fail}", result)
	}
	if !shouldQuarantine {
		t.Error("a failed DMARC check under p=reject should quarantine")
	}
}

func TestCheckDMARCFailWithNonePolicyDoesNotQuarantine(t *testing.T) {
	// A domain publishing p=none is explicitly asking receivers not to act
	// on failures — typically because it's still rolling DMARC out.
	lookup := fakeDNS(map[string][]string{
		"_dmarc.example.com": {"v=DMARC1; p=none"},
	})
	_, shouldQuarantine := checkDMARC(lookup, "example.com", spf.Fail, "other.com", nil)
	if shouldQuarantine {
		t.Error("p=none must never quarantine, even on a DMARC failure")
	}
}

func TestCheckDMARCPassNeverQuarantines(t *testing.T) {
	lookup := fakeDNS(map[string][]string{
		"_dmarc.example.com": {"v=DMARC1; p=reject"},
	})
	// SPF passes and is aligned with the From domain.
	result, shouldQuarantine := checkDMARC(lookup, "example.com", spf.Pass, "example.com", nil)
	dr, ok := result.(*authres.DMARCResult)
	if !ok || dr.Value != authres.ResultPass {
		t.Fatalf("result = %+v, want DMARCResult{Value: pass}", result)
	}
	if shouldQuarantine {
		t.Error("an aligned pass must never quarantine, regardless of policy")
	}
}

func TestCheckDMARCNoRecordFound(t *testing.T) {
	lookup := fakeDNS(nil)
	result, shouldQuarantine := checkDMARC(lookup, "example.com", spf.Fail, "other.com", nil)
	dr, ok := result.(*authres.DMARCResult)
	if !ok || dr.Value != authres.ResultNone {
		t.Fatalf("result = %+v, want DMARCResult{Value: none}", result)
	}
	if shouldQuarantine {
		t.Error("no DMARC record at all must never quarantine")
	}
}

// TestLookupDMARCRecordSubdomainFallback is the RFC 7489 §6.6.3 gap that
// dmarc.LookupWithOptions doesn't handle on its own: a subdomain with no
// DMARC record of its own must fall back to its organizational domain's
// record, and use that record's *subdomain* policy (sp=), not its
// top-level policy (p=) — the two can legitimately differ.
func TestLookupDMARCRecordSubdomainFallback(t *testing.T) {
	lookup := fakeDNS(map[string][]string{
		// no record at _dmarc.mail.example.com
		"_dmarc.example.com": {"v=DMARC1; p=none; sp=quarantine"},
	})
	record, policy, err := lookupDMARCRecord(lookup, "mail.example.com")
	if err != nil {
		t.Fatalf("lookupDMARCRecord: %v", err)
	}
	if record.Policy != dmarc.PolicyNone {
		t.Errorf("record.Policy = %q, want none (the record found at example.com)", record.Policy)
	}
	if policy != dmarc.PolicyQuarantine {
		t.Errorf("policy = %q, want quarantine (sp= from the org domain's record applies to a subdomain)", policy)
	}
}

func TestLookupDMARCRecordSubdomainFallsBackToTopLevelPolicyWithoutSp(t *testing.T) {
	lookup := fakeDNS(map[string][]string{
		"_dmarc.example.com": {"v=DMARC1; p=reject"}, // no sp= at all
	})
	_, policy, err := lookupDMARCRecord(lookup, "mail.example.com")
	if err != nil {
		t.Fatalf("lookupDMARCRecord: %v", err)
	}
	if policy != dmarc.PolicyReject {
		t.Errorf("policy = %q, want reject (falls back to p= when sp= isn't set)", policy)
	}
}

func TestLookupDMARCRecordExactDomainNeverUsesSubdomainPolicy(t *testing.T) {
	// The record is found at fromDomain itself, so its own p= applies
	// regardless of what sp= says — sp= only matters for descendants.
	lookup := fakeDNS(map[string][]string{
		"_dmarc.example.com": {"v=DMARC1; p=quarantine; sp=reject"},
	})
	_, policy, err := lookupDMARCRecord(lookup, "example.com")
	if err != nil {
		t.Fatalf("lookupDMARCRecord: %v", err)
	}
	if policy != dmarc.PolicyQuarantine {
		t.Errorf("policy = %q, want quarantine (p=, not sp=, for the domain that owns the record)", policy)
	}
}

func TestLookupDMARCRecordNoRecordAnywhereUpToOrgDomain(t *testing.T) {
	lookup := fakeDNS(nil)
	_, _, err := lookupDMARCRecord(lookup, "deep.sub.example.com")
	if err != dmarc.ErrNoPolicy {
		t.Errorf("err = %v, want dmarc.ErrNoPolicy", err)
	}
}
