// Package credentials carries exchanged tokens from the replica that ran an
// exchange to the leader, which writes them into instance Secrets.
package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/solidDoWant/toolgate/internal/secrets"
)

// deliveryVersion is the version of the delivery payload.
const deliveryVersion = 1

// MaxDeliveryPayloadSize is the largest payload sent on the credentials
// channel. PostgreSQL refuses notifications of 8000 bytes or more, and the
// 4 KiB token limit keeps every payload under this.
const MaxDeliveryPayloadSize = 7 << 10

// ErrPayloadTooLarge is returned for a delivery whose payload is over
// MaxDeliveryPayloadSize.
var ErrPayloadTooLarge = errors.New("credential delivery: the payload is over 7 KiB")

// errOpen is the only error Open returns for a payload it can't read, so no
// payload text or token reaches an error.
var errOpen = errors.New("credential delivery: the payload can't be opened")

// Token is an exchanged token. It prints, logs and marshals as a placeholder,
// so it can't reach a log, a span or an error by accident.
type Token []byte

const redacted = "[redacted]"

// String returns a placeholder, never the token.
func (Token) String() string { return redacted }

// GoString returns a placeholder, never the token.
func (Token) GoString() string { return redacted }

// LogValue returns a placeholder, never the token.
func (Token) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON returns a placeholder, never the token.
func (Token) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

// Delivery is an exchanged token for one exchange of one instance.
type Delivery struct {
	Instance  string
	Exchange  string
	Principal string
	IssuedAt  time.Time
	ExpiresAt time.Time
	Token     Token
}

// deliveryPayload is the JSON sent on the credentials channel. It holds the
// token only as ciphertext.
type deliveryPayload struct {
	Version    int    `json:"v"`
	Instance   string `json:"instance"`
	Exchange   string `json:"exchange"`
	Principal  string `json:"principal"`
	IssuedAt   string `json:"issuedAt"`
	ExpiresAt  string `json:"expiresAt"`
	Ciphertext []byte `json:"ciphertext"`
}

// secretName is the name the token is encrypted under. It binds the token to
// the instance, the exchange and both times; the principal is bound by the
// cipher. A user secret name can't contain "/", so it never equals one.
func (p deliveryPayload) secretName() string {
	return "credential/" + p.Instance + "/" + p.Exchange + "/" + p.IssuedAt + "/" + p.ExpiresAt
}

// Seal encrypts delivery's token and returns the payload to send. It refuses
// a payload over MaxDeliveryPayloadSize.
func Seal(cipher *secrets.Cipher, delivery Delivery) (string, error) {
	payload := deliveryPayload{
		Version:   deliveryVersion,
		Instance:  delivery.Instance,
		Exchange:  delivery.Exchange,
		Principal: delivery.Principal,
		IssuedAt:  delivery.IssuedAt.UTC().Format(time.RFC3339Nano),
		ExpiresAt: delivery.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}

	ciphertext, err := cipher.Encrypt(payload.Principal, payload.secretName(), delivery.Token)
	if err != nil {
		return "", fmt.Errorf("credential delivery: %w", err)
	}

	payload.Ciphertext = ciphertext

	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("credential delivery: %w", err)
	}

	if len(encoded) > MaxDeliveryPayloadSize {
		return "", ErrPayloadTooLarge
	}

	return string(encoded), nil
}

// Open decodes and decrypts a payload made by Seal. A payload with any bound
// field changed fails to open.
func Open(cipher *secrets.Cipher, encoded string) (Delivery, error) {
	if len(encoded) > MaxDeliveryPayloadSize {
		return Delivery{}, errOpen
	}

	decoder := json.NewDecoder(bytes.NewReader([]byte(encoded)))
	decoder.DisallowUnknownFields()

	var payload deliveryPayload
	if err := decoder.Decode(&payload); err != nil || payload.Version != deliveryVersion {
		return Delivery{}, errOpen
	}

	issuedAt, err := time.Parse(time.RFC3339Nano, payload.IssuedAt)
	if err != nil {
		return Delivery{}, errOpen
	}

	expiresAt, err := time.Parse(time.RFC3339Nano, payload.ExpiresAt)
	if err != nil {
		return Delivery{}, errOpen
	}

	token, err := cipher.Decrypt(payload.Principal, payload.secretName(), payload.Ciphertext)
	if err != nil {
		return Delivery{}, errOpen
	}

	return Delivery{
		Instance:  payload.Instance,
		Exchange:  payload.Exchange,
		Principal: payload.Principal,
		IssuedAt:  issuedAt,
		ExpiresAt: expiresAt,
		Token:     token,
	}, nil
}

// Notifier queues a payload on the credentials channel.
type Notifier interface {
	NotifyCredentials(ctx context.Context, payload string) error
}

// Send seals delivery and queues it for the leader. Nothing retries a lost
// delivery: the claim expires and a later request exchanges again.
func Send(ctx context.Context, notifier Notifier, cipher *secrets.Cipher, delivery Delivery) error {
	payload, err := Seal(cipher, delivery)
	if err != nil {
		return err
	}

	if err := notifier.NotifyCredentials(ctx, payload); err != nil {
		return fmt.Errorf("credential delivery: notify: %w", err)
	}

	return nil
}
