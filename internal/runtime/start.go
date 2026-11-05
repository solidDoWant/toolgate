package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cli-utils/pkg/kstatus/status"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/auth"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/telemetry"
	"github.com/solidDoWant/toolgate/internal/upstreamlimit"
)

// FieldManager is the server-side apply field manager of every object the
// reconciler applies.
const FieldManager = "toolgate"

// DefaultStartTimeout is how long an instance may take to become ready when
// its template sets no startTimeout.
const DefaultStartTimeout = 5 * time.Minute

// DefaultToolListTimeout bounds one `initialize` and `tools/list` exchange
// with a starting instance when the Starter sets no ToolListTimeout. An
// upstream that never answers can't hold up the reconciler's pass: the
// attempt is retried on the next pass until startTimeout fails it.
const DefaultToolListTimeout = 10 * time.Second

// NamespacePrefix starts the name of every per-instance namespace.
const NamespacePrefix = "tg-"

// Tool list limits (docs/design/phase-1.md §3).
const (
	MaxToolNameLength = 64
	MaxTools          = 512
	MaxToolListBytes  = 1 << 20
)

// toolListResponseBudget bounds the upstream response bytes one tool
// listing reads, so a huge list is refused without being read whole.
const toolListResponseBudget = MaxToolListBytes + upstreamlimit.Slack

// ErrToolListTooLarge is returned by the MCP tool listers when the
// upstream's `tools/list` responses are larger than the tool list limit.
var ErrToolListTooLarge = errors.New("the tool list is larger than the limit")

// toolListTooLargeReason is the failure reason of an instance whose tool
// list responses were cut off at the budget.
const toolListTooLargeReason = "The server's tool list is more than the limit of 1 MiB."

// maxReasonBytes bounds a reason copied from object conditions and events.
const maxReasonBytes = 1024

// Instance statuses.
const (
	StatusPending  = "pending"
	StatusStarting = "starting"
)

// ToolLister connects to an instance's MCP endpoint, runs `initialize` and
// `tools/list`, and returns the tools.
type ToolLister func(ctx context.Context, endpoint string) ([]*mcp.Tool, error)

// statusKinds are the allowed kinds whose readiness kstatus judges from
// their status. The other allowed kinds are current once they exist.
var statusKinds = []schema.GroupVersionKind{
	{Version: "v1", Kind: "Pod"},
	{Group: "apps", Version: "v1", Kind: "Deployment"},
	{Group: "apps", Version: "v1", Kind: "StatefulSet"},
	{Version: "v1", Kind: "Service"},
	{Version: "v1", Kind: "PersistentVolumeClaim"},
}

