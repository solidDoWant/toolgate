package catalog_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
)

func newRemoteTemplate(url string, headers map[string]string) *v1alpha1.MCPServerTemplate {
	return &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "mail-archiver", Namespace: "catalog", Generation: 1},
		Spec: v1alpha1.MCPServerTemplateSpec{
			Scope:     catalog.ScopePerUser,
			Namespace: catalog.NamespacePerInstance,
			Remote:    &v1alpha1.RemoteServer{URL: url, Headers: headers},
		},
	}
}

// withScope sets template's scope and returns it.
func withScope(template *v1alpha1.MCPServerTemplate, scope string) *v1alpha1.MCPServerTemplate {
	template.Spec.Scope = scope

	return template
}

const clusterURL = "http://mail-archiver.email.svc.cluster.local:5000/mcp"

func TestValidateRemoteURL(t *testing.T) {
	const httpRule = "may use http:// only for a cluster-internal host"

	tests := []struct {
		url         string
		wantProblem string
	}{
		{url: "https://mail.example.com/mcp"},
		{url: "https://203.0.113.7:8443/mcp"},
		{url: "HTTPS://mail.example.com/mcp"},
		{url: clusterURL},
		{url: "http://mail-archiver.email.svc:5000/mcp"},
		{url: "http://MAIL-ARCHIVER.EMAIL.SVC.CLUSTER.LOCAL/mcp"},
		{url: "http://mail.example.com/mcp", wantProblem: httpRule},
		{url: "http://10.0.0.5:5000/mcp", wantProblem: httpRule},
		{url: "http://mail-archiver.email.svc.example.com/mcp", wantProblem: httpRule},
		{url: "http://mail-archiver.email.svc.cluster.local.evil.com/mcp", wantProblem: httpRule},
		{url: "http://svc/mcp", wantProblem: httpRule},
		{url: "http://.svc/mcp", wantProblem: httpRule},
		{url: "http://..svc.cluster.local/mcp", wantProblem: httpRule},
		{url: "http://mail-archiversvc/mcp", wantProblem: httpRule},
		{url: "ftp://mail.example.com/mcp", wantProblem: "must start with https:// or http://"},
		{url: "ws://mail-archiver.email.svc/mcp", wantProblem: "must start with https:// or http://"},
		{url: "mail.example.com/mcp", wantProblem: "must be an absolute http:// or https:// URL"},
		{url: "https:///mcp", wantProblem: "must be an absolute http:// or https:// URL"},
		{url: "https://user:hunter2@mail.example.com/mcp", wantProblem: "must not contain a user name or password"},
		{url: "https://mail.example.com/%zz", wantProblem: "must be an absolute http:// or https:// URL"},
	}

	for _, test := range tests {
		t.Run(test.url, func(t *testing.T) {
			problems := catalog.Validator{}.Validate(newRemoteTemplate(test.url, nil))
			if test.wantProblem == "" {
				assert.Empty(t, problems)

				return
			}

			require.Len(t, problems, 1)
			assert.Contains(t, problems[0], test.wantProblem)
			assert.Contains(t, problems[0], "`remote.url`")
		})
	}
}

func TestValidateRemoteHeaderValues(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		allowed bool
	}{
		{name: "a secret", value: "Bearer ${secret:mail-archiver-api-key}", allowed: true},
		{name: "a principal field", value: "${principal.id}", allowed: true},
		{name: "a secret and a principal field", value: "${principal.email}:${secret:key}", allowed: true},
		{name: "a literal value", value: "application/json", allowed: true},
		{name: "an escaped dollar brace", value: "$${workspace.id}", allowed: true},
		{name: "an empty value", value: "", allowed: true},
		{name: "a workspace field", value: "${workspace.id}"},
		{name: "a session field", value: "${session.id}"},
		{name: "an instance field", value: "${instance.id}"},
		{name: "a resolver", value: "Bearer ${resolver:app-token}"},
		{name: "a secret without a name", value: "${secret:}"},
		{name: "principal without a field", value: "${principal.}"},
		{name: "principal without a dot", value: "${principal}"},
		{name: "an unknown variable", value: "${env:HOME}"},
		{name: "an unclosed variable", value: "Bearer ${secret:key"},
		{name: "an allowed variable then a refused one", value: "${secret:key} ${workspace.id}"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template := newRemoteTemplate(clusterURL, map[string]string{"X-Key": test.value})
			problems := catalog.Validator{}.Validate(template)

			if test.allowed {
				assert.Empty(t, problems)

				return
			}

			require.Len(t, problems, 1)
			assert.Contains(t, problems[0], "The value of header `X-Key` in `remote.headers` may use only ${secret:NAME} and ${principal.*}")
		})
	}
}

func TestValidateRemoteTimeout(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		allowed bool
	}{
		{name: "1s", timeout: time.Second, allowed: true},
		{name: "5m", timeout: 5 * time.Minute, allowed: true},
		{name: "under 1s", timeout: time.Second - time.Millisecond},
		{name: "over 5m", timeout: 5*time.Minute + time.Second},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template := newRemoteTemplate(clusterURL, nil)
			template.Spec.Remote.Timeout = &metav1.Duration{Duration: test.timeout}
			problems := catalog.Validator{}.Validate(template)

			if test.allowed {
				assert.Empty(t, problems)

				return
			}

			assert.Equal(t, []string{"`remote.timeout` must be 1s to 5m."}, problems)
		})
	}
}

