package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/audit"
	"github.com/solidDoWant/toolgate/internal/catalog"
	"github.com/solidDoWant/toolgate/internal/credentials"
	"github.com/solidDoWant/toolgate/internal/store"
	"github.com/solidDoWant/toolgate/internal/store/db"
)

// heldClaims collects the instances a request inserted with their exchange
// rows already claimed, so the request runs those exchanges after its
// transaction commits. An insert whose transaction rolled back leaves no
// row, and runHeldClaims skips it.
type heldClaims struct {
	mutex     sync.Mutex
	instances []string
}

type heldClaimsKey struct{}

// withHeldClaims returns a context whose instance inserts record the claims
// they hold in the returned collector.
func withHeldClaims(ctx context.Context) (context.Context, *heldClaims) {
	held := &heldClaims{}

	return context.WithValue(ctx, heldClaimsKey{}, held), held
}

func (h *heldClaims) add(instanceID string) {
	if h == nil {
		return
	}

	h.mutex.Lock()
	defer h.mutex.Unlock()

	if !slices.Contains(h.instances, instanceID) {
		h.instances = append(h.instances, instanceID)
	}
}

func (h *heldClaims) list() []string {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	return slices.Clone(h.instances)
}

// insertExchangeRows inserts one instance_exchanges row for each exchange
// the template uses, in the transaction that inserts the pending instance.
// The inserting request holds the claim of each row, so it exchanges right
// after the commit. A template that also has persistent volumes gets closed
// rows: nobody exchanges until the leader has its volumes and opens them.
func insertExchangeRows(ctx context.Context, queries *db.Queries, template *v1alpha1.MCPServerTemplate, instanceID string) error {
	names := catalog.ExchangeNames(template)
	if len(names) == 0 {
		return nil
	}

	open := len(template.Spec.PersistentVolumes) == 0

	for _, name := range names {
		if err := queries.InsertInstanceExchange(ctx, db.InsertInstanceExchangeParams{
			InstanceID: instanceID, Exchange: name, Closed: !open, Wanted: open, Claimed: open,
		}); err != nil {
			return fmt.Errorf("insert exchange %s of instance %s: %w", name, instanceID, err)
		}
	}

	if open {
		held, _ := ctx.Value(heldClaimsKey{}).(*heldClaims)
		held.add(instanceID)
	}

	return nil
}

// runHeldClaims runs the exchanges of the instances this request inserted
// with their claims held. It runs after the inserting transaction commits.
func (m *metaTools) runHeldClaims(ctx context.Context, current requestContext, held *heldClaims) {
	for _, id := range held.list() {
		instance, err := db.New(m.database).GetInstance(ctx, id)
		if err != nil {
			// The insert rolled back, or the row is already gone.
			continue
		}

		if !instance.UsesExchange || instance.Desired != "running" {
			continue
		}

		rows, err := db.New(m.database).ListInstanceExchanges(ctx, id)
		if err != nil {
			m.logger.WarnContext(ctx, "read the exchanges of a new instance", slog.String("instance", id), slog.Any("error", err))

			continue
		}

		names := make([]string, 0, len(rows))
		for _, row := range rows {
			if row.ClaimedAt.Valid {
				names = append(names, row.Exchange)
			}
		}

		m.runExchanges(ctx, current, instance, names)
	}
}

