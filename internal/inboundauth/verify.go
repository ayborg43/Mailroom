// Package inboundauth verifies SPF, DKIM, and DMARC on inbound mail and
// formats the result as an RFC 8601 Authentication-Results header. It
// never rejects mail outright based on the result, but Result.ShouldQuarantine
// does drive one soft, reversible action: routing to Junk instead of
// INBOX when the sending domain's own DMARC policy asks for it (see
// Result for exactly when that is). Anything beyond that — outright
// rejection, spam scoring on the SPF/DKIM results individually — is left
// to whatever reads the header downstream.
//
// DMARC alignment uses golang.org/x/net/publicsuffix for the organizational
// domain comparison relaxed alignment requires — the same public-suffix
// list logic browsers use for cookie scoping, so "mail.example.co.uk" and
// "example.co.uk" are correctly recognized as aligned while "co.uk" alone
// is not treated as a registrable domain.
package inboundauth

import (
	"bytes"
	"context"
	"net"
	"net/mail"
	"strings"
	"time"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
	"golang.org/x/net/publicsuffix"

	"github.com/sociolytik/mailserver/internal/address"
	"github.com/sociolytik/mailserver/internal/metrics"
)

// dnsTimeout bounds each of the SPF/DKIM/DMARC DNS lookup passes, so a slow
// or unresponsive DNS server delays inbound delivery by seconds, not
// indefinitely.
const dnsTimeout = 10 * time.Second

// Result is the outcome of verifying one inbound message.
type Result struct {
	// AuthenticationResults is the formatted header value (RFC 8601),
	// without the header name or trailing CRLF.
	AuthenticationResults string

	// ShouldQuarantine is true only when DMARC was evaluated against a
	// found policy record, failed outright, and that record (or the
	// applicable subdomain policy — see lookupDMARCRecord) actually
	// requests action ("quarantine" or "reject"). A sending domain
	// publishing "p=none" is explicitly asking receivers not to act on
	// failures (typically because it's still rolling DMARC out), so a
	// "none" policy is honored here rather than quarantining anyway —
	// same for "none"/temperror/permerror verdicts, which reached no
	// definitive failure at all.
	ShouldQuarantine bool
}

// Verify runs SPF, DKIM, and DMARC checks on an inbound message.
// authServID is normally the receiving server's own hostname, used as the
// Authentication-Results reporting identity.
func Verify(authServID string, clientIP net.IP, helo, mailFrom string, rawMessage []byte) Result {
	var results []authres.Result

	spfResult, spfDomain := checkSPF(clientIP, helo, mailFrom)
	metrics.InboundAuthResults.WithLabelValues("spf", string(spfResult)).Inc()
	results = append(results, &authres.SPFResult{
		Value: authres.ResultValue(spfResult),
		From:  spfDomain,
		Helo:  helo,
	})

	dkimResults := checkDKIM(rawMessage)
	for _, r := range dkimResults {
		if dr, ok := r.(*authres.DKIMResult); ok {
			metrics.InboundAuthResults.WithLabelValues("dkim", string(dr.Value)).Inc()
		}
	}
	results = append(results, dkimResults...)

	dmarcCtx, dmarcCancel := context.WithTimeout(context.Background(), dnsTimeout)
	defer dmarcCancel()

	fromDomain := extractFromDomain(rawMessage)
	dmarcResult, shouldQuarantine := checkDMARC(boundedLookupTXT(dmarcCtx), fromDomain, spfResult, spfDomain, dkimResults)
	if dmarcResult != nil {
		if dr, ok := dmarcResult.(*authres.DMARCResult); ok {
			metrics.InboundAuthResults.WithLabelValues("dmarc", string(dr.Value)).Inc()
		}
		results = append(results, dmarcResult)
	}

	return Result{
		AuthenticationResults: authres.Format(authServID, results),
		ShouldQuarantine:      shouldQuarantine,
	}
}

func checkSPF(clientIP net.IP, helo, mailFrom string) (spf.Result, string) {
	if clientIP == nil {
		return spf.None, ""
	}

	domain := helo
	if _, d, ok := address.Split(mailFrom); ok {
		domain = d
	}
	if domain == "" {
		return spf.None, ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), dnsTimeout)
	defer cancel()

	result, _ := spf.CheckHostWithSender(clientIP, helo, mailFrom, spf.WithContext(ctx))
	return result, domain
}

func checkDKIM(rawMessage []byte) []authres.Result {
	ctx, cancel := context.WithTimeout(context.Background(), dnsTimeout)
	defer cancel()

	verifications, err := dkim.VerifyWithOptions(bytes.NewReader(rawMessage), &dkim.VerifyOptions{
		LookupTXT: boundedLookupTXT(ctx),
	})
	if err != nil || len(verifications) == 0 {
		return []authres.Result{&authres.DKIMResult{Value: authres.ResultNone}}
	}

	results := make([]authres.Result, 0, len(verifications))
	for _, v := range verifications {
		value := authres.ResultValue(authres.ResultPass)
		reason := ""
		if v.Err != nil {
			reason = v.Err.Error()
			switch {
			case dkim.IsTempFail(v.Err):
				value = authres.ResultTempError
			case dkim.IsPermFail(v.Err):
				value = authres.ResultPermError
			default:
				value = authres.ResultFail
			}
		}
		results = append(results, &authres.DKIMResult{
			Value:      value,
			Reason:     reason,
			Domain:     v.Domain,
			Identifier: v.Identifier,
		})
	}
	return results
}

