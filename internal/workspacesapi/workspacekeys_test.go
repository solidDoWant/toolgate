package workspacesapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/workspacesapi"
)

func TestWorkspaceKeyRoutesNeedAUser(t *testing.T) {
	// The refusals come before any database access, so the service needs
	// no database.
	workspaces := gateway.NewWorkspaces(nil, nil, nil, 0)
	workspaces.SetWorkspaceKeys(true, "")

	routes := []struct{ method, path, body string }{
		{http.MethodPost, workspacesapi.WorkspaceKeysPath, `{"name":"dev","publicKey":"` + strings.Repeat("A", 43) + `"}`},
		{http.MethodGet, workspacesapi.WorkspaceKeysPath, ""},
		{http.MethodDelete, workspacesapi.WorkspaceKeysPath + "/dev", ""},
	}

	serve := func(identity *auth.Identity) *http.ServeMux {
		mux := http.NewServeMux()
		(&workspacesapi.Handler{Workspaces: workspaces, Keys: workspaces, Enabled: true}).Register(mux,
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if identity != nil {
						request = request.WithContext(auth.WithIdentity(request.Context(), *identity))
					}

					next.ServeHTTP(writer, request)
				})
			})

		return mux
	}

	tests := []struct {
		name       string
		identity   *auth.Identity
		wantStatus int
	}{
		{name: "no identity", wantStatus: http.StatusUnauthorized},
		{name: "service account", identity: &auth.Identity{
			Principal: "system:serviceaccount:ci:runner", Username: "runner", Kind: auth.KindServiceAccount, Attestation: auth.AttestationAttested,
		}, wantStatus: http.StatusForbidden},
		{name: "SPIFFE workload", identity: &auth.Identity{
			Principal: "spiffe://example.org/runner", Kind: auth.KindSPIFFE, Attestation: auth.AttestationAttested,
		}, wantStatus: http.StatusForbidden},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mux := serve(test.identity)

			for _, route := range routes {
				request := httptest.NewRequestWithContext(t.Context(), route.method, route.path, strings.NewReader(route.body))
				recorder := httptest.NewRecorder()
				mux.ServeHTTP(recorder, request)

				assert.Equal(t, test.wantStatus, recorder.Code, "%s %s: %s", route.method, route.path, recorder.Body.String())
			}
		})
	}
}
