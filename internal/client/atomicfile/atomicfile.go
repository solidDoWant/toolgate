// Package atomicfile writes files so that readers see the old content or the
// new content, never part of either.
package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Write writes content to path through a temporary file in the
// same directory and a rename, so readers see the old file or the new one,
// never part of either. It creates the directory with mode 0700 when it is
// missing.
func Write(path string, content []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	temporary, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	temporaryName := temporary.Name()

	cleanup := func(err error) error {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)

		return fmt.Errorf("write %s: %w", path, err)
	}

	if _, err := temporary.Write(content); err != nil {
		return cleanup(err)
	}

	if err := temporary.Chmod(mode); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		return cleanup(err)
	}

	if err := temporary.Sync(); err != nil {
		return cleanup(err)
	}

	if err := temporary.Close(); err != nil {
		return cleanup(err)
	}

	if err := os.Rename(temporaryName, path); err != nil {
		_ = os.Remove(temporaryName)

		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}
