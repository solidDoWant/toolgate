//go:build !windows

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/toolgate/internal/client/attestation"
)

type keysGateway struct {
	mu       sync.Mutex
	posts    [][]byte
	deletes  []string
	postCode int
	postBody string
	statuses []string
	polls    int
	listBody string
	delCode  int
	delBody  string
	attested []string
}

func (g *keysGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.attested = append(g.attested, r.Header.Get("X-Toolgate-Attestation"))

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/workspace-keys":
		body, _ := io.ReadAll(r.Body)
		g.posts = append(g.posts, body)

		if g.postCode >= 400 {
			http.Error(w, g.postBody, g.postCode)

			return
		}

		w.WriteHeader(g.postCode)
		_, _ = io.WriteString(w, g.postBody)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/workspace-keys":
		_, _ = io.WriteString(w, g.listBody)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/workspace-keys/"):
		g.deletes = append(g.deletes, strings.TrimPrefix(r.URL.Path, "/api/v1/workspace-keys/"))

		if g.delCode >= 400 {
			http.Error(w, g.delBody, g.delCode)

			return
		}

		_, _ = io.WriteString(w, g.delBody)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/access-requests/"):
		index := min(g.polls, len(g.statuses)-1)
		g.polls++

		_, _ = io.WriteString(w, `{"id":"abc","status":"`+g.statuses[index]+`","expiresAt":"2030-01-01T00:00:00Z"}`)
	default:
		http.NotFound(w, r)
	}
}

type keysRun struct {
	code           int
	stdout, stderr string
}

func runKeys(t *testing.T, gateway *keysGateway, dir, token string, args ...string) keysRun {
	t.Helper()

	server := httptest.NewServer(gateway)
	t.Cleanup(server.Close)

	var stdout, stderr bytes.Buffer

	env := workspaceEnv{
		stdout:    &stdout,
		stderr:    &stderr,
		now:       func() time.Time { return testNow },
		wait:      func(context.Context, time.Duration) error { return nil },
		configDir: func() (string, error) { return dir, nil },
	}

	access := gatewayAccess{gateway: server.URL, accessToken: token, attestation: "workload"}
	code := runWorkspace(t.Context(), env, args, func(call func(gatewayAccess) error) error { return call(access) })

	return keysRun{code, stdout.String(), stderr.String()}
}

func tokenWithLifetime(t *testing.T, lifetime time.Duration) string {
	t.Helper()

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: []byte("0123456789abcdef0123456789abcdef")}, nil)
	require.NoError(t, err)

	token, err := jwt.Signed(signer).Claims(jwt.Claims{
		IssuedAt: jwt.NewNumericDate(testNow), Expiry: jwt.NewNumericDate(testNow.Add(lifetime)),
	}).Serialize()
	require.NoError(t, err)

	return token
}

const pendingBody = `{"request":"abc","url":"https://gw/ui/approvals/abc","name":"dev","workspace":"registered/alice/dev","fingerprint":"SHA256:xyz","replaces":null}`

func TestWorkspaceRegister(t *testing.T) {
	t.Run("makes a key and waits for approval", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "config")
		gateway := &keysGateway{postCode: 202, postBody: pendingBody, statuses: []string{"pending", "approved"}}
		run := runKeys(t, gateway, dir, "not-a-jwt", "register", "dev")

		require.Equal(t, 0, run.code, run.stderr)

		path := filepath.Join(dir, "workspace-keys", "dev.pem")
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

		parent, err := os.Stat(filepath.Dir(path))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), parent.Mode().Perm())

		key, err := attestation.LoadKeyFile(path)
		require.NoError(t, err)

		var body map[string]string

		require.Len(t, gateway.posts, 1)
		require.NoError(t, json.Unmarshal(gateway.posts[0], &body))
		assert.Equal(t, "dev", body["name"])
		assert.Equal(t, base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)), body["publicKey"])
		assert.Equal(t, "workload", gateway.attested[0])

		assert.Contains(t, run.stdout, "Registered key `dev` with fingerprint SHA256:xyz.")
		assert.Contains(t, run.stdout, "Approve it at https://gw/ui/approvals/abc.")
		assert.Contains(t, run.stdout, "Key `dev` is active. Sessions that use it run in workspace `registered/alice/dev`. "+
			"To use it, run: toolgate configure --attestation-key-file "+path)
		assert.Equal(t, 2, gateway.polls)
		assert.Empty(t, run.stderr)

		pemText, err := os.ReadFile(path)
		require.NoError(t, err)

		for _, output := range []string{run.stdout, run.stderr, string(gateway.posts[0])} {
			assert.NotContains(t, output, string(key.Seed()))
			assert.NotContains(t, output, base64.RawURLEncoding.EncodeToString(key.Seed()))
			assert.NotContains(t, output, strings.Split(string(pemText), "\n")[1])
		}
	})

	t.Run("reuses an existing key file", func(t *testing.T) {
		_, private, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)

		path := writeKeyFile(t, private, 0o600)
		gateway := &keysGateway{postCode: 200, postBody: `{"status":"active","name":"dev","workspace":"registered/alice/dev","fingerprint":"SHA256:xyz"}`}
		run := runKeys(t, gateway, t.TempDir(), "x", "register", "dev", "--key-file", path)

		require.Equal(t, 0, run.code, run.stderr)

		var body map[string]string
		require.NoError(t, json.Unmarshal(gateway.posts[0], &body))
		assert.Equal(t, base64.RawURLEncoding.EncodeToString(private.Public().(ed25519.PublicKey)), body["publicKey"])
		assert.Contains(t, run.stdout, "Key `dev` is already registered and active. Its workspace is `registered/alice/dev`.")
	})

	t.Run("prints what the key replaces", func(t *testing.T) {
		body := strings.Replace(pendingBody, `"replaces":null`, `"replaces":"SHA256:old"`, 1)
		run := runKeys(t, &keysGateway{postCode: 202, postBody: body, statuses: []string{"approved"}}, t.TempDir(), "x", "register", "dev")

		require.Equal(t, 0, run.code, run.stderr)
		assert.Contains(t, run.stdout, "It replaces the key with fingerprint SHA256:old.")
	})

	t.Run("denied and expired requests exit 1", func(t *testing.T) {
		for _, status := range []string{"denied", "expired"} {
			run := runKeys(t, &keysGateway{postCode: 202, postBody: pendingBody, statuses: []string{status}}, t.TempDir(), "x", "register", "dev")

			assert.Equal(t, 1, run.code, status)
			assert.NotContains(t, run.stdout, "is active")
		}
	})

	t.Run("prints the gateway's error", func(t *testing.T) {
		run := runKeys(t, &keysGateway{postCode: 409, postBody: "Workspace keys are off on this gateway."}, t.TempDir(), "x", "register", "dev")

		assert.Equal(t, 1, run.code)
		assert.Contains(t, run.stderr, "Workspace keys are off on this gateway.")
	})

	t.Run("no warning for a long-lived access token", func(t *testing.T) {
		gateway := &keysGateway{postCode: 202, postBody: pendingBody, statuses: []string{"approved"}}
		run := runKeys(t, gateway, t.TempDir(), tokenWithLifetime(t, time.Hour), "register", "dev")

		require.Equal(t, 0, run.code, run.stderr)
		assert.Empty(t, run.stderr)
	})

	t.Run("usage errors exit 2", func(t *testing.T) {
		for _, args := range [][]string{{"register"}, {"register", "a", "b"}, {"register", "bad name"}} {
			run := runKeys(t, &keysGateway{}, t.TempDir(), "x", args...)

			assert.Equal(t, 2, run.code, args)
		}
	})
}

