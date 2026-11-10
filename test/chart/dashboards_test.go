package chart

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/solidDoWant/toolgate/internal/embeddings"
	"github.com/solidDoWant/toolgate/internal/opsmetrics"
	"github.com/solidDoWant/toolgate/internal/runtime"
	"github.com/solidDoWant/toolgate/internal/telemetry/ingest"
)

var dashboardUIDs = map[string]string{
	"overview.json":    "toolgate-overview",
	"servers.json":     "toolgate-servers",
	"tool-search.json": "toolgate-tool-search",
	"platform.json":    "toolgate-platform",
}

// usageDashboard reads the OTLP metrics that clients send, not the gateway's
// own metrics, so it has its own variables and its own metric names.
const usageDashboard = "usage.json"

type dashboard struct {
	UID        string   `json:"uid"`
	Tags       []string `json:"tags"`
	Templating struct {
		List []struct {
			Name  string `json:"name"`
			Type  string `json:"type"`
			Query any    `json:"query"`
		} `json:"list"`
	} `json:"templating"`
	Panels []struct {
		Type    string `json:"type"`
		Title   string `json:"title"`
		Targets []struct {
			Expr string `json:"expr"`
		} `json:"targets"`
		Datasource struct {
			UID string `json:"uid"`
		} `json:"datasource"`
	} `json:"panels"`
}

func loadDashboards(t *testing.T) map[string]dashboard {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(chartDir, "dashboards", "*.json"))
	require.NoError(t, err)

	loaded := map[string]dashboard{}

	for _, path := range paths {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)

		var parsed dashboard
		require.NoError(t, json.Unmarshal(raw, &parsed), path)

		loaded[filepath.Base(path)] = parsed
	}

	return loaded
}

type fakePool struct{}

func (fakePool) Stat() *pgxpool.Stat { return nil }

