package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

const (
	instanceID        = "0123456789abcdef0123456789abcdef"
	instanceNamespace = "tg-" + instanceID
	templateUID       = "template-uid"
)

// memoryStore is a StartStore whose compare-and-set writes behave like the
// SQL ones: a write whose from status doesn't match changes nothing.
type memoryStore struct {
	mu         sync.Mutex
	status     string
	generation int64
	reason     string
	tools      []byte
	sessions   map[int64]db.Session
	writes     int
	// resolvedExpiresAt is what MarkStarting recorded.
	resolvedExpiresAt time.Time
	// healthCheck is what MarkStarting recorded.
	healthCheck []byte
	// secretsRecorded says RecordSecretsUsed was called.
	secretsRecorded bool
	// unhealthy and unhealthyReason are what SetHealth recorded.
	unhealthy       bool
	unhealthyReason string
}

func (m *memoryStore) Session(_ context.Context, id int64) (db.Session, error) {
	session, found := m.sessions[id]
	if !found {
		return db.Session{}, errors.New("no session")
	}

	return session, nil
}

func (m *memoryStore) RecordSecretsUsed(context.Context, string, map[string]time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.secretsRecorded = true

	return nil
}

func (m *memoryStore) MarkStarting(_ context.Context, _ string, generation int64, healthCheck []byte, resolvedExpiresAt time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.status != "pending" {
		return false, nil
	}

	m.status, m.generation, m.resolvedExpiresAt, m.healthCheck = "starting", generation, resolvedExpiresAt, healthCheck
	m.writes++

	return true, nil
}

func (m *memoryStore) MarkReady(_ context.Context, _ string, tools []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.status != "starting" {
		return false, nil
	}

	m.status, m.tools = "ready", tools
	m.writes++

	return true, nil
}

func (m *memoryStore) MarkFailed(_ context.Context, _, from, reason string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.status != from {
		return false, nil
	}

	m.status, m.reason = "failed", reason
	m.writes++

	return true, nil
}

func (m *memoryStore) SetHealth(_ context.Context, _ db.Instance, healthy bool, reason string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.status != "ready" || m.unhealthy == !healthy {
		return false, nil
	}

	m.unhealthy, m.unhealthyReason = !healthy, reason
	m.writes++

	return true, nil
}

func (m *memoryStore) health() (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.unhealthy, m.unhealthyReason
}

func (m *memoryStore) snapshot() (string, string, []byte, int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.status, m.reason, m.tools, m.writes
}

func testTemplate(resources ...string) *v1alpha1.MCPServerTemplate {
	template := &v1alpha1.MCPServerTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "catalog", UID: templateUID, Generation: 3},
		Spec: v1alpha1.MCPServerTemplateSpec{
			Scope:        "per-user",
			StartTimeout: &metav1.Duration{Duration: 5 * time.Minute},
			Endpoint:     v1alpha1.Endpoint{Service: "server", Port: 8080, Path: "/mcp"},
		},
	}

	for _, resource := range resources {
		template.Spec.Resources = append(template.Spec.Resources, k8sruntime.RawExtension{Raw: []byte(resource)})
	}

	return template
}

const configMapResource = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings"},"data":{"email":"${principal.email}"}}`

type starterFixture struct {
	starter  *runtime.Starter
	store    *memoryStore
	kube     client.Client
	now      time.Time
	instance db.Instance
	listed   []string
}

func newStarterFixture(t *testing.T, template *v1alpha1.MCPServerTemplate, status string, objects ...client.Object) *starterFixture {
	t.Helper()

	scheme := k8sruntime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))

	fixture := &starterFixture{
		store: &memoryStore{status: status, generation: 1, sessions: map[int64]db.Session{
			7: {ID: 7, Principal: "alice", SessionID: "session-1", Workspace: "laptop", Attestation: "user", Email: "alice@example.com", Name: "Alice"},
		}},
		kube: clientfake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
			WithInterceptorFuncs(interceptor.Funcs{Apply: fakeApply}).WithReturnManagedFields().Build(),
		now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC),
	}

	fixture.instance = db.Instance{
		ID: instanceID, TemplateNs: template.Namespace, TemplateName: template.Name, TemplateUid: templateUID,
		Generation: 1, Scope: "per-user", ScopeKey: "user:alice", OwnerPrincipal: "alice",
		RenderSession: pgtype.Int8{Int64: 7, Valid: true}, Desired: "running", Status: status,
		Namespace:        instanceNamespace,
		AttemptStartedAt: pgtype.Timestamptz{Time: fixture.now.Add(-time.Minute), Valid: true},
	}

	fixture.starter = &runtime.Starter{
		Store:  fixture.store,
		Client: fixture.kube,
		Template: func(key types.NamespacedName) (*v1alpha1.MCPServerTemplate, bool) {
			if key != (types.NamespacedName{Namespace: template.Namespace, Name: template.Name}) {
				return nil, false
			}

			return template.DeepCopy(), true
		},
		Secrets: runtime.BrokerSecrets(func(context.Context, string, string) ([]byte, time.Time, error) {
			return nil, time.Time{}, errors.New("no secrets")
		}),
		ListTools: func(_ context.Context, endpoint string) ([]*mcp.Tool, error) {
			fixture.listed = append(fixture.listed, endpoint)

			return []*mcp.Tool{{Name: "search", InputSchema: map[string]any{"type": "object"}}}, nil
		},
		GatewayNamespace: "toolgate",
		GatewayPodLabels: map[string]string{"app.kubernetes.io/name": "toolgate"},
		Now:              func() time.Time { return fixture.now },
	}

	return fixture
}

