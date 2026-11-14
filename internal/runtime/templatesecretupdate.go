package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/solidDoWant/toolgate/internal/store/db"
)

// Outcomes of one template secret update, the label of
// toolgate_template_secret_updates_total.
const (
	templateSecretUpdated = "updated"
	templateSecretMissing = "missing"
	templateSecretError   = "error"
)

const errTemplateSecretsNamespaceChanged = "the template secrets namespace changed"

// TemplateSecretWatch runs the cache of the Secrets in the template secrets
// namespace and reports when it has synced. It is added to the manager as a
// runnable that needs leader election, so only the leader holds the values.
type TemplateSecretWatch struct {
	Cache cache.Cache

	synced atomic.Bool
}

// Start runs the cache until ctx ends. It creates the Secret informer at
// once, so Synced does not depend on a first read.
func (w *TemplateSecretWatch) Start(ctx context.Context) error {
	done := make(chan error, 1)

	go func() { done <- w.Cache.Start(ctx) }()

	if _, err := w.Cache.GetInformer(ctx, &corev1.Secret{}); err == nil && w.Cache.WaitForCacheSync(ctx) {
		w.synced.Store(true)
	}

	err := <-done

	w.synced.Store(false)

	return err
}

// Synced reports whether the cache has read every Secret of the namespace.
func (w *TemplateSecretWatch) Synced() bool { return w.synced.Load() }

// TemplateSecretUpdater is the step of the full pass that patches changed
// template secret values into the Secrets of running instances.
type TemplateSecretUpdater struct {
	Queries *db.Queries
	// Source reads the template secrets, from the cache of the template
	// secrets namespace. Nil when no namespace is configured.
	Source client.Reader
	// Namespace is the configured template secrets namespace, empty for none.
	Namespace string
	// Synced reports whether Source has synced. Nil means it always has.
	Synced func() bool
	// Patcher patches the target Secrets.
	Patcher client.Writer
	Metrics *Metrics
	Log     *slog.Logger
}

// Step compares the rows of one live instance with the source Secrets and
// patches each target key whose source changed. It is level-based: it looks
// at versions on every pass, so no event is needed for it to catch up.
func (u *TemplateSecretUpdater) Step(ctx context.Context, instance db.Instance) error {
	if instance.Desired != "running" || (instance.Status != "starting" && instance.Status != "ready") {
		return nil
	}

	if u.Namespace != "" && u.Synced != nil && !u.Synced() {
		return nil
	}

	rows, err := u.Queries.ListInstanceTemplateSecrets(ctx, instance.ID)
	if err != nil {
		return fmt.Errorf("list template secret rows of instance %s: %w", instance.ID, err)
	}

	var (
		problems []error
		changes  = map[string][]templateSecretChange{}
		order    []string
		sources  = map[string]*corev1.Secret{}
	)

	for _, row := range rows {
		if u.Namespace == "" || row.SourceNamespace != u.Namespace {
			u.recordMissing(ctx, instance, row, errTemplateSecretsNamespaceChanged)

			continue
		}

		source, found := sources[row.SourceName]
		if !found {
			source, err = u.readSource(ctx, row.SourceName)
			if err != nil {
				problems = append(problems, fmt.Errorf("read template secret %s: %w", row.SourceName, err))

				continue
			}

			sources[row.SourceName] = source
		}

		if source == nil {
			u.recordMissing(ctx, instance, row, fmt.Sprintf("Secret `%s` was deleted", row.SourceName))

			continue
		}

		value, present := source.Data[row.SourceKey]
		if !present {
			u.recordMissing(ctx, instance, row, fmt.Sprintf("Secret `%s` has no key `%s`", row.SourceName, row.SourceKey))

			continue
		}

		if source.ResourceVersion == row.SourceVersion {
			continue
		}

		if row.TargetField != "data" && !utf8.Valid(value) {
			u.recordError(ctx, instance, row, fmt.Sprintf(
				"the value of key `%s` in Secret `%s` isn't UTF-8 text; use it under `data`", row.SourceKey, row.SourceName))
			u.count(templateSecretError)

			continue
		}

		if _, seen := changes[row.TargetSecret]; !seen {
			order = append(order, row.TargetSecret)
		}

		changes[row.TargetSecret] = append(changes[row.TargetSecret], templateSecretChange{row: row, value: value, version: source.ResourceVersion})
	}

	for _, target := range order {
		if err := u.patchTarget(ctx, instance, target, changes[target]); err != nil {
			problems = append(problems, err)
		}
	}

	return errors.Join(problems...)
}

