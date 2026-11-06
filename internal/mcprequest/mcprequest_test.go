package mcprequest_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/mcprequest"
)

func TestStateless(t *testing.T) {
	tests := []struct {
		name     string
		revision string
		want     bool
	}{
		{name: "no header", revision: "", want: false},
		{name: "2025-11-25", revision: "2025-11-25", want: false},
		{name: "the day before", revision: "2026-07-27", want: false},
		{name: "2026-07-28", revision: "2026-07-28", want: true},
		{name: "later", revision: "2027-01-01", want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil)
			if test.revision != "" {
				request.Header.Set("Mcp-Protocol-Version", test.revision)
			}

			assert.Equal(t, test.want, mcprequest.Stateless(request))
		})
	}
}

func TestMethod(t *testing.T) {
	discover := `{"jsonrpc":"2.0","id":1,"method":"server/discover"}`
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}`

	tests := []struct {
		name   string
		header string
		body   string
		want   string
	}{
		{name: "header alone", header: "server/discover", body: "", want: "server/discover"},
		{name: "body alone", body: discover, want: "server/discover"},
		{name: "header and body agree", header: "server/discover", body: discover, want: "server/discover"},
		{name: "header claims another method than the body", header: "server/discover", body: call, want: ""},
		{name: "a notification has no request method", body: `{"jsonrpc":"2.0","method":"notifications/initialized"}`, want: "notifications/initialized"},
		{name: "a response", body: `{"jsonrpc":"2.0","id":1,"result":{}}`, want: ""},
		{name: "not JSON", body: "nope", want: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(test.body))
			if test.header != "" {
				request.Header.Set("Mcp-Method", test.header)
			}

			assert.Equal(t, test.want, mcprequest.Method(request))

			// The next handler still reads the whole body.
			rest, err := io.ReadAll(request.Body)
			require.NoError(t, err)
			assert.Equal(t, test.body, string(rest))
		})
	}

	t.Run("a body over the limit is put back whole", func(t *testing.T) {
		body := `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"pad":"` + strings.Repeat("a", mcprequest.MaxBodyBytes) + `"}}`
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(body))

		assert.Empty(t, mcprequest.Method(request))

		rest, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		assert.Len(t, rest, len(body))
	})
}