// Starter drives instances from pending through starting to ready or
// failed, and fails a ready instance whose objects kstatus reports failed.
type Starter struct {
	Store StartStore
	// Client applies, reads and lists instance objects.
	Client client.Client
	// Template returns the template an instance is started from.
	Template func(key types.NamespacedName) (*v1alpha1.MCPServerTemplate, bool)
	// Secrets returns the secret source for a principal's renders.
	Secrets func(principal string) SecretSource
	// ListTools reaches the instance over MCP.
	ListTools ToolLister
	// ToolListTimeout bounds each ListTools call. Zero means
	// DefaultToolListTimeout.
	ToolListTimeout time.Duration
	// GatewayNamespace is the namespace of the gateway's pods, which the
	// instance NetworkPolicy admits.
	GatewayNamespace string
	// GatewayPodLabels select the gateway's pods in GatewayNamespace.
	GatewayPodLabels map[string]string
	// TracerProvider makes the render and apply spans. Nil means no spans.
	TracerProvider trace.TracerProvider
	// Now is the clock. Nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// tracerName names the tracer of the reconciler's spans.
const tracerName = "github.com/solidDoWant/toolgate/internal/runtime"

func (s *Starter) tracer() trace.Tracer {
	if s.TracerProvider == nil {
		return noop.NewTracerProvider().Tracer(tracerName)
	}

	return s.TracerProvider.Tracer(tracerName)
}

// Step moves one instance row forward, if its status is one the start path
// handles.
func (s *Starter) Step(ctx context.Context, instance db.Instance) error {
	if instance.Desired != "running" {
		return nil
	}

	switch instance.Status {
	case StatusPending:
		return s.startPending(ctx, instance)
	case StatusStarting:
		return s.checkStarting(ctx, instance)
	case StatusReady:
		return s.checkReady(ctx, instance)
	default:
		return nil
	}
}

func (s *Starter) startPending(ctx context.Context, instance db.Instance) error {
	template, found := s.template(instance)
	if !found {
		// The stop path handles a deleted or re-created template.
		return nil
	}

	identity, err := s.identity(ctx, instance)
	if err != nil {
		return err
	}

	// The render and apply spans are children of the enable request that
	// inserted the row.
	ctx = telemetry.DecodeContext(ctx, instance.TraceContext)
	spanAttributes := trace.WithAttributes(attribute.String("toolgate.server", template.Name),
		attribute.String("toolgate.instance", instance.ID))

	_, renderSpan := s.tracer().Start(ctx, "toolgate.render", spanAttributes)

	result, err := Render(ctx, Input{
		Template: template,
		Identity: identity,
		Instance: Instance{ID: instance.ID, Namespace: instance.Namespace},
		Secrets:  s.Secrets(identity.PrincipalID),
	})
	if err != nil {
		renderSpan.SetStatus(codes.Error, "render")
	}

	renderSpan.End()

	if err != nil {
		if ctx.Err() != nil {
			return err
		}

		return s.fail(ctx, instance, StatusPending, "The template didn't render: "+err.Error())
	}

	applied, err := s.applyAll(ctx, instance, template, result, spanAttributes)
	if err != nil || !applied {
		return err
	}

	if _, err := s.Store.MarkStarting(ctx, instance.ID, template.Generation); err != nil {
		return fmt.Errorf("instance %s: %w", instance.ID, err)
	}

	return nil
}

func (s *Starter) checkStarting(ctx context.Context, instance db.Instance) error {
	template, found := s.template(instance)
	if !found {
		return nil
	}

	if s.timedOut(instance, template) {
		reason, err := s.startFailureReason(ctx, instance)
		if err != nil {
			return err
		}

		return s.fail(ctx, instance, StatusStarting, "The server didn't become ready within "+
			startTimeout(template).String()+": "+reason)
	}

	current, reason, err := s.objectsCurrent(ctx, instance)
	if err != nil {
		return err
	}

	if reason != "" {
		return s.fail(ctx, instance, StatusStarting, reason)
	}

	if !current {
		return nil
	}

	tools, err := s.listTools(ctx, EndpointURL(instance, template))
	if errors.Is(err, ErrToolListTooLarge) {
		return s.fail(ctx, instance, StatusStarting, toolListTooLargeReason)
	}

	if err != nil {
		// The server may not be listening yet; startTimeout bounds the wait.
		s.logger().DebugContext(ctx, "instance not answering MCP yet",
			slog.String("instance", instance.ID), slog.String("error", err.Error()))

		return nil
	}

	encoded, reason := CheckToolList(template.Name, tools)
	if reason != "" {
		return s.fail(ctx, instance, StatusStarting, reason)
	}

	if _, err := s.Store.MarkReady(ctx, instance.ID, encoded); err != nil {
		return fmt.Errorf("instance %s: %w", instance.ID, err)
	}

	return nil
}

// checkReady fails a ready instance when kstatus reports one of its
// objects failed. An object that is only in progress, such as a restarting
// pod, is left to the health checks.
func (s *Starter) checkReady(ctx context.Context, instance db.Instance) error {
	if _, found := s.template(instance); !found {
		return nil
	}

	_, reason, err := s.objectsCurrent(ctx, instance)
	if err != nil {
		return err
	}

	if reason != "" {
		return s.fail(ctx, instance, StatusReady, reason)
	}

	return nil
}

// CheckToolList applies the tool list checks to the tools of server. It
// returns the stored form of the list, or a reason when a check fails.
func CheckToolList(server string, tools []*mcp.Tool) ([]byte, string) {
	if len(tools) > MaxTools {
		return nil, fmt.Sprintf("The server lists %d tools, more than the limit of %d.", len(tools), MaxTools)
	}

	for _, tool := range tools {
		if len(server)+len("__")+len(tool.Name) > MaxToolNameLength {
			return nil, fmt.Sprintf("Tool `%s` is longer than %d characters with its `%s__` prefix.",
				truncate(tool.Name, maxReasonBytes/2), MaxToolNameLength, server)
		}
	}

	if tools == nil {
		tools = []*mcp.Tool{}
	}

	encoded, err := json.Marshal(tools)
	if err != nil {
		return nil, "The server's tool list can't be stored: " + err.Error()
	}

	if len(encoded) > MaxToolListBytes {
		return nil, fmt.Sprintf("The server's tool list is %d bytes, more than the limit of 1 MiB.", len(encoded))
	}

	return encoded, ""
}

func (s *Starter) template(instance db.Instance) (*v1alpha1.MCPServerTemplate, bool) {
	template, found := s.Template(types.NamespacedName{Namespace: instance.TemplateNs, Name: instance.TemplateName})
	if !found || string(template.UID) != instance.TemplateUid {
		return nil, false
	}

	return template, true
}

func (s *Starter) identity(ctx context.Context, instance db.Instance) (Identity, error) {
	if !instance.RenderSession.Valid {
		return Identity{PrincipalID: instance.OwnerPrincipal}, nil
	}

	session, err := s.Store.Session(ctx, instance.RenderSession.Int64)
	if err != nil {
		return Identity{}, fmt.Errorf("instance %s: %w", instance.ID, err)
	}

	return Identity{
		PrincipalID:    session.Principal,
		PrincipalEmail: session.Email,
		PrincipalName:  session.Name,
		WorkspaceID:    session.Workspace,
		Attested:       session.Attestation == auth.AttestationAttested,
		SessionID:      session.SessionID,
	}, nil
}

func (s *Starter) timedOut(instance db.Instance, template *v1alpha1.MCPServerTemplate) bool {
	if !instance.AttemptStartedAt.Valid {
		return false
	}

	return !s.now().Before(instance.AttemptStartedAt.Time.Add(startTimeout(template)))
}

func startTimeout(template *v1alpha1.MCPServerTemplate) time.Duration {
	if template.Spec.StartTimeout != nil && template.Spec.StartTimeout.Duration > 0 {
		return template.Spec.StartTimeout.Duration
	}

	return DefaultStartTimeout
}

// prepareNamespace creates the instance namespace and applies what every
// instance namespace holds before the template's objects. It reports false
// when the instance has failed because the namespace exists without the
// instance's label.
func (s *Starter) prepareNamespace(ctx context.Context, instance db.Instance, template *v1alpha1.MCPServerTemplate) (bool, error) {
	objectLabels := map[string]string{
		LabelManagedBy: ManagedByValue,
		LabelInstance:  instance.ID,
		LabelTemplate:  template.Name,
	}

	namespaceLabels := maps.Clone(objectLabels)
	namespaceLabels[LabelWritable] = "true"
	namespaceLabels["pod-security.kubernetes.io/enforce"] = "restricted"
	namespaceLabels["pod-security.kubernetes.io/warn"] = "restricted"
	namespaceLabels["pod-security.kubernetes.io/audit"] = "restricted"

	if instance.OwnerPrincipal != "" {
		namespaceLabels[LabelOwner] = hashClaimed(instance.OwnerPrincipal)
	}

	dnsPort := intstr.FromInt32(53)
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP

	// The gateway's RBAC grants namespaces create but not patch, so the
	// namespace is created rather than applied. A new leader repeating the
	// step finds it already there and goes on, but only when it carries the
	// instance's label: toolgate never adopts a namespace it didn't create.
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instance.Namespace, Labels: namespaceLabels}}
	if err := s.Client.Create(ctx, namespace); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return false, fmt.Errorf("create Namespace %s: %w", instance.Namespace, err)
		}

		existing := &corev1.Namespace{}
		if err := s.Client.Get(ctx, client.ObjectKey{Name: instance.Namespace}, existing); err != nil {
			return false, fmt.Errorf("get Namespace %s: %w", instance.Namespace, err)
		}

		if existing.Labels[LabelInstance] != instance.ID {
			return false, s.fail(ctx, instance, StatusPending, fmt.Sprintf(
				"The namespace %s already exists and doesn't carry the label %s=%s, so toolgate won't use it.",
				instance.Namespace, LabelInstance, instance.ID))
		}
	}

	objects := []k8sruntime.Object{
		&corev1.ServiceAccount{
			TypeMeta:                     metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
			ObjectMeta:                   metav1.ObjectMeta{Name: "default", Namespace: instance.Namespace},
			AutomountServiceAccountToken: ptr.To(false),
		},
		&networkingv1.NetworkPolicy{
			TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
			ObjectMeta: metav1.ObjectMeta{
				Name: "toolgate-default-deny", Namespace: instance.Namespace, Labels: objectLabels,
			},
			Spec: networkingv1.NetworkPolicySpec{
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
							"kubernetes.io/metadata.name": s.GatewayNamespace,
						}},
						PodSelector: &metav1.LabelSelector{MatchLabels: maps.Clone(s.GatewayPodLabels)},
					}},
				}},
				Egress: []networkingv1.NetworkPolicyEgressRule{{
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &udp, Port: &dnsPort},
						{Protocol: &tcp, Port: &dnsPort},
					},
				}},
			},
		},
	}

	for _, object := range objects {
		content, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(object)
		if err != nil {
			return false, fmt.Errorf("instance %s: %w", instance.ID, err)
		}

		// Server-side apply sends only the fields set here, never an empty
		// status or creation time.
		unstructured.RemoveNestedField(content, "status")
		unstructured.RemoveNestedField(content, "metadata", "creationTimestamp")

		if err := s.apply(ctx, &unstructured.Unstructured{Object: content}); err != nil {
			return false, err
		}
	}

	return true, nil
}

