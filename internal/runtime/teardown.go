package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/solidDoWant/toolgate/internal/opsmetrics"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

const (
	// StuckAfter is how long a namespace may stay Terminating before the
	// reconciler warns and records the namespace's conditions as the reason.
	StuckAfter = 10 * time.Minute
	// StoppedRetention is how long stopped rows are kept for the audit
	// trail.
	StoppedRetention = 7 * 24 * time.Hour
)

// Deleter deletes Kubernetes objects. Reconciler.Delete implements it, and
// deletes only while this replica holds the Lease.
type Deleter interface {
	Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error
}

// Teardown moves failed and stopped instances through stopping to stopped,
// deleting their namespaces.
type Teardown struct {
	Pool *pgxpool.Pool
	// Reader reads instance namespaces.
	Reader  client.Reader
	Deleter Deleter
	Log     *slog.Logger
	Metrics *Metrics
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time

	// stuck counts the stuck namespaces seen in the current pass.
	stuck atomic.Int64
}

// Metrics are the instance metrics the reconciler keeps.
type Metrics struct {
	// Instances counts instance rows that aren't stopped, by scope and
	// status.
	Instances *prometheus.GaugeVec
	// NamespacesStuck counts instance namespaces Terminating for longer
	// than StuckAfter.
	NamespacesStuck prometheus.Gauge
	// Sessions counts open sessions, by whether the workspace is verified.
	Sessions *prometheus.GaugeVec

	// ToolCalls counts tool calls by server and outcome.
	ToolCalls *prometheus.CounterVec
	// ToolCallDuration is the time from receiving a forwarded call to the
	// upstream answer.
	ToolCallDuration prometheus.Histogram
	// ResultCacheLookups counts tool result cache lookups by server and
	// result (hit or miss).
	ResultCacheLookups *prometheus.CounterVec
	// ResultCacheStores counts tool result cache stores by result.
	ResultCacheStores *prometheus.CounterVec
	// PolicyRefusals counts policy refusals by step and reason.
	PolicyRefusals *prometheus.CounterVec
	// InstanceStarts counts finished starts by server and outcome.
	InstanceStarts *prometheus.CounterVec
	// InstanceStartDuration is the time from the insert of an instance to
	// ready.
	InstanceStartDuration prometheus.Histogram
	// CallsWaiting counts the calls waiting for a starting instance on this
	// replica.
	CallsWaiting prometheus.Gauge
	// HealthFailures counts instances that turned unhealthy.
	HealthFailures *prometheus.CounterVec
	// Repairs counts repairs by server and action.
	Repairs *prometheus.CounterVec
	// TemplateSecretUpdates counts the updates of template secret copies by
	// outcome.
	TemplateSecretUpdates *prometheus.CounterVec

	// Searches, SearchHits, SearchResearches and SearchNoCalls count the
	// outcomes of tool searches (see searchquality.go).
	Searches         *prometheus.CounterVec
	SearchHits       *prometheus.CounterVec
	SearchResearches *prometheus.CounterVec
	SearchNoCalls    *prometheus.CounterVec
	// ApprovalsPending counts pending approval and access requests, by
	// kind.
	ApprovalsPending *prometheus.GaugeVec
	// Leader is 1 while this replica holds the Lease.
	Leader prometheus.Gauge

	// PersistentVolumes counts volume rows by scope and state.
	PersistentVolumes *prometheus.GaugeVec
	// PersistentVolumeBytes is the sum of the rows' size_bytes by scope.
	PersistentVolumeBytes *prometheus.GaugeVec
	// PersistentVolumeEvents counts volume events (the Volume* constants).
	PersistentVolumeEvents *prometheus.CounterVec
}

// Outcomes of a tool call (the `outcome` label of toolgate_tool_calls_total).
const (
	CallForwarded     = "forwarded"
	CallRefused       = "refused"
	CallApproval      = "approval"
	CallUpstreamError = "upstream_error"
	CallTimeout       = "timeout"
	CallStartFailed   = "start_failed"
	// CallCached is a call served from the tool result cache.
	CallCached = "cached"
)

// Results of a tool result cache lookup and store (the `result` label of
// toolgate_tool_result_cache_lookups_total and
// toolgate_tool_result_cache_stores_total). The other store results are the
// resultcache.PutResult values.
const (
	CacheHit         = "hit"
	CacheMiss        = "miss"
	CacheStoreFailed = "failed"
)

