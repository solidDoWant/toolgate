// Package secrets stores user secrets (SPEC.md §11). Each value is
// envelope-encrypted with Tink: a fresh AES256-GCM data key encrypts the
// value, and the gateway's key-encryption keyset, read from a mounted
// Kubernetes Secret and never stored in PostgreSQL, encrypts the data key.
package secrets

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"
	"github.com/tink-crypto/tink-go/v2/tink"
)

// maxKeysetFileSize bounds the keyset file the gateway reads.
const maxKeysetFileSize = 1 << 20

// associatedDataPrefix starts the associated data of every user secret, so
// the ciphertexts can't be confused with anything else the keyset encrypts.
const associatedDataPrefix = "toolgate user secret v1\x00"

// Cipher encrypts and decrypts user secrets with one keyset.
type Cipher struct {
	envelope     *aead.KMSEnvelopeAEAD
	primaryKeyID uint32
}

// LoadKeyset reads a Tink keyset in JSON from path (the mounted Kubernetes
// Secret) and returns a Cipher using it.
func LoadKeyset(path string) (*Cipher, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("secrets keyset: %w", err)
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, maxKeysetFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("secrets keyset %s: %w", path, err)
	}

	if len(data) > maxKeysetFileSize {
		return nil, fmt.Errorf("secrets keyset %s: larger than %d bytes", path, maxKeysetFileSize)
	}

	// The keyset file is plain JSON because a Kubernetes Secret protects it;
	// Tink names that reader "insecure" for this reason.
	handle, err := insecurecleartextkeyset.Read(keyset.NewJSONReader(bytes.NewReader(data)))
	if err != nil {
		// Tink's errors don't contain key material.
		return nil, fmt.Errorf("secrets keyset %s: %w", path, err)
	}

	return NewCipher(handle)
}

// NewCipher returns a Cipher using handle, an AEAD keyset, as the
// key-encryption keyset.
func NewCipher(handle *keyset.Handle) (*Cipher, error) {
	keyEncryption, err := aead.New(handle)
	if err != nil {
		return nil, fmt.Errorf("secrets keyset: not an AEAD keyset: %w", err)
	}

	return newCipher(keyEncryption, handle.KeysetInfo().GetPrimaryKeyId()), nil
}

func newCipher(keyEncryption tink.AEAD, primaryKeyID uint32) *Cipher {
	return &Cipher{
		envelope:     aead.NewKMSEnvelopeAEAD2(aead.AES256GCMKeyTemplate(), keyEncryption),
		primaryKeyID: primaryKeyID,
	}
}

// PrimaryKeyID is the ID of the keyset's primary key, which encrypts every
// new value.
func (c *Cipher) PrimaryKeyID() uint32 {
	return c.primaryKeyID
}

// Encrypt encrypts value for the secret name of principal.
func (c *Cipher) Encrypt(principal, name string, value []byte) ([]byte, error) {
	ciphertext, err := c.envelope.Encrypt(value, associatedData(principal, name))
	if err != nil {
		return nil, fmt.Errorf("encrypt secret: %w", err)
	}

	return ciphertext, nil
}

// Decrypt decrypts the ciphertext of the secret name of principal. It fails
// for a ciphertext written for another principal or name.
func (c *Cipher) Decrypt(principal, name string, ciphertext []byte) ([]byte, error) {
	value, err := c.envelope.Decrypt(ciphertext, associatedData(principal, name))
	if err != nil {
		return nil, errors.New("decrypt secret: the keyset can't decrypt it")
	}

	return value, nil
}

// associatedData binds a ciphertext to its principal and name. Each part is
// length-prefixed, so no two (principal, name) pairs give the same bytes.
func associatedData(principal, name string) []byte {
	data := make([]byte, 0, len(associatedDataPrefix)+16+len(principal)+len(name))
	data = append(data, associatedDataPrefix...)
	data = binary.BigEndian.AppendUint64(data, uint64(len(principal)))
	data = append(data, principal...)
	data = binary.BigEndian.AppendUint64(data, uint64(len(name)))
	data = append(data, name...)

	return data
}
