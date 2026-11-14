package templatescope_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/templatescope"
)

func podWithEnv(value string) runtime.RawExtension {
	return runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"mcp"},"spec":{"containers":[{"name":"mcp","env":[{"name":"V","value":"` + value + `"}]}]}}`)}
}

func launched(scope string, value string) v1alpha1.MCPServerTemplateSpec {
	return v1alpha1.MCPServerTemplateSpec{
		Scope:     scope,
		Endpoint:  v1alpha1.Endpoint{Service: "mcp", Port: 8080},
		Resources: []runtime.RawExtension{podWithEnv(value)},
	}
}

func remote(scope string, headers map[string]string) v1alpha1.MCPServerTemplateSpec {
	return v1alpha1.MCPServerTemplateSpec{
		Scope:  scope,
		Remote: &v1alpha1.RemoteServer{URL: "https://mcp.example.com/mcp", Headers: headers},
	}
}

const envPath = "resources[0].spec.containers[0].env[0].value"

func TestDecide(t *testing.T) {
	tests := []struct {
		name        string
		spec        v1alpha1.MCPServerTemplateSpec
		wantScope   string
		wantReason  string
		wantProblem string
	}{
		{
			name:       "no variables derive shared",
			spec:       launched("", "plain"),
			wantScope:  templatescope.Shared,
			wantReason: "shared: references nothing per-user",
		},
		{
			name:       "instance and template variables derive shared",
			spec:       launched("", "${instance.id} ${instance.namespace} ${template.name}"),
			wantScope:  templatescope.Shared,
			wantReason: "shared: references nothing per-user",
		},
		{
			name:       "an escaped variable derives shared",
			spec:       launched("", "$${session.id}"),
			wantScope:  templatescope.Shared,
			wantReason: "shared: references nothing per-user",
		},
		{
			name:       "a principal derives per-user",
			spec:       launched("", "${principal.email}"),
			wantScope:  templatescope.PerUser,
			wantReason: "per-user: `" + envPath + "` uses `${principal.email}`",
		},
		{
			name:       "a secret derives per-user without naming the secret",
			spec:       launched("", "${secret:github-token}"),
			wantScope:  templatescope.PerUser,
			wantReason: "per-user: `" + envPath + "` uses `${secret:…}`",
		},
		{
			name:       "a resolver derives per-user",
			spec:       launched("", "${resolver:vault}"),
			wantScope:  templatescope.PerUser,
			wantReason: "per-user: `" + envPath + "` uses `${resolver:…}`",
		},
		{
			name:       "a session derives per-session",
			spec:       launched("", "${session.id}"),
			wantScope:  templatescope.PerSession,
			wantReason: "per-session: `" + envPath + "` uses `${session.id}`",
		},
		{
			name:       "a workspace derives per-session even next to a secret",
			spec:       launched("", "${secret:token}-${workspace.id}"),
			wantScope:  templatescope.PerSession,
			wantReason: "per-session: `" + envPath + "` uses `${workspace.id}`",
		},
		{
			name:       "a principal in the endpoint service derives per-user",
			spec:       v1alpha1.MCPServerTemplateSpec{Endpoint: v1alpha1.Endpoint{Service: "mcp-${principal.id}"}},
			wantScope:  templatescope.PerUser,
			wantReason: "per-user: `endpoint.service` uses `${principal.id}`",
		},
		{
			name:       "a remote with no headers derives shared",
			spec:       remote("", nil),
			wantScope:  templatescope.Shared,
			wantReason: "shared: references nothing per-user",
		},
		{
			name:       "a remote with a static header derives shared",
			spec:       remote("", map[string]string{"Authorization": "Bearer public"}),
			wantScope:  templatescope.Shared,
			wantReason: "shared: references nothing per-user",
		},
		{
			name:       "a remote with a secret header derives per-user",
			spec:       remote("", map[string]string{"Authorization": "Bearer ${secret:api-key}"}),
			wantScope:  templatescope.PerUser,
			wantReason: "per-user: `remote.headers.Authorization` uses `${secret:…}`",
		},
		{
			name:       "an explicit narrower scope is kept",
			spec:       launched(templatescope.PerSession, "${principal.id}"),
			wantScope:  templatescope.PerSession,
			wantReason: "per-session: set by `scope`",
		},
		{
			name:       "an explicit per-user scope with no variables is kept",
			spec:       remote(templatescope.PerUser, nil),
			wantScope:  templatescope.PerUser,
			wantReason: "per-user: set by `scope`",
		},
		{
			name:       "an explicit scope equal to the derived one gives the reference",
			spec:       launched(templatescope.PerUser, "${principal.id}"),
			wantScope:  templatescope.PerUser,
			wantReason: "per-user: `" + envPath + "` uses `${principal.id}`",
		},
		{
			name:        "an explicit shared scope with a secret header is a problem that doesn't quote the value",
			spec:        remote(templatescope.Shared, map[string]string{"Authorization": "Bearer ${secret:api-key}"}),
			wantScope:   templatescope.Shared,
			wantProblem: "`scope: shared`, but `remote.headers.Authorization` uses `${secret:…}`, which needs `per-user` or narrower",
		},
		{
			name:        "an explicit per-user scope with a session is a problem",
			spec:        launched(templatescope.PerUser, "${session.id}"),
			wantScope:   templatescope.PerUser,
			wantProblem: "`scope: per-user`, but `" + envPath + "` uses `${session.id}`, which needs `per-session` or narrower",
		},
		{
			name:        "an explicit per-user scope with a workspace is a problem",
			spec:        launched(templatescope.PerUser, "${workspace.id}"),
			wantScope:   templatescope.PerUser,
			wantProblem: "`scope: per-user`, but `" + envPath + "` uses `${workspace.id}`, which needs `per-session` or narrower",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := templatescope.Decide(test.spec)

			assert.Equal(t, test.wantScope, decision.Scope)
			assert.Equal(t, test.wantReason, decision.Reason)
			assert.Equal(t, test.wantProblem, decision.Problem)
			assert.Equal(t, test.wantScope, templatescope.Effective(test.spec))
		})
	}
}

func secretSpec(scope, namespace, value string) v1alpha1.MCPServerTemplateSpec {
	raw := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"s"},"stringData":{"token":"` + value + `"}}`

	return v1alpha1.MCPServerTemplateSpec{
		Scope:     scope,
		Namespace: namespace,
		Endpoint:  v1alpha1.Endpoint{Service: "mcp", Port: 8080},
		Resources: []runtime.RawExtension{{Raw: []byte(raw)}},
	}
}

func TestExchangeNarrowsAndTemplateSecretDoesNot(t *testing.T) {
	assert.Equal(t, templatescope.PerUser, templatescope.ForVariable("exchange:temporal"))
	assert.Equal(t, templatescope.Shared, templatescope.ForVariable("templateSecret:keys/api-key"))
	assert.Equal(t, "${exchange:…}", templatescope.Display("exchange:temporal"))
	assert.Equal(t, "${templateSecret:…}", templatescope.Display("templateSecret:keys/api-key"))

	decision := templatescope.Decide(secretSpec("shared", "shared", "${exchange:temporal}"))
	assert.Contains(t, decision.Problem, "uses `${exchange:…}`, which needs `per-user` or narrower")

	assert.Empty(t, templatescope.Decide(secretSpec("shared", "shared", "${templateSecret:keys/api-key}")).Problem)
	assert.Empty(t, templatescope.Decide(secretSpec("per-user", "", "${exchange:temporal}")).Problem)
	assert.Equal(t, templatescope.PerUser, templatescope.Effective(secretSpec("", "", "${exchange:temporal}")))
	assert.Equal(t, templatescope.Shared, templatescope.Effective(secretSpec("", "", "${templateSecret:keys/api-key}")))
}
