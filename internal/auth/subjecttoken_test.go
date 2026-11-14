package auth_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/solidDoWant/toolgate/internal/auth"
	tgtestutil "github.com/solidDoWant/toolgate/internal/testutil"
)

func TestSubjectTokenIsNeverPrinted(t *testing.T) {
	t.Parallel()

	const secret = "eyJhbGciOiJSUzI1NiJ9.secret-subject-token.signature"

	token := auth.SubjectToken(secret)

	type holder struct {
		Name  string
		Token auth.SubjectToken
	}

	wrapped := holder{Name: "request", Token: token}

	values := map[string]any{
		"value":            token,
		"struct":           wrapped,
		"pointer":          &wrapped,
		"pointer to value": &token,
	}

	for name, value := range values {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
				assert.NotContains(t, fmt.Sprintf(verb, value), "secret-subject-token", verb)
			}

			for _, format := range []string{"json", "text"} {
				var buffer bytes.Buffer

				var handler slog.Handler = slog.NewJSONHandler(&buffer, nil)
				if format == "text" {
					handler = slog.NewTextHandler(&buffer, nil)
				}

				slog.New(handler).Info("request", "token", value)
				assert.NotContains(t, buffer.String(), "secret-subject-token", format)
			}

			encoded, err := json.Marshal(value)
			require.Error(t, err)
			assert.NotContains(t, string(encoded), "secret-subject-token")
		})
	}

	t.Run("reveal returns the token", func(t *testing.T) {
		t.Parallel()

		assert.Equal(t, secret, token.Reveal())
	})
}

func TestUserRequestCarriesItsSubjectToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		headers map[string]string
	}{
		{name: "plugin session", headers: map[string]string{auth.SessionHeader: "s-1"}},
		{name: "connector without session header"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, auth.Options{}, true)
			token := f.provider.MintAccessToken(t, tgtestutil.TokenClaims{Subject: "alice"})

			response := f.do(t, token, test.headers)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Equal(t, token, response.Header().Get(subjectTokenHeader))
			assert.NotContains(t, response.Body.String(), token, "the identity holds no token")
		})
	}
}

func TestWorkloadIdentitiesGetNoSubjectToken(t *testing.T) {
	t.Parallel()

	t.Run("service account", func(t *testing.T) {
		t.Parallel()

		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace:       "ci",
			Name:            "runner-abc",
			UID:             "pod-uid-1",
			OwnerReferences: controller("batch/v1", "Job", "runner", "job-uid"),
		}}
		f, server := newPodFixture(t, []string{"ci"}, pod)

		token := server.issue(t, podToken{namespace: "ci", serviceAccount: "runner", podName: "runner-abc", podUID: "pod-uid-1", audiences: []string{"toolgate"}})

		response := f.do(t, token, nil)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Equal(t, auth.KindServiceAccount, decodeIdentity(t, response.Body.Bytes()).Kind)
		assert.Empty(t, response.Header().Values(subjectTokenHeader))
	})

	t.Run("SPIFFE", func(t *testing.T) {
		t.Parallel()

		key := newSigningKey(t, "key-1")
		endpoint := startBundleEndpoint(t, "allowed.test", key)
		f := newSPIFFEFixture(t, &fakeClock{now: time.Now()}, endpoint.trustDomainConfig())

		token := key.sign(t, svidClaims{subject: allowedID, expiry: time.Now().Add(20 * time.Minute)})
		waitForBundle(t, f, token)

		response := f.do(t, token, nil)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Equal(t, auth.KindSPIFFE, decodeIdentity(t, response.Body.Bytes()).Kind)
		assert.Empty(t, response.Header().Values(subjectTokenHeader))
	})
}
