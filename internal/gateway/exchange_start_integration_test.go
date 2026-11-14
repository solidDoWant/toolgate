//go:build integration

package gateway_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/keyset"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/config"
	"github.com/solidDoWant/toolgate/internal/credentials"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/secrets"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// fakeTokenEndpoint answers token exchanges with a token naming the
// subject's `sub` and a counter, or with an OAuth error when refuse is set.
type fakeTokenEndpoint struct {
	// requests counts every exchange request, answered counts the tokens
	// issued.
	requests atomic.Int64
	answered atomic.Int64
	refuse   atomic.Value
}

func (e *fakeTokenEndpoint) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	e.requests.Add(1)

	if err := request.ParseForm(); err != nil {
		http.Error(writer, "bad form", http.StatusBadRequest)

		return
	}

	writer.Header().Set("Content-Type", "application/json")

	if code, _ := e.refuse.Load().(string); code != "" {
		writer.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(writer).Encode(map[string]any{"error": code})

		return
	}

	subject := "unknown"

	if parsed, err := jwt.ParseSigned(request.PostForm.Get("subject_token"), []jose.SignatureAlgorithm{jose.RS256, jose.ES256}); err == nil {
		var claims jwt.Claims
		if parsed.UnsafeClaimsWithoutVerification(&claims) == nil {
			subject = claims.Subject
		}
	}

	count := e.answered.Add(1)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"access_token": fmt.Sprintf("exchanged-for-%s-%d", subject, count), "token_type": "Bearer", "expires_in": 3600,
	})
}

// exchangeFixture is an MCP fixture with the token exchange `temporal`
// pointed at a fake token endpoint, and a stand-in leader that keeps the
// delivered tokens and starts pending instances with them.
type exchangeFixture struct {
	*mcpFixture

	endpoint *fakeTokenEndpoint
	tokens   *credentials.DeliveredTokens
	starter  *runtime.Starter
	// dropDeliveries, while positive, counts down deliveries the leader
	// loses.
	dropDeliveries atomic.Int64

	appliedMutex sync.Mutex
	// applied holds each object the starter applied, as kind/name, with
	// the token of a Secret's token key after a space.
	applied map[string][]string
}

func newExchangeStartFixture(t *testing.T, extra ...func(*gateway.MCPOptions)) *exchangeFixture {
	t.Helper()

	endpoint := &fakeTokenEndpoint{}
	server := httptest.NewTLSServer(http.HandlerFunc(endpoint.serveHTTP))
	t.Cleanup(server.Close)

	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600))

	secretFile := filepath.Join(dir, "client-secret")
	require.NoError(t, os.WriteFile(secretFile, []byte("client-secret"), 0o600))

	exchanger, err := credentials.NewExchanger(config.TokenExchange{
		Name: "temporal", TokenURL: server.URL + "/token", ClientID: "toolgate", ClientSecretFile: secretFile, CAFile: caFile,
	}, nil, nil)
	require.NoError(t, err)

	handle, err := keyset.NewHandle(aead.AES256GCMKeyTemplate())
	require.NoError(t, err)

	cipher, err := secrets.NewCipher(handle)
	require.NoError(t, err)

	fixture := &exchangeFixture{endpoint: endpoint, tokens: &credentials.DeliveredTokens{}, applied: map[string][]string{}}
	fixture.mcpFixture = newMCPFixture(t, func(options *gateway.MCPOptions) {
		options.TokenExchanges = []*credentials.Exchanger{exchanger}
		options.CredentialsCipher = cipher

		for _, change := range extra {
			change(options)
		}
	})

	fixture.listenForDeliveries(t, cipher)

	kube := clientfake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
		Apply: func(_ context.Context, _ client.WithWatch, object k8sruntime.ApplyConfiguration, _ ...client.ApplyOption) error {
			content, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(object)
			if err != nil {
				return err
			}

			fixture.recordApply(content)

			return nil
		},
	}).Build()

	fixture.starter = &runtime.Starter{
		Store:            runtime.PostgresStartStore{Pool: fixture.pool},
		Client:           kube,
		Template:         fixture.catalog.Running,
		Secrets:          func(string) runtime.SecretSource { return nil },
		Deliveries:       fixture.tokens,
		GatewayNamespace: "toolgate",
		GatewayPodLabels: map[string]string{"app.kubernetes.io/name": "toolgate"},
		Logger:           slog.New(slog.DiscardHandler),
	}

	return fixture
}

