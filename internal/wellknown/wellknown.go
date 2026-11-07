// Package wellknown is the metadata the gateway publishes without
// authentication: the client metadata document at
// /.well-known/toolgate-client, so `toolgate login` can find the IdP from the
// gateway URL alone, and the OAuth protected
// resource metadata of the MCP endpoint, so standard MCP clients can sign in. Both hold only public metadata, never secrets.
package wellknown

import (
	"encoding/json"
	"net/http"
	"net/url"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/solidDoWant/toolgate/internal/auth"
)

// Paths of the protected resource metadata document. ProtectedResourcePath
// is the RFC 9728 path for a resource at /mcp; the document is served at
// ProtectedResourceRootPath too.
const (
	ProtectedResourcePath     = "/.well-known/oauth-protected-resource/mcp"
	ProtectedResourceRootPath = "/.well-known/oauth-protected-resource"
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

	return rateLimited(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-store")
		_, _ = writer.Write(body)
	}))
}

// ProtectedResource is what the protected resource metadata names.
type ProtectedResource struct {
	// Resource is the MCP endpoint's URL as clients reach it.
	Resource string
	// Issuer is the IdP's issuer URL, the only authorization server.
	Issuer string
	// Scopes are the scopes a client asks for.
	Scopes []string
}

// ProtectedResourceHandler serves the protected resource metadata with the
// go-sdk's handler. Each remote address gets the same budget as the client
// metadata document; requests over it get 429.
func ProtectedResourceHandler(resource ProtectedResource) http.Handler {
	return rateLimited(mcpauth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               resource.Resource,
		AuthorizationServers:   []string{resource.Issuer},
		ScopesSupported:        resource.Scopes,
		BearerMethodsSupported: []string{"header"},
	}))
}

// ProtectedResourceMetadataURL is the URL of the metadata document for the
// MCP endpoint at resource: the resource's origin plus
// ProtectedResourcePath. It is empty when resource is not an absolute URL.
func ProtectedResourceMetadataURL(resource string) string {
	parsed, err := url.Parse(resource)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}

	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host, Path: ProtectedResourcePath}).String()
}

// rateLimited refuses requests from a remote address over the per-address
// budget of recorded sign-in refusals with 429, and passes the rest to next.
func rateLimited(next http.Handler) http.Handler {
	limiter := auth.NewRateLimiter()

	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !limiter.Allow(auth.RemoteIP(request)) {
			http.Error(writer, "too many requests", http.StatusTooManyRequests)

			return
		}

		next.ServeHTTP(writer, request)
	})
}
