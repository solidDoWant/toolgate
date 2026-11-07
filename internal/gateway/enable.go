package gateway

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/types"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/policy"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
	"github.com/solidDoWant/toolgate/internal/telemetry"
)

// MaxEnabledServers is the number of servers one session may have enabled.
const MaxEnabledServers = 32

// progressInterval is the longest time a waiting enable goes without a
// progress notification and without re-reading its rows.
const progressInterval = 10 * time.Second

// defaultStartTimeout is how long an enable waits when its template sets no
// startTimeout. It matches the reconciler's default.
const defaultStartTimeout = 5 * time.Minute

// instanceNamespacePrefix starts the name of every per-instance namespace.
const instanceNamespacePrefix = "tg-"

// EnableResult is the toolgate_enable_server result.
type EnableResult struct {
	Name string `json:"name"`
	// Tools are the server's tools as this session sees them.
	Tools []string `json:"tools"`
}

// DisableResult is the toolgate_disable_server result.
type DisableResult struct {
	Name string `json:"name"`
	// Enabled is always false: the server is no longer enabled.
	Enabled bool `json:"enabled"`
}

const (
	sqlStateUniqueViolation      = "23505"
	sqlStateSerializationFailure = "40001"
)

func (m *metaTools) enableServer(ctx context.Context, request *mcp.CallToolRequest, input ServerNameInput) (*mcp.CallToolResult, EnableResult, error) {
	current, ok := requestContextOf(request)
	if !ok {
		return nil, EnableResult{}, errNoIdentity
	}

	template, err := m.catalog.Offered(input.Name)
	if err != nil {
		return nil, EnableResult{}, notOffered(input.Name, err)
	}

	// Step 1: policy, from this request's own identity.
	state, err := m.policyState(ctx, current.identity)
	if err != nil {
		return nil, EnableResult{}, err
	}

	decision := m.decide(ctx, policy.Request{Identity: current.identity, Template: template, State: state})

	// In a verified workspace, a refusal by the workspace-scope step alone
	// asks the user for a session addition.
	requestID := ""

	if m.asksForAddition(current, decision) {
		id, result, err := m.requestSessionAddition(ctx, additionEnable{current: current, request: request, template: template})
		if result != nil || err != nil {
			return result, EnableResult{}, err
		}

		requestID, decision = id, policy.Decision{Allowed: true}
	}

	if !decision.Allowed {
		if decision.Step == policy.StepAccess {
			return nil, EnableResult{}, notInCatalog(input.Name)
		}

		if err := m.recordRefusal(ctx, current, template, "server.enable", decision); err != nil {
			return nil, EnableResult{}, err
		}

		return nil, EnableResult{}, errors.New(decision.Reason)
	}

	// Step 2: every secret the template names must be set.
	if err := m.checkSecrets(ctx, current, template); err != nil {
		return nil, EnableResult{}, err
	}

	lazy, err := m.startsOnCall(ctx, template)
	if err != nil {
		return nil, EnableResult{}, err
	}

	// Steps 3 and 4.
	instance, err := m.enableInTransaction(ctx, current, template, enableOptions{requestID: requestID, lazy: lazy})

	var limitErr *instanceLimitError
	if errors.As(err, &limitErr) {
		// The transaction rolled back, so nothing is enabled.
		recorder := audit.NewRecorder(m.auditOutput, db.New(m.database))
		if err := recorder.Record(ctx, auditEvent(current, "server.enable", template.Name, map[string]any{
			"decision": "refuse", "reason": "limit", "limit": limitErr.limit,
		})); err != nil {
			return nil, EnableResult{}, err
		}

		return nil, EnableResult{}, limitErr
	}

	if err != nil {
		return nil, EnableResult{}, err
	}

	// An `onCall` server with a catalog list doesn't wait: its instance, if
	// any, starts in the background and the first call waits for it.
	if lazy {
		names, err := m.syncSession(ctx, request.Session, current)
		if err != nil {
			return nil, EnableResult{}, err
		}

		return nil, EnableResult{Name: template.Name, Tools: names[template.Name]}, nil
	}

	// Step 5.
	waitCtx, enableSpan := tracer(ctx).Start(ctx, "toolgate.enable",
		trace.WithAttributes(attribute.String("toolgate.server", template.Name)))
	instance, err = m.waitForInstance(waitCtx, request, current, template, instance)

	enableSpan.End()

	if err != nil {
		return nil, EnableResult{}, err
	}

	// Step 7.
	if instance.Status != statusReady {
		reason := instance.Reason
		if reason == "" {
			reason = "it stopped before it became ready"
		}

		return nil, EnableResult{}, fmt.Errorf("`%s` failed to start: %s", template.Name, reason)
	}

	if instance.UnhealthySince.Valid {
		return nil, EnableResult{}, errors.New(unhealthyMessage(template.Name, instance))
	}

	// Step 6: decide again, now that the instance is ready, with fresh
	// grants and temporary blocks.
	state, err = m.policyState(ctx, current.identity)
	if err != nil {
		return nil, EnableResult{}, err
	}

	decision = m.decide(ctx, policy.Request{Identity: current.identity, Template: template, State: state})
	if !decision.Allowed {
		if err := m.disableInTransaction(ctx, current, template, decision); err != nil {
			return nil, EnableResult{}, err
		}

		return nil, EnableResult{}, errors.New(decision.Reason)
	}

	names, err := m.syncSession(ctx, request.Session, current)
	if err != nil {
		return nil, EnableResult{}, err
	}

	return nil, EnableResult{Name: template.Name, Tools: names[template.Name]}, nil
}

