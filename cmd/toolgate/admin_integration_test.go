//go:build integration

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/insecurecleartextkeyset"
	"github.com/tink-crypto/tink-go/v2/keyset"

	"github.com/solidDoWant/toolgate/internal/secrets"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

func writeKeysetFile(t *testing.T, path string, handle *keyset.Handle) {
	t.Helper()

	var buffer bytes.Buffer
	require.NoError(t, insecurecleartextkeyset.Write(handle, keyset.NewJSONWriter(&buffer)))
	require.NoError(t, os.WriteFile(path, buffer.Bytes(), 0o600))
}

func TestAdminRewrapReencryptsEveryRowUnderThePrimaryKey(t *testing.T) {
	clearConfigEnv(t)

	pg := testutil.StartPostgres(t)
	pool, err := store.Open(t.Context(), pg.ConnString())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, store.Migrate(t.Context(), pool))

	queries := db.New(pool)

	handle, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	require.NoError(t, err)
	oldCipher, err := secrets.NewCipher(handle)
	require.NoError(t, err)

	values := map[[2]string]string{}

	for _, principal := range []string{"alice", "bob"} {
		for _, name := range []string{"github-token", "api-key", "third"} {
			value := principal + "/" + name + "-value"
			values[[2]string{principal, name}] = value
			_, err := secrets.NewBroker(queries, oldCipher).Set(t.Context(), principal, name, []byte(value))
			require.NoError(t, err)
		}
	}

	// A new keyset version: a new primary key, the old key kept.
	manager := keyset.NewManagerFromHandle(handle)
	newKeyID, err := manager.Add(aead.AES256GCMKeyTemplate())
	require.NoError(t, err)
	require.NoError(t, manager.SetPrimary(newKeyID))
	rotated, err := manager.Handle()
	require.NoError(t, err)

	keysetPath := filepath.Join(t.TempDir(), "keyset.json")
	writeKeysetFile(t, keysetPath, rotated)

	t.Setenv("TOOLGATE_DATABASE_URL", pg.ConnString())
	t.Setenv("TOOLGATE_OIDC_ISSUER_URL", "https://idp.example")
	t.Setenv("TOOLGATE_OIDC_AUDIENCE", "toolgate")
	t.Setenv("TOOLGATE_SECRETS_KEYSET_FILE", keysetPath)

	var stdout, stderr bytes.Buffer

	code := run([]string{"admin", "rewrap"}, &stdout, &stderr)
	require.Equal(t, 0, code, "stderr: %s", stderr.String())
	assert.Contains(t, stdout.String(), "Re-encrypted 6 secrets")

	var notPrimary int
	require.NoError(t, pool.QueryRow(t.Context(),
		"SELECT count(*) FROM user_secrets WHERE key_id <> $1", int64(newKeyID)).Scan(&notPrimary))
	assert.Zero(t, notPrimary, "every row is under the primary key")

	// The old key can now be removed, and every value still decrypts.
	require.NoError(t, manager.Disable(oldCipher.PrimaryKeyID()))
	require.NoError(t, manager.Delete(oldCipher.PrimaryKeyID()))
	newOnly, err := manager.Handle()
	require.NoError(t, err)
	require.Len(t, newOnly.KeysetInfo().GetKeyInfo(), 1)

	newCipher, err := secrets.NewCipher(newOnly)
	require.NoError(t, err)

	broker := secrets.NewBroker(queries, newCipher)

	for key, value := range values {
		got, _, err := broker.Get(t.Context(), key[0], key[1])
		require.NoError(t, err, "%v", key)
		assert.Equal(t, value, string(got))
	}
}

func TestAdminRewrapNeedsTheKeyset(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("TOOLGATE_DATABASE_URL", "postgres://toolgate@127.0.0.1:1/toolgate")
	t.Setenv("TOOLGATE_OIDC_ISSUER_URL", "https://idp.example")
	t.Setenv("TOOLGATE_OIDC_AUDIENCE", "toolgate")

	var stdout, stderr bytes.Buffer

	code := run([]string{"admin", "rewrap"}, &stdout, &stderr)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr.String(), "secretsKeysetFile is not set")
}
