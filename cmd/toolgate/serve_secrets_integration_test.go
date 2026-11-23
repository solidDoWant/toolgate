//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"

	"github.com/solidDoWant/toolgate/internal/testutil"
)

func TestServeMountsTheSecretsAPIBehindAuthentication(t *testing.T) {
	t.Parallel()

	env := testEnv{}

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})

	handle, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	require.NoError(t, err)

	var buffer bytes.Buffer
	require.NoError(t, insecurecleartextkeyset.Write(handle, keyset.NewJSONWriter(&buffer)))

	keysetPath := filepath.Join(t.TempDir(), "keyset.json")
	require.NoError(t, os.WriteFile(keysetPath, buffer.Bytes(), 0o600))

	env.set("TOOLGATE_LISTEN_ADDRESS", "127.0.0.1:0")
	env.set("TOOLGATE_METRICS_LISTEN_ADDRESS", "127.0.0.1:0")
	env.set("TOOLGATE_DATABASE_URL", testutil.StartPostgres(t).ConnString())
	env.set("TOOLGATE_OIDC_ISSUER_URL", provider.Issuer)
	env.set("TOOLGATE_OIDC_AUDIENCE", provider.Audience)
	env.set("TOOLGATE_SECRETS_KEYSET_FILE", keysetPath)

	stdout, stderr, serving := startServeCommand(t, env, &lockedBuffer{}, &lockedBuffer{})

	var address string

	require.Eventually(t, func() bool {
		for line := range strings.SplitSeq(stdout.String(), "\n") {
			var entry struct{ Msg, Address string }
			if json.Unmarshal([]byte(line), &entry) == nil && entry.Msg == "gateway started" {
				address = entry.Address

				return true
			}
		}

		return false
	}, 30*time.Second, 10*time.Millisecond, "the gateway did not start: %s", stderr.String())

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address+"/api/v1/secrets", nil)
	require.NoError(t, err)

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)

	_ = response.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, response.StatusCode, "a request without a token is refused")

	stopServe(t, stderr, serving)
}