// Outcomes of an instance start, and the actions of a repair.
const (
	StartReady  = "ready"
	StartFailed = "failed"

	RepairReplace = "replace"
	RepairReapply = "reapply"
	RepairGaveUp  = "gave_up"
)

// NewMetrics returns unregistered instance metrics.
func NewMetrics() *Metrics {
	return &Metrics{
		Instances: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "toolgate_instances",
			Help: "Instances that aren't stopped, by scope, status and MCP revision. Set by the replica holding the leader Lease.",
		}, []string{"scope", "status", "revision"}),
		NamespacesStuck: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "toolgate_instance_namespaces_stuck",
			Help: "Instance namespaces Terminating for more than 10 minutes. Set by the replica holding the leader Lease.",
		}),
		Sessions: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "toolgate_sessions",
			Help: "Open sessions (not ended), by tool search mode (tool_search) and whether the session is attested (verified). Set by the replica holding the leader Lease.",
		}, []string{"tool_search", "verified"}),
		ToolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toolgate_tool_calls_total",
			Help: "Tool calls, by server and outcome.",
		}, []string{"server", "outcome"}),
		ToolCallDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "toolgate_tool_call_duration_seconds",
			Help:    "Time from receiving a forwarded tool call to the upstream answer, including any wait for a starting instance.",
			Buckets: []float64{0.05, 0.25, 1, 5, 15, 30, 60, 120},
		}),
		ResultCacheLookups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toolgate_tool_result_cache_lookups_total",
			Help: "Tool result cache lookups, by server and result (hit or miss).",
		}, []string{"server", "result"}),
		ResultCacheStores: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toolgate_tool_result_cache_stores_total",
			Help: "Tool result cache stores, by result (stored, entry_too_large, template_full, session_ended or failed).",
		}, []string{"result"}),
		PolicyRefusals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toolgate_policy_refusals_total",
			Help: "Policy refusals, by step and reason.",
		}, []string{"step", "reason"}),
		InstanceStarts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toolgate_instance_starts_total",
			Help: "Instance starts that finished, by server and outcome.",
		}, []string{"server", "outcome"}),
		InstanceStartDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "toolgate_instance_start_duration_seconds",
			Help:    "Time from the insert of an instance to ready.",
			Buckets: []float64{1, 5, 15, 30, 60, 120, 300},
		}),
		CallsWaiting: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "toolgate_calls_waiting_for_start",
			Help: "Tool calls on this replica waiting for a starting instance.",
		}),
		HealthFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toolgate_instance_health_failures_total",
			Help: "Instances that turned unhealthy, by server.",
		}, []string{"server"}),
		Repairs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toolgate_instance_repairs_total",
			Help: "Repairs of unhealthy instances, by server and action.",
		}, []string{"server", "action"}),
		TemplateSecretUpdates: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toolgate_template_secret_updates_total",
			Help: "Updates of template secret copies in running instances, by outcome. Counted by the replica holding the leader Lease.",
		}, []string{"outcome"}),
		Searches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toolgate_tool_search_total",
			Help: "Tool searches, by whether search was degraded (keywords only) and the embedding model configured.",
		}, []string{"degraded", "embedding_model"}),
		SearchHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toolgate_tool_search_hits_total",
			Help: "Searches followed by a call of a returned tool, by the rank of that tool, whether search was degraded and the embedding model.",
		}, []string{"rank", "degraded", "embedding_model"}),
		SearchResearches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toolgate_tool_search_researches_total",
			Help: "Searches followed by a new search before any call of a returned tool, by whether search was degraded and the embedding model.",
		}, []string{"degraded", "embedding_model"}),
		SearchNoCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toolgate_tool_search_no_call_total",
			Help: "Searches with no call and no newer search when the session ended or after 30 minutes, by whether search was degraded and the embedding model. Counted by the replica holding the leader Lease.",
		}, []string{"degraded", "embedding_model"}),
		ApprovalsPending: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "toolgate_approval_requests_pending",
			Help: "Approval and access requests waiting for a decision, by kind. Set by the replica holding the leader Lease.",
		}, []string{"kind"}),
		Leader: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "toolgate_leader",
			Help: "1 on the replica holding the leader Lease. Other replicas export no series.",
		}),
		PersistentVolumes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "toolgate_persistent_volumes",
			Help: "Persistent volume rows, by scope and state (in_use, idle or deleting). Set by the replica holding the leader Lease.",
		}, []string{"scope", "state"}),
		PersistentVolumeBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "toolgate_persistent_volume_bytes",
			Help: "Sum of the sizes of persistent volume rows, by scope. Set by the replica holding the leader Lease.",
		}, []string{"scope"}),
		PersistentVolumeEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "toolgate_persistent_volume_events_total",
			Help: "Persistent volume events, by event.",
		}, []string{"event"}),
	}
}