// checkDMARC returns the Authentication-Results DMARC entry and whether
// the sending domain's own policy actually asks receivers to act on a
// failure. lookupTXT is injectable so tests can supply canned DNS
// responses instead of hitting real nameservers (see checkDKIM/dkim.Verify
// for the same pattern).
func checkDMARC(lookupTXT func(domain string) ([]string, error), fromDomain string, spfResult spf.Result, spfDomain string, dkimResults []authres.Result) (result authres.Result, shouldQuarantine bool) {
	if fromDomain == "" {
		return nil, false
	}

	record, policy, err := lookupDMARCRecord(lookupTXT, fromDomain)
	if err != nil {
		if err == dmarc.ErrNoPolicy {
			return &authres.DMARCResult{Value: authres.ResultNone, From: fromDomain}, false
		}
		value := authres.ResultValue(authres.ResultPermError)
		if dmarc.IsTempFail(err) {
			value = authres.ResultTempError
		}
		return &authres.DMARCResult{Value: value, From: fromDomain, Reason: err.Error()}, false
	}

	spfAligned := spfResult == spf.Pass && aligned(fromDomain, spfDomain, record.SPFAlignment)

	dkimAligned := false
	for _, r := range dkimResults {
		dr, ok := r.(*authres.DKIMResult)
		if !ok || dr.Value != authres.ResultPass {
			continue
		}
		if aligned(fromDomain, dr.Domain, record.DKIMAlignment) {
			dkimAligned = true
			break
		}
	}

	if spfAligned || dkimAligned {
		return &authres.DMARCResult{Value: authres.ResultPass, From: fromDomain}, false
	}

	failResult := &authres.DMARCResult{Value: authres.ResultFail, From: fromDomain}
	return failResult, policy == dmarc.PolicyQuarantine || policy == dmarc.PolicyReject
}

// lookupDMARCRecord implements RFC 7489 §6.6.3's discovery algorithm: try
// fromDomain's own _dmarc TXT record; if it publishes none, walk up one
// label at a time until the organizational domain (inclusive) and use the
// first record found there instead. When the record actually used was
// found above fromDomain itself, its subdomain policy ("sp=") is what
// applies — falling back to the top-level policy if sp= wasn't set, per
// spec — since that's what the publishing domain intended for exactly
// this situation (a subdomain with no DMARC record of its own).
func lookupDMARCRecord(lookupTXT func(domain string) ([]string, error), fromDomain string) (record *dmarc.Record, policy dmarc.Policy, err error) {
	fromDomain = strings.ToLower(fromDomain)

	orgDomain, orgErr := publicsuffix.EffectiveTLDPlusOne(fromDomain)
	if orgErr != nil {
		orgDomain = fromDomain
	}

	domain := fromDomain
	for {
		record, err = dmarc.LookupWithOptions(domain, &dmarc.LookupOptions{LookupTXT: lookupTXT})
		if err == nil {
			policy = record.Policy
			if domain != fromDomain && record.SubdomainPolicy != "" {
				policy = record.SubdomainPolicy
			}
			return record, policy, nil
		}
		if err != dmarc.ErrNoPolicy {
			return nil, "", err
		}
		if domain == orgDomain {
			return nil, "", dmarc.ErrNoPolicy
		}

		_, rest, ok := strings.Cut(domain, ".")
		if !ok {
			return nil, "", dmarc.ErrNoPolicy
		}
		domain = rest
	}
}

// aligned reports whether otherDomain is DMARC-aligned with fromDomain
// under mode: an exact match always aligns; under relaxed alignment (the
// default), sharing the same organizational (public-suffix-aware) domain
// also aligns.
func aligned(fromDomain, otherDomain string, mode dmarc.AlignmentMode) bool {
	if otherDomain == "" {
		return false
	}
	if strings.EqualFold(fromDomain, otherDomain) {
		return true
	}
	if mode == dmarc.AlignmentStrict {
		return false
	}

	fromOrg, err1 := publicsuffix.EffectiveTLDPlusOne(strings.ToLower(fromDomain))
	otherOrg, err2 := publicsuffix.EffectiveTLDPlusOne(strings.ToLower(otherDomain))
	if err1 != nil || err2 != nil {
		return false
	}
	return strings.EqualFold(fromOrg, otherOrg)
}

// extractFromDomain returns the domain part of the first RFC5322.From
// address, which is what DMARC evaluates alignment against (distinct from
// the SMTP envelope's MAIL FROM, which SPF checks).
func extractFromDomain(rawMessage []byte) string {
	msg, err := mail.ReadMessage(bytes.NewReader(rawMessage))
	if err != nil {
		return ""
	}
	fromHeader := msg.Header.Get("From")
	if fromHeader == "" {
		return ""
	}
	addrs, err := mail.ParseAddressList(fromHeader)
	if err != nil || len(addrs) == 0 {
		return ""
	}
	_, domain, ok := address.Split(addrs[0].Address)
	if !ok {
		return ""
	}
	return domain
}

func boundedLookupTXT(ctx context.Context) func(domain string) ([]string, error) {
	return func(domain string) ([]string, error) {
		return net.DefaultResolver.LookupTXT(ctx, domain)
	}
}
