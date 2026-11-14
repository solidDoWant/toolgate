//go:build integration

package runtime_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

func TestReapplyRendersCurrentTemplateSecret(t *testing.T) {
	fixture := newStopFixture(t)
	everyone := v1alpha1.Access{Groups: []string{"staff"}, Environments: "any"}

	template := fixture.template("reapply-template-secret", "per-user", time.Hour, everyone)
	template.Spec.Resources = []k8sruntime.RawExtension{{Raw: []byte(templateSecretResource("data"))}}
	fixture.catalog.Set(template, nil)

	session := fixture.session(t, "reapply-template-secret-user", "s1", "", "staff")
	fixture.enable(t, session, template)
	instance := fixture.instance(t, template, "user:reapply-template-secret-user", "reapply-template-secret-user", "ready")
	_, err := fixture.pool.Exec(t.Context(), "UPDATE instances SET render_session = $2 WHERE id = $1", instance.ID, session.ID)
	require.NoError(t, err)

	// The start recorded the value it rendered then, at an older version.
	require.NoError(t, fixture.queries.InsertInstanceTemplateSecret(t.Context(), db.InsertInstanceTemplateSecretParams{
		InstanceID: instance.ID, TargetSecret: "credentials", TargetKey: "api-key", TargetField: "data",
		SourceNamespace: templateSecretsNamespace, SourceName: "keys", SourceKey: "api-key", SourceVersion: "1",
	}))

	now := *fixture.now.Load()
	instance = fixture.makeUnhealthy(t, fixture.get(t, instance.ID), healthCheck("reapply", 3), now.Add(-time.Second))

	current := sourceSecret(map[string][]byte{"api-key": []byte("sk-rotated")})
	source := templateSecretsKube(t, interceptor.Funcs{}, current)
	require.NoError(t, source.Get(t.Context(), client.ObjectKeyFromObject(current), current))

	kube := repairKube(t)
	repairer := fixture.repairer(kube)
	repairer.Starter.TemplateSecrets = runtime.APITemplateSecrets{Reader: source, Namespace: templateSecretsNamespace}
	repairer.Starter.TemplateSecretsNamespace = templateSecretsNamespace

	require.NoError(t, repairer.Step(t.Context(), instance))

	rendered := &corev1.Secret{}
	require.NoError(t, kube.Get(t.Context(), client.ObjectKey{Namespace: instance.Namespace, Name: "credentials"}, rendered))
	assert.Equal(t, []byte("sk-rotated"), rendered.Data["api-key"], "the reapply renders the current value")

	rows, err := fixture.queries.ListInstanceTemplateSecrets(t.Context(), instance.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, current.ResourceVersion, rows[0].SourceVersion, "the rows are written again with the version applied")
	assert.Equal(t, templateSecretsNamespace, rows[0].SourceNamespace)
	assert.NotEqual(t, "1", rows[0].SourceVersion)
}