// Collectors returns the metrics for registration.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.Instances, m.NamespacesStuck, m.Sessions, m.ToolCalls, m.ToolCallDuration,
		m.ResultCacheLookups, m.ResultCacheStores,
		m.PolicyRefusals, m.InstanceStarts, m.InstanceStartDuration, m.CallsWaiting, m.HealthFailures, m.Repairs, m.TemplateSecretUpdates,
		m.Searches, m.SearchHits, m.SearchResearches, m.SearchNoCalls,
		m.ApprovalsPending, m.Leader,
		m.PersistentVolumes, m.PersistentVolumeBytes, m.PersistentVolumeEvents}
}

// Step takes one instance one step towards stopped. It is a Step of the
// full pass, which must be wrapped with Pass.
func (t *Teardown) Step(ctx context.Context, instance db.Instance) error {
	stuck, err := t.Advance(ctx, instance)
	if stuck {
		t.stuck.Add(1)
	}

	return err
}

// Pass wraps a full pass that runs Step. After the pass it deletes stopped
// rows older than StoppedRetention and sets the metrics.
func (t *Teardown) Pass(queries *db.Queries, pass func(ctx context.Context) error) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		t.stuck.Store(0)

		err := pass(ctx)

		return errors.Join(err, t.DeleteExpired(ctx), t.Metrics.update(ctx, queries, int(t.stuck.Load())))
	}
}

// Advance takes one instance one step towards stopped. It returns true when
// the instance's namespace has been Terminating for longer than StuckAfter.
func (t *Teardown) Advance(ctx context.Context, instance db.Instance) (bool, error) {
	if instance.Status == statusFailed || (instance.Desired == desiredStopped && isActive(instance.Status)) {
		changed, err := t.write(ctx, instance.ID, func(queries *db.Queries) (int64, error) {
			return queries.SetInstanceStopping(ctx, db.SetInstanceStoppingParams{ID: instance.ID, FromStatus: instance.Status})
		})
		if err != nil || !changed {
			// The row changed under us; the next pass reads it again.
			return false, err
		}

		instance.Status = statusStopping
	}

	if instance.Status != statusStopping {
		return false, nil
	}

	// A remote server's instance has no Kubernetes objects to delete.
	if IsRemote(instance) {
		_, err := t.write(ctx, instance.ID, func(queries *db.Queries) (int64, error) {
			return queries.SetInstanceStopped(ctx, instance.ID)
		})

		return false, err
	}

	if !hasOwnNamespace(instance) {
		return false, t.deleteSharedObjects(ctx, instance)
	}

	var namespace corev1.Namespace

	err := t.Reader.Get(ctx, types.NamespacedName{Name: instance.Namespace}, &namespace)
	if apierrors.IsNotFound(err) {
		_, err := t.write(ctx, instance.ID, func(queries *db.Queries) (int64, error) {
			return queries.SetInstanceStopped(ctx, instance.ID)
		})

		return false, err
	}

	if err != nil {
		return false, fmt.Errorf("read namespace of instance %s: %w", instance.ID, err)
	}

	// Only a namespace made for this instance is deleted, never a shared
	// one.
	if namespace.Labels[LabelInstance] != instance.ID {
		reason := fmt.Sprintf("Namespace %s doesn't carry the label %s=%s, so it isn't deleted.", namespace.Name, LabelInstance, instance.ID)

		return false, t.setReason(ctx, instance, reason)
	}

	if namespace.DeletionTimestamp == nil {
		if err := t.Deleter.Delete(ctx, &namespace, client.Preconditions{UID: &namespace.UID}); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}

		return false, nil
	}

	if t.now().Sub(namespace.DeletionTimestamp.Time) <= StuckAfter {
		return false, nil
	}

	reason := truncate("Namespace "+namespace.Name+" is still terminating: "+namespaceConditions(&namespace), maxReasonBytes)
	if reason != instance.Reason {
		t.Log.WarnContext(ctx, "instance namespace is still terminating", slog.String("instance", instance.ID),
			slog.String("namespace", namespace.Name), slog.Time("deletionTimestamp", namespace.DeletionTimestamp.Time))
	}

	return true, t.setReason(ctx, instance, reason)
}

