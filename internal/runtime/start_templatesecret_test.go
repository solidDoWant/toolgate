package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/solidDoWant/toolgate/internal/runtime"
)

const templateSecretsNamespace = "toolgate-template-secrets"

// templateSecretResource is a Secret whose api-key comes from the admin's
// Secret keys, under field ("data" or "stringData").
func templateSecretResource(field string) string {
	return `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials"},"` + field +
		`":{"api-key":"${templateSecret:keys/api-key}"}}`
}

// templateSecretsKube is a client for the template secrets namespace that
// reads straight from its object store, as the API reader does.
func templateSecretsKube(t *testing.T, funcs interceptor.Funcs, objects ...client.Object) client.WithWatch {
	t.Helper()

	scheme := k8sruntime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))

	return clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(funcs).Build()
}

func sourceSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "keys", Namespace: templateSecretsNamespace}, Data: data}
}

func TestTemplateSecretMissingFailsStart(t *testing.T) {
	const prefix = "The template didn't render: resources[0].stringData.api-key: ${templateSecret:keys/api-key} could not be read: "

	refuse := interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "keys",
			errors.New(`User "system:serviceaccount:toolgate:toolgate" cannot get resource "secrets"`))
	}}

	tests := []struct {
		name    string
		field   string
		funcs   interceptor.Funcs
		objects []client.Object
		reason  string
	}{
		{
			name:   "a missing Secret fails the start with a reason naming it and the namespace",
			field:  "stringData",
			reason: prefix + "Secret `keys` doesn't exist in `toolgate-template-secrets`",
		},
		{
			name:    "a missing key fails the start with a reason naming the Secret and the key",
			field:   "stringData",
			objects: []client.Object{sourceSecret(map[string][]byte{"other": []byte("hunter2")})},
			reason:  prefix + "Secret `keys` has no key `api-key`",
		},
		{
			name:    "a value that isn't UTF-8 fails the start under stringData",
			field:   "stringData",
			objects: []client.Object{sourceSecret(map[string][]byte{"api-key": {0xff, 0xfe, 'h', 'u', 'n', 't'}})},
			reason:  prefix + "the value of key `api-key` in Secret `keys` isn't UTF-8 text; use it under `data`",
		},
		{
			name:   "an API refusal fails the start with the API server's message",
			field:  "stringData",
			funcs:  refuse,
			reason: prefix + "the API server refused to read Secret `keys`: secrets \"keys\" is forbidden: " + `User "system:serviceaccount:toolgate:toolgate" cannot get resource "secrets"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newStarterFixture(t, testTemplate(templateSecretResource(test.field)), "pending")
			fixture.starter.TemplateSecrets = runtime.APITemplateSecrets{
				Reader: templateSecretsKube(t, test.funcs, test.objects...), Namespace: templateSecretsNamespace,
			}
			fixture.starter.TemplateSecretsNamespace = templateSecretsNamespace

			require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

			status, reason, _, _ := fixture.store.snapshot()
			assert.Equal(t, "failed", status)
			assert.Equal(t, test.reason, reason)
			assert.NotContains(t, reason, "hunter", "the reason never holds a value")

			err := fixture.kube.Get(t.Context(), client.ObjectKey{Namespace: instanceNamespace, Name: "credentials"}, &corev1.Secret{})
			assert.True(t, apierrors.IsNotFound(err), "nothing is applied when the render fails")
		})
	}
}

func TestTemplateSecretReadThroughAPIAtRender(t *testing.T) {
	source := templateSecretsKube(t, interceptor.Funcs{})

	newFixture := func(t *testing.T) *starterFixture {
		t.Helper()

		fixture := newStarterFixture(t, testTemplate(templateSecretResource("data"),
			`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"text"},"stringData":{"token":"${templateSecret:keys/api-key}"}}`), "pending")
		fixture.starter.TemplateSecrets = runtime.APITemplateSecrets{Reader: source, Namespace: templateSecretsNamespace}
		fixture.starter.TemplateSecretsNamespace = templateSecretsNamespace

		return fixture
	}

	failed := newFixture(t)
	require.NoError(t, failed.starter.Step(t.Context(), failed.instance))

	status, _, _, _ := failed.store.snapshot()
	require.Equal(t, "failed", status, "the start fails while the Secret is missing")

	created := sourceSecret(map[string][]byte{"api-key": []byte("sk-live-1")})
	require.NoError(t, source.Create(t.Context(), created))

	started := newFixture(t)
	require.NoError(t, started.starter.Step(t.Context(), started.instance))

	status, reason, _, _ := started.store.snapshot()
	require.Equal(t, "starting", status, "the next start reads the new Secret at once: %s", reason)

	rendered := &corev1.Secret{}
	require.NoError(t, started.kube.Get(t.Context(), client.ObjectKey{Namespace: instanceNamespace, Name: "credentials"}, rendered))
	assert.Equal(t, []byte("sk-live-1"), rendered.Data["api-key"], "under data the key holds the value's bytes")

	text := &corev1.Secret{}
	require.NoError(t, started.kube.Get(t.Context(), client.ObjectKey{Namespace: instanceNamespace, Name: "text"}, text))
	assert.Equal(t, "sk-live-1", text.StringData["token"])

	assert.ElementsMatch(t, []runtime.TemplateSecretTarget{
		{
			TargetSecret: "credentials", TargetKey: "api-key", TargetField: "data",
			SourceNamespace: templateSecretsNamespace, SourceName: "keys", SourceKey: "api-key", SourceVersion: created.ResourceVersion,
		},
		{
			TargetSecret: "text", TargetKey: "token", TargetField: "stringData",
			SourceNamespace: templateSecretsNamespace, SourceName: "keys", SourceKey: "api-key", SourceVersion: created.ResourceVersion,
		},
	}, started.store.templateSecrets, "the starting write records each value's source and version")
	assert.NotEmpty(t, created.ResourceVersion)
}
