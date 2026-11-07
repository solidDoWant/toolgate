package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/client/headers"
	"github.com/solidDoWant/toolgate/internal/client/login"
	"github.com/solidDoWant/toolgate/internal/client/spiffe"
	"github.com/solidDoWant/toolgate/internal/client/tokenstore"
)

// noSessionMessage is printed when no Claude Code session is found.
const noSessionMessage = "No Claude Code session found. Start Claude Code with the toolgate plugin, or set TOOLGATE_SESSION."

// printHeaders runs `toolgate headers`: it prints the headers for Claude
// Code's headersHelper as a JSON object and returns the exit code.
func printHeaders(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		_, _ = fmt.Fprintf(stderr, "toolgate headers: unexpected argument %q\n", args[0])

		return 2
	}

	values, err := buildHeaders(context.Background())
	if errors.Is(err, headers.ErrNoSession) {
		_, _ = fmt.Fprintln(stderr, noSessionMessage)

		return 1
	}

	if errors.Is(err, headers.ErrSignInAgain) {
		_, _ = fmt.Fprintln(stderr, headers.SignInAgainMessage)

		return 1
	}

	if errors.Is(err, login.ErrNotSignedIn) || errors.Is(err, tokenstore.ErrNotFound) {
		_, _ = fmt.Fprintln(stderr, "toolgate headers: not signed in. Run toolgate login.")

		return 1
	}

	if err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate headers: %v\n", err)

		return 1
	}

	if err := json.NewEncoder(stdout).Encode(values); err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate headers: %v\n", err)

		return 1
	}

	return 0
}

func buildHeaders(ctx context.Context) (map[string]string, error) {
	// The session is looked up first: without one, no headers are printed
	// and the IdP is not asked for a token.
	session, err := headers.FindSession(ctx, headers.SessionOptions{})
	if err != nil {
		return nil, err
	}

	values, err := credentialHeaders(ctx)
	if err != nil {
		return nil, err
	}

	values[auth.SessionHeader] = session

	if mode := toolSearchMode(os.Getenv(headers.ToolSearchEnv)); mode != "" {
		values[auth.ToolSearchHeader] = mode
	}

	return values, nil
}

// credentialHeaders returns the Authorization and workspace headers shared
// by `toolgate headers` and `toolgate otel-headers`.
func credentialHeaders(ctx context.Context) (map[string]string, error) {
	accessToken, err := bearerToken(ctx)
	if err != nil {
		return nil, err
	}

	attested, err := attestationSource().Token(ctx)
	if err != nil {
		return nil, err
	}

	workingDir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("read the working directory: %w", err)
	}

	workspace, err := headers.Workspace(ctx, workingDir)
	if err != nil {
		return nil, err
	}

	values := map[string]string{
		"Authorization":      "Bearer " + accessToken,
		auth.WorkspaceHeader: workspace,
	}

	if attested != "" {
		values[auth.AttestationHeader] = attested
	}

	return values, nil
}

// bearerToken returns the token for the gateway: a fresh JWT-SVID when
// TOOLGATE_SPIFFE_SOCKET is set, otherwise the stored sign-in's access token.
// A failed fetch never falls back to the sign-in.
func bearerToken(ctx context.Context) (string, error) {
	if source := tokenCommandSource(); source.Command != "" {
		return source.Token(ctx)
	}

	if socket := spiffe.Socket(); socket != "" {
		return spiffe.FetchToken(ctx, socket)
	}

	dir, err := configDir()
	if err != nil {
		return "", err
	}

	settings, err := login.LoadSettings(dir)
	if err != nil {
		return "", err
	}

	return headers.AccessToken(ctx, nil, settings, tokenStoreFor(dir, settings))
}

// printOtelHeaders runs `toolgate otel-headers`: it prints the headers for
// Claude Code's otelHeadersHelper as a JSON object and returns the exit
// code. It doesn't wait for a session file.
func printOtelHeaders(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		_, _ = fmt.Fprintf(stderr, "toolgate otel-headers: unexpected argument %q\n", args[0])

		return 2
	}

	values, err := credentialHeaders(context.Background())
	if errors.Is(err, headers.ErrSignInAgain) {
		_, _ = fmt.Fprintln(stderr, headers.SignInAgainMessage)

		return 1
	}

	if errors.Is(err, login.ErrNotSignedIn) || errors.Is(err, tokenstore.ErrNotFound) {
		_, _ = fmt.Fprintln(stderr, "toolgate otel-headers: not signed in. Run toolgate login.")

		return 1
	}

	if err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate otel-headers: %v\n", err)

		return 1
	}

	if err := json.NewEncoder(stdout).Encode(values); err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate otel-headers: %v\n", err)

		return 1
	}

	return 0
}

// toolSearchMode maps the value of TOOLGATE_TOOL_SEARCH to the header
// value: "on" or "off", or empty when the variable is unset or not a
// boolean, so the gateway's setting decides.
func toolSearchMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "on", "true", "1", "yes":
		return "on"
	case "off", "false", "0", "no":
		return "off"
	default:
		return ""
	}
}
