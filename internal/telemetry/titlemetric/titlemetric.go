// Package titlemetric defines the gauge that carries a Claude Code session's
// title: the plugin's hooks send it, and the OTLP ingest filters it.
package titlemetric

import (
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

const (
	// Name is the gauge's name.
	Name = "toolgate.session.title"
	// AttributeSessionID holds Claude Code's session ID.
	AttributeSessionID = "session.id"
	// AttributeTitle holds the session's title. It is written from the
	// user's prompts, so the ingest treats it as content.
	AttributeTitle = "session.title"
	// ServiceName is the resource's service.name.
	ServiceName = "toolgate-plugin"
)

// Metrics returns one gauge data point of value 1 at now, naming the
// session's title.
func Metrics(sessionID, title string, now time.Time) pmetric.Metrics {
	metrics := pmetric.NewMetrics()

	resource := metrics.ResourceMetrics().AppendEmpty()
	resource.Resource().Attributes().PutStr("service.name", ServiceName)

	metric := resource.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	metric.SetName(Name)
	metric.SetUnit("1")

	point := metric.SetEmptyGauge().DataPoints().AppendEmpty()
	point.SetTimestamp(pcommon.NewTimestampFromTime(now))
	point.SetIntValue(1)
	point.Attributes().PutStr(AttributeSessionID, sessionID)
	point.Attributes().PutStr(AttributeTitle, title)

	return metrics
}
