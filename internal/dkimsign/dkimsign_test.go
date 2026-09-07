package dkimsign

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/emersion/go-msgauth/dkim"
)

func TestSignAndVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "private.pem")

	key, generated, err := LoadOrGenerateKey(keyPath)
	if err != nil {
		t.Fatalf("LoadOrGenerateKey: %v", err)
	}
	if !generated {
		t.Fatal("expected a fresh key to be generated")
	}

	record, err := DNSRecord(key)
	if err != nil {
		t.Fatalf("DNSRecord: %v", err)
	}

	sign := Signer("default", key)
	msg := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: hi\r\n\r\nHello there.\r\n")

	signed, err := sign("example.com", msg)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if !bytes.Contains(signed, []byte("DKIM-Signature:")) {
		t.Fatal("signed message missing DKIM-Signature header")
	}

	verifications, err := dkim.VerifyWithOptions(bytes.NewReader(signed), &dkim.VerifyOptions{
		LookupTXT: func(domain string) ([]string, error) {
			if domain != "default._domainkey.example.com" {
				t.Fatalf("unexpected DNS lookup for %q", domain)
			}
			return []string{record}, nil
		},
	})
	if err != nil {
		t.Fatalf("VerifyWithOptions: %v", err)
	}
	if len(verifications) != 1 {
		t.Fatalf("got %d verifications, want 1", len(verifications))
	}
	if verifications[0].Err != nil {
		t.Fatalf("signature failed verification: %v", verifications[0].Err)
	}
	if verifications[0].Domain != "example.com" {
		t.Errorf("verified domain = %q, want example.com", verifications[0].Domain)
	}

	// Loading the same key file a second time must not regenerate it.
	_, generatedAgain, err := LoadOrGenerateKey(keyPath)
	if err != nil {
		t.Fatalf("LoadOrGenerateKey (reload): %v", err)
	}
	if generatedAgain {
		t.Fatal("expected the existing key to be reused, not regenerated")
	}

	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key file missing on disk: %v", err)
	}
}

// TestSignTamperedBodyFailsVerification confirms a body altered after
// signing (e.g. by a buggy relay step) is correctly rejected.
func TestSignTamperedBodyFailsVerification(t *testing.T) {
	dir := t.TempDir()
	key, _, err := LoadOrGenerateKey(filepath.Join(dir, "private.pem"))
	if err != nil {
		t.Fatalf("LoadOrGenerateKey: %v", err)
	}
	record, err := DNSRecord(key)
	if err != nil {
		t.Fatalf("DNSRecord: %v", err)
	}

	sign := Signer("default", key)
	msg := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: hi\r\n\r\nOriginal body.\r\n")
	signed, err := sign("example.com", msg)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	tampered := bytes.Replace(signed, []byte("Original body."), []byte("Tampered body!!"), 1)

	verifications, err := dkim.VerifyWithOptions(bytes.NewReader(tampered), &dkim.VerifyOptions{
		LookupTXT: func(domain string) ([]string, error) { return []string{record}, nil },
	})
	if err != nil {
		t.Fatalf("VerifyWithOptions: %v", err)
	}
	if len(verifications) != 1 {
		t.Fatalf("got %d verifications, want 1", len(verifications))
	}
	if verifications[0].Err == nil {
		t.Fatal("expected verification to fail for tampered body, but it succeeded")
	}
}
