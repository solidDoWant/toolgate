package credentials_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/credentials"
)

const (
	testSubjectToken = "subject-token-value"
	testClientSecret = "client-secret-value"
)

// tokenEndpoint is a TLS token endpoint answering with handler, and an entry
// pointing at it with its certificate as caFile.
func tokenEndpoint(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *endpointEntry) {
	t.Helper()

	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)

	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600))

	secretFile := filepath.Join(dir, "client-secret")
	require.NoError(t, os.WriteFile(secretFile, []byte(testClientSecret+"\n"), 0o600))

	entry := &endpointEntry{
		ExchangeSpec: credentials.ExchangeSpec{
			Name:     "temporal",
			TokenURL: server.URL + "/token",
			ClientID: "toolgate-client",
			Timeout:  10 * time.Second,
		},
		CAFile:           caFile,
		ClientSecretFile: secretFile,
	}
	entry.ClientSecret = func(context.Context) ([]byte, error) {
		content, err := os.ReadFile(entry.ClientSecretFile)
		if err != nil {
			return nil, errors.New("the test secret file can't be read")
		}

		return content, nil
	}

	return server, entry
}

// endpointEntry is an exchange spec with the CA file of its endpoint and
// the file its client secret is read from.
type endpointEntry struct {
	credentials.ExchangeSpec

	CAFile           string
	ClientSecretFile string
}

func buildExchanger(t *testing.T, entry *endpointEntry, metrics *credentials.Metrics, logger *slog.Logger) *credentials.Exchanger {
	t.Helper()

	transport, err := credentials.NewTransport(entry.CAFile)
	require.NoError(t, err)

	return credentials.NewExchanger(entry.ExchangeSpec, transport, metrics, logger)
}

func answerJSON(writer http.ResponseWriter, status int, body map[string]any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}

func signedJWT(t *testing.T, expiry time.Time) string {
	t.Helper()

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: bytes.Repeat([]byte("k"), 32)}, nil)
	require.NoError(t, err)

	token, err := jwt.Signed(signer).Claims(jwt.Claims{Subject: "alice", Expiry: jwt.NewNumericDate(expiry)}).Serialize()
	require.NoError(t, err)

	return token
}

func newExchanger(t *testing.T, entry *endpointEntry, now time.Time) (*credentials.Exchanger, *credentials.Metrics) {
	t.Helper()

	metrics := credentials.NewMetrics()
	exchanger := buildExchanger(t, entry, metrics, nil)

	exchanger.Now = func() time.Time { return now }

	return exchanger, metrics
}

