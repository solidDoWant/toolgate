//go:build integration

package runtime_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// updateFixture is a stop fixture with a source client for the template
// secrets namespace and an updater over it.
type updateFixture struct {
	*stopFixture

	source  client.Client
	updater *runtime.TemplateSecretUpdater
	logs    *bytes.Buffer
	patches int
}

func newUpdateFixture(t *testing.T) *updateFixture {
	t.Helper()

	fixture := &updateFixture{stopFixture: newStopFixture(t), logs: &bytes.Buffer{}}
	fixture.source = templateSecretsKube(t, interceptor.Funcs{})
	fixture.updater = &runtime.TemplateSecretUpdater{
		Queries:   fixture.queries,
		Source:    fixture.source,
		Namespace: templateSecretsNamespace,
		Patcher:   countingPatcher{Client: fixture.kube, patches: &fixture.patches},
		Metrics:   runtime.NewMetrics(),
		Log:       slog.New(slog.NewTextHandler(fixture.logs, nil)),
	}

	return fixture
}

type countingPatcher struct {
	client.Client

	patches *int
}

func (c countingPatcher) Patch(ctx context.Context, object client.Object, patch client.Patch, options ...client.PatchOption) error {
	*c.patches++

	return c.Client.Patch(ctx, object, patch, options...)
}

func (f *updateFixture) setSource(t *testing.T, data map[string][]byte) string {
	t.Helper()

	secret := sourceSecret(data)
	existing := &corev1.Secret{}

	err := f.source.Get(t.Context(), client.ObjectKeyFromObject(secret), existing)
	if err == nil {
		existing.Data = data
		require.NoError(t, f.source.Update(t.Context(), existing))

		return existing.ResourceVersion
	}

	require.NoError(t, f.source.Create(t.Context(), secret))

	return secret.ResourceVersion
}

// liveInstance creates an instance with a target Secret and one row for each
// field in fields, recorded at version. The row of field "data" or
// "stringData" targets the key "api-key-<field>".
func (f *updateFixture) liveInstance(t *testing.T, name, status, version string, fields ...string) db.Instance {
	t.Helper()

	template := f.template(name, "shared", time.Hour, v1alpha1.Access{Groups: []string{"staff"}, Environments: "any"})
	instance := f.instance(t, template, "shared", "owner-"+name, status)

	require.NoError(t, f.kube.Create(t.Context(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: instance.Namespace},
		Data:       map[string][]byte{"api-key-data": []byte("rendered"), "api-key-stringData": []byte("rendered")},
	}))

	for _, field := range fields {
		require.NoError(t, f.queries.InsertInstanceTemplateSecret(t.Context(), db.InsertInstanceTemplateSecretParams{
			InstanceID: instance.ID, TargetSecret: "credentials", TargetKey: "api-key-" + field, TargetField: field,
			SourceNamespace: templateSecretsNamespace, SourceName: "keys", SourceKey: "api-key", SourceVersion: version,
		}))
	}

	return instance
}

func (f *updateFixture) targetValue(t *testing.T, instance db.Instance, field string) string {
	t.Helper()

	secret := &corev1.Secret{}
	require.NoError(t, f.kube.Get(t.Context(), client.ObjectKey{Namespace: instance.Namespace, Name: "credentials"}, secret))

	return string(secret.Data["api-key-"+field])
}

func (f *updateFixture) row(t *testing.T, instance db.Instance, field string) db.InstanceTemplateSecret {
	t.Helper()

	rows, err := f.queries.ListInstanceTemplateSecrets(t.Context(), instance.ID)
	require.NoError(t, err)

	for _, row := range rows {
		if row.TargetKey == "api-key-"+field {
			return row
		}
	}

	require.FailNow(t, "no row for "+field)

	return db.InstanceTemplateSecret{}
}

func (f *updateFixture) step(t *testing.T, instance db.Instance) {
	t.Helper()

	require.NoError(t, f.updater.Step(t.Context(), f.get(t, instance.ID)))
}

func (f *updateFixture) outcome(outcome string) float64 {
	return promtestutil.ToFloat64(f.updater.Metrics.TemplateSecretUpdates.WithLabelValues(outcome))
}

