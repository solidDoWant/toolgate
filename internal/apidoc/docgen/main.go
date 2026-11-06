// Command docgen writes the OpenAPI document of the gateway's `/api/v1/`
// routes: `docgen docs/dev/openapi.yaml`.
package main

import (
	"fmt"
	"os"

	"github.com/solidDoWant/toolgate/internal/apidoc"
	"github.com/solidDoWant/toolgate/internal/audittail"
	"github.com/solidDoWant/toolgate/internal/secrets"
	"github.com/solidDoWant/toolgate/internal/sessionsapi"
	"github.com/solidDoWant/toolgate/internal/settings"
	"github.com/solidDoWant/toolgate/internal/workspacesapi"
)

// Operations is every `/api/v1/` operation, in document order.
func Operations() []apidoc.Operation {
	var operations []apidoc.Operation

	operations = append(operations, secrets.Operations...)
	operations = append(operations, settings.Operations...)
	operations = append(operations, sessionsapi.Operations...)
	operations = append(operations, audittail.Operations...)
	operations = append(operations, workspacesapi.Operations...)

	return operations
}

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
