package testutil

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
	"github.com/zitadel/oidc/v3/example/server/storage"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
	"golang.org/x/text/language"
)

// Defaults of the fake OIDC provider.
const (
	DefaultOIDCClientID = "toolgate-cli"
	DefaultOIDCAudience = "toolgate"
)

// OIDCOptions configures StartOIDCProvider. The zero value is valid.
type OIDCOptions struct {
	// ClientID is the public client that runs the device-code flow.
	// Default DefaultOIDCClientID.
	ClientID string
	// Audience is added to the audience of the access and ID tokens the
	// provider issues, next to the client ID. Default DefaultOIDCAudience.
	Audience string
	// Listener serves the provider. Default: a new listener on 127.0.0.1.
	// The e2e harness passes one that pods can reach.
	Listener net.Listener
	// Issuer is the public base URL of the provider. Default:
	// "http://" + the listener's address.
	Issuer string
}

// OIDCUser is a user of the fake OIDC provider.
type OIDCUser struct {
	Subject       string
	Email         string
	EmailVerified bool
	Groups        []string
}

// TokenClaims are the claims of a token minted with MintAccessToken or
// MintIDToken. Empty fields take the provider's defaults: its issuer, its
// audience, and an expiry one hour from now.
type TokenClaims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Groups        []string
	Audience      []string
	Issuer        string
	IssuedAt      time.Time
	ExpiresAt     time.Time
	// Extra claims, merged last.
	Extra map[string]any
}

// OIDCProvider is a fake OIDC provider running in the test process. It
// serves discovery, JWKS, the device authorization and token endpoints and
// userinfo through github.com/zitadel/oidc, and signs every token, issued or
// minted, with the same key.
type OIDCProvider struct {
	Issuer   string
	ClientID string
	Audience string

	storage *oidcStorage
	server  *httptest.Server
}

// StartOIDCProvider starts a fake OIDC provider and stops it when tb ends.
// It needs no Docker.
func StartOIDCProvider(tb testing.TB, options OIDCOptions) *OIDCProvider {
	tb.Helper()

	provider, err := NewOIDCProvider(options)
	require.NoError(tb, err)
	tb.Cleanup(provider.Close)

	return provider
}

// NewOIDCProvider starts a fake OIDC provider that runs until Close. Tests
// use StartOIDCProvider; this is for a TestMain that shares one provider
// between all the tests of a package.
func NewOIDCProvider(options OIDCOptions) (*OIDCProvider, error) {
	if options.ClientID == "" {
		options.ClientID = DefaultOIDCClientID
	}

	if options.Audience == "" {
		options.Audience = DefaultOIDCAudience
	}

	if options.Listener == nil {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}

		options.Listener = listener
	}

	if options.Issuer == "" {
		options.Issuer = "http://" + options.Listener.Addr().String()
	}

	users := &oidcUsers{users: map[string]OIDCUser{}}
	client := &deviceClient{id: options.ClientID}
	store := &oidcStorage{
		Storage: storage.NewStorageWithClients(users, map[string]*storage.Client{
			// The example storage checks that a device authorization
			// belongs to a client it knows; the client itself is deviceClient.
			options.ClientID: storage.DeviceClient(options.ClientID, ""),
		}),
		users:  users,
		client: client,
	}

	config := &op.Config{
		CryptoKey:      sha256.Sum256([]byte("toolgate fake OIDC provider")),
		CodeMethodS256: true,
		// The CLI keeps a refresh token and gets each access token with it.
		GrantTypeRefreshToken: true,
		SupportedUILocales:    []language.Tag{language.English},
		DeviceAuthorization: op.DeviceAuthorizationConfig{
			Lifetime:     5 * time.Minute,
			PollInterval: time.Second,
			UserFormPath: "/device",
			UserCode:     op.UserCodeBase20,
		},
	}

	provider, err := op.NewProvider(config, store, op.StaticIssuer(options.Issuer), op.WithAllowInsecure())
	if err != nil {
		_ = options.Listener.Close()

		return nil, err
	}

	server := httptest.NewUnstartedServer(provider)
	_ = server.Listener.Close()
	server.Listener = options.Listener
	server.Start()

	return &OIDCProvider{
		Issuer:   options.Issuer,
		ClientID: options.ClientID,
		Audience: options.Audience,
		storage:  store,
		server:   server,
	}, nil
}

// Close stops the provider.
func (p *OIDCProvider) Close() {
	p.server.Close()
}

// DeviceAuthorizationURL is the device authorization endpoint.
func (p *OIDCProvider) DeviceAuthorizationURL() string {
	return p.Issuer + "/device_authorization"
}

// TokenURL is the token endpoint.
func (p *OIDCProvider) TokenURL() string {
	return p.Issuer + "/oauth/token"
}

