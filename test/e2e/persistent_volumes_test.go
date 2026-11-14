//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/gateway"
	"github.com/solidDoWant/toolgate/internal/testutil"
)

const (
	// retainStorageClass is the class scripts/e2e/run.sh creates: its
	// PersistentVolumes outlive their claims.
	retainStorageClass = "toolgate-retain"
	// volumeName is the volume every test template declares.
	volumeName = "data"
	// volumeCleanupWait bounds the wait for the leader to delete a volume,
	// the provisioner included.
	volumeCleanupWait = 6 * time.Minute
)

var noteTools = []string{"write_note", "read_note"}

// volumeTemplate creates a per-user template whose test MCP server keeps its
// notes on a persistent volume of class storageClass, and returns its name.
func volumeTemplate(t *testing.T, kube client.Client, prefix, storageClass string) string {
	t.Helper()

	resources := testServerResourcesWithTools(t, mcpServerImage(t), noteTools, 0, map[string]string{"MCPSERVER_DATA_DIR": "/data"})

	for index, resource := range resources {
		var object map[string]any
		require.NoError(t, json.Unmarshal(resource.Raw, &object))

		if object["kind"] != "Deployment" {
			continue
		}

		podSpec := object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		podSpec["volumes"] = []any{map[string]any{
			"name":                  "data",
			"persistentVolumeClaim": map[string]any{"claimName": volumeName},
		}}

		container := podSpec["containers"].([]any)[0].(map[string]any)
		container["volumeMounts"] = []any{map[string]any{"name": "data", "mountPath": "/data"}}

		raw, err := json.Marshal(object)
		require.NoError(t, err)

		resources[index] = k8sruntime.RawExtension{Raw: raw}
	}

	name := createTemplate(t, kube, prefix, "per-user", resources)

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		var template v1alpha1.MCPServerTemplate
		require.NoError(collect, kube.Get(t.Context(), client.ObjectKey{Namespace: releaseNamespace(), Name: name}, &template))

		template.Spec.Start = catalog.StartOnEnable
		template.Spec.PersistentVolumes = []v1alpha1.PersistentVolume{{Name: volumeName, Size: "100Mi", StorageClassName: storageClass}}
		require.NoError(collect, kube.Update(t.Context(), &template))
	}, time.Minute, time.Second, "declare the volume")

	requireTemplateValid(t, kube, name)

	return name
}

// volumeRows returns the output of a query on the persistent_volumes rows of
// a template.
func volumeRows(t *testing.T, admin *kubernetes.Clientset, columns, template, where string) string {
	t.Helper()

	query := "SELECT " + columns + " FROM persistent_volumes WHERE template_name = '" + template + "'"
	if where != "" {
		query += " AND " + where
	}

	return strings.TrimSpace(gatewayQuery(t, admin, query))
}

// recordedVolume returns the PersistentVolume name of the template's only
// volume row for owner, or "" when there is none.
func recordedVolume(t *testing.T, admin *kubernetes.Clientset, template, owner string) string {
	t.Helper()

	return volumeRows(t, admin, "coalesce(pv_name, '')", template, "scope_id = '"+owner+"'")
}

// writeNote and readNote call the test MCP server's note tools through the
// gateway.
func writeNote(t *testing.T, session *gatewaySession, server, text string) {
	t.Helper()

	result, err := session.session.CallTool(t.Context(), &mcp.CallToolParams{Name: server + "__write_note", Arguments: map[string]any{"text": text}})
	require.NoError(t, err)
	require.False(t, result.IsError, toolText(result))
}

func readNote(ctx context.Context, session *gatewaySession, server string) (string, error) {
	result, err := session.session.CallTool(ctx, &mcp.CallToolParams{Name: server + "__read_note"})
	if err != nil {
		return "", err
	}

	if result.IsError {
		return "", &toolError{text: toolText(result)}
	}

	return toolText(result), nil
}

type toolError struct{ text string }

func (e *toolError) Error() string { return e.text }

// requireNote waits until the server's note is want. A server that was just
// restarted refuses calls for a moment.
func requireNote(t *testing.T, session *gatewaySession, server, want, why string) {
	t.Helper()

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		text, err := readNote(t.Context(), session, server)
		require.NoError(collect, err)
		assert.Equal(collect, want, text, why)
	}, 2*time.Minute, 2*time.Second)
}

