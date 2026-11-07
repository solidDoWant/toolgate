package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/solidDoWant/toolgate/internal/client/attestation"
	"github.com/solidDoWant/toolgate/internal/client/login"
	"github.com/solidDoWant/toolgate/internal/client/tokencommand"
)

// savedSettings returns config.json, or empty settings when there is none or
// it can't be read. It is the fallback for settings the environment doesn't
// set.
func savedSettings() login.Settings {
	dir, err := configDir()
	if err != nil {
		return login.Settings{}
	}

	settings, err := login.LoadSettings(dir)
	if err != nil {
		return login.Settings{}
	}

	return settings
}

// tokenCommandSource returns the token command from TOOLGATE_TOKEN_COMMAND,
// or else from config.json.
func tokenCommandSource() tokencommand.Source {
	if source := tokencommand.FromEnv(); source.Command != "" {
		return source
	}

	return tokencommand.Source{Command: strings.TrimSpace(savedSettings().TokenCommand)}
}

// attestationSource returns the attestation source from the environment
// when any attestation variable is set, and otherwise from config.json.
func attestationSource() attestation.Source {
	source := attestation.FromEnv()
	if source.Configured() {
		return source
	}

	saved := savedSettings()
	source.TokenFile = strings.TrimSpace(saved.AttestationTokenFile)
	source.TokenRequest = strings.TrimSpace(saved.AttestationTokenRequest)

	return source
}

// configure runs `toolgate configure`: it saves the given settings in
// config.json, keeps the others, and returns the exit code. A flag given
// with an empty value removes its setting.
func configure(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("configure", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.String("gateway", "", "gateway URL")
	flags.String("token-command", "", "command that prints the user's access token (as "+tokencommand.Env+")")
	flags.String("attestation-token-file", "", "path of the workload token file (as "+attestation.TokenFileEnv+")")
	flags.String("attestation-token-request", "", "directory for a TokenRequest token (as "+attestation.TokenRequestEnv+")")

	if err := flags.Parse(args); err != nil {
		return 2
	}

	if flags.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "toolgate configure: unexpected argument %q\n", flags.Arg(0))

		return 2
	}

	if flags.NFlag() == 0 {
		_, _ = fmt.Fprintln(stderr, "toolgate configure: give at least one setting; run toolgate configure -h")

		return 2
	}

	dir, err := configDir()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate configure: %v\n", err)

		return 1
	}

	settings, err := login.LoadSettings(dir)
	if err != nil && !errors.Is(err, login.ErrNotSignedIn) {
		_, _ = fmt.Fprintf(stderr, "toolgate configure: %v\n", err)

		return 1
	}

	flags.Visit(func(set *flag.Flag) {
		value := strings.TrimSpace(set.Value.String())

		switch set.Name {
		case "gateway":
			settings.Gateway = strings.TrimSuffix(value, "/")
		case "token-command":
			settings.TokenCommand = value
		case "attestation-token-file":
			settings.AttestationTokenFile = value
		case "attestation-token-request":
			settings.AttestationTokenRequest = value
		}
	})

	if err := validateConfigured(settings); err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate configure: %v\n", err)

		return 2
	}

	if err := login.SaveSettings(dir, settings); err != nil {
		_, _ = fmt.Fprintf(stderr, "toolgate configure: %v\n", err)

		return 1
	}

	_, _ = fmt.Fprintf(stdout, "Saved the settings in %s.\n", dir)

	return 0
}

func validateConfigured(settings login.Settings) error {
	if settings.Gateway != "" {
		parsed, err := url.Parse(settings.Gateway)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("gateway URL %q is not an http or https URL", settings.Gateway)
		}
	}

	if settings.AttestationTokenFile != "" && settings.AttestationTokenRequest != "" {
		return errors.New("set only one of --attestation-token-file and --attestation-token-request")
	}

	return nil
}
