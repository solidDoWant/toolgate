package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
)

type fakeSecrets struct {
	secrets   map[string]string
	resolvers map[string]string
}

var errNoSecret = errors.New("not set")

func (f fakeSecrets) Secret(_ context.Context, name string) (string, error) {
	value, found := f.secrets[name]
	if !found {
		return "", errNoSecret
	}

	return value, nil
}

func (f fakeSecrets) Resolve(_ context.Context, name string) (string, error) {
	value, found := f.resolvers[name]
	if !found {
		return "", errNoSecret
	}

	return value, nil
}

// newTemplate returns a template named "echo" whose resources are the given
// YAML documents.
func newTemplate(t *testing.T, scope string, resources ...string) *v1alpha1.MCPServerTemplate {
	t.Helper()

	template := &v1alpha1.MCPServerTemplate{}
	template.Name = "echo"
	template.Namespace = "toolgate-catalog"
	template.Spec.Scope = scope

	for _, resource := range resources {
		raw, err := yaml.YAMLToJSON([]byte(resource))
		require.NoError(t, err)

		template.Spec.Resources = append(template.Spec.Resources, k8sruntime.RawExtension{Raw: raw})
	}

	return template
}

func newInput(t *testing.T, scope string, resources ...string) runtime.Input {
	t.Helper()

	return runtime.Input{
		Template: newTemplate(t, scope, resources...),
		Identity: runtime.Identity{
			PrincipalID:    "user-sub-1",
			PrincipalEmail: "alice@example.com",
			PrincipalName:  "Alice",
			WorkspaceID:    "claimed/laptop",
			Attested:       false,
			SessionID:      "session-ABC",
		},
		Instance: runtime.Instance{
			ID:        "0192f0c3a1b27c3d8e9f00112233aabb",
			Namespace: "tg-0192f0c3a1b27c3d8e9f00112233aabb",
		},
		Secrets: fakeSecrets{
			secrets:   map[string]string{"github-token": "ghp_secret"},
			resolvers: map[string]string{"app-token": "resolved"},
		},
	}
}

func render(t *testing.T, input runtime.Input) []*unstructured.Unstructured {
	t.Helper()

	result, err := runtime.Render(t.Context(), input)
	require.NoError(t, err)

	return result.Objects
}

func nestedString(t *testing.T, object *unstructured.Unstructured, fields ...string) string {
	t.Helper()

	value, found, err := unstructured.NestedString(object.Object, fields...)
	require.NoError(t, err)
	require.True(t, found, "field %v not found", fields)

	return value
}

func TestRenderSubstitutesEveryVariable(t *testing.T) {
	input := newInput(t, "per-session",
		`apiVersion: v1
kind: ConfigMap
metadata: {name: vars}
data:
  principalID: "${principal.id}"
  email: "mail ${principal.email}"
  name: "${principal.name}"
  workspace: "${workspace.id}"
  session: "${session.id}"
  instance: "${instance.id}"
  namespace: "${instance.namespace}"
  template: "${template.name}"
  "${principal.id}": key-is-not-rendered
  number: 5
`,
		`apiVersion: v1
kind: Secret
metadata: {name: creds}
stringData:
  token: "${secret:github-token}"
  app: "${resolver:app-token}"
`)

	result, err := runtime.Render(t.Context(), input)
	require.NoError(t, err)
	require.Len(t, result.Objects, 2)

	configMap := result.Objects[0]
	assert.Equal(t, map[string]any{
		"principalID":     "user-sub-1",
		"email":           "mail alice@example.com",
		"name":            "Alice",
		"workspace":       "claimed/laptop",
		"session":         "session-ABC",
		"instance":        "0192f0c3a1b27c3d8e9f00112233aabb",
		"namespace":       "tg-0192f0c3a1b27c3d8e9f00112233aabb",
		"template":        "echo",
		"${principal.id}": "key-is-not-rendered",
		"number":          int64(5),
	}, configMap.Object["data"])

	secret := result.Objects[1]
	assert.Equal(t, "ghp_secret", nestedString(t, secret, "stringData", "token"))
	assert.Equal(t, "resolved", nestedString(t, secret, "stringData", "app"))
	assert.Equal(t, []string{"github-token"}, result.SecretsUsed)
}

func TestRenderValuesAreLiteral(t *testing.T) {
	const hostile = "x: 1\ninjected: true\n${session.id} $${y} {\"a\": 1}"

	input := newInput(t, "per-session",
		`apiVersion: v1
kind: ConfigMap
metadata: {name: literal}
data:
  name: "${principal.name}"
  escaped: "$${principal.name} and $${"
  mixed: "$$${principal.id}"
`)
	input.Identity.PrincipalName = hostile

	objects := render(t, input)
	require.Len(t, objects, 1)

	data := objects[0].Object["data"].(map[string]any)
	assert.Equal(t, map[string]any{
		"name":    hostile,
		"escaped": "${principal.name} and ${",
		"mixed":   "$${principal.id}",
	}, data)
	assert.NotContains(t, objects[0].Object, "injected")
}

