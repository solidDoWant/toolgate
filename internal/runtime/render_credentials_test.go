package runtime_test

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	toolgateruntime "github.com/solidDoWant/toolgate/internal/runtime"
)

type fakeTemplateSecrets struct {
	values map[string][]byte
}

var errNoKey = errors.New("no such key")

func (f fakeTemplateSecrets) TemplateSecret(_ context.Context, name, key string) ([]byte, string, error) {
	value, found := f.values[name+"/"+key]
	if !found {
		return nil, "", errNoKey
	}

	return value, "rv-7", nil
}

func TestRenderAppliesSecretsFirst(t *testing.T) {
	input := newInput(t, "per-user",
		"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: config-a}\n",
		"apiVersion: v1\nkind: Secret\nmetadata: {name: secret-a}\n",
		"apiVersion: v1\nkind: ServiceAccount\nmetadata: {name: account}\n",
		"apiVersion: v1\nkind: Secret\nmetadata: {name: sa-token}\ntype: kubernetes.io/service-account-token\n",
		"apiVersion: v1\nkind: Secret\nmetadata: {name: secret-b}\n",
		"apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: app}\n",
	)

	names := []string{}
	for _, object := range toolgateruntime.OrderForApply(render(t, input)) {
		names = append(names, object.GetName())
	}

	assert.Equal(t, []string{"secret-a", "secret-b", "config-a", "account", "sa-token", "app"}, names)
}

func TestRenderTemplateSecretEncoding(t *testing.T) {
	binary := []byte{0xff, 0xfe, 0x00, 'a'}

	tests := []struct {
		name      string
		field     string
		source    string
		want      string
		assertErr require.ErrorAssertionFunc
	}{
		{name: "data holds base64 of the bytes", field: "data", source: "keys/bin", want: base64.StdEncoding.EncodeToString(binary)},
		{name: "data holds base64 of text", field: "data", source: "keys/text", want: base64.StdEncoding.EncodeToString([]byte("héllo"))},
		{name: "stringData holds the text", field: "stringData", source: "keys/text", want: "héllo"},
		{name: "stringData refuses non-UTF-8", field: "stringData", source: "keys/bin", assertErr: require.Error},
		{name: "missing key fails", field: "data", source: "keys/none", assertErr: require.Error},
		{name: "no key part fails", field: "data", source: "keys", assertErr: require.Error},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.assertErr == nil {
				test.assertErr = require.NoError
			}

			input := newInput(t, "shared",
				"apiVersion: v1\nkind: Secret\nmetadata: {name: creds}\n"+test.field+":\n  token: \"${templateSecret:"+test.source+"}\"\n")
			input.TemplateSecrets = fakeTemplateSecrets{values: map[string][]byte{
				"keys/bin": binary, "keys/text": []byte("héllo"),
			}}

			result, err := toolgateruntime.Render(t.Context(), input)
			test.assertErr(t, err)

			if err != nil {
				var variableError *toolgateruntime.VariableError
				require.ErrorAs(t, err, &variableError)
				assert.NotContains(t, err.Error(), "héllo")

				return
			}

			assert.Equal(t, test.want, nestedString(t, result.Objects[0], test.field, "token"))
			assert.Equal(t, []toolgateruntime.TemplateSecretTarget{{
				TargetSecret: "creds", TargetKey: "token", TargetField: test.field,
				SourceName: "keys", SourceKey: strings.TrimPrefix(test.source, "keys/"), SourceVersion: "rv-7",
			}}, result.TemplateSecrets)
		})
	}
}

func TestRenderExchangeTokenTargets(t *testing.T) {
	input := newInput(t, "per-user",
		"apiVersion: v1\nkind: Secret\nmetadata: {name: creds}\ndata:\n  token: \"${exchange:temporal}\"\nstringData:\n  plain: \"${exchange:temporal}\"\n")
	input.ExchangeTokens = map[string]string{"temporal": "tok-1"}

	result, err := toolgateruntime.Render(t.Context(), input)
	require.NoError(t, err)

	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("tok-1")), nestedString(t, result.Objects[0], "data", "token"))
	assert.Equal(t, "tok-1", nestedString(t, result.Objects[0], "stringData", "plain"))
	assert.ElementsMatch(t, []toolgateruntime.ExchangeTarget{
		{Exchange: "temporal", Secret: "creds", Key: "token"},
		{Exchange: "temporal", Secret: "creds", Key: "plain"},
	}, result.Exchanges)
}

