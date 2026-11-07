// Package login signs the CLI in to the IdP with the OAuth 2.0 device
// authorization grant and PKCE, and keeps what `toolgate status` shows.
//
// The gateway names the IdP: its /.well-known/toolgate-client document gives
// the issuer, the public client ID, the scopes and the email claim. The
// refresh token goes to the token store; everything else goes to
// config.json, which holds no token.
package login

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/solidDoWant/toolgate/internal/client/tokenstore"
	"github.com/solidDoWant/toolgate/internal/wellknown"
)

const (
	// SettingsFileName is the name of the settings file inside the
	// toolgate configuration directory.
	SettingsFileName = "config.json"

	// maxSettingsBytes bounds the settings file the CLI reads.
	maxSettingsBytes = 64 << 10

	fileMode = 0o600
	dirMode  = 0o700
)

// ErrNotSignedIn is returned by LoadSettings when nobody has signed in.
var ErrNotSignedIn = errors.New("not signed in")

// ErrCodeExpired is returned by Run when nobody approved the device code
// before it expired.
var ErrCodeExpired = errors.New("The sign-in code expired. Run `toolgate login` again.") //nolint:staticcheck // The message is shown to the user as a sentence.

// Settings is the content of config.json.
type Settings struct {
	// Gateway is the gateway's base URL.
	Gateway string `json:"gateway"`
	// Issuer, ClientID, Scopes and EmailClaim are the values the CLI signed
	// in with: fetched from the gateway unless overridden by flags.
	Issuer     string   `json:"issuer"`
	ClientID   string   `json:"clientID"`
	Scopes     []string `json:"scopes"`
	EmailClaim string   `json:"emailClaim"`
	// Subject, Email and EmailVerified are read from the verified ID token
	// at sign-in. They are for display only; the gateway decides access.
	Subject       string `json:"subject"`
	Email         string `json:"email,omitempty"`
	EmailVerified bool   `json:"emailVerified"`
	// TokenStore is where the refresh token is kept: "keychain" or "file".
	// Every later load and save uses only this store. A config.json from
	// before this field existed has none, and keeps the keychain-then-file
	// behaviour.
	TokenStore string `json:"tokenStore,omitempty"`

	// TokenCommand, AttestationTokenFile and AttestationTokenRequest are the
	// file forms of TOOLGATE_TOKEN_COMMAND, TOOLGATE_ATTESTATION_TOKEN_FILE
	// and TOOLGATE_ATTESTATION_TOKEN_REQUEST, set by `toolgate configure`.
	// Claude Code removes variables with credential-like names from the
	// environment of a plugin's headersHelper, so the helper reads them
	// here. A set environment variable wins. They hold a command line and
	// paths, never a token.
	TokenCommand            string `json:"tokenCommand,omitempty"`
	AttestationTokenFile    string `json:"attestationTokenFile,omitempty"`
	AttestationTokenRequest string `json:"attestationTokenRequest,omitempty"`
}

// KeepConfigured copies the fields set by `toolgate configure` from previous
// into settings, so that a new sign-in does not drop them.
func (settings *Settings) KeepConfigured(previous Settings) {
	settings.TokenCommand = previous.TokenCommand
	settings.AttestationTokenFile = previous.AttestationTokenFile
	settings.AttestationTokenRequest = previous.AttestationTokenRequest
}

// Options configures Run.
type Options struct {
	// Gateway is the gateway's base URL. Required.
	Gateway string
	// Issuer, ClientID and Scopes override the values from the gateway when
	// they are not empty.
	Issuer   string
	ClientID string
	Scopes   []string
	// Prompt receives the instructions for the user: the verification URL
	// and the user code.
	Prompt io.Writer
	// HTTPClient makes every request. Nil uses http.DefaultClient.
	HTTPClient *http.Client
	// Tokens stores the refresh token.
	Tokens *tokenstore.Store
}

