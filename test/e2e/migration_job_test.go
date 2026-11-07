//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// splitRoleSQL is the SQL docs/admin/chart.md gives for the split mode,
// with passwords added so the roles can sign in over the network. It runs in
// a new database as the PostgreSQL superuser.
const splitRoleSQL = `
CREATE ROLE toolgate_migrator LOGIN PASSWORD '%[1]s';
CREATE ROLE toolgate_app LOGIN PASSWORD '%[1]s';
CREATE SCHEMA toolgate AUTHORIZATION toolgate_migrator;
CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA toolgate;
CREATE EXTENSION IF NOT EXISTS vector SCHEMA toolgate;
ALTER ROLE toolgate_migrator SET search_path = toolgate;
ALTER ROLE toolgate_app SET search_path = toolgate;
GRANT USAGE ON SCHEMA toolgate TO toolgate_app;
ALTER DEFAULT PRIVILEGES FOR ROLE toolgate_migrator IN SCHEMA toolgate GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO toolgate_app;
ALTER DEFAULT PRIVILEGES FOR ROLE toolgate_migrator IN SCHEMA toolgate GRANT USAGE, SELECT ON SEQUENCES TO toolgate_app;
ALTER DEFAULT PRIVILEGES FOR ROLE toolgate_migrator IN SCHEMA toolgate GRANT EXECUTE ON FUNCTIONS TO toolgate_app;
`

// psql runs SQL in the PostgreSQL pod of the harness, as the superuser of
// database `database`, and returns its output and error.
func psql(t *testing.T, namespace, user, database, sql string) (string, error) {
	t.Helper()

	var output bytes.Buffer

	command := exec.CommandContext(context.WithoutCancel(t.Context()), "kubectl", "--namespace", namespace,
		"exec", "deployment/postgres", "--", "psql", "--host", "127.0.0.1", "--username", user, "--dbname", database,
		"--no-psqlrc", "--tuples-only", "--no-align", "--set", "ON_ERROR_STOP=1", "--command", sql)
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()

	return strings.TrimSpace(output.String()), err
}

// jobObservation records when the migration Job completed and when each new
// gateway pod was created, from watches that run during the helm call.
type jobObservation struct {
	mutex     sync.Mutex
	completed time.Time
	pods      []time.Time
	names     []string
}

func (o *jobObservation) observe(t *testing.T, admin *kubernetes.Clientset, stop <-chan struct{}) {
	t.Helper()

	namespace := releaseNamespace()
	started := time.Now().Add(-time.Second)
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))

	go func() { <-stop; cancel() }()

	jobs, err := admin.BatchV1().Jobs(namespace).Watch(ctx, metav1.ListOptions{})
	require.NoError(t, err)

	pods, err := admin.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{LabelSelector: gatewayPodSelector + ",app.kubernetes.io/controller=gateway"})
	require.NoError(t, err)

	go func() {
		for event := range jobs.ResultChan() {
			job, ok := event.Object.(interface{ GetName() string })
			if !ok || job.GetName() != "toolgate-migrate" {
				continue
			}

			raw, _ := json.Marshal(event.Object)

			var parsed struct {
				Status struct {
					CompletionTime *metav1.Time `json:"completionTime"`
				} `json:"status"`
			}

			_ = json.Unmarshal(raw, &parsed)

			if parsed.Status.CompletionTime != nil {
				o.mutex.Lock()
				o.completed = parsed.Status.CompletionTime.Time
				o.mutex.Unlock()
			}
		}
	}()

	go func() {
		seen := map[string]bool{}

		for event := range pods.ResultChan() {
			pod, ok := event.Object.(*corev1.Pod)
			if !ok || seen[pod.Name] || pod.CreationTimestamp.Time.Before(started) {
				continue
			}

			seen[pod.Name] = true

			o.mutex.Lock()
			o.pods = append(o.pods, pod.CreationTimestamp.Time)
			o.names = append(o.names, pod.Name+" owner="+pod.OwnerReferences[0].Name)
			o.mutex.Unlock()
		}
	}()
}

