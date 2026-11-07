package catalog_test

import (
	"strings"
	"testing"
	"time"

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

func withHealthCheck(template *v1alpha1.MCPServerTemplate, change func(*v1alpha1.HealthCheck)) *v1alpha1.MCPServerTemplate {
	check := &v1alpha1.HealthCheck{
		HTTP:     v1alpha1.HealthCheckHTTP{Service: "mcp", Port: 8080, Path: "/healthz", ExpectedStatus: 200},
		Interval: metav1.Duration{Duration: 30 * time.Second}, Timeout: metav1.Duration{Duration: 5 * time.Second},
		FailureThreshold: 3, SuccessThreshold: 1, OnFailure: "replace", MaxRepairs: 3,
	}
	change(check)
	template.Spec.HealthCheck = check

	return template
}

func TestValidateHealthCheck(t *testing.T) {
	longPath := "/" + strings.Repeat("a", 1023)
	tests := []struct {
		name        string
		change      func(*v1alpha1.HealthCheck)
		shared      bool
		wantProblem string
	}{
		{name: "a valid check", change: func(*v1alpha1.HealthCheck) {}},
		{name: "defaults left out are valid", change: func(check *v1alpha1.HealthCheck) {
			*check = v1alpha1.HealthCheck{HTTP: v1alpha1.HealthCheckHTTP{Service: "mcp", Port: 80, Path: "/"}}
		}},
		{name: "path of exactly 1024 characters is valid", change: func(check *v1alpha1.HealthCheck) { check.HTTP.Path = longPath }},
		{name: "path of 1025 characters", change: func(check *v1alpha1.HealthCheck) { check.HTTP.Path = longPath + "a" }, wantProblem: "healthCheck.http.path"},
		{name: "path without a leading slash", change: func(check *v1alpha1.HealthCheck) { check.HTTP.Path = "healthz" }, wantProblem: "healthCheck.http.path"},
		{name: "empty service", change: func(check *v1alpha1.HealthCheck) { check.HTTP.Service = "" }, wantProblem: "healthCheck.http.service"},
		{name: "port 0", change: func(check *v1alpha1.HealthCheck) { check.HTTP.Port = 0 }, wantProblem: "healthCheck.http.port"},
		{name: "port 65536", change: func(check *v1alpha1.HealthCheck) { check.HTTP.Port = 65536 }, wantProblem: "healthCheck.http.port"},
		{name: "expectedStatus 99", change: func(check *v1alpha1.HealthCheck) { check.HTTP.ExpectedStatus = 99 }, wantProblem: "expectedStatus"},
		{name: "expectedStatus 600", change: func(check *v1alpha1.HealthCheck) { check.HTTP.ExpectedStatus = 600 }, wantProblem: "expectedStatus"},
		{name: "expectedStatus 599", change: func(check *v1alpha1.HealthCheck) { check.HTTP.ExpectedStatus = 599 }},
		{name: "interval 10s", change: func(check *v1alpha1.HealthCheck) { check.Interval.Duration = 10 * time.Second }},
		{name: "interval 9s", change: func(check *v1alpha1.HealthCheck) { check.Interval.Duration = 9 * time.Second }, wantProblem: "healthCheck.interval"},
		{name: "interval 10m", change: func(check *v1alpha1.HealthCheck) { check.Interval.Duration = 10 * time.Minute }},
		{name: "interval 10m1s", change: func(check *v1alpha1.HealthCheck) { check.Interval.Duration = 10*time.Minute + time.Second }, wantProblem: "healthCheck.interval"},
		{name: "timeout 999ms", change: func(check *v1alpha1.HealthCheck) { check.Timeout.Duration = 999 * time.Millisecond }, wantProblem: "healthCheck.timeout"},
		{name: "timeout 61s", change: func(check *v1alpha1.HealthCheck) {
			check.Interval.Duration, check.Timeout.Duration = 10*time.Minute, 61*time.Second
		}, wantProblem: "healthCheck.timeout"},
		{name: "timeout equal to interval", change: func(check *v1alpha1.HealthCheck) { check.Timeout.Duration = 30 * time.Second }, wantProblem: "less than"},
		{name: "timeout one below interval", change: func(check *v1alpha1.HealthCheck) { check.Timeout.Duration = 29 * time.Second }},
		{name: "failureThreshold 11", change: func(check *v1alpha1.HealthCheck) { check.FailureThreshold = 11 }, wantProblem: "failureThreshold"},
		{name: "failureThreshold 10", change: func(check *v1alpha1.HealthCheck) { check.FailureThreshold = 10 }},
		{name: "successThreshold 11", change: func(check *v1alpha1.HealthCheck) { check.SuccessThreshold = 11 }, wantProblem: "successThreshold"},
		{name: "maxRepairs 11", change: func(check *v1alpha1.HealthCheck) { check.MaxRepairs = 11 }, wantProblem: "maxRepairs"},
		{name: "maxRepairs negative", change: func(check *v1alpha1.HealthCheck) { check.MaxRepairs = -1 }, wantProblem: "maxRepairs"},
		{name: "unknown onFailure", change: func(check *v1alpha1.HealthCheck) { check.OnFailure = "restart" }, wantProblem: "onFailure"},
		{name: "onFailure none", change: func(check *v1alpha1.HealthCheck) { check.OnFailure = "none" }},
		{name: "shared namespace service without the instance id", shared: true, change: func(check *v1alpha1.HealthCheck) {}, wantProblem: "`healthCheck.http.service` must contain `${instance.id}`"},
		{name: "shared namespace service with the instance id", shared: true, change: func(check *v1alpha1.HealthCheck) { check.HTTP.Service = "mcp-${instance.id}" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var template *v1alpha1.MCPServerTemplate

			validator := catalog.Validator{}

			if test.shared {
				template = newTemplate("github", catalog.ScopeShared)
				template.Spec.Namespace = catalog.NamespaceShared
				template.Spec.Endpoint.Service = "mcp-${instance.id}"
				validator.SharedNamespace = "shared"
			} else {
				template = newTemplate("github", catalog.ScopePerUser)
			}

			problems := validator.Validate(withHealthCheck(template, test.change))
			if test.wantProblem == "" {
				assert.Empty(t, problems)

				return
			}

			assert.Contains(t, catalog.Reason(problems), test.wantProblem)
		})
	}
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
			name:      "a secret in data is valid",
			template:  newTemplate("github", catalog.ScopePerUser, secretWith("data", "${resolver:vault}")),
			validator: catalog.Validator{Resolvers: []string{"vault"}},
		},
		{
			name:        "a resolver that isn't configured is invalid",
			template:    newTemplate("github", catalog.ScopePerUser, secretWith("data", "${resolver:other}")),
			validator:   catalog.Validator{Resolvers: []string{"vault"}},
			wantProblem: "resolver `other` isn't configured",
		},
		{
			name:     "an escaped variable is not a variable",
			template: newTemplate("github", catalog.ScopeShared, podWithEnv("$${secret:x} $${principal.id}")),
		},
		{
			name:        "a secret in a Pod's env is invalid",
			template:    newTemplate("github", catalog.ScopePerSession, podWithEnv("${secret:github-token}")),
			wantProblem: "resources[0].spec.containers[0].env[0].value: ${secret:github-token} is allowed only in the data or stringData values of a Secret",
		},
		{
			name:        "a resolver outside a Secret is invalid",
			template:    newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"},"data":{"T":"${resolver:vault}"}}`),
			wantProblem: "${resolver:vault} is allowed only in the data or stringData values of a Secret",
		},
		{
			name:        "a secret in a Secret's metadata is invalid",
			template:    newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"${secret:x}"}}`),
			wantProblem: "resources[0].metadata.name: ${secret:x} is allowed only",
		},
		{
			name:        "a secret as a Secret data key is invalid",
			template:    newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"s"},"stringData":{"${secret:x}":"v"}}`),
			wantProblem: "(a key): ${secret:x} is allowed only",
		},
		{
			name:        "a cluster-scoped kind is invalid",
			template:    newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"n"}}`),
			wantProblem: "resources[0]: v1 Namespace is cluster-scoped; templates may contain only namespaced kinds",
		},
		{
			name:        "a ClusterRole is invalid",
			template:    newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"n"}}`),
			wantProblem: "rbac.authorization.k8s.io/v1 ClusterRole is cluster-scoped",
		},
		{
			name: "a template with a Role or RoleBinding is valid",
			template: newTemplate("github", catalog.ScopePerSession,
				`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"Role","metadata":{"name":"n"}}`,
				`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"RoleBinding","metadata":{"name":"n"}}`),
		},
		{
			name:     "a namespaced kind not on the built-in list is valid",
			template: newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"n"}}`),
		},
		{
			name:     "a HelmRelease is valid when its CRD is installed",
			template: newTemplate("github", catalog.ScopePerSession, `{"apiVersion":"helm.toolkit.fluxcd.io/v2","kind":"HelmRelease","metadata":{"name":"n"}}`),
		},
		{
			name:        "a kind whose CRD isn't installed is invalid",
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
			name:        "a shared template using a secret is invalid",
			template:    newTemplate("github", catalog.ScopeShared, secretWith("stringData", "${secret:github-token}")),
			wantProblem: "a shared template may not use ${secret:github-token}",
		},
		{
			name:        "a shared template using a resolver is invalid",
			template:    newTemplate("github", catalog.ScopeShared, secretWith("stringData", "${resolver:vault}")),
			wantProblem: "a shared template may not use ${resolver:vault}",
		},
		{
			name:        "a shared template using a principal is invalid",
			template:    newTemplate("github", catalog.ScopeShared, podWithEnv("${principal.email}")),
			wantProblem: "a shared template may not use ${principal.email}",
		},
		{
			name:        "a shared template using a workspace is invalid",
			template:    newTemplate("github", catalog.ScopeShared, podWithEnv("ws-${workspace.id}")),
			wantProblem: "a shared template may not use ${workspace.id}",
		},
		{
			name:        "a shared template using a session is invalid",
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

func TestValidateSPIFFEIDPatterns(t *testing.T) {
	valid := newTemplate("github", catalog.ScopePerSession, serviceResource)
	valid.Spec.Access.SPIFFEIDs = []string{"spiffe://coder.example.com/owner/*/workspace/*"}
	assert.Empty(t, catalog.Validator{Mapper: testRESTMapper()}.Validate(valid))

	invalid := newTemplate("github", catalog.ScopePerSession, serviceResource)
	invalid.Spec.Access.SPIFFEIDs = []string{"spiffe://coder.example.com/[*"}
	assert.Contains(t, catalog.Reason(catalog.Validator{Mapper: testRESTMapper()}.Validate(invalid)), "access.spiffeIDs[0]")
}

func TestValidateUserAndGroupPatterns(t *testing.T) {
	valid := newTemplate("github", catalog.ScopePerSession, serviceResource)
	valid.Spec.Access.Users = []string{"*", "*@example.com", "alice", `\*`}
	valid.Spec.Access.Groups = []string{"*", "dev-*"}
	assert.Empty(t, catalog.Validator{Mapper: testRESTMapper()}.Validate(valid))

	invalidUser := newTemplate("github", catalog.ScopePerSession, serviceResource)
	invalidUser.Spec.Access.Users = []string{"alice", "[*"}
	assert.Contains(t, catalog.Reason(catalog.Validator{Mapper: testRESTMapper()}.Validate(invalidUser)), `access.users[1]: "[*" is not a valid pattern`)

	invalidGroup := newTemplate("github", catalog.ScopePerSession, serviceResource)
	invalidGroup.Spec.Access.Groups = []string{"dev-[a"}
	assert.Contains(t, catalog.Reason(catalog.Validator{Mapper: testRESTMapper()}.Validate(invalidGroup)), `access.groups[0]: "dev-[a" is not a valid pattern`)
}