// fakeApply refuses server-side apply of a Namespace, as the chart's RBAC
// does (it grants namespaces no patch), and of a Role, as RBAC does when the
// admin grants no extra rules. It stands in for server-side apply
// of a NetworkPolicy, which the fake client can't merge; other kinds are
// applied as usual.
func fakeApply(ctx context.Context, kube client.WithWatch, object k8sruntime.ApplyConfiguration, options ...client.ApplyOption) error {
	content, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		return err
	}

	if content["kind"] == "Namespace" {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "", errors.New("cannot patch resource \"namespaces\""))
	}

	if content["kind"] == "Role" {
		return apierrors.NewForbidden(schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "roles"}, "reader",
			errors.New(`User "system:serviceaccount:toolgate:toolgate" cannot patch resource "roles"`))
	}

	if content["kind"] != "NetworkPolicy" {
		return kube.Apply(ctx, object, options...)
	}

	policy := &networkingv1.NetworkPolicy{}
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(content, policy); err != nil {
		return err
	}

	return kube.Create(ctx, policy)
}

func instanceLabels() map[string]string {
	return map[string]string{runtime.LabelInstance: instanceID, runtime.LabelManagedBy: runtime.ManagedByValue}
}

func readyPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "server", Namespace: instanceNamespace, Labels: instanceLabels()},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

func TestStarterPendingPreparesNamespaceAndApplies(t *testing.T) {
	fixture := newStarterFixture(t, testTemplate(configMapResource), "pending")

	require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

	status, _, _, _ := fixture.store.snapshot()
	assert.Equal(t, "starting", status)
	assert.True(t, fixture.store.secretsRecorded, "the render's secrets are stored before the objects are applied")
	assert.Equal(t, int64(3), fixture.store.generation, "the write records the generation it rendered")

	namespace := &corev1.Namespace{}
	require.NoError(t, fixture.kube.Get(t.Context(), client.ObjectKey{Name: instanceNamespace}, namespace))
	assert.Equal(t, "toolgate", namespace.Labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, instanceID, namespace.Labels["toolgate.soliddowant.dev/instance"])
	assert.Equal(t, "true", namespace.Labels["toolgate.soliddowant.dev/writable"])
	assert.Equal(t, "restricted", namespace.Labels["pod-security.kubernetes.io/enforce"])

	serviceAccount := &corev1.ServiceAccount{}
	require.NoError(t, fixture.kube.Get(t.Context(), client.ObjectKey{Namespace: instanceNamespace, Name: "default"}, serviceAccount))
	require.NotNil(t, serviceAccount.AutomountServiceAccountToken)
	assert.False(t, *serviceAccount.AutomountServiceAccountToken)

	policies := &networkingv1.NetworkPolicyList{}
	require.NoError(t, fixture.kube.List(t.Context(), policies, client.InNamespace(instanceNamespace)))
	require.Len(t, policies.Items, 1)

	policy := policies.Items[0].Spec
	assert.Empty(t, policy.PodSelector.MatchLabels, "the policy selects every pod")
	assert.ElementsMatch(t, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}, policy.PolicyTypes)
	require.Len(t, policy.Ingress, 1)
	require.Len(t, policy.Ingress[0].From, 1)
	assert.Equal(t, map[string]string{"kubernetes.io/metadata.name": "toolgate"}, policy.Ingress[0].From[0].NamespaceSelector.MatchLabels)
	assert.Equal(t, map[string]string{"app.kubernetes.io/name": "toolgate"}, policy.Ingress[0].From[0].PodSelector.MatchLabels)
	require.Len(t, policy.Egress, 1)
	require.Len(t, policy.Egress[0].Ports, 2)

	for _, port := range policy.Egress[0].Ports {
		assert.Equal(t, int32(53), port.Port.IntVal, "egress is DNS only")
	}

	configMap := &corev1.ConfigMap{}
	require.NoError(t, fixture.kube.Get(t.Context(), client.ObjectKey{Namespace: instanceNamespace, Name: "settings"}, configMap))
	assert.Equal(t, "alice@example.com", configMap.Data["email"], "rendered with the render_session snapshot")
	assert.Equal(t, instanceID, configMap.Labels[runtime.LabelInstance])

	managers := map[string]bool{}
	for _, field := range configMap.ManagedFields {
		managers[field.Manager] = field.Operation == metav1.ManagedFieldsOperationApply
	}

	assert.Equal(t, map[string]bool{"toolgate": true}, managers, "applied server-side by field manager toolgate")
}

func TestStarterPendingReusesLabelledNamespace(t *testing.T) {
	existing := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instanceNamespace, Labels: instanceLabels()}}
	fixture := newStarterFixture(t, testTemplate(configMapResource), "pending", existing)

	require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

	status, _, _, _ := fixture.store.snapshot()
	assert.Equal(t, "starting", status, "a namespace from an earlier attempt is reused")

	configMap := &corev1.ConfigMap{}
	require.NoError(t, fixture.kube.Get(t.Context(), client.ObjectKey{Namespace: instanceNamespace, Name: "settings"}, configMap))
}

