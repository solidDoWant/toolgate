package resolver_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/test/resolver"
)

func post(t *testing.T, handler http.Handler, token string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(`{"resolver":"test"}`))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	return recorder
}

func TestResolverCountsWithTheSetLifetime(t *testing.T) {
	handler := resolver.New(3 * time.Minute)

	for _, want := range []string{"1", "2"} {
		before := time.Now()
		recorder := post(t, handler, "token")
		require.Equal(t, http.StatusOK, recorder.Code)

		var answer struct {
			Value     string    `json:"value"`
			ExpiresAt time.Time `json:"expiresAt"`
		}
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &answer))
		assert.Equal(t, want, answer.Value)
		assert.WithinDuration(t, before.Add(3*time.Minute), answer.ExpiresAt, 5*time.Second)
	}

	handler.SetLifetime(time.Hour)
	recorder := post(t, handler, "token")

	var answer struct {
		ExpiresAt time.Time `json:"expiresAt"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &answer))
	assert.WithinDuration(t, time.Now().Add(time.Hour), answer.ExpiresAt, 5*time.Second)

	requests := handler.Requests()
	require.Len(t, requests, 3)
	assert.Equal(t, "token", requests[0].Token)
	assert.Equal(t, "test", requests[0].Body["resolver"])
}

func TestResolverRequiresAToken(t *testing.T) {
	handler := resolver.New(time.Hour)
	assert.Equal(t, http.StatusUnauthorized, post(t, handler, "").Code)
	assert.Empty(t, handler.Requests())
}

func TestWriteCertificateServesTLS(t *testing.T) {
	directory := t.TempDir()
	require.NoError(t, resolver.WriteCertificate(directory, "127.0.0.1"))

	certificate, err := tls.LoadX509KeyPair(filepath.Join(directory, "cert.pem"), filepath.Join(directory, "key.pem"))
	require.NoError(t, err)

	server := httptest.NewUnstartedServer(resolver.New(time.Hour))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)

	caPEM, err := os.ReadFile(filepath.Join(directory, "cert.pem"))
	require.NoError(t, err)

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(caPEM))

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(`{}`))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer token")

	response, err := client.Do(request)
	require.NoError(t, err)

	defer func() { _ = response.Body.Close() }()

	assert.Equal(t, http.StatusOK, response.StatusCode)
}