// listenForDeliveries is the leader's receiving side: it opens each
// delivered token and keeps it for the starter.
func (f *exchangeFixture) listenForDeliveries(t *testing.T, cipher *secrets.Cipher) {
	t.Helper()

	listener := store.NewListener(f.pool, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	payloads, unsubscribe := listener.SubscribeCredentials()
	t.Cleanup(unsubscribe)

	go func() { _ = listener.Run(ctx) }()

	select {
	case <-listener.Listening():
	case <-t.Context().Done():
	}

	receiver := &runtime.DeliveryReceiver{Cipher: cipher, Store: runtime.PostgresDeliveryStore{Pool: f.pool}, Tokens: f.tokens}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case payload := <-payloads:
				if f.dropDeliveries.Add(-1) >= 0 {
					continue
				}

				_, _ = receiver.Receive(ctx, payload)
			}
		}
	}()
}

func (f *exchangeFixture) recordApply(content map[string]any) {
	metadata, _ := content["metadata"].(map[string]any)
	namespace, _ := metadata["namespace"].(string)
	name, _ := metadata["name"].(string)
	entry := fmt.Sprintf("%v/%s", content["kind"], name)

	if data, ok := content["data"].(map[string]any); ok && content["kind"] == "Secret" {
		if encoded, ok := data["token"].(string); ok {
			decoded, _ := base64.StdEncoding.DecodeString(encoded)
			entry += " " + string(decoded)
		}
	}

	if data, ok := content["stringData"].(map[string]any); ok && content["kind"] == "Secret" {
		if token, ok := data["token"].(string); ok {
			entry += " " + token
		}
	}

	f.appliedMutex.Lock()
	defer f.appliedMutex.Unlock()

	f.applied[namespace] = append(f.applied[namespace], entry)
}

// appliedIn returns the objects applied in namespace, in order.
func (f *exchangeFixture) appliedIn(namespace string) []string {
	f.appliedMutex.Lock()
	defer f.appliedMutex.Unlock()

	return append([]string{}, f.applied[namespace]...)
}

// addTokenTemplate offers a per-user template to alice and bob whose
// Secret takes the `temporal` token, followed by a ConfigMap and a Pod.
func (f *exchangeFixture) addTokenTemplate(name string) *v1alpha1.MCPServerTemplate {
	template := f.addEnableTemplate(name, "per-user", v1alpha1.Access{Users: []string{"alice", "bob"}, Environments: "any"})
	template.Spec.Start = "onEnable"
	template.Spec.Resources = []k8sruntime.RawExtension{
		{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings"},"data":{"a":"b"}}`)},
		{Raw: []byte(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"server"},"spec":{"containers":[{"name":"server","image":"ghcr.io/example/server@sha256:0000000000000000000000000000000000000000000000000000000000000000"}]}}`)},
		{Raw: []byte(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"token"},"stringData":{"token":"${exchange:temporal}"}}`)},
	}
	f.catalog.Set(template, nil)

	return template
}

// awaitDelivery waits until the leader holds a token for the instance.
func (f *exchangeFixture) awaitDelivery(t *testing.T, instanceID string) credentials.Delivery {
	t.Helper()

	var delivery credentials.Delivery

	require.Eventually(t, func() bool {
		held, found := f.tokens.Get(instanceID, "temporal")
		delivery = held

		return found
	}, 30*time.Second, 10*time.Millisecond, "the exchanged token reaches the leader")

	return delivery
}

// step runs one start pass of the leader for the instance and returns it.
func (f *exchangeFixture) step(t *testing.T, id string) db.Instance {
	t.Helper()

	instance, err := db.New(f.pool).GetInstance(t.Context(), id)
	require.NoError(t, err)
	require.NoError(t, f.starter.Step(t.Context(), instance))

	instance, err = db.New(f.pool).GetInstance(t.Context(), id)
	require.NoError(t, err)

	return instance
}

func (f *exchangeFixture) exchangeEvents(t *testing.T, server string) []map[string]any {
	t.Helper()

	return f.auditEvents(t, "credential.exchange", server)
}

