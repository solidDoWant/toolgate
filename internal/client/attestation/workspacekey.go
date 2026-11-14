package attestation

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	// MaxKeyFileBytes bounds how much of a workspace key file is read.
	MaxKeyFileBytes = 16 << 10

	// AssertionType is the typ header of a workspace key assertion.
	AssertionType = "toolgate-workspace-key+jwt"

	// AssertionLifetime is the lifetime of a workspace key assertion. The
	// gateway refuses longer ones.
	AssertionLifetime = 300 * time.Second

	// FingerprintPrefix comes before the key ID in the fingerprint people
	// compare.
	FingerprintPrefix = "SHA256:"

	jtiBytes = 16
)

// LoadKeyFile reads a workspace key: one PEM PRIVATE KEY block holding a
// PKCS #8 Ed25519 key, which other users can't read. The errors are the
// sentences the user sees.
func LoadKeyFile(path string) (ed25519.PrivateKey, error) {
	file, err := os.Open(path) //nolint:gosec // the user names the path
	if err != nil {
		return nil, fmt.Errorf("read the key file: %w", err)
	}

	defer func() { _ = file.Close() }()

	// The mode is checked on the open file, so it is the file that is read.
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("read the key file: %w", err)
	}

	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("The key file %s can be read by other users. Run chmod 600 %s.", path, path) //nolint:staticcheck // user-facing sentence
	}

	content, err := io.ReadAll(io.LimitReader(file, MaxKeyFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read the key file: %w", err)
	}

	notEd25519 := fmt.Errorf("The key file %s doesn't hold an Ed25519 private key.", path) //nolint:staticcheck // user-facing sentence

	if len(content) > MaxKeyFileBytes {
		return nil, notEd25519
	}

	block, rest := pem.Decode(content)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, notEd25519
	}

	if next, _ := pem.Decode(rest); next != nil {
		return nil, notEd25519
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, notEd25519
	}

	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, notEd25519
	}

	return key, nil
}

// KeyID returns the RFC 7638 thumbprint of a public key, base64url without
// padding. The gateway finds the registered key by it.
func KeyID(public crypto.PublicKey) (string, error) {
	thumbprint, err := (&jose.JSONWebKey{Key: public}).Thumbprint(crypto.SHA256)
	if err != nil {
		return "", fmt.Errorf("compute the key ID: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(thumbprint), nil
}

// Fingerprint returns the fingerprint people compare: SHA256: and the key ID.
func Fingerprint(keyID string) string {
	return FingerprintPrefix + keyID
}

// signAssertion loads the key file and signs a fresh assertion issued at now.
func signAssertion(path string, now time.Time) (string, error) {
	key, err := LoadKeyFile(path)
	if err != nil {
		return "", err
	}

	keyID, err := KeyID(key.Public())
	if err != nil {
		return "", err
	}

	nonce := make([]byte, jtiBytes)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("make the assertion ID: %w", err)
	}

	options := (&jose.SignerOptions{}).WithType(AssertionType).WithHeader(jose.HeaderKey("kid"), keyID)

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: key}, options)
	if err != nil {
		return "", fmt.Errorf("make the assertion signer: %w", err)
	}

	issued := now.Truncate(time.Second)

	token, err := jwt.Signed(signer).Claims(jwt.Claims{
		Audience: jwt.Audience{Audience},
		IssuedAt: jwt.NewNumericDate(issued),
		Expiry:   jwt.NewNumericDate(issued.Add(AssertionLifetime)),
		ID:       base64.RawURLEncoding.EncodeToString(nonce),
	}).Serialize()
	if err != nil {
		return "", errors.New("sign the workspace key assertion")
	}

	return token, nil
}
