package runtime_test

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/runtime"
)

// resolverServer is a TLS resolver whose answer the test sets.
type resolverServer struct {
	server    *httptest.Server
	caFile    string
	tokenFile string

	mu       sync.Mutex
	status   int
	body     string
	requests []recordedResolve
}

type recordedResolve struct {
	method        string
	authorization string
	contentType   string
	body          map[string]any
}

var resolverNow = time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)

func newResolverServer(t *testing.T) *resolverServer {
	t.Helper()

	resolver := &resolverServer{status: http.StatusOK}

	mux := http.NewServeMux()
	mux.HandleFunc("/resolve", func(writer http.ResponseWriter, request *http.Request) {
		data, _ := io.ReadAll(request.Body)

		var body map[string]any

		_ = json.Unmarshal(data, &body)

		resolver.mu.Lock()
		defer resolver.mu.Unlock()

		resolver.requests = append(resolver.requests, recordedResolve{
			method: request.Method, authorization: request.Header.Get("Authorization"),
			contentType: request.Header.Get("Content-Type"), body: body,
		})

		writer.WriteHeader(resolver.status)
		_, _ = io.WriteString(writer, resolver.body)
	})
	mux.HandleFunc("/redirect", func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/resolve", http.StatusFound)
	})

	resolver.server = httptest.NewTLSServer(mux)
	t.Cleanup(resolver.server.Close)

	directory := t.TempDir()
	resolver.caFile = filepath.Join(directory, "ca.pem")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: resolver.server.Certificate().Raw})
	require.NoError(t, os.WriteFile(resolver.caFile, certificate, 0o600))

	resolver.tokenFile = filepath.Join(directory, "token")
	require.NoError(t, os.WriteFile(resolver.tokenFile, []byte("token-1\n"), 0o600))

	return resolver
}

func (s *resolverServer) answer(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.status, s.body = status, body
}

func (s *resolverServer) client(t *testing.T, path string) *runtime.HTTPResolvers {
	t.Helper()

	resolvers, err := runtime.NewHTTPResolvers([]runtime.ResolverConfig{{
		Name: "github-app", URL: s.server.URL + path, Timeout: 10 * time.Second,
		CAFile: s.caFile, TokenFile: s.tokenFile,
	}})
	require.NoError(t, err)

	resolvers.Now = func() time.Time { return resolverNow }

	return resolvers
}

func validAnswer(value string, expiresAt time.Time) string {
	encoded, _ := json.Marshal(map[string]string{"value": value, "expiresAt": expiresAt.Format(time.RFC3339)})

	return string(encoded)
}

func TestHTTPResolversSendsTheRequest(t *testing.T) {
	resolver := newResolverServer(t)
	resolver.answer(http.StatusOK, validAnswer("value-1", resolverNow.Add(time.Hour)))
	client := resolver.client(t, "/resolve")

	request := runtime.ResolveRequest{
		Resolver:  "github-app",
		Template:  runtime.ResolveTemplate{Namespace: "catalog", Name: "github"},
		Instance:  "instance-1",
		Principal: runtime.ResolvePrincipal{ID: "sub-1", Email: "a@example.com", Name: "Alice", Groups: []string{"dev"}},
		Workspace: "ws-1",
		Session:   "session-1",
	}

	resolved, err := client.Resolve(t.Context(), request)
	require.NoError(t, err)
	assert.Equal(t, "value-1", resolved.Value)
	assert.True(t, resolverNow.Add(time.Hour).Equal(resolved.ExpiresAt))

	// The token file is read for each request.
	require.NoError(t, os.WriteFile(resolver.tokenFile, []byte("token-2"), 0o600))
	_, err = client.Resolve(t.Context(), runtime.ResolveRequest{Resolver: "github-app"})
	require.NoError(t, err)

	require.Len(t, resolver.requests, 2)
	first := resolver.requests[0]
	assert.Equal(t, http.MethodPost, first.method)
	assert.Equal(t, "Bearer token-1", first.authorization)
	assert.Equal(t, "application/json", first.contentType)
	assert.Equal(t, map[string]any{
		"resolver":  "github-app",
		"template":  map[string]any{"namespace": "catalog", "name": "github"},
		"instance":  "instance-1",
		"principal": map[string]any{"id": "sub-1", "email": "a@example.com", "name": "Alice", "groups": []any{"dev"}},
		"workspace": "ws-1",
		"session":   "session-1",
	}, first.body)

	second := resolver.requests[1]
	assert.Equal(t, "Bearer token-2", second.authorization)
	assert.NotContains(t, second.body, "workspace")
	assert.NotContains(t, second.body, "session")
}

func TestHTTPResolversRefusesBadAnswers(t *testing.T) {
	const secretValue = "s3cr3t-value"

	// padded returns a valid answer whose body is exactly size bytes.
	padded := func(size int) string {
		base := validAnswer("", resolverNow.Add(time.Hour))
		return strings.Replace(base, `"value":""`, `"value":"`+strings.Repeat("v", size-len(base))+`"`, 1)
	}

	tests := []struct {
		name       string
		path       string
		status     int
		body       string
		wantReason string
	}{
		{name: "exactly 1 MiB", path: "/resolve", status: http.StatusOK, body: padded(1 << 20)},
		{name: "expiry exactly 2 minutes away", path: "/resolve", status: http.StatusOK, body: validAnswer(secretValue, resolverNow.Add(2*time.Minute))},
		{name: "HTTP status", path: "/resolve", status: http.StatusServiceUnavailable, body: secretValue, wantReason: "resolver `github-app` answered HTTP 503"},
		{name: "redirect", path: "/redirect", status: http.StatusOK, body: validAnswer(secretValue, resolverNow.Add(time.Hour)), wantReason: "resolver `github-app` answered HTTP 302"},
		{name: "over 1 MiB", path: "/resolve", status: http.StatusOK, body: padded(1<<20 + 1), wantReason: "more than 1 MiB"},
		{name: "empty value", path: "/resolve", status: http.StatusOK, body: validAnswer("", resolverNow.Add(time.Hour)), wantReason: "without a `value`"},
		{name: "no expiresAt", path: "/resolve", status: http.StatusOK, body: `{"value":"` + secretValue + `"}`, wantReason: "without an `expiresAt`"},
		{name: "bad expiresAt", path: "/resolve", status: http.StatusOK, body: `{"value":"` + secretValue + `","expiresAt":"soon"}`, wantReason: "isn't RFC 3339"},
		{name: "expiry under 2 minutes away", path: "/resolve", status: http.StatusOK, body: validAnswer(secretValue, resolverNow.Add(2*time.Minute-time.Second)), wantReason: "less than 2 minutes away"},
		{name: "not JSON", path: "/resolve", status: http.StatusOK, body: secretValue, wantReason: "isn't a JSON object"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := newResolverServer(t)
			resolver.answer(test.status, test.body)

			_, err := resolver.client(t, test.path).Resolve(t.Context(), runtime.ResolveRequest{Resolver: "github-app"})
			if test.wantReason == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantReason)
			assert.NotContains(t, err.Error(), secretValue)
		})
	}
}