// Tokens issued through the device-code flow expire after 5 minutes (the
// example storage's fixed lifetime); mint tokens to choose the expiry.

// AddUser adds or replaces a user. Tokens issued for the user carry its
// email, email_verified and groups claims.
func (p *OIDCProvider) AddUser(user OIDCUser) {
	p.users().set(user)
}

// ApproveDevice approves the pending device authorization with userCode as
// subject, as if the user had signed in and confirmed the code in a browser.
// The subject must have been added with AddUser.
func (p *OIDCProvider) ApproveDevice(ctx context.Context, userCode, subject string) error {
	if _, ok := p.users().get(subject); !ok {
		return errors.New("unknown user " + subject)
	}

	state, err := p.storage.GetDeviceAuthorizationByUserCode(ctx, userCode)
	if err != nil {
		return err
	}

	state.Audience = []string{p.Audience}
	state.AuthTime = time.Now()

	return p.storage.CompleteDeviceAuthorization(ctx, userCode, subject)
}

// DenyDevice denies the pending device authorization with userCode.
func (p *OIDCProvider) DenyDevice(ctx context.Context, userCode string) error {
	if _, err := p.storage.GetDeviceAuthorizationByUserCode(ctx, userCode); err != nil {
		return err
	}

	return p.storage.DenyDeviceAuthorization(ctx, userCode)
}

// MintAccessToken returns a JWT access token with the given claims, signed
// with the provider's key.
func (p *OIDCProvider) MintAccessToken(tb testing.TB, claims TokenClaims) string {
	tb.Helper()

	values := p.tokenClaims(claims)
	values["client_id"] = p.ClientID

	return p.sign(tb, "at+jwt", values)
}

// MintIDToken returns an ID token with the given claims, signed with the
// provider's key.
func (p *OIDCProvider) MintIDToken(tb testing.TB, claims TokenClaims) string {
	tb.Helper()

	values := p.tokenClaims(claims)
	values["azp"] = p.ClientID

	return p.sign(tb, "JWT", values)
}

func (p *OIDCProvider) tokenClaims(claims TokenClaims) map[string]any {
	now := time.Now()

	if claims.Issuer == "" {
		claims.Issuer = p.Issuer
	}

	if claims.Audience == nil {
		claims.Audience = []string{p.Audience}
	}

	if claims.IssuedAt.IsZero() {
		claims.IssuedAt = now
	}

	if claims.ExpiresAt.IsZero() {
		claims.ExpiresAt = now.Add(time.Hour)
	}

	values := map[string]any{
		"iss":            claims.Issuer,
		"sub":            claims.Subject,
		"aud":            claims.Audience,
		"iat":            claims.IssuedAt.Unix(),
		"nbf":            claims.IssuedAt.Unix(),
		"exp":            claims.ExpiresAt.Unix(),
		"email_verified": claims.EmailVerified,
	}
	if claims.Email != "" {
		values["email"] = claims.Email
	}

	if claims.Groups != nil {
		values["groups"] = claims.Groups
	}

	for name, value := range claims.Extra {
		values[name] = value
	}

	return values
}

func (p *OIDCProvider) sign(tb testing.TB, tokenType string, claims map[string]any) string {
	tb.Helper()

	key, err := p.storage.SigningKey(tb.Context())
	require.NoError(tb, err)

	signer, err := jose.NewSigner(
		jose.SigningKey{
			Algorithm: key.SignatureAlgorithm(),
			Key:       jose.JSONWebKey{Key: key.Key(), KeyID: key.ID()},
		},
		(&jose.SignerOptions{}).WithType(jose.ContentType(tokenType)),
	)
	require.NoError(tb, err)

	token, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(tb, err)

	return token
}

func (p *OIDCProvider) users() *oidcUsers {
	return p.storage.users
}

// oidcUsers implements the example storage's UserStore and keeps the groups
// the example user type has no field for.
type oidcUsers struct {
	mu    sync.Mutex
	users map[string]OIDCUser
}

func (u *oidcUsers) set(user OIDCUser) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.users[user.Subject] = user
}

func (u *oidcUsers) get(subject string) (OIDCUser, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()

	user, ok := u.users[subject]

	return user, ok
}

func (u *oidcUsers) GetUserByID(id string) *storage.User {
	user, ok := u.get(id)
	if !ok {
		return nil
	}

	return &storage.User{
		ID:                user.Subject,
		Username:          user.Subject,
		Email:             user.Email,
		EmailVerified:     user.EmailVerified,
		PreferredLanguage: language.English,
	}
}

func (u *oidcUsers) GetUserByUsername(username string) *storage.User {
	return u.GetUserByID(username)
}

