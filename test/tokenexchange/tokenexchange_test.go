package tokenexchange_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/test/tokenexchange"
)

func subjectToken(t *testing.T, subject string) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, nil)
	require.NoError(t, err)

	token, err := jwt.Signed(signer).Claims(jwt.Claims{Subject: subject}).Serialize()
	require.NoError(t, err)

	return token
}

func exchange(t *testing.T, handler http.Handler, form url.Values, secret string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if secret != "" {
		request.SetBasicAuth("client", secret)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	return recorder
}

func exchangeForm(t *testing.T, subject string) url.Values {
	t.Helper()

	return url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":      {subjectToken(t, subject)},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
	}
}

func TestEndpointCountsPerSubjectWithTheSetLifetime(t *testing.T) {
	handler := tokenexchange.New(3 * time.Minute)

	for _, want := range []string{"alice-1", "bob-1", "alice-2"} {
		subject, _, _ := strings.Cut(want, "-")
		recorder := exchange(t, handler, exchangeForm(t, subject), "secret")
		require.Equal(t, http.StatusOK, recorder.Code)

		var answer map[string]any
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &answer))
		assert.Equal(t, "exchanged-"+want, answer["access_token"])
		assert.InDelta(t, 180, answer["expires_in"], 0)
	}

	handler.SetLifetime(10 * time.Minute)

	recorder := exchange(t, handler, exchangeForm(t, "bob"), "secret")
	assert.Contains(t, recorder.Body.String(), `"expires_in":600`)
	assert.Len(t, handler.Requests(), 4)
}

func TestEndpointRefusesASubjectWithTheSetCode(t *testing.T) {
	handler := tokenexchange.New(3 * time.Minute)
	handler.Refuse("carol", "invalid_grant")

	recorder := exchange(t, handler, exchangeForm(t, "carol"), "secret")
	assert.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"error":"invalid_grant"`)

	handler.Allow("carol")
	assert.Equal(t, http.StatusOK, exchange(t, handler, exchangeForm(t, "carol"), "secret").Code)
}

func TestEndpointRefusesBadRequests(t *testing.T) {
	handler := tokenexchange.New(time.Minute)

	wrongGrant := exchangeForm(t, "alice")
	wrongGrant.Set("grant_type", "client_credentials")

	notAJWT := exchangeForm(t, "alice")
	notAJWT.Set("subject_token", "opaque")

	tests := []struct {
		name   string
		form   url.Values
		secret string
		status int
	}{
		{"no client secret", exchangeForm(t, "alice"), "", http.StatusUnauthorized},
		{"wrong grant type", wrongGrant, "secret", http.StatusBadRequest},
		{"subject token that is not a JWT", notAJWT, "secret", http.StatusBadRequest},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.status, exchange(t, handler, test.form, test.secret).Code)
		})
	}
}
