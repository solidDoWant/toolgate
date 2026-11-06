package auth_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/auth"
	tgtestutil "github.com/solidDoWant/toolgate/internal/testutil"
)

const secondAudience = "coder-toolgate"

func TestTokensForAnyListedAudienceAreAccepted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		audience []string
	}{
		{name: "first audience", audience: []string{"toolgate"}},
		{name: "second audience", audience: []string{secondAudience}},
		{name: "second audience with an unlisted one", audience: []string{"another-service", secondAudience}},
		{name: "both audiences", audience: []string{secondAudience, "toolgate"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, auth.Options{Audiences: []string{"toolgate", secondAudience}}, true)

			token := f.provider.MintAccessToken(t, tgtestutil.TokenClaims{
				Subject:       "alice",
				Email:         "alice@example.com",
				EmailVerified: true,
				Groups:        []string{"developers"},
				Audience:      test.audience,
			})

			response := f.do(t, token, map[string]string{"X-Toolgate-Session": "session-1"})
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())

			var identity auth.Identity
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &identity))
			assert.Equal(t, "alice", identity.Principal)
			assert.Equal(t, "alice@example.com", identity.Email)
			assert.Equal(t, []string{"developers"}, identity.Groups)
			assert.Equal(t, auth.KindUser, identity.Kind)
			assert.Empty(t, f.recorder.all())
		})
	}
}

func TestSecondAudienceTokensAreVerifiedFully(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		token        func(provider *tgtestutil.OIDCProvider) string
		wantReason   string
		wantAudience []string
	}{
		{
			name: "bad signature",
			token: func(provider *tgtestutil.OIDCProvider) string {
				// Another key signs a token with the right issuer.
				other := tgtestutil.StartOIDCProvider(t, tgtestutil.OIDCOptions{})

				return other.MintAccessToken(t, tgtestutil.TokenClaims{
					Subject:  "alice",
					Audience: []string{secondAudience},
					Issuer:   provider.Issuer,
				})
			},
			wantReason:   "invalid token",
			wantAudience: []string{secondAudience},
		},
		{
			name: "expired",
			token: func(provider *tgtestutil.OIDCProvider) string {
				return provider.MintAccessToken(t, tgtestutil.TokenClaims{
					Subject:   "alice",
					Audience:  []string{secondAudience},
					IssuedAt:  time.Now().Add(-2 * time.Hour),
					ExpiresAt: time.Now().Add(-time.Hour),
				})
			},
			wantReason:   "invalid token",
			wantAudience: []string{secondAudience},
		},
		{
			name: "wrong issuer",
			token: func(provider *tgtestutil.OIDCProvider) string {
				return provider.MintAccessToken(t, tgtestutil.TokenClaims{
					Subject:  "alice",
					Audience: []string{secondAudience},
					Issuer:   "https://other-idp.example",
				})
			},
			wantReason:   "invalid token",
			wantAudience: []string{secondAudience},
		},
		{
			name: "no listed audience",
			token: func(provider *tgtestutil.OIDCProvider) string {
				return provider.MintAccessToken(t, tgtestutil.TokenClaims{
					Subject:  "alice",
					Audience: []string{"another-service", "third-service"},
				})
			},
			wantReason:   "audience not allowed",
			wantAudience: []string{"another-service", "third-service"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, auth.Options{Audiences: []string{"toolgate", secondAudience}}, true)

			response := f.do(t, test.token(f.provider), map[string]string{"X-Toolgate-Session": "session-1"})
			assert.Equal(t, http.StatusUnauthorized, response.Code)

			refusals := f.recorder.all()
			require.Len(t, refusals, 1)
			assert.Equal(t, test.wantReason, refusals[0].Reason)
			assert.Equal(t, test.wantAudience, refusals[0].Audience)
		})
	}
}

// A setting is read at start: an audience removed from the list refuses its
// tokens.
func TestRemovedAudienceIsRefused(t *testing.T) {
	t.Parallel()

	f := newFixture(t, auth.Options{Audiences: []string{"toolgate"}}, true)

	token := f.provider.MintAccessToken(t, tgtestutil.TokenClaims{Subject: "alice", Audience: []string{secondAudience}})

	response := f.do(t, token, map[string]string{"X-Toolgate-Session": "session-1"})
	assert.Equal(t, http.StatusUnauthorized, response.Code)

	refusals := f.recorder.all()
	require.Len(t, refusals, 1)
	assert.Equal(t, "audience not allowed", refusals[0].Reason)
	assert.Equal(t, []string{secondAudience}, refusals[0].Audience)
}