// stopInstance disables the server for the session. The per-user instance
// has no enabler then and stops, which deletes its namespace and releases
// its volume.
func stopInstance(t *testing.T, admin *kubernetes.Clientset, session *gatewaySession, server string) {
	t.Helper()

	result := callServerTool(t.Context(), session.session, gateway.ToolDisableServer, server)
	require.Empty(t, result.err)

	requireVolumesReleased(t, admin, server)
}

// requireVolumesReleased waits until the instance has stopped, so no volume
// of the template has a holder, and its namespace is gone.
func requireVolumesReleased(t *testing.T, admin *kubernetes.Clientset, server string) {
	t.Helper()

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		assert.Equal(collect, "0", volumeRows(t, admin, "count(*)", server, "instance_id IS NOT NULL"), "no instance holds the volume")
		assert.Empty(collect, instanceNamespaces(collect, t.Context(), admin, server), "the instance namespace is gone")
	}, 4*time.Minute, 2*time.Second)
}

// requirePVGone waits until the PersistentVolume does not exist.
func requirePVGone(t *testing.T, admin *kubernetes.Clientset, pvName, why string) {
	t.Helper()

	require.NotEmpty(t, pvName)

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		_, err := admin.CoreV1().PersistentVolumes().Get(t.Context(), pvName, metav1.GetOptions{})
		assert.True(collect, apierrors.IsNotFound(err), "%s: %v", why, err)
	}, volumeCleanupWait, 3*time.Second)
}

// volumeSession returns an MCP session of user with the gateway.
func volumeSession(t *testing.T, admin *kubernetes.Clientset, pod, user string) *gatewaySession {
	t.Helper()

	return connectGateway(t, forwardToGateway(t, admin, pod), user, "e2e-"+rand.String(8))
}

func TestPersistentVolumeKeepsDataAcrossInstances(t *testing.T) {
	admin := adminClient(t)
	kube := templateClient(t)
	name := volumeTemplate(t, kube, "e2e-pv-keep", retainStorageClass)
	alice := volumeSession(t, admin, gatewayPods(t, admin)[0], "alice")

	enable(t, alice, name)
	writeNote(t, alice, name, "first instance")

	first := requireOneInstance(t, admin, name)
	pvName := recordedVolume(t, admin, name, "alice")
	require.NotEmpty(t, pvName, "a ready instance has its volume recorded")

	volume, err := admin.CoreV1().PersistentVolumes().Get(t.Context(), pvName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "Retain", string(volume.Spec.PersistentVolumeReclaimPolicy))

	stopInstance(t, admin, alice, name)

	volume, err = admin.CoreV1().PersistentVolumes().Get(t.Context(), pvName, metav1.GetOptions{})
	require.NoError(t, err, "the volume outlives the instance")
	assert.Equal(t, "Retain", string(volume.Spec.PersistentVolumeReclaimPolicy))

	enable(t, alice, name)

	second := requireOneInstance(t, admin, name)
	assert.NotEqual(t, first.Name, second.Name, "the second instance is a new one")
	assert.Equal(t, pvName, recordedVolume(t, admin, name, "alice"), "the same volume is in the row both times")
	assert.Equal(t, "1", volumeRows(t, admin, "count(*)", name, ""), "no second row")
	requireNote(t, alice, name, "first instance", "the new instance reads the old instance's note")
}

