package main

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/apidoc"
	"github.com/solidDoWant/toolgate/internal/audittail"
	"github.com/solidDoWant/toolgate/internal/secrets"
	"github.com/solidDoWant/toolgate/internal/sessionsapi"
	"github.com/solidDoWant/toolgate/internal/settings"
	"github.com/solidDoWant/toolgate/internal/workspacesapi"
)

// recordingMux records the patterns registered on it.
type recordingMux struct{ patterns []string }

func (m *recordingMux) Handle(pattern string, _ http.Handler) {
	m.patterns = append(m.patterns, pattern)
}

func passThrough(next http.Handler) http.Handler { return next }

func TestOperationsAreExactlyTheRegisteredAPIRoutes(t *testing.T) {
	mux := &recordingMux{}
	secrets.NewAPI(nil, nil, nil).Register(mux, passThrough)
	(&settings.Handler{}).Register(mux, passThrough)
	(&sessionsapi.Handler{}).Register(mux, passThrough)
	audittail.New(audittail.Options{}).Register(mux, passThrough)
	(&workspacesapi.Handler{}).Register(mux, passThrough)

	var routes []string

	for _, pattern := range mux.patterns {
		if strings.Contains(pattern, " /api/v1/") {
			routes = append(routes, pattern)
		}
	}

	var documented []string
	for _, operation := range Operations() {
		documented = append(documented, operation.Pattern())
	}

	slices.Sort(routes)
	slices.Sort(documented)
	assert.Equal(t, routes, documented)
	assert.NotEmpty(t, routes)
}

func TestBuildDocumentsEveryOperation(t *testing.T) {
	document, err := apidoc.Build("t", "v1", Operations())
	require.NoError(t, err)

	text := string(document)
	for _, path := range []string{"/api/v1/secrets/{name}:", "/api/v1/secrets:", "/api/v1/settings:", "/api/v1/sessions:", "/api/v1/audit/tail:", "/api/v1/workspaces:", "/api/v1/workspaces/changes:", "/api/v1/access-requests/{id}:"} {
		assert.Contains(t, text, path)
	}
}
