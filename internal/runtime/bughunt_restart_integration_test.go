//go:build integration

package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
)

// Row 17: a ready instance whose objects are gone after a gateway restart
// goes back to pending and is started again. If it had been running for
// longer than startTimeout, it fails at once instead.
func TestBughuntRow17RestartOfLongReadyInstanceFailsAtOnce(t *testing.T) {
	fixture := newStopFixture(t)
	template := fixture.template("long-ready", "shared", 0, v1alpha1.Access{Groups: []string{"staff"}})
	instance := fixture.instance(t, template, "shared", "", "ready")

	// It has been ready for an hour (startTimeout defaults to 5 minutes).
	_, err := fixture.pool.Exec(t.Context(), `UPDATE instances SET attempt_started_at = now() - interval '1 hour', ready_at = now() - interval '55 minutes' WHERE id = $1`, instance.ID)
	require.NoError(t, err)

	// Its namespace is gone (deleted while the gateway was down).
	require.NoError(t, fixture.kube.Delete(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instance.Namespace}}))

	fixture.sweep(t, fixture.kube)
	require.Equal(t, "pending", fixture.get(t, instance.ID).Status)

	scheme := k8sruntime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	kube := clientfake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{Apply: fakeApply}).WithReturnManagedFields().Build()

	starter := &runtime.Starter{
		Store:    runtime.PostgresStartStore{Pool: fixture.pool},
		Client:   kube,
		Template: fixture.catalog.Running,
		Secrets: runtime.BrokerSecrets(func(context.Context, string, string) ([]byte, time.Time, error) {
			return nil, time.Time{}, nil
		}),
		ListTools: func(context.Context, string) ([]*mcp.Tool, error) {
			return []*mcp.Tool{{Name: "search", InputSchema: map[string]any{"type": "object"}}}, nil
		},
		GatewayNamespace: "toolgate",
		GatewayPodLabels: map[string]string{"app.kubernetes.io/name": "toolgate"},
	}

	for range 2 {
		require.NoError(t, starter.Step(t.Context(), fixture.get(t, instance.ID)))
	}

	current := fixture.get(t, instance.ID)
	t.Logf("after the restart: status=%s reason=%q", current.Status, current.Reason)
	assert.Equal(t, "ready", current.Status, "the instance should be started again, not failed")
}
