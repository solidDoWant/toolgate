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
  serve     Run the gateway (not implemented yet)
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
	case "serve":
		_, _ = fmt.Fprintln(stderr, "toolgate serve: not implemented yet")

		return 1
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)

		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "toolgate: unknown command %q\n\n%s", args[0], usage)

		return 2
	}
}
