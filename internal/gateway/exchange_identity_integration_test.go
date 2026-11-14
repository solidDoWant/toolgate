//go:build integration

package gateway_test

import (
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

const (
	// jobPodToken signs in the service account of a pod a Job controls.
	jobPodToken = "job-pod-token"
	// barePodToken signs in a pod with no controller.
	barePodToken = "bare-pod-token"
)

// nonUserClient is a signed-in identity that has no user token.
type nonUserClient struct {
	name      string
	tag       string
	principal string
	client    user
}

// newNonUserFixture is an MCP fixture that signs in service accounts in
// namespace ci, through pod tokens, and SPIFFE workloads of allowed.test.
func newNonUserFixture(t *testing.T) (*mcpFixture, []nonUserClient) {
	t.Helper()

	kubernetes := fake.NewClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: "ci", Name: "build-1", UID: "build-uid",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: "build", UID: "job-uid", Controller: new(true),
			}},
		}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ci", Name: "debug", UID: "debug-uid"}},
	)

	pods := map[string][2]string{
		jobPodToken:  {"system:serviceaccount:ci:builder", "build-1/build-uid"},
		barePodToken: {"system:serviceaccount:ci:debugger", "debug/debug-uid"},
	}

	kubernetes.PrependReactor("create", "tokenreviews", func(action clienttesting.Action) (bool, k8sruntime.Object, error) {
		review := action.(clienttesting.CreateAction).GetObject().(*authenticationv1.TokenReview).DeepCopy()

		pod, found := pods[review.Spec.Token]
		if !found {
			return true, review, nil
		}

		name, uid, _ := strings.Cut(pod[1], "/")
		review.Status = authenticationv1.TokenReviewStatus{
			Authenticated: true,
			Audiences:     []string{auth.ServiceAccountAudience},
			User: authenticationv1.UserInfo{
				Username: pod[0],
				Extra: map[string]authenticationv1.ExtraValue{
					"authentication.kubernetes.io/pod-name": {name},
					"authentication.kubernetes.io/pod-uid":  {uid},
				},
			},
		}

		return true, review, nil
	})

	domain := testutil.NewSPIFFETrustDomain(t, "allowed.test")
	bundleServer := httptest.NewTLSServer(domain.BundleHandler(t))
	t.Cleanup(bundleServer.Close)

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: bundleServer.Certificate().Raw}), 0o600))

	fixture := newMCPFixtureWithAuth(t, &auth.Options{
		ServiceAccounts: &auth.ServiceAccountOptions{Client: kubernetes, Namespaces: []string{"ci"}},
		SPIFFE: &auth.SPIFFEOptions{
			TrustDomains: []auth.SPIFFETrustDomain{{Name: "allowed.test", BundleEndpointURL: bundleServer.URL, CAFile: caFile}},
		},
	})

	return fixture, []nonUserClient{
		{name: "service account", tag: "job", principal: "system:serviceaccount:ci:builder", client: user{token: jobPodToken, session: "session-1"}},
		{name: "SPIFFE workload", tag: "spiffe", principal: "spiffe://allowed.test/agent", client: user{token: domain.MintSVID(t, "/agent"), session: "session-1"}},
		{name: "pod sign-in", tag: "pod", principal: "system:serviceaccount:ci:debugger", client: user{token: barePodToken, session: "session-1"}},
	}
}

// addExchangeTemplate offers a per-user template whose Secret takes a token
// exchanged from the caller's sign-in, to every identity in clients and to
// alice.
func (f *mcpFixture) addExchangeTemplate(name string, clients []nonUserClient) *v1alpha1.MCPServerTemplate {
	access := v1alpha1.Access{Users: []string{"alice"}, Environments: "any"}

	for _, client := range clients {
		if client.name == "SPIFFE workload" {
			access.SPIFFEIDs = append(access.SPIFFEIDs, client.principal)
		} else {
			access.ServiceAccounts = append(access.ServiceAccounts, client.principal)
		}
	}

	template := f.addEnableTemplate(name, "per-user", access)
	template.Spec.Resources = []k8sruntime.RawExtension{{Raw: []byte(
		`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"token"},"stringData":{"token":"${exchange:temporal}"}}`,
	)}}
	f.catalog.Set(template, nil)

	return template
}

func noUserTokenMessage(server string) string {
	return "`" + server + "` needs a token exchanged from your sign-in. Service accounts, SPIFFE workloads and pod sign-in have none, so they can't use it."
}

// sessionPK returns the sessions row ID of the client's session.
func (f *mcpFixture) sessionPK(t *testing.T, principal string) int64 {
	t.Helper()

	return f.sessionRow(t, principal, "session-1").ID
}