// applyAll prepares the instance's own namespace, if it has one, and applies
// the rendered objects inside a toolgate.apply span. It reports false,
// with no error, when the namespace isn't prepared yet: the row then stays
// pending.
func (s *Starter) applyAll(ctx context.Context, instance db.Instance, template *v1alpha1.MCPServerTemplate,
	result *Result, spanAttributes trace.SpanStartEventOption,
) (bool, error) {
	ctx, span := s.tracer().Start(ctx, "toolgate.apply", spanAttributes)
	defer span.End()

	if hasOwnNamespace(instance) {
		prepared, err := s.prepareNamespace(ctx, instance, template)
		if err != nil {
			span.SetStatus(codes.Error, "namespace")

			return false, err
		}

		if !prepared {
			return false, nil
		}
	}

	var owner *metav1.OwnerReference

	if !hasOwnNamespace(instance) {
		anchor, err := s.prepareAnchor(ctx, instance, template)
		if err != nil {
			span.SetStatus(codes.Error, "anchor")

			return false, err
		}

		if anchor == nil {
			return false, nil
		}

		owner = anchor
	}

	for _, object := range result.Objects {
		if owner != nil {
			object.SetOwnerReferences(append(object.GetOwnerReferences(), *owner))
		}

		if err := s.apply(ctx, object); err != nil {
			span.SetStatus(codes.Error, "apply")

			// RBAC (or the admission policy) refusing an object won't change
			// on a retry: the instance fails with the API server's message
			// (docs/design/phase-2.md §7).
			if apierrors.IsForbidden(err) {
				return false, s.fail(ctx, instance, StatusPending, "The API server refused "+object.GetKind()+" "+
					object.GetName()+": "+apiServerMessage(err))
			}

			return false, err
		}
	}

	return true, nil
}

