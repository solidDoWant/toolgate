package tokenstore_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/solidDoWant/toolgate/internal/client/tokenstore"
)

// errNoKeychain stands in for the error go-keyring returns when the OS has no
// keychain, for example a Linux host without a Secret Service on D-Bus.
var errNoKeychain = errors.New("no keychain: the name org.freedesktop.secrets was not provided")

func tokenFile(dir string) string {
	return filepath.Join(dir, tokenstore.FileName)
}

func TestSaveWithKeychainStoresInKeychain(t *testing.T) {
	keyring.MockInit()

	dir := t.TempDir()
	store := tokenstore.New(dir)

	require.NoError(t, store.Save("refresh-1"))

	stored, err := keyring.Get(tokenstore.KeychainService, tokenstore.KeychainUser)
	require.NoError(t, err)
	assert.Equal(t, "refresh-1", stored)
	assert.NoFileExists(t, tokenFile(dir), "with a keychain the token must not be written to a file")

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "refresh-1", loaded)
}

func TestSaveWithKeychainRemovesStaleFile(t *testing.T) {
	keyring.MockInit()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(tokenFile(dir), []byte("old"), 0o600))

	require.NoError(t, tokenstore.New(dir).Save("refresh-2"))

	assert.NoFileExists(t, tokenFile(dir))
}

func TestSaveWithoutKeychainWrites0600File(t *testing.T) {
	keyring.MockInitWithError(errNoKeychain)

	dir := filepath.Join(t.TempDir(), "toolgate")
	store := tokenstore.New(dir)

	require.NoError(t, store.Save("refresh-3"))

	content, err := os.ReadFile(tokenFile(dir))
	require.NoError(t, err)
	assert.Equal(t, "refresh-3", string(content))

	if runtime.GOOS != "windows" {
		info, err := os.Stat(tokenFile(dir))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

		dirInfo, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())
	}

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "refresh-3", loaded)
}

func TestSaveWithoutKeychainReplacesFile(t *testing.T) {
	keyring.MockInitWithError(errNoKeychain)

	dir := t.TempDir()
	store := tokenstore.New(dir)

	require.NoError(t, store.Save("first"))
	require.NoError(t, store.Save("second"))

	loaded, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, "second", loaded)
}

func TestSaveRejectsEmptyToken(t *testing.T) {
	keyring.MockInit()

	require.Error(t, tokenstore.New(t.TempDir()).Save(""))
}

func TestLoadWithNothingStored(t *testing.T) {
	tests := []struct {
		name    string
		keyring func()
	}{
		{name: "keychain", keyring: keyring.MockInit},
		{name: "no keychain", keyring: func() { keyring.MockInitWithError(errNoKeychain) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.keyring()

			_, err := tokenstore.New(t.TempDir()).Load()
			require.ErrorIs(t, err, tokenstore.ErrNotFound)
		})
	}
}

func TestDeleteRemovesTokenFromKeychainAndFile(t *testing.T) {
	keyring.MockInit()

	dir := t.TempDir()
	store := tokenstore.New(dir)

	require.NoError(t, keyring.Set(tokenstore.KeychainService, tokenstore.KeychainUser, "in-keychain"))
	require.NoError(t, os.WriteFile(tokenFile(dir), []byte("in-file"), 0o600))

	require.NoError(t, store.Delete())

	_, err := keyring.Get(tokenstore.KeychainService, tokenstore.KeychainUser)
	require.ErrorIs(t, err, keyring.ErrNotFound)
	assert.NoFileExists(t, tokenFile(dir))

	_, err = store.Load()
	require.ErrorIs(t, err, tokenstore.ErrNotFound)
}

func TestDeleteWithoutKeychainRemovesFile(t *testing.T) {
	keyring.MockInitWithError(errNoKeychain)

	dir := t.TempDir()
	store := tokenstore.New(dir)
	require.NoError(t, store.Save("in-file"))

	require.NoError(t, store.Delete())

	assert.NoFileExists(t, tokenFile(dir))
}

func TestDeleteWithNothingStored(t *testing.T) {
	keyring.MockInit()

	require.NoError(t, tokenstore.New(t.TempDir()).Delete())
}

func TestDefaultUsesUserConfigDir(t *testing.T) {
	keyring.MockInitWithError(errNoKeychain)

	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("HOME", configDir)
	t.Setenv("AppData", configDir)

	userConfigDir, err := os.UserConfigDir()
	require.NoError(t, err)

	store, err := tokenstore.Default()
	require.NoError(t, err)
	require.NoError(t, store.Save("refresh-4"))

	assert.FileExists(t, filepath.Join(userConfigDir, "toolgate", tokenstore.FileName))
}
