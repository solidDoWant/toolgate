package catalog

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
	"github.com/solidDoWant/toolgate/internal/election"
)

// Condition type and reasons written to a template's status.
const (
	ConditionValid = "Valid"

	ReasonValid   = "Valid"
	ReasonInvalid = "Invalid"
)

// Reconciler reads MCPServerTemplate objects into a Catalog and writes the
// validation result to their status. It writes nothing but the status
// subresource. It runs on every replica, because each replica's policy reads
// its own catalog, and writes status only while Elected is closed.
type Reconciler struct {
	Client    client.Client
	Catalog   *Catalog
	Validator Validator
	Log       *slog.Logger
	// Elected is closed once this replica holds the leader Lease
	// (manager.Manager.Elected). Nil means the reconciler always writes
	// status.
	Elected <-chan struct{}
	// Namespaces are the namespaces whose templates are read. Templates in
	// other namespaces are ignored.
	Namespaces []string
}

// CacheNamespaces returns the cache configuration that limits a manager's
// informers to the watched namespaces.
func CacheNamespaces(namespaces []string) map[string]cache.Config {
	configs := make(map[string]cache.Config, len(namespaces))
	for _, namespace := range namespaces {
		configs[namespace] = cache.Config{}
	}

	return configs
}

// SetupWithManager registers the reconciler with mgr. It allows
// CiliumNetworkPolicy objects when that CRD exists in the cluster.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if len(r.Namespaces) == 0 {
		return fmt.Errorf("catalog: no namespaces to watch")
	}

	if _, err := mgr.GetRESTMapper().RESTMapping(CiliumNetworkPolicy); err == nil {
		r.Validator.CiliumNetworkPolicy = true
	}

	requeue, err := election.RequeueOnElection(mgr, r.listWatched)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.MCPServerTemplate{}, builder.WithPredicates(predicate.NewPredicateFuncs(r.watched))).
		WatchesRawSource(requeue).
		WithOptions(controller.Options{NeedLeaderElection: ptr.To(false)}).
		Named("catalog").
		Complete(r)
}

// listWatched returns every template in the watched namespaces.
func (r *Reconciler) listWatched(ctx context.Context) ([]client.Object, error) {
	var objects []client.Object

	for _, namespace := range r.Namespaces {
		var templates v1alpha1.MCPServerTemplateList
		if err := r.Client.List(ctx, &templates, client.InNamespace(namespace)); err != nil {
			return nil, fmt.Errorf("catalog: listing templates in %s: %w", namespace, err)
		}

		for index := range templates.Items {
			objects = append(objects, &templates.Items[index])
		}
	}

	return objects, nil
}

func (r *Reconciler) watched(object client.Object) bool {
	return slices.Contains(r.Namespaces, object.GetNamespace())
}

// Reconcile reads one template, validates it, updates the catalog, and
// writes the status of every template whose validity the change affects.
func (r *Reconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	if !slices.Contains(r.Namespaces, request.Namespace) {
		return reconcile.Result{}, nil
	}

	var template v1alpha1.MCPServerTemplate

	err := r.Client.Get(ctx, request.NamespacedName, &template)

	var affected []types.NamespacedName

	switch {
	case apierrors.IsNotFound(err):
		affected = r.Catalog.Delete(request.NamespacedName)
	case err != nil:
		return reconcile.Result{}, fmt.Errorf("catalog: reading template %s: %w", request.NamespacedName, err)
	case !template.DeletionTimestamp.IsZero():
		affected = r.Catalog.Delete(request.NamespacedName)
	default:
		affected = r.Catalog.Set(&template, r.Validator.Validate(&template))
	}

	if !election.IsElected(r.Elected) {
		return reconcile.Result{}, nil
	}

	for _, key := range affected {
		if err := r.writeStatus(ctx, key); err != nil {
			return reconcile.Result{}, err
		}
	}

	return reconcile.Result{}, nil
}

// writeStatus sets the Valid condition of one template from the catalog.
func (r *Reconciler) writeStatus(ctx context.Context, key types.NamespacedName) error {
	status, found := r.Catalog.Status(key)
	if !found {
		return nil
	}

	var template v1alpha1.MCPServerTemplate
	if err := r.Client.Get(ctx, key, &template); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("catalog: reading template %s: %w", key, err)
	}

	// The object is newer than what the catalog holds; its own event
	// writes its status.
	if template.Generation != status.Generation {
		return nil
	}

	condition := metav1.Condition{
		Type:               ConditionValid,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonValid,
		Message:            "The template is valid and offered.",
		ObservedGeneration: status.Generation,
	}

	if !status.Valid {
		condition.Status = metav1.ConditionFalse
		condition.Reason = ReasonInvalid
		condition.Message = status.Reason
	}

	if !meta.SetStatusCondition(&template.Status.Conditions, condition) {
		return nil
	}

	if !status.Valid {
		r.logger().WarnContext(ctx, "template is invalid", "namespace", key.Namespace, "template", key.Name, "generation", status.Generation, "reason", status.Reason)
	}

	if err := r.Client.Status().Update(ctx, &template); err != nil {
		return fmt.Errorf("catalog: writing the status of template %s: %w", key, err)
	}

	return nil
}

func (r *Reconciler) logger() *slog.Logger {
	if r.Log == nil {
		return slog.Default()
	}

	return r.Log
}