func TestTemplateSecretChangeUpdatesRunningInstances(t *testing.T) {
	fixture := newUpdateFixture(t)
	version := fixture.setSource(t, map[string][]byte{"api-key": []byte("v1")})

	ready := fixture.liveInstance(t, "update-ready", "ready", version, "stringData")
	starting := fixture.liveInstance(t, "update-starting", "starting", version, "stringData")

	fixture.step(t, ready)
	assert.Zero(t, fixture.patches, "an unchanged source patches nothing")

	newVersion := fixture.setSource(t, map[string][]byte{"api-key": []byte("v2")})
	require.NotEqual(t, version, newVersion)

	fixture.step(t, ready)
	fixture.step(t, starting)

	for _, instance := range []db.Instance{ready, starting} {
		assert.Equal(t, "v2", fixture.targetValue(t, instance, "stringData"))
		assert.Equal(t, newVersion, fixture.row(t, instance, "stringData").SourceVersion)
	}

	assert.Equal(t, 2, fixture.patches, "each instance gets its own patch")
	assert.InDelta(t, 2, fixture.outcome("updated"), 0)

	fixture.step(t, ready)
	assert.Equal(t, 2, fixture.patches, "a recorded version is not patched again")
}

func TestTemplateSecretUpdateIsLevelBased(t *testing.T) {
	fixture := newUpdateFixture(t)
	rendered := fixture.setSource(t, map[string][]byte{"api-key": []byte("v1")})

	// The render read v1, then the source changed twice before any pass.
	instance := fixture.liveInstance(t, "update-level", "ready", rendered, "stringData")
	fixture.setSource(t, map[string][]byte{"api-key": []byte("v2")})
	latest := fixture.setSource(t, map[string][]byte{"api-key": []byte("v3")})

	fixture.step(t, instance)

	assert.Equal(t, "v3", fixture.targetValue(t, instance, "stringData"), "the pass writes the latest value only")
	assert.Equal(t, latest, fixture.row(t, instance, "stringData").SourceVersion)
	assert.Equal(t, 1, fixture.patches)

	// The render recorded a version that is already the current one.
	current := fixture.liveInstance(t, "update-level-current", "ready", latest, "stringData")
	fixture.step(t, current)
	assert.Equal(t, 1, fixture.patches)
	assert.Equal(t, "rendered", fixture.targetValue(t, current, "stringData"))
}

func TestTemplateSecretDeletedKeepsCopies(t *testing.T) {
	fixture := newUpdateFixture(t)
	fixture.setSource(t, map[string][]byte{"api-key": []byte("v0")})
	// The fake API server numbers a new object from 999 again, so the
	// rendered version is one that a created Secret won't have.
	version := fixture.setSource(t, map[string][]byte{"api-key": []byte("v1")})
	instance := fixture.liveInstance(t, "update-deleted", "ready", version, "stringData")

	fixture.setSource(t, map[string][]byte{"other": []byte("x")})
	fixture.step(t, instance)
	fixture.step(t, instance)

	assert.Equal(t, "rendered", fixture.targetValue(t, instance, "stringData"), "a removed key keeps the copy")
	assert.Equal(t, "Secret `keys` has no key `api-key`", fixture.row(t, instance, "stringData").LastError.String)

	require.NoError(t, fixture.source.Delete(t.Context(), sourceSecret(nil)))
	fixture.step(t, instance)
	fixture.step(t, instance)

	assert.Equal(t, "rendered", fixture.targetValue(t, instance, "stringData"), "a deleted Secret keeps the copy")
	assert.Equal(t, "Secret `keys` was deleted", fixture.row(t, instance, "stringData").LastError.String)
	assert.Equal(t, 2, bytes.Count(fixture.logs.Bytes(), []byte("template secret copy kept")), "a warning is logged when the error changes, not on each pass")
	assert.InDelta(t, 4, fixture.outcome("missing"), 0)

	recreated := fixture.setSource(t, map[string][]byte{"api-key": []byte("v9")})
	fixture.step(t, instance)

	assert.Equal(t, "v9", fixture.targetValue(t, instance, "stringData"))
	row := fixture.row(t, instance, "stringData")
	assert.Equal(t, recreated, row.SourceVersion)
	assert.False(t, row.LastError.Valid, "the update clears the error")
}

func TestTemplateSecretUpdateIgnoresTemplateChanges(t *testing.T) {
	fixture := newUpdateFixture(t)
	version := fixture.setSource(t, map[string][]byte{"api-key": []byte("v1")})
	instance := fixture.liveInstance(t, "update-template-change", "ready", version, "stringData")

	changed := fixture.template("update-template-change", "shared", time.Hour, v1alpha1.Access{Groups: []string{"staff"}, Environments: "any"})
	changed.Generation = 5
	fixture.catalog.Set(changed, nil)

	fixture.setSource(t, map[string][]byte{"api-key": []byte("v2")})
	fixture.step(t, instance)

	assert.Equal(t, "v2", fixture.targetValue(t, instance, "stringData"), "an instance of an older generation keeps getting updates")
}