func (m *metaTools) disableServer(ctx context.Context, request *mcp.CallToolRequest, input ServerNameInput) (*mcp.CallToolResult, DisableResult, error) {
	current, ok := requestContextOf(request)
	if !ok {
		return nil, DisableResult{}, errNoIdentity
	}

	result := DisableResult{Name: input.Name}

	template, found := m.enabledTemplateNamed(ctx, current, input.Name)
	if !found {
		// Disabling a server that isn't enabled changes nothing.
		return nil, result, nil
	}

	if err := m.disableInTransaction(ctx, current, template, policy.Decision{Allowed: true}); err != nil {
		return nil, DisableResult{}, err
	}

	if _, err := m.syncSession(ctx, request.Session, current); err != nil {
		return nil, DisableResult{}, err
	}

	return nil, result, nil
}

// enabledTemplateNamed returns the template of the session's enabled server
// called name. A disable must work even for a template that is no longer
// offered, so it looks at the enabled set, not only at the catalog.
func (m *metaTools) enabledTemplateNamed(ctx context.Context, current requestContext, name string) (*v1alpha1.MCPServerTemplate, bool) {
	rows, err := db.New(m.database).ListSessionServers(ctx, current.session.ID)
	if err != nil {
		return nil, false
	}

	for _, row := range rows {
		if row.TemplateName != name {
			continue
		}

		key := types.NamespacedName{Namespace: row.TemplateNs, Name: row.TemplateName}
		if template, found := m.catalog.Running(key); found {
			return template, true
		}

		// The template is gone: only its key is needed to delete the row.
		template := &v1alpha1.MCPServerTemplate{}
		template.Namespace, template.Name = key.Namespace, key.Name

		return template, true
	}

	return nil, false
}

func (m *metaTools) checkSecrets(ctx context.Context, current requestContext, template *v1alpha1.MCPServerTemplate) error {
	needed := catalog.SecretNames(template)
	if len(needed) == 0 {
		return nil
	}

	stored, err := db.New(m.database).ListUserSecretNames(ctx, current.identity.Principal)
	if err != nil {
		return fmt.Errorf("read secret names: %w", err)
	}

	set := make(map[string]bool, len(stored))
	for _, row := range stored {
		set[row.Name] = true
	}

	for _, name := range needed {
		if !set[name] {
			return fmt.Errorf("Secret `%s` isn't set. Run `toolgate secret set %s`.", name, name) //nolint:staticcheck // A sentence for the model.
		}
	}

	return nil
}

