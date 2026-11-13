//go:build !windows

package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/client/attestation"
	"github.com/solidDoWant/toolgate/internal/client/login"
)

// writeKeyFile writes a PKCS #8 PEM file for key with the given mode.
func writeKeyFile(t *testing.T, key any, mode os.FileMode) string {
	t.Helper()

	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "workspace.key")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600))
	require.NoError(t, os.Chmod(path, mode))

	return path
}

func newWorkspaceKey(t *testing.T) (ed25519.PublicKey, string) {
	t.Helper()

	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	return public, writeKeyFile(t, private, 0o600)
}

// expectedKeyID is the RFC 7638 thumbprint, computed here from the JWK
// members for an OKP key rather than through the code under test.
func expectedKeyID(public ed25519.PublicKey) string {
	members := `{"crv":"Ed25519","kty":"OKP","x":"` + base64.RawURLEncoding.EncodeToString(public) + `"}`
	sum := crypto.SHA256.New()
	_, _ = sum.Write([]byte(members))

	return base64.RawURLEncoding.EncodeToString(sum.Sum(nil))
}

// keySourceEnvironment is the helper's environment with a user token, so
// that only the attestation decides the outcome.
func keySourceEnvironment(t *testing.T) string {
	t.Helper()

	dir := pluginHelperEnvironment(t)
	t.Setenv("TOOLGATE_TOKEN_COMMAND", "echo user-token")

	return dir
}

func TestWorkspaceKeySource(t *testing.T) {
	t.Run("assertion header and claims", func(t *testing.T) {
		keySourceEnvironment(t)
		public, path := newWorkspaceKey(t)
		t.Setenv(attestation.SignerFileEnv, path)

		before := time.Now().Truncate(time.Second)
		code, stdout, stderr := runCLI(t, "", "headers")
		require.Equal(t, 0, code, stderr)

		token := headerValues(t, stdout)["X-Toolgate-Attestation"]
		require.NotEmpty(t, token)

		parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.EdDSA})
		require.NoError(t, err)
		require.Len(t, parsed.Headers, 1)

		header := parsed.Headers[0]
		assert.Equal(t, "EdDSA", header.Algorithm)
		assert.Equal(t, "toolgate-workspace-key+jwt", header.ExtraHeaders[jose.HeaderType])
		assert.Equal(t, expectedKeyID(public), header.KeyID)

		var claims jwt.Claims

		var raw map[string]any

		require.NoError(t, parsed.Claims(public, &claims, &raw), "signed by the key in the file")
		assert.ElementsMatch(t, []string{"aud", "iat", "exp", "jti"}, mapKeys(raw), "no other claims")
		assert.Equal(t, "toolgate", raw["aud"])
		require.NotNil(t, claims.IssuedAt)
		require.NotNil(t, claims.Expiry)
		assert.WithinRange(t, claims.IssuedAt.Time(), before, time.Now())
		assert.Equal(t, 300*time.Second, claims.Expiry.Time().Sub(claims.IssuedAt.Time()))

		nonce, err := base64.RawURLEncoding.DecodeString(claims.ID)
		require.NoError(t, err)
		assert.Len(t, nonce, 16)

		code, stdout, stderr = runCLI(t, "", "headers")
		require.Equal(t, 0, code, stderr)

		second, err := jwt.ParseSigned(headerValues(t, stdout)["X-Toolgate-Attestation"], []jose.SignatureAlgorithm{jose.EdDSA})
		require.NoError(t, err)

		var secondClaims jwt.Claims
		require.NoError(t, second.Claims(public, &secondClaims))
		assert.NotEqual(t, claims.ID, secondClaims.ID, "each run makes a new jti")
	})

	t.Run("conflict with each other source", func(t *testing.T) {
		for _, other := range []string{"TOOLGATE_ATTESTATION_TOKEN_FILE", "TOOLGATE_ATTESTATION_SPIFFE_SOCKET", "TOOLGATE_ATTESTATION_TOKEN_REQUEST"} {
			t.Run(other, func(t *testing.T) {
				keySourceEnvironment(t)
				_, path := newWorkspaceKey(t)
				t.Setenv(attestation.SignerFileEnv, path)
				t.Setenv(other, "/somewhere")

				code, stdout, stderr := runCLI(t, "", "headers")
				assert.NotEqual(t, 0, code)
				assert.Empty(t, stdout, "no headers")
				assert.Contains(t, stderr, "Set only one of TOOLGATE_ATTESTATION_TOKEN_FILE, TOOLGATE_ATTESTATION_SPIFFE_SOCKET, TOOLGATE_ATTESTATION_TOKEN_REQUEST and TOOLGATE_ATTESTATION_SIGNER_FILE.")
			})
		}
	})

	t.Run("group-readable key file is refused", func(t *testing.T) {
		keySourceEnvironment(t)

		_, private, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)

		path := writeKeyFile(t, private, 0o640)
		t.Setenv(attestation.SignerFileEnv, path)

		code, stdout, stderr := runCLI(t, "", "headers")
		assert.NotEqual(t, 0, code)
		assert.Empty(t, stdout, "no headers")
		assert.Contains(t, stderr, "The key file "+path+" can be read by other users. Run chmod 600 "+path+".")
	})

	t.Run("file that is not an Ed25519 key is refused", func(t *testing.T) {
		ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)

		notPEM := filepath.Join(t.TempDir(), "not-pem.key")
		require.NoError(t, os.WriteFile(notPEM, []byte("hello"), 0o600))

		for name, path := range map[string]string{
			"ECDSA key":  writeKeyFile(t, ecKey, 0o600),
			"not PEM":    notPEM,
			"empty file": writeEmpty(t),
		} {
			t.Run(name, func(t *testing.T) {
				keySourceEnvironment(t)
				t.Setenv(attestation.SignerFileEnv, path)

				code, stdout, stderr := runCLI(t, "", "headers")
				assert.NotEqual(t, 0, code)
				assert.Empty(t, stdout, "no headers")
				assert.Contains(t, stderr, "The key file "+path+" doesn't hold an Ed25519 private key.")
			})
		}
	})

	t.Run("environment variable wins over config.json", func(t *testing.T) {
		keySourceEnvironment(t)
		_, configured := newWorkspaceKey(t)
		fromEnv, envPath := newWorkspaceKey(t)

		code, _, stderr := runCLI(t, "", "configure", "--token-command", "echo user-token", "--attestation-key-file", configured)
		require.Equal(t, 0, code, stderr)

		t.Setenv(attestation.SignerFileEnv, envPath)

		code, stdout, stderr := runCLI(t, "", "headers")
		require.Equal(t, 0, code, stderr)

		parsed, err := jwt.ParseSigned(headerValues(t, stdout)["X-Toolgate-Attestation"], []jose.SignatureAlgorithm{jose.EdDSA})
		require.NoError(t, err)
		assert.Equal(t, expectedKeyID(fromEnv), parsed.Headers[0].KeyID)
	})
}

