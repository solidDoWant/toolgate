package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/secrets"
)

func TestSecretCommandChecksItsInputBeforeCallingTheGateway(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		stdin      string
		wantCode   int
		wantStderr string
	}{
		{name: "no command", args: []string{"secret"}, wantCode: 2, wantStderr: "Usage: toolgate secret"},
		{name: "unknown command", args: []string{"secret", "show", "x"}, wantCode: 2, wantStderr: `unknown command "show"`},
		{name: "set without a name", args: []string{"secret", "set"}, wantCode: 2, wantStderr: "exactly one secret name"},
		{name: "set with an invalid name", args: []string{"secret", "set", "a/b"}, wantCode: 2, wantStderr: "^[A-Za-z0-9._-]{1,128}$"},
		{name: "set with a name of 129 characters", args: []string{"secret", "set", strings.Repeat("a", 129)}, wantCode: 2, wantStderr: "^[A-Za-z0-9._-]{1,128}$"},
		{name: "delete with two names", args: []string{"secret", "delete", "a", "b"}, wantCode: 2, wantStderr: "exactly one secret name"},
		{name: "list with an argument", args: []string{"secret", "list", "a"}, wantCode: 2, wantStderr: `unexpected argument "a"`},
		{
			name: "set with a value over the limit", args: []string{"secret", "set", "big"},
			stdin: strings.Repeat("x", secrets.MaxValueBytes+1), wantCode: 1, wantStderr: "longer than 4194304 bytes",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isolateUserConfig(t)

			code, stdout, stderr := runCLI(t, test.stdin, test.args...)

			require.Equal(t, test.wantCode, code, "stderr: %s", stderr)
			assert.Contains(t, stderr, test.wantStderr)
			assert.Empty(t, stdout)
		})
	}
}

func TestSecretSetAtTheLimitNeedsSignIn(t *testing.T) {
	isolateUserConfig(t)

	code, _, stderr := runCLI(t, strings.Repeat("x", secrets.MaxValueBytes), "secret", "set", strings.Repeat("a", 128))

	require.Equal(t, 1, code)
	assert.Contains(t, stderr, "not signed in. Run toolgate login")
}

func TestServersWithoutSignIn(t *testing.T) {
	isolateUserConfig(t)

	code, _, stderr := runCLI(t, "", "servers")

	require.Equal(t, 1, code)
	assert.Contains(t, stderr, "not signed in. Run toolgate login")
}

// runCLI runs one CLI command with stdin and returns the exit code and output.
func runCLI(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr strings.Builder

	code := runWithInput(args, strings.NewReader(stdin), &stdout, &stderr)

	return code, stdout.String(), stderr.String()
}