// enableOptions are the parts of an enable that differ between an enable
// tool call and a call that enables its server.
type enableOptions struct {
	// requestID is the approved session addition, if any.
	requestID string
	// reason is written to server.enable, such as first-call.
	reason string
	// lazy is true for an `onCall` server with a catalog list.
	lazy bool
}

// enableInTransaction runs Enable steps 3 and 4: it adds the server to the
// session's enabled set and returns the live instance, inserting a pending
// one when there is none. requestID, when set, is the session addition's
// access request, recorded with the allow. A serialization failure or
// unique violation is retried once.
func (m *metaTools) enableInTransaction(ctx context.Context, current requestContext, template *v1alpha1.MCPServerTemplate, options enableOptions) (db.Instance, error) {
	var instance db.Instance

	attempt := func() error {
		return pgx.BeginFunc(ctx, m.database, func(tx pgx.Tx) error {
			var err error

			instance, err = m.enableTx(ctx, tx, current, template, options)

			return err
		})
	}

	err := attempt()
	if retryable(err) {
		err = attempt()
	}

	return instance, err
}

func (m *metaTools) enableTx(ctx context.Context, tx pgx.Tx, current requestContext, template *v1alpha1.MCPServerTemplate, options enableOptions) (db.Instance, error) {
	queries := db.New(tx)

	if err := m.lockSession(ctx, tx, current); err != nil {
		return db.Instance{}, err
	}

	enabled, err := m.isEnabled(ctx, queries, current.session.ID, template)
	if err != nil {
		return db.Instance{}, err
	}

	// A call that enables its server finds it already enabled when another
	// call of the session got there first: that call wrote server.enable.
	if enabled && options.reason == reasonFirstCall {
		return db.Instance{}, nil
	}

	if !enabled {
		count, err := queries.CountSessionServers(ctx, current.session.ID)
		if err != nil {
			return db.Instance{}, fmt.Errorf("count enabled servers: %w", err)
		}

		if count >= MaxEnabledServers {
			return db.Instance{}, errTooManyServers
		}
	}

	if err := queries.EnableServer(ctx, db.EnableServerParams{
		SessionPk: current.session.ID, TemplateNs: template.Namespace, TemplateName: template.Name,
	}); err != nil {
		return db.Instance{}, fmt.Errorf("enable server: %w", err)
	}

	// The built-in server never has an instance.
	var instance db.Instance

	switch {
	case isBuiltin(template):
	case options.lazy && options.reason == reasonFirstCall:
		// A call's implicit enable never retries a failed start: the call
		// itself refuses or starts the instance. Only an explicit enable
		// retries.
	case options.lazy:
		instance, err = m.retryInstance(ctx, queries, current, template)
	default:
		instance, err = m.liveOrNewInstance(ctx, queries, current, template)
	}

	if err != nil {
		return db.Instance{}, err
	}

	details := map[string]any{"decision": "allow"}
	if instance.ID != "" {
		details["instance"] = instance.ID
	}

	if options.requestID != "" {
		details[sessionAdditionRequest] = options.requestID
	}

	if options.reason != "" {
		details["reason"] = options.reason
	}

	return instance, m.recordEnable(ctx, tx, current, template, instance, details)
}

