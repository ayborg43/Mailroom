// Package inboundauth verifies SPF, DKIM, and DMARC on inbound mail and
// formats the result as an RFC 8601 Authentication-Results header. It only
// verifies (annotates); it never rejects mail based on the result — that's
// a policy decision left to whatever reads the header downstream (a mail
// client's junk filter, a future spam-scoring pass, etc.), matching the
// project's explicit "verification, not enforcement" scope for this phase.
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

// Verify runs SPF, DKIM, and DMARC checks on an inbound message and returns
// a formatted Authentication-Results header value (without the header name
// or trailing CRLF) using authServID — normally the receiving server's own
// hostname — as the reporting identity.
func Verify(authServID string, clientIP net.IP, helo, mailFrom string, rawMessage []byte) string {
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

	fromDomain := extractFromDomain(rawMessage)
	if dmarcResult := checkDMARC(fromDomain, spfResult, spfDomain, dkimResults); dmarcResult != nil {
		if dr, ok := dmarcResult.(*authres.DMARCResult); ok {
			metrics.InboundAuthResults.WithLabelValues("dmarc", string(dr.Value)).Inc()
		}
		results = append(results, dmarcResult)
	}

	return authres.Format(authServID, results)
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

func checkDMARC(fromDomain string, spfResult spf.Result, spfDomain string, dkimResults []authres.Result) authres.Result {
	if fromDomain == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), dnsTimeout)
	defer cancel()

	record, err := dmarc.LookupWithOptions(fromDomain, &dmarc.LookupOptions{LookupTXT: boundedLookupTXT(ctx)})
	if err != nil {
		if err == dmarc.ErrNoPolicy {
			return &authres.DMARCResult{Value: authres.ResultNone, From: fromDomain}
		}
		value := authres.ResultValue(authres.ResultPermError)
		if dmarc.IsTempFail(err) {
			value = authres.ResultTempError
		}
		return &authres.DMARCResult{Value: value, From: fromDomain, Reason: err.Error()}
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
		return &authres.DMARCResult{Value: authres.ResultPass, From: fromDomain}
	}
	return &authres.DMARCResult{Value: authres.ResultFail, From: fromDomain}
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
