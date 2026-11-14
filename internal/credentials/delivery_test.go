package credentials_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/keyset"

	"github.com/solidDoWant/toolgate/internal/credentials"
	"github.com/solidDoWant/toolgate/internal/secrets"
)

const exchangedToken = "eyJexchanged-token-text-0123456789"

func newCipher(t *testing.T) *secrets.Cipher {
	t.Helper()

	handle, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	require.NoError(t, err)

	cipher, err := secrets.NewCipher(handle)
	require.NoError(t, err)

	return cipher
}

func newDelivery() credentials.Delivery {
	issuedAt := time.Date(2026, 11, 13, 12, 0, 0, 123456789, time.UTC)

	return credentials.Delivery{
		Instance:  "inst-1",
		Exchange:  "temporal",
		Principal: "user:alice",
		IssuedAt:  issuedAt,
		ExpiresAt: issuedAt.Add(time.Hour),
		Token:     credentials.Token(exchangedToken),
	}
}

type recordingNotifier struct {
	payloads []string
}

func (n *recordingNotifier) NotifyCredentials(_ context.Context, payload string) error {
	n.payloads = append(n.payloads, payload)

	return nil
}

func TestDeliveryPayloadHoldsNoToken(t *testing.T) {
	cipher := newCipher(t)
	delivery := newDelivery()

	notifier := &recordingNotifier{}
	require.NoError(t, credentials.Send(t.Context(), notifier, cipher, delivery))
	require.Len(t, notifier.payloads, 1)

	payload := notifier.payloads[0]
	for _, encoding := range []string{
		exchangedToken,
		base64.StdEncoding.EncodeToString([]byte(exchangedToken)),
		hex.EncodeToString([]byte(exchangedToken)),
	} {
		assert.NotContains(t, payload, encoding)
	}

	var fields map[string]any
	require.NoError(t, json.Unmarshal([]byte(payload), &fields))
	assert.ElementsMatch(t,
		[]string{"v", "instance", "exchange", "principal", "issuedAt", "expiresAt", "ciphertext"},
		keys(fields))
	assert.InDelta(t, 1, fields["v"], 0)
	assert.Equal(t, "inst-1", fields["instance"])
	assert.Equal(t, "temporal", fields["exchange"])
	assert.Equal(t, "user:alice", fields["principal"])
	assert.Equal(t, "2026-11-13T12:00:00.123456789Z", fields["issuedAt"])
	assert.Equal(t, "2026-11-13T13:00:00.123456789Z", fields["expiresAt"])

	opened, err := credentials.Open(cipher, payload)
	require.NoError(t, err)
	assert.Equal(t, exchangedToken, string(opened.Token))
	assert.Equal(t, delivery.Instance, opened.Instance)
	assert.Equal(t, delivery.Exchange, opened.Exchange)
	assert.Equal(t, delivery.Principal, opened.Principal)
	assert.True(t, delivery.IssuedAt.Equal(opened.IssuedAt))
	assert.True(t, delivery.ExpiresAt.Equal(opened.ExpiresAt))

	t.Run("the delivery prints and logs no token", func(t *testing.T) {
		var logs bytes.Buffer
		slog.New(slog.NewJSONHandler(&logs, nil)).Info("delivery", "delivery", opened, "token", opened.Token)

		encoded, err := json.Marshal(opened)
		require.NoError(t, err)

		for _, text := range []string{
			fmt.Sprintf("%v", opened),
			fmt.Sprintf("%+v", opened),
			fmt.Sprintf("%#v", opened),
			fmt.Sprint(opened.Token),
			logs.String(),
			string(encoded),
		} {
			assert.NotContains(t, text, exchangedToken)
		}
	})
}

func keys(fields map[string]any) []string {
	result := make([]string, 0, len(fields))
	for key := range fields {
		result = append(result, key)
	}

	return result
}

func TestDeliveryRejectsChangedPayload(t *testing.T) {
	cipher := newCipher(t)

	payload, err := credentials.Seal(cipher, newDelivery())
	require.NoError(t, err)

	for _, test := range []struct {
		field string
		value any
	}{
		{field: "instance", value: "inst-2"},
		{field: "exchange", value: "other"},
		{field: "principal", value: "user:mallory"},
		{field: "issuedAt", value: "2026-11-13T12:00:01.123456789Z"},
		{field: "expiresAt", value: "2026-11-13T14:00:00.123456789Z"},
		{field: "v", value: 2},
	} {
		t.Run(test.field, func(t *testing.T) {
			var fields map[string]any
			require.NoError(t, json.Unmarshal([]byte(payload), &fields))

			fields[test.field] = test.value
			changed, err := json.Marshal(fields)
			require.NoError(t, err)

			_, err = credentials.Open(cipher, string(changed))
			require.Error(t, err)
			assert.NotContains(t, err.Error(), exchangedToken)
		})
	}

	t.Run("another keyset", func(t *testing.T) {
		_, err := credentials.Open(newCipher(t), payload)
		require.Error(t, err)
	})

	t.Run("unchanged", func(t *testing.T) {
		_, err := credentials.Open(cipher, payload)
		require.NoError(t, err)
	})
}

func TestDeliveryRefusesPayloadOver7KiB(t *testing.T) {
	cipher := newCipher(t)

	delivery := newDelivery()
	delivery.Token = credentials.Token(strings.Repeat("a", 4<<10))
	payload, err := credentials.Seal(cipher, delivery)
	require.NoError(t, err, "a 4 KiB token fits")
	assert.LessOrEqual(t, len(payload), 7<<10)

	delivery.Token = credentials.Token(strings.Repeat("a", 6<<10))
	notifier := &recordingNotifier{}
	err = credentials.Send(t.Context(), notifier, cipher, delivery)
	require.ErrorIs(t, err, credentials.ErrPayloadTooLarge)
	assert.Empty(t, notifier.payloads)
}
