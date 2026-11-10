package ingest

import (
	"strings"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/solidDoWant/toolgate/internal/auth"
)

// The attributes set from the verified identity on every span, metric data
// point and log record.
const (
	attributeUserID      = "user.id"
	attributeEndUserID   = "enduser.id"
	attributeUserEmail   = "user.email"
	attributeWorkspaceID = "workspace.id"
)

// identityPrefixes are the normalized key prefixes removed at every level, so
// a client can't send a value next to the verified one, under any spelling a
// metrics backend would merge with it.
var identityPrefixes = []string{"user_", "enduser_", "workspace_"}

// Content that Claude Code adds when OTEL_LOG_USER_PROMPTS,
// OTEL_LOG_TOOL_DETAILS or OTEL_LOG_TOOL_CONTENT is set, as listed on
// https://code.claude.com/docs/en/monitoring-usage. It is removed unless the
// gateway's telemetry.allowContent setting is true.
var (
	// contentAttributes are removed from every span, span event and log
	// record.
	contentAttributes = map[string]bool{
		"prompt":                true,
		"prompt_text":           true,
		"user_prompt":           true,
		"response":              true,
		"response.model_output": true,
		"new_context":           true,
		"system_reminders":      true,
		"system_prompt_preview": true,
		"user_system_prompt":    true,
		"tool_parameters":       true,
		"tool_input":            true,
		"full_command":          true,
		"file_path":             true,
		"bash_command":          true,
		"diff":                  true,
		"content":               true,
		"output":                true,
		"hook_definitions":      true,
		"hook_matcher":          true,
	}

	// errorContentSpans and errorContentEvents name the spans and log
	// records whose `error` attribute holds the full error message, which
	// can quote tool content. Elsewhere `error` is kept.
	errorContentSpans  = map[string]bool{"claude_code.tool.execution": true}
	errorContentEvents = map[string]bool{
		"claude_code.tool_result":           true,
		"claude_code.mcp_server_connection": true,
	}

	// contentSpanEvents and contentLogEvents are removed whole.
	contentSpanEvents = map[string]bool{"tool.output": true}
	contentLogEvents  = map[string]bool{"claude_code.system_prompt": true}
)

const attributeError = "error"

// stamper rewrites a decoded export request for one verified identity.
type stamper struct {
	identity     auth.Identity
	allowContent bool
	// dropSessionID removes `session.id` from metric data points.
	dropSessionID bool
}

// normalizeKey replaces every character outside [A-Za-z0-9] with `_` and
// lowers the case, as a metrics backend's label translation would.
func normalizeKey(key string) string {
	var builder strings.Builder

	builder.Grow(len(key))

	for _, character := range key {
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			builder.WriteRune(character)
		case character >= 'A' && character <= 'Z':
			builder.WriteRune(character - 'A' + 'a')
		default:
			builder.WriteByte('_')
		}
	}

	return builder.String()
}

// isIdentityKey reports whether key's normalized form starts with one of the
// identity prefixes.
func isIdentityKey(key string) bool {
	normalized := normalizeKey(key)
	for _, prefix := range identityPrefixes {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}

	return false
}

// removeIdentity removes the identity attributes from attributes.
func removeIdentity(attributes pcommon.Map) {
	attributes.RemoveIf(func(key string, _ pcommon.Value) bool { return isIdentityKey(key) })
}

// removeContent removes the content attributes, and `error` too when
// withError is set, unless content is allowed.
func (s stamper) removeContent(attributes pcommon.Map, withError bool) {
	if s.allowContent {
		return
	}

	attributes.RemoveIf(func(key string, _ pcommon.Value) bool {
		return contentAttributes[key] || (withError && key == attributeError)
	})
}

// record cleans the attributes of a span, data point or log record and sets
// the verified identity on them.
func (s stamper) record(attributes pcommon.Map) {
	removeIdentity(attributes)
	attributes.PutStr(attributeUserID, s.identity.Principal)
	attributes.PutStr(attributeEndUserID, s.identity.Principal)

	if s.identity.Email != "" {
		attributes.PutStr(attributeUserEmail, s.identity.Email)
	}

	attributes.PutStr(attributeWorkspaceID, s.identity.Workspace)
}

func (s stamper) traces(traces ptrace.Traces) {
	resources := traces.ResourceSpans()
	for resourceIndex := range resources.Len() {
		resource := resources.At(resourceIndex)
		removeIdentity(resource.Resource().Attributes())

		scopes := resource.ScopeSpans()
		for scopeIndex := range scopes.Len() {
			scope := scopes.At(scopeIndex)
			removeIdentity(scope.Scope().Attributes())

			spans := scope.Spans()
			for spanIndex := range spans.Len() {
				s.span(spans.At(spanIndex))
			}
		}
	}
}

