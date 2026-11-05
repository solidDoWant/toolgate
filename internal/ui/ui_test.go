package ui_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/ui"
)

// muxRegistrar records which patterns were registered as public.
type muxRegistrar struct {
	mux    *http.ServeMux
	public []string
}

func newMuxRegistrar() *muxRegistrar {
	return &muxRegistrar{mux: http.NewServeMux()}
}

func (r *muxRegistrar) HandlePublic(pattern string, handler http.Handler) {
	r.mux.Handle(pattern, handler)
	r.public = append(r.public, pattern)
}

func (r *muxRegistrar) Handle(pattern string, handler http.Handler) {
	r.mux.Handle(pattern, handler)
}

func newUI(t *testing.T) (*ui.UI, *muxRegistrar) {
	t.Helper()

	registrar := newMuxRegistrar()
	webUI := ui.New(ui.Options{IssuerURL: "https://idp.invalid", ClientID: "toolgate-ui", PublicURL: "https://toolgate.example"}, registrar)

	return webUI, registrar
}

func TestOnlySignInAndStaticRoutesArePublic(t *testing.T) {
	webUI, registrar := newUI(t)

	assert.ElementsMatch(t, []string{"GET /ui/login", "GET /ui/auth/callback", "GET /ui/static/"}, registrar.public)

	var public []string

	for _, route := range webUI.Routes() {
		if route.Access == ui.Public {
			public = append(public, route.Pattern)
		}
	}

	assert.ElementsMatch(t, registrar.public, public)
}

func TestRequestsWithoutSessionAreRefused(t *testing.T) {
	webUI, registrar := newUI(t)

	marker := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusTeapot)
	})

	webUI.Handle("GET /ui/servers", ui.User, marker)
	webUI.Handle("POST /ui/servers", ui.User, marker)
	webUI.Handle("GET /ui/blocks", ui.Admin, marker)
	webUI.Handle("POST /ui/blocks", ui.Admin, marker)

	tests := []struct {
		name         string
		method       string
		target       string
		cookie       bool
		wantStatus   int
		wantLocation string
	}{
		{name: "GET of a user page redirects to sign-in", method: http.MethodGet, target: "/ui/servers?tab=1", wantStatus: http.StatusSeeOther,
			wantLocation: "/ui/login?return_to=" + url.QueryEscape("/ui/servers?tab=1")},
		{name: "GET of an admin page redirects to sign-in", method: http.MethodGet, target: "/ui/blocks", wantStatus: http.StatusSeeOther,
			wantLocation: "/ui/login?return_to=" + url.QueryEscape("/ui/blocks")},
		{name: "GET of the root redirects to sign-in", method: http.MethodGet, target: "/", wantStatus: http.StatusSeeOther,
			wantLocation: "/ui/login?return_to=" + url.QueryEscape("/")},
		{name: "POST of a user page gets 401", method: http.MethodPost, target: "/ui/servers", wantStatus: http.StatusUnauthorized},
		{name: "POST of an admin page gets 401", method: http.MethodPost, target: "/ui/blocks", wantStatus: http.StatusUnauthorized},
		{name: "logout without a session gets 401", method: http.MethodPost, target: "/ui/logout", wantStatus: http.StatusUnauthorized},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			registrar.mux.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), test.method, test.target, nil))

			assert.Equal(t, test.wantStatus, recorder.Code)
			assert.Equal(t, test.wantLocation, recorder.Header().Get("Location"))
		})
	}
}

func TestReturnToStaysUnderUI(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "/ui/grants/new?template=github", want: "/ui/grants/new?template=github"},
		{raw: "/ui/approvals/0190", want: "/ui/approvals/0190"},
		{raw: "", want: ui.HomePath},
		{raw: "/", want: ui.HomePath},
		{raw: "/ui", want: ui.HomePath},
		{raw: "/api/v1/secrets", want: ui.HomePath},
		{raw: "https://evil.example/ui/servers", want: ui.HomePath},
		{raw: "//evil.example/ui/servers", want: ui.HomePath},
		{raw: "/ui/\\evil.example", want: ui.HomePath},
		{raw: "/ui/servers\r\nSet-Cookie: x=y", want: ui.HomePath},
		{raw: "/ui/" + strings.Repeat("a", 2048), want: ui.HomePath},
	}

	for _, test := range tests {
		t.Run(test.raw, func(t *testing.T) {
			assert.Equal(t, test.want, ui.ReturnTo(test.raw))
		})
	}
}

func TestStaticFilesArePublic(t *testing.T) {
	_, registrar := newUI(t)

	recorder := httptest.NewRecorder()
	registrar.mux.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ui/static/toolgate.css", nil))

	require.Equal(t, http.StatusOK, recorder.Code)
}
