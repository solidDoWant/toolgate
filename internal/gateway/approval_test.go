package gateway_test

import (
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/gateway"
)

func TestCanonicalArguments(t *testing.T) {
	tests := []struct {
		name        string
		arguments   string
		want        string
		assertError require.ErrorAssertionFunc
	}{
		{name: "keys are sorted", arguments: `{"b": 1, "a": {"d": true, "c": null}}`, want: `{"a":{"c":null,"d":true},"b":1}`},
		{name: "numbers keep their form", arguments: `{"n": 1.50, "big": 12345678901234567890, "e": 1e3}`, want: `{"big":12345678901234567890,"e":1e3,"n":1.50}`},
		{name: "the last duplicate key wins", arguments: `{"a": 1, "a": 2}`, want: `{"a":2}`},
		{name: "invalid UTF-8 is replaced", arguments: "{\"a\": \"\xff\"}", want: `{"a":"�"}`},
		{name: "empty arguments stay empty", arguments: ``, want: ``},
		{name: "invalid JSON", arguments: `{"a":`, assertError: require.Error},
		{name: "two values", arguments: `{} {}`, assertError: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertError := test.assertError
			if assertError == nil {
				assertError = require.NoError
			}

			canonical, hash, err := gateway.CanonicalArguments(json.RawMessage(test.arguments))
			assertError(t, err)

			if err != nil {
				return
			}

			assert.Equal(t, test.want, string(canonical))

			sum := sha256.Sum256([]byte(test.want))
			assert.Equal(t, sum[:], hash)
		})
	}
}

func TestCanonicalArgumentsHashIgnoresKeyOrderAndSpace(t *testing.T) {
	_, first, err := gateway.CanonicalArguments(json.RawMessage(`{"a":1,"b":2}`))
	require.NoError(t, err)

	_, second, err := gateway.CanonicalArguments(json.RawMessage("{ \"b\": 2,\n \"a\": 1 }"))
	require.NoError(t, err)

	_, other, err := gateway.CanonicalArguments(json.RawMessage(`{"a":1,"b":3}`))
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.NotEqual(t, first, other)
}

func TestDurationWords(t *testing.T) {
	for duration, want := range map[time.Duration]string{
		time.Minute:      "1 minute",
		10 * time.Minute: "10 minutes",
		time.Hour:        "1 hour",
		90 * time.Minute: "90 minutes",
	} {
		assert.Equal(t, want, gateway.DurationWords(duration))
	}
}

func TestApprovalWaitersAllowThirtyTwoWaitingCallsPerPrincipal(t *testing.T) {
	var waiters gateway.ApprovalWaiters

	for range 32 {
		require.True(t, waiters.Acquire("alice"))
	}

	assert.False(t, waiters.Acquire("alice"), "the 33rd waiting call is refused")
	assert.True(t, waiters.Acquire("bob"), "another principal has its own limit")

	waiters.Release("alice")
	assert.True(t, waiters.Acquire("alice"), "a released slot can be taken again")
	assert.Equal(t, 32, gateway.WaitingCallLimit)
}
