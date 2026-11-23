//go:build integration

package runtime_test

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
)

const (
	ownInstall   = "11111111111111111111111111111111"
	otherInstall = "22222222222222222222222222222222"
)

// sweepAs runs the sweep as the install with the given ID.
func (f *stopFixture) sweepAs(t *testing.T, installID string) {
	t.Helper()

	fence := &fakeFence{}
	fence.holds.Store(true)

	sweep := &runtime.Sweep{
		InstallID:       installID,
		Pool:            f.pool,
		Reader:          f.kube,
		Writer:          f.kube,
		Deleter:         &runtime.Reconciler{Fence: fence, Client: f.kube},
		Templates:       f.catalog,
		SharedNamespace: sharedNamespace,
		Log:             slog.New(slog.NewTextHandler(f.logs, nil)),
	}
	require.NoError(t, sweep.Run(t.Context()))
}

func (f *stopFixture) createInstallNamespace(t *testing.T, name, instanceID, installID string) {
	t.Helper()

	labels := map[string]string{runtime.LabelInstance: instanceID, runtime.LabelManagedBy: runtime.ManagedByValue}
	if installID != "" {
		labels[runtime.LabelInstall] = installID
	}

	require.NoError(t, f.kube.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}))
}

func (f *stopFixture) installLabel(t *testing.T, namespace string) string {
	t.Helper()

	var found corev1.Namespace
	require.NoError(t, f.kube.Get(t.Context(), types.NamespacedName{Name: namespace}, &found))

	return found.Labels[runtime.LabelInstall]
}

func TestSweepTouchesOnlyItsOwnInstallsObjects(t *testing.T) {
	t.Parallel()

	fixture := newStopFixture(t)
	everyone := v1alpha1.Access{Groups: []string{"staff"}}

	t.Run("own, other, unlabeled-known and unlabeled-unknown namespaces", func(t *testing.T) {
		live := fixture.instance(t, fixture.template("install-live", "shared", 0, everyone), "shared", "", "ready")

		const (
			ownOrphan     = "0000000000000000000000000000a001"
			otherOrphan   = "0000000000000000000000000000a002"
			unlabeledLost = "0000000000000000000000000000a003"
		)

		fixture.createInstallNamespace(t, "tg-"+ownOrphan, ownOrphan, ownInstall)
		fixture.createInstallNamespace(t, "tg-"+otherOrphan, otherOrphan, otherInstall)
		fixture.createInstallNamespace(t, "tg-"+unlabeledLost, unlabeledLost, "")

		fixture.sweepAs(t, ownInstall)

		assert.False(t, fixture.namespaceExists(t, "tg-"+ownOrphan), "an own namespace with no row is deleted")
		assert.True(t, fixture.namespaceExists(t, "tg-"+otherOrphan), "another install's namespace stays")
		assert.Equal(t, otherInstall, fixture.installLabel(t, "tg-"+otherOrphan), "another install's namespace is not relabelled")
		assert.True(t, fixture.namespaceExists(t, "tg-"+unlabeledLost), "an unlabeled namespace no row names is never deleted")
		assert.Empty(t, fixture.installLabel(t, "tg-"+unlabeledLost), "an unlabeled namespace no row names is not labelled")
		assert.True(t, fixture.namespaceExists(t, live.Namespace), "a live instance's namespace stays")
		fixture.requireRunning(t, live)

		// The other install, with its own rows elsewhere, still sees nothing of ours.
		fixture.sweepAs(t, otherInstall)
		assert.False(t, fixture.namespaceExists(t, "tg-"+otherOrphan), "the other install deletes its own orphan")
		assert.True(t, fixture.namespaceExists(t, live.Namespace), "a namespace labelled for another install stays")
	})

	t.Run("shared namespace objects follow the same rules", func(t *testing.T) {
		live := fixture.sharedInstance(t, fixture.template("install-shared-live", "shared", 0, everyone), "ready")
		fixture.createSharedObjects(t, "0000000000000000000000000000b001")

		fixture.sweepAs(t, ownInstall)

		names := fixture.sharedObjectNames(t)
		assert.Contains(t, names, "ConfigMap/settings-"+live.ID, "an unlabeled object with a row stays")

		var adopted corev1.ConfigMap
		require.NoError(t, fixture.kube.Get(t.Context(), types.NamespacedName{Namespace: sharedNamespace, Name: "settings-" + live.ID}, &adopted))
		assert.Equal(t, ownInstall, adopted.Labels[runtime.LabelInstall], "an unlabeled object with a row is adopted")
		assert.Contains(t, names, "ConfigMap/settings-0000000000000000000000000000b001", "an unlabeled object with no row is never deleted")
	})
}