func TestExchangeSendsTokenExchangeGrant(t *testing.T) {
	var redirected atomic.Bool

	var form url.Values

	var user, password string

	var basic bool

	var server *httptest.Server

	server, entry := tokenEndpoint(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirect" {
			http.Redirect(writer, request, server.URL+"/elsewhere", http.StatusTemporaryRedirect)

			return
		}

		if request.URL.Path == "/elsewhere" {
			redirected.Store(true)
			answerJSON(writer, http.StatusOK, map[string]any{"access_token": "x", "expires_in": 3600})

			return
		}

		require.NoError(t, request.ParseForm())
		form = request.PostForm
		user, password, basic = request.BasicAuth()

		answerJSON(writer, http.StatusOK, map[string]any{
			"access_token":      "exchanged",
			"token_type":        "Bearer",
			"expires_in":        3600,
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
		})
	})

	entry.Audience = "temporal-client-id"
	entry.Scope = "openid  temporal-permissions"
	entry.RequestedTokenType = "urn:ietf:params:oauth:token-type:access_token"

	now := time.Now()
	exchanger, metrics := newExchanger(t, entry, now)

	exchanged, err := exchanger.Exchange(t.Context(), auth.SubjectToken(testSubjectToken), credentials.KindStart)
	require.NoError(t, err)

	assert.Equal(t, "exchanged", string(exchanged.Token))
	assert.Equal(t, now, exchanged.IssuedAt)
	assert.Equal(t, now.Add(time.Hour), exchanged.ExpiresAt)

	assert.Equal(t, url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":        {testSubjectToken},
		"subject_token_type":   {"urn:ietf:params:oauth:token-type:access_token"},
		"audience":             {"temporal-client-id"},
		"scope":                {"openid temporal-permissions"},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
	}, form, "the form has exactly the token exchange fields, never resource or a client secret")
	assert.True(t, basic, "the client authenticates with HTTP Basic")
	assert.Equal(t, "toolgate-client", user)
	assert.Equal(t, testClientSecret, password, "trailing white space of the secret file is trimmed")
	assert.Equal(t, 1.0, testutil.ToFloat64(metrics.Exchanges.WithLabelValues("temporal", "start", "success")))

	t.Run("optional fields left out", func(t *testing.T) {
		bare := entry
		bare.Audience, bare.Scope, bare.RequestedTokenType = "", "", ""
		exchanger, _ := newExchanger(t, bare, now)

		_, err := exchanger.Exchange(t.Context(), auth.SubjectToken(testSubjectToken), credentials.KindStart)
		require.NoError(t, err)
		assert.Equal(t, url.Values{
			"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
			"subject_token":      {testSubjectToken},
			"subject_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		}, form)
	})

	t.Run("redirect not followed", func(t *testing.T) {
		redirecting := entry
		redirecting.TokenURL = server.URL + "/redirect"
		exchanger, _ := newExchanger(t, redirecting, now)

		_, err := exchanger.Exchange(t.Context(), auth.SubjectToken(testSubjectToken), credentials.KindStart)
		require.EqualError(t, err, "Token exchange `temporal` failed: the token endpoint answered HTTP 307.")
		assert.False(t, redirected.Load(), "the redirect target got no request")
	})
}

func TestExchangeExpiryFromExpiresInOrJWT(t *testing.T) {
	now := time.Now().Truncate(time.Second)

	tests := []struct {
		name   string
		answer map[string]any
		want   time.Time
	}{
		{
			name:   "expires_in only",
			answer: map[string]any{"access_token": "opaque", "expires_in": 600},
			want:   now.Add(10 * time.Minute),
		},
		{
			name:   "JWT exp only",
			answer: map[string]any{"access_token": signedJWT(t, now.Add(20*time.Minute))},
			want:   now.Add(20 * time.Minute),
		},
		{
			name:   "JWT exp earlier than expires_in",
			answer: map[string]any{"access_token": signedJWT(t, now.Add(5*time.Minute)), "expires_in": 3600},
			want:   now.Add(5 * time.Minute),
		},
		{
			name:   "expires_in earlier than JWT exp",
			answer: map[string]any{"access_token": signedJWT(t, now.Add(time.Hour)), "expires_in": 300},
			want:   now.Add(5 * time.Minute),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, entry := tokenEndpoint(t, func(writer http.ResponseWriter, _ *http.Request) {
				answerJSON(writer, http.StatusOK, test.answer)
			})
			exchanger, _ := newExchanger(t, entry, now)

			exchanged, err := exchanger.Exchange(t.Context(), auth.SubjectToken(testSubjectToken), credentials.KindRefresh)
			require.NoError(t, err)
			assert.Equal(t, now, exchanged.IssuedAt)
			assert.Equal(t, test.want, exchanged.ExpiresAt)
		})
	}
}

