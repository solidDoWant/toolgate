// Package headers builds what `toolgate headers` prints for Claude Code's
// headersHelper: a fresh access token, the session ID and the workspace.
package headers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/oauth2"

	"github.com/solidDoWant/toolgate/internal/client/atomicfile"
	"github.com/solidDoWant/toolgate/internal/client/login"
	"github.com/solidDoWant/toolgate/internal/client/tokenstore"
)

// Environment variables `toolgate headers` reads.
const (
	SessionEnv   = "TOOLGATE_SESSION"
	WorkspaceEnv = "TOOLGATE_WORKSPACE"
	// ToolSearchEnv turns tool search mode on or off for the session. Its
	// value is sent as the X-Toolgate-Tool-Search header.
	ToolSearchEnv = "TOOLGATE_TOOL_SEARCH"
)

const (
	// SessionWait is how long FindSession waits for the SessionStart hook
	// to write the session file. Claude Code runs the hook and the
	// headersHelper at the same time.
	SessionWait = 5 * time.Second

	// maxHeaderValue is the longest value the gateway accepts in an
	// identity header.
	maxHeaderValue = 128
	// maxSessionFileBytes bounds the session file FindSession reads.
	maxSessionFileBytes = 1 << 10
	// maxAncestors bounds the parent walk.
	maxAncestors = 64
	// pollInterval is how often FindSession looks for the session file.
	pollInterval = 50 * time.Millisecond
	// gitTimeout bounds the `git remote` call that names the workspace.
	gitTimeout = 2 * time.Second
)

// ErrNoSession is returned by FindSession when no session file appeared in
// time.
var ErrNoSession = errors.New("no Claude Code session found")

// headerValuePattern is the pattern the gateway requires of both identity
// headers.
var headerValuePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// disallowedCharacters matches every character the identity headers can't
// hold.
var disallowedCharacters = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// RuntimeDir is the directory that holds the session files:
// $XDG_RUNTIME_DIR/toolgate when that variable is set, and otherwise
// os.UserCacheDir()/toolgate/run.
func RuntimeDir() (string, error) {
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		return filepath.Join(runtimeDir, tokenstore.DirName), nil
	}

	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find the user cache directory: %w", err)
	}

	return filepath.Join(cacheDir, tokenstore.DirName, "run"), nil
}

// ErrSignInAgain is returned by AccessToken when the IdP refuses the stored
// refresh token, because it expired or was revoked.
var ErrSignInAgain = errors.New("the IdP ended this sign-in")

// SignInAgainMessage is what the commands print for ErrSignInAgain.
const SignInAgainMessage = "Sign in again: run `toolgate login`."

// AccessToken uses the stored refresh token to get a new access token from
// the IdP the CLI signed in with. When the IdP rotates the refresh token, the
// new one is stored.
//
// Loading, refreshing and storing run under the store's lock, and the
// refresh token is loaded only once the lock is held. An IdP that rotates
// refresh tokens refuses the old one, so two processes that refreshed with
// the same token would end the sign-in.
func AccessToken(ctx context.Context, httpClient *http.Client, settings login.Settings, tokens *tokenstore.Store) (string, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	unlock, err := tokens.Lock(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()

	refreshToken, err := tokens.Load()
	if err != nil {
		return "", err
	}

	ctx = oidc.ClientContext(ctx, httpClient)

	provider, err := oidc.NewProvider(ctx, settings.Issuer)
	if err != nil {
		return "", fmt.Errorf("read the IdP's discovery document: %w", err)
	}

	config := &oauth2.Config{
		ClientID: settings.ClientID,
		Endpoint: provider.Endpoint(),
		Scopes:   settings.Scopes,
	}
	// The client is public: send the client ID in the form, with no secret.
	config.Endpoint.AuthStyle = oauth2.AuthStyleInParams

	token, err := config.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		var retrieveErr *oauth2.RetrieveError
		if errors.As(err, &retrieveErr) && retrieveErr.ErrorCode == "invalid_grant" {
			return "", fmt.Errorf("%w: %w", ErrSignInAgain, err)
		}

		return "", fmt.Errorf("refresh the access token: %w", err)
	}

	if token.AccessToken == "" {
		return "", errors.New("the IdP returned no access token")
	}

	if token.RefreshToken != "" && token.RefreshToken != refreshToken {
		if err := tokens.Save(token.RefreshToken); err != nil {
			return "", fmt.Errorf("store the new refresh token: %w", err)
		}
	}

	return token.AccessToken, nil
}

// SessionOptions configures FindSession.
type SessionOptions struct {
	// StartPID is the first process checked for a session file. Zero uses
	// the parent of this process.
	StartPID int
	// Wait is how long to wait for the session file. Zero uses SessionWait.
	Wait time.Duration
}