func TestSeparateMigrationCredential(t *testing.T) {
	image := mcpServerImage(t)
	admin := adminClient(t)
	kube := templateClient(t)
	namespace := releaseNamespace()

	// The database host, and the namespace of the PostgreSQL pod, come from
	// the connection string the harness gave the gateway.
	secret, err := admin.CoreV1().Secrets(namespace).Get(t.Context(), "toolgate-database", metav1.GetOptions{})
	require.NoError(t, err)

	original, err := url.Parse(string(secret.Data["url"]))
	require.NoError(t, err)

	hostParts := strings.Split(original.Hostname(), ".")
	require.GreaterOrEqual(t, len(hostParts), 2, "the host is postgres.<namespace>.svc")

	postgresNamespace := hostParts[1]
	superuser := original.User.Username()
	database := "split" + rand.String(5)
	password := rand.String(24)

	_, err = psql(t, postgresNamespace, superuser, "postgres", "CREATE DATABASE "+database)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = psql(t, postgresNamespace, superuser, "postgres", "DROP DATABASE "+database+" WITH (FORCE)")
		_, _ = psql(t, postgresNamespace, superuser, "postgres", "DROP ROLE IF EXISTS toolgate_app, toolgate_migrator")
	})

	output, err := psql(t, postgresNamespace, superuser, database, "SET search_path = public;\n"+fmt.Sprintf(splitRoleSQL, password))
	require.NoError(t, err, output)

	connection := func(role string) string {
		return (&url.URL{
			Scheme: "postgres", User: url.UserPassword(role, password), Host: original.Host, Path: "/" + database,
			RawQuery: "sslmode=disable",
		}).String()
	}

	for name, role := range map[string]string{"toolgate-split-app": "toolgate_app", "toolgate-split-migrator": "toolgate_migrator"} {
		_, err := admin.CoreV1().Secrets(namespace).Create(t.Context(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			StringData: map[string]string{"url": connection(role)},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		t.Cleanup(func() {
			_ = admin.CoreV1().Secrets(namespace).Delete(context.WithoutCancel(t.Context()), name, metav1.DeleteOptions{})
		})
	}

	// Keep the harness's values, and change only the database settings.
	harness := helmCommand(t, "get", "values", releaseName, "--namespace", namespace, "--output", "yaml")
	harnessFile := filepath.Join(t.TempDir(), "harness.yaml")
	require.NoError(t, os.WriteFile(harnessFile, harness, 0o600))

	splitValues := map[string]any{"config": map[string]any{"database": map[string]any{
		"existingSecret":  "toolgate-split-app",
		"migrateOnStart":  false,
		"migrationSecret": "toolgate-split-migrator",
		"migrationEgress": []any{map[string]any{
			"to": []any{map[string]any{"namespaceSelector": map[string]any{
				"matchLabels": map[string]any{"kubernetes.io/metadata.name": postgresNamespace},
			}}},
			"ports": []any{map[string]any{"protocol": "TCP", "port": 5432}},
		}},
	}}}
	splitFile := filepath.Join(t.TempDir(), "split.yaml")
	raw, err := yaml.Marshal(splitValues)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(splitFile, raw, 0o600))

	// Whatever happens, leave the release as the harness installed it.
	t.Cleanup(func() {
		helmCommand(t, "upgrade", releaseName, chartDir, "--namespace", namespace, "--reset-values",
			"--values", harnessFile, "--wait", "--timeout", "5m")
	})

	upgrade := func(extra ...string) *jobObservation {
		observation := &jobObservation{}
		stop := make(chan struct{})

		observation.observe(t, admin, stop)

		defer close(stop)

		args := append([]string{"upgrade", releaseName, chartDir, "--namespace", namespace, "--reset-values",
			"--values", harnessFile, "--values", splitFile, "--wait", "--timeout", "6m"}, extra...)
		helmCommand(t, args...)

		return observation
	}

	requireJobBeforePods := func(t *testing.T, observation *jobObservation) {
		t.Helper()

		observation.mutex.Lock()
		defer observation.mutex.Unlock()

		t.Logf("new pods %v, job completed %s", observation.names, observation.completed)

		require.False(t, observation.completed.IsZero(), "the migration Job completed during the upgrade")
		require.NotEmpty(t, observation.pods, "the upgrade created new gateway pods")

		for _, created := range observation.pods {
			assert.False(t, created.Before(observation.completed.Truncate(time.Second)),
				"a gateway pod created at %s before the Job completed at %s", created, observation.completed)
		}
	}

	t.Run("the Job migrates before the first gateway pod starts and the schema is current", func(t *testing.T) {
		requireJobBeforePods(t, upgrade())

		latest, err := psql(t, postgresNamespace, superuser, database, "SELECT max(version_id) FROM toolgate.goose_db_version")
		require.NoError(t, err, latest)

		// The newest migration file is the version the binary needs.
		files, err := filepath.Glob(filepath.Join("..", "..", "internal", "store", "migrations", "*.sql"))
		require.NoError(t, err)
		require.NotEmpty(t, files)

		newest := filepath.Base(files[len(files)-1])
		version, err := strconv.Atoi(strings.SplitN(newest, "_", 2)[0])
		require.NoError(t, err)
		assert.Equal(t, strconv.Itoa(version), latest)
	})

	t.Run("the app role cannot change the schema, and enable and a call work", func(t *testing.T) {
		output, err := psql(t, postgresNamespace, "toolgate_app", database, "CREATE TABLE toolgate.stolen (id int)")
		require.Error(t, err)
		assert.Contains(t, output, "permission denied")

		name := createTemplate(t, kube, "e2e-split", "per-session", testServerResources(t, image, 0, nil))
		requireTemplateValid(t, kube, name)

		// The harness gives e2e principals both scoping defaults all in its
		// own database only; this test's database needs the same row, or
		// the workspace-scope step refuses alice every server.
		output, err = psql(t, postgresNamespace, superuser, database,
			"INSERT INTO toolgate.user_scopes (principal, verified_default, unverified_default) VALUES ('alice', 'all', 'all') ON CONFLICT (principal) DO NOTHING")
		require.NoError(t, err, output)

		pods := gatewayPods(t, admin)
		session := connectGateway(t, forwardToGateway(t, admin, pods[0]), "alice", "e2e-"+rand.String(8))

		tools := enable(t, session, name)
		assert.ElementsMatch(t, prefixedTools(name), tools)

		result, err := session.session.CallTool(t.Context(), &mcp.CallToolParams{Name: name + "__echo", Arguments: map[string]any{"text": "split"}})
		require.NoError(t, err)
		assert.False(t, result.IsError)

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			count, err := psql(t, postgresNamespace, superuser, database, "SELECT count(*) FROM toolgate.audit_events")
			require.NoError(collect, err, count)
			assert.NotEqual(collect, "0", count, "the app role wrote audit rows")
		}, time.Minute, time.Second)
	})

	t.Run("helm upgrade runs the hook again before the new pods", func(t *testing.T) {
		requireJobBeforePods(t, upgrade("--set-json", `config.extraEnv=[{"name":"TOOLGATE_E2E_ROLLOUT","value":"`+rand.String(6)+`"}]`))
	})
}