func TestEnableRendersExchangedTokenBeforeOtherObjects(t *testing.T) {
	fixture := newExchangeStartFixture(t)
	template := fixture.addTokenTemplate("temporal-mcp")
	session := fixture.connect(t, alice())

	results := startEnable(t, session, template.Name)
	instance := fixture.awaitInstance(t, template, "user:alice")

	assert.True(t, instance.UsesExchange, "the inserted row records that its generation exchanges")

	delivery := fixture.awaitDelivery(t, instance.ID)
	assert.Equal(t, "alice", delivery.Principal)

	instance = fixture.step(t, instance.ID)
	require.Equal(t, "starting", instance.Status, instance.Reason)

	applied := fixture.appliedIn(instance.Namespace)
	require.NotEmpty(t, applied)

	secretAt, configMapAt, podAt := -1, -1, -1

	for index, entry := range applied {
		switch {
		case strings.HasPrefix(entry, "Secret/token "):
			secretAt = index

			assert.Equal(t, "Secret/token exchanged-for-alice-1", entry)
		case entry == "ConfigMap/settings":
			configMapAt = index
		case entry == "Pod/server":
			podAt = index
		}
	}

	require.GreaterOrEqual(t, secretAt, 0, "the Secret with the token is applied: %v", applied)
	assert.Less(t, secretAt, configMapAt, "the Secret comes before the template's other objects: %v", applied)
	assert.Less(t, secretAt, podAt, "the Secret comes before the template's other objects: %v", applied)

	fixture.markReady(t, instance.ID, "echo")

	result := awaitEnable(t, results)
	require.Empty(t, result.err)

	events := fixture.exchangeEvents(t, template.Name)
	require.Len(t, events, 1)
	assert.Equal(t, instance.ID, events[0]["instance"])
	assert.Equal(t, "temporal", events[0]["exchange"])
	assert.Equal(t, "start", events[0]["kind"])
	assert.Equal(t, "success", events[0]["outcome"])
	assert.NotEmpty(t, events[0]["expiresAt"])
}

func TestEnableFailsWithExchangeErrorCode(t *testing.T) {
	fixture := newExchangeStartFixture(t)
	fixture.endpoint.refuse.Store("invalid_grant")

	template := fixture.addTokenTemplate("temporal-mcp")
	session := fixture.connect(t, alice())

	results := startEnable(t, session, template.Name)
	instance := fixture.awaitInstance(t, template, "user:alice")

	require.Eventually(t, func() bool { return len(fixture.exchangeEvents(t, template.Name)) == 1 },
		30*time.Second, 10*time.Millisecond, "the failed exchange is recorded")

	reason := "Token exchange `temporal` failed: the token endpoint answered `invalid_grant`."

	rows, err := db.New(fixture.pool).ListInstanceExchanges(t.Context(), instance.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, reason, rows[0].LastError.String)
	assert.True(t, rows[0].LastErrorAt.Valid)
	assert.False(t, rows[0].ClaimedAt.Valid, "a failure gives up the claim")

	event := fixture.exchangeEvents(t, template.Name)[0]
	assert.Equal(t, "failure", event["outcome"])
	assert.Equal(t, "invalid_grant", event["error"])
	assert.Equal(t, "start", event["kind"])

	instance = fixture.step(t, instance.ID)
	assert.Equal(t, "failed", instance.Status)

	result := awaitEnable(t, results)
	assert.Equal(t, "`temporal-mcp` failed to start: "+reason, result.err)
	assert.Equal(t, int64(1), fixture.endpoint.requests.Load(), "a failed start isn't exchanged again")
	assert.Len(t, fixture.exchangeEvents(t, template.Name), 1)
}

func TestConcurrentStartsExchangeOnce(t *testing.T) {
	fixture := newExchangeStartFixture(t)
	template := fixture.addTokenTemplate("temporal-mcp")

	first, second := alice(), alice()
	second.session = "session-2"

	results := []<-chan enableResult{
		startEnable(t, fixture.connect(t, first), template.Name),
		startEnable(t, fixture.connect(t, second), template.Name),
	}

	instance := fixture.awaitInstance(t, template, "user:alice")
	fixture.awaitDelivery(t, instance.ID)

	// Both enables wait on the one row before it starts.
	require.Eventually(t, func() bool { return len(fixture.auditEvents(t, "server.enable", template.Name)) == 2 },
		30*time.Second, 10*time.Millisecond)

	instance = fixture.step(t, instance.ID)
	require.Equal(t, "starting", instance.Status, instance.Reason)
	fixture.markReady(t, instance.ID, "echo")

	for _, results := range results {
		require.Empty(t, awaitEnable(t, results).err)
	}

	assert.Len(t, fixture.instancesOf(t, template), 1)
	assert.Equal(t, int64(1), fixture.endpoint.answered.Load(), "one exchange for the one instance")
	assert.Len(t, fixture.exchangeEvents(t, template.Name), 1)
}

