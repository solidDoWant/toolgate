package runtime

import "time"

// revisionLabel maps an instance's stored MCP revision to the `revision`
// label. A row that hasn't negotiated one yet counts as the stateful
// revision, which toolgate tries first.
func revisionLabel(revision string) string {
	if revision == RevisionStateless {
		return RevisionStateless
	}

	return RevisionStateful
}

// ObserveCall counts one tool call. A nil Metrics does nothing.
func (m *Metrics) ObserveCall(server, outcome string) {
	if m != nil {
		m.ToolCalls.WithLabelValues(server, outcome).Inc()
	}
}

// ObserveCallDuration records the duration of a forwarded call.
func (m *Metrics) ObserveCallDuration(duration time.Duration) {
	if m != nil {
		m.ToolCallDuration.Observe(duration.Seconds())
	}
}

// ObservePolicyRefusal counts one policy refusal.
func (m *Metrics) ObservePolicyRefusal(step, reason string) {
	if m != nil {
		m.PolicyRefusals.WithLabelValues(step, reason).Inc()
	}
}

// WaitingForStart changes the count of calls waiting for a starting
// instance by delta.
func (m *Metrics) WaitingForStart(delta float64) {
	if m != nil {
		m.CallsWaiting.Add(delta)
	}
}

func (m *Metrics) observeStart(server, outcome string) {
	if m != nil {
		m.InstanceStarts.WithLabelValues(server, outcome).Inc()
	}
}

func (m *Metrics) observeReady(server string, startedAt, readyAt time.Time) {
	if m == nil {
		return
	}

	m.InstanceStarts.WithLabelValues(server, StartReady).Inc()

	if !startedAt.IsZero() {
		m.InstanceStartDuration.Observe(readyAt.Sub(startedAt).Seconds())
	}
}

func (m *Metrics) observeHealthFailure(server string) {
	if m != nil {
		m.HealthFailures.WithLabelValues(server).Inc()
	}
}

func (m *Metrics) observeRepair(server, action string) {
	if m != nil {
		m.Repairs.WithLabelValues(server, action).Inc()
	}
}

// Events of the toolgate_persistent_volume_events_total metric.
const (
	VolumeEventCreated      = "created"
	VolumeEventRebound      = "rebound"
	VolumeEventLost         = "lost"
	VolumeEventDeleted      = "deleted"
	VolumeEventDeleteFailed = "delete_failed"
	VolumeEventDrainTimeout = "drain_timeout"
	VolumeEventLimitRefused = "limit_refused"
)

// VolumeEvents lists every value of the `event` label of
// toolgate_persistent_volume_events_total.
var VolumeEvents = []string{
	VolumeEventCreated, VolumeEventRebound, VolumeEventLost, VolumeEventDeleted,
	VolumeEventDeleteFailed, VolumeEventDrainTimeout, VolumeEventLimitRefused,
}

// ObserveVolumeEvent counts one persistent volume event. A nil Metrics does
// nothing.
func (m *Metrics) ObserveVolumeEvent(event string) {
	if m != nil {
		m.PersistentVolumeEvents.WithLabelValues(event).Inc()
	}
}