func TestStarterPendingRefusesUnlabelledNamespace(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
	}{
		{name: "no labels"},
		{name: "another instance", labels: map[string]string{runtime.LabelInstance: "other", runtime.LabelManagedBy: runtime.ManagedByValue}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			existing := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instanceNamespace, Labels: test.labels}}
			fixture := newStarterFixture(t, testTemplate(configMapResource), "pending", existing)

			require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

			status, reason, _, _ := fixture.store.snapshot()
			assert.Equal(t, "failed", status)
			assert.Contains(t, reason, instanceNamespace)

			namespace := &corev1.Namespace{}
			require.NoError(t, fixture.kube.Get(t.Context(), client.ObjectKey{Name: instanceNamespace}, namespace))
			assert.Equal(t, test.labels, namespace.Labels, "the namespace is not adopted")

			configMaps := &corev1.ConfigMapList{}
			require.NoError(t, fixture.kube.List(t.Context(), configMaps, client.InNamespace(instanceNamespace)))
			assert.Empty(t, configMaps.Items, "nothing is applied into a namespace toolgate doesn't own")
		})
	}
}

func TestStarterPendingApplyRefusedByRBACFails(t *testing.T) {
	role := `{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"Role","metadata":{"name":"reader"}}`
	fixture := newStarterFixture(t, testTemplate(configMapResource, role), "pending")

	require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

	status, reason, _, _ := fixture.store.snapshot()
	assert.Equal(t, "failed", status)
	assert.Contains(t, reason, `roles.rbac.authorization.k8s.io "reader" is forbidden: User "system:serviceaccount:toolgate:toolgate" cannot patch resource "roles"`,
		"the reason is the API server's message")
}

func TestStarterPendingRenderErrorFails(t *testing.T) {
	fixture := newStarterFixture(t, testTemplate(configMapResource), "pending")
	session := fixture.store.sessions[7]
	session.Email = ""
	fixture.store.sessions[7] = session

	require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

	status, reason, _, _ := fixture.store.snapshot()
	assert.Equal(t, "failed", status)
	assert.Contains(t, reason, "${principal.email}")

	configMaps := &corev1.ConfigMapList{}
	require.NoError(t, fixture.kube.List(t.Context(), configMaps, client.InNamespace(instanceNamespace)))
	assert.Empty(t, configMaps.Items, "nothing is applied after a render error")
}

func TestStarterStartingBecomesReady(t *testing.T) {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "server", Namespace: instanceNamespace, Labels: instanceLabels()},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP},
	}
	fixture := newStarterFixture(t, testTemplate(), "starting", readyPod(), service)

	require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

	status, _, tools, _ := fixture.store.snapshot()
	assert.Equal(t, "ready", status)
	assert.Equal(t, []string{"http://server." + instanceNamespace + ".svc:8080/mcp"}, fixture.listed)

	var stored []map[string]any
	require.NoError(t, json.Unmarshal(tools, &stored))
	require.Len(t, stored, 1)
	assert.Equal(t, "search", stored[0]["name"])
}

func TestStarterStartingWaitsForObjects(t *testing.T) {
	pod := readyPod()
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	fixture := newStarterFixture(t, testTemplate(), "starting", pod)

	require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

	status, _, _, writes := fixture.store.snapshot()
	assert.Equal(t, "starting", status)
	assert.Zero(t, writes)
	assert.Empty(t, fixture.listed, "MCP isn't tried before every object is current")
}

func TestStarterStartingKstatusFailure(t *testing.T) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "server", Namespace: instanceNamespace, Labels: instanceLabels(), Generation: 1},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			Conditions: []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
				Reason: "ProgressDeadlineExceeded", Message: "deadline exceeded",
			}},
		},
	}
	fixture := newStarterFixture(t, testTemplate(), "starting", readyPod(), deployment)

	require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

	status, reason, _, _ := fixture.store.snapshot()
	assert.Equal(t, "failed", status)
	assert.Contains(t, reason, "Deployment server failed")
	assert.Empty(t, fixture.listed)
}

func TestStarterStartingToolListCheckFails(t *testing.T) {
	fixture := newStarterFixture(t, testTemplate(), "starting", readyPod())
	fixture.starter.ListTools = func(context.Context, string) ([]*mcp.Tool, error) {
		return []*mcp.Tool{{Name: strings.Repeat("x", 57)}}, nil
	}

	require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

	status, reason, _, _ := fixture.store.snapshot()
	assert.Equal(t, "failed", status)
	assert.Equal(t, "Tool `"+strings.Repeat("x", 57)+"` is longer than 64 characters with its `github__` prefix.", reason)
}

