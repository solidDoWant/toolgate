//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/solidDoWant/toolgate/internal/runtime"
)

// leaderPod returns the name of the gateway pod holding the runtime Lease,
// or "" when nobody holds it. The holder identity is the pod's hostname, an
// underscore and a UUID.
func leaderPod(ctx context.Context, admin *kubernetes.Clientset) (string, error) {
	lease, err := admin.CoordinationV1().Leases(releaseNamespace()).Get(ctx, runtime.LeaseName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	holder := ptr.Deref(lease.Spec.HolderIdentity, "")
	pod, _, _ := strings.Cut(holder, "_")

	return pod, nil
}

// orphanNamespace creates an instance namespace that no instance row
// belongs to.
func orphanNamespace(t *testing.T, admin *kubernetes.Clientset, id string) string {
	t.Helper()

	name := runtime.NamespacePrefix + id
	_, err := admin.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{runtime.LabelManagedBy: runtime.ManagedByValue, runtime.LabelInstance: id},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = admin.CoreV1().Namespaces().Delete(context.WithoutCancel(t.Context()), name, metav1.DeleteOptions{})
	})

	return name
}

func requireNamespaceGone(t *testing.T, admin *kubernetes.Clientset, name, why string) {
	t.Helper()

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		namespace, err := admin.CoreV1().Namespaces().Get(t.Context(), name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return
		}

		require.NoError(collect, err)
		assert.NotNil(collect, namespace.DeletionTimestamp, why)
	}, 3*time.Minute, 2*time.Second)
}

func TestLeaderChangeRecovery(t *testing.T) {
	admin := adminClient(t)

	var leader string

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		var err error

		leader, err = leaderPod(t.Context(), admin)
		require.NoError(collect, err)
		assert.NotEmpty(collect, leader, "a gateway replica holds the runtime Lease")
	}, 3*time.Minute, 2*time.Second)

	t.Run("[row 17] the leader deletes an instance namespace with no instance row", func(t *testing.T) {
		name := orphanNamespace(t, admin, "00000000000000000000000000e2e017")
		requireNamespaceGone(t, admin, name, "the leader's sweep deletes the orphaned namespace")
	})

	t.Run("[row 17] after the leader is killed the new leader deletes orphans", func(t *testing.T) {
		require.NoError(t, admin.CoreV1().Pods(releaseNamespace()).Delete(t.Context(), leader,
			metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}))

		// Created after the old leader was killed, so only a new leader can
		// delete it.
		name := orphanNamespace(t, admin, "00000000000000000000000000e2e117")

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			current, err := leaderPod(t.Context(), admin)
			require.NoError(collect, err)
			assert.NotEmpty(collect, current)
			assert.NotEqual(collect, leader, current, "another replica takes the Lease")
		}, 3*time.Minute, 2*time.Second)

		requireNamespaceGone(t, admin, name, "the new leader's sweep deletes the orphaned namespace")
	})
}