func TestWorkspaceKeysList(t *testing.T) {
	body := `{"enabled":true,"keys":[` +
		`{"name":"dev","workspace":"registered/alice/dev","fingerprint":"SHA256:aaa","status":"active","registeredAt":"2026-01-02T03:04:05Z","lastUsedAt":"2026-02-03T04:05:06Z"},` +
		`{"name":"new","workspace":"registered/alice/new","fingerprint":"SHA256:bbb","status":"pending","registeredAt":"2026-01-02T03:04:05Z","lastUsedAt":null}]}`

	t.Run("prints the table", func(t *testing.T) {
		run := runKeys(t, &keysGateway{listBody: body}, t.TempDir(), "x", "keys", "list")

		require.Equal(t, 0, run.code, run.stderr)
		assert.Regexp(t, `(?m)^NAME\s+WORKSPACE\s+FINGERPRINT\s+STATUS\s+REGISTERED\s+LAST USED$`, run.stdout)
		assert.Regexp(t, `(?m)^dev\s+registered/alice/dev\s+SHA256:aaa\s+active\s+2026-01-02T03:04:05Z\s+2026-02-03T04:05:06Z$`, run.stdout)
		assert.Regexp(t, `(?m)^new\s+registered/alice/new\s+SHA256:bbb\s+pending\s+2026-01-02T03:04:05Z\s+never$`, run.stdout)
		assert.NotContains(t, run.stdout, "are off")
	})

	t.Run("says when keys are off", func(t *testing.T) {
		off := strings.Replace(body, `"enabled":true`, `"enabled":false`, 1)
		run := runKeys(t, &keysGateway{listBody: off}, t.TempDir(), "x", "keys", "list")

		require.Equal(t, 0, run.code, run.stderr)
		assert.True(t, strings.HasPrefix(run.stdout, "Workspace keys are off on this gateway. These keys don't attest.\n"))
	})
}

func TestWorkspaceKeysRevoke(t *testing.T) {
	t.Run("prints both messages", func(t *testing.T) {
		gateway := &keysGateway{delBody: `{"revoked":true,"deniedRequests":2}`}
		run := runKeys(t, gateway, t.TempDir(), "x", "keys", "revoke", "dev")

		require.Equal(t, 0, run.code, run.stderr)
		assert.Equal(t, []string{"dev"}, gateway.deletes)
		assert.Equal(t, "Revoked key `dev`.\nDenied 2 pending requests for `dev`.\n", run.stdout)
	})

	t.Run("prints one message when nothing was denied", func(t *testing.T) {
		run := runKeys(t, &keysGateway{delBody: `{"revoked":true,"deniedRequests":0}`}, t.TempDir(), "x", "keys", "revoke", "dev")

		require.Equal(t, 0, run.code, run.stderr)
		assert.Equal(t, "Revoked key `dev`.\n", run.stdout)
	})

	t.Run("a missing key exits 1", func(t *testing.T) {
		run := runKeys(t, &keysGateway{delCode: 404, delBody: "You have no key named `dev`."}, t.TempDir(), "x", "keys", "revoke", "dev")

		assert.Equal(t, 1, run.code)
		assert.Contains(t, run.stderr, "You have no key named `dev`.")
		assert.Empty(t, run.stdout)
	})

	t.Run("usage errors exit 2", func(t *testing.T) {
		assert.Equal(t, 2, runKeys(t, &keysGateway{}, t.TempDir(), "x", "keys", "revoke").code)
		assert.Equal(t, 2, runKeys(t, &keysGateway{}, t.TempDir(), "x", "keys").code)
	})
}
