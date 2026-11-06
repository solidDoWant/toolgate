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

// Mode says where a Store keeps the refresh token.
type Mode string

const (
	// ModeAuto tries the keychain and falls back to the file. Loading reads
	// the keychain first and then the file. An empty Mode means ModeAuto.
	ModeAuto Mode = "auto"
	// ModeKeychain uses only the OS keychain.
	ModeKeychain Mode = "keychain"
	// ModeFile uses only the file.
	ModeFile Mode = "file"
)

// ParseMode checks that value names a Mode. The empty string is ModeAuto.
func ParseMode(value string) (Mode, error) {
	switch Mode(value) {
	case "", ModeAuto:
		return ModeAuto, nil
	case ModeKeychain:
		return ModeKeychain, nil
	case ModeFile:
		return ModeFile, nil
	default:
		return "", fmt.Errorf("unknown token store %q: use auto, keychain or file", value)
	}
}

// Store saves, loads and deletes the refresh token.
type Store struct {
	dir  string
	mode Mode
	used Mode
}

// New returns a Store in ModeAuto whose file lives in dir.
func New(dir string) *Store {
	return NewWithMode(dir, ModeAuto)
}

// NewWithMode returns a Store that keeps the token as mode says, with its
// file in dir. An empty mode means ModeAuto.
func NewWithMode(dir string, mode Mode) *Store {
	if mode == "" {
		mode = ModeAuto
	}

	return &Store{dir: dir, mode: mode}
}

// Used returns the place the last Save wrote to: ModeKeychain or ModeFile.
// It is empty before a Save succeeds.
func (s *Store) Used() Mode {
	return s.used
}

// Path returns the token file's path.
func (s *Store) Path() string {
	return s.path()
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

// ErrKeychainUnusable is returned when a Store in ModeKeychain can't reach
// the OS keychain.
var ErrKeychainUnusable = errors.New("the OS keychain is not usable; sign in again with `toolgate login --token-store file`")

// Save stores token as the Store's mode says. In ModeAuto it tries the
// keychain and, if that can't be used, writes the file with mode 0600.
func (s *Store) Save(token string) error {
	if token == "" {
		return errors.New("refresh token is empty")
	}

	if s.mode == ModeFile {
		return s.saveFile(token)
	}

	err := keyring.Set(KeychainService, KeychainUser, token)
	if err != nil && s.mode == ModeKeychain {
		return fmt.Errorf("%w: %w", ErrKeychainUnusable, err)
	}

	if err == nil {
		s.used = ModeKeychain
		// A file left from a time without a keychain holds an older token.
		// Remove it so the keychain holds the only copy.
		if err := os.Remove(s.path()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove old token file: %w", err)
		}

		return nil
	}

	return s.saveFile(token)
}

func (s *Store) saveFile(token string) error {
	if err := s.writeFile(token); err != nil {
		return err
	}

	s.used = ModeFile

	return nil
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

// Load returns the stored token. In ModeAuto it reads the keychain first and
// then the file; in the other modes only the store the mode names. It returns
// ErrNotFound when that place holds none.
func (s *Store) Load() (string, error) {
	if s.mode != ModeFile {
		token, err := keyring.Get(KeychainService, KeychainUser)
		if err == nil && token != "" {
			return token, nil
		}

		if s.mode == ModeKeychain {
			if err == nil || errors.Is(err, keyring.ErrNotFound) {
				return "", ErrNotFound
			}

			return "", fmt.Errorf("%w: %w", ErrKeychainUnusable, err)
		}
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
