// Package dkimsign signs outbound mail with DKIM (RFC 6376) using
// emersion/go-msgauth/dkim, and manages the RSA key pair used to do so.
package dkimsign

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"github.com/emersion/go-msgauth/dkim"
)

// LoadOrGenerateKey loads a PKCS#1 RSA private key from path, generating a
// new 2048-bit key and writing it there (mode 0600) if none exists yet.
// generated reports whether a new key was created.
func LoadOrGenerateKey(path string) (key *rsa.PrivateKey, generated bool, err error) {
	buf, err := os.ReadFile(path)
	if err == nil {
		block, _ := pem.Decode(buf)
		if block == nil {
			return nil, false, fmt.Errorf("no PEM data found in %s", path)
		}
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, false, fmt.Errorf("parsing DKIM private key: %w", err)
		}
		return key, false, nil
	}
	if !os.IsNotExist(err) {
		return nil, false, err
	}

	key, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, false, fmt.Errorf("generating DKIM key: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, false, err
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		return nil, false, fmt.Errorf("writing DKIM private key: %w", err)
	}

	return key, true, nil
}

// DNSRecord returns the TXT record value to publish at
// "<selector>._domainkey.<domain>" for key.
func DNSRecord(key *rsa.PrivateKey) (string, error) {
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("v=DKIM1; k=rsa; p=%s", base64.StdEncoding.EncodeToString(pubDER)), nil
}

// Signer returns a function that DKIM-signs a raw RFC 822 message (a
// DKIM-Signature header is prepended; the rest of the message is
// unchanged), suitable for use as smtpserver.SubmissionBackend.Sign.
//
// The signature's "d=" domain is passed in per message rather than fixed at
// construction time, since it must match the sending domain (the
// authenticated user's From address domain) for DMARC alignment, which can
// differ from the server's own hostname; the same key/selector is used to
// sign for every locally configured domain.
func Signer(selector string, key *rsa.PrivateKey) func(domain string, data []byte) ([]byte, error) {
	return func(domain string, data []byte) ([]byte, error) {
		options := &dkim.SignOptions{
			Domain:                 domain,
			Selector:               selector,
			Signer:                 key,
			Hash:                   crypto.SHA256,
			HeaderCanonicalization: dkim.CanonicalizationRelaxed,
			BodyCanonicalization:   dkim.CanonicalizationRelaxed,
		}

		var buf bytes.Buffer
		if err := dkim.Sign(&buf, bytes.NewReader(data), options); err != nil {
			return nil, fmt.Errorf("signing message: %w", err)
		}
		return buf.Bytes(), nil
	}
}
