package catalog_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
)

const serviceResource = `{"apiVersion":"v1","kind":"Service","metadata":{"name":"mcp"},"spec":{"ports":[{"port":8080}]}}`

func newTemplate(name, scope string, resources ...string) *v1alpha1.MCPServerTemplate {
	template := &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "catalog", Generation: 1},
		Spec: v1alpha1.MCPServerTemplateSpec{
			Scope:     scope,
			Namespace: catalog.NamespacePerInstance,
			Endpoint:  v1alpha1.Endpoint{Service: "mcp", Port: 8080},
		},
	}

	for _, resource := range resources {
		template.Spec.Resources = append(template.Spec.Resources, runtime.RawExtension{Raw: []byte(resource)})
	}

	return template
}

func secretWith(field, value string) string {
	return `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"token"},"` + field + `":{"TOKEN":"` + value + `"}}`
}

func podWithEnv(value string) string {
	return `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"mcp"},"spec":{"containers":[{"name":"mcp","image":"mcp:1","env":[{"name":"V","value":"` + value + `"}]}]}}`
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name      string
		template  *v1alpha1.MCPServerTemplate
		validator catalog.Validator
		// wantProblem is a substring of the reason; empty means valid.
		wantProblem string
	}{
		{
			name:     "a per-session template with secrets in a Secret is valid",
			template: newTemplate("github", catalog.ScopePerSession, serviceResource, secretWith("stringData", "${secret:github-token}"), podWithEnv("${principal.id} ${session.id}")),
		},
		{
			name:     "a secret in data is valid",
			template: newTemplate("github", catalog.ScopePerUser, secretWith("data", "${resolver:vault}")),
		},
		{
			name:     "an escaped variable is not a variable",
			template: newTemplate("github", catalog.ScopeShared, podWithEnv("$${secret:x} $${principal.id}")),
		},
		{
			name:        "[AC-1-23] a secret in a Pod's env is invalid",
			template:    newTemplate("github", catalog.ScopePerSession, podWithEnv("${secret:github-token}")),
			wantProblem: "resources[0].spec.containers[0].env[0].value: ${secret:github-token} is allowed only in the data or stringData values of a Secret",
		},
		{
			name:        "[AC-1-23] a resolver outside a Secret is invalid",
			template:    newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"},"data":{"T":"${resolver:vault}"}}`),
			wantProblem: "${resolver:vault} is allowed only in the data or stringData values of a Secret",
		},
		{
			name:        "[AC-1-23] a secret in a Secret's metadata is invalid",
			template:    newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"${secret:x}"}}`),
			wantProblem: "resources[0].metadata.name: ${secret:x} is allowed only",
		},
		{
			name:        "[AC-1-23] a secret as a Secret data key is invalid",
			template:    newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"s"},"stringData":{"${secret:x}":"v"}}`),
			wantProblem: "(a key): ${secret:x} is allowed only",
		},
		{
			name:        "[AC-1-24] a cluster-scoped kind is invalid",
			template:    newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"n"}}`),
			wantProblem: "resources[0]: v1 Namespace is not an allowed kind",
		},
		{
			name:        "[AC-1-24] a ClusterRole is invalid",
			template:    newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"n"}}`),
			wantProblem: "rbac.authorization.k8s.io/v1 ClusterRole is an RBAC object",
		},
		{
			name:        "[AC-1-24] a namespaced RBAC kind is invalid",
			template:    newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"RoleBinding","metadata":{"name":"n"}}`),
			wantProblem: "RoleBinding is an RBAC object",
		},
		{
			name:        "[AC-1-24] a kind not on the phase 1 list is invalid",
			template:    newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"n"}}`),
			wantProblem: "batch/v1 Job is not an allowed kind",
		},
		{
			name:        "[AC-1-24] a HelmRelease is invalid in phase 1",
			template:    newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"helm.toolkit.fluxcd.io/v2","kind":"HelmRelease","metadata":{"name":"n"}}`),
			wantProblem: "HelmRelease is not an allowed kind",
		},
		{
			name:        "a CiliumNetworkPolicy without its CRD is invalid",
			template:    newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"cilium.io/v2","kind":"CiliumNetworkPolicy","metadata":{"name":"n"}}`),
			wantProblem: "allowed only when the CiliumNetworkPolicy CRD exists",
		},
		{
			name:      "a CiliumNetworkPolicy with its CRD is valid",
			template:  newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"cilium.io/v2","kind":"CiliumNetworkPolicy","metadata":{"name":"n"}}`),
			validator: catalog.Validator{CiliumNetworkPolicy: true},
		},
		{
			name:     "every allowed kind is valid",
			template: newTemplate("github", catalog.ScopePerSession, allowedKindResources()...),
		},
		{
			name:        "a resource without a kind is invalid",
			template:    newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"v1","metadata":{"name":"n"}}`),
			wantProblem: "resources[0] must have apiVersion and kind",
		},
		{
			name:        "[AC-1-14] a shared template using a secret is invalid",
			template:    newTemplate("github", catalog.ScopeShared, secretWith("stringData", "${secret:github-token}")),
			wantProblem: "a shared template may not use ${secret:github-token}",
		},
		{
			name:        "[AC-1-14] a shared template using a resolver is invalid",
			template:    newTemplate("github", catalog.ScopeShared, secretWith("stringData", "${resolver:vault}")),
			wantProblem: "a shared template may not use ${resolver:vault}",
		},
		{
			name:        "[AC-1-14] a shared template using a principal is invalid",
			template:    newTemplate("github", catalog.ScopeShared, podWithEnv("${principal.email}")),
			wantProblem: "a shared template may not use ${principal.email}",
		},
		{
			name:        "[AC-1-14] a shared template using a workspace is invalid",
			template:    newTemplate("github", catalog.ScopeShared, podWithEnv("ws-${workspace.id}")),
			wantProblem: "a shared template may not use ${workspace.id}",
		},
		{
			name:        "[AC-1-14] a shared template using a session is invalid",
			template:    newTemplate("github", catalog.ScopeShared, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c","labels":{"s":"${session.id}"}}}`),
			wantProblem: "a shared template may not use ${session.id}",
		},
		{
			name:     "a shared template using instance and template variables is valid",
			template: newTemplate("github", catalog.ScopeShared, podWithEnv("${instance.id} ${instance.namespace} ${template.name}")),
		},
		{
			name: "namespace shared with a per-user scope is invalid",
			template: func() *v1alpha1.MCPServerTemplate {
				template := newTemplate("github", catalog.ScopePerUser)
				template.Spec.Namespace = catalog.NamespaceShared

				return template
			}(),
			wantProblem: "namespace: shared is allowed only with scope: shared",
		},
		{
			name:        "a name that is no server name is invalid",
			template:    newTemplate("GitHub", catalog.ScopePerUser),
			wantProblem: "must match ^[a-z][a-z0-9-]{0,30}$",
		},
		{
			name:        "an unknown scope is invalid",
			template:    newTemplate("github", "per-team"),
			wantProblem: `scope "per-team" must be one of`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			problems := test.validator.Validate(test.template)

			if test.wantProblem == "" {
				require.Empty(t, problems)

				return
			}

			require.NotEmpty(t, problems)
			assert.Contains(t, catalog.Reason(problems), test.wantProblem)
		})
	}
}

func allowedKindResources() []string {
	kinds := [][2]string{
		{"v1", "Pod"}, {"apps/v1", "Deployment"}, {"apps/v1", "StatefulSet"}, {"v1", "Service"},
		{"v1", "ConfigMap"}, {"v1", "Secret"}, {"v1", "ServiceAccount"},
		{"networking.k8s.io/v1", "NetworkPolicy"}, {"v1", "PersistentVolumeClaim"},
	}

	resources := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		resources = append(resources, `{"apiVersion":"`+kind[0]+`","kind":"`+kind[1]+`","metadata":{"name":"n"}}`)
	}

	return resources
}

func TestReasonIsBounded(t *testing.T) {
	problems := make([]string, 0, 200)
	for range 200 {
		problems = append(problems, "resources[10]: v1 Namespace is not an allowed kind")
	}

	reason := catalog.Reason(problems)
	assert.Len(t, reason, 1024)
	assert.Equal(t, "...", reason[len(reason)-3:])
	assert.Equal(t, "a; b", catalog.Reason([]string{"a", "b"}))
}