func (u *oidcUsers) ExampleClientID() string {
	return ""
}

// claims returns the identity claims of subject's tokens.
func (u *oidcUsers) claims(subject string) map[string]any {
	user, ok := u.get(subject)
	if !ok {
		return nil
	}

	claims := map[string]any{"email_verified": user.EmailVerified}
	if user.Email != "" {
		claims["email"] = user.Email
	}

	if user.Groups != nil {
		claims["groups"] = user.Groups
	}

	return claims
}

// oidcStorage is the zitadel example storage with the test's users, a public
// device-code client and the identity claims in every token.
type oidcStorage struct {
	*storage.Storage

	users  *oidcUsers
	client *deviceClient
}

func (s *oidcStorage) GetClientByClientID(_ context.Context, clientID string) (op.Client, error) {
	if clientID != s.client.id {
		return nil, errors.New("client not found")
	}

	return s.client, nil
}

// TokenRequestByRefreshToken names the device client in the refresh token's
// request. The example storage records no client for tokens issued by the
// device grant, and the provider refuses a refresh whose request names a
// different client from the caller.
func (s *oidcStorage) TokenRequestByRefreshToken(ctx context.Context, refreshToken string) (op.RefreshTokenRequest, error) {
	request, err := s.Storage.TokenRequestByRefreshToken(ctx, refreshToken)
	if err != nil {
		return nil, err
	}

	if request.GetClientID() != "" {
		return request, nil
	}

	return deviceRefreshRequest{RefreshTokenRequest: request, clientID: s.client.id}, nil
}

// deviceRefreshRequest is a refresh token's request with the device client
// as its client.
type deviceRefreshRequest struct {
	op.RefreshTokenRequest

	clientID string
}

func (r deviceRefreshRequest) GetClientID() string { return r.clientID }

func (s *oidcStorage) AuthorizeClientIDSecret(context.Context, string, string) error {
	return errors.New("the device client is public and has no secret")
}

func (s *oidcStorage) GetPrivateClaimsFromScopes(_ context.Context, userID, _ string, _ []string) (map[string]any, error) {
	return s.users.claims(userID), nil
}

func (s *oidcStorage) SetUserinfoFromScopes(_ context.Context, userinfo *oidc.UserInfo, userID, _ string, _ []string) error {
	return s.setUserinfo(userinfo, userID)
}

func (s *oidcStorage) SetUserinfoFromRequest(_ context.Context, userinfo *oidc.UserInfo, token op.IDTokenRequest, _ []string) error {
	return s.setUserinfo(userinfo, token.GetSubject())
}

func (s *oidcStorage) setUserinfo(userinfo *oidc.UserInfo, subject string) error {
	if _, ok := s.users.get(subject); !ok {
		return errors.New("user not found")
	}

	userinfo.Subject = subject
	for name, value := range s.users.claims(subject) {
		userinfo.AppendClaims(name, value)
	}

	return nil
}

// deviceClient is a public client allowed the device-code and refresh-token
// grants. Its access tokens are JWTs, as the gateway verifies them against
// the JWKS.
type deviceClient struct {
	id string
}

func (c *deviceClient) GetID() string                       { return c.id }
func (c *deviceClient) RedirectURIs() []string              { return nil }
func (c *deviceClient) PostLogoutRedirectURIs() []string    { return nil }
func (c *deviceClient) ApplicationType() op.ApplicationType { return op.ApplicationTypeNative }
func (c *deviceClient) AuthMethod() oidc.AuthMethod         { return oidc.AuthMethodNone }
func (c *deviceClient) ResponseTypes() []oidc.ResponseType {
	return []oidc.ResponseType{oidc.ResponseTypeCode}
}

func (c *deviceClient) GrantTypes() []oidc.GrantType {
	return []oidc.GrantType{oidc.GrantTypeDeviceCode, oidc.GrantTypeRefreshToken}
}
func (c *deviceClient) LoginURL(string) string               { return "" }
func (c *deviceClient) AccessTokenType() op.AccessTokenType  { return op.AccessTokenTypeJWT }
func (c *deviceClient) IDTokenLifetime() time.Duration       { return 5 * time.Minute }
func (c *deviceClient) DevMode() bool                        { return false }
func (c *deviceClient) IsScopeAllowed(string) bool           { return false }
func (c *deviceClient) IDTokenUserinfoClaimsAssertion() bool { return true }
func (c *deviceClient) ClockSkew() time.Duration             { return 0 }

func (c *deviceClient) RestrictAdditionalIdTokenScopes() func([]string) []string {
	return func(scopes []string) []string { return scopes }
}

func (c *deviceClient) RestrictAdditionalAccessTokenScopes() func([]string) []string {
	return func(scopes []string) []string { return scopes }
}