// claimAndExchange runs the claim for the instance and the exchanges it
// returns. A waiting request calls it at each wake, so a lost delivery is
// retried once its claim expires.
func (m *metaTools) claimAndExchange(ctx context.Context, current requestContext, instanceID string) {
	if m.exchanges == nil || instanceID == "" || current.subjectToken == "" {
		return
	}

	queries := db.New(m.database)

	instance, err := queries.GetInstance(ctx, instanceID)
	if err != nil {
		m.logger.WarnContext(ctx, "read an instance with token exchanges", slog.String("instance", instanceID), slog.Any("error", err))

		return
	}

	rows, err := queries.ListInstanceExchanges(ctx, instanceID)
	if err != nil {
		m.logger.WarnContext(ctx, "read token exchanges", slog.String("instance", instanceID), slog.Any("error", err))

		return
	}

	timeouts := m.exchangeTimeouts(ctx, instance, rows)

	// A start whose exchange failed is over: the leader fails the instance
	// with that reason. Claiming again would exchange in a loop, because
	// each failure wakes the waiting requests.
	for _, row := range rows {
		if row.WantedSince.Valid && row.LastErrorAt.Valid && !row.LastErrorAt.Time.Before(row.WantedSince.Time) {
			return
		}
	}

	claimed, err := (&credentials.Claimer{}).Claim(ctx, queries, instanceID, credentials.Caller{
		Principal: current.identity.Principal, Kind: current.identity.Kind, TokenExpiry: current.identity.Expiry,
	}, timeouts)
	if err != nil {
		m.logger.WarnContext(ctx, "claim token exchanges", slog.String("instance", instanceID), slog.Any("error", err))

		return
	}

	if len(claimed) == 0 {
		return
	}

	m.runExchanges(ctx, current, instance, claimed)
}

// refreshInBackground keeps the exchanged tokens of a ready instance fresh
// while its owner calls it. It runs the claim and the exchanges it returns
// in a goroutine, so the call is forwarded at once with the token already in
// the Secret. The goroutine gets a copy of the request's subject token and a
// context that keeps no cancellation from the request and ends after the
// longest exchange timeout.
func (m *metaTools) refreshInBackground(ctx context.Context, current requestContext, instance db.Instance) {
	if instance.ID == "" || !instance.UsesExchange || m.exchanges == nil || current.subjectToken == "" {
		return
	}

	// The margin covers the writes that follow an exchange. The timeout of
	// each exchange is read from its resource when it runs, so the context
	// allows the longest one a resource may set.
	background, cancel := context.WithTimeout(context.WithoutCancel(ctx), maxExchangeTimeout+refreshWriteMargin)

	go func() {
		defer cancel()

		m.claimAndExchange(background, current, instance.ID)
	}()
}

// refreshWriteMargin is the time a background refresh keeps after the
// exchange's own timeout, to write its result.
const refreshWriteMargin = 10 * time.Second

// maxExchangeTimeout is the longest timeout a TokenExchange may set.
const maxExchangeTimeout = 60 * time.Second

// exchangeTimeouts returns the claim timeout of each exchange row whose
// TokenExchange can be used now: the timeout the resource sets. A row whose
// exchange is missing or not valid is left out, so it is never claimed and
// nothing is sent; a running instance then stops at its token's expiry.
func (m *metaTools) exchangeTimeouts(ctx context.Context, instance db.Instance, rows []db.InstanceExchange) map[string]time.Duration {
	timeouts := make(map[string]time.Duration, len(rows))

	for _, row := range rows {
		if exchanger, err := m.exchanges.Exchanger(ctx, instance.TemplateNs, row.Exchange); err == nil {
			timeouts[row.Exchange] = exchanger.Timeout()
		}
	}

	return timeouts
}

// runExchanges runs each named exchange of the instance with the request's
// token. A success sends the sealed token to the leader; a failure records
// its reason on the row so the leader fails the start. Either writes
// credential.exchange in the same transaction.
func (m *metaTools) runExchanges(ctx context.Context, current requestContext, instance db.Instance, names []string) {
	for _, name := range names {
		if err := m.runExchange(ctx, current, instance, name); err != nil {
			m.logger.WarnContext(ctx, "token exchange", slog.String("instance", instance.ID),
				slog.String("exchange", name), slog.Any("error", err))
		}
	}
}

