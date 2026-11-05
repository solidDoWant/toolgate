// Package tokenstore keeps the CLI's refresh token. It uses the OS keychain
// through github.com/zalando/go-keyring and falls back to a file only its
// owner can read when the OS has no keychain.
package tokenstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
)

const (
	// KeychainService is the keychain service name the token is stored under.
	KeychainService = "toolgate"
	// KeychainUser is the keychain account name the token is stored under.
	KeychainUser = "refresh-token"
	// FileName is the name of the fallback file inside the store's directory.
	FileName = "refresh-token"
	// DirName is the directory under os.UserConfigDir that Default uses.
	DirName = "toolgate"

	fileMode = 0o600
	dirMode  = 0o700
)

// ErrNotFound is returned by Load when no token is stored.
var ErrNotFound = errors.New("no refresh token stored")

// Store saves, loads and deletes the refresh token.
type Store struct {
	dir string
}

// New returns a Store whose fallback file lives in dir.
func New(dir string) *Store {
	return &Store{dir: dir}
}

// Default returns a Store whose fallback file lives in os.UserConfigDir()/toolgate.
func Default() (*Store, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("find the user configuration directory: %w", err)
	}

	return New(filepath.Join(configDir, DirName)), nil
}

func (s *Store) path() string {
	return filepath.Join(s.dir, FileName)
}

// Save stores token in the keychain. If the keychain can't be used, it writes
// the token to the fallback file with mode 0600 instead.
func (s *Store) Save(token string) error {
	if token == "" {
		return errors.New("refresh token is empty")
	}

	if err := keyring.Set(KeychainService, KeychainUser, token); err == nil {
		// A file left from a time without a keychain holds an older token.
		// Remove it so the keychain holds the only copy.
		if err := os.Remove(s.path()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove old token file: %w", err)
		}

		return nil
	}

	return s.writeFile(token)
}

// writeFile writes token to the fallback file atomically, so a reader never
// sees a partial token and the file never exists with a wider mode.
func (s *Store) writeFile(token string) error {
	if err := os.MkdirAll(s.dir, dirMode); err != nil {
		return fmt.Errorf("create token directory: %w", err)
	}

	tmp, err := os.CreateTemp(s.dir, FileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create token file: %w", err)
	}

	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	// os.CreateTemp already creates the file with mode 0600; Chmod states it.
	if err := tmp.Chmod(fileMode); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		_ = tmp.Close()

		return fmt.Errorf("set token file mode: %w", err)
	}

	if _, err := tmp.WriteString(token); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("write token file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close token file: %w", err)
	}

	if err := os.Rename(tmpName, s.path()); err != nil {
		return fmt.Errorf("replace token file: %w", err)
	}

	return nil
}

// Load returns the stored token, from the keychain first and then from the
// fallback file. It returns ErrNotFound when neither holds one.
func (s *Store) Load() (string, error) {
	if token, err := keyring.Get(KeychainService, KeychainUser); err == nil && token != "" {
		return token, nil
	}

	content, err := os.ReadFile(s.path())
	if errors.Is(err, fs.ErrNotExist) || (err == nil && len(content) == 0) {
		return "", ErrNotFound
	}

	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}

	return string(content), nil
}

// Delete removes the token from the keychain and the fallback file. Having
// nothing stored is not an error.
func (s *Store) Delete() error {
	keychainErr := keyring.Delete(KeychainService, KeychainUser)

	if err := os.Remove(s.path()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove token file: %w", err)
	}

	// Without a keychain, Delete fails the same way Set and Get do, and there
	// is nothing to remove. Only report a keychain error if a token is still
	// readable from it.
	if keychainErr != nil && !errors.Is(keychainErr, keyring.ErrNotFound) {
		if token, err := keyring.Get(KeychainService, KeychainUser); err == nil && token != "" {
			return fmt.Errorf("remove token from keychain: %w", keychainErr)
		}
	}

	return nil
}
