//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

// gatewayPodSelector selects the gateway pods of a release named toolgate.
const gatewayPodSelector = "app.kubernetes.io/name=toolgate,app.kubernetes.io/instance=toolgate"

// enableTimeout bounds one toolgate_enable_server call: an instance start
// on a kind node, plus a leader change in the failover test.
const enableTimeout = 4 * time.Minute

// testServerTools are the tools the test MCP server of the e2e templates
// serves.
var testServerTools = []string{"echo", "env"}

// templateClient returns a controller-runtime client that knows the
// toolgate custom resources.
func templateClient(t *testing.T) client.Client {
	t.Helper()

	// The client logs nothing the tests need.
	ctrllog.SetLogger(logr.Discard())

	scheme := k8sruntime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	kube, err := client.New(adminConfig(t), client.Options{Scheme: scheme})
	require.NoError(t, err)

	return kube
}

// testServerResources are a Deployment and a Service that run the test MCP
// server under Pod Security restricted. readinessDelay holds the pod
// unready for that long, so a test can act while the instance starts.
func testServerResources(t *testing.T, image string, readinessDelay time.Duration, extraEnv map[string]string) []k8sruntime.RawExtension {
	t.Helper()

	return testServerResourcesWithTools(t, image, testServerTools, readinessDelay, extraEnv)
}

// testServerResourcesWithTools is testServerResources for a test MCP server
// that exposes tools.
func testServerResourcesWithTools(t *testing.T, image string, tools []string, readinessDelay time.Duration, extraEnv map[string]string) []k8sruntime.RawExtension {
	t.Helper()

	env := []map[string]any{{"name": "MCPSERVER_TOOLS", "value": strings.Join(tools, ",")}}
	for name, value := range extraEnv {
		env = append(env, map[string]any{"name": name, "value": value})
	}

	labels := map[string]any{"app": "mcp"}
	deployment := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "mcp"},
		"spec": map[string]any{
			"replicas": 1,
			"selector": map[string]any{"matchLabels": labels},
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"securityContext": map[string]any{
						"runAsNonRoot":   true,
						"seccompProfile": map[string]any{"type": "RuntimeDefault"},
					},
					"containers": []any{map[string]any{
						"name":            "mcp",
						"image":           image,
						"imagePullPolicy": "Never",
						"env":             env,
						"ports":           []any{map[string]any{"name": "mcp", "containerPort": 8080}},
						"securityContext": map[string]any{
							"allowPrivilegeEscalation": false,
							"capabilities":             map[string]any{"drop": []any{"ALL"}},
						},
						"readinessProbe": map[string]any{
							"tcpSocket":           map[string]any{"port": 8080},
							"initialDelaySeconds": int(readinessDelay.Seconds()),
							"periodSeconds":       1,
						},
					}},
				},
			},
		},
	}
	service := map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   map[string]any{"name": "mcp"},
		"spec": map[string]any{
			"selector": labels,
			"ports":    []any{map[string]any{"name": "mcp", "port": 8080, "targetPort": 8080}},
		},
	}

	var resources []k8sruntime.RawExtension

	for _, object := range []map[string]any{deployment, service} {
		raw, err := json.Marshal(object)
		require.NoError(t, err)

		resources = append(resources, k8sruntime.RawExtension{Raw: raw})
	}

	return resources
}

// createTemplate creates an MCPServerTemplate in the release namespace that
// alice and bob may use, deletes it when the test ends, and returns its
// name.
func createTemplate(t *testing.T, kube client.Client, prefix, scope string, resources []k8sruntime.RawExtension) string {
	t.Helper()

	name := prefix + "-" + rand.String(5)
	template := &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: releaseNamespace(), Name: name},
		Spec: v1alpha1.MCPServerTemplateSpec{
			Scope:        scope,
			StartTimeout: &metav1.Duration{Duration: enableTimeout},
			Access:       v1alpha1.Access{Users: []string{"alice", "bob"}, Environments: "any"},
			Endpoint:     v1alpha1.Endpoint{Service: "mcp", Port: 8080, Path: "/mcp"},
			Resources:    resources,
		},
	}
	require.NoError(t, kube.Create(t.Context(), template))

	t.Cleanup(func() {
		_ = kube.Delete(context.WithoutCancel(t.Context()), template)
	})

	return name
}