func TestReconcilerReportsInvalidRemoteTemplates(t *testing.T) {
	tests := []struct {
		name     string
		template *v1alpha1.MCPServerTemplate
		reason   string
	}{
		{
			name:     "plain http to a host outside the cluster",
			template: newRemoteTemplate("http://mail.example.com/mcp", nil),
			reason:   "`remote.url` may use http:// only for a cluster-internal host ending in .svc or .svc.cluster.local",
		},
		{
			name:     "a shared remote template whose header uses a secret",
			template: withScope(newRemoteTemplate(clusterURL, map[string]string{"Authorization": "Bearer ${secret:mail-archiver-api-key}"}), catalog.ScopeShared),
			reason:   "`scope: shared`, but `remote.headers.Authorization` uses `${secret:…}`, which needs `per-user` or narrower",
		},
		{
			name:     "a header value using a workspace field",
			template: newRemoteTemplate(clusterURL, map[string]string{"X-Workspace": "${workspace.id}"}),
			reason:   "The value of header `X-Workspace` in `remote.headers` may use only ${secret:NAME} and ${principal.*}",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requireReconciledInvalid(t, test.template, test.reason)
		})
	}
}

func TestReconcilerOffersValidRemoteTemplate(t *testing.T) {
	template := newRemoteTemplate(clusterURL, map[string]string{"Authorization": "Bearer ${secret:mail-archiver-api-key}"})
	fixture := newReconcilerFixture(t, template)

	fixture.reconcile(t, keyOf(template))

	condition := fixture.condition(t, keyOf(template))
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)

	_, err := fixture.catalog.Offered(template.Name)
	require.NoError(t, err)
}

func TestRemoteHeaderValuesStayOutOfStatusAndLog(t *testing.T) {
	// Each value holds a marker that must not show up anywhere the
	// reconciler writes, whether the value is allowed or refused.
	const marker = "hunter2-literal-credential"

	tests := []struct {
		name    string
		headers map[string]string
	}{
		{name: "an allowed value", headers: map[string]string{"Authorization": "Bearer " + marker + " ${secret:key}"}},
		{name: "a refused value", headers: map[string]string{"Authorization": "Bearer " + marker + " ${workspace.id}"}},
		{name: "an unclosed value", headers: map[string]string{"Authorization": marker + "${secret:"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template := newRemoteTemplate("http://mail.example.com/mcp", test.headers)
			fixture := newReconcilerFixture(t, template)

			var logs bytes.Buffer

			fixture.reconciler.Log = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

			fixture.reconcile(t, keyOf(template))

			var stored v1alpha1.MCPServerTemplate
			require.NoError(t, fixture.client.Get(t.Context(), keyOf(template), &stored))

			status, err := json.Marshal(stored.Status)
			require.NoError(t, err)

			condition := fixture.condition(t, keyOf(template))
			require.NotNil(t, condition)
			assert.Equal(t, metav1.ConditionFalse, condition.Status, "the http:// URL makes the template invalid")
			assert.NotContains(t, string(status), marker)
			assert.NotContains(t, condition.Message, marker)
			assert.NotContains(t, logs.String(), marker)

			_, err = fixture.catalog.Offered(template.Name)

			var invalid *catalog.InvalidError
			require.ErrorAs(t, err, &invalid)
			assert.NotContains(t, invalid.Reason, marker)
		})
	}
}

func TestReconcilerReportsTheEffectiveScope(t *testing.T) {
	tests := []struct {
		name      string
		template  *v1alpha1.MCPServerTemplate
		wantScope string
		message   string
	}{
		{
			name:      "a remote template without headers and without a scope is shared",
			template:  withScope(newRemoteTemplate(clusterURL, nil), ""),
			wantScope: catalog.ScopeShared,
			message:   "Scope shared: references nothing per-user.",
		},
		{
			name:      "a remote template whose header uses a secret and without a scope is per-user",
			template:  withScope(newRemoteTemplate(clusterURL, map[string]string{"Authorization": "Bearer ${secret:mail-archiver-api-key}"}), ""),
			wantScope: catalog.ScopePerUser,
			message:   "Scope per-user: `remote.headers.Authorization` uses `${secret:…}`.",
		},
		{
			name:      "a launched template using a session and without a scope is per-session",
			template:  newTemplate("github", "", podWithEnv("${session.id}")),
			wantScope: catalog.ScopePerSession,
			message:   "Scope per-session: `resources[0].spec.containers[0].env[0].value` uses `${session.id}`.",
		},
		{
			name:      "an explicit narrower scope is kept",
			template:  withScope(newRemoteTemplate(clusterURL, nil), catalog.ScopePerUser),
			wantScope: catalog.ScopePerUser,
			message:   "Scope per-user: set by `scope`.",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newReconcilerFixture(t, test.template)
			key := keyOf(test.template)

			fixture.reconcile(t, key)

			condition := fixture.condition(t, key)
			require.NotNil(t, condition)
			assert.Equal(t, metav1.ConditionTrue, condition.Status, condition.Message)
			assert.Contains(t, condition.Message, test.message)

			offered, err := fixture.catalog.Offered(test.template.Name)
			require.NoError(t, err)
			assert.Equal(t, test.wantScope, offered.Spec.Scope)

			// The effective scope is not written back to the spec.
			var stored v1alpha1.MCPServerTemplate
			require.NoError(t, fixture.client.Get(t.Context(), key, &stored))
			assert.Equal(t, test.template.Spec.Scope, stored.Spec.Scope)
		})
	}
}
