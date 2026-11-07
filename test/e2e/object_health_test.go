//go:build e2e

package e2e

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"

	"github.com/solidDoWant/toolgate/internal/runtime"
)

// objectFailureBound is how long an object's Ready condition must be False
// before it counts as failed, plus one reconciler pass, both scaled, plus
// time for Flux to reconcile the change, the write and the fan-out.
func objectFailureBound() time.Duration {
	return scaled(runtime.ReadyFailureHold) + scaled(runtime.DefaultPassInterval) + 20*time.Second
}

// gatewayQuery runs SQL against the gateway's database, through psql in
// the harness's PostgreSQL pod, and returns its output.
func gatewayQuery(t *testing.T, admin *kubernetes.Clientset, sql string) string {
	t.Helper()

	secret, err := admin.CoreV1().Secrets(releaseNamespace()).Get(t.Context(), "toolgate-database", metav1.GetOptions{})
	require.NoError(t, err)

	database, err := url.Parse(string(secret.Data["url"]))
	require.NoError(t, err)

	hostParts := strings.Split(database.Hostname(), ".")
	require.GreaterOrEqual(t, len(hostParts), 2, "the host is postgres.<namespace>.svc")

	output, err := psql(t, hostParts[1], database.User.Username(), strings.TrimPrefix(database.Path, "/"), sql)
	require.NoError(t, err, output)

	return output
}

func TestObjectHealthAfterReady(t *testing.T) {
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)

	values := map[string]any{"image": image, "tools": strings.Join(testServerTools, ",")}
	name := createTemplate(t, kube, "e2e-objhealth", "per-user", helmReleaseResources(t, values))
	requireTemplateValid(t, kube, name)

	pods := gatewayPods(t, admin)
	session := connectGateway(t, forwardToGateway(t, admin, pods[0]), "alice", "e2e-"+rand.String(8))

	t.Run("an OCIRepository that turns Ready=False after ready withdraws the tools with its reason, and the instance is repaired", func(t *testing.T) {
		require.ElementsMatch(t, prefixedTools(name), enable(t, session, name))

		old := requireOneInstance(t, admin, name)
		instanceID := strings.TrimPrefix(old.Name, runtime.NamespacePrefix)

		// Point the source at a tag that doesn't exist: its Ready condition
		// turns False at the new generation.
		_, err := admin.RESTClient().Patch(types.MergePatchType).
			AbsPath("/apis/source.toolkit.fluxcd.io/v1/namespaces", old.Name, "ocirepositories", "mcpserver").
			Body([]byte(`{"spec":{"ref":{"tag":"9.9.9-missing"}}}`)).
			DoRaw(t.Context())
		require.NoError(t, err)

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			listed := listedTools(collect, t.Context(), session.session)
			for _, tool := range prefixedTools(name) {
				assert.NotContains(collect, listed, tool)
			}
		}, objectFailureBound(), time.Second, "the tools are withdrawn within the Ready=False hold plus one pass")

		reason := gatewayQuery(t, admin, "SELECT details->>'reason' FROM audit_events WHERE type = 'instance.health' "+
			"AND details->>'instance' = '"+instanceID+"' AND details->>'healthy' = 'false'")
		assert.True(t, strings.HasPrefix(reason, "OCIRepository mcpserver failed: "),
			"the instance is unhealthy with the OCIRepository's reason, got %q", reason)

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			namespaces := instanceNamespaces(collect, t.Context(), admin, name)
			require.Len(collect, namespaces, 1)
			assert.NotEqual(collect, old.Name, namespaces[0].Name)
		}, enableTimeout, time.Second, "a new instance replaces the unhealthy one")

		requireToolsBack(t, session, name)
	})
}