func TestExchangeRefusesBadAnswers(t *testing.T) {
	now := time.Now().Truncate(time.Second)

	tests := []struct {
		name          string
		answer        map[string]any
		requestedType string
		wantReason    string
		errorFunc     require.ErrorAssertionFunc
	}{
		{
			name:       "no access token",
			answer:     map[string]any{"expires_in": 3600},
			wantReason: "the answer has no access token",
		},
		{
			name:       "empty access token",
			answer:     map[string]any{"access_token": "", "expires_in": 3600},
			wantReason: "the answer has no access token",
		},
		{
			name:       "no expiry",
			answer:     map[string]any{"access_token": "opaque"},
			wantReason: "the answer has no expiry",
		},
		{
			name:       "expires_in under 2 minutes",
			answer:     map[string]any{"access_token": "opaque", "expires_in": 119},
			wantReason: "the answer has an expiry less than 2 minutes away",
		},
		{
			name:   "expires_in exactly 2 minutes",
			answer: map[string]any{"access_token": "opaque", "expires_in": 120},
		},
		{
			name:       "JWT exp under 2 minutes",
			answer:     map[string]any{"access_token": signedJWT(t, now.Add(119*time.Second)), "expires_in": 3600},
			wantReason: "the answer has an expiry less than 2 minutes away",
		},
		{
			name:       "token over 4 KiB",
			answer:     map[string]any{"access_token": strings.Repeat("a", 4097), "expires_in": 3600},
			wantReason: "the answer has a token larger than 4 KiB",
		},
		{
			name:   "token of exactly 4 KiB",
			answer: map[string]any{"access_token": strings.Repeat("a", 4096), "expires_in": 3600},
		},
		{
			name:          "different issued_token_type",
			answer:        map[string]any{"access_token": "opaque", "expires_in": 3600, "issued_token_type": "urn:ietf:params:oauth:token-type:id_token"},
			requestedType: "urn:ietf:params:oauth:token-type:access_token",
			wantReason:    "the answer has a different `issued_token_type`",
		},
		{
			name:          "missing issued_token_type",
			answer:        map[string]any{"access_token": "opaque", "expires_in": 3600},
			requestedType: "urn:ietf:params:oauth:token-type:access_token",
			wantReason:    "the answer has a different `issued_token_type`",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, entry := tokenEndpoint(t, func(writer http.ResponseWriter, _ *http.Request) {
				answerJSON(writer, http.StatusOK, test.answer)
			})
			entry.RequestedTokenType = test.requestedType
			exchanger, metrics := newExchanger(t, entry, now)

			_, err := exchanger.Exchange(t.Context(), auth.SubjectToken(testSubjectToken), credentials.KindStart)
			if test.wantReason == "" {
				require.NoError(t, err)

				return
			}

			require.EqualError(t, err, "Token exchange `temporal` failed: "+test.wantReason)

			var exchangeErr *credentials.ExchangeError
			require.ErrorAs(t, err, &exchangeErr)
			assert.Equal(t, "bad-answer", exchangeErr.Code)
			assert.Equal(t, 1.0, testutil.ToFloat64(metrics.Exchanges.WithLabelValues("temporal", "start", "error")))
		})
	}
}