// prepareAnchor creates the anchor ConfigMap of an instance in the shared
// namespace and returns an owner reference to it. Every object applied for
// the instance is owned by the anchor, so deleting the anchor lets
// Kubernetes' garbage collector delete them all, Secrets included, without
// the gateway reading Secrets (docs/design/phase-1.md §7). It returns nil,
// with no error, when the instance has failed because a ConfigMap of that
// name exists without the instance's label.
func (s *Starter) prepareAnchor(ctx context.Context, instance db.Instance, template *v1alpha1.MCPServerTemplate) (*metav1.OwnerReference, error) {
	anchor := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Namespace: instance.Namespace,
		Name:      AnchorName(instance.ID),
		Labels: map[string]string{
			LabelManagedBy: ManagedByValue,
			LabelInstance:  instance.ID,
			LabelTemplate:  template.Name,
		},
	}}

	if err := s.Client.Create(ctx, anchor); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create ConfigMap %s/%s: %w", anchor.Namespace, anchor.Name, err)
		}

		if err := s.Client.Get(ctx, client.ObjectKeyFromObject(anchor), anchor); err != nil {
			return nil, fmt.Errorf("get ConfigMap %s/%s: %w", anchor.Namespace, anchor.Name, err)
		}

		if anchor.Labels[LabelInstance] != instance.ID {
			return nil, s.fail(ctx, instance, StatusPending, fmt.Sprintf(
				"The ConfigMap %s/%s already exists and doesn't carry the label %s=%s, so toolgate won't use it.",
				anchor.Namespace, anchor.Name, LabelInstance, instance.ID))
		}
	}

	return &metav1.OwnerReference{
		APIVersion:         "v1",
		Kind:               "ConfigMap",
		Name:               anchor.Name,
		UID:                anchor.UID,
		BlockOwnerDeletion: ptr.To(false),
	}, nil
}

