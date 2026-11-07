package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/secrets"
)

type noSecrets struct{}

func (noSecrets) Set(context.Context, string, string, []byte) (secrets.Info, error) {
	return secrets.Info{}, nil
}

func (noSecrets) List(context.Context, string) ([]secrets.Info, error) { return nil, nil }

func (noSecrets) Delete(context.Context, string, string) error { return nil }

type nopRegistrar struct{ mux *http.ServeMux }

func (r nopRegistrar) HandlePublic(pattern string, handler http.Handler) {
	r.mux.Handle(pattern, handler)
}
func (r nopRegistrar) Handle(pattern string, handler http.Handler) { r.mux.Handle(pattern, handler) }

func TestNavLinksToSecretsOnlyWhenThePageIsRegistered(t *testing.T) {
	tests := []struct {
		name  string
		store SecretStore
		want  bool
	}{
		{name: "keyset configured", store: noSecrets{}, want: true},
		{name: "no keyset", store: nil, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mux := http.NewServeMux()
			webUI := New(Options{IssuerURL: "https://idp.invalid", ClientID: "toolgate-ui", PublicURL: "https://toolgate.example", Secrets: test.store}, nopRegistrar{mux})

			recorder := httptest.NewRecorder()
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ui/secrets", nil)
			webUI.render(recorder, request, "secrets", "Secrets", secretsPage{})

			require.Equal(t, http.StatusOK, recorder.Code)
			assert.Equal(t, test.want, contains(recorder.Body.String(), `href="/ui/secrets"`))

			routed := httptest.NewRecorder()
			mux.ServeHTTP(routed, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ui/secrets", nil))

			if test.want {
				assert.NotEqual(t, http.StatusNotFound, routed.Code)
			} else {
				assert.Equal(t, http.StatusNotFound, routed.Code)
			}
		})
	}
}

func contains(body, substring string) bool {
	return len(body) >= len(substring) && strings.Contains(body, substring)
}
