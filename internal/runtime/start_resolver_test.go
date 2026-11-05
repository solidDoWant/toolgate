package runtime_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const resolverSecretResource = `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"creds"},"stringData":{"token":"${resolver:github-app}"}}`

func TestStarterPendingResolvesIntoTheSecret(t *testing.T) {
	fixture := newStarterFixture(t, testTemplate(resolverSecretResource), "pending")
	session := fixture.store.sessions[7]
	session.Groups = []string{"dev"}
	fixture.store.sessions[7] = session

	resolver := newResolverServer(t)
	expiresAt := resolverNow.Add(time.Hour)
	resolver.answer(http.StatusOK, validAnswer("resolved-value", expiresAt))
	fixture.starter.Resolvers = resolver.client(t, "/resolve")

	require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

	status, _, _, _ := fixture.store.snapshot()
	assert.Equal(t, "starting", status)
	assert.True(t, expiresAt.Equal(fixture.store.resolvedExpiresAt), "the starting write records the expiry")

	secret := &corev1.Secret{}
	require.NoError(t, fixture.kube.Get(t.Context(), client.ObjectKey{Namespace: instanceNamespace, Name: "creds"}, secret))
	assert.Equal(t, "resolved-value", secret.StringData["token"]+string(secret.Data["token"]))

	require.Len(t, resolver.requests, 1, "the resolver is called once")
	body := resolver.requests[0].body
	assert.Equal(t, map[string]any{"id": "alice", "email": "alice@example.com", "name": "Alice", "groups": []any{"dev"}}, body["principal"])
	assert.Equal(t, instanceID, body["instance"])
	assert.NotContains(t, body, "workspace", "a per-user instance has no workspace")
}

func TestStarterPendingResolverFailureFails(t *testing.T) {
	fixture := newStarterFixture(t, testTemplate(resolverSecretResource), "pending")

	resolver := newResolverServer(t)
	resolver.answer(http.StatusServiceUnavailable, "body-text")
	fixture.starter.Resolvers = resolver.client(t, "/resolve")

	require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

	status, reason, _, _ := fixture.store.snapshot()
	assert.Equal(t, "failed", status)
	assert.Contains(t, reason, "resolver `github-app` answered HTTP 503")
	assert.NotContains(t, reason, "body-text")

	secrets := &corev1.SecretList{}
	require.NoError(t, fixture.kube.List(t.Context(), secrets, client.InNamespace(instanceNamespace)))
	assert.Empty(t, secrets.Items, "nothing is applied after a resolver failure")
}