// requireTemplateValid waits until the leader reports the template valid,
// so the gateway's catalog has it.
func requireTemplateValid(t *testing.T, kube client.Client, name string) {
	t.Helper()

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		var template v1alpha1.MCPServerTemplate
		require.NoError(collect, kube.Get(t.Context(), types.NamespacedName{Namespace: releaseNamespace(), Name: name}, &template))
		assert.True(collect, meta.IsStatusConditionTrue(template.Status.Conditions, catalog.ConditionValid),
			"template %s is valid: %+v", name, template.Status.Conditions)
	}, 2*time.Minute, time.Second)
}

// gatewayPods returns the names of the running, ready gateway pods that are
// not being deleted.
func gatewayPods(t *testing.T, admin *kubernetes.Clientset) []string {
	t.Helper()

	var names []string

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		pods, err := admin.CoreV1().Pods(releaseNamespace()).List(t.Context(), metav1.ListOptions{LabelSelector: gatewayPodSelector})
		require.NoError(collect, err)

		names = nil

		for index := range pods.Items {
			pod := &pods.Items[index]
			if pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning && podReady(pod) {
				names = append(names, pod.Name)
			}
		}

		assert.Len(collect, names, 2, "two gateway replicas are ready")
	}, 3*time.Minute, 2*time.Second)

	slices.Sort(names)

	return names
}

// forwardToGateway forwards a local port to port 8080 of a gateway pod and
// returns the MCP endpoint URL on it. The API server's proxy drops the
// bearer token, so the tests reach the pods through a port-forward.
func forwardToGateway(t *testing.T, admin *kubernetes.Clientset, pod string) string {
	t.Helper()

	config := adminConfig(t)
	roundTripper, upgrader, err := spdy.RoundTripperFor(config)
	require.NoError(t, err)

	url := admin.CoreV1().RESTClient().Post().Resource("pods").Namespace(releaseNamespace()).Name(pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, url)

	stop, ready := make(chan struct{}), make(chan struct{})
	forwarder, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{"0:8080"}, stop, ready, io.Discard, io.Discard)
	require.NoError(t, err)

	errs := make(chan error, 1)

	go func() { errs <- forwarder.ForwardPorts() }()

	t.Cleanup(func() { close(stop) })

	select {
	case <-ready:
	case err := <-errs:
		require.NoError(t, err, "port-forward to %s", pod)
	case <-time.After(time.Minute):
		require.FailNow(t, "port-forward to "+pod+" is not ready")
	}

	ports, err := forwarder.GetPorts()
	require.NoError(t, err)
	require.Len(t, ports, 1)

	return fmt.Sprintf("http://127.0.0.1:%d%s", ports[0].Local, gateway.MCPPath)
}

// bearerTransport adds a user's token and session header to every request.
type bearerTransport struct{ header http.Header }

func (b bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	for name, values := range b.header {
		request.Header[name] = values
	}

	return http.DefaultTransport.RoundTrip(request)
}

// gatewaySession is an MCP client session with the gateway that records
// the notifications it receives, in order.
type gatewaySession struct {
	session *mcp.ClientSession

	mu     sync.Mutex
	events []string
}

const (
	eventProgress    = "progress"
	eventListChanged = "list_changed"
)

func (g *gatewaySession) recorded() []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	return slices.Clone(g.events)
}

func (g *gatewaySession) record(event string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.events = append(g.events, event)
}

// connectGateway opens an MCP session with the gateway at endpoint as user,
// with the toolgate session ID session.
func connectGateway(t *testing.T, endpoint, user, session string) *gatewaySession {
	t.Helper()

	return connectGatewayWithHeader(t, endpoint, user, session, nil)
}

// connectGatewayInWorkspace is connectGateway that also claims workspace,
// unless it is empty.
func connectGatewayInWorkspace(t *testing.T, endpoint, user, session, workspace string) *gatewaySession {
	t.Helper()

	extra := http.Header{}
	if workspace != "" {
		extra.Set(auth.WorkspaceHeader, workspace)
	}

	return connectGatewayWithHeader(t, endpoint, user, session, extra)
}

// connectGatewayWithHeader is connectGateway with extra headers on every
// request.
func connectGatewayWithHeader(t *testing.T, endpoint, user, session string, extra http.Header) *gatewaySession {
	t.Helper()

	header := gatewayHeader(t, user, session, extra)

	observed := &gatewaySession{}
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "toolgate-e2e", Version: "0"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { observed.record(eventListChanged) },
		ProgressNotificationHandler: func(context.Context, *mcp.ProgressNotificationClientRequest) {
			observed.record(eventProgress)
		},
	})

	connected, err := mcpClient.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: bearerTransport{header: header}},
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = connected.Close() })

	observed.session = connected

	return observed
}

