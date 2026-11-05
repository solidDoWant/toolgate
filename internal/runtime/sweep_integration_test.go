//go:build integration

package runtime_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// sweep runs the sweep as a new leader does, holding the Lease and reading
// namespaces through reader.
func (f *stopFixture) sweep(t *testing.T, reader client.Reader) {
	t.Helper()

	fence := &fakeFence{}
	fence.holds.Store(true)

	sweep := &runtime.Sweep{
		Pool:            f.pool,
		Reader:          reader,
		Deleter:         &runtime.Reconciler{Fence: fence, Client: f.kube},
		Templates:       f.catalog,
		SharedNamespace: sharedNamespace,
		Log:             slog.New(slog.NewTextHandler(f.logs, nil)),
	}
	require.NoError(t, sweep.Run(t.Context()))
}

func (f *stopFixture) namespaceExists(t *testing.T, name string) bool {
	t.Helper()

	var namespace corev1.Namespace

	err := f.kube.Get(t.Context(), types.NamespacedName{Name: name}, &namespace)
	if apierrors.IsNotFound(err) {
		return false
	}

	require.NoError(t, err)

	return namespace.DeletionTimestamp == nil
}

func (f *stopFixture) createNamespace(t *testing.T, name, instanceID string) {
	t.Helper()
	require.NoError(t, f.kube.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name, Labels: map[string]string{runtime.LabelInstance: instanceID, runtime.LabelManagedBy: runtime.ManagedByValue},
	}}))
}

// listHook is a reader that runs a function before each List, standing for
// a leader that applies objects while the sweep runs.
type listHook struct {
	client.Reader
	before func(ctx context.Context)
}

func (h listHook) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	h.before(ctx)

	return h.Reader.List(ctx, list, options...)
}

