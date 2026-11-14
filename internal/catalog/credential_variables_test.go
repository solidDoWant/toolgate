package catalog_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
)

func secretTemplate(t *testing.T, scope, field, value string) *v1alpha1.MCPServerTemplate {
	t.Helper()

	raw, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "s"},
		field: map[string]any{"token": value},
	})
	require.NoError(t, err)

	template := &v1alpha1.MCPServerTemplate{}
	template.Name = "creds"
	template.Spec.Scope = scope
	template.Spec.Resources = []runtime.RawExtension{{Raw: raw}}

	return template
}

func TestValidateExchangeVariable(t *testing.T) {
	const location = "resources[0].stringData.token"

	tests := []struct {
		name     string
		scope    string
		field    string
		value    string
		problem  string
		exchange []string
	}{
		{name: "valid", scope: "per-user", field: "stringData", value: "${exchange:temporal}", exchange: []string{"temporal"}},
		{name: "valid under data", scope: "per-user", field: "data", value: "${exchange:temporal}", exchange: []string{"temporal"}},
		{name: "not a Secret value", scope: "per-user", field: "metadata", value: "${exchange:temporal}", exchange: []string{"temporal"},
			problem: "resources[0].metadata.token: ${exchange:temporal} is allowed only in the data or stringData values of a Secret"},
		{name: "not the whole value", scope: "per-user", field: "stringData", value: "Bearer ${exchange:temporal}", exchange: []string{"temporal"},
			problem: location + ": ${exchange:temporal} must be the whole value, with no other text"},
		{name: "two variables", scope: "per-user", field: "stringData", value: "${exchange:temporal}${exchange:temporal}", exchange: []string{"temporal"},
			problem: location + ": ${exchange:temporal} must be the whole value, with no other text"},
		{name: "name not configured", scope: "per-user", field: "stringData", value: "${exchange:temporal}", exchange: []string{"other"},
			problem: location + ": token exchange `temporal` isn't configured"},
		{name: "no exchanges", scope: "per-user", field: "stringData", value: "${exchange:temporal}",
			problem: location + ": token exchange `temporal` isn't configured"},
		{name: "scope too broad", scope: "shared", field: "stringData", value: "${exchange:temporal}", exchange: []string{"temporal"},
			problem: "`scope: shared`, but `" + location + "` uses `${exchange:…}`, which needs `per-user` or narrower"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			problems := catalog.Reason(catalog.Validator{Exchanges: test.exchange}.Validate(secretTemplate(t, test.scope, test.field, test.value)))
			if test.problem == "" {
				assert.NotContains(t, problems, "exchange")
				assert.NotContains(t, problems, "whole value")

				return
			}

			assert.Contains(t, problems, test.problem)
		})
	}
}

func TestValidateTemplateSecretVariable(t *testing.T) {
	const location = "resources[0].stringData.token"

	tests := []struct {
		name      string
		namespace string
		field     string
		value     string
		problem   string
	}{
		{name: "valid", namespace: "tg-keys", field: "stringData", value: "${templateSecret:keys/api-key}"},
		{name: "valid under data", namespace: "tg-keys", field: "data", value: "${templateSecret:keys/api.key_1}"},
		{name: "not a Secret value", namespace: "tg-keys", field: "metadata", value: "${templateSecret:keys/api-key}",
			problem: "resources[0].metadata.token: ${templateSecret:keys/api-key} is allowed only in the data or stringData values of a Secret"},
		{name: "not the whole value", namespace: "tg-keys", field: "stringData", value: "x ${templateSecret:keys/api-key}",
			problem: location + ": ${templateSecret:keys/api-key} must be the whole value, with no other text"},
		{name: "no namespace", field: "stringData", value: "${templateSecret:keys/api-key}",
			problem: location + ": ${templateSecret:…} needs a template secrets namespace, and the gateway has none (`config.templateSecretsNamespace`)"},
		{name: "no slash", namespace: "tg-keys", field: "stringData", value: "${templateSecret:keys}",
			problem: location + ": ${templateSecret:…} must be NAME/KEY, with a Secret name and a key"},
		{name: "bad name", namespace: "tg-keys", field: "stringData", value: "${templateSecret:Keys_1/api-key}",
			problem: location + ": ${templateSecret:…} must be NAME/KEY, with a Secret name and a key"},
		{name: "empty key", namespace: "tg-keys", field: "stringData", value: "${templateSecret:keys/}",
			problem: location + ": ${templateSecret:…} must be NAME/KEY, with a Secret name and a key"},
		{name: "bad key", namespace: "tg-keys", field: "stringData", value: "${templateSecret:keys/a b}",
			problem: location + ": ${templateSecret:…} must be NAME/KEY, with a Secret name and a key"},
		{name: "key splits at the first slash", namespace: "tg-keys", field: "stringData", value: "${templateSecret:keys/a/b}",
			problem: location + ": ${templateSecret:…} must be NAME/KEY, with a Secret name and a key"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// A shared template is valid: the variable doesn't narrow the scope.
			problems := catalog.Reason(catalog.Validator{TemplateSecretsNamespace: test.namespace}.Validate(secretTemplate(t, "shared", test.field, test.value)))
			if test.problem == "" {
				assert.NotContains(t, problems, "templateSecret")
				assert.NotContains(t, problems, "scope")

				return
			}

			assert.Contains(t, problems, test.problem)
		})
	}
}

func TestParseTemplateSecretKeyLength(t *testing.T) {
	long := ""
	for range 253 {
		long += "k"
	}

	_, _, valid := catalog.ParseTemplateSecret("keys/" + long)
	assert.True(t, valid)

	_, _, valid = catalog.ParseTemplateSecret("keys/" + long + "k")
	assert.False(t, valid)
}

func TestUsesExchange(t *testing.T) {
	assert.True(t, catalog.UsesExchange(secretTemplate(t, "per-user", "stringData", "${exchange:temporal}")))
	assert.False(t, catalog.UsesExchange(secretTemplate(t, "per-user", "stringData", "${secret:token}")))
	assert.False(t, catalog.UsesExchange(secretTemplate(t, "shared", "stringData", "${templateSecret:keys/k}")))
}
