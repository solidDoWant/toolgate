//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/rand"
)

func TestRenderedVariables(t *testing.T) {
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)

	resources := testServerResources(t, image, 0, map[string]string{
		"TG_EMAIL":     "${principal.email}",
		"TG_WORKSPACE": "${workspace.id}",
		"TG_SESSION":   "${session.id}",
		"TG_INSTANCE":  "${instance.id}",
	})
	name := createTemplate(t, kube, "e2e-variables", "per-session", resources)
	requireTemplateValid(t, kube, name)

	pods := gatewayPods(t, admin)
	sessionID := "e2e-" + rand.String(8)
	workspace := "e2e-workspace-" + rand.String(6)
	session := connectGatewayInWorkspace(t, forwardToGateway(t, admin, pods[0]), "alice", sessionID, workspace)

	t.Run("the instance's environment holds the session's rendered identity", func(t *testing.T) {
		assert.ElementsMatch(t, prefixedTools(name), enable(t, session, name))

		namespace := requireOneInstance(t, admin, name)
		instanceID, found := strings.CutPrefix(namespace.Name, "tg-")
		require.True(t, found, "the instance namespace %s is tg- plus the instance ID", namespace.Name)
		require.NotEmpty(t, instanceID)

		var environment map[string]string
		structuredResult(t, session.session, name+"__env", &environment)

		assert.Equal(t, "alice@example.com", environment["TG_EMAIL"], "the verified email")
		assert.Equal(t, "claimed/"+workspace, environment["TG_WORKSPACE"], "the claimed workspace")
		assert.Equal(t, sessionID, environment["TG_SESSION"], "the session ID")
		assert.Equal(t, instanceID, environment["TG_INSTANCE"], "the instance ID")
	})
}