func TestTemplateSecretUpdateSkipsStoppedInstances(t *testing.T) {
	fixture := newUpdateFixture(t)
	version := fixture.setSource(t, map[string][]byte{"api-key": []byte("v1")})

	failed := fixture.liveInstance(t, "update-failed", "failed", version, "stringData")
	stopped := fixture.liveInstance(t, "update-stopped", "ready", version, "stringData")
	_, err := fixture.pool.Exec(t.Context(), "UPDATE instances SET desired = 'stopped' WHERE id = $1", stopped.ID)
	require.NoError(t, err)

	pending := fixture.liveInstance(t, "update-pending", "pending", version, "stringData")

	fixture.setSource(t, map[string][]byte{"api-key": []byte("v2")})

	for _, instance := range []db.Instance{failed, stopped, pending} {
		fixture.step(t, instance)
		assert.Equal(t, "rendered", fixture.targetValue(t, instance, "stringData"), instance.Status)
	}

	assert.Zero(t, fixture.patches)
}

func TestNewLeaderCatchesUpTemplateSecrets(t *testing.T) {
	fixture := newUpdateFixture(t)
	version := fixture.setSource(t, map[string][]byte{"api-key": []byte("v1")})
	instance := fixture.liveInstance(t, "update-new-leader", "ready", version, "stringData")

	// The source changed while no replica was watching.
	fixture.setSource(t, map[string][]byte{"api-key": []byte("v2")})

	synced := false
	fixture.updater.Synced = func() bool { return synced }

	fixture.step(t, instance)
	assert.Zero(t, fixture.patches, "the step waits for the cache to sync")
	assert.Equal(t, "rendered", fixture.targetValue(t, instance, "stringData"))

	synced = true

	fixture.step(t, instance)
	assert.Equal(t, "v2", fixture.targetValue(t, instance, "stringData"), "the first pass after the sync compares every row, with no event")
}

func TestTemplateSecretUpdateChecksUTF8(t *testing.T) {
	fixture := newUpdateFixture(t)
	version := fixture.setSource(t, map[string][]byte{"api-key": []byte("v1")})
	instance := fixture.liveInstance(t, "update-utf8", "ready", version, "stringData", "data")

	invalid := []byte{0xff, 0xfe, 'x'}
	changed := fixture.setSource(t, map[string][]byte{"api-key": invalid})

	fixture.step(t, instance)

	assert.Equal(t, "rendered", fixture.targetValue(t, instance, "stringData"), "text that isn't UTF-8 is not written under stringData")
	assert.Equal(t, "the value of key `api-key` in Secret `keys` isn't UTF-8 text; use it under `data`",
		fixture.row(t, instance, "stringData").LastError.String)
	assert.Equal(t, version, fixture.row(t, instance, "stringData").SourceVersion)

	assert.Equal(t, string(invalid), fixture.targetValue(t, instance, "data"), "the same bytes are fine under data")
	assert.Equal(t, changed, fixture.row(t, instance, "data").SourceVersion)
	assert.NotEmpty(t, base64.StdEncoding.EncodeToString(invalid))
}

func TestTemplateSecretNamespaceChangeKeepsCopies(t *testing.T) {
	const reason = "the template secrets namespace changed"

	setup := func(t *testing.T) (*updateFixture, db.Instance) {
		t.Helper()

		fixture := newUpdateFixture(t)
		version := fixture.setSource(t, map[string][]byte{"api-key": []byte("v1")})

		return fixture, fixture.liveInstance(t, "update-namespace", "ready", version, "stringData")
	}

	t.Run("namespace moved", func(t *testing.T) {
		fixture, instance := setup(t)

		// The new namespace has a Secret of the same name with another value.
		other := sourceSecret(map[string][]byte{"api-key": []byte("other")})
		other.Namespace = "moved-template-secrets"
		require.NoError(t, fixture.source.Create(t.Context(), other))

		fixture.updater.Namespace = other.Namespace
		fixture.step(t, instance)

		assert.Equal(t, "rendered", fixture.targetValue(t, instance, "stringData"))
		assert.Equal(t, reason, fixture.row(t, instance, "stringData").LastError.String)
		assert.InDelta(t, 1, fixture.outcome("missing"), 0)
		assert.Zero(t, fixture.patches)
	})

	t.Run("namespace removed", func(t *testing.T) {
		fixture, instance := setup(t)

		fixture.updater.Namespace, fixture.updater.Source = "", nil
		fixture.step(t, instance)

		assert.Equal(t, "rendered", fixture.targetValue(t, instance, "stringData"))
		assert.Equal(t, reason, fixture.row(t, instance, "stringData").LastError.String)
		assert.InDelta(t, 1, fixture.outcome("missing"), 0)
		assert.Equal(t, 1, bytes.Count(fixture.logs.Bytes(), []byte("template secret copy kept")))
	})
}