func (m *metaTools) runExchange(ctx context.Context, current requestContext, instance db.Instance, name string) error {
	row, err := m.exchangeRow(ctx, instance.ID, name)
	if err != nil {
		return err
	}

	kind := credentials.KindRefresh
	if row.WantedSince.Valid {
		kind = credentials.KindStart
	}

	// The exchanger is built from the TokenExchange of the template's
	// namespace as it is now, so an edit applies from the next exchange.
	// One that is missing, invalid or not allowed fails the exchange, and
	// nothing is sent. A gateway that runs no exchanges leaves the row
	// alone; its templates that use one are invalid.
	if m.exchanges == nil {
		return nil
	}

	exchanger, err := m.exchanges.Exchanger(ctx, instance.TemplateNs, name)
	if err != nil {
		return m.recordExchangeFailure(ctx, current, instance, name, kind, credentials.UnavailableError(name, err))
	}

	exchanged, err := exchanger.Exchange(ctx, current.subjectToken, kind)
	if err != nil {
		return m.recordExchangeFailure(ctx, current, instance, name, kind, err)
	}

	details := map[string]any{
		"instance": instance.ID, "exchange": name, "kind": kind, "outcome": "success",
		"expiresAt": exchanged.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}

	return pgx.BeginFunc(ctx, m.database, func(tx pgx.Tx) error {
		queries := db.New(tx)

		if err := audit.NewRecorder(m.auditOutput, queries).Record(ctx,
			auditEvent(current, audit.TypeCredentialExchange, instance.TemplateName, details)); err != nil {
			return err
		}

		// Queued in the transaction, so the leader gets the token only
		// with its audit event.
		return credentials.Send(ctx, queries, m.credentialsCipher, credentials.Delivery{
			Instance: instance.ID, Exchange: name, Principal: instance.OwnerPrincipal,
			IssuedAt: exchanged.IssuedAt, ExpiresAt: exchanged.ExpiresAt, Token: exchanged.Token,
		})
	})
}

// recordExchangeFailure writes the reason of a failed exchange on its row,
// gives up the claim, writes credential.exchange and wakes the leader and
// the waiting requests, in one transaction.
func (m *metaTools) recordExchangeFailure(ctx context.Context, current requestContext, instance db.Instance, name, kind string, cause error) error {
	reason, code := fmt.Sprintf("Token exchange `%s` failed.", name), credentials.ErrorBadAnswer

	var exchangeErr *credentials.ExchangeError
	if errors.As(cause, &exchangeErr) {
		reason, code = exchangeErr.Error(), exchangeErr.Code
	}

	details := map[string]any{
		"instance": instance.ID, "exchange": name, "kind": kind, "outcome": "failure", "error": code,
	}

	return pgx.BeginFunc(ctx, m.database, func(tx pgx.Tx) error {
		queries := db.New(tx)

		if _, err := queries.RecordExchangeFailure(ctx, db.RecordExchangeFailureParams{
			InstanceID: instance.ID, Exchange: name, LastError: pgtype.Text{String: reason, Valid: true},
			Retry: kind == credentials.KindRefresh,
		}); err != nil {
			return fmt.Errorf("record the failed exchange %s of instance %s: %w", name, instance.ID, err)
		}

		if err := audit.NewRecorder(m.auditOutput, queries).Record(ctx,
			auditEvent(current, audit.TypeCredentialExchange, instance.TemplateName, details)); err != nil {
			return err
		}

		return store.Notify(ctx, tx, store.KindInstance, instance.ID)
	})
}

func (m *metaTools) exchangeRow(ctx context.Context, instanceID, name string) (db.InstanceExchange, error) {
	rows, err := db.New(m.database).ListInstanceExchanges(ctx, instanceID)
	if err != nil {
		return db.InstanceExchange{}, fmt.Errorf("read the exchanges of instance %s: %w", instanceID, err)
	}

	for _, row := range rows {
		if row.Exchange == name {
			return row, nil
		}
	}

	return db.InstanceExchange{}, fmt.Errorf("instance %s has no row for exchange %s", instanceID, name)
}
