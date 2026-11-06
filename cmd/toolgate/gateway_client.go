package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/client/attestation"
	"github.com/solidDoWant/toolgate/internal/client/headers"
	"github.com/solidDoWant/toolgate/internal/client/login"
	"github.com/solidDoWant/toolgate/internal/client/spiffe"
	"github.com/solidDoWant/toolgate/internal/client/tokencommand"
	"github.com/solidDoWant/toolgate/internal/client/tokenstore"
)

// maxErrorBody bounds how much of a gateway error response is read.
const maxErrorBody = 4 << 10

// errNotSignedIn is printed by the commands that call the gateway when
// nobody has signed in.
var errNotSignedIn = errors.New("not signed in. Run toolgate login")

// gatewayAccess is what a command needs to call the gateway as the signed-in
// user: the gateway's base URL and a fresh access token.
type gatewayAccess struct {
	gateway     string
	accessToken string

	// attestation is the workload token for X-Toolgate-Attestation, or ""
	// when no attestation is configured.
	attestation string
}

// setCredentials sets the user's token and, when there is one, the
// attestation on a request's headers.
func setCredentials(header http.Header, accessToken, attestation string) {
	header.Set("Authorization", "Bearer "+accessToken)

	if attestation != "" {
		header.Set(auth.AttestationHeader, attestation)
	}
}

// signedInGateway loads the sign-in settings and gets a new access token with
// the stored refresh token.
func signedInGateway(ctx context.Context) (gatewayAccess, error) {
	access, err := userGateway(ctx)
	if err != nil {
		return gatewayAccess{}, err
	}

	access.attestation, err = attestation.FromEnv().Token(ctx)
	if err != nil {
		return gatewayAccess{}, err
	}

	return access, nil
}

// userGateway gets the gateway URL and the user's token.
func userGateway(ctx context.Context) (gatewayAccess, error) {
	if source := tokencommand.FromEnv(); source.Command != "" {
		return tokenCommandGateway(ctx, source)
	}

	if socket := spiffe.Socket(); socket != "" {
		return spiffeGateway(ctx, socket)
	}

	dir, err := configDir()
	if err != nil {
		return gatewayAccess{}, err
	}

	settings, err := login.LoadSettings(dir)
	if errors.Is(err, login.ErrNotSignedIn) {
		return gatewayAccess{}, errNotSignedIn
	}

	if err != nil {
		return gatewayAccess{}, err
	}

	accessToken, err := headers.AccessToken(ctx, nil, settings, tokenStoreFor(dir, settings))
	if errors.Is(err, tokenstore.ErrNotFound) {
		return gatewayAccess{}, errNotSignedIn
	}

	if err != nil {
		return gatewayAccess{}, err
	}

	return gatewayAccess{gateway: strings.TrimRight(settings.Gateway, "/"), accessToken: accessToken}, nil
}

// spiffeGateway gets a JWT-SVID from the Workload API. It never falls back
// to the stored sign-in. The gateway URL comes from the saved sign-in, or
// from TOOLGATE_GATEWAY when none is saved.
func spiffeGateway(ctx context.Context, socket string) (gatewayAccess, error) {
	gateway, err := spiffeGatewayURL()
	if err != nil {
		return gatewayAccess{}, err
	}

	token, err := spiffe.FetchToken(ctx, socket)
	if err != nil {
		return gatewayAccess{}, err
	}

	return gatewayAccess{gateway: gateway, accessToken: token}, nil
}

// tokenCommandGateway gets the user token from TOOLGATE_TOKEN_COMMAND. The
// stored sign-in is ignored, apart from the gateway URL.
func tokenCommandGateway(ctx context.Context, source tokencommand.Source) (gatewayAccess, error) {
	gateway, err := spiffeGatewayURL()
	if err != nil {
		return gatewayAccess{}, err
	}

	token, err := source.Token(ctx)
	if err != nil {
		return gatewayAccess{}, err
	}

	return gatewayAccess{gateway: gateway, accessToken: token}, nil
}

// gatewayError is a non-2xx answer from the gateway.
type gatewayError struct {
	Status  int
	Message string
}

func (e *gatewayError) Error() string {
	return fmt.Sprintf("the gateway answered %d: %s", e.Status, e.Message)
}

// errUnauthorized marks a gateway answer of 401.
var errUnauthorized = errors.New("the gateway refused the token")

// withAccess runs call with the user's access token. When the token came
// from TOOLGATE_TOKEN_COMMAND and the gateway answers 401, it deletes the
// cached token, runs the command once more and calls again, once.
func withAccess(ctx context.Context, call func(gatewayAccess) error) error {
	access, err := signedInGateway(ctx)
	if err != nil {
		return err
	}

	err = call(access)
	if !errors.Is(err, errUnauthorized) {
		return err
	}

	token, refreshErr := refreshedToken(ctx)
	if refreshErr != nil {
		return err
	}

	access.accessToken = token

	return call(access)
}

// refreshedToken deletes the cached command token and runs the command
// again. It fails when TOOLGATE_TOKEN_COMMAND is not in use.
func refreshedToken(ctx context.Context) (string, error) {
	source := tokencommand.FromEnv()
	if source.Command == "" {
		return "", errors.New("no token command")
	}

	if err := source.Invalidate(); err != nil {
		return "", err
	}

	return source.Token(ctx)
}

// spiffeGatewayURL returns the gateway URL for a command that uses the SPIFFE
// credential.
func spiffeGatewayURL() (string, error) {
	if dir, err := configDir(); err == nil {
		settings, err := login.LoadSettings(dir)
		if err == nil && settings.Gateway != "" {
			return strings.TrimRight(settings.Gateway, "/"), nil
		}
	}

	gateway := strings.TrimRight(strings.TrimSpace(os.Getenv(spiffe.GatewayEnv)), "/")
	if gateway == "" {
		return "", fmt.Errorf("no gateway URL: sign in with toolgate login, or set %s", spiffe.GatewayEnv)
	}

	return gateway, nil
}

// do sends a request to the gateway with the access token. A response that
// isn't 2xx becomes an error holding the gateway's message.
func (g gatewayAccess) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, g.gateway+path, body)
	if err != nil {
		return nil, err
	}

	setCredentials(request.Header, g.accessToken, g.attestation)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call the gateway: %w", err)
	}

	if response.StatusCode < 200 || response.StatusCode > 299 {
		defer func() { _ = response.Body.Close() }()

		message, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))

		err := error(&gatewayError{Status: response.StatusCode, Message: strings.TrimSpace(string(message))})
		if response.StatusCode == http.StatusUnauthorized {
			err = errors.Join(errUnauthorized, err)
		}

		return nil, err
	}

	return response, nil
}
