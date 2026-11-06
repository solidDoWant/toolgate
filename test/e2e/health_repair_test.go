//go:build e2e

package e2e

import (
	"net/http"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
)

const (
	healthInterval = 10 * time.Second
	healthTimeout  = time.Second
	// unhealthyBound is interval × failureThreshold plus one timeout
	// (docs/design/phase-3.md §6), plus time for the write and the fan-out.
	unhealthyBound = healthInterval + healthTimeout + 10*time.Second
)

// notReady matches the refusal of a call to an unhealthy or starting server.
var notReady = regexp.MustCompile("is unhealthy|isn't ready")

// createHealthTemplate creates a per-user test server template with a
// health check on the mcp Service's /healthz, failing after one failure.
func createHealthTemplate(t *testing.T, kube client.Client, prefix, onFailure string, maxRepairs int32, env map[string]string) string {
	t.Helper()

	name := createTemplate(t, kube, prefix, "per-user", testServerResources(t, mcpServerImage(t), 0, env))

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		var template v1alpha1.MCPServerTemplate
		require.NoError(collect, kube.Get(t.Context(), types.NamespacedName{Namespace: releaseNamespace(), Name: name}, &template))

		template.Spec.HealthCheck = &v1alpha1.HealthCheck{
			HTTP:             v1alpha1.HealthCheckHTTP{Service: "mcp", Port: 8080, Path: "/healthz", ExpectedStatus: 200},
			Interval:         metav1.Duration{Duration: healthInterval},
			Timeout:          metav1.Duration{Duration: healthTimeout},
			FailureThreshold: 1,
			SuccessThreshold: 1,
			OnFailure:        onFailure,
			MaxRepairs:       maxRepairs,
		}
		require.NoError(collect, kube.Update(t.Context(), &template))
	}, time.Minute, time.Second, "add the health check")

	requireTemplateValid(t, kube, name)

	return name
}

// setHealth makes the test server's /healthz answer status, through a
// port-forward to the instance's pod.
func setHealth(t *testing.T, admin *kubernetes.Clientset, namespace string, status int) {
	t.Helper()

	pods, err := admin.CoreV1().Pods(namespace).List(t.Context(), metav1.ListOptions{LabelSelector: "app=mcp"})
	require.NoError(t, err)
	require.NotEmpty(t, pods.Items, "the instance's pod")

	endpoint := forwardToPod(t, admin, namespace, pods.Items[0].Name, 8080)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPut, endpoint+"/healthz?status="+strconv.Itoa(status), nil)
	require.NoError(t, err)

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusNoContent, response.StatusCode)
}

// requireWithdrawn waits, within the detection bound, until the session no
// longer lists the server's tools, and checks that a call is refused. The
// refusal says the instance is unhealthy, or, once a replace repair started
// the next instance, that the server isn't ready yet.
func requireWithdrawn(t *testing.T, session *gatewaySession, name string) {
	t.Helper()

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		listed := listedTools(collect, t.Context(), session.session)
		for _, tool := range prefixedTools(name) {
			assert.NotContains(collect, listed, tool)
		}
	}, unhealthyBound, 250*time.Millisecond, "the tools are withdrawn within interval × failureThreshold + timeout")

	result, err := session.session.CallTool(t.Context(), &mcp.CallToolParams{Name: name + "__env", Arguments: map[string]any{}})
	if err == nil {
		require.True(t, result.IsError, "the call is refused")
		assert.Regexp(t, notReady, callText(result))
	} else {
		assert.Regexp(t, notReady, err.Error())
	}
}

func requireToolsBack(t *testing.T, session *gatewaySession, name string) {
	t.Helper()

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		assert.Subset(collect, listedTools(collect, t.Context(), session.session), prefixedTools(name))
	}, enableTimeout, time.Second, "the server's tools come back")
}