// FindSession returns the session ID from TOOLGATE_SESSION, or else from the
// session file of the first ancestor process that has one in RuntimeDir. It
// waits up to the bound for the file to appear and returns ErrNoSession when
// none does.
func FindSession(ctx context.Context, options SessionOptions) (string, error) {
	if session := os.Getenv(SessionEnv); session != "" {
		return session, nil
	}

	dir, err := RuntimeDir()
	if err != nil {
		return "", err
	}

	if options.StartPID == 0 {
		options.StartPID = os.Getppid()
	}

	if options.Wait == 0 {
		options.Wait = SessionWait
	}

	ancestors := ancestorPIDs(ctx, options.StartPID)

	deadline := time.NewTimer(options.Wait)
	defer deadline.Stop()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		session, found, err := readSessionFile(dir, ancestors)
		if err != nil || found {
			return session, err
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", ErrNoSession
		case <-ticker.C:
		}
	}
}

// ancestorPIDs returns start and its ancestors, nearest first. A process that
// can't be read ends the walk.
func ancestorPIDs(ctx context.Context, start int) []int {
	pids := []int{}

	for pid := start; pid > 0 && len(pids) < maxAncestors; {
		pids = append(pids, pid)

		current, err := process.NewProcessWithContext(ctx, int32(pid)) //nolint:gosec // PIDs fit in int32.
		if err != nil {
			break
		}

		parent, err := current.PpidWithContext(ctx)
		if err != nil || int(parent) == pid {
			break
		}

		pid = int(parent)
	}

	return pids
}

// readSessionFile returns the session ID in the file of the first PID in
// pids that has one. An empty file is not written yet.
func readSessionFile(dir string, pids []int) (string, bool, error) {
	for _, pid := range pids {
		name := filepath.Join(dir, strconv.Itoa(pid))

		file, err := os.Open(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}

		if err != nil {
			return "", false, fmt.Errorf("read session file: %w", err)
		}

		content, err := io.ReadAll(io.LimitReader(file, maxSessionFileBytes))
		_ = file.Close()

		if err != nil {
			return "", false, fmt.Errorf("read session file: %w", err)
		}

		session := strings.TrimSpace(string(content))
		if session == "" {
			continue
		}

		if !headerValuePattern.MatchString(session) {
			return "", false, fmt.Errorf("session file %s holds an invalid session ID", name)
		}

		return session, true, nil
	}

	return "", false, nil
}

// Workspace returns TOOLGATE_WORKSPACE when it is set. Otherwise it returns
// `<hostname>/<repository>` for the directory dir, normalised to the header
// pattern: every character outside [A-Za-z0-9._-] becomes "-", and the value
// is cut to 128 characters. The repository is the name of the git remote
// "origin", or the directory's name when there is none.
func Workspace(ctx context.Context, dir string) (string, error) {
	if workspace := os.Getenv(WorkspaceEnv); workspace != "" {
		return workspace, nil
	}

	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("read the hostname: %w", err)
	}

	name := repositoryName(ctx, dir)
	if name == "" {
		name = filepath.Base(dir)
	}

	return normalise(hostname + "/" + name), nil
}

// repositoryName is the last path element of the URL of the git remote
// "origin" of dir, without ".git", or "" when there is no such remote.
func repositoryName(ctx context.Context, dir string) string {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	command := exec.CommandContext(ctx, "git", "-C", dir, "remote", "get-url", "origin")

	output, err := command.Output()
	if err != nil {
		return ""
	}

	remote := strings.TrimSuffix(strings.TrimSpace(string(output)), "/")
	// scp-like remotes (host:owner/repo) have no slash before the path.
	remote = remote[strings.LastIndex(remote, ":")+1:]

	name := strings.TrimSuffix(path.Base(filepath.ToSlash(remote)), ".git")
	if name == "." || name == "/" {
		return ""
	}

	return name
}

func normalise(value string) string {
	value = disallowedCharacters.ReplaceAllString(value, "-")
	if len(value) > maxHeaderValue {
		value = value[:maxHeaderValue]
	}

	return value
}

// WriteSessionFile writes session to the session file of the Claude Code
// process pid in RuntimeDir, atomically. The directory gets mode 0700 and
// the file 0600.
func WriteSessionFile(pid int, session string) error {
	if !headerValuePattern.MatchString(session) {
		return errors.New("the session ID is invalid")
	}

	dir, err := RuntimeDir()
	if err != nil {
		return err
	}

	return atomicfile.Write(filepath.Join(dir, strconv.Itoa(pid)), []byte(session+"\n"), 0o600)
}
