package tokenstore

import (
	"errors"
	"path/filepath"
	"runtime"
)

// ConfigDir is os.UserConfigDir with the environment read through getenv.
func ConfigDir(getenv func(string) string) (string, error) {
	switch runtime.GOOS {
	case "windows":
		if dir := getenv("AppData"); dir != "" {
			return dir, nil
		}

		return "", errors.New("%AppData% is not defined")
	case "darwin", "ios":
		if home := getenv("HOME"); home != "" {
			return filepath.Join(home, "Library", "Application Support"), nil
		}

		return "", errors.New("$HOME is not defined")
	case "plan9":
		if home := getenv("home"); home != "" {
			return filepath.Join(home, "lib"), nil
		}

		return "", errors.New("$home is not defined")
	default:
		if dir := getenv("XDG_CONFIG_HOME"); dir != "" {
			if !filepath.IsAbs(dir) {
				return "", errors.New("path in $XDG_CONFIG_HOME is relative")
			}

			return dir, nil
		}

		if home := getenv("HOME"); home != "" {
			return filepath.Join(home, ".config"), nil
		}

		return "", errors.New("neither $XDG_CONFIG_HOME nor $HOME are defined")
	}
}

// CacheDir is os.UserCacheDir with the environment read through getenv.
func CacheDir(getenv func(string) string) (string, error) {
	switch runtime.GOOS {
	case "windows":
		if dir := getenv("LocalAppData"); dir != "" {
			return dir, nil
		}

		return "", errors.New("%LocalAppData% is not defined")
	case "darwin", "ios":
		if home := getenv("HOME"); home != "" {
			return filepath.Join(home, "Library", "Caches"), nil
		}

		return "", errors.New("$HOME is not defined")
	case "plan9":
		if home := getenv("home"); home != "" {
			return filepath.Join(home, "lib", "cache"), nil
		}

		return "", errors.New("$home is not defined")
	default:
		if dir := getenv("XDG_CACHE_HOME"); dir != "" {
			if !filepath.IsAbs(dir) {
				return "", errors.New("path in $XDG_CACHE_HOME is relative")
			}

			return dir, nil
		}

		if home := getenv("HOME"); home != "" {
			return filepath.Join(home, ".cache"), nil
		}

		return "", errors.New("neither $XDG_CACHE_HOME nor $HOME are defined")
	}
}

// HomeDir is os.UserHomeDir with the environment read through getenv.
func HomeDir(getenv func(string) string) (string, error) {
	name := "HOME"

	switch runtime.GOOS {
	case "windows":
		name = "USERPROFILE"
	case "plan9":
		name = "home"
	}

	if dir := getenv(name); dir != "" {
		return dir, nil
	}

	return "", errors.New("$" + name + " is not defined")
}