// deleteSharedObjects deletes the objects labelled with the instance in the
// namespace it shares with other instances, never the namespace. Once none
// is left, the row is stopped.
func (t *Teardown) deleteSharedObjects(ctx context.Context, instance db.Instance) error {
	objects, err := listInstanceObjects(ctx, t.Reader, instance.Namespace, client.MatchingLabels{LabelInstance: instance.ID})
	if err != nil {
		return fmt.Errorf("instance %s: %w", instance.ID, err)
	}

	if len(objects) == 0 {
		_, err := t.write(ctx, instance.ID, func(queries *db.Queries) (int64, error) {
			return queries.SetInstanceStopped(ctx, instance.ID)
		})

		return err
	}

	var problems []error

	for _, object := range objects {
		if object.GetDeletionTimestamp() != nil {
			continue
		}

		uid := object.GetUID()
		if err := t.Deleter.Delete(ctx, object, client.Preconditions{UID: &uid}, backgroundDeletion); err != nil &&
			!apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			problems = append(problems, err)
		}
	}

	return errors.Join(problems...)
}

// backgroundDeletion deletes an object at once and leaves its dependents to
// Kubernetes' garbage collector. Deleting an instance's anchor ConfigMap so
// removes the instance's Secrets in the shared namespace, which the gateway
// can't list.
var backgroundDeletion = client.PropagationPolicy(metav1.DeletePropagationBackground)

// sharedNamespaceKinds are the kinds of instance objects found by their
// labels in a shared namespace: the allowed kinds the gateway may list.
// Secrets are left out because the gateway never reads Secrets: they are
// owned by the instance's anchor ConfigMap and deleted with it.
var sharedNamespaceKinds = []schema.GroupVersionKind{
	{Version: "v1", Kind: "Pod"},
	{Group: "apps", Version: "v1", Kind: "Deployment"},
	{Group: "apps", Version: "v1", Kind: "StatefulSet"},
	{Version: "v1", Kind: "Service"},
	{Version: "v1", Kind: "ConfigMap"},
	{Version: "v1", Kind: "ServiceAccount"},
	{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"},
	{Version: "v1", Kind: "PersistentVolumeClaim"},
}

// listInstanceObjects lists the objects of sharedNamespaceKinds in namespace
// that carry toolgate's label and match selector.
func listInstanceObjects(ctx context.Context, reader client.Reader, namespace string, selector client.ListOption) ([]*unstructured.Unstructured, error) {
	var objects []*unstructured.Unstructured

	for _, kind := range sharedNamespaceKinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(kind.GroupVersion().WithKind(kind.Kind + "List"))

		if err := reader.List(ctx, list, client.InNamespace(namespace),
			client.MatchingLabels{LabelManagedBy: ManagedByValue}, selector); err != nil {
			return nil, fmt.Errorf("list %s in %s: %w", kind.Kind, namespace, err)
		}

		for index := range list.Items {
			objects = append(objects, &list.Items[index])
		}
	}

	return objects, nil
}

// hasOwnNamespace reports whether the instance runs in the `tg-<id>`
// namespace made for it, rather than in the shared namespace.
func hasOwnNamespace(instance db.Instance) bool {
	return instance.Namespace == NamespacePrefix+instance.ID
}

// DeleteExpired deletes stopped rows older than StoppedRetention.
func (t *Teardown) DeleteExpired(ctx context.Context) error {
	if _, err := db.New(t.Pool).DeleteStoppedInstancesBefore(ctx, pgtype.Timestamptz{Time: t.now().Add(-StoppedRetention), Valid: true}); err != nil {
		return fmt.Errorf("delete old stopped instances: %w", err)
	}

	return nil
}

