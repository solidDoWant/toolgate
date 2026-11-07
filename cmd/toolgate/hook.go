package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/solidDoWant/toolgate/internal/client/headers"
	"github.com/solidDoWant/toolgate/internal/client/login"
	"github.com/solidDoWant/toolgate/internal/client/settingssync"
	"github.com/solidDoWant/toolgate/internal/client/spiffe"
	"github.com/solidDoWant/toolgate/internal/client/tokencommand"
	"github.com/solidDoWant/toolgate/internal/client/tokenstore"
	"github.com/solidDoWant/toolgate/internal/settings"
)

const (
	// signedOutContext is the context the hook gives Claude Code when the
	// user is not signed in.
	signedOutContext = "toolgate: you are not signed in, so the toolgate MCP servers are not available. Run `toolgate login` in a terminal, then restart Claude Code."

	// maxHookInputBytes bounds the hook input read from standard input.
	maxHookInputBytes = 64 << 10
	// maxSettingsResponseBytes bounds the gateway's settings response.
	maxSettingsResponseBytes = 4 << 20
	// hookTimeout bounds the network calls of the hook.
	hookTimeout = 20 * time.Second
	// claudePIDEnv names the Claude Code process for its hooks.
	claudePIDEnv = "CLAUDE_PID"
)

// hookInput is the part of the SessionStart hook input the hook reads.
type hookInput struct {
	SessionID string `json:"session_id"`
}

// hookOutput is the SessionStart hook's answer to Claude Code.
type hookOutput struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// hook runs `toolgate hook <event>`.
func hook(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "session-start" {
		_, _ = fmt.Fprintln(stderr, "Usage: toolgate hook session-start")

		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()

	// The hook never blocks the session: every problem becomes context for
	// the session, and the exit code is 0.
	message := sessionStart(ctx, stdin)

	var output hookOutput

	output.HookSpecificOutput.HookEventName = "SessionStart"
	output.HookSpecificOutput.AdditionalContext = message

	if err := json.NewEncoder(stdout).Encode(output); err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate hook: %v\n", err)
	}

	return 0
}

// sessionStart writes the session file, then syncs the settings, and returns
// the context to give the session.
func sessionStart(ctx context.Context, stdin io.Reader) string {
	var notes []string

	// The session file comes first: `toolgate headers` runs at the same time
	// and waits for it.
	if err := writeSessionFile(stdin); err != nil {
		notes = append(notes, "toolgate: the session could not be recorded, so the toolgate MCP servers may not connect: "+err.Error())
	}

	// A failed attestation sends nothing: the request would change the
	// session's attestation.
	attested, err := attestationSource().Token(ctx)
	if err != nil {
		return strings.Join(append(notes, "toolgate: the attestation failed, so the settings profile was not fetched ("+err.Error()+")."), "\n")
	}

	if source := tokenCommandSource(); source.Command != "" {
		return strings.Join(append(notes, tokenCommandSettingsNotes(ctx, source, attested)...), "\n")
	}

	if socket := spiffe.Socket(); socket != "" {
		return strings.Join(append(notes, spiffeSettingsNotes(ctx, socket, attested)...), "\n")
	}

	dir, err := configDir()
	if err != nil {
		return strings.Join(append(notes, "toolgate: "+err.Error()), "\n")
	}

	clientSettings, err := login.LoadSettings(dir)
	if err == nil {
		var accessToken string

		accessToken, err = headers.AccessToken(ctx, nil, clientSettings, tokenStoreFor(dir, clientSettings))
		if err == nil {
			notes = append(notes, syncSettings(ctx, clientSettings.Gateway, accessToken, attested, dir)...)

			return strings.Join(notes, "\n")
		}
	}

	if errors.Is(err, login.ErrNotSignedIn) || errors.Is(err, tokenstore.ErrNotFound) {
		return strings.Join(append(notes, signedOutContext), "\n")
	}

	return strings.Join(append(notes, "toolgate: sign-in failed ("+err.Error()+"). Run `toolgate login` in a terminal, then restart Claude Code."), "\n")
}