// registeredMetricNames returns every series name the gateway can export:
// each registered collector's name, plus the _bucket, _sum and _count series
// of histograms, and the standard process and Go metrics.
func registeredMetricNames(t *testing.T) map[string]bool {
	t.Helper()

	names := map[string]bool{}
	namePattern := regexp.MustCompile(`fqName: "([^"]+)"`)

	add := func(collector prometheus.Collector) {
		_, histogram := collector.(prometheus.Histogram)
		_, observer := collector.(prometheus.ObserverVec)

		descriptions := make(chan *prometheus.Desc, 64)
		collector.Describe(descriptions)
		close(descriptions)

		for description := range descriptions {
			name := namePattern.FindStringSubmatch(description.String())[1]
			names[name] = true

			if histogram || observer {
				for _, suffix := range []string{"_bucket", "_sum", "_count"} {
					names[name+suffix] = true
				}
			}
		}
	}

	for _, collector := range runtime.NewMetrics().Collectors() {
		add(collector)
	}

	for _, collector := range embeddings.NewMetrics().Collectors() {
		add(collector)
	}

	for _, collector := range opsmetrics.Collectors() {
		add(collector)
	}

	for _, collector := range ingest.New(ingest.Options{}).Collectors() {
		add(collector)
	}

	add(opsmetrics.BuildInfo())
	add(opsmetrics.PoolCollector(fakePool{}))
	add(collectors.NewGoCollector())
	add(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	return names
}

var metricNamePattern = regexp.MustCompile(`\b(?:toolgate|process|go)_[a-z0-9_]+`)

// metricNames returns the toolgate, process and Go metric names an expression
// mentions. Label names such as tool_search never match: they follow no prefix
// of those three.
func metricNames(expression string) []string {
	return metricNamePattern.FindAllString(expression, -1)
}

func TestDashboardsAreValidAndComplete(t *testing.T) {
	loaded := loadDashboards(t)
	require.Len(t, loaded, len(dashboardUIDs)+1)

	for file, uid := range dashboardUIDs {
		board, ok := loaded[file]
		require.True(t, ok, file)
		assert.Equal(t, uid, board.UID, file)
		assert.Contains(t, board.Tags, "toolgate", file)

		variables := map[string]string{}
		for _, variable := range board.Templating.List {
			variables[variable.Name] = variable.Type
		}

		assert.Equal(t, "datasource", variables["datasource"], file)
		assert.Equal(t, "query", variables["namespace"], file)
		assert.Equal(t, "query", variables["job"], file)

		for _, panel := range board.Panels {
			if panel.Type == "row" {
				continue
			}

			assert.NotEmpty(t, panel.Targets, "%s: %s", file, panel.Title)
			assert.Equal(t, "${datasource}", panel.Datasource.UID, "%s: %s", file, panel.Title)
		}
	}

	raw, err := os.ReadFile(filepath.Join(chartDir, "dashboards", "overview.json"))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"uid": "P`, "no hard-coded datasource UID")
}

func TestDashboardQueriesUseRegisteredMetrics(t *testing.T) {
	registered := registeredMetricNames(t)
	used := map[string]bool{}

	for file, board := range loadDashboards(t) {
		if file == usageDashboard {
			continue
		}

		for _, panel := range board.Panels {
			for _, target := range panel.Targets {
				for _, name := range metricNames(target.Expr) {
					used[name] = true

					assert.True(t, registered[name], "%s: %q uses %q, which toolgate does not register", file, panel.Title, name)
				}
			}
		}
	}

	sorted := make([]string, 0, len(used))
	for name := range used {
		sorted = append(sorted, name)
	}

	sort.Strings(sorted)
	assert.Contains(t, sorted, "toolgate_tool_search_total")
	assert.Contains(t, sorted, "toolgate_leader")
	assert.Contains(t, sorted, "toolgate_otlp_ingest_requests_total")
	assert.Contains(t, sorted, "toolgate_otlp_ingest_discarded_total")
	assert.Contains(t, sorted, "toolgate_otlp_ingest_forward_duration_seconds_bucket")
}

// Metrics have no label for a user, session, workspace or tool, and the
// metrics with a server label are counters, so per-server panels use topk and
// never a histogram.
func TestDashboardQueriesKeepSeriesCardinalityBounded(t *testing.T) {
	missingLabel := regexp.MustCompile(`\b(user|session|workspace|tool|request)\b\s*(=|!=|=~|!~)|by \([^)]*\b(user|session|workspace|tool|request)\b`)
	serverGrouping := regexp.MustCompile(`by \([^)]*\bserver\b`)

	for file, board := range loadDashboards(t) {
		if file == usageDashboard {
			continue
		}

		for _, panel := range board.Panels {
			for _, target := range panel.Targets {
				assert.False(t, missingLabel.MatchString(target.Expr), "%s: %s", file, target.Expr)

				if serverGrouping.MatchString(target.Expr) {
					assert.Contains(t, target.Expr, "topk(", "%s: per-server panels need topk: %s", file, target.Expr)
					assert.NotContains(t, target.Expr, "_bucket", "%s: server has no histogram: %s", file, target.Expr)
				}
			}
		}
	}
}

func TestDashboardsRenderAsConfigMaps(t *testing.T) {
	for _, candidate := range renderQuickStart(t) {
		assert.NotContains(t, candidate.Metadata.Name, "dashboard")
		assert.NotEqual(t, "GrafanaDashboard", candidate.Kind)
	}

	manifests := renderQuickStart(t, "--set", "dashboards.enabled=true")

	var found []string

	for _, config := range ofKind(manifests, "ConfigMap") {
		if config.Metadata.Labels["grafana_dashboard"] == "" {
			continue
		}

		assert.Equal(t, "1", config.Metadata.Labels["grafana_dashboard"])

		var parsed corev1.ConfigMap
		require.NoError(t, yaml.Unmarshal(config.raw, &parsed))
		require.Len(t, parsed.Data, 1)

		for key, value := range parsed.Data {
			found = append(found, key)

			var board dashboard
			require.NoError(t, json.Unmarshal([]byte(value), &board), key)
			assert.Equal(t, dashboardUIDs[key], board.UID)
		}
	}

	assert.ElementsMatch(t, []string{"overview.json", "servers.json", "tool-search.json", "platform.json"}, found,
		"the usage dashboard is off by default")

	withUsage := renderQuickStart(t, "--set", "dashboards.enabled=true", "--set", "dashboards.usage.enabled=true")
	usage := 0

	for _, config := range ofKind(withUsage, "ConfigMap") {
		if _, ok := config.Metadata.Labels["grafana_dashboard"]; ok && strings.HasSuffix(config.Metadata.Name, "-usage") {
			usage++
		}
	}

	assert.Equal(t, 1, usage)

	custom := renderQuickStart(t, "--set", "dashboards.enabled=true", "--set", "dashboards.labelKey=grafana", "--set", "dashboards.labelValue=yes")
	labelled := 0

	for _, config := range ofKind(custom, "ConfigMap") {
		if config.Metadata.Labels["grafana"] == "yes" {
			labelled++
		}
	}

	assert.Equal(t, 4, labelled)
}

func TestDashboardsRenderAsGrafanaOperatorResources(t *testing.T) {
	_, err := render(t, quickStartValues, "--set", "dashboards.grafanaOperator.enabled=true")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "instanceSelector")

	manifests := renderQuickStart(t,
		"--set", "dashboards.grafanaOperator.enabled=true",
		"--set", "dashboards.grafanaOperator.instanceSelector.matchLabels.dashboards=grafana",
		"--set", "dashboards.grafanaOperator.folder=toolgate")

	resources := ofKind(manifests, "GrafanaDashboard")
	require.Len(t, resources, 4)

	for _, resource := range resources {
		var parsed struct {
			Spec struct {
				InstanceSelector struct {
					MatchLabels map[string]string `json:"matchLabels"`
				} `json:"instanceSelector"`
				Folder string `json:"folder"`
				JSON   string `json:"json"`
			} `json:"spec"`
		}

		require.NoError(t, yaml.Unmarshal(resource.raw, &parsed))
		assert.Equal(t, map[string]string{"dashboards": "grafana"}, parsed.Spec.InstanceSelector.MatchLabels)
		assert.Equal(t, "toolgate", parsed.Spec.Folder)

		var board dashboard
		require.NoError(t, json.Unmarshal([]byte(parsed.Spec.JSON), &board))
		assert.Contains(t, board.Tags, "toolgate")
	}

	for _, candidate := range manifests {
		assert.False(t, candidate.Kind == "ConfigMap" && candidate.Metadata.Labels["grafana_dashboard"] != "", "operator mode makes no sidecar ConfigMaps")
	}
}