// liveOrNewInstance runs the insert part of Enable step 4: under the
// (template, scope key) advisory lock it returns the live instance, or
// inserts a pending one within the instance limits.
func (m *metaTools) liveOrNewInstance(ctx context.Context, queries *db.Queries, current requestContext, template *v1alpha1.MCPServerTemplate) (db.Instance, error) {
	key := scopeKey(template.Spec.Scope, current.identity)

	if err := queries.LockInstanceScope(ctx, db.LockInstanceScopeParams{
		TemplateNs: template.Namespace, TemplateName: template.Name, ScopeKey: key,
	}); err != nil {
		return db.Instance{}, fmt.Errorf("lock instance scope: %w", err)
	}

	instance, err := queries.GetLiveInstance(ctx, db.GetLiveInstanceParams{
		TemplateNs: template.Namespace, TemplateName: template.Name, ScopeKey: key,
	})

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if err := m.checkInstanceLimits(ctx, queries, current, template); err != nil {
			return db.Instance{}, err
		}

		instance, err = insertInstance(ctx, queries, current, template, key, m.sharedNamespace)
		if err != nil {
			return db.Instance{}, err
		}
	case err != nil:
		return db.Instance{}, fmt.Errorf("read live instance: %w", err)
	}

	return instance, nil
}

// recordEnable writes server.enable with details and notifies the session
// and the instance.
func (m *metaTools) recordEnable(ctx context.Context, tx pgx.Tx, current requestContext, template *v1alpha1.MCPServerTemplate, instance db.Instance, details map[string]any) error {
	recorder := audit.NewRecorder(m.auditOutput, db.New(tx))
	if err := recorder.Record(ctx, auditEvent(current, "server.enable", template.Name, details)); err != nil {
		return err
	}

	if err := store.Notify(ctx, tx, store.KindSession, strconv.FormatInt(current.session.ID, 10)); err != nil {
		return err
	}

	if instance.ID == "" {
		return nil
	}

	return store.Notify(ctx, tx, store.KindInstance, instance.ID)
}

// startsOnCall reports whether template's instance starts on the first call
// rather than on enable: its start is `onCall` (the default) and it has a
// catalog list.
func (m *metaTools) startsOnCall(ctx context.Context, template *v1alpha1.MCPServerTemplate) (bool, error) {
	if template.Spec.Start == catalog.StartOnEnable || template.UID == "" {
		return false, nil
	}

	_, found, err := m.catalogList(ctx, template)

	return found, err
}

// retryInstance is the enable of an `onCall` server: it inserts an instance
// only when the newest one failed and none is live, and returns the zero
// instance otherwise. An enable after a failure is how a user retries.
func (m *metaTools) retryInstance(ctx context.Context, queries *db.Queries, current requestContext, template *v1alpha1.MCPServerTemplate) (db.Instance, error) {
	key := scopeKey(template.Spec.Scope, current.identity)
	params := db.GetLiveInstanceParams{TemplateNs: template.Namespace, TemplateName: template.Name, ScopeKey: key}

	if err := queries.LockInstanceScope(ctx, db.LockInstanceScopeParams(params)); err != nil {
		return db.Instance{}, fmt.Errorf("lock instance scope: %w", err)
	}

	live, err := queries.GetLiveInstance(ctx, params)
	if err == nil {
		return live, nil
	}

	if !errors.Is(err, pgx.ErrNoRows) {
		return db.Instance{}, fmt.Errorf("read live instance: %w", err)
	}

	newest, err := queries.GetNewestInstance(ctx, db.GetNewestInstanceParams(params))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !newest.FailedAt.Valid) {
		return db.Instance{}, nil
	}

	if err != nil {
		return db.Instance{}, fmt.Errorf("read newest instance: %w", err)
	}

	if err := m.checkInstanceLimits(ctx, queries, current, template); err != nil {
		return db.Instance{}, err
	}

	return insertInstance(ctx, queries, current, template, key, m.sharedNamespace)
}

var errTooManyServers = fmt.Errorf("This session already has %d servers enabled. Disable one with `%s` first.", MaxEnabledServers, ToolDisableServer) //nolint:staticcheck // A sentence for the model.

// instanceLimitError refuses an enable that needs a new instance when the
// user or the gateway already runs its limit of instances.
type instanceLimitError struct {
	// limit is "per-user" or "gateway".
	limit   string
	message string
}

func (e *instanceLimitError) Error() string { return e.message }