// Run fetches the gateway's client metadata, runs the device grant with
// PKCE, verifies the ID token, stores the refresh token and returns the
// settings to save.
func Run(ctx context.Context, options Options) (Settings, error) {
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	ctx = oidc.ClientContext(ctx, httpClient)

	gateway, err := gatewayURL(options.Gateway)
	if err != nil {
		return Settings{}, err
	}

	metadata, err := FetchClientMetadata(ctx, httpClient, gateway)
	if err != nil {
		return Settings{}, err
	}

	settings := Settings{
		Gateway:    gateway,
		Issuer:     valueOr(options.Issuer, metadata.Issuer),
		ClientID:   valueOr(options.ClientID, metadata.ClientID),
		Scopes:     metadata.Scopes,
		EmailClaim: valueOr(metadata.EmailClaim, "email"),
	}
	if len(options.Scopes) > 0 {
		settings.Scopes = options.Scopes
	}

	if settings.Issuer == "" {
		return Settings{}, errors.New("the gateway names no IdP issuer; pass --issuer")
	}

	if settings.ClientID == "" {
		return Settings{}, errors.New("the gateway names no client ID for the CLI; pass --client-id")
	}

	provider, err := oidc.NewProvider(ctx, settings.Issuer)
	if err != nil {
		return Settings{}, fmt.Errorf("read the IdP's discovery document: %w", err)
	}

	config := &oauth2.Config{
		ClientID: settings.ClientID,
		Endpoint: provider.Endpoint(),
		Scopes:   settings.Scopes,
	}
	// The client is public: send the client ID in the form, with no secret.
	config.Endpoint.AuthStyle = oauth2.AuthStyleInParams

	if config.Endpoint.DeviceAuthURL == "" {
		return Settings{}, errors.New("the IdP does not offer the device authorization grant")
	}

	verifier := oauth2.GenerateVerifier()

	authorization, err := config.DeviceAuth(ctx, oauth2.S256ChallengeOption(verifier))
	if err != nil {
		return Settings{}, fmt.Errorf("start the device sign-in: %w", err)
	}

	if authorization.VerificationURIComplete != "" {
		_, _ = fmt.Fprintf(options.Prompt, "To sign in, open %s\nand check that it shows the code %s\n",
			authorization.VerificationURIComplete, authorization.UserCode)
	} else {
		_, _ = fmt.Fprintf(options.Prompt, "To sign in, open %s\nand enter the code %s\n",
			authorization.VerificationURI, authorization.UserCode)
	}

	_, _ = fmt.Fprintf(options.Prompt, "The code expires in %s. You can open the link on any device.\n",
		DurationInWords(time.Until(authorization.Expiry)))

	token, err := config.DeviceAccessToken(ctx, authorization, oauth2.VerifierOption(verifier))
	if err != nil {
		if codeExpired(err) {
			return Settings{}, ErrCodeExpired
		}

		return Settings{}, fmt.Errorf("finish the device sign-in: %w", err)
	}

	if err := readIdentity(ctx, provider, token, &settings); err != nil {
		return Settings{}, err
	}

	if token.RefreshToken == "" {
		return Settings{}, errors.New("the IdP returned no refresh token; the client needs the offline_access scope and the refresh token grant")
	}

	if err := options.Tokens.Save(token.RefreshToken); err != nil {
		return Settings{}, fmt.Errorf("store the refresh token: %w", err)
	}

	settings.TokenStore = string(options.Tokens.Used())

	return settings, nil
}

// codeExpired reports whether err says the device code ran out.
func codeExpired(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var retrieveErr *oauth2.RetrieveError

	return errors.As(err, &retrieveErr) && retrieveErr.ErrorCode == "expired_token"
}

// DurationInWords writes d as whole minutes, or seconds when it is under a
// minute, for example "10 minutes" or "45 seconds".
func DurationInWords(d time.Duration) string {
	seconds := int(d.Round(time.Second) / time.Second)
	if seconds < 1 {
		seconds = 1
	}

	unit := func(count int, name string) string {
		if count == 1 {
			return "1 " + name
		}

		return fmt.Sprintf("%d %ss", count, name)
	}

	if seconds < 60 {
		return unit(seconds, "second")
	}

	if seconds%60 == 0 {
		return unit(seconds/60, "minute")
	}

	return unit(seconds/60, "minute") + " and " + unit(seconds%60, "second")
}