func TestLostDeliveryIsRetriedByWaitingRequest(t *testing.T) {
	fixture := newExchangeStartFixture(t)
	fixture.dropDeliveries.Store(1)

	template := fixture.addTokenTemplate("temporal-mcp")
	session := fixture.connect(t, alice())

	results := startEnable(t, session, template.Name)
	instance := fixture.awaitInstance(t, template, "user:alice")

	require.Eventually(t, func() bool { return len(fixture.exchangeEvents(t, template.Name)) == 1 },
		30*time.Second, 10*time.Millisecond, "the first exchange runs")

	_, held := fixture.tokens.Get(instance.ID, "temporal")
	require.False(t, held, "the first delivery is lost")

	// The claim expires: its time is older than the exchange's timeout and
	// its margin. The notification wakes the waiting enable.
	require.NoError(t, pgx.BeginFunc(t.Context(), fixture.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(),
			"UPDATE instance_exchanges SET claimed_at = now() - interval '1 hour' WHERE instance_id = $1", instance.ID); err != nil {
			return err
		}

		return store.Notify(t.Context(), tx, store.KindInstance, instance.ID)
	}))

	delivery := fixture.awaitDelivery(t, instance.ID)
	assert.Equal(t, credentials.Token("exchanged-for-alice-2"), delivery.Token)

	instance = fixture.step(t, instance.ID)
	require.Equal(t, "starting", instance.Status, instance.Reason)
	fixture.markReady(t, instance.ID, "echo")

	require.Empty(t, awaitEnable(t, results).err)
	assert.Equal(t, int64(2), fixture.endpoint.answered.Load())
}

func TestTwoUsersGetTheirOwnTokens(t *testing.T) {
	fixture := newExchangeStartFixture(t)
	template := fixture.addTokenTemplate("temporal-mcp")

	for _, client := range []user{alice(), bob()} {
		principal := client.claims.Subject

		results := startEnable(t, fixture.connect(t, client), template.Name)
		instance := fixture.awaitInstance(t, template, "user:"+principal)

		delivery := fixture.awaitDelivery(t, instance.ID)
		assert.Equal(t, principal, delivery.Principal)
		assert.Equal(t, principal, instance.OwnerPrincipal)
		assert.True(t, strings.HasPrefix(string(delivery.Token), "exchanged-for-"+principal+"-"),
			"the token was exchanged with %s's own token", principal)

		instance = fixture.step(t, instance.ID)
		require.Equal(t, "starting", instance.Status, instance.Reason)
		assert.Contains(t, fixture.appliedIn(instance.Namespace), "Secret/token "+string(delivery.Token))

		fixture.markReady(t, instance.ID, "echo")
		require.Empty(t, awaitEnable(t, results).err)
	}

	instances := fixture.instancesOf(t, template)
	require.Len(t, instances, 2)
	assert.NotEqual(t, instances[0].Namespace, instances[1].Namespace)
	assert.Equal(t, int64(2), fixture.endpoint.answered.Load())
}

// A template with persistent volumes waits for its volumes before anyone
// exchanges: the enable inserts closed exchange rows and runs no exchange.
func TestEnableWithVolumesInsertsClosedExchangeRows(t *testing.T) {
	fixture := newExchangeStartFixture(t)
	template := fixture.addTokenTemplate("temporal-mcp")
	template.Spec.PersistentVolumes = []v1alpha1.PersistentVolume{{Name: "data", Size: "1Gi"}}
	fixture.catalog.Set(template, nil)
	session := fixture.connect(t, alice())

	results := startEnable(t, session, template.Name)
	instance := fixture.awaitInstance(t, template, "user:alice")
	assert.True(t, instance.UsesExchange)

	rows, err := db.New(fixture.pool).ListInstanceExchanges(t.Context(), instance.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.False(t, rows[0].WantedSince.Valid, "the row is closed until the leader has the volumes")
	assert.False(t, rows[0].ClaimedAt.Valid, "the inserting request holds no claim")
	assert.Equal(t, pgtype.Infinity, rows[0].RefreshAfter.InfinityModifier, "no request may claim the row")

	// This leader has persistent volumes off, so the start fails at the
	// volume step. The enable returns only after its own exchanges ran.
	instance = fixture.step(t, instance.ID)
	require.Equal(t, "failed", instance.Status)
	require.NotEmpty(t, awaitEnable(t, results).err)

	assert.Zero(t, fixture.endpoint.requests.Load(), "no exchange runs while the row waits for its volumes")
	assert.Empty(t, fixture.exchangeEvents(t, template.Name))
	_, held := fixture.tokens.Get(instance.ID, "temporal")
	assert.False(t, held)
}