// checkInstanceLimits takes the gateway-wide insert lock, then counts live
// instances against the per-user and the gateway limits.
// Only the insert path calls
// it, after the (template, scope key) lock.
func (m *metaTools) checkInstanceLimits(ctx context.Context, queries *db.Queries, current requestContext, template *v1alpha1.MCPServerTemplate) error {
	if err := queries.LockInstanceInsert(ctx); err != nil {
		return fmt.Errorf("lock instance insert: %w", err)
	}

	if template.Spec.Scope != catalog.ScopeShared {
		owned, err := queries.CountLiveInstancesOfOwner(ctx, current.identity.Principal)
		if err != nil {
			return fmt.Errorf("count the user's instances: %w", err)
		}

		if owned >= int64(m.instanceLimitPerUser) {
			return &instanceLimitError{limit: "per-user", message: fmt.Sprintf(
				"You already have %d servers running, the limit for each user. Disable servers you no longer need, or wait for idle ones to stop.",
				m.instanceLimitPerUser)}
		}
	}

	total, err := queries.CountLiveInstances(ctx)
	if err != nil {
		return fmt.Errorf("count instances: %w", err)
	}

	if total >= int64(m.instanceLimit) {
		return &instanceLimitError{limit: "gateway", message: fmt.Sprintf(
			"The gateway is already running its limit of %d servers. Try again later, or ask an admin to raise `limits.instances`.",
			m.instanceLimit)}
	}

	return nil
}

func insertInstance(ctx context.Context, queries *db.Queries, current requestContext, template *v1alpha1.MCPServerTemplate, key, sharedNamespace string) (db.Instance, error) {
	generated, err := uuid.NewV7()
	if err != nil {
		return db.Instance{}, fmt.Errorf("instance ID: %w", err)
	}

	id := hex.EncodeToString(generated[:])

	owner := current.identity.Principal
	if template.Spec.Scope == catalog.ScopeShared {
		owner = ""
	}

	// A `namespace: shared` template is valid only while the gateway has a
	// shared namespace, so the catalog offers none without it.
	namespace := instanceNamespacePrefix + id
	if template.Spec.Namespace == catalog.NamespaceShared {
		namespace = sharedNamespace
	}

	instance, err := queries.InsertInstance(ctx, db.InsertInstanceParams{
		ID:             id,
		TemplateNs:     template.Namespace,
		TemplateName:   template.Name,
		TemplateUid:    string(template.UID),
		Generation:     template.Generation,
		Scope:          template.Spec.Scope,
		ScopeKey:       key,
		OwnerPrincipal: owner,
		RenderSession:  pgtype.Int8{Int64: current.session.ID, Valid: true},
		Namespace:      namespace,
		TraceContext:   telemetry.EncodeContext(ctx),
	})
	if err != nil {
		return db.Instance{}, fmt.Errorf("insert instance: %w", err)
	}

	return instance, nil
}

// lockSession takes the sessions row lock and restarts the session when it
// ended since the request resolved it, deleting
// the enabled set the ended session kept.
func (m *metaTools) lockSession(ctx context.Context, tx pgx.Tx, current requestContext) error {
	queries := db.New(tx)

	locked, err := queries.LockSession(ctx, current.session.ID)
	if err != nil {
		return fmt.Errorf("lock session: %w", err)
	}

	if !locked.EndedAt.Valid {
		return nil
	}

	if _, err := queries.RestartSession(ctx, db.RestartSessionParams{
		ID:          current.session.ID,
		Workspace:   current.identity.Workspace,
		Attestation: current.identity.Attestation,
		Kind:        current.identity.Kind,
	}); err != nil {
		return fmt.Errorf("restart session: %w", err)
	}

	if err := queries.DeleteSessionServers(ctx, current.session.ID); err != nil {
		return fmt.Errorf("delete enabled servers: %w", err)
	}

	return audit.NewRecorder(m.auditOutput, queries).Record(ctx, auditEvent(current, "session.start", "", nil))
}

