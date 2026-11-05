// Package wellknown is the client metadata document the gateway publishes
// without authentication at /.well-known/toolgate-client, so `toolgate login`
// can find the IdP from the gateway URL alone (docs/design/phase-1.md §8).
// It holds only public client metadata, never secrets.
package wellknown

import (
	"encoding/json"
	"net/http"

	"github.com/solidDoWant/toolgate/internal/auth"
)

// Path is the path of the client metadata document.
const Path = "/.well-known/toolgate-client"

// MaxDocumentBytes bounds the document a client reads.
const MaxDocumentBytes = 64 << 10

// ClientMetadata is what `toolgate login` needs to sign in.
type ClientMetadata struct {
	// Issuer is the IdP's issuer URL.
	Issuer string `json:"issuer"`
	// ClientID is the IdP's public client with the device grant.
	ClientID string `json:"clientID"`
	// Scopes are the scopes to ask for.
	Scopes []string `json:"scopes"`
	// EmailClaim is the claim that holds the user's email.
	EmailClaim string `json:"emailClaim"`
}

// Handler serves metadata as JSON. Each remote address gets the same budget
// as recorded sign-in refusals; requests over it get 429.
func Handler(metadata ClientMetadata) http.Handler {
	body, err := json.Marshal(metadata)
	if err != nil {
		// ClientMetadata has only strings and a string list.
		panic(err)
	}

	limiter := auth.NewRateLimiter()

	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !limiter.Allow(auth.RemoteIP(request)) {
			http.Error(writer, "too many requests", http.StatusTooManyRequests)

			return
		}

		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-store")
		_, _ = writer.Write(body)
	})
}
