package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/solidDoWant/toolgate/internal/client/headers"
	"github.com/solidDoWant/toolgate/internal/client/login"
	"github.com/solidDoWant/toolgate/internal/client/spiffe"
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
}

// signedInGateway loads the sign-in settings and gets a new access token with
// the stored refresh token.
func signedInGateway(ctx context.Context) (gatewayAccess, error) {
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

	accessToken, err := headers.AccessToken(ctx, nil, settings, tokenstore.New(dir))
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

	request.Header.Set("Authorization", "Bearer "+g.accessToken)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call the gateway: %w", err)
	}

	if response.StatusCode < 200 || response.StatusCode > 299 {
		defer func() { _ = response.Body.Close() }()

		message, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))

		return nil, fmt.Errorf("the gateway answered %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}

	return response, nil
}