// gatewayHeader returns extra with a fresh access token of user and the
// toolgate session ID.
func gatewayHeader(t *testing.T, user, session string, extra http.Header) http.Header {
	t.Helper()

	header := extra.Clone()
	if header == nil {
		header = http.Header{}
	}

	header.Set("Authorization", "Bearer "+oidcProvider(t).MintAccessToken(t, testutil.TokenClaims{
		Subject: user, Email: user + "@example.com", EmailVerified: true,
	}))
	header.Set(auth.SessionHeader, session)

	return header
}

// serverCall is the outcome of one meta tool call.
type serverCall struct {
	tools []string
	err   string
}

// callServerTool calls a meta tool that takes a server name, with a
// progress token, and returns its tools or its error text.
func callServerTool(ctx context.Context, session *mcp.ClientSession, tool, name string) serverCall {
	params := &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"name": name}}
	params.SetProgressToken(tool + "-" + name)

	result, err := session.CallTool(ctx, params)
	if err != nil {
		return serverCall{err: err.Error()}
	}

	if result.IsError {
		var text strings.Builder

		for _, content := range result.Content {
			if textContent, ok := content.(*mcp.TextContent); ok {
				text.WriteString(textContent.Text)
			}
		}

		return serverCall{err: text.String()}
	}

	encoded, _ := json.Marshal(result.StructuredContent)

	var decoded gateway.EnableResult

	_ = json.Unmarshal(encoded, &decoded)

	return serverCall{tools: decoded.Tools}
}

// startEnable calls toolgate_enable_server in the background.
func startEnable(t *testing.T, session *gatewaySession, name string) <-chan serverCall {
	t.Helper()

	results := make(chan serverCall, 1)
	ctx, cancel := context.WithTimeout(t.Context(), enableTimeout+time.Minute)

	go func() {
		defer cancel()

		results <- callServerTool(ctx, session.session, gateway.ToolEnableServer, name)
	}()

	return results
}

func awaitEnable(t *testing.T, results <-chan serverCall) serverCall {
	t.Helper()

	select {
	case result := <-results:
		return result
	case <-time.After(enableTimeout + 2*time.Minute):
		require.FailNow(t, "toolgate_enable_server did not return")

		return serverCall{}
	}
}

func enable(t *testing.T, session *gatewaySession, name string) []string {
	t.Helper()

	result := awaitEnable(t, startEnable(t, session, name))
	require.Empty(t, result.err, "enable %s", name)

	return result.tools
}

func prefixedTools(server string) []string {
	var names []string
	for _, tool := range testServerTools {
		names = append(names, server+"__"+tool)
	}

	return names
}

func listedTools(t require.TestingT, ctx context.Context, session *mcp.ClientSession) []string {
	listed, err := session.ListTools(ctx, nil)
	require.NoError(t, err)

	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}

	return names
}

// instanceNamespaces returns the instance namespaces of a template that are
// not being deleted.
func instanceNamespaces(t require.TestingT, ctx context.Context, admin *kubernetes.Clientset, template string) []corev1.Namespace {
	namespaces, err := admin.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: runtime.LabelManagedBy + "=" + runtime.ManagedByValue + "," + runtime.LabelTemplate + "=" + template,
	})
	require.NoError(t, err)

	var live []corev1.Namespace

	for _, namespace := range namespaces.Items {
		if namespace.DeletionTimestamp == nil {
			live = append(live, namespace)
		}
	}

	return live
}

func requireOneInstance(t *testing.T, admin *kubernetes.Clientset, template string) corev1.Namespace {
	t.Helper()

	namespaces := instanceNamespaces(t, t.Context(), admin, template)
	require.Len(t, namespaces, 1, "one instance namespace for %s", template)

	return namespaces[0]
}

