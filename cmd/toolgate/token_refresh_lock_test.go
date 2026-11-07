package main

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/client/headers"
	"github.com/solidDoWant/toolgate/internal/client/login"
	"github.com/solidDoWant/toolgate/internal/client/tokenstore"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// refreshRecorder records the refresh token of every refresh the IdP's token
// endpoint receives.
type refreshRecorder struct {
	tokenURL string

	mu            sync.Mutex
	refreshTokens []string
}

func (r *refreshRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.String() == r.tokenURL && request.Body != nil {
		body, err := io.ReadAll(request.Body)
		_ = request.Body.Close()

		if err != nil {
			return nil, err
		}

		request.Body = io.NopCloser(bytes.NewReader(body))

		form, err := url.ParseQuery(string(body))
		if err == nil && form.Get("grant_type") == "refresh_token" {
			r.mu.Lock()
			r.refreshTokens = append(r.refreshTokens, form.Get("refresh_token"))
			r.mu.Unlock()
		}
	}

	return http.DefaultTransport.RoundTrip(request)
}

func (r *refreshRecorder) sent() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.refreshTokens...)
}

func TestConcurrentRefreshesNeverReuseARotatedRefreshToken(t *testing.T) {
	const callers = 20

	for _, store := range []string{"file", "keychain"} {
		t.Run(store+" store", func(t *testing.T) {
			dir := isolateUserConfig(t)

			// The fake IdP rotates the refresh token on every refresh and
			// refuses the old one from then on.
			provider := testutil.StartOIDCProvider(t, testutil.OIDCOptions{})
			provider.AddUser(testutil.OIDCUser{Subject: "alice", Email: "alice@example.com", EmailVerified: true})
			gatewayURL := startGateway(t, provider, provider.ClientID)

			code, _, stderr := signInApproving(t, provider, "alice", "--gateway", gatewayURL, "--token-store", store)
			require.Equal(t, 0, code, "stderr: %s", stderr)

			settings, err := login.LoadSettings(dir)
			require.NoError(t, err)

			recorder := &refreshRecorder{tokenURL: provider.TokenURL()}
			client := &http.Client{Transport: recorder}

			start := make(chan struct{})
			accessTokens := make([]string, callers)
			errs := make([]error, callers)

			var wait sync.WaitGroup
			for caller := range callers {
				wait.Go(func() {
					<-start

					// Each caller has its own store, as each toolgate
					// process does.
					accessTokens[caller], errs[caller] = headers.AccessToken(t.Context(), client, settings, tokenStoreFor(dir, settings))
				})
			}

			close(start)
			wait.Wait()

			for caller := range callers {
				require.NoError(t, errs[caller], "caller %d", caller)
				assert.NotEmpty(t, accessTokens[caller], "caller %d", caller)
			}

			sent := recorder.sent()
			require.Len(t, sent, callers, "each caller refreshes once")

			distinct := map[string]bool{}
			for _, refreshToken := range sent {
				distinct[refreshToken] = true
			}

			assert.Len(t, distinct, callers, "no refresh token is sent twice")

			// The token stored last is the newest one, and still works.
			_, err = headers.AccessToken(t.Context(), client, settings, tokenStoreFor(dir, settings))
			require.NoError(t, err)

			assert.FileExists(t, filepath.Join(dir, tokenstore.LockFileName))

			if store == "keychain" {
				_, err := os.Stat(filepath.Join(dir, tokenstore.FileName))
				assert.ErrorIs(t, err, os.ErrNotExist, "the refresh token stays in the keychain")
			}
		})
	}
}