type templateSecretChange struct {
	row     db.InstanceTemplateSecret
	value   []byte
	version string
}

// readSource returns the Secret from the cache, or nil when it doesn't exist.
func (u *TemplateSecretUpdater) readSource(ctx context.Context, name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}

	err := u.Source.Get(ctx, client.ObjectKey{Namespace: u.Namespace, Name: name}, secret)
	if apierrors.IsNotFound(err) {
		return nil, nil //nolint:nilnil // nil means the Secret is gone.
	}

	if err != nil {
		return nil, fmt.Errorf("get: %w", err)
	}

	return secret, nil
}

// patchTarget patches all changed keys of one target Secret in one request,
// then records the new versions. A failed patch keeps the rows, so the next
// pass tries again.
func (u *TemplateSecretUpdater) patchTarget(ctx context.Context, instance db.Instance, target string, changed []templateSecretChange) error {
	values := make([]SecretKeyValue, 0, len(changed))
	for _, change := range changed {
		values = append(values, SecretKeyValue{Namespace: instance.Namespace, Secret: target, Key: change.row.TargetKey, Value: change.value})
	}

	if err := PatchSecretKeys(ctx, u.Patcher, values); err != nil {
		u.log().WarnContext(ctx, "template secret update failed", slog.String("instance", instance.ID),
			slog.String("secret", target), slog.String("error", err.Error()))

		for range changed {
			u.count(templateSecretError)
		}

		return fmt.Errorf("update template secret copies of instance %s: %w", instance.ID, err)
	}

	var problems []error

	for _, change := range changed {
		// A compare-and-set that changes no row means a render or repair
		// wrote the row meanwhile; the next pass compares again.
		_, err := u.Queries.SetTemplateSecretVersion(ctx, db.SetTemplateSecretVersionParams{
			InstanceID: instance.ID, TargetSecret: change.row.TargetSecret, TargetKey: change.row.TargetKey,
			SourceVersion: change.version, OldSourceVersion: change.row.SourceVersion,
		})
		if err != nil {
			problems = append(problems, fmt.Errorf("record template secret version of instance %s: %w", instance.ID, err))

			continue
		}

		u.count(templateSecretUpdated)
	}

	return errors.Join(problems...)
}

func (u *TemplateSecretUpdater) recordMissing(ctx context.Context, instance db.Instance, row db.InstanceTemplateSecret, reason string) {
	u.recordError(ctx, instance, row, reason)
	u.count(templateSecretMissing)
}

// recordError stores why the update failed and logs a warning when the
// reason is new.
func (u *TemplateSecretUpdater) recordError(ctx context.Context, instance db.Instance, row db.InstanceTemplateSecret, reason string) {
	changed, err := u.Queries.SetTemplateSecretError(ctx, db.SetTemplateSecretErrorParams{
		InstanceID: instance.ID, TargetSecret: row.TargetSecret, TargetKey: row.TargetKey,
		LastError: pgtype.Text{String: reason, Valid: true},
	})
	if err != nil {
		u.log().WarnContext(ctx, "could not record the template secret error", slog.String("instance", instance.ID),
			slog.String("error", err.Error()))

		return
	}

	if changed > 0 {
		u.log().WarnContext(ctx, "template secret copy kept", slog.String("instance", instance.ID),
			slog.String("secret", row.TargetSecret), slog.String("key", row.TargetKey), slog.String("reason", reason))
	}
}

func (u *TemplateSecretUpdater) count(outcome string) {
	if u.Metrics != nil {
		u.Metrics.TemplateSecretUpdates.WithLabelValues(outcome).Inc()
	}
}

func (u *TemplateSecretUpdater) log() *slog.Logger {
	if u.Log == nil {
		return slog.New(slog.DiscardHandler)
	}

	return u.Log
}