func TestExchangeTemplateRefusesNonUserIdentities(t *testing.T) {
	fixture, clients := newNonUserFixture(t)

	for _, client := range clients {
		t.Run("an enable by a "+client.name+" is refused with no-user-token and writes no row", func(t *testing.T) {
			template := fixture.addExchangeTemplate("enable-"+client.tag, clients)
			session := fixture.connect(t, client.client)

			result := callTool(t, session, gateway.ToolEnableServer, map[string]any{"name": template.Name})

			require.True(t, result.IsError)
			assert.Equal(t, noUserTokenMessage(template.Name), resultText(result))

			enables := fixture.auditEvents(t, "server.enable", template.Name)
			require.Len(t, enables, 1)
			assert.Equal(t, "refuse", enables[0]["decision"])
			assert.Equal(t, "no-user-token", enables[0]["reason"])
			assert.Empty(t, fixture.instancesOf(t, template))
			assert.Empty(t, fixture.enabledServers(t, client.principal, "session-1"))
		})

		t.Run("a call that enables, by a "+client.name+", is refused with no-user-token and writes no row", func(t *testing.T) {
			template := fixture.addExchangeTemplate("call-"+client.tag, clients)
			session := fixture.connect(t, client.client)

			result := callTool(t, session, template.Name+"__echo", nil)

			require.True(t, result.IsError)
			assert.Equal(t, noUserTokenMessage(template.Name), resultText(result))

			decisions := fixture.auditEvents(t, "tool.decision", template.Name)
			require.Len(t, decisions, 1)
			assert.Equal(t, "refuse", decisions[0]["decision"])
			assert.Equal(t, "no-user-token", decisions[0]["reason"])
			assert.Empty(t, fixture.auditEvents(t, "server.enable", template.Name))
			assert.Empty(t, fixture.instancesOf(t, template))
			assert.Empty(t, fixture.enabledServers(t, client.principal, "session-1"))
		})

		t.Run("a call that would start an instance, by a "+client.name+", is refused with no-user-token and starts none", func(t *testing.T) {
			template := fixture.addExchangeTemplate("start-"+client.tag, clients)
			session := fixture.connect(t, client.client)

			// The server was enabled before the template gained its
			// exchange, and its instance failed on that older generation, so
			// this call would start a new one.
			require.NoError(t, db.New(fixture.pool).EnableServer(t.Context(), db.EnableServerParams{
				SessionPk: fixture.sessionPK(t, client.principal), TemplateNs: template.Namespace, TemplateName: template.Name,
			}))
			fixture.addInstance(t, template, "user:"+client.principal, template.Generation-1, "failed", "it crashed")

			result := callTool(t, session, template.Name+"__echo", nil)

			require.True(t, result.IsError)
			assert.Equal(t, noUserTokenMessage(template.Name), resultText(result))

			decisions := fixture.auditEvents(t, "tool.decision", template.Name)
			require.Len(t, decisions, 1)
			assert.Equal(t, "no-user-token", decisions[0]["reason"])
			assert.Len(t, fixture.instancesOf(t, template), 1, "no new instance is inserted")
		})

		t.Run("a resume by a "+client.name+" drops the server with server.disable and no-user-token", func(t *testing.T) {
			template := fixture.addExchangeTemplate("resume-"+client.tag, clients)
			fixture.connect(t, client.client)

			require.NoError(t, db.New(fixture.pool).EnableServer(t.Context(), db.EnableServerParams{
				SessionPk: fixture.sessionPK(t, client.principal), TemplateNs: template.Namespace, TemplateName: template.Name,
			}))
			_, err := fixture.pool.Exec(t.Context(), "UPDATE sessions SET ended_at = now() WHERE principal = $1 AND session_id = 'session-1'", client.principal)
			require.NoError(t, err)

			fixture.connect(t, client.client)

			disables := fixture.auditEvents(t, "server.disable", template.Name)
			require.Len(t, disables, 1)
			assert.Equal(t, "refuse", disables[0]["decision"])
			assert.Equal(t, "no-user-token", disables[0]["reason"])
			assert.Empty(t, fixture.auditEvents(t, "server.enable", template.Name))
			assert.Empty(t, fixture.instancesOf(t, template))
			assert.Empty(t, fixture.enabledServers(t, client.principal, "session-1"))
		})
	}

	t.Run("a user's call enables the server and starts its instance", func(t *testing.T) {
		template := fixture.addExchangeTemplate("user-call", clients)
		fixture.startUpstream(t, template.Name)
		session := fixture.connect(t, alice())

		var wait sync.WaitGroup

		var (
			result  *mcp.CallToolResult
			callErr error
		)

		wait.Go(func() {
			result, callErr = session.CallTool(t.Context(), &mcp.CallToolParams{
				Name: template.Name + "__echo", Arguments: map[string]any{"text": "x"},
			})
		})

		instance := fixture.awaitInstance(t, template, "user:alice")
		fixture.markReady(t, instance.ID, "echo")
		wait.Wait()

		require.NoError(t, callErr)
		require.False(t, result.IsError, resultText(result))
		assert.Equal(t, []string{template.Name}, fixture.enabledServers(t, "alice", "session-1"))
	})
}
