// Command toolgate is the toolgate MCP gateway (`toolgate serve`) and the
// client CLI used by the Claude Code plugin.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/solidDoWant/toolgate/internal/version"
)

const usage = `Usage: toolgate <command>

Commands:
  login     Sign in (login [--gateway URL])
  headers   Print the request headers for Claude Code's headersHelper
  otel-headers  Print the request headers for Claude Code's otelHeadersHelper
  hook      Run a Claude Code hook (hook session-start)
  logout    Remove the stored sign-in token
  secret    Manage your secrets (secret set NAME | list | delete NAME)
  sessions  List your open sessions and their enabled servers
  servers   List the servers you can use
  serve     Run the gateway (serve --config FILE)
  migrate   Apply the database migrations (migrate [--config FILE])
  admin     Admin commands (admin rewrap --config FILE)
  audit     Print audit events as they happen (audit tail [--after KEY] [--json])
  status    Show the gateway and who is signed in
  version   Print the version
`

func main() {
	os.Exit(runWithInput(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run executes one command with no standard input and returns the process
// exit code.
func run(args []string, stdout, stderr io.Writer) int {
	return runWithInput(args, strings.NewReader(""), stdout, stderr)
}

// runWithInput executes one command and returns the process exit code.
func runWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)

		return 2
	}

	switch args[0] {
	case "version":
		_, _ = fmt.Fprintln(stdout, version.Get())

		return 0
	case "login":
		return signIn(args[1:], stdout, stderr)
	case "status":
		return status(stdout, stderr)
	case "headers":
		return printHeaders(args[1:], stdout, stderr)
	case "otel-headers":
		return printOtelHeaders(args[1:], stdout, stderr)
	case "hook":
		return hook(args[1:], stdin, stdout, stderr)
	case "logout":
		return logout(stdout, stderr)
	case "secret":
		return secret(args[1:], stdin, stdout, stderr)
	case "servers":
		return servers(args[1:], stdout, stderr)
	case "sessions":
		return sessions(args[1:], stdout, stderr)
	case "serve":
		return serve(args[1:], stdout, stderr)
	case "migrate":
		return migrate(args[1:], stdout, stderr)
	case "audit":
		return auditCommand(args[1:], stdout, stderr)
	case "admin":
		return admin(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)

		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "toolgate: unknown command %q\n\n%s", args[0], usage)

		return 2
	}
}