// apiServerMessage returns the message of the API server's status in err,
// or err's text when it carries none.
func apiServerMessage(err error) string {
	var status apierrors.APIStatus
	if errors.As(err, &status) && status.Status().Message != "" {
		return status.Status().Message
	}

	return err.Error()
}

// AnchorName is the name of the anchor ConfigMap that owns the objects of
// an instance in the shared namespace.
func AnchorName(id string) string {
	return NamespacePrefix + id
}

func (s *Starter) apply(ctx context.Context, object *unstructured.Unstructured) error {
	err := s.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(object),
		client.FieldOwner(FieldManager), client.ForceOwnership)
	if err != nil {
		return fmt.Errorf("apply %s %s/%s: %w", object.GetKind(), object.GetNamespace(), object.GetName(), err)
	}

	return nil
}

// objectsCurrent reports whether every status-bearing object of the
// instance is current by kstatus, or the reason when one has failed.
func (s *Starter) objectsCurrent(ctx context.Context, instance db.Instance) (bool, string, error) {
	current := true

	for _, kind := range statusKinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(kind.GroupVersion().WithKind(kind.Kind + "List"))

		if err := s.Client.List(ctx, list, client.InNamespace(instance.Namespace),
			client.MatchingLabels{LabelInstance: instance.ID}); err != nil {
			return false, "", fmt.Errorf("list %s in %s: %w", kind.Kind, instance.Namespace, err)
		}

		for index := range list.Items {
			object := &list.Items[index]

			result, err := status.Compute(object)
			if err != nil {
				return false, "", fmt.Errorf("status of %s %s/%s: %w", kind.Kind, object.GetNamespace(), object.GetName(), err)
			}

			switch result.Status {
			case status.CurrentStatus:
			case status.FailedStatus:
				return false, truncate(fmt.Sprintf("%s %s failed: %s", kind.Kind, object.GetName(), result.Message), maxReasonBytes), nil
			default:
				current = false
			}
		}
	}

	return current, "", nil
}

// startFailureReason explains why an instance didn't start: the waiting
// state of its pods' containers, or else the namespace's latest Warning
// event.
func (s *Starter) startFailureReason(ctx context.Context, instance db.Instance) (string, error) {
	pods := &corev1.PodList{}
	if err := s.Client.List(ctx, pods, client.InNamespace(instance.Namespace),
		client.MatchingLabels{LabelInstance: instance.ID}); err != nil {
		return "", fmt.Errorf("list pods in %s: %w", instance.Namespace, err)
	}

	for _, pod := range pods.Items {
		statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
		for _, container := range statuses {
			waiting := container.State.Waiting
			if waiting == nil || waiting.Reason == "" || waiting.Reason == "ContainerCreating" || waiting.Reason == "PodInitializing" {
				continue
			}

			reason := fmt.Sprintf("container %s of pod %s is waiting: %s", container.Name, pod.Name, waiting.Reason)
			if waiting.Message != "" {
				reason += ": " + waiting.Message
			}

			return truncate(reason, maxReasonBytes), nil
		}
	}

	events := &corev1.EventList{}
	if err := s.Client.List(ctx, events, client.InNamespace(instance.Namespace)); err != nil {
		return "", fmt.Errorf("list events in %s: %w", instance.Namespace, err)
	}

	var latest *corev1.Event

	for index := range events.Items {
		event := &events.Items[index]
		if event.Type != corev1.EventTypeWarning {
			continue
		}

		if latest == nil || eventTime(event).After(eventTime(latest)) {
			latest = event
		}
	}

	if latest == nil {
		return "no pod reported a reason", nil
	}

	return truncate(fmt.Sprintf("%s %s: %s: %s", latest.InvolvedObject.Kind, latest.InvolvedObject.Name,
		latest.Reason, latest.Message), maxReasonBytes), nil
}