// disableInTransaction deletes the session's session_servers row, writes
// server.disable and notifies session:<id>. A refused decision is recorded
// as the reason (Enable step 6).
func (m *metaTools) disableInTransaction(ctx context.Context, current requestContext, template *v1alpha1.MCPServerTemplate, decision policy.Decision) error {
	details := map[string]any{}
	if !decision.Allowed {
		details = refusalDetails(decision)
	}

	return m.removeServer(ctx, m.database, current, template, details)
}

// beginner starts a transaction: a pool, or a transaction that starts a
// savepoint.
type beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// removeServer deletes the session's session_servers row, writes
// server.disable with details and notifies session:<id>, in a transaction
// started on database.
func (m *metaTools) removeServer(ctx context.Context, database beginner, current requestContext, template *v1alpha1.MCPServerTemplate, details map[string]any) error {
	return pgx.BeginFunc(ctx, database, func(tx pgx.Tx) error {
		queries := db.New(tx)

		if err := m.lockSession(ctx, tx, current); err != nil {
			return err
		}

		if _, err := queries.DisableServer(ctx, db.DisableServerParams{
			SessionPk: current.session.ID, TemplateNs: template.Namespace, TemplateName: template.Name,
		}); err != nil {
			return fmt.Errorf("disable server: %w", err)
		}

		if err := audit.NewRecorder(m.auditOutput, queries).Record(ctx, auditEvent(current, "server.disable", template.Name, details)); err != nil {
			return err
		}

		return store.Notify(ctx, tx, store.KindSession, strconv.FormatInt(current.session.ID, 10))
	})
}

func (m *metaTools) recordRefusal(ctx context.Context, current requestContext, template *v1alpha1.MCPServerTemplate, eventType string, decision policy.Decision) error {
	recorder := audit.NewRecorder(m.auditOutput, db.New(m.database))

	return recorder.Record(ctx, auditEvent(current, eventType, template.Name, refusalDetails(decision)))
}

// refusalReason is the reason a refusal reports: a denial names the
// denied reason, not its detail.
func refusalReason(decision policy.Decision) string {
	if decision.Denied != "" {
		return policy.ReasonDenied
	}

	return decision.Reason
}

func refusalDetails(decision policy.Decision) map[string]any {
	details := map[string]any{"decision": "refuse", "step": decision.Step, "reason": refusalReason(decision)}

	if decision.Rule != "" {
		details["rule"] = decision.Rule
	}

	if decision.Grant != "" {
		details["grant"] = decision.Grant
	}

	if decision.Block != "" {
		details["block"] = decision.Block
	}

	return details
}

func auditEvent(current requestContext, eventType, server string, details map[string]any) audit.Event {
	identity := current.identity

	return audit.Event{
		Type:        eventType,
		Principal:   identity.Principal,
		Workspace:   identity.Workspace,
		Session:     identity.Session,
		Attestation: identity.Attestation,
		Server:      server,
		Details:     details,
	}
}

func (m *metaTools) isEnabled(ctx context.Context, queries *db.Queries, sessionPK int64, template *v1alpha1.MCPServerTemplate) (bool, error) {
	rows, err := queries.ListSessionServers(ctx, sessionPK)
	if err != nil {
		return false, fmt.Errorf("read enabled servers: %w", err)
	}

	for _, row := range rows {
		if row.TemplateNs == template.Namespace && row.TemplateName == template.Name {
			return true, nil
		}
	}

	return false, nil
}

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}

	return pgErr.Code == sqlStateUniqueViolation || pgErr.Code == sqlStateSerializationFailure
}

func notOffered(name string, err error) error {
	var invalid *catalog.InvalidError
	if errors.As(err, &invalid) {
		return fmt.Errorf("Server `%s` can't be enabled: %s", name, invalid.Reason) //nolint:staticcheck // A sentence for the model.
	}

	return notInCatalog(name)
}

func notInCatalog(name string) error {
	return fmt.Errorf("Server `%s` isn't in your catalog. Call `%s` to see the servers you can use.", name, ToolListServers) //nolint:staticcheck // A sentence for the model.
}
