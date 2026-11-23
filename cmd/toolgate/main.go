// Command toolgate is the toolgate MCP gateway (`toolgate serve`) and the
// client CLI used by the Claude Code plugin.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/solidDoWant/toolgate/internal/client/spiffe"
	"github.com/solidDoWant/toolgate/internal/client/tokencommand"
	"github.com/solidDoWant/toolgate/internal/version"
)

const usage = `Usage: toolgate <command>

Commands:
  login     Sign in (login [--gateway URL])
  configure Save client settings in config.json (configure [--gateway URL] [--token-command CMD] [--attestation-token-file PATH] [--attestation-token-request DIR] [--attestation-key-file PATH] [--no-attestation])
  headers   Print the request headers for Claude Code's headersHelper
  otel-headers  Print the request headers for Claude Code's otelHeadersHelper
  hook      Run a Claude Code hook (hook session-start, stop or session-end)
  logout    Remove the stored sign-in token
  secret    Manage your secrets (secret set NAME | list | delete NAME)
  sessions  List your open sessions and their enabled servers
  servers   List the servers you can use
  workspace Manage which servers each workspace can use (workspace list | allow | deny | clear | defaults | register | keys)
  serve     Run the gateway (serve --config FILE)
  migrate   Apply the database migrations (migrate [--config FILE])
  admin     Admin commands (admin rewrap, admin volumes)
  audit     Print audit events as they happen (audit tail [--after KEY] [--json])
  status    Show the gateway and who is signed in
  version   Print the version
`

func main() {
	// The server commands stop when this context ends, so the process
	// stops cleanly on SIGTERM or SIGINT.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	code := runWithContext(ctx, os.Getenv, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)

	stop()
	os.Exit(code)
}

// run executes one command with no standard input and returns the process
// exit code.
func run(getenv func(string) string, args []string, stdout, stderr io.Writer) int {
	return runWithInput(getenv, args, strings.NewReader(""), stdout, stderr)
}

// runContext is run with a context that stops the server commands.
func runContext(ctx context.Context, getenv func(string) string, args []string, stdout, stderr io.Writer) int {
	return runWithContext(ctx, getenv, args, strings.NewReader(""), stdout, stderr)
}

// runWithInput executes one command and returns the process exit code.
func runWithInput(getenv func(string) string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runWithContext(context.Background(), getenv, args, stdin, stdout, stderr)
}

// runWithContext executes one command and returns the process exit code. The
// serve, migrate and admin commands stop when ctx ends.
func runWithContext(ctx context.Context, getenv func(string) string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)

		return 2
	}

	if args[0] != "version" && args[0] != "help" && args[0] != "-h" && args[0] != "--help" {
		if err := checkCredentialChoice(getenv); err != nil {
			_, _ = fmt.Fprintln(stderr, err)

			return 1
		}
	}

	switch args[0] {
	case "version":
		_, _ = fmt.Fprintln(stdout, version.Get())

		return 0
	case "login":
		return signIn(getenv, args[1:], stdout, stderr)
	case "configure":
		return configure(getenv, args[1:], stdout, stderr)
	case "status":
		return status(getenv, stdout, stderr)
	case "headers":
		return printHeaders(getenv, args[1:], stdout, stderr)
	case "otel-headers":
		return printOtelHeaders(getenv, args[1:], stdout, stderr)
	case "hook":
		return hook(getenv, args[1:], stdin, stdout, stderr)
	case "logout":
		return logout(getenv, stdout, stderr)
	case "secret":
		return secret(getenv, args[1:], stdin, stdout, stderr)
	case "servers":
		return servers(getenv, args[1:], stdout, stderr)
	case "workspace":
		return workspace(getenv, args[1:], stdout, stderr)
	case "sessions":
		return sessions(getenv, args[1:], stdout, stderr)
	case "serve":
		return serve(ctx, getenv, args[1:], stdout, stderr)
	case "migrate":
		return migrate(ctx, getenv, args[1:], stdout, stderr)
	case "audit":
		return auditCommand(getenv, args[1:], stdout, stderr)
	case "admin":
		return admin(ctx, getenv, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)

		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "toolgate: unknown command %q\n\n%s", args[0], usage)

		return 2
	}
}

// checkCredentialChoice refuses a setup that names two sources for the user
// token.
func checkCredentialChoice(getenv func(string) string) error {
	if spiffe.Socket(getenv) != "" && tokenCommandSource(getenv).Command != "" {
		return fmt.Errorf("Set only one of %s and %s.", spiffe.SocketEnv, tokencommand.Env) //nolint:staticcheck // user-facing sentence
	}

	source := attestationSource(getenv)
	if err := source.Validate(); err != nil {
		return err
	}

	if spiffe.Socket(getenv) != "" && source.Configured() {
		return fmt.Errorf("Set only one of %s and %s.", spiffe.SocketEnv, strings.Join(source.Names(), " and ")) //nolint:staticcheck // user-facing sentence
	}

	return nil
}