func TestStarterStartingTimeout(t *testing.T) {
	waitingPod := func() *corev1.Pod {
		pod := readyPod()
		pod.Status.Conditions[0].Status = corev1.ConditionFalse
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "server",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "ImagePullBackOff", Message: `Back-off pulling image "example.invalid/missing:1"`,
			}},
		}}

		return pod
	}

	t.Run("[AC-1-18] an image that can't be pulled fails at startTimeout with the image problem", func(t *testing.T) {
		fixture := newStarterFixture(t, testTemplate(), "starting", waitingPod())

		fixture.now = fixture.instance.AttemptStartedAt.Time.Add(5*time.Minute - time.Nanosecond)
		require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

		status, _, _, _ := fixture.store.snapshot()
		assert.Equal(t, "starting", status, "not failed before startTimeout")

		fixture.now = fixture.instance.AttemptStartedAt.Time.Add(5 * time.Minute)
		require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

		status, reason, _, _ := fixture.store.snapshot()
		assert.Equal(t, "failed", status)
		assert.Contains(t, reason, "ImagePullBackOff")
		assert.Contains(t, reason, "example.invalid/missing:1")
	})

	t.Run("falls back to the latest Warning event", func(t *testing.T) {
		pod := readyPod()
		pod.Status.Conditions[0].Status = corev1.ConditionFalse
		older := &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "older", Namespace: instanceNamespace},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "server"},
			Type:           corev1.EventTypeWarning, Reason: "Old", Message: "old problem",
			LastTimestamp: metav1.NewTime(time.Date(2026, 11, 5, 11, 0, 0, 0, time.UTC)),
		}
		newer := &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "newer", Namespace: instanceNamespace},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "server"},
			Type:           corev1.EventTypeWarning, Reason: "Failed", Message: "Failed to pull image",
			LastTimestamp: metav1.NewTime(time.Date(2026, 11, 5, 11, 30, 0, 0, time.UTC)),
		}
		normal := &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "normal", Namespace: instanceNamespace},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "server"},
			Type:           corev1.EventTypeNormal, Reason: "Pulling", Message: "Pulling image",
			LastTimestamp: metav1.NewTime(time.Date(2026, 11, 5, 11, 50, 0, 0, time.UTC)),
		}
		fixture := newStarterFixture(t, testTemplate(), "starting", pod, older, newer, normal)
		fixture.now = fixture.instance.AttemptStartedAt.Time.Add(10 * time.Minute)

		require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

		status, reason, _, _ := fixture.store.snapshot()
		assert.Equal(t, "failed", status)
		assert.Contains(t, reason, "Failed to pull image")
		assert.NotContains(t, reason, "old problem")
		assert.NotContains(t, reason, "Pulling image")
	})
}

// A readiness check that finishes after the attempt timed out changes
// nothing: its compare-and-set on the old row fails.
func TestStarterLateResultAfterTimeout(t *testing.T) {
	fixture := newStarterFixture(t, testTemplate(), "starting", readyPod())

	timeoutPass := newStarterFixture(t, testTemplate(), "starting", readyPod())
	timeoutPass.store = fixture.store
	timeoutPass.starter.Store = fixture.store
	timeoutPass.now = fixture.instance.AttemptStartedAt.Time.Add(time.Hour)

	fixture.starter.ListTools = func(ctx context.Context, _ string) ([]*mcp.Tool, error) {
		// The timeout commits while this readiness check still runs.
		require.NoError(t, timeoutPass.starter.Step(ctx, timeoutPass.instance))

		return []*mcp.Tool{{Name: "search"}}, nil
	}

	require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

	status, reason, tools, writes := fixture.store.snapshot()
	assert.Equal(t, "failed", status)
	assert.Contains(t, reason, "didn't become ready within 5m0s")
	assert.Nil(t, tools)
	assert.Equal(t, 1, writes, "only the timeout wrote")
}

// TestAC1_20LongToolNames: a server exposing a tool whose prefixed name is
// over 64 characters is marked failed with a reason saying so; at exactly
// 64 it becomes ready.
func TestAC1_20LongToolNames(t *testing.T) {
	tests := []struct {
		name   string
		length int
		status string
		reason string
	}{
		{name: "github__ plus 56 characters is 64 and ready", length: 56, status: "ready"},
		{
			name: "github__ plus 57 characters is 65 and failed", length: 57, status: "failed",
			reason: "Tool `" + strings.Repeat("x", 57) + "` is longer than 64 characters with its `github__` prefix.",
		},
	}

	for _, test := range tests {
		t.Run("[AC-1-20] "+test.name, func(t *testing.T) {
			fixture := newStarterFixture(t, testTemplate(), "starting", readyPod())
			fixture.starter.ListTools = func(context.Context, string) ([]*mcp.Tool, error) {
				return []*mcp.Tool{{Name: strings.Repeat("x", test.length)}}, nil
			}

			require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

			status, reason, _, _ := fixture.store.snapshot()
			assert.Equal(t, test.status, status)
			assert.Equal(t, test.reason, reason)
		})
	}
}

func TestCheckToolList(t *testing.T) {
	tools := func(count int, name func(int) string) []*mcp.Tool {
		list := make([]*mcp.Tool, count)
		for index := range list {
			list[index] = &mcp.Tool{Name: name(index)}
		}

		return list
	}
	numbered := func(index int) string { return "tool" + strconv.Itoa(index) }

	tests := []struct {
		name   string
		tools  []*mcp.Tool
		reason string
	}{
		{name: "name at 64 characters with prefix", tools: tools(1, func(int) string { return strings.Repeat("x", 56) })},
		{
			name: "name at 65 characters with prefix", tools: tools(1, func(int) string { return strings.Repeat("x", 57) }),
			reason: "Tool `" + strings.Repeat("x", 57) + "` is longer than 64 characters with its `github__` prefix.",
		},
		{name: "512 tools", tools: tools(512, numbered)},
		{name: "513 tools", tools: tools(513, numbered), reason: "The server lists 513 tools, more than the limit of 512."},
		{name: "no tools", tools: nil},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, reason := runtime.CheckToolList("github", test.tools)
			assert.Equal(t, test.reason, reason)

			if test.reason == "" {
				assert.NotNil(t, encoded)
			}
		})
	}
}