func eventTime(event *corev1.Event) time.Time {
	switch {
	case !event.LastTimestamp.IsZero():
		return event.LastTimestamp.Time
	case !event.EventTime.IsZero():
		return event.EventTime.Time
	default:
		return event.CreationTimestamp.Time
	}
}

func (s *Starter) fail(ctx context.Context, instance db.Instance, from, reason string) error {
	if _, err := s.Store.MarkFailed(ctx, instance.ID, from, truncate(reason, maxReasonBytes)); err != nil {
		return fmt.Errorf("instance %s: %w", instance.ID, err)
	}

	return nil
}

// EndpointURL returns the MCP URL of an instance started from template: the
// template's endpoint Service in the instance's namespace.
func EndpointURL(instance db.Instance, template *v1alpha1.MCPServerTemplate) string {
	endpoint := template.Spec.Endpoint

	service, err := RenderEndpointService(template, Instance{ID: instance.ID, Namespace: instance.Namespace})
	if err != nil {
		// A name that doesn't render can't be reached; the start timeout
		// reports it.
		service = endpoint.Service
	}

	host := net.JoinHostPort(service+"."+instance.Namespace+".svc", strconv.Itoa(int(endpoint.Port)))

	path := endpoint.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	return "http://" + host + path
}

// truncate cuts text to at most limit bytes, on a UTF-8 boundary.
func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}

	cut := limit
	for cut > 0 && !utf8RuneStart(text[cut]) {
		cut--
	}

	return text[:cut]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

func (s *Starter) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}

	return time.Now()
}

// listTools calls ListTools with a deadline of ToolListTimeout.
func (s *Starter) listTools(ctx context.Context, endpoint string) ([]*mcp.Tool, error) {
	timeout := s.ToolListTimeout
	if timeout == 0 {
		timeout = DefaultToolListTimeout
	}

	listCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return s.ListTools(listCtx, endpoint)
}

func (s *Starter) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}

	return slog.Default()
}

// MCPToolLister is the ToolLister that speaks MCP over Streamable HTTP.
func MCPToolLister(ctx context.Context, endpoint string) ([]*mcp.Tool, error) {
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "toolgate", Version: "0"}, nil)

	ctx, budget := upstreamlimit.WithBudget(ctx, toolListResponseBudget)

	session, err := mcpClient.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, DisableStandaloneSSE: true, HTTPClient: toolListHTTPClient,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("initialize %s: %w", endpoint, toolListError(err, budget))
	}
	defer func() { _ = session.Close() }()

	var tools []*mcp.Tool

	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("tools/list %s: %w", endpoint, toolListError(err, budget))
		}

		tools = append(tools, tool)
		if len(tools) > MaxTools {
			break
		}
	}

	return tools, nil
}

// toolListHTTPClient carries the MCP tool listers' requests. A request
// with no budget of its own, such as a ping, is bounded like a tool list.
var toolListHTTPClient = upstreamlimit.NewClient(http.DefaultClient, toolListResponseBudget)

// toolListError returns ErrToolListTooLarge for an error caused by the
// budget running out, and err otherwise.
func toolListError(err error, budget *upstreamlimit.Budget) error {
	if budget.Exceeded() {
		return fmt.Errorf("%w: %w", ErrToolListTooLarge, err)
	}

	return err
}