func TestRenderCredentialVariablesNeedWholeSecretValue(t *testing.T) {
	tests := []struct {
		name     string
		resource string
	}{
		{"prefix", "kind: Secret\nmetadata: {name: c}\nstringData:\n  t: \"Bearer ${exchange:temporal}\"\n"},
		{"suffix", "kind: Secret\nmetadata: {name: c}\nstringData:\n  t: \"${exchange:temporal} \"\n"},
		{"two variables", "kind: Secret\nmetadata: {name: c}\nstringData:\n  t: \"${exchange:temporal}${exchange:temporal}\"\n"},
		{"config map", "kind: ConfigMap\nmetadata: {name: c}\ndata:\n  t: \"${exchange:temporal}\"\n"},
		{"secret metadata", "kind: Secret\nmetadata: {name: c, labels: {a: \"${exchange:temporal}\"}}\n"},
		{"nested key", "kind: Secret\nmetadata: {name: c}\nstringData:\n  t: {u: \"${exchange:temporal}\"}\n"},
		{"template secret with text", "kind: Secret\nmetadata: {name: c}\nstringData:\n  t: \"x${templateSecret:keys/text}\"\n"},
		{"unknown exchange", "kind: Secret\nmetadata: {name: c}\nstringData:\n  t: \"${exchange:other}\"\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := newInput(t, "per-user", "apiVersion: v1\n"+test.resource)
			input.ExchangeTokens = map[string]string{"temporal": "tok-secret"}
			input.TemplateSecrets = fakeTemplateSecrets{values: map[string][]byte{"keys/text": []byte("v")}}

			_, err := toolgateruntime.Render(t.Context(), input)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "tok-secret")
		})
	}
}

func TestRenderExchangeNeedsPerUserScope(t *testing.T) {
	input := newInput(t, "shared",
		"apiVersion: v1\nkind: Secret\nmetadata: {name: c}\nstringData:\n  t: \"${exchange:temporal}\"\n")
	input.ExchangeTokens = map[string]string{"temporal": "tok"}

	_, err := toolgateruntime.Render(t.Context(), input)
	require.Error(t, err)
}

func TestPatchSecretKeysSendsOnePatchPerSecret(t *testing.T) {
	type patchCall struct {
		key       client.ObjectKey
		patchType types.PatchType
		body      string
		manager   string
	}

	var calls []patchCall

	kube := clientfake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(_ context.Context, _ client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			body, err := patch.Data(object)
			require.NoError(t, err)

			applied := &client.PatchOptions{}
			applied.ApplyOptions(options)

			calls = append(calls, patchCall{client.ObjectKeyFromObject(object), patch.Type(), string(body), applied.FieldManager})

			return nil
		}}).Build()

	err := toolgateruntime.PatchSecretKeys(t.Context(), kube, []toolgateruntime.SecretKeyValue{
		{Namespace: "tg-1", Secret: "creds", Key: "a", Value: []byte("one")},
		{Namespace: "tg-1", Secret: "other", Key: "z", Value: []byte{0xff}},
		{Namespace: "tg-1", Secret: "creds", Key: "b", Value: []byte("two")},
	})
	require.NoError(t, err)

	require.Len(t, calls, 2)
	assert.Equal(t, patchCall{
		key: client.ObjectKey{Namespace: "tg-1", Name: "creds"}, patchType: types.MergePatchType, manager: "toolgate",
		body: `{"data":{"a":"b25l","b":"dHdv"}}`,
	}, calls[0])
	assert.Equal(t, `{"data":{"z":"/w=="}}`, calls[1].body)
	assert.Equal(t, "toolgate", calls[1].manager)
}
