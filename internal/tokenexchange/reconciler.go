package tokenexchange

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/solidDoWant/toolgate/api/v1alpha1"
)

// DefaultRecheckInterval is how often the reconciler checks every exchange
// again, so a client Secret created or emptied later shows in the status.
const DefaultRecheckInterval = time.Minute

// Reconciler writes the Valid condition of the TokenExchange resources in
// the catalog namespaces. It writes nothing but the status subresource, and
// runs only on the leader.
type Reconciler struct {
	Client  client.Client
	Checker Checker
	Log     *slog.Logger
	// Namespaces are the catalog namespaces. Resources in other namespaces
	// are ignored.
	Namespaces []string
	// RecheckInterval is how often every exchange is checked again. Zero
	// means DefaultRecheckInterval.
	RecheckInterval time.Duration
}

// SetupWithManager registers the reconciler with mgr. Every event queues its
// whole namespace, because the per-namespace limit makes one resource's
// validity depend on the others.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if len(r.Namespaces) == 0 {
		return errors.New("token exchanges: no namespaces to watch")
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("tokenexchange").
		Watches(&v1alpha1.TokenExchange{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, object client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: object.GetNamespace()}}}
			}),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(object client.Object) bool {
				return slices.Contains(r.Namespaces, object.GetNamespace())
			}))).
		Complete(r)
}

// Reconcile checks every TokenExchange in the request's namespace and
// writes the status of each one whose condition changed.
func (r *Reconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	if !slices.Contains(r.Namespaces, request.Namespace) {
		return reconcile.Result{}, nil
	}

	var list v1alpha1.TokenExchangeList
	if err := r.Client.List(ctx, &list, client.InNamespace(request.Namespace)); err != nil {
		return reconcile.Result{}, fmt.Errorf("token exchanges: listing %s: %w", request.Namespace, err)
	}

	slices.SortFunc(list.Items, func(a, b v1alpha1.TokenExchange) int { return strings.Compare(a.Name, b.Name) })

	var problems []error

	for index := range list.Items {
		exchange := &list.Items[index]
		if !exchange.DeletionTimestamp.IsZero() {
			continue
		}

		result, err := r.Checker.Check(ctx, exchange, index >= MaxPerNamespace)
		if err != nil {
			problems = append(problems, err)

			continue
		}

		if err := r.writeStatus(ctx, exchange, result); err != nil {
			problems = append(problems, err)
		}
	}

	if len(problems) > 0 {
		return reconcile.Result{}, errors.Join(problems...)
	}

	interval := r.RecheckInterval
	if interval == 0 {
		interval = DefaultRecheckInterval
	}

	return reconcile.Result{RequeueAfter: interval}, nil
}

func (r *Reconciler) writeStatus(ctx context.Context, exchange *v1alpha1.TokenExchange, result Result) error {
	condition := metav1.Condition{
		Type:               ConditionValid,
		Status:             metav1.ConditionTrue,
		Reason:             result.Reason,
		Message:            result.Message,
		ObservedGeneration: exchange.Generation,
	}
	if !result.Valid() {
		condition.Status = metav1.ConditionFalse
	}

	if !meta.SetStatusCondition(&exchange.Status.Conditions, condition) {
		return nil
	}

	if !result.Valid() {
		r.logger().WarnContext(ctx, "token exchange is invalid", "namespace", exchange.Namespace, "exchange", exchange.Name,
			"generation", exchange.Generation, "reason", result.Reason, "message", result.Message)
	}

	if err := r.Client.Status().Update(ctx, exchange); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("token exchanges: writing the status of %s/%s: %w", exchange.Namespace, exchange.Name, err)
	}

	return nil
}

func (r *Reconciler) logger() *slog.Logger {
	if r.Log == nil {
		return slog.Default()
	}

	return r.Log
}