func TestAC3_13HealthCheckRepair(t *testing.T) {
	admin := adminClient(t)
	kube := templateClient(t)
	pods := gatewayPods(t, admin)

	t.Run("[AC-3-13] replace: the unhealthy instance is withdrawn, refused and replaced, and the tools come back", func(t *testing.T) {
		name := createHealthTemplate(t, kube, "e2e-health-replace", runtime.OnFailureReplace, 3, nil)
		session := connectGateway(t, forwardToGateway(t, admin, pods[0]), "alice", "e2e-"+rand.String(8))
		require.ElementsMatch(t, prefixedTools(name), enable(t, session, name))

		old := requireOneInstance(t, admin, name)
		setHealth(t, admin, old.Name, http.StatusServiceUnavailable)
		requireWithdrawn(t, session, name)

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			namespaces := instanceNamespaces(collect, t.Context(), admin, name)
			require.Len(collect, namespaces, 1)
			assert.NotEqual(collect, old.Name, namespaces[0].Name)
		}, enableTimeout, time.Second, "a new instance replaces the unhealthy one")

		requireToolsBack(t, session, name)
	})

	t.Run("[AC-3-13] reapply: deleted objects are applied again, the instance keeps its ID, and the tools come back once the check passes", func(t *testing.T) {
		name := createHealthTemplate(t, kube, "e2e-health-reapply", runtime.OnFailureReapply, 3, nil)
		session := connectGateway(t, forwardToGateway(t, admin, pods[0]), "alice", "e2e-"+rand.String(8))
		require.ElementsMatch(t, prefixedTools(name), enable(t, session, name))

		instance := requireOneInstance(t, admin, name)
		setHealth(t, admin, instance.Name, http.StatusServiceUnavailable)
		requireWithdrawn(t, session, name)

		// Once unhealthy, delete the Service: the reapply creates it again.
		require.NoError(t, admin.CoreV1().Services(instance.Name).Delete(t.Context(), "mcp", metav1.DeleteOptions{}))
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			_, err := admin.CoreV1().Services(instance.Name).Get(t.Context(), "mcp", metav1.GetOptions{})
			assert.NoError(collect, err)
		}, 3*healthInterval+time.Minute, time.Second, "the reapply applies the deleted Service again")

		setHealth(t, admin, instance.Name, http.StatusOK)
		requireToolsBack(t, session, name)
		assert.Equal(t, instance.Name, requireOneInstance(t, admin, name).Name, "the instance kept its ID")
	})

	t.Run("[AC-3-13] none: no repair; the tools stay withdrawn until the check passes", func(t *testing.T) {
		name := createHealthTemplate(t, kube, "e2e-health-none", runtime.OnFailureNone, 3, nil)
		session := connectGateway(t, forwardToGateway(t, admin, pods[0]), "alice", "e2e-"+rand.String(8))
		require.ElementsMatch(t, prefixedTools(name), enable(t, session, name))

		instance := requireOneInstance(t, admin, name)
		setHealth(t, admin, instance.Name, http.StatusServiceUnavailable)
		requireWithdrawn(t, session, name)

		// Two more checks fail, and nothing is repaired.
		assert.Never(t, func() bool {
			namespaces := instanceNamespaces(t, t.Context(), admin, name)

			return len(namespaces) != 1 || namespaces[0].Name != instance.Name
		}, 2*healthInterval, time.Second, "the instance is not replaced")
		assert.NotSubset(t, listedTools(t, t.Context(), session.session), prefixedTools(name))

		setHealth(t, admin, instance.Name, http.StatusOK)
		requireToolsBack(t, session, name)
	})

	t.Run("[AC-3-13] after maxRepairs repairs the instance fails and is torn down", func(t *testing.T) {
		// Every instance answers 503 from the start, so each replacement
		// turns unhealthy again.
		name := createHealthTemplate(t, kube, "e2e-health-giveup", runtime.OnFailureReplace, 1,
			map[string]string{"MCPSERVER_HEALTH": "503"})
		session := connectGateway(t, forwardToGateway(t, admin, pods[0]), "alice", "e2e-"+rand.String(8))

		// The first check runs as the instance becomes ready, so the enable
		// may return the tools or be refused as unhealthy.
		if result := awaitEnable(t, startEnable(t, session, name)); result.err != "" {
			require.Regexp(t, notReady, result.err)
		}

		first := requireOneInstance(t, admin, name)
		requireWithdrawn(t, session, name)

		replaced := false

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			namespaces := instanceNamespaces(collect, t.Context(), admin, name)
			for _, namespace := range namespaces {
				if namespace.Name != first.Name {
					replaced = true
				}
			}

			assert.True(collect, replaced, "one replacement")
			assert.Empty(collect, namespaces, "the replacement failed and was torn down")
		}, enableTimeout+3*unhealthyBound, time.Second)

		assert.NotSubset(t, listedTools(t, t.Context(), session.session), prefixedTools(name), "the tools stay withdrawn")
	})
}
