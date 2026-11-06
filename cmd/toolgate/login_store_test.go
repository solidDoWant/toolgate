package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/solidDoWant/toolgate/internal/client/tokenstore"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

func TestLoginTokenStoreFileIsRecordedAndUsedAlone(t *testing.T) {
	dir := isolateUserConfig(t)
	t.Setenv("TOOLGATE_SESSION", "session-1")
	t.Setenv("TOOLGATE_WORKSPACE", "ws")

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	provider.AddUser(testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true})
	gatewayURL := startGateway(t, provider, provider.ClientID)

	code, output, stderr := signInApproving(t, provider, "alice", "--gateway", gatewayURL, "--token-store", "file")
	require.Equal(t, 0, code, "stderr: %s", stderr)

	lines := strings.Split(strings.TrimSpace(output), "\n")
	require.Len(t, lines, 6, output)
	assert.Regexp(t, `^To sign in, open http\S+\?\S+$`, lines[0])
	assert.Regexp(t, `^and check that it shows the code \S+$`, lines[1])
	assert.Equal(t, "The code expires in 5 minutes. You can open the link on any device.", lines[2])
	assert.Equal(t, "Signed in as alice@example.com (sub alice).", lines[3])
	assert.Equal(t, "The refresh token is stored in "+filepath.Join(dir, tokenstore.FileName)+".", lines[4])
	assert.Equal(t, "Sessions on this machine act as you until the IdP ends this sign-in or you run `toolgate logout`.", lines[5])

	settingsFile, err := os.ReadFile(filepath.Join(dir, "config.json"))
	require.NoError(t, err)
	assert.Contains(t, string(settingsFile), `"tokenStore": "file"`)

	// The keychain was not used, and a keychain token does not replace the file's.
	_, err = keyring.Get(tokenstore.KeychainService, tokenstore.KeychainUser)
	require.ErrorIs(t, err, keyring.ErrNotFound)
	require.NoError(t, keyring.Set(tokenstore.KeychainService, tokenstore.KeychainUser, "other"))

	var stdout, headersErr strings.Builder

	require.Equal(t, 0, run([]string{"headers"}, &stdout, &headersErr), "stderr: %s", headersErr.String())

	// With the file gone, the keychain token is not used either.
	require.NoError(t, os.Remove(filepath.Join(dir, tokenstore.FileName)))

	stdout.Reset()
	headersErr.Reset()
	assert.Equal(t, 1, run([]string{"headers"}, &stdout, &headersErr))
	assert.Empty(t, stdout.String())
	assert.Contains(t, headersErr.String(), "not signed in")
}

func TestLoginTokenStoreFromEnvironment(t *testing.T) {
	dir := isolateUserConfig(t)
	t.Setenv("TOOLGATE_TOKEN_STORE", "file")

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	provider.AddUser(testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true})
	gatewayURL := startGateway(t, provider, provider.ClientID)

	code, _, stderr := signInApproving(t, provider, "alice", "--gateway", gatewayURL)
	require.Equal(t, 0, code, "stderr: %s", stderr)
	assert.FileExists(t, filepath.Join(dir, tokenstore.FileName))
}

func TestLoginTokenStoreAutoRecordsTheStoreUsed(t *testing.T) {
	dir := isolateUserConfig(t)

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	provider.AddUser(testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true})
	gatewayURL := startGateway(t, provider, provider.ClientID)

	code, output, stderr := signInApproving(t, provider, "alice", "--gateway", gatewayURL)
	require.Equal(t, 0, code, "stderr: %s", stderr)
	assert.Contains(t, output, "The refresh token is stored in the OS keychain.\n")

	settingsFile, err := os.ReadFile(filepath.Join(dir, "config.json"))
	require.NoError(t, err)
	assert.Contains(t, string(settingsFile), `"tokenStore": "keychain"`)

	// Auto falls back to the file when the keychain fails.
	keyring.MockInitWithError(assert.AnError)

	code, output, stderr = signInApproving(t, provider, "alice", "--gateway", gatewayURL)
	require.Equal(t, 0, code, "stderr: %s", stderr)
	assert.Contains(t, output, "The refresh token is stored in "+filepath.Join(dir, tokenstore.FileName)+".")

	settingsFile, err = os.ReadFile(filepath.Join(dir, "config.json"))
	require.NoError(t, err)
	assert.Contains(t, string(settingsFile), `"tokenStore": "file"`)
}

