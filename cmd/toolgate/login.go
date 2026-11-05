package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/solidDoWant/toolgate/internal/client/login"
	"github.com/solidDoWant/toolgate/internal/client/tokenstore"
)

// configDir is os.UserConfigDir()/toolgate, which holds config.json and the
// token store's fallback file.
func configDir() (string, error) {
	userConfigDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find the user configuration directory: %w", err)
	}

	return filepath.Join(userConfigDir, tokenstore.DirName), nil
}

// signIn runs `toolgate login` and returns the exit code.
func signIn(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("login", flag.ContinueOnError)
	flags.SetOutput(stderr)
	gateway := flags.String("gateway", "", "gateway URL (default: the gateway of the last sign-in)")
	issuer := flags.String("issuer", "", "IdP issuer URL, instead of the gateway's (for testing)")
	clientID := flags.String("client-id", "", "IdP client ID, instead of the gateway's (for testing)")
	scopes := flags.String("scopes", "", "space-separated scopes, instead of the gateway's (for testing)")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	if flags.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "toolgate login: unexpected argument %q\n", flags.Arg(0))

		return 2
	}

	if err := runSignIn(*gateway, *issuer, *clientID, strings.Fields(*scopes), stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate login: %v\n", err)

		return 1
	}

	return 0
}

func runSignIn(gateway, issuer, clientID string, scopes []string, stdout io.Writer) error {
	dir, err := configDir()
	if err != nil {
		return err
	}

	if gateway == "" {
		previous, err := login.LoadSettings(dir)
		if err != nil && !errors.Is(err, login.ErrNotSignedIn) {
			return err
		}

		gateway = previous.Gateway
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	settings, err := login.Run(ctx, login.Options{
		Gateway:  gateway,
		Issuer:   issuer,
		ClientID: clientID,
		Scopes:   scopes,
		Prompt:   stdout,
		Tokens:   tokenstore.New(dir),
	})
	if err != nil {
		return err
	}

	if err := login.SaveSettings(dir, settings); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(stdout, "Signed in as %s.\n", displayName(settings))

	return nil
}

// status runs `toolgate status` and returns the exit code.
func status(stdout, stderr io.Writer) int {
	dir, err := configDir()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate status: %v\n", err)

		return 1
	}

	settings, err := login.LoadSettings(dir)
	if err == nil {
		_, err = tokenstore.New(dir).Load()
	}

	if errors.Is(err, login.ErrNotSignedIn) || errors.Is(err, tokenstore.ErrNotFound) {
		_, _ = fmt.Fprintln(stdout, "Not signed in. Run toolgate login.")

		return 1
	}

	if err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate status: %v\n", err)

		return 1
	}

	_, _ = fmt.Fprintf(stdout, "Gateway:   %s\nPrincipal: %s\n", settings.Gateway, settings.Subject)
	if settings.Email != "" {
		_, _ = fmt.Fprintf(stdout, "Email:     %s\n", emailDisplay(settings))
	}

	return 0
}

// emailDisplay is the email, followed by "(unverified)" unless the IdP
// verified it.
func emailDisplay(settings login.Settings) string {
	if settings.EmailVerified {
		return settings.Email
	}

	return settings.Email + " (unverified)"
}

func displayName(settings login.Settings) string {
	if settings.Email != "" {
		return emailDisplay(settings)
	}

	return settings.Subject
}