func TestRestartRecovery(t *testing.T) {
	fixture := newStopFixture(t)
	everyone := v1alpha1.Access{Groups: []string{"staff"}}

	t.Run("[row 17] labelled namespaces with no live instance row are deleted", func(t *testing.T) {
		template := fixture.template("orphans", "shared", 0, everyone)

		// A namespace whose row is gone, and one whose row is stopped.
		fixture.createNamespace(t, "tg-0000000000000000000000000000ffff", "0000000000000000000000000000ffff")
		stopped := fixture.instance(t, template, "shared", "", "failed")
		fixture.pass(t)
		fixture.pass(t)
		require.Equal(t, "stopped", fixture.get(t, stopped.ID).Status)
		fixture.createNamespace(t, stopped.Namespace, stopped.ID)

		// A live instance's namespace, and a namespace that isn't an
		// instance's.
		live := fixture.instance(t, fixture.template("orphans-live", "shared", 0, everyone), "shared", "", "ready")
		require.NoError(t, fixture.kube.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: "shared-instances", Labels: map[string]string{runtime.LabelManagedBy: runtime.ManagedByValue},
		}}))

		fixture.sweep(t, fixture.kube)

		assert.False(t, fixture.namespaceExists(t, "tg-0000000000000000000000000000ffff"), "a namespace without a row is deleted")
		assert.False(t, fixture.namespaceExists(t, stopped.Namespace), "a namespace of a stopped row is deleted")
		assert.True(t, fixture.namespaceExists(t, live.Namespace), "a live instance's namespace stays")
		assert.True(t, fixture.namespaceExists(t, "shared-instances"), "a namespace without the instance label stays")
		fixture.requireRunning(t, live)
	})

	t.Run("[row 17] orphaned objects in the shared namespace are deleted, and the namespace stays", func(t *testing.T) {
		live := fixture.sharedInstance(t, fixture.template("shared-sweep-live", "shared", 0, everyone), "ready")
		fixture.createSharedObjects(t, "0000000000000000000000000000eeee")

		// The shared namespace is kept even if someone labelled it like an
		// instance namespace.
		var namespace corev1.Namespace
		require.NoError(t, fixture.kube.Get(t.Context(), types.NamespacedName{Name: sharedNamespace}, &namespace))
		namespace.Labels[runtime.LabelInstance] = "0000000000000000000000000000eeee"
		require.NoError(t, fixture.kube.Update(t.Context(), &namespace))

		fixture.sweep(t, fixture.kube)

		assert.ElementsMatch(t, []string{"ConfigMap/settings-" + live.ID, "Service/server-" + live.ID}, fixture.sharedObjectNames(t),
			"the orphan's objects are deleted and the live instance's stay")
		assert.True(t, fixture.namespaceExists(t, sharedNamespace), "the shared namespace is never deleted")
		fixture.requireRunning(t, live)
	})

	t.Run("[row 17] the sweep lists namespaces before it reads rows", func(t *testing.T) {
		template := fixture.template("applied-during-sweep", "shared", 0, everyone)

		var created string

		reader := listHook{Reader: fixture.kube, before: func(ctx context.Context) {
			if created != "" {
				return
			}

			// Between the list and the read, a pending row appears whose
			// namespace is already applied.
			instance := fixture.instance(t, template, "shared", "", "pending")
			created = instance.Namespace
		}}

		fixture.sweep(t, reader)

		require.NotEmpty(t, created)
		assert.True(t, fixture.namespaceExists(t, created), "a namespace applied before its row was read isn't an orphan")
	})

	t.Run("[row 17] starting and ready rows without a namespace go back to pending at the current generation", func(t *testing.T) {
		template := fixture.template("lost-objects", "shared", 0, everyone)
		starting := fixture.instance(t, template, "shared", "", "starting")
		other := fixture.template("lost-objects-ready", "shared", 0, everyone)
		ready := fixture.instance(t, other, "shared", "", "ready")

		for _, instance := range []string{starting.Namespace, ready.Namespace} {
			require.NoError(t, fixture.kube.Delete(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instance}}))
		}

		changed := template.DeepCopy()
		changed.Generation = 2
		fixture.catalog.Set(changed, nil)

		fixture.sweep(t, fixture.kube)

		current := fixture.get(t, starting.ID)
		assert.Equal(t, "pending", current.Status)
		assert.Equal(t, "running", current.Desired)
		assert.Equal(t, int64(2), current.Generation, "the instance is rendered again at the template's current generation")

		current = fixture.get(t, ready.ID)
		assert.Equal(t, "pending", current.Status)
		assert.Equal(t, int64(1), current.Generation)
	})

	t.Run("[row 17] live rows with their namespace and pending rows without one are left alone", func(t *testing.T) {
		template := fixture.template("intact", "shared", 0, everyone)
		ready := fixture.instance(t, template, "shared", "", "ready")
		other := fixture.template("intact-pending", "shared", 0, everyone)
		pending := fixture.instance(t, other, "shared", "", "pending")
		require.NoError(t, fixture.kube.Delete(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: pending.Namespace}}))

		fixture.sweep(t, fixture.kube)

		fixture.requireRunning(t, ready)
		assert.Equal(t, "pending", fixture.get(t, pending.ID).Status)
	})

	t.Run("[row 16] a stopping instance whose namespace the old leader didn't delete is deleted by the new leader", func(t *testing.T) {
		template := fixture.template("stopping-handover", "shared", 0, everyone)
		instance := fixture.instance(t, template, "shared", "", "ready")
		_, err := fixture.queries.SetInstanceDesiredStopped(t.Context(), instance.ID)
		require.NoError(t, err)
		_, err = fixture.queries.SetInstanceStopping(t.Context(), db.SetInstanceStoppingParams{ID: instance.ID, FromStatus: "ready"})
		require.NoError(t, err)

		fixture.sweep(t, fixture.kube)
		fixture.pass(t)

		assert.False(t, fixture.namespaceExists(t, instance.Namespace))
		assert.Equal(t, "stopped", fixture.get(t, instance.ID).Status)
	})
}
