// Package tokencommand gets the user's access token from the command in
// TOOLGATE_TOKEN_COMMAND, for workspaces where another service (such as
// Coder's external auth) already holds the user's consent.
package tokencommand

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/solidDoWant/toolgate/internal/client/atomicfile"
	"github.com/solidDoWant/toolgate/internal/client/headers"
)

const (
	// Env names the variable that holds the command.
	Env = "TOOLGATE_TOKEN_COMMAND"

	// Timeout bounds one run of the command.
	Timeout = 5 * time.Second

	// MaxOutput is the most standard output the command may print.
	MaxOutput = 16 << 10

	// MaxStderr is the most standard error passed through.
	MaxStderr = 4 << 10

	// reuseMargin is how long before its expiry a cached token stops being
	// handed out.
	reuseMargin = 60 * time.Second

	waitDelay = time.Second
	lockName  = "token-command.lock"
	cacheStem = "token-command-"
)

// token68 is the RFC 6750 token68 syntax.
var token68 = regexp.MustCompile(`^[A-Za-z0-9\-._~+/]+=*$`)

// Error is a failure of the command. Its text ends in a full stop and never
// holds the command's output.
type Error struct{ message string }

func (e *Error) Error() string { return e.message + "." }

func failure(format string, args ...any) error {
	return &Error{message: fmt.Sprintf(format, args...)}
}

// Command returns the configured command, or "" when it is not in use.
func Command() string {
	return strings.TrimSpace(os.Getenv(Env))
}

// Source runs one command. The zero values of the optional fields mean the
// defaults.
type Source struct {
	// Command is run with the shell.
	Command string
	// Dir holds the cache and the lock. Default: headers.RuntimeDir().
	Dir string
	// Stderr receives the command's standard error, cut at MaxStderr.
	// Default: os.Stderr.
	Stderr io.Writer
	// Timeout overrides the package Timeout.
	Timeout time.Duration
	// Now overrides the clock.
	Now func() time.Time
}

// FromEnv returns a Source for the command in the environment.
func FromEnv() Source {
	return Source{Command: Command()}
}

// Token returns the cached token while it has more than 60 s to live, and
// otherwise runs the command. The lock is held while checking, running and
// writing, so concurrent callers run the command once.
func (s Source) Token(ctx context.Context) (string, error) {
	dir, err := s.dir()
	if err != nil {
		return "", err
	}

	lock := flock.New(filepath.Join(dir, lockName))
	if err := lock.Lock(); err != nil {
		return "", fmt.Errorf("lock the %s cache: %w", Env, err)
	}

	defer func() { _ = lock.Unlock() }()

	path := s.cachePath(dir)

	if cached, err := os.ReadFile(path); err == nil { //nolint:gosec // path is built from the runtime directory
		token := strings.TrimSpace(string(cached))
		if s.reusable(token) {
			return token, nil
		}
	}

	token, err := s.run(ctx)
	if err != nil {
		return "", err
	}

	if s.reusable(token) {
		if err := atomicfile.Write(path, []byte(token), 0o600); err != nil {
			return "", fmt.Errorf("write the %s cache: %w", Env, err)
		}
	} else {
		_ = os.Remove(path)
	}

	return token, nil
}

// Invalidate deletes the cached token, for a token the gateway refused.
func (s Source) Invalidate() error {
	dir, err := s.dir()
	if err != nil {
		return err
	}

	lock := flock.New(filepath.Join(dir, lockName))
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("lock the %s cache: %w", Env, err)
	}

	defer func() { _ = lock.Unlock() }()

	if err := os.Remove(s.cachePath(dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

func (s Source) dir() (string, error) {
	dir := s.Dir
	if dir == "" {
		var err error

		dir, err = headers.RuntimeDir()
		if err != nil {
			return "", err
		}
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create the runtime directory: %w", err)
	}

	return dir, nil
}

func (s Source) cachePath(dir string) string {
	sum := sha256.Sum256([]byte(s.Command))

	return filepath.Join(dir, cacheStem+hex.EncodeToString(sum[:])[:16])
}

func (s Source) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}

	return time.Now()
}

// reusable reports whether token has a readable exp more than 60 s away. The
// signature is deliberately not checked: the gateway does that.
func (s Source) reusable(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}

	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return false
	}

	var claims struct {
		Exp *float64 `json:"exp"`
	}

	if json.Unmarshal(payload, &claims) != nil || claims.Exp == nil {
		return false
	}

	expiry := time.Unix(int64(*claims.Exp), 0)

	return expiry.Sub(s.now()) > reuseMargin
}

// limitedWriter passes at most max bytes to dst and drops the rest.
type limitedWriter struct {
	dst  io.Writer
	left int
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	if w.left > 0 {
		chunk := data
		if len(chunk) > w.left {
			chunk = chunk[:w.left]
		}

		w.left -= len(chunk)
		_, _ = w.dst.Write(chunk)
	}

	return len(data), nil
}

// run executes the command once and validates its output.
func (s Source) run(ctx context.Context) (string, error) {
	timeout := s.Timeout
	if timeout == 0 {
		timeout = Timeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell, flags := shellCommand()
	command := exec.CommandContext(ctx, shell, append(flags, s.Command)...) //nolint:gosec // running the user's own command is the feature
	command.Stdin = bytes.NewReader(nil)
	command.WaitDelay = waitDelay

	stderr := s.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	command.Stderr = &limitedWriter{dst: stderr, left: MaxStderr}
	setProcessGroup(command)

	stdout, err := command.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("%s: %w", Env, err)
	}

	if err := command.Start(); err != nil {
		return "", fmt.Errorf("%s couldn't start: %w", Env, err)
	}

	output, _ := io.ReadAll(io.LimitReader(stdout, MaxOutput+1))
	tooBig := len(output) > MaxOutput

	if tooBig {
		// Stop the command, which may be blocked writing.
		killGroup(command)
	}

	waitErr := command.Wait()

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "", failure("%s didn't finish within %d s", Env, int(timeout/time.Second))
	case tooBig:
		return "", failure("%s printed more than 16 KiB", Env)
	case waitErr != nil:
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return "", failure("%s exited with status %d", Env, exitErr.ExitCode())
		}

		return "", fmt.Errorf("%s failed: %w", Env, waitErr)
	}

	token := strings.TrimSpace(string(output))
	if token == "" {
		return "", failure("%s printed no token", Env)
	}

	if !token68.MatchString(token) {
		return "", failure("%s printed something that isn't a token", Env)
	}

	return token, nil
}