func TestPersistentVolumeSurvivesCrash(t *testing.T) {
	admin := adminClient(t)
	kube := templateClient(t)
	name := volumeTemplate(t, kube, "e2e-pv-crash", retainStorageClass)

	// Talk to a gateway pod that is not the leader, so killing the leader
	// leaves the session's pod running.
	leader := runningLeader(t, admin)

	var other string

	for _, pod := range gatewayPods(t, admin) {
		if pod != leader {
			other = pod
		}
	}

	require.NotEmpty(t, other, "a gateway pod that is not the leader")

	alice := volumeSession(t, admin, other, "alice")

	enable(t, alice, name)
	writeNote(t, alice, name, "before the server crash")

	namespace := requireOneInstance(t, admin, name)
	pvName := recordedVolume(t, admin, name, "alice")
	require.NotEmpty(t, pvName)

	t.Run("a force-deleted server pod comes back with its data", func(t *testing.T) {
		pods, err := admin.CoreV1().Pods(namespace.Name).List(t.Context(), metav1.ListOptions{LabelSelector: "app=mcp"})
		require.NoError(t, err)
		require.Len(t, pods.Items, 1)

		crashed := pods.Items[0].Name

		require.NoError(t, admin.CoreV1().Pods(namespace.Name).Delete(t.Context(), crashed,
			metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}))

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			replacements, err := admin.CoreV1().Pods(namespace.Name).List(t.Context(), metav1.ListOptions{LabelSelector: "app=mcp"})
			require.NoError(collect, err)

			ready := 0

			for index := range replacements.Items {
				if replacements.Items[index].Name != crashed && podReady(&replacements.Items[index]) {
					ready++
				}
			}

			assert.Equal(collect, 1, ready, "a new server pod is ready")
		}, 2*time.Minute, time.Second)

		requireNote(t, alice, name, "before the server crash", "the replacement pod mounts the same volume")
		assert.Equal(t, pvName, recordedVolume(t, admin, name, "alice"))
		assert.Equal(t, namespace.Name, requireOneInstance(t, admin, name).Name, "the instance keeps its namespace")
	})

	t.Run("a leader killed during a stop loses no data", func(t *testing.T) {
		writeNote(t, alice, name, "before the leader crash")

		leader := runningLeader(t, admin)

		result := callServerTool(t.Context(), alice.session, gateway.ToolDisableServer, name)
		require.Empty(t, result.err)

		require.NoError(t, admin.CoreV1().Pods(releaseNamespace()).Delete(t.Context(), leader,
			metav1.DeleteOptions{GracePeriodSeconds: ptr.To[int64](0)}))

		requireVolumesReleased(t, admin, name)

		enable(t, alice, name)
		assert.Equal(t, pvName, recordedVolume(t, admin, name, "alice"), "the same volume is in the row")
		requireNote(t, alice, name, "before the leader crash", "the new instance reads the note")
	})
}

func TestPersistentVolumeIsolation(t *testing.T) {
	admin := adminClient(t)
	kube := templateClient(t)
	provider := oidcProvider(t)

	for _, user := range []string{"alice", "bob"} {
		provider.AddUser(testutil.OIDCUser{Subject: user, Email: user + "@example.com", EmailVerified: true})
	}

	first := volumeTemplate(t, kube, "e2e-pv-iso-a", retainStorageClass)
	second := volumeTemplate(t, kube, "e2e-pv-iso-b", retainStorageClass)
	pod := gatewayPods(t, admin)[0]
	alice, bob := volumeSession(t, admin, pod, "alice"), volumeSession(t, admin, pod, "bob")

	enable(t, alice, first)
	enable(t, bob, first)
	enable(t, alice, second)

	requireNote(t, alice, first, "", "alice's volume of the first template starts empty")
	requireNote(t, bob, first, "", "bob's volume starts empty")
	requireNote(t, alice, second, "", "the second template's volume starts empty")

	writeNote(t, alice, first, "alice in first")
	writeNote(t, bob, first, "bob in first")
	writeNote(t, alice, second, "alice in second")

	requireNote(t, alice, first, "alice in first", "alice reads her own note")
	requireNote(t, bob, first, "bob in first", "bob reads his own note")
	requireNote(t, alice, second, "alice in second", "the second template keeps its own note")

	volumes := map[string]bool{}

	for _, row := range []struct{ template, owner string }{{first, "alice"}, {first, "bob"}, {second, "alice"}} {
		pvName := recordedVolume(t, admin, row.template, row.owner)
		require.NotEmpty(t, pvName, "%s of %s has a volume", row.owner, row.template)

		volumes[pvName] = true
	}

	assert.Len(t, volumes, 3, "each user and template has a PersistentVolume of its own")
}