func writeEmpty(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "empty.key")
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	return path
}

func mapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}

	return keys
}

func TestConfigureAttestationKeyFile(t *testing.T) {
	t.Run("saves the absolute path and prints the fingerprint", func(t *testing.T) {
		dir := pluginHelperEnvironment(t)
		public, path := newWorkspaceKey(t)

		t.Chdir(filepath.Dir(path))

		code, stdout, stderr := runCLI(t, "", "configure", "--token-command", "echo user-token", "--attestation-key-file", filepath.Base(path))
		require.Equal(t, 0, code, stderr)
		assert.Equal(t, "Saved the settings in "+dir+". The key's fingerprint is SHA256:"+expectedKeyID(public)+".\n", stdout)

		settings, err := login.LoadSettings(dir)
		require.NoError(t, err)

		resolved, err := filepath.EvalSymlinks(path)
		require.NoError(t, err)
		saved, err := filepath.EvalSymlinks(settings.AttestationKeyFile)
		require.NoError(t, err)
		assert.True(t, filepath.IsAbs(settings.AttestationKeyFile))
		assert.Equal(t, resolved, saved)

		// Without the variable, the helper signs with the configured key.
		code, stdout, stderr = runCLI(t, "", "headers")
		require.Equal(t, 0, code, stderr)

		parsed, err := jwt.ParseSigned(headerValues(t, stdout)["X-Toolgate-Attestation"], []jose.SignatureAlgorithm{jose.EdDSA})
		require.NoError(t, err)
		assert.Equal(t, expectedKeyID(public), parsed.Headers[0].KeyID)
	})

	t.Run("refuses a key file it can't load", func(t *testing.T) {
		dir := pluginHelperEnvironment(t)

		_, private, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)

		path := writeKeyFile(t, private, 0o644)

		code, _, stderr := runCLI(t, "", "configure", "--attestation-key-file", path)
		assert.NotEqual(t, 0, code)
		assert.Contains(t, stderr, "can be read by other users")
		assert.NoFileExists(t, filepath.Join(dir, login.SettingsFileName))
	})

	t.Run("refuses a second source", func(t *testing.T) {
		for _, other := range []string{"--attestation-token-file", "--attestation-token-request"} {
			t.Run(other, func(t *testing.T) {
				dir := pluginHelperEnvironment(t)
				_, path := newWorkspaceKey(t)

				code, _, stderr := runCLI(t, "", "configure", other, "/run/token")
				require.Equal(t, 0, code, stderr)

				code, _, stderr = runCLI(t, "", "configure", "--attestation-key-file", path)
				assert.Equal(t, 2, code)
				assert.Contains(t, stderr, "set only one of --attestation-token-file, --attestation-token-request and --attestation-key-file")

				settings, err := login.LoadSettings(dir)
				require.NoError(t, err)
				assert.Empty(t, settings.AttestationKeyFile)

				// The user removes the other source in the same command.
				code, _, stderr = runCLI(t, "", "configure", other, "", "--attestation-key-file", path)
				require.Equal(t, 0, code, stderr)
			})
		}
	})

	t.Run("removes the setting with an empty value", func(t *testing.T) {
		dir := pluginHelperEnvironment(t)
		_, path := newWorkspaceKey(t)

		code, _, stderr := runCLI(t, "", "configure", "--token-command", "echo user-token", "--attestation-key-file", path)
		require.Equal(t, 0, code, stderr)

		code, stdout, stderr := runCLI(t, "", "configure", "--attestation-key-file", "")
		require.Equal(t, 0, code, stderr)
		assert.Equal(t, "Saved the settings in "+dir+".\n", stdout)

		settings, err := login.LoadSettings(dir)
		require.NoError(t, err)
		assert.Empty(t, settings.AttestationKeyFile)
		assert.Equal(t, "echo user-token", settings.TokenCommand)
	})
}