func TestCheckToolListSizeLimit(t *testing.T) {
	sized := func(total int) []*mcp.Tool {
		// One tool whose description grows the stored list to total bytes.
		base, _ := runtime.CheckToolList("github", []*mcp.Tool{{Name: "a", Description: "d"}})

		return []*mcp.Tool{{Name: "a", Description: strings.Repeat("d", total-len(base)+1)}}
	}

	encoded, reason := runtime.CheckToolList("github", sized(1<<20))
	require.Empty(t, reason)
	require.Len(t, encoded, 1<<20)

	_, reason = runtime.CheckToolList("github", sized(1<<20+1))
	assert.Equal(t, "The server's tool list is 1048577 bytes, more than the limit of 1 MiB.", reason)
}

func TestMCPToolListerInitializesAndListsTools(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "search", Description: "Search."},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})

	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(httpServer.Close)

	tools, err := runtime.MCPToolLister(t.Context(), httpServer.URL)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	assert.Equal(t, "search", tools[0].Name)

	_, err = runtime.MCPToolLister(t.Context(), httpServer.URL+"/missing\x00")
	require.Error(t, err)
}

const sharedNamespace = "toolgate-shared"

func sharedNamespaceTemplate() *v1alpha1.MCPServerTemplate {
	template := testTemplate(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings-${instance.id}"},"data":{"where":"${instance.namespace}"}}`)
	template.Spec.Scope = "shared"
	template.Spec.Namespace = "shared"
	template.Spec.Endpoint.Service = "server-${instance.id}"

	return template
}

func sharedInstance(instance db.Instance) db.Instance {
	instance.Scope, instance.ScopeKey, instance.OwnerPrincipal = "shared", "shared", ""
	instance.Namespace = sharedNamespace

	return instance
}

func TestStarterPendingAppliesSharedNamespaceInstanceWithoutNamespace(t *testing.T) {
	existing := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: sharedNamespace, Labels: map[string]string{runtime.LabelManagedBy: runtime.ManagedByValue},
	}}
	fixture := newStarterFixture(t, sharedNamespaceTemplate(), "pending", existing)

	require.NoError(t, fixture.starter.Step(t.Context(), sharedInstance(fixture.instance)))

	status, reason, _, _ := fixture.store.snapshot()
	require.Equal(t, "starting", status, reason)

	configMap := &corev1.ConfigMap{}
	require.NoError(t, fixture.kube.Get(t.Context(), client.ObjectKey{Namespace: sharedNamespace, Name: "settings-" + instanceID}, configMap))
	assert.Equal(t, sharedNamespace, configMap.Data["where"])
	assert.Equal(t, instanceID, configMap.Labels[runtime.LabelInstance])

	namespaces := &corev1.NamespaceList{}
	require.NoError(t, fixture.kube.List(t.Context(), namespaces))
	require.Len(t, namespaces.Items, 1, "no tg-<id> namespace is created")

	namespace := namespaces.Items[0]
	assert.Equal(t, sharedNamespace, namespace.Name)
	assert.Equal(t, existing.Labels, namespace.Labels, "the shared namespace is not labelled")

	policies := &networkingv1.NetworkPolicyList{}
	require.NoError(t, fixture.kube.List(t.Context(), policies))
	assert.Empty(t, policies.Items, "the per-instance default-deny policy is not applied in the shared namespace")
}

