//go:build integration

package testutil_test

import (
	"context"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/solidDoWant/toolgate/internal/testutil"
)

type identityClaims struct {
	Subject       string   `json:"sub"`
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	Groups        []string `json:"groups"`
}

func TestOIDCProviderMintedTokens(t *testing.T) {
	t.Parallel()

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	discovered, err := oidc.NewProvider(t.Context(), provider.Issuer)
	require.NoError(t, err)

	verifier := discovered.Verifier(&oidc.Config{ClientID: provider.Audience})

	tests := []struct {
		name        string
		claims      testutil.TokenClaims
		mint        func(*testutil.OIDCProvider, testing.TB, testutil.TokenClaims) string
		verifier    *oidc.IDTokenVerifier
		errorAssert require.ErrorAssertionFunc
	}{
		{
			name: "access token with chosen claims verifies",
			claims: testutil.TokenClaims{
				Subject: "alice", Email: "alice@example.com", EmailVerified: true,
				Groups: []string{"dev", "ops"},
			},
			mint: (*testutil.OIDCProvider).MintAccessToken,
		},
		{
			name:   "ID token verifies",
			claims: testutil.TokenClaims{Subject: "bob", Email: "bob@example.com", Groups: []string{}},
			mint:   (*testutil.OIDCProvider).MintIDToken,
		},
		{
			name:     "chosen audience verifies against that audience",
			claims:   testutil.TokenClaims{Subject: "carol", Audience: []string{"other-gateway"}},
			mint:     (*testutil.OIDCProvider).MintAccessToken,
			verifier: discovered.Verifier(&oidc.Config{ClientID: "other-gateway"}),
		},
		{
			name:        "wrong audience is refused",
			claims:      testutil.TokenClaims{Subject: "carol", Audience: []string{"other-gateway"}},
			mint:        (*testutil.OIDCProvider).MintAccessToken,
			errorAssert: require.Error,
		},
		{
			name:        "expired token is refused",
			claims:      testutil.TokenClaims{Subject: "dave", IssuedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)},
			mint:        (*testutil.OIDCProvider).MintAccessToken,
			errorAssert: require.Error,
		},
		{
			name:        "foreign issuer is refused",
			claims:      testutil.TokenClaims{Subject: "eve", Issuer: "https://idp.invalid"},
			mint:        (*testutil.OIDCProvider).MintIDToken,
			errorAssert: require.Error,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if test.errorAssert == nil {
				test.errorAssert = require.NoError
			}

			if test.verifier == nil {
				test.verifier = verifier
			}

			token, err := test.verifier.Verify(t.Context(), test.mint(provider, t, test.claims))
			test.errorAssert(t, err)

			if err != nil {
				return
			}

			var claims identityClaims
			require.NoError(t, token.Claims(&claims))
			assert.Equal(t, test.claims.Subject, claims.Subject)
			assert.Equal(t, test.claims.Email, claims.Email)
			assert.Equal(t, test.claims.EmailVerified, claims.EmailVerified)
			assert.Equal(t, test.claims.Groups, claims.Groups)
		})
	}
}

func TestOIDCProviderDeviceFlow(t *testing.T) {
	t.Parallel()

	provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
	provider.AddUser(testutil.OIDCUser{
		Subject: "alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"dev"},
	})

	discovered, err := oidc.NewProvider(t.Context(), provider.Issuer)
	require.NoError(t, err)

	config := &oauth2.Config{
		ClientID: provider.ClientID,
		Endpoint: discovered.Endpoint(),
		Scopes:   []string{oidc.ScopeOpenID, "email", "profile"},
	}

	t.Run("approved device code yields verifiable tokens", func(t *testing.T) {
		verifier := oauth2.GenerateVerifier()
		authorization, err := config.DeviceAuth(t.Context(), oauth2.S256ChallengeOption(verifier))
		require.NoError(t, err)
		require.NotEmpty(t, authorization.UserCode)

		require.NoError(t, provider.ApproveDevice(t.Context(), authorization.UserCode, "alice"))

		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()

		token, err := config.DeviceAccessToken(ctx, authorization, oauth2.VerifierOption(verifier))
		require.NoError(t, err)

		accessToken, err := discovered.Verifier(&oidc.Config{ClientID: provider.Audience}).Verify(t.Context(), token.AccessToken)
		require.NoError(t, err)

		var claims identityClaims
		require.NoError(t, accessToken.Claims(&claims))
		assert.Equal(t, identityClaims{
			Subject: "alice", Email: "alice@example.com", EmailVerified: true, Groups: []string{"dev"},
		}, claims)

		rawIDToken, ok := token.Extra("id_token").(string)
		require.True(t, ok, "token response has an id_token")
		idToken, err := discovered.Verifier(&oidc.Config{ClientID: provider.ClientID}).Verify(t.Context(), rawIDToken)
		require.NoError(t, err)

		claims = identityClaims{}
		require.NoError(t, idToken.Claims(&claims))
		assert.Equal(t, "alice", claims.Subject)
		assert.Equal(t, []string{"dev"}, claims.Groups)
	})

	t.Run("denied device code yields no token", func(t *testing.T) {
		authorization, err := config.DeviceAuth(t.Context())
		require.NoError(t, err)

		require.NoError(t, provider.DenyDevice(t.Context(), authorization.UserCode))

		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()

		_, err = config.DeviceAccessToken(ctx, authorization)
		require.Error(t, err)
	})
}