func TestRenderHashesClaimedValuesInNamesAndLabels(t *testing.T) {
	input := newInput(t, "per-session",
		`apiVersion: apps/v1
kind: Deployment
metadata:
  name: "echo-${session.id}"
  labels: {session: "${session.id}", workspace: "${workspace.id}"}
spec:
  selector:
    matchLabels: {session: "${session.id}"}
  template:
    metadata:
      labels: {session: "${session.id}"}
    spec:
      serviceAccountName: "${session.id}"
      containers:
      - name: echo
        env:
        - {name: SESSION, value: "${session.id}"}
        - {name: WORKSPACE, value: "${workspace.id}"}
`)
	input.Identity.SessionID = "Session_ID.With-Upper.Case_" + "0123456789012345678901234567890123456789012345678901234567890123456789"

	objects := render(t, input)
	require.Len(t, objects, 1)

	deployment := objects[0]

	name := deployment.GetName()
	assert.NotContains(t, name, "Session_ID")
	assert.Empty(t, validation.IsDNS1123Label(name), "name %q", name)

	labels := deployment.GetLabels()
	for _, key := range []string{"session", "workspace"} {
		assert.NotContains(t, labels[key], "Session_ID")
		assert.NotContains(t, labels[key], "claimed/")
		assert.Empty(t, validation.IsValidLabelValue(labels[key]), "label %s=%q", key, labels[key])
	}

	assert.Equal(t, labels["session"], nestedString(t, deployment, "spec", "selector", "matchLabels", "session"))
	assert.Equal(t, labels["session"], nestedString(t, deployment, "spec", "template", "metadata", "labels", "session"))
	assert.Equal(t, labels["session"], nestedString(t, deployment, "spec", "template", "spec", "serviceAccountName"))

	containers, _, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	require.NoError(t, err)
	env := containers[0].(map[string]any)["env"].([]any)
	assert.Equal(t, input.Identity.SessionID, env[0].(map[string]any)["value"], "a value outside names and labels is not hashed")
	assert.Equal(t, "claimed/laptop", env[1].(map[string]any)["value"])

	t.Run("an attested workspace is not hashed", func(t *testing.T) {
		attested := newInput(t, "per-session",
			`apiVersion: v1
kind: ConfigMap
metadata:
  name: cm
  labels: {owner: "${principal.id}", workspace: "${workspace.id}"}
`)
		attested.Identity.WorkspaceID = "ns.owner"
		attested.Identity.Attested = true

		objects := render(t, attested)
		assert.Equal(t, "ns.owner", objects[0].GetLabels()["workspace"])
		assert.Equal(t, "user-sub-1", objects[0].GetLabels()["owner"])
	})
}

func TestRenderUndefinedVariablesFail(t *testing.T) {
	testCases := []struct {
		name     string
		scope    string
		value    string
		mutate   func(*runtime.Input)
		variable string
	}{
		{name: "unknown variable", scope: "per-session", value: "${principal.phone}", variable: "principal.phone"},
		{name: "unknown namespace", scope: "per-user", value: "a ${foo}", variable: "foo"},
		{name: "principal in shared", scope: "shared", value: "${principal.id}", variable: "principal.id"},
		{name: "workspace in shared", scope: "shared", value: "${workspace.id}", variable: "workspace.id"},
		{name: "session in shared", scope: "shared", value: "${session.id}", variable: "session.id"},
		{name: "secret in shared", scope: "shared", value: "${secret:github-token}", variable: "secret:github-token"},
		{name: "resolver in shared", scope: "shared", value: "${resolver:app-token}", variable: "resolver:app-token"},
		{name: "missing secret", scope: "per-user", value: "${secret:absent}", variable: "secret:absent"},
		{name: "missing resolver", scope: "per-user", value: "${resolver:absent}", variable: "resolver:absent"},
		{
			name: "no verified email", scope: "per-user", value: "${principal.email}", variable: "principal.email",
			mutate: func(input *runtime.Input) { input.Identity.PrincipalEmail = "" },
		},
		{name: "unterminated", scope: "per-user", value: "${principal.id", variable: "principal.id"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			input := newInput(t, testCase.scope, `apiVersion: v1
kind: Secret
metadata: {name: s}
stringData:
  value: "`+testCase.value+`"
`)
			if testCase.mutate != nil {
				testCase.mutate(&input)
			}

			_, err := runtime.Render(t.Context(), input)
			require.Error(t, err)

			var variableError *runtime.VariableError
			require.ErrorAs(t, err, &variableError)
			assert.Equal(t, testCase.variable, variableError.Variable)
			assert.Contains(t, err.Error(), testCase.variable)
			assert.NotContains(t, err.Error(), "alice@example.com")
		})
	}

	t.Run("shared scope renders its own variables", func(t *testing.T) {
		objects := render(t, newInput(t, "shared", `apiVersion: v1
kind: ConfigMap
metadata: {name: "${template.name}-${instance.id}"}
data: {ns: "${instance.namespace}"}
`))
		assert.Equal(t, "echo-0192f0c3a1b27c3d8e9f00112233aabb", objects[0].GetName())
	})
}

func TestRenderLabelsAndNamespace(t *testing.T) {
	input := newInput(t, "per-session",
		`apiVersion: v1
kind: ConfigMap
metadata:
  name: one
  namespace: kube-system
  labels: {app.kubernetes.io/managed-by: someone-else, keep: me}
`,
		`apiVersion: v1
kind: Service
metadata: {name: two}
spec: {ports: [{port: 8080}]}
`)

	objects := render(t, input)
	require.Len(t, objects, 2)

	for _, object := range objects {
		assert.Equal(t, input.Instance.Namespace, object.GetNamespace())

		labels := object.GetLabels()
		assert.Equal(t, input.Instance.ID, labels["toolgate.soliddowant.dev/instance"])
		assert.Equal(t, "echo", labels["toolgate.soliddowant.dev/template"])
		assert.Equal(t, "toolgate", labels["app.kubernetes.io/managed-by"])
	}

	assert.Equal(t, "me", objects[0].GetLabels()["keep"])
}