func TestStarterOwnsSharedNamespaceObjectsByAnchor(t *testing.T) {
	existing := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: sharedNamespace, Labels: map[string]string{runtime.LabelManagedBy: runtime.ManagedByValue},
	}}
	template := sharedNamespaceTemplate()
	template.Spec.Resources = append(template.Spec.Resources, k8sruntime.RawExtension{
		Raw: []byte(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"creds-${instance.id}"},"stringData":{"token":"x"}}`),
	})
	fixture := newStarterFixture(t, template, "pending", existing)

	require.NoError(t, fixture.starter.Step(t.Context(), sharedInstance(fixture.instance)))

	status, reason, _, _ := fixture.store.snapshot()
	require.Equal(t, "starting", status, reason)

	anchor := &corev1.ConfigMap{}
	require.NoError(t, fixture.kube.Get(t.Context(), client.ObjectKey{Namespace: sharedNamespace, Name: "tg-" + instanceID}, anchor))
	assert.Equal(t, map[string]string{
		runtime.LabelManagedBy: runtime.ManagedByValue,
		runtime.LabelInstance:  instanceID,
		runtime.LabelTemplate:  template.Name,
	}, anchor.Labels)

	want := []metav1.OwnerReference{{
		APIVersion: "v1", Kind: "ConfigMap", Name: anchor.Name, UID: anchor.UID, BlockOwnerDeletion: ptr.To(false),
	}}

	configMap := &corev1.ConfigMap{}
	require.NoError(t, fixture.kube.Get(t.Context(), client.ObjectKey{Namespace: sharedNamespace, Name: "settings-" + instanceID}, configMap))
	assert.Equal(t, want, configMap.OwnerReferences, "the ConfigMap is owned by the anchor")

	secret := &corev1.Secret{}
	require.NoError(t, fixture.kube.Get(t.Context(), client.ObjectKey{Namespace: sharedNamespace, Name: "creds-" + instanceID}, secret))
	assert.Equal(t, want, secret.OwnerReferences, "the Secret is owned by the anchor, so deleting the anchor deletes it")
}

func TestStarterReusesOwnAnchorConfigMap(t *testing.T) {
	existing := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: sharedNamespace, Labels: map[string]string{runtime.LabelManagedBy: runtime.ManagedByValue},
	}}
	own := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: sharedNamespace, Name: "tg-" + instanceID, UID: "anchor-uid",
		Labels: map[string]string{runtime.LabelInstance: instanceID},
	}}
	fixture := newStarterFixture(t, sharedNamespaceTemplate(), "pending", existing, own)

	require.NoError(t, fixture.starter.Step(t.Context(), sharedInstance(fixture.instance)))

	status, reason, _, _ := fixture.store.snapshot()
	require.Equal(t, "starting", status, reason)

	configMap := &corev1.ConfigMap{}
	require.NoError(t, fixture.kube.Get(t.Context(), client.ObjectKey{Namespace: sharedNamespace, Name: "settings-" + instanceID}, configMap))
	assert.Equal(t, []metav1.OwnerReference{{
		APIVersion: "v1", Kind: "ConfigMap", Name: "tg-" + instanceID, UID: "anchor-uid", BlockOwnerDeletion: ptr.To(false),
	}}, configMap.OwnerReferences)
}

func TestStarterRefusesForeignAnchorConfigMap(t *testing.T) {
	existing := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: sharedNamespace, Labels: map[string]string{runtime.LabelManagedBy: runtime.ManagedByValue},
	}}
	foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: sharedNamespace, Name: "tg-" + instanceID}}
	fixture := newStarterFixture(t, sharedNamespaceTemplate(), "pending", existing, foreign)

	require.NoError(t, fixture.starter.Step(t.Context(), sharedInstance(fixture.instance)))

	status, reason, _, _ := fixture.store.snapshot()
	assert.Equal(t, "failed", status)
	assert.Contains(t, reason, "tg-"+instanceID)

	configMap := &corev1.ConfigMap{}
	err := fixture.kube.Get(t.Context(), client.ObjectKey{Namespace: sharedNamespace, Name: "settings-" + instanceID}, configMap)
	assert.True(t, apierrors.IsNotFound(err), "nothing is applied: %v", err)
}

func TestStarterReachesSharedNamespaceInstanceAtRenderedService(t *testing.T) {
	pod := readyPod()
	pod.Namespace = sharedNamespace
	fixture := newStarterFixture(t, sharedNamespaceTemplate(), "starting", pod)

	require.NoError(t, fixture.starter.Step(t.Context(), sharedInstance(fixture.instance)))

	status, reason, _, _ := fixture.store.snapshot()
	assert.Equal(t, "ready", status, reason)
	assert.Equal(t, []string{"http://server-" + instanceID + "." + sharedNamespace + ".svc:8080/mcp"}, fixture.listed)
}

const helmReleaseResource = `{"apiVersion":"helm.toolkit.fluxcd.io/v2","kind":"HelmRelease","metadata":{"name":"server"},"spec":{"interval":"10m"}}`

// helmRelease returns a HelmRelease of the instance at generation 2 with the
// given status.
func helmRelease(observedGeneration int64, conditions ...map[string]any) *unstructured.Unstructured {
	release := &unstructured.Unstructured{}
	release.SetAPIVersion("helm.toolkit.fluxcd.io/v2")
	release.SetKind("HelmRelease")
	release.SetName("server")
	release.SetNamespace(instanceNamespace)
	release.SetLabels(instanceLabels())
	release.SetGeneration(2)

	items := make([]any, 0, len(conditions))
	for _, condition := range conditions {
		items = append(items, condition)
	}

	release.Object["status"] = map[string]any{"observedGeneration": observedGeneration, "conditions": items}

	return release
}

// condition returns a condition that changed at 12:00 UTC, the fixture's
// clock, minus age.
func condition(kind, status, reason, message string, age time.Duration) map[string]any {
	return map[string]any{
		"type": kind, "status": status, "reason": reason, "message": message,
		"lastTransitionTime": time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC).Add(-age).Format(time.RFC3339),
	}
}

// atGeneration sets the observedGeneration of a condition.
func atGeneration(condition map[string]any, generation int64) map[string]any {
	condition["observedGeneration"] = generation

	return condition
}

func TestStarterReadyConditionRule(t *testing.T) {
	const message = "Helm install failed for release tg/server: the value image is required"

	tests := []struct {
		name       string
		release    *unstructured.Unstructured
		wantStatus string
		wantReason string
	}{
		{
			name:       "Ready False for exactly 60 s at the current generation fails with the Ready message",
			release:    helmRelease(-1, atGeneration(condition("Ready", "False", "InstallFailed", message, 60*time.Second), 2)),
			wantStatus: "failed",
			wantReason: "HelmRelease server failed: " + message,
		},
		{
			name: "Ready False for 60 s fails whatever Reconciling says",
			release: helmRelease(-1,
				condition("Reconciling", "True", "Progressing", "installing", time.Minute),
				atGeneration(condition("Ready", "False", "InstallFailed", message, 90*time.Second), 2)),
			wantStatus: "failed",
			wantReason: "HelmRelease server failed: " + message,
		},
		{
			name:       "Ready False for 59 s waits",
			release:    helmRelease(-1, atGeneration(condition("Ready", "False", "InstallFailed", message, 59*time.Second), 2)),
			wantStatus: "starting",
		},
		{
			name:       "Ready False for 60 s at an older condition generation waits",
			release:    helmRelease(2, atGeneration(condition("Ready", "False", "InstallFailed", message, 5*time.Minute), 1)),
			wantStatus: "starting",
		},
		{
			name:       "Ready False for 60 s without a condition generation waits",
			release:    helmRelease(2, condition("Ready", "False", "InstallFailed", message, 5*time.Minute)),
			wantStatus: "starting",
		},
		{
			name:       "Ready True becomes ready",
			release:    helmRelease(2, atGeneration(condition("Ready", "True", "InstallSucceeded", "installed", time.Hour), 2)),
			wantStatus: "ready",
		},
		{
			name: "Stalled fails by kstatus",
			release: helmRelease(2, condition("Stalled", "True", "RetriesExceeded", "out of retries", 0),
				condition("Ready", "Unknown", "Progressing", "retrying", 0)),
			wantStatus: "failed",
			wantReason: "HelmRelease server failed: out of retries",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newStarterFixture(t, testTemplate(helmReleaseResource), "starting", readyPod(), test.release)

			require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

			status, reason, _, _ := fixture.store.snapshot()
			assert.Equal(t, test.wantStatus, status)
			assert.Equal(t, test.wantReason, reason)
		})
	}
}

func TestStarterReadyObjectHealth(t *testing.T) {
	const message = "failed to get digest for tag 9.9.9: not found"

	customCheck := []byte(`{"http":{"service":"mcp","port":8080,"path":"/healthz"}}`)

	// notReadyPod is a running pod whose Ready condition has been False for
	// an hour; a pod's conditions carry no observedGeneration.
	notReadyPod := readyPod()
	notReadyPod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady",
		LastTransitionTime: metav1.NewTime(time.Date(2026, 11, 5, 11, 0, 0, 0, time.UTC)),
	}}

	tests := []struct {
		name        string
		pod         *corev1.Pod
		release     *unstructured.Unstructured
		unhealthy   bool
		healthCheck []byte
		// wantUnhealthy and wantReason are the health after the step.
		wantUnhealthy bool
		wantReason    string
		objectReason  string
	}{
		{
			name:          "Ready False for exactly 60 s at the condition's generation marks it unhealthy with the object's message",
			pod:           readyPod(),
			release:       helmRelease(2, atGeneration(condition("Ready", "False", "UpgradeFailed", message, 60*time.Second), 2)),
			wantUnhealthy: true,
			wantReason:    "HelmRelease server failed: " + message,
		},
		{
			name:    "Ready False for 59 s leaves it healthy",
			pod:     readyPod(),
			release: helmRelease(2, atGeneration(condition("Ready", "False", "UpgradeFailed", message, 59*time.Second), 2)),
		},
		{
			name:    "Ready False at an older condition generation leaves it healthy",
			pod:     readyPod(),
			release: helmRelease(2, atGeneration(condition("Ready", "False", "UpgradeFailed", message, time.Hour), 1)),
		},
		{
			name:    "a pod without observedGeneration is judged by kstatus alone: not ready for an hour is only in progress",
			pod:     notReadyPod,
			release: helmRelease(2, atGeneration(condition("Ready", "True", "UpgradeSucceeded", "upgraded", time.Hour), 2)),
		},
		{
			name: "a Stalled object marks it unhealthy by kstatus",
			pod:  readyPod(),
			release: helmRelease(2, condition("Stalled", "True", "RetriesExceeded", "out of retries", 0),
				condition("Ready", "Unknown", "Progressing", "retrying", 0)),
			wantUnhealthy: true,
			wantReason:    "HelmRelease server failed: out of retries",
		},
		{
			name:      "ready again: an unhealthy instance without a custom check becomes healthy",
			pod:       readyPod(),
			release:   helmRelease(2, atGeneration(condition("Ready", "True", "UpgradeSucceeded", "upgraded", time.Second), 2)),
			unhealthy: true,
		},
		{
			name:          "ready again: an unhealthy instance with a custom check is left to its checker",
			pod:           readyPod(),
			release:       helmRelease(2, atGeneration(condition("Ready", "True", "UpgradeSucceeded", "upgraded", time.Second), 2)),
			unhealthy:     true,
			healthCheck:   customCheck,
			wantUnhealthy: true,
			wantReason:    "GET http://mcp:8080/healthz answered 503, expected 200",
		},
		{
			name:         "ready again: an instance a custom check did not mark becomes healthy",
			pod:          readyPod(),
			release:      helmRelease(2, atGeneration(condition("Ready", "True", "UpgradeSucceeded", "upgraded", time.Second), 2)),
			unhealthy:    true,
			objectReason: "HelmRelease server failed: out of retries",
			healthCheck:  customCheck,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newStarterFixture(t, testTemplate(helmReleaseResource), "ready", test.pod, test.release)
			fixture.instance.HealthCheck = test.healthCheck

			if test.unhealthy {
				fixture.store.unhealthy = true
				fixture.store.unhealthyReason = "GET http://mcp:8080/healthz answered 503, expected 200"

				if test.objectReason != "" {
					fixture.store.unhealthyReason = test.objectReason
				}

				fixture.instance.UnhealthySince = pgtype.Timestamptz{Time: fixture.now.Add(-time.Minute), Valid: true}
				fixture.instance.UnhealthyReason = fixture.store.unhealthyReason
			}

			require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

			status, _, _, _ := fixture.store.snapshot()
			assert.Equal(t, "ready", status, "the instance is not failed")

			unhealthy, reason := fixture.store.health()
			assert.Equal(t, test.wantUnhealthy, unhealthy)
			assert.Equal(t, test.wantReason, reason)
		})
	}
}

func TestStarterTimeoutReasonIsTheNotReadyObjectsMessage(t *testing.T) {
	release := helmRelease(-1, atGeneration(condition("Ready", "False", "InstallFailed", "chart not found", 10*time.Second), 2))
	fixture := newStarterFixture(t, testTemplate(helmReleaseResource), "starting", readyPod(), release)
	fixture.instance.AttemptStartedAt = pgtype.Timestamptz{Time: fixture.now.Add(-10 * time.Minute), Valid: true}

	require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

	status, reason, _, _ := fixture.store.snapshot()
	assert.Equal(t, "failed", status)
	assert.Equal(t, "The server didn't become ready within 5m0s: HelmRelease server: chart not found", reason)
}

func healthCheckTemplate(service, onFailure string) *v1alpha1.MCPServerTemplate {
	template := testTemplate(configMapResource)
	template.Spec.HealthCheck = &v1alpha1.HealthCheck{
		HTTP:      v1alpha1.HealthCheckHTTP{Service: service, Port: 8081, Path: "/healthz-${instance.id}", ExpectedStatus: 204},
		Interval:  metav1.Duration{Duration: time.Minute},
		Timeout:   metav1.Duration{Duration: 2 * time.Second},
		OnFailure: onFailure, FailureThreshold: 2, SuccessThreshold: 1, MaxRepairs: 4,
	}

	return template
}

func decodeCheck(t *testing.T, raw []byte) v1alpha1.HealthCheck {
	t.Helper()
	require.NotNil(t, raw)

	var check v1alpha1.HealthCheck
	require.NoError(t, json.Unmarshal(raw, &check))

	return check
}

func TestStarterWritesRenderedHealthCheckWithStarting(t *testing.T) {
	template := healthCheckTemplate("db-${instance.id}", "reapply")
	fixture := newStarterFixture(t, template, "pending")

	require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

	check := decodeCheck(t, fixture.store.healthCheck)
	assert.Equal(t, "db-"+instanceID, check.HTTP.Service, "http.service is rendered")
	assert.Equal(t, "/healthz-${instance.id}", check.HTTP.Path, "the path is not rendered")
	assert.Equal(t, int32(8081), check.HTTP.Port)
	assert.Equal(t, int32(204), check.HTTP.ExpectedStatus)
	assert.Equal(t, "reapply", check.OnFailure)
	assert.Equal(t, time.Minute, check.Interval.Duration)
	assert.Equal(t, int32(4), check.MaxRepairs)
}

func TestStarterWithoutHealthCheckWritesNull(t *testing.T) {
	fixture := newStarterFixture(t, testTemplate(configMapResource), "pending")

	require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

	status, _, _, _ := fixture.store.snapshot()
	assert.Equal(t, "starting", status)
	assert.Nil(t, fixture.store.healthCheck)
}

func TestStarterKeepsHealthCheckItStartedWith(t *testing.T) {
	template := healthCheckTemplate("db-${instance.id}", "replace")
	fixture := newStarterFixture(t, template, "pending", readyPod())

	require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

	started := string(fixture.store.healthCheck)

	// The admin changes the check and onFailure, then removes the check. The
	// running instance's column is not written again.
	template.Spec.HealthCheck.OnFailure = "none"
	template.Spec.HealthCheck.HTTP.Path = "/other"
	template.Generation = 4

	starting := fixture.instance
	starting.Status = "starting"
	require.NoError(t, fixture.starter.Step(t.Context(), starting))
	assert.Equal(t, started, string(fixture.store.healthCheck))

	template.Spec.HealthCheck = nil

	require.NoError(t, fixture.starter.Step(t.Context(), starting))
	assert.Equal(t, started, string(fixture.store.healthCheck))

	// The next instance gets the template's current (here: no) check.
	next := newStarterFixture(t, template, "pending")
	require.NoError(t, next.starter.Step(t.Context(), next.instance))
	assert.Nil(t, next.store.healthCheck)
}

func TestCheckCuratedToolList(t *testing.T) {
	template := &v1alpha1.MCPServerTemplate{ObjectMeta: metav1.ObjectMeta{Name: "github"}}
	template.Spec.Tools = map[string]v1alpha1.ToolOverride{"create_issue": {Name: "open_issue"}}
	tools := []*mcp.Tool{{Name: "create_issue"}, {Name: "open_issue"}}

	_, reason := runtime.CheckCuratedToolList(template, tools)
	assert.Equal(t, "Tool name `open_issue` of `open_issue` collides with another tool.", reason)

	// Hiding the other tool removes the collision, and the stored list is
	// the upstream list, not the curated one.
	template.Spec.ToolFilter = &v1alpha1.ToolFilter{Allow: []string{"create_issue"}}

	_, reason = runtime.CheckCuratedToolList(template, tools)
	assert.Equal(t, "Tool name `open_issue` of `create_issue` is the name of a hidden tool.", reason)

	template.Spec.ToolFilter = &v1alpha1.ToolFilter{Deny: []string{"open_issue"}}

	_, reason = runtime.CheckCuratedToolList(template, tools)
	assert.Equal(t, "Tool name `open_issue` of `create_issue` is the name of a hidden tool.", reason)

	template.Spec.Tools = nil

	encoded, reason := runtime.CheckCuratedToolList(template, tools)
	require.Empty(t, reason)
	assert.Contains(t, string(encoded), "open_issue")
}
