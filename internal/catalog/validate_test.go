package catalog_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

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
		// noMapper leaves the validator without a RESTMapper; otherwise a
		// nil mapper is replaced by testRESTMapper.
		noMapper bool
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
			wantProblem: "resources[0]: v1 Namespace is cluster-scoped; templates may contain only namespaced kinds",
		},
		{
			name:        "[AC-1-24] a ClusterRole is invalid",
			template:    newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"n"}}`),
			wantProblem: "rbac.authorization.k8s.io/v1 ClusterRole is cluster-scoped",
		},
		{
			name: "[AC-1-24] a template with a Role or RoleBinding is valid",
			template: newTemplate("github", catalog.ScopePerSession,
				`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"Role","metadata":{"name":"n"}}`,
				`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"RoleBinding","metadata":{"name":"n"}}`),
		},
		{
			name:     "[AC-1-24] a namespaced kind not on the phase 1 list is valid",
			template: newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"n"}}`),
		},
		{
			name:     "[AC-1-24] a HelmRelease is valid when its CRD is installed",
			template: newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"helm.toolkit.fluxcd.io/v2","kind":"HelmRelease","metadata":{"name":"n"}}`),
		},
		{
			name:        "[AC-1-24] a kind whose CRD isn't installed is invalid",
			template:    newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"example.com/v1","kind":"Widget","metadata":{"name":"n"}}`),
			wantProblem: "resources[0]: example.com/v1 Widget is not installed in the cluster; a CRD installed after the gateway started needs a gateway restart",
		},
		{
			name:        "a version the cluster doesn't serve is invalid",
			template:    newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"apps/v1beta1","kind":"Deployment","metadata":{"name":"n"}}`),
			wantProblem: "apps/v1beta1 Deployment is not installed in the cluster",
		},
		{
			name:        "every kind is invalid without a mapper",
			template:    newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"n"}}`),
			noMapper:    true,
			wantProblem: "v1 Pod is not installed in the cluster",
		},
		{
			name:     "a CiliumNetworkPolicy with its CRD is valid",
			template: newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"cilium.io/v2","kind":"CiliumNetworkPolicy","metadata":{"name":"n"}}`),
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
			name:      "a shared-namespace template that tells its instances apart is valid",
			template:  sharedNamespaceTemplate(),
			validator: catalog.Validator{SharedNamespace: "toolgate-shared"},
		},
		{
			name:        "a shared-namespace template without a configured shared namespace is invalid",
			template:    sharedNamespaceTemplate(),
			wantProblem: "The gateway has no shared namespace configured (`config.sharedNamespace.name`).",
		},
		{
			name: "a shared-namespace object whose name lacks the instance ID is invalid",
			template: sharedNamespaceTemplate(func(resources []string) []string {
				return append(resources, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings"}}`)
			}),
			validator:   catalog.Validator{SharedNamespace: "toolgate-shared"},
			wantProblem: "`ConfigMap/settings`: `metadata.name` must contain `${instance.id}` in a shared namespace.",
		},
		{
			name: "a shared-namespace endpoint Service without the instance ID is invalid",
			template: func() *v1alpha1.MCPServerTemplate {
				template := sharedNamespaceTemplate()
				template.Spec.Endpoint.Service = "mcp"

				return template
			}(),
			validator:   catalog.Validator{SharedNamespace: "toolgate-shared"},
			wantProblem: "`endpoint.service` must contain `${instance.id}` in a shared namespace.",
		},
		{
			name: "a shared-namespace Service selector without the instance label is invalid",
			template: sharedNamespaceTemplate(func(resources []string) []string {
				resources[0] = `{"apiVersion":"v1","kind":"Service","metadata":{"name":"mcp-${instance.id}"},"spec":{"selector":{"app":"mcp"}}}`

				return resources
			}),
			validator:   catalog.Validator{SharedNamespace: "toolgate-shared"},
			wantProblem: "`Service/mcp-${instance.id}`: `spec.selector` must contain `toolgate.soliddowant.dev/instance: ${instance.id}`",
		},
		{
			name: "a shared-namespace Deployment selector without the instance label is invalid",
			template: sharedNamespaceTemplate(func(resources []string) []string {
				resources[1] = sharedDeployment(`{"app":"mcp"}`, `{"toolgate.soliddowant.dev/instance":"${instance.id}"}`)

				return resources
			}),
			validator:   catalog.Validator{SharedNamespace: "toolgate-shared"},
			wantProblem: "`Deployment/mcp-${instance.id}`: `spec.selector.matchLabels` must contain",
		},
		{
			name: "a shared-namespace Deployment pod template without the instance label is invalid",
			template: sharedNamespaceTemplate(func(resources []string) []string {
				resources[1] = sharedDeployment(`{"toolgate.soliddowant.dev/instance":"${instance.id}"}`, `{"toolgate.soliddowant.dev/instance":"fixed"}`)

				return resources
			}),
			validator:   catalog.Validator{SharedNamespace: "toolgate-shared"},
			wantProblem: "`Deployment/mcp-${instance.id}`: `spec.template.metadata.labels` must contain",
		},
		{
			name: "a shared-namespace StatefulSet selector without the instance label is invalid",
			template: sharedNamespaceTemplate(func(resources []string) []string {
				return append(resources, `{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"name":"db-${instance.id}"},"spec":{"selector":{"matchLabels":{"app":"db"}},"template":{"metadata":{"labels":{"toolgate.soliddowant.dev/instance":"${instance.id}"}}}}}`)
			}),
			validator:   catalog.Validator{SharedNamespace: "toolgate-shared"},
			wantProblem: "`StatefulSet/db-${instance.id}`: `spec.selector.matchLabels` must contain",
		},
		{
			name: "a shared-namespace NetworkPolicy selector without the instance label is invalid",
			template: sharedNamespaceTemplate(func(resources []string) []string {
				return append(resources, `{"apiVersion":"networking.k8s.io/v1","kind":"NetworkPolicy","metadata":{"name":"np-${instance.id}"},"spec":{"podSelector":{}}}`)
			}),
			validator:   catalog.Validator{SharedNamespace: "toolgate-shared"},
			wantProblem: "`NetworkPolicy/np-${instance.id}`: `spec.podSelector.matchLabels` must contain",
		},
		{
			name: "a CiliumNetworkPolicy in a shared namespace is invalid",
			template: sharedNamespaceTemplate(func(resources []string) []string {
				return append(resources, `{"apiVersion":"cilium.io/v2","kind":"CiliumNetworkPolicy","metadata":{"name":"cnp-${instance.id}"}}`)
			}),
			validator:   catalog.Validator{SharedNamespace: "toolgate-shared"},
			wantProblem: "`CiliumNetworkPolicy/cnp-${instance.id}`: a CiliumNetworkPolicy is not allowed in a shared namespace",
		},
		{
			name: "a HelmRelease in a shared namespace is invalid",
			template: sharedNamespaceTemplate(func(resources []string) []string {
				return append(resources, `{"apiVersion":"helm.toolkit.fluxcd.io/v2","kind":"HelmRelease","metadata":{"name":"hr-${instance.id}"}}`)
			}),
			validator:   catalog.Validator{SharedNamespace: "toolgate-shared"},
			wantProblem: "`HelmRelease/hr-${instance.id}`: a HelmRelease is not allowed in a shared namespace",
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
			validator := test.validator
			if validator.Mapper == nil && !test.noMapper {
				validator.Mapper = testRESTMapper()
			}

			problems := validator.Validate(test.template)

			if test.wantProblem == "" {
				require.Empty(t, problems)

				return
			}

			require.NotEmpty(t, problems)
			assert.Contains(t, catalog.Reason(problems), test.wantProblem)
		})
	}
}