// spiffeSettingsNotes syncs the settings with a JWT-SVID. It never falls
// back to the stored sign-in.
func spiffeSettingsNotes(ctx context.Context, socket, attested string) []string {
	access, err := spiffeGateway(ctx, socket)
	if err != nil {
		return []string{"toolgate: the SPIFFE sign-in failed (" + err.Error() + ")."}
	}

	dir, err := configDir()
	if err != nil {
		return []string{"toolgate: " + err.Error()}
	}

	return syncSettings(ctx, access.gateway, access.accessToken, attested, dir)
}

// tokenCommandSettingsNotes syncs the settings with the token from
// TOOLGATE_TOKEN_COMMAND. It never falls back to the stored sign-in.
func tokenCommandSettingsNotes(ctx context.Context, source tokencommand.Source, attested string) []string {
	access, err := tokenCommandGateway(ctx, source)
	if err != nil {
		return []string{"toolgate: the token command failed (" + err.Error() + ")."}
	}

	dir, err := configDir()
	if err != nil {
		return []string{"toolgate: " + err.Error()}
	}

	return syncSettings(ctx, access.gateway, access.accessToken, attested, dir)
}

// writeSessionFile writes the session ID from the hook input to the session
// file of the Claude Code process.
func writeSessionFile(stdin io.Reader) error {
	content, err := io.ReadAll(io.LimitReader(stdin, maxHookInputBytes))
	if err != nil {
		return fmt.Errorf("read the hook input: %w", err)
	}

	var input hookInput
	if err := json.Unmarshal(content, &input); err != nil {
		return fmt.Errorf("read the hook input: %w", err)
	}

	if input.SessionID == "" {
		input.SessionID = os.Getenv("CLAUDE_CODE_SESSION_ID")
	}

	if input.SessionID == "" {
		return errors.New("the hook input has no session ID")
	}

	pid := os.Getppid()
	if value, err := strconv.Atoi(os.Getenv(claudePIDEnv)); err == nil && value > 0 {
		pid = value
	}

	return headers.WriteSessionFile(pid, input.SessionID)
}

// syncSettings fetches the merged settings profile and merges it into
// ~/.claude/settings.json. It returns the notes for the session.
func syncSettings(ctx context.Context, gateway, accessToken, attested, dir string) []string {
	profile, err := fetchSettings(ctx, gateway, accessToken, attested)
	if errors.Is(err, errUnauthorized) {
		if token, refreshErr := refreshedToken(ctx); refreshErr == nil {
			profile, err = fetchSettings(ctx, gateway, token, attested)
		}
	}

	if err != nil {
		return []string{"toolgate: the settings profile could not be fetched: " + err.Error()}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return []string{"toolgate: " + err.Error()}
	}

	result, err := settingssync.Sync(
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(dir, settingssync.OwnedFileName),
		profile,
	)
	if err != nil {
		return []string{"toolgate: the settings profile could not be applied: " + err.Error()}
	}

	notes := []string{}
	if len(result.UserChanged) > 0 {
		notes = append(notes, "toolgate: these settings were changed by hand, so the settings profile did not change them: "+strings.Join(result.UserChanged, ", ")+".")
	}

	return notes
}

func fetchSettings(ctx context.Context, gateway, accessToken, attested string) (map[string]json.RawMessage, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(gateway, "/")+settings.Path, nil)
	if err != nil {
		return nil, err
	}

	setCredentials(request.Header, accessToken, attested)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()

	// A gateway from before the route existed has no profile to give.
	if response.StatusCode == http.StatusNotFound {
		return map[string]json.RawMessage{}, nil
	}

	if response.StatusCode != http.StatusOK {
		err := fmt.Errorf("the gateway answered %s", response.Status)
		if response.StatusCode == http.StatusUnauthorized {
			err = errors.Join(errUnauthorized, err)
		}

		return nil, err
	}

	content, err := io.ReadAll(io.LimitReader(response.Body, maxSettingsResponseBytes+1))
	if err != nil {
		return nil, err
	}

	if len(content) > maxSettingsResponseBytes {
		return nil, errors.New("the settings profile is too large")
	}

	profile := map[string]json.RawMessage{}
	if err := json.Unmarshal(content, &profile); err != nil {
		return nil, fmt.Errorf("the gateway sent an invalid settings profile: %w", err)
	}

	return profile, nil
}
