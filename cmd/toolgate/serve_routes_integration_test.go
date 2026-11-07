//go:build integration

package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/apidoc/operations"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

var pathParameter = regexp.MustCompile(`\{[^}]+\}`)

func TestServeMountsEveryDocumentedAPIRoute(t *testing.T) {
	gateway := startServeWithSecrets(t)
	defer stopServe(t, gateway.stderr, gateway.done)

	for _, operation := range operations.All() {
		t.Run(operation.Pattern()+" is refused without a token", func(t *testing.T) {
			path := pathParameter.ReplaceAllString(operation.Path, "x")

			request, err := http.NewRequestWithContext(t.Context(), operation.Method, gateway.url+path, nil)
			require.NoError(t, err)

			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err)

			_ = response.Body.Close()

			assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
		})
	}
}

func TestServeSettingsRouteReturnsTheMatchingProfile(t *testing.T) {
	gateway := startServeWithSecrets(t)
	defer stopServe(t, gateway.stderr, gateway.done)

	require.NoError(t, gateway.kube.Client.Create(t.Context(), &v1alpha1.SettingsProfile{
		ObjectMeta: metav1.ObjectMeta{Namespace: serveCatalogNamespace, Name: "eng"},
		Spec: v1alpha1.SettingsProfileSpec{
			Selector: v1alpha1.ProfileSelector{Groups: []string{"eng"}},
			Settings: k8sruntime.RawExtension{Raw: []byte(`{"model":"opus"}`)},
		},
	}))

	fetch := func(t *testing.T, claims testutil.TokenClaims) map[string]any {
		t.Helper()

		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, gateway.url+"/api/v1/settings", nil)
		require.NoError(t, err)

		request.Header.Set("Authorization", "Bearer "+gateway.provider.MintAccessToken(t, claims))

		response, err := http.DefaultClient.Do(request)
		require.NoError(t, err)

		defer func() { _ = response.Body.Close() }()

		require.Equal(t, http.StatusOK, response.StatusCode)

		body := map[string]any{}
		require.NoError(t, json.NewDecoder(response.Body).Decode(&body))

		return body
	}

	selected := testutil.TokenClaims{Subject: "alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"eng"}}
	other := testutil.TokenClaims{Subject: "bob", Email: "bob@example.com", EmailVerified: true, Groups: []string{"ops"}}

	require.Eventually(t, func() bool {
		return fetch(t, selected)["model"] == "opus"
	}, time.Minute, 50*time.Millisecond, "the selected user gets the profile")

	assert.Equal(t, map[string]any{}, fetch(t, other), "a user that no profile selects gets an empty object")
}
