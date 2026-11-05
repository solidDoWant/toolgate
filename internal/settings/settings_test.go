package settings_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/settings"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

func profile(namespace, name string, selector v1alpha1.ProfileSelector, fragment string) *v1alpha1.SettingsProfile {
	return &v1alpha1.SettingsProfile{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       v1alpha1.SettingsProfileSpec{Selector: selector, Settings: runtime.RawExtension{Raw: []byte(fragment)}},
	}
}

func TestMatches(t *testing.T) {
	alice := auth.Identity{Principal: "alice", Groups: []string{"dev", "eu"}, Attestation: auth.AttestationUser}

	tests := []struct {
		name     string
		selector v1alpha1.ProfileSelector
		want     bool
	}{
		{name: "group", selector: v1alpha1.ProfileSelector{Groups: []string{"ops", "eu"}}, want: true},
		{name: "other group", selector: v1alpha1.ProfileSelector{Groups: []string{"ops"}}},
		{name: "principal", selector: v1alpha1.ProfileSelector{Principals: []string{"alice"}}, want: true},
		{name: "other principal", selector: v1alpha1.ProfileSelector{Principals: []string{"bob"}}},
		{name: "group and attestation", selector: v1alpha1.ProfileSelector{Groups: []string{"dev"}, Attestation: auth.AttestationUser}, want: true},
		{name: "group and other attestation", selector: v1alpha1.ProfileSelector{Groups: []string{"dev"}, Attestation: auth.AttestationAttested}},
		{name: "empty selector applies to no one", selector: v1alpha1.ProfileSelector{}},
		{name: "attestation alone applies to no one", selector: v1alpha1.ProfileSelector{Attestation: auth.AttestationUser}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, settings.Matches(alice, test.selector))
		})
	}
}

func TestMergeIsPerKeyAndTheLaterProfileWins(t *testing.T) {
	merged, err := settings.Merge([]map[string]json.RawMessage{
		{"model": json.RawMessage(`"opus"`), "env": json.RawMessage(`{"FOO":"1","SHARED":"a"}`), "hooks": json.RawMessage(`{"Stop":[1]}`)},
		{"model": json.RawMessage(`"haiku"`), "env": json.RawMessage(`{"BAR":"2","SHARED":"b"}`), "hooks": json.RawMessage(`{"Start":[2]}`)},
	})
	require.NoError(t, err)

	encoded, err := json.Marshal(merged)
	require.NoError(t, err)
	assert.JSONEq(t, `{"model":"haiku","env":{"FOO":"1","BAR":"2","SHARED":"b"},"hooks":{"Start":[2]}}`, string(encoded),
		"env merges per key; other keys, like hooks, are replaced whole")
}

func TestHandler(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	dev := v1alpha1.ProfileSelector{Groups: []string{"dev"}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		// Listed out of name order: b-later must win over a-first.
		profile("toolgate", "b-later", dev, `{"model":"haiku","env":{"BAR":"2"}}`),
		profile("toolgate", "a-first", dev, `{"model":"opus","env":{"FOO":"1"}}`),
		profile("toolgate", "c-ops", v1alpha1.ProfileSelector{Groups: []string{"ops"}}, `{"theme":"dark"}`),
		profile("toolgate", "d-broken", dev, `{"env":"not an object"}`),
		profile("elsewhere", "e-unwatched", dev, `{"editor":"vim"}`),
	).Build()

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	authenticator, err := auth.New(t.Context(), auth.Options{IssuerURL: provider.Issuer, Audience: provider.Audience})
	require.NoError(t, err)

	mux := http.NewServeMux()
	handler := &settings.Handler{Reader: kube, Namespaces: []string{"toolgate"}}
	handler.Register(mux, func(next http.Handler) http.Handler { return authenticator.Middleware(next, false) })

	get := func(token string) *httptest.ResponseRecorder {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, settings.Path, nil)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}

		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)

		return recorder
	}

	t.Run("unauthenticated is refused", func(t *testing.T) {
		recorder := get("")
		assert.Equal(t, http.StatusUnauthorized, recorder.Code)
		assert.NotContains(t, recorder.Body.String(), "opus")
	})

	t.Run("matching profiles merged in name order", func(t *testing.T) {
		recorder := get(provider.MintAccessToken(t, testutil.TokenClaims{Subject: "alice", Groups: []string{"dev"}}))
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		assert.JSONEq(t, `{"model":"haiku","env":{"FOO":"1","BAR":"2"}}`, recorder.Body.String())
	})

	t.Run("no matching profile", func(t *testing.T) {
		recorder := get(provider.MintAccessToken(t, testutil.TokenClaims{Subject: "bob", Groups: []string{"qa"}}))
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		assert.JSONEq(t, `{}`, recorder.Body.String())
	})
}
