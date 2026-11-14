// Package operations lists every operation of the gateway's `/api/v1/` API.
package operations

import (
	"github.com/solidDoWant/toolgate/internal/apidoc"
	"github.com/solidDoWant/toolgate/internal/audittail"
	"github.com/solidDoWant/toolgate/internal/secrets"
	"github.com/solidDoWant/toolgate/internal/sessionsapi"
	"github.com/solidDoWant/toolgate/internal/settings"
	"github.com/solidDoWant/toolgate/internal/volumesapi"
	"github.com/solidDoWant/toolgate/internal/workspacesapi"
)

// All is every `/api/v1/` operation, in document order.
func All() []apidoc.Operation {
	var operations []apidoc.Operation

	operations = append(operations, secrets.Operations...)
	operations = append(operations, settings.Operations...)
	operations = append(operations, sessionsapi.Operations...)
	operations = append(operations, audittail.Operations...)
	operations = append(operations, workspacesapi.Operations...)
	operations = append(operations, volumesapi.Operations...)

	return operations
}