func (s stamper) span(span ptrace.Span) {
	s.removeContent(span.Attributes(), errorContentSpans[span.Name()])
	s.record(span.Attributes())

	events := span.Events()
	if !s.allowContent {
		events.RemoveIf(func(event ptrace.SpanEvent) bool { return contentSpanEvents[event.Name()] })
	}

	for eventIndex := range events.Len() {
		attributes := events.At(eventIndex).Attributes()
		removeIdentity(attributes)
		s.removeContent(attributes, false)
	}

	links := span.Links()
	for linkIndex := range links.Len() {
		removeIdentity(links.At(linkIndex).Attributes())
	}
}

func (s stamper) metrics(metrics pmetric.Metrics) {
	resources := metrics.ResourceMetrics()
	for resourceIndex := range resources.Len() {
		resource := resources.At(resourceIndex)
		removeIdentity(resource.Resource().Attributes())

		scopes := resource.ScopeMetrics()
		for scopeIndex := range scopes.Len() {
			scope := scopes.At(scopeIndex)
			removeIdentity(scope.Scope().Attributes())

			list := scope.Metrics()
			for metricIndex := range list.Len() {
				s.metric(list.At(metricIndex))
			}
		}
	}
}

func (s stamper) metric(metric pmetric.Metric) {
	switch metric.Type() {
	case pmetric.MetricTypeGauge:
		points := metric.Gauge().DataPoints()
		for index := range points.Len() {
			s.metricPoint(points.At(index).Attributes())
			exemplars(points.At(index).Exemplars())
		}
	case pmetric.MetricTypeSum:
		points := metric.Sum().DataPoints()
		for index := range points.Len() {
			s.metricPoint(points.At(index).Attributes())
			exemplars(points.At(index).Exemplars())
		}
	case pmetric.MetricTypeHistogram:
		points := metric.Histogram().DataPoints()
		for index := range points.Len() {
			s.metricPoint(points.At(index).Attributes())
			exemplars(points.At(index).Exemplars())
		}
	case pmetric.MetricTypeExponentialHistogram:
		points := metric.ExponentialHistogram().DataPoints()
		for index := range points.Len() {
			s.metricPoint(points.At(index).Attributes())
			exemplars(points.At(index).Exemplars())
		}
	case pmetric.MetricTypeSummary:
		points := metric.Summary().DataPoints()
		for index := range points.Len() {
			s.metricPoint(points.At(index).Attributes())
		}
	case pmetric.MetricTypeEmpty:
	}

	if s.dropSessionID {
		mergeDuplicatePoints(metric)
	}
}

// attributeSessionID is the client's session label, which the gateway can
// remove from metric data points.
const attributeSessionID = "session.id"

// metricPoint stamps a metric data point's attributes, and removes the
// session label when configured.
func (s stamper) metricPoint(attributes pcommon.Map) {
	s.record(attributes)

	if s.dropSessionID {
		attributes.Remove(attributeSessionID)
	}
}

func exemplars(list pmetric.ExemplarSlice) {
	for index := range list.Len() {
		removeIdentity(list.At(index).FilteredAttributes())
	}
}

func (s stamper) logs(logs plog.Logs) {
	resources := logs.ResourceLogs()
	for resourceIndex := range resources.Len() {
		resource := resources.At(resourceIndex)
		removeIdentity(resource.Resource().Attributes())

		scopes := resource.ScopeLogs()
		for scopeIndex := range scopes.Len() {
			scope := scopes.At(scopeIndex)
			removeIdentity(scope.Scope().Attributes())

			records := scope.LogRecords()
			if !s.allowContent {
				records.RemoveIf(func(record plog.LogRecord) bool { return namedOneOf(record, contentLogEvents) })
			}

			for recordIndex := range records.Len() {
				record := records.At(recordIndex)
				s.removeContent(record.Attributes(), namedOneOf(record, errorContentEvents))
				s.record(record.Attributes())
			}
		}
	}
}

// claudeCodeEventPrefix prefixes the short `event.name` attribute of Claude
// Code's log events to give the full event name.
const claudeCodeEventPrefix = "claude_code."

// namedOneOf reports whether the log record's event name is in names. The
// name is read from the record's event name field, its string body, or its
// `event.name` attribute, which Claude Code sends without its
// `claude_code.` prefix.
func namedOneOf(record plog.LogRecord, names map[string]bool) bool {
	if names[record.EventName()] {
		return true
	}

	if record.Body().Type() == pcommon.ValueTypeStr && names[record.Body().Str()] {
		return true
	}

	if value, ok := record.Attributes().Get("event.name"); ok && value.Type() == pcommon.ValueTypeStr {
		name := value.Str()

		return names[name] || names[claudeCodeEventPrefix+name]
	}

	return false
}
