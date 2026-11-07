// Command docgen writes the OpenAPI document of the gateway's `/api/v1/`
// routes: `docgen docs/dev/openapi.yaml`.
package main

import (
	"fmt"
	"os"

	"github.com/solidDoWant/toolgate/internal/apidoc"
	"github.com/solidDoWant/toolgate/internal/apidoc/operations"
)

func Operations() []apidoc.Operation { return operations.All() }

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: docgen OUTPUT.yaml")
		os.Exit(2)
	}

	document, err := apidoc.Build("toolgate gateway API", "v1", Operations())
	if err == nil {
		err = os.WriteFile(os.Args[1], document, 0o644) //nolint:gosec // A public document.
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "docgen:", err)
		os.Exit(1)
	}
}