// sharedNamespaceTemplate returns a valid `namespace: shared` template: its
// names, endpoint and selectors all carry ${instance.id}. Each change edits
// its resources.
func sharedNamespaceTemplate(changes ...func(resources []string) []string) *v1alpha1.MCPServerTemplate {
	instanceLabels := `{"toolgate.soliddowant.dev/instance":"${instance.id}"}`
	resources := []string{
		`{"apiVersion":"v1","kind":"Service","metadata":{"name":"mcp-${instance.id}"},"spec":{"selector":` + instanceLabels + `}}`,
		sharedDeployment(instanceLabels, instanceLabels),
		`{"apiVersion":"networking.k8s.io/v1","kind":"NetworkPolicy","metadata":{"name":"mcp-${instance.id}"},"spec":{"podSelector":{"matchLabels":` + instanceLabels + `}}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings-${instance.id}"}}`,
	}

	for _, change := range changes {
		resources = change(resources)
	}

	template := newTemplate("github", catalog.ScopeShared, resources...)
	template.Spec.Namespace = catalog.NamespaceShared
	template.Spec.Endpoint.Service = "mcp-${instance.id}"

	return template
}

func sharedDeployment(matchLabels, podLabels string) string {
	return `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"mcp-${instance.id}"},"spec":{"selector":{"matchLabels":` + matchLabels +
		`},"template":{"metadata":{"labels":` + podLabels + `},"spec":{"containers":[{"name":"mcp","image":"mcp:1"}]}}}}`
}

// testRESTMapper maps the kinds the validation tests use, as a cluster with
// the Flux and Cilium CRDs would. example.com Widget is not installed.
func testRESTMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper(nil)

	namespaced := []schema.GroupVersionKind{
		{Version: "v1", Kind: "Pod"}, {Version: "v1", Kind: "Service"}, {Version: "v1", Kind: "ConfigMap"},
		{Version: "v1", Kind: "Secret"}, {Version: "v1", Kind: "ServiceAccount"}, {Version: "v1", Kind: "PersistentVolumeClaim"},
		{Group: "apps", Version: "v1", Kind: "Deployment"}, {Group: "apps", Version: "v1", Kind: "StatefulSet"},
		{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"},
		{Group: "batch", Version: "v1", Kind: "Job"},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"},
		{Group: "helm.toolkit.fluxcd.io", Version: "v2", Kind: "HelmRelease"},
		{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"},
	}
	for _, kind := range namespaced {
		mapper.Add(kind, meta.RESTScopeNamespace)
	}

	clusterScoped := []schema.GroupVersionKind{
		{Version: "v1", Kind: "Namespace"},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"},
	}
	for _, kind := range clusterScoped {
		mapper.Add(kind, meta.RESTScopeRoot)
	}

	return mapper
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