// setReason records reason on a stopping row, unless it is already there.
func (t *Teardown) setReason(ctx context.Context, instance db.Instance, reason string) error {
	if instance.Reason == reason {
		return nil
	}

	_, err := t.write(ctx, instance.ID, func(queries *db.Queries) (int64, error) {
		return queries.SetInstanceReason(ctx, db.SetInstanceReasonParams{ID: instance.ID, Reason: reason, FromStatus: statusStopping})
	})

	return err
}

// write runs one compare-and-set update in a transaction and, if it changed
// the row, notifies instance:<id> in the same transaction.
func (t *Teardown) write(ctx context.Context, id string, update func(queries *db.Queries) (int64, error)) (bool, error) {
	tx, err := t.Pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("update instance %s: %w", id, err)
	}

	defer func() { _ = tx.Rollback(ctx) }()

	changed, err := update(db.New(tx))
	if err != nil {
		return false, fmt.Errorf("update instance %s: %w", id, err)
	}

	if changed == 0 {
		return false, nil
	}

	if err := store.Notify(ctx, tx, store.KindInstance, id); err != nil {
		return false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("update instance %s: %w", id, err)
	}

	return true, nil
}

func (t *Teardown) now() time.Time {
	if t.Now == nil {
		return time.Now()
	}

	return t.Now()
}

// isActive reports whether a status is one a stop moves to stopping.
func isActive(status string) bool {
	return status == StatusPending || status == StatusStarting || status == statusReady
}

// namespaceConditions describes the namespace's conditions that are true,
// which say what blocks its deletion.
func namespaceConditions(namespace *corev1.Namespace) string {
	var parts []string

	for _, condition := range namespace.Status.Conditions {
		if condition.Status == corev1.ConditionTrue {
			parts = append(parts, string(condition.Type)+": "+condition.Message)
		}
	}

	if len(parts) == 0 {
		return "no conditions reported"
	}

	return strings.Join(parts, "; ")
}

// update sets the metrics from the rows that aren't stopped and the number
// of stuck namespaces. A nil Metrics does nothing.
func (m *Metrics) update(ctx context.Context, queries *db.Queries, stuck int) error {
	if m == nil {
		return nil
	}

	instances, err := queries.ListInstancesNotStopped(ctx)
	if err != nil {
		return fmt.Errorf("count instances: %w", err)
	}

	m.Instances.Reset()

	for _, instance := range instances {
		m.Instances.WithLabelValues(instance.Scope, instance.Status, revisionLabel(instance.McpRevision)).Inc()
	}

	m.NamespacesStuck.Set(float64(stuck))

	sessions, err := queries.CountOpenSessionsByAttestation(ctx)
	if err != nil {
		return fmt.Errorf("count sessions: %w", err)
	}

	// Both series always exist on the leader, so a scaler reads 0 rather
	// than a missing series.
	open := map[[2]string]int64{}

	for _, search := range []string{"on", "off"} {
		for _, verified := range []string{"true", "false"} {
			open[[2]string{search, verified}] = 0
		}
	}

	for _, row := range sessions {
		search := "off"
		if row.ToolSearch {
			search = "on"
		}

		open[[2]string{search, strconv.FormatBool(row.Attestation == "attested")}] += row.Total
	}

	for labels, total := range open {
		m.Sessions.WithLabelValues(labels[0], labels[1]).Set(float64(total))
	}

	return m.updatePending(ctx, queries)
}

// updatePending sets the pending request gauge and marks this replica as
// the leader. Every kind is always present, so an alert reads 0 rather than
// a missing series.
func (m *Metrics) updatePending(ctx context.Context, queries *db.Queries) error {
	rows, err := queries.CountPendingRequestsByKind(ctx)
	if err != nil {
		return fmt.Errorf("count pending requests: %w", err)
	}

	pending := make(map[string]int64, len(opsmetrics.Kinds))
	for _, row := range rows {
		pending[row.Kind] += row.Total
	}

	for _, kind := range opsmetrics.Kinds {
		m.ApprovalsPending.WithLabelValues(kind).Set(float64(pending[kind]))
	}

	m.Leader.Set(1)

	return nil
}
