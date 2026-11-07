package runtime_test

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/internal/runtime"
)

var labelValuePattern = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$`)

func assertLabelsValid(t *testing.T, labels map[string]string) {
	t.Helper()

	for key, value := range labels {
		assert.LessOrEqual(t, len(value), 63, key)
		assert.Regexp(t, labelValuePattern, value, key)
	}
}

// ownerID is SHA-256("alice"), first 16 hex characters, computed
// independently of the code under test.
const ownerIDAlice = "2bd806c97f0e00af"

func TestPerUserInstanceNamespaceAndObjectsCarryOwnerMetadata(t *testing.T) {
	fixture := newStarterFixture(t, testTemplate(configMapResource), "pending")
	fixture.starter.GatewayVersion = "v1.2.3"

	require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

	wantLabels := map[string]string{
		runtime.LabelScope:             "per-user",
		runtime.LabelTemplateNamespace: "catalog",
		runtime.LabelOwner:             ownerIDAlice,
	}
	wantAnnotations := map[string]string{
		runtime.AnnotationOwnerSubject:       "alice",
		runtime.AnnotationOwnerEmail:         "alice@example.com",
		runtime.AnnotationOwnerName:          "Alice",
		runtime.AnnotationTemplateGeneration: "1",
		runtime.AnnotationTemplateUID:        templateUID,
		runtime.AnnotationCreatedBy:          "v1.2.3",
	}

	namespace := &corev1.Namespace{}
	require.NoError(t, fixture.kube.Get(t.Context(), client.ObjectKey{Name: instanceNamespace}, namespace))
	assert.Equal(t, wantAnnotations, namespace.Annotations)
	assertLabelsValid(t, namespace.Labels)

	configMap := &corev1.ConfigMap{}
	require.NoError(t, fixture.kube.Get(t.Context(), client.ObjectKey{Namespace: instanceNamespace, Name: "settings"}, configMap))
	assert.Equal(t, wantAnnotations, configMap.Annotations)

	policies := &networkingv1.NetworkPolicyList{}
	require.NoError(t, fixture.kube.List(t.Context(), policies, client.InNamespace(instanceNamespace)))
	require.Len(t, policies.Items, 1)
	assert.Equal(t, wantAnnotations, policies.Items[0].Annotations)

	for key, value := range wantLabels {
		assert.Equal(t, value, namespace.Labels[key], key)
		assert.Equal(t, value, configMap.Labels[key], key)
		assert.Equal(t, value, policies.Items[0].Labels[key], key)
	}

	for _, key := range []string{runtime.LabelAttestation, runtime.AnnotationWorkspace, runtime.AnnotationSession} {
		assert.NotContains(t, namespace.Labels, key)
		assert.NotContains(t, namespace.Annotations, key)
	}
}

func TestPerSessionInstanceCarriesWorkspaceAndSession(t *testing.T) {
	tests := []struct {
		name            string
		attestation     string
		wantAttestation string
		wantVerified    string
	}{
		{name: "claimed workspace", attestation: "user", wantAttestation: "user", wantVerified: "false"},
		{name: "attested workspace", attestation: "attested", wantAttestation: "attested", wantVerified: "true"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			template := testTemplate(configMapResource)
			template.Spec.Scope = "per-session"
			fixture := newStarterFixture(t, template, "pending")
			fixture.instance.Scope = "per-session"
			session := fixture.store.sessions[7]
			session.Attestation = test.attestation
			session.Workspace = "coder/alice/<proj>@x"
			fixture.store.sessions[7] = session

			require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

			namespace := &corev1.Namespace{}
			require.NoError(t, fixture.kube.Get(t.Context(), client.ObjectKey{Name: instanceNamespace}, namespace))
			assert.Equal(t, "per-session", namespace.Labels[runtime.LabelScope])
			assert.Equal(t, test.wantAttestation, namespace.Labels[runtime.LabelAttestation])
			assert.Equal(t, "coder/alice/<proj>@x", namespace.Annotations[runtime.AnnotationWorkspace])
			assert.Equal(t, test.wantVerified, namespace.Annotations[runtime.AnnotationWorkspaceVerified])
			assert.Equal(t, "session-1", namespace.Annotations[runtime.AnnotationSession])
			assertLabelsValid(t, namespace.Labels)
		})
	}
}

func TestSharedInstanceObjectsCarryOnlyTemplateMetadata(t *testing.T) {
	existing := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: sharedNamespace, Labels: map[string]string{runtime.LabelManagedBy: runtime.ManagedByValue},
	}}
	fixture := newStarterFixture(t, sharedNamespaceTemplate(), "pending", existing)
	fixture.starter.GatewayVersion = "v1.2.3"

	require.NoError(t, fixture.starter.Step(t.Context(), sharedInstance(fixture.instance)))

	wantAnnotations := map[string]string{
		runtime.AnnotationTemplateGeneration: "1",
		runtime.AnnotationTemplateUID:        templateUID,
		runtime.AnnotationCreatedBy:          "v1.2.3",
	}

	configMap := &corev1.ConfigMap{}
	require.NoError(t, fixture.kube.Get(t.Context(), client.ObjectKey{Namespace: sharedNamespace, Name: "settings-" + instanceID}, configMap))
	assert.Equal(t, wantAnnotations, configMap.Annotations)
	assert.Equal(t, "shared", configMap.Labels[runtime.LabelScope])
	assert.Equal(t, "catalog", configMap.Labels[runtime.LabelTemplateNamespace])
	assert.NotContains(t, configMap.Labels, runtime.LabelOwner)

	anchor := &corev1.ConfigMap{}
	require.NoError(t, fixture.kube.Get(t.Context(), client.ObjectKey{Namespace: sharedNamespace, Name: "tg-" + instanceID}, anchor))
	assert.Equal(t, wantAnnotations, anchor.Annotations)
}

func TestOwnerDetailsAnnotationsCanBeTurnedOff(t *testing.T) {
	fixture := newStarterFixture(t, testTemplate(configMapResource), "pending")
	fixture.starter.OmitOwnerDetails = true

	require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

	namespace := &corev1.Namespace{}
	require.NoError(t, fixture.kube.Get(t.Context(), client.ObjectKey{Name: instanceNamespace}, namespace))
	assert.NotContains(t, namespace.Annotations, runtime.AnnotationOwnerEmail)
	assert.NotContains(t, namespace.Annotations, runtime.AnnotationOwnerName)
	assert.Equal(t, "alice", namespace.Annotations[runtime.AnnotationOwnerSubject])
}

func TestTemplateCannotOverrideToolgateMetadata(t *testing.T) {
	resource := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"server",` +
		`"labels":{"toolgate.soliddowant.dev/owner":"mallory","team":"a"},` +
		`"annotations":{"toolgate.soliddowant.dev/owner-subject":"mallory","note":"b"}},` +
		`"spec":{"template":{"metadata":{"labels":{"toolgate.soliddowant.dev/owner":"mallory"},` +
		`"annotations":{"toolgate.soliddowant.dev/owner-subject":"mallory"}}}}}`
	fixture := newStarterFixture(t, testTemplate(resource), "pending")

	require.NoError(t, fixture.starter.Step(t.Context(), fixture.instance))

	deployment := &appsv1.Deployment{}
	require.NoError(t, fixture.kube.Get(t.Context(), client.ObjectKey{Namespace: instanceNamespace, Name: "server"}, deployment))

	assert.Equal(t, ownerIDAlice, deployment.Labels[runtime.LabelOwner])
	assert.Equal(t, "a", deployment.Labels["team"], "the template's own labels are kept")
	assert.Equal(t, "alice", deployment.Annotations[runtime.AnnotationOwnerSubject])
	assert.Equal(t, "b", deployment.Annotations["note"])

	pod := deployment.Spec.Template
	assert.Equal(t, ownerIDAlice, pod.Labels[runtime.LabelOwner], "pods carry the owner too")
	assert.Equal(t, "alice", pod.Annotations[runtime.AnnotationOwnerSubject])
	assert.NotContains(t, pod.Labels, runtime.LabelInstance,
		"a pod without the instance label is not judged by the readiness check, so a pod that is still starting doesn't fail the instance")
}

func TestOwnerIDIsALabelSafeHash(t *testing.T) {
	assert.Equal(t, ownerIDAlice, runtime.OwnerID("alice"))

	for _, subject := range []string{"", "a/b@c.d", "名前", string(make([]byte, 5000))} {
		assert.Regexp(t, `^[0-9a-f]{16}$`, runtime.OwnerID(subject))
	}
}
