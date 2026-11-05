package health_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/health"
)

func TestProbes(t *testing.T) {
	passing := health.Check{Name: "passing", Run: func(context.Context) error { return nil }}
	failing := health.Check{Name: "failing", Run: func(context.Context) error { return errors.New("down") }}

	tests := []struct {
		name       string
		checks     []health.Check
		path       string
		wantStatus int
		wantBody   string
	}{
		{name: "healthz ignores checks", checks: []health.Check{failing}, path: "/healthz", wantStatus: http.StatusOK},
		{name: "readyz with passing checks", checks: []health.Check{passing}, path: "/readyz", wantStatus: http.StatusOK},
		{name: "readyz with a failing check", checks: []health.Check{passing, failing}, path: "/readyz", wantStatus: http.StatusServiceUnavailable, wantBody: "failing: unavailable"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mux := http.NewServeMux()
			health.Register(mux, slog.New(slog.DiscardHandler), test.checks...)

			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, test.path, nil))

			body, err := io.ReadAll(recorder.Result().Body)
			require.NoError(t, err)
			assert.Equal(t, test.wantStatus, recorder.Code)
			assert.Contains(t, string(body), test.wantBody)
			assert.NotContains(t, string(body), "down")
		})
	}
}