// readIdentity verifies the token response's ID token for the client and
// copies the subject and email into settings.
func readIdentity(ctx context.Context, provider *oidc.Provider, token *oauth2.Token, settings *Settings) error {
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return errors.New("the IdP returned no ID token; the client needs the openid scope")
	}

	idToken, err := provider.Verifier(&oidc.Config{ClientID: settings.ClientID}).Verify(ctx, rawIDToken)
	if err != nil {
		return fmt.Errorf("verify the ID token: %w", err)
	}

	var claims map[string]json.RawMessage
	if err := idToken.Claims(&claims); err != nil {
		return fmt.Errorf("read the ID token: %w", err)
	}

	settings.Subject = idToken.Subject
	// A claim of another type is not shown.
	_ = json.Unmarshal(claims[settings.EmailClaim], &settings.Email)
	_ = json.Unmarshal(claims["email_verified"], &settings.EmailVerified)

	return nil
}

// FetchClientMetadata reads the gateway's client metadata document.
func FetchClientMetadata(ctx context.Context, httpClient *http.Client, gateway string) (wellknown.ClientMetadata, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, gateway+wellknown.Path, nil)
	if err != nil {
		return wellknown.ClientMetadata{}, fmt.Errorf("gateway URL: %w", err)
	}

	response, err := httpClient.Do(request)
	if err != nil {
		return wellknown.ClientMetadata{}, fmt.Errorf("reach the gateway: %w", err)
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return wellknown.ClientMetadata{}, fmt.Errorf("the gateway answered %s for %s", response.Status, wellknown.Path)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, wellknown.MaxDocumentBytes+1))
	if err != nil {
		return wellknown.ClientMetadata{}, fmt.Errorf("read %s: %w", wellknown.Path, err)
	}

	if len(body) > wellknown.MaxDocumentBytes {
		return wellknown.ClientMetadata{}, fmt.Errorf("%s is larger than %d bytes", wellknown.Path, wellknown.MaxDocumentBytes)
	}

	var metadata wellknown.ClientMetadata
	if err := json.Unmarshal(body, &metadata); err != nil {
		return wellknown.ClientMetadata{}, fmt.Errorf("read %s: %w", wellknown.Path, err)
	}

	return metadata, nil
}

// gatewayURL normalises the user's gateway value.
func gatewayURL(raw string) (string, error) {
	return NormalizeGatewayURL(raw)
}

// SaveSettings writes settings to dir/config.json with mode 0600.
func SaveSettings(dir string, settings Settings) error {
	content, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}

	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("create settings directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, SettingsFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create settings file: %w", err)
	}

	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(fileMode); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		_ = tmp.Close()

		return fmt.Errorf("set settings file mode: %w", err)
	}

	if _, err := tmp.Write(append(content, '\n')); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("write settings file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close settings file: %w", err)
	}

	if err := os.Rename(tmpName, filepath.Join(dir, SettingsFileName)); err != nil {
		return fmt.Errorf("replace settings file: %w", err)
	}

	return nil
}

// LoadSettings reads dir/config.json. It returns ErrNotSignedIn when the
// file does not exist.
func LoadSettings(dir string) (Settings, error) {
	file, err := os.Open(filepath.Join(dir, SettingsFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return Settings{}, ErrNotSignedIn
	}

	if err != nil {
		return Settings{}, fmt.Errorf("read settings: %w", err)
	}

	defer func() { _ = file.Close() }()

	content, err := io.ReadAll(io.LimitReader(file, maxSettingsBytes+1))
	if err != nil {
		return Settings{}, fmt.Errorf("read settings: %w", err)
	}

	if len(content) > maxSettingsBytes {
		return Settings{}, fmt.Errorf("settings file is larger than %d bytes", maxSettingsBytes)
	}

	var settings Settings
	if err := json.Unmarshal(content, &settings); err != nil {
		return Settings{}, fmt.Errorf("read settings: %w", err)
	}

	return settings, nil
}

func valueOr(value, fallback string) string {
	if value != "" {
		return value
	}

	return fallback
}