func TestUsageDashboardHasItsVariablesAndNoGatewayMetrics(t *testing.T) {
	board, ok := loadDashboards(t)[usageDashboard]
	require.True(t, ok)
	assert.Equal(t, "toolgate-usage", board.UID)

	variables := map[string]string{}
	for _, variable := range board.Templating.List {
		variables[variable.Name] = variable.Type
	}

	assert.Equal(t, map[string]string{"datasource": "datasource", "user": "query", "workspace": "query", "model": "query"}, variables)

	for _, panel := range board.Panels {
		if panel.Type == "row" {
			continue
		}

		for _, target := range panel.Targets {
			assert.Contains(t, target.Expr, "claude_code.", panel.Title)
		}
	}
}

// Claude Code starts new counter series for every session, often with all
// of a short session's usage in the first export. increase() doesn't count a
// new series' first value, so the usage dashboard must count from zero.
func TestUsageDashboardCountsNewSeriesFromZero(t *testing.T) {
	board, ok := loadDashboards(t)[usageDashboard]
	require.True(t, ok)

	increase := regexp.MustCompile(`\bincrease\(`)

	for _, panel := range board.Panels {
		for _, target := range panel.Targets {
			assert.NotRegexp(t, increase, target.Expr, panel.Title)
			assert.Contains(t, target.Expr, "increase_pure(", panel.Title)
		}
	}
}

func TestOverviewLinksToUsageDashboard(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(chartDir, "dashboards", "overview.json"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), "/d/toolgate-usage")
}

func TestChartPassesPerSignalOTLPSettings(t *testing.T) {
	env := gatewayEnv(t, renderQuickStart(t,
		"--set", "config.otlpIngest=true",
		"--set", "config.otlpMetricsEndpoint=http://vm:8428/opentelemetry",
		"--set", "config.otlpLogsEndpoint=http://vl:9428/insert/opentelemetry",
		"--set", "config.dropSessionIdFromMetrics=true"))

	assert.Equal(t, "http://vm:8428/opentelemetry", env["TOOLGATE_OTLP_METRICS_ENDPOINT"])
	assert.Equal(t, "http://vl:9428/insert/opentelemetry", env["TOOLGATE_OTLP_LOGS_ENDPOINT"])
	assert.Equal(t, "true", env["TOOLGATE_OTLP_INGEST"])
	assert.Equal(t, "true", env["TOOLGATE_TELEMETRY_DROP_SESSION_ID_FROM_METRICS"])
	assert.NotContains(t, env, "TOOLGATE_OTLP_TRACES_ENDPOINT")
}
