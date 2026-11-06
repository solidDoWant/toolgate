// Package spiffe fetches the client's JWT-SVID from a SPIFFE Workload API.
package spiffe

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
)

const (
	// SocketEnv names the variable that holds the Workload API address.
	// It is deliberately not go-spiffe's SPIFFE_ENDPOINT_SOCKET, which may
	// be set for other tools.
	SocketEnv = "TOOLGATE_SPIFFE_SOCKET"

	// GatewayEnv names the variable that holds the gateway URL when no
	// sign-in is saved.
	GatewayEnv = "TOOLGATE_GATEWAY"

	// Audience is the audience of the requested JWT-SVID.
	Audience = "toolgate"

	// FetchTimeout bounds one fetch.
	FetchTimeout = 5 * time.Second
)

// Socket returns the configured Workload API address, or "" when the SPIFFE
// credential is not in use.
func Socket() string {
	return strings.TrimSpace(os.Getenv(SocketEnv))
}

// FetchToken gets a fresh JWT-SVID for the toolgate audience.
func FetchToken(ctx context.Context, socket string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()

	svid, err := workloadapi.FetchJWTSVID(ctx, jwtsvid.Params{Audience: Audience}, workloadapi.WithAddr(socket))
	if err != nil {
		return "", fmt.Errorf("fetch a JWT-SVID from %s: %w", socket, err)
	}

	return svid.Marshal(), nil
}