func TestPersistentVolumeCleanup(t *testing.T) {
	admin := adminClient(t)
	kube := templateClient(t)

	// startedVolume starts an instance for alice, writes a note and returns
	// the template, its session and its volume, with the instance running.
	startedVolume := func(t *testing.T, prefix string) (string, *gatewaySession, string) {
		t.Helper()

		name := volumeTemplate(t, kube, prefix, retainStorageClass)
		alice := volumeSession(t, admin, gatewayPods(t, admin)[0], "alice")

		enable(t, alice, name)
		writeNote(t, alice, name, "to be deleted")

		pvName := recordedVolume(t, admin, name, "alice")
		require.NotEmpty(t, pvName)

		return name, alice, pvName
	}

	t.Run("deleting the template deletes the volume", func(t *testing.T) {
		name, _, pvName := startedVolume(t, "e2e-pv-tmpl")

		var template v1alpha1.MCPServerTemplate
		require.NoError(t, kube.Get(t.Context(), client.ObjectKey{Namespace: releaseNamespace(), Name: name}, &template))
		require.NoError(t, kube.Delete(t.Context(), &template))

		requirePVGone(t, admin, pvName, "the volume of a deleted template is deleted")

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			assert.Equal(collect, "0", volumeRows(t, admin, "count(*)", name, ""), "the row is deleted")
		}, volumeCleanupWait, 3*time.Second)
	})

	t.Run("a volume unused for longer than unusedRetention is deleted", func(t *testing.T) {
		name, alice, pvName := startedVolume(t, "e2e-pv-unused")

		stopInstance(t, admin, alice, name)

		// The chart sets unusedRetention to its 1 hour minimum. Move the
		// last use back past it instead of waiting.
		gatewayQuery(t, admin, "UPDATE persistent_volumes SET last_used_at = now() - interval '2 hours' WHERE template_name = '"+name+"'")

		requirePVGone(t, admin, pvName, "the unused volume is deleted")
	})

	t.Run("toolgate admin volumes delete deletes the volume", func(t *testing.T) {
		provider := oidcProvider(t)
		endpoint := forwardToGateway(t, admin, gatewayPods(t, admin)[0])
		binary := buildToolgate(t)

		subject := "pv-admin-" + rand.String(6)
		provider.AddUser(testutil.OIDCUser{
			Subject: subject, Email: subject + "@example.com", EmailVerified: true, Groups: []string{"toolgate-admins"},
		})

		home := t.TempDir()
		login := exec.CommandContext(t.Context(), binary, "login", "--gateway", strings.TrimSuffix(endpoint, gateway.MCPPath),
			"--client-id", provider.ClientID, "--token-store", "file")
		login.Env = workspaceEnvironment(home)

		scanForMatch(t, login, regexp.MustCompile(`the code (\S+)$`), func(match []string) {
			require.NoError(t, provider.ApproveDevice(t.Context(), match[1], subject))
		})
		require.NoError(t, login.Wait())

		name, alice, pvName := startedVolume(t, "e2e-pv-admin")
		stopInstance(t, admin, alice, name)

		id := volumeRows(t, admin, "id", name, "")
		require.Regexp(t, `^[0-9a-f]{32}$`, id)

		command := exec.CommandContext(t.Context(), binary, "admin", "volumes", "delete", id)
		command.Env = workspaceEnvironment(home)

		output, err := command.CombinedOutput()
		require.NoError(t, err, string(output))

		requirePVGone(t, admin, pvName, "the volume an admin deleted is deleted")

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			assert.Equal(collect, "0", volumeRows(t, admin, "count(*)", name, ""), "the row is deleted")
		}, volumeCleanupWait, 3*time.Second)
	})
}

func TestPersistentVolumeNeedsRetainClass(t *testing.T) {
	admin := adminClient(t)
	kube := templateClient(t)

	// kind's default class deletes the volume when the claim goes.
	name := volumeTemplate(t, kube, "e2e-pv-delete", "standard")
	alice := volumeSession(t, admin, gatewayPods(t, admin)[0], "alice")

	result := awaitEnable(t, startEnable(t, alice, name))
	require.NotEmpty(t, result.err, "the instance fails to start")
	assert.Contains(t, result.err, "has reclaimPolicy Delete")
	assert.Contains(t, result.err, "Persistent volumes need Retain")

	assert.Empty(t, recordedVolume(t, admin, name, "alice"), "no volume is recorded")
}

func TestPersistentVolumeLostIsReplaced(t *testing.T) {
	admin := adminClient(t)
	kube := templateClient(t)
	name := volumeTemplate(t, kube, "e2e-pv-lost", retainStorageClass)
	alice := volumeSession(t, admin, gatewayPods(t, admin)[0], "alice")

	enable(t, alice, name)
	writeNote(t, alice, name, "in the lost volume")

	lost := recordedVolume(t, admin, name, "alice")
	require.NotEmpty(t, lost)

	stopInstance(t, admin, alice, name)

	require.NoError(t, admin.CoreV1().PersistentVolumes().Delete(t.Context(), lost, metav1.DeleteOptions{}))
	requirePVGone(t, admin, lost, "the test deleted the volume")

	enable(t, alice, name)

	requireNote(t, alice, name, "", "the new instance gets an empty volume")

	replacement := recordedVolume(t, admin, name, "alice")
	require.NotEmpty(t, replacement)
	assert.NotEqual(t, lost, replacement, "the row names a new volume")

	assert.Equal(t, "1", strings.TrimSpace(gatewayQuery(t, admin,
		"SELECT count(*) FROM audit_events WHERE type = 'volume.lost' AND server = '"+name+"' AND details->>'persistentVolume' = '"+lost+"'")),
		"the loss is audited")
}