func TestExchangeErrorsNameCodeOnly(t *testing.T) {
	const secretInBody = "body-secret-marker"

	// The slow endpoint holds its answer until the exchange gave up.
	release := make(chan struct{})

	longDescription := "first line\nsecond\x07line " + strings.Repeat("d", 300) + " " + secretInBody

	tests := []struct {
		name        string
		handler     http.HandlerFunc
		timeout     time.Duration
		closed      bool
		wantReason  string
		wantCode    string
		wantOutcome string
		wantLog     string
	}{
		{
			name: "OAuth error answer",
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				answerJSON(writer, http.StatusBadRequest, map[string]any{
					"error": "invalid_grant", "error_description": longDescription, "access_token": secretInBody,
				})
			},
			wantReason:  "the token endpoint answered `invalid_grant`.",
			wantCode:    "invalid_grant",
			wantOutcome: "refused",
			wantLog:     "first linesecondline " + strings.Repeat("d", 256-len("first linesecondline ")),
		},
		{
			name: "OAuth error code of another form",
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				answerJSON(writer, http.StatusBadRequest, map[string]any{"error": "Invalid Grant " + secretInBody})
			},
			wantReason:  "the token endpoint answered HTTP 400.",
			wantCode:    "http-status",
			wantOutcome: "error",
		},
		{
			name: "other HTTP status",
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(http.StatusBadGateway)
				_, _ = writer.Write([]byte(secretInBody))
			},
			wantReason:  "the token endpoint answered HTTP 502.",
			wantCode:    "http-status",
			wantOutcome: "error",
		},
		{
			name: "timeout",
			handler: func(_ http.ResponseWriter, request *http.Request) {
				select {
				case <-request.Context().Done():
				case <-release:
				}
			},
			timeout:     200 * time.Millisecond,
			wantReason:  "the token endpoint didn't answer within 200ms.",
			wantCode:    "timeout",
			wantOutcome: "error",
		},
		{
			name:        "unreachable",
			handler:     func(http.ResponseWriter, *http.Request) {},
			closed:      true,
			wantReason:  "the token endpoint couldn't be reached.",
			wantCode:    "unreachable",
			wantOutcome: "error",
		},
		{
			name: "unreadable answer",
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte("{" + secretInBody))
			},
			wantReason:  "the answer has no access token",
			wantCode:    "bad-answer",
			wantOutcome: "error",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, entry := tokenEndpoint(t, test.handler)
			if test.timeout != 0 {
				entry.Timeout = test.timeout
			}

			var logs bytes.Buffer

			metrics := credentials.NewMetrics()
			exchanger := buildExchanger(t, entry, metrics, slog.New(slog.NewJSONHandler(&logs, nil)))

			if test.closed {
				server.Close()
			}

			_, err := exchanger.Exchange(t.Context(), auth.SubjectToken(testSubjectToken), credentials.KindRefresh)

			if test.timeout != 0 {
				close(release)
			}

			require.EqualError(t, err, "Token exchange `temporal` failed: "+test.wantReason)

			var exchangeErr *credentials.ExchangeError
			require.ErrorAs(t, err, &exchangeErr)
			assert.Equal(t, test.wantCode, exchangeErr.Code)
			assert.Equal(t, "temporal", exchangeErr.Exchange)
			assert.Equal(t, 1.0, testutil.ToFloat64(metrics.Exchanges.WithLabelValues("temporal", "refresh", test.wantOutcome)))

			for _, text := range []string{err.Error(), fmt.Sprintf("%+v", err), logs.String()} {
				assert.NotContains(t, text, secretInBody)
				assert.NotContains(t, text, testSubjectToken)
				assert.NotContains(t, text, testClientSecret)
			}

			if test.wantLog != "" {
				var record map[string]any
				require.NoError(t, json.Unmarshal(logs.Bytes(), &record))
				assert.Equal(t, "WARN", record["level"])
				assert.Equal(t, test.wantLog, record["description"])
			}
		})
	}
}

func TestClientSecretReadForEachExchange(t *testing.T) {
	var password string

	_, entry := tokenEndpoint(t, func(writer http.ResponseWriter, request *http.Request) {
		_, password, _ = request.BasicAuth()

		answerJSON(writer, http.StatusOK, map[string]any{"access_token": "exchanged", "expires_in": 3600})
	})
	exchanger, _ := newExchanger(t, entry, time.Now())

	exchange := func() error {
		_, err := exchanger.Exchange(t.Context(), auth.SubjectToken(testSubjectToken), credentials.KindRefresh)

		return err
	}

	require.NoError(t, exchange())
	assert.Equal(t, testClientSecret, password)

	require.NoError(t, os.WriteFile(entry.ClientSecretFile, []byte("rotated-secret \t\r\n"), 0o600))
	require.NoError(t, exchange())
	assert.Equal(t, "rotated-secret", password, "a rotated secret is used by the next exchange")

	clientSecretFails := func(t *testing.T) {
		t.Helper()

		err := exchange()
		require.ErrorContains(t, err, "Token exchange `temporal` failed: its client secret can't be read: ")

		var exchangeErr *credentials.ExchangeError
		require.ErrorAs(t, err, &exchangeErr)
		assert.Equal(t, "client-secret", exchangeErr.Code)
	}

	t.Run("empty file", func(t *testing.T) {
		require.NoError(t, os.WriteFile(entry.ClientSecretFile, []byte(" \n"), 0o600))
		clientSecretFails(t)
	})

	t.Run("missing file", func(t *testing.T) {
		require.NoError(t, os.Remove(entry.ClientSecretFile))
		clientSecretFails(t)
	})
}