func TestAC1_07EnableAndDisable(t *testing.T) {
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)

	name := createTemplate(t, kube, "e2e-session", "per-session", testServerResources(t, image, 5*time.Second, nil))
	requireTemplateValid(t, kube, name)

	pods := gatewayPods(t, admin)
	session := connectGateway(t, forwardToGateway(t, admin, pods[0]), "alice", "e2e-"+rand.String(8))

	var namespace corev1.Namespace

	t.Run("[AC-1-07] a per-session enable gets progress, then list_changed, and tools named server__tool", func(t *testing.T) {
		tools := enable(t, session, name)
		assert.ElementsMatch(t, prefixedTools(name), tools)

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			assert.Contains(collect, session.recorded(), eventListChanged)
		}, time.Minute, 100*time.Millisecond)

		events := session.recorded()
		first := slices.Index(events, eventListChanged)
		assert.Contains(t, events[:first], eventProgress, "progress comes before list_changed: %v", events)

		listed := listedTools(t, t.Context(), session.session)
		assert.Subset(t, listed, prefixedTools(name))
	})

	t.Run("[AC-1-07] the instance namespace is Pod Security restricted with default-deny networking", func(t *testing.T) {
		namespace = requireOneInstance(t, admin, name)
		assert.Equal(t, "restricted", namespace.Labels["pod-security.kubernetes.io/enforce"])

		policy, err := admin.NetworkingV1().NetworkPolicies(namespace.Name).Get(t.Context(), "toolgate-default-deny", metav1.GetOptions{})
		require.NoError(t, err)
		assert.Empty(t, policy.Spec.PodSelector.MatchLabels, "the policy selects every pod")
		assert.ElementsMatch(t, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}, policy.Spec.PolicyTypes)
	})

	t.Run("[AC-1-11] disable removes the tools and deletes the unused per-session namespace", func(t *testing.T) {
		require.NotEmpty(t, namespace.Name, "the enable subtest found the namespace")

		result := callServerTool(t.Context(), session.session, gateway.ToolDisableServer, name)
		require.Empty(t, result.err)

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			listed := listedTools(collect, t.Context(), session.session)
			for _, tool := range prefixedTools(name) {
				assert.NotContains(collect, listed, tool)
			}
		}, time.Minute, time.Second)

		requireNamespaceGone(t, admin, namespace.Name, "the reconciler stops the unused per-session instance")
	})
}

func TestAC1_13PerUserScope(t *testing.T) {
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)

	name := createTemplate(t, kube, "e2e-user", "per-user", testServerResources(t, image, 0, nil))
	requireTemplateStartsOnEnable(t, kube, name)

	pods := gatewayPods(t, admin)

	t.Run("[AC-1-13] two sessions of one user enabling at the same moment share one instance", func(t *testing.T) {
		first := connectGateway(t, forwardToGateway(t, admin, pods[0]), "alice", "e2e-"+rand.String(8))
		second := connectGateway(t, forwardToGateway(t, admin, pods[1]), "alice", "e2e-"+rand.String(8))

		firstResult, secondResult := startEnable(t, first, name), startEnable(t, second, name)

		for _, result := range []serverCall{awaitEnable(t, firstResult), awaitEnable(t, secondResult)} {
			require.Empty(t, result.err)
			assert.ElementsMatch(t, prefixedTools(name), result.tools)
		}

		requireOneInstance(t, admin, name)
	})

	t.Run("[AC-1-13] another user gets a separate instance", func(t *testing.T) {
		bob := connectGateway(t, forwardToGateway(t, admin, pods[0]), "bob", "e2e-"+rand.String(8))
		assert.ElementsMatch(t, prefixedTools(name), enable(t, bob, name))

		namespaces := instanceNamespaces(t, t.Context(), admin, name)
		require.Len(t, namespaces, 2, "alice's and bob's instances")
		assert.NotEqual(t, namespaces[0].Labels[runtime.LabelOwner], namespaces[1].Labels[runtime.LabelOwner])
	})
}

func TestAC1_14SharedScope(t *testing.T) {
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)

	t.Run("[AC-1-14] all users share one instance of a shared server", func(t *testing.T) {
		name := createTemplate(t, kube, "e2e-shared", "shared", testServerResources(t, image, 0, nil))
		requireTemplateValid(t, kube, name)

		pods := gatewayPods(t, admin)
		alice := connectGateway(t, forwardToGateway(t, admin, pods[0]), "alice", "e2e-"+rand.String(8))
		bob := connectGateway(t, forwardToGateway(t, admin, pods[1]), "bob", "e2e-"+rand.String(8))

		assert.ElementsMatch(t, prefixedTools(name), enable(t, alice, name))
		assert.ElementsMatch(t, prefixedTools(name), enable(t, bob, name))

		requireOneInstance(t, admin, name)
	})

	t.Run("[AC-1-14] a shared template that uses a principal variable is invalid and not offered", func(t *testing.T) {
		resources := testServerResources(t, image, 0, map[string]string{"OWNER": "${principal.id}"})
		name := createTemplate(t, kube, "e2e-shared-invalid", "shared", resources)

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			var template v1alpha1.MCPServerTemplate
			require.NoError(collect, kube.Get(t.Context(), types.NamespacedName{Namespace: releaseNamespace(), Name: name}, &template))

			condition := meta.FindStatusCondition(template.Status.Conditions, catalog.ConditionValid)
			require.NotNil(collect, condition, "the leader reports validity")
			assert.Equal(collect, metav1.ConditionFalse, condition.Status)
			assert.Contains(collect, condition.Message, "principal.id")
		}, 2*time.Minute, time.Second)

		pods := gatewayPods(t, admin)
		alice := connectGateway(t, forwardToGateway(t, admin, pods[0]), "alice", "e2e-"+rand.String(8))

		result, err := alice.session.CallTool(t.Context(), &mcp.CallToolParams{Name: gateway.ToolListServers})
		require.NoError(t, err)
		require.False(t, result.IsError)

		encoded, err := json.Marshal(result.StructuredContent)
		require.NoError(t, err)

		var listed gateway.ListServersResult
		require.NoError(t, json.Unmarshal(encoded, &listed))

		for _, server := range listed.Servers {
			assert.NotEqual(t, name, server.Name, "the invalid template is not offered")
		}

		assert.NotEmpty(t, callServerTool(t.Context(), alice.session, gateway.ToolEnableServer, name).err)
	})
}