func TestHeadersWithUnusableKeychainSaysToSignInWithFile(t *testing.T) {
	dir := isolateUserConfig(t)
	t.Setenv("TOOLGATE_SESSION", "session-1")
	t.Setenv("TOOLGATE_WORKSPACE", "ws")

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	provider.AddUser(testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true})
	gatewayURL := startGateway(t, provider, provider.ClientID)

	code, _, stderr := signInApproving(t, provider, "alice", "--gateway", gatewayURL, "--token-store", "keychain")
	require.Equal(t, 0, code, "stderr: %s", stderr)

	// A file token exists, but the recorded store is the keychain, which is now unusable.
	require.NoError(t, os.WriteFile(filepath.Join(dir, tokenstore.FileName), []byte("stale"), 0o600))
	keyring.MockInitWithError(assert.AnError)

	var stdout, headersErr strings.Builder

	assert.Equal(t, 1, run([]string{"headers"}, &stdout, &headersErr))
	assert.Empty(t, stdout.String())
	assert.Contains(t, headersErr.String(), "sign in again with `toolgate login --token-store file`")
}

func TestLoginRejectsUnknownTokenStore(t *testing.T) {
	isolateUserConfig(t)

	var stdout, stderr strings.Builder

	assert.Equal(t, 2, run([]string{"login", "--token-store", "vault"}, &stdout, &stderr))
	assert.Contains(t, stderr.String(), "unknown token store")
}

func TestHeadersWithoutTokenStoreKeepsKeychainThenFile(t *testing.T) {
	dir := isolateUserConfig(t)
	t.Setenv("TOOLGATE_SESSION", "session-1")
	t.Setenv("TOOLGATE_WORKSPACE", "ws")

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	provider.AddUser(testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true})
	gatewayURL := startGateway(t, provider, provider.ClientID)

	code, _, stderr := signInApproving(t, provider, "alice", "--gateway", gatewayURL)
	require.Equal(t, 0, code, "stderr: %s", stderr)

	// Make config.json look like it came from 0.2.
	settingsPath := filepath.Join(dir, "config.json")
	content, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	require.Contains(t, string(content), `"tokenStore": "keychain"`)

	var fields map[string]any
	require.NoError(t, json.Unmarshal(content, &fields))
	delete(fields, "tokenStore")

	oldContent, err := json.Marshal(fields)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(settingsPath, oldContent, 0o600))

	// Keychain first.
	var stdout, headersErr strings.Builder

	require.Equal(t, 0, run([]string{"headers"}, &stdout, &headersErr), "stderr: %s", headersErr.String())

	// Then the file, once the keychain is empty.
	token, err := tokenstore.New(dir).Load()
	require.NoError(t, err)
	require.NoError(t, keyring.Delete(tokenstore.KeychainService, tokenstore.KeychainUser))
	require.NoError(t, os.WriteFile(filepath.Join(dir, tokenstore.FileName), []byte(token), 0o600))

	stdout.Reset()
	headersErr.Reset()
	require.Equal(t, 0, run([]string{"headers"}, &stdout, &headersErr), "stderr: %s", headersErr.String())
}

func TestHeadersWithRevokedRefreshTokenSaysSignInAgain(t *testing.T) {
	isolateUserConfig(t)
	t.Setenv("TOOLGATE_SESSION", "session-1")
	t.Setenv("TOOLGATE_WORKSPACE", "ws")

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	provider.AddUser(testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true})
	gatewayURL := startGateway(t, provider, provider.ClientID)

	code, _, stderr := signInApproving(t, provider, "alice", "--gateway", gatewayURL)
	require.Equal(t, 0, code, "stderr: %s", stderr)

	provider.RevokeRefreshTokens("alice")

	for _, command := range []string{"headers", "otel-headers"} {
		var stdout, headersErr strings.Builder

		assert.Equal(t, 1, run([]string{command}, &stdout, &headersErr), command)
		assert.Empty(t, stdout.String(), command)
		assert.Equal(t, "Sign in again: run `toolgate login`.\n", headersErr.String(), command)
	}
}

func TestLoginExpiredCodeExitsOne(t *testing.T) {
	isolateUserConfig(t)

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{DeviceCodeLifetime: 2 * time.Second})
	gatewayURL := startGateway(t, provider, provider.ClientID)

	var stdout, stderr strings.Builder

	// Nobody approves the code.
	assert.Equal(t, 1, run([]string{"login", "--gateway", gatewayURL}, &stdout, &stderr))
	assert.Equal(t, "The sign-in code expired. Run `toolgate login` again.\n", stderr.String())
	assert.Contains(t, stdout.String(), "The code expires in 2 seconds.")
}
