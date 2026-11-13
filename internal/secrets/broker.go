package secrets

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// MaxValueBytes bounds a secret value: the gateway's 4 MiB request body
// limit.
const MaxValueBytes = 4 << 20

// rewrapBatchSize is how many rows Rewrap reads at a time.
const rewrapBatchSize = 100

// namePattern is the form of a secret name. It is the form of the
// `X-Toolgate-*` header values, so a name is safe in paths and messages.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]{0,127}$`)

// Errors returned by the Broker.
var (
	ErrNotFound     = errors.New("secret not found")
	ErrInvalidName  = errors.New("a secret name must match ^[A-Za-z0-9._][A-Za-z0-9._-]{0,127}$")
	ErrValueTooLong = fmt.Errorf("a secret value can be at most %d bytes", MaxValueBytes)
)

// Store holds the encrypted rows. *db.Queries implements it.
type Store interface {
	PutUserSecret(ctx context.Context, arg db.PutUserSecretParams) (db.UserSecret, error)
	GetUserSecret(ctx context.Context, arg db.GetUserSecretParams) (db.UserSecret, error)
	ListUserSecretNames(ctx context.Context, principal string) ([]db.ListUserSecretNamesRow, error)
	DeleteUserSecret(ctx context.Context, arg db.DeleteUserSecretParams) (int64, error)
	ListUserSecretsNotUnderKey(ctx context.Context, arg db.ListUserSecretsNotUnderKeyParams) ([]db.UserSecret, error)
	RewrapUserSecret(ctx context.Context, arg db.RewrapUserSecretParams) (int64, error)
}

// Info describes a secret without its value.
type Info struct {
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Broker stores, lists, reads and deletes the secrets of each principal.
// Every operation is scoped to the principal it is given, which must be a
// verified principal.
type Broker struct {
	store  Store
	cipher *Cipher
}

// NewBroker returns a Broker keeping rows in store, encrypted with cipher.
func NewBroker(store Store, cipher *Cipher) *Broker {
	return &Broker{store: store, cipher: cipher}
}

// ValidName reports whether name is a valid secret name.
func ValidName(name string) bool {
	return namePattern.MatchString(name)
}

// Set stores value as the secret name of principal, replacing any value it
// had.
func (b *Broker) Set(ctx context.Context, principal, name string, value []byte) (Info, error) {
	if !ValidName(name) {
		return Info{}, ErrInvalidName
	}

	if len(value) > MaxValueBytes {
		return Info{}, ErrValueTooLong
	}

	ciphertext, err := b.cipher.Encrypt(principal, name, value)
	if err != nil {
		return Info{}, err
	}

	row, err := b.store.PutUserSecret(ctx, db.PutUserSecretParams{
		Principal:  principal,
		Name:       name,
		Ciphertext: ciphertext,
		KeyID:      int64(b.cipher.PrimaryKeyID()),
	})
	if err != nil {
		return Info{}, fmt.Errorf("store secret: %w", err)
	}

	return Info{Name: row.Name, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}, nil
}

// Get returns the value of the secret name of principal, and when it was
// last set. It is for rendering that principal's instances only; no API
// returns a value.
func (b *Broker) Get(ctx context.Context, principal, name string) ([]byte, time.Time, error) {
	if !ValidName(name) {
		return nil, time.Time{}, ErrNotFound
	}

	row, err := b.store.GetUserSecret(ctx, db.GetUserSecretParams{Principal: principal, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, time.Time{}, ErrNotFound
	}

	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read secret: %w", err)
	}

	value, err := b.cipher.Decrypt(principal, name, row.Ciphertext)
	if err != nil {
		return nil, time.Time{}, err
	}

	return value, row.UpdatedAt.Time, nil
}

// List returns the names and dates of the secrets of principal, in name
// order.
func (b *Broker) List(ctx context.Context, principal string) ([]Info, error) {
	rows, err := b.store.ListUserSecretNames(ctx, principal)
	if err != nil {
		return nil, fmt.Errorf("list secrets: %w", err)
	}

	infos := make([]Info, 0, len(rows))
	for _, row := range rows {
		infos = append(infos, Info{Name: row.Name, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time})
	}

	return infos, nil
}

// Delete removes the secret name of principal. It returns ErrNotFound when
// principal has no such secret.
func (b *Broker) Delete(ctx context.Context, principal, name string) error {
	if !ValidName(name) {
		return ErrNotFound
	}

	deleted, err := b.store.DeleteUserSecret(ctx, db.DeleteUserSecretParams{Principal: principal, Name: name})
	if err != nil {
		return fmt.Errorf("delete secret: %w", err)
	}

	if deleted == 0 {
		return ErrNotFound
	}

	return nil
}

// Rewrap re-encrypts every row not under the keyset's primary key with the
// primary key, and returns how many rows it re-encrypted. Afterwards the
// keyset's other keys can be removed. A row set again while Rewrap runs is
// read again; a row the keyset can't decrypt stops Rewrap with an
// error naming its principal and name.
func (b *Broker) Rewrap(ctx context.Context) (int, error) {
	primary := int64(b.cipher.PrimaryKeyID())
	rewrapped := 0

	for {
		rows, err := b.store.ListUserSecretsNotUnderKey(ctx, db.ListUserSecretsNotUnderKeyParams{
			KeyID:     primary,
			BatchSize: rewrapBatchSize,
		})
		if err != nil {
			return rewrapped, fmt.Errorf("rewrap: list secrets: %w", err)
		}

		if len(rows) == 0 {
			return rewrapped, nil
		}

		progress := false

		for _, row := range rows {
			value, err := b.cipher.Decrypt(row.Principal, row.Name, row.Ciphertext)
			if err != nil {
				return rewrapped, fmt.Errorf("rewrap: secret %q of %q: %w", row.Name, row.Principal, err)
			}

			ciphertext, err := b.cipher.Encrypt(row.Principal, row.Name, value)
			if err != nil {
				return rewrapped, fmt.Errorf("rewrap: %w", err)
			}

			updated, err := b.store.RewrapUserSecret(ctx, db.RewrapUserSecretParams{
				NewCiphertext: ciphertext,
				NewKeyID:      primary,
				Principal:     row.Principal,
				Name:          row.Name,
				OldCiphertext: row.Ciphertext,
			})
			if err != nil {
				return rewrapped, fmt.Errorf("rewrap: store secret: %w", err)
			}

			// A row set again since it was read is skipped here; if a gateway
			// still writes under an old key, the next batch reads it again.
			if updated > 0 {
				progress = true
				rewrapped += int(updated)
			}
		}

		if !progress {
			return rewrapped, errors.New("rewrap: secrets keep changing under an old key; restart every gateway with the new keyset first")
		}
	}
}
