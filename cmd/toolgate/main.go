// Command toolgate is the toolgate MCP gateway (`toolgate serve`) and the
// client CLI used by the Claude Code plugin.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/solidDoWant/toolgate/internal/version"
)

const usage = `Usage: toolgate <command>

Commands:
  login     Sign in (login [--gateway URL])
  logout    Remove the stored sign-in token
  serve     Run the gateway (serve --config FILE)
  admin     Admin commands (admin rewrap --config FILE)
  status    Show the gateway and who is signed in
  version   Print the version
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes one command and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
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
	case "logout":
		return logout(stdout, stderr)
	case "serve":
		return serve(args[1:], stdout, stderr)
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