// The leader is killed while an instance starts: the new leader finishes
// the start from the rows and the labelled objects, with no duplicate
// instance and no orphaned objects.
func TestLeaderKilledDuringStart(t *testing.T) {
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)

	// The pod stays unready long enough for the leader to be killed while
	// the instance is starting.
	name := createTemplate(t, kube, "e2e-failover", "per-user", testServerResources(t, image, 45*time.Second, nil))
	requireTemplateValid(t, kube, name)

	leader := runningLeader(t, admin)
	pods := gatewayPods(t, admin)

	follower := pods[0]
	if follower == leader {
		follower = pods[1]
	}

	require.NotEqual(t, leader, follower)

	session := connectGateway(t, forwardToGateway(t, admin, follower), "alice", "e2e-"+rand.String(8))
	results := startEnable(t, session, name)

	var started string

	t.Run("[row 15] the leader is killed after it applied the instance's objects", func(t *testing.T) {
		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			namespaces := instanceNamespaces(collect, t.Context(), admin, name)
			require.Len(collect, namespaces, 1)

			started = namespaces[0].Name

			deployment, err := admin.AppsV1().Deployments(namespaces[0].Name).Get(t.Context(), "mcp", metav1.GetOptions{})
			require.NoError(collect, err, "the leader applied the template's objects")
			assert.Zero(collect, deployment.Status.ReadyReplicas, "the instance is still starting")
		}, 2*time.Minute, 500*time.Millisecond)

		require.NoError(t, admin.CoreV1().Pods(releaseNamespace()).Delete(t.Context(), leader,
			metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}))
	})

	t.Run("[row 15] the new leader finishes the start with no duplicate and no orphan", func(t *testing.T) {
		result := awaitEnable(t, results)
		require.Empty(t, result.err)
		assert.ElementsMatch(t, prefixedTools(name), result.tools)

		current, err := leaderPod(t.Context(), admin)
		require.NoError(t, err)
		assert.NotEqual(t, leader, current, "another replica holds the Lease")

		require.NotEmpty(t, started, "the first subtest saw the instance start")

		// The new leader continued the instance the old one started: no
		// second instance, and no namespace left over or being deleted.
		all, err := admin.CoreV1().Namespaces().List(t.Context(), metav1.ListOptions{
			LabelSelector: runtime.LabelManagedBy + "=" + runtime.ManagedByValue + "," + runtime.LabelTemplate + "=" + name,
		})
		require.NoError(t, err)
		require.Len(t, all.Items, 1, "one namespace for the template")
		assert.Equal(t, started, all.Items[0].Name)
		assert.Nil(t, all.Items[0].DeletionTimestamp)

		deployments, err := admin.AppsV1().Deployments(started).List(t.Context(), metav1.ListOptions{})
		require.NoError(t, err)
		require.Len(t, deployments.Items, 1, "the template's objects are applied once")

		assert.ElementsMatch(t, prefixedTools(name), listedToolsOf(t, session, name))
	})
}

// listedToolsOf returns the tools of tools/list that belong to server.
func listedToolsOf(t *testing.T, session *gatewaySession, server string) []string {
	t.Helper()

	var names []string

	for _, tool := range listedTools(t, t.Context(), session.session) {
		if strings.HasPrefix(tool, server+"__") {
			names = append(names, tool)
		}
	}

	return names
}
