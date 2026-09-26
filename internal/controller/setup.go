/*
Copyright The FleetPermit Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	clusterv1beta1 "open-cluster-management.io/api/cluster/v1beta1"
	workv1 "open-cluster-management.io/api/work/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
	"github.com/fleetpermit/fleetpermit/internal/placement/ocm"
)

// IndexFields registers the field indexes the controller reads: leases by
// the policy they reference, and ManifestWorks by the policy they deliver.
func IndexFields(ctx context.Context, mgr ctrl.Manager) error {
	err := mgr.GetFieldIndexer().IndexField(ctx, &fpv1.ToolAccessLease{}, PolicyIndexField, func(o client.Object) []string {
		return []string{o.(*fpv1.ToolAccessLease).Spec.PolicyRef.Name}
	})
	if err != nil {
		return err
	}
	return ocm.IndexWorks(ctx, mgr.GetFieldIndexer())
}

// CacheOptions returns the manager's cache settings. Only FleetPermit's own
// ManifestWorks are cached, never the rest of the hub's. With a watch
// namespace, FleetPermit objects and placements are cached from that
// namespace only.
func CacheOptions(watchNamespace string) cache.Options {
	byObject := map[client.Object]cache.ByObject{
		&workv1.ManifestWork{}: {Label: labels.SelectorFromSet(labels.Set{ocm.LabelManagedBy: ocm.ManagedByValue})},
	}
	if watchNamespace != "" {
		ns := map[string]cache.Config{watchNamespace: {}}
		byObject[&fpv1.FleetAccessPolicy{}] = cache.ByObject{Namespaces: ns}
		byObject[&fpv1.ToolAccessLease{}] = cache.ByObject{Namespaces: ns}
		byObject[&clusterv1beta1.Placement{}] = cache.ByObject{Namespaces: ns}
		byObject[&clusterv1beta1.PlacementDecision{}] = cache.ByObject{Namespaces: ns}
	}
	return cache.Options{ByObject: byObject}
}

// SetupWithManager wires the reconciler and its watches.
func (r *PolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&fpv1.FleetAccessPolicy{}).
		Watches(&fpv1.ToolAccessLease{}, handler.EnqueueRequestsFromMapFunc(r.leaseToPolicy)).
		Watches(&clusterv1beta1.PlacementDecision{}, handler.EnqueueRequestsFromMapFunc(r.placementToPolicies)).
		Watches(&clusterv1beta1.Placement{}, handler.EnqueueRequestsFromMapFunc(r.placementToPolicies)).
		Watches(&workv1.ManifestWork{}, handler.EnqueueRequestsFromMapFunc(r.workToPolicy)).
		Watches(&clusterv1.ManagedCluster{}, handler.EnqueueRequestsFromMapFunc(r.allPolicies),
			builder.WithPredicates(availabilityChanged)).
		Named("fleetaccesspolicy").
		Complete(r)
}

// availabilityChanged passes ManagedCluster creations, deletions and changes
// to the Available condition, the only cluster state the controller reads, so
// that routine status updates do not reconcile every policy.
var availabilityChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		before, ok := e.ObjectOld.(*clusterv1.ManagedCluster)
		after, ok2 := e.ObjectNew.(*clusterv1.ManagedCluster)
		if !ok || !ok2 {
			return true
		}
		return availability(before) != availability(after)
	},
}

func availability(mc *clusterv1.ManagedCluster) metav1.ConditionStatus {
	if c := meta.FindStatusCondition(mc.Status.Conditions, clusterv1.ManagedClusterConditionAvailable); c != nil {
		return c.Status
	}
	return ""
}

// watches reports whether policies in the namespace are reconciled.
func (r *PolicyReconciler) watches(namespace string) bool {
	return r.WatchNamespace == "" || namespace == r.WatchNamespace
}

func (r *PolicyReconciler) leaseToPolicy(_ context.Context, o client.Object) []reconcile.Request {
	l := o.(*fpv1.ToolAccessLease)
	if !r.watches(l.Namespace) {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: l.Namespace, Name: l.Spec.PolicyRef.Name}}}
}

// workToPolicy maps a ManifestWork, which lives in a cluster namespace, to
// the policy named in its annotation.
func (r *PolicyReconciler) workToPolicy(_ context.Context, o client.Object) []reconcile.Request {
	ref := o.GetAnnotations()[ocm.AnnotationPolicy]
	ns, name, ok := strings.Cut(ref, "/")
	if !ok || ns == "" || name == "" || !r.watches(ns) {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}}
}

// placementToPolicies maps a Placement or PlacementDecision to the policies
// in its namespace that reference the placement.
func (r *PolicyReconciler) placementToPolicies(ctx context.Context, o client.Object) []reconcile.Request {
	name := o.GetName()
	if _, isDecision := o.(*clusterv1beta1.PlacementDecision); isDecision {
		name = o.GetLabels()[clusterv1beta1.PlacementLabel]
	}
	if name == "" || !r.watches(o.GetNamespace()) {
		return nil
	}
	var list fpv1.FleetAccessPolicyList
	if err := r.List(ctx, &list, client.InNamespace(o.GetNamespace())); err != nil {
		return nil
	}
	var out []reconcile.Request
	for _, p := range list.Items {
		if p.Spec.Placement.PlacementRef.Name == name {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: p.Name}})
		}
	}
	return out
}

func (r *PolicyReconciler) allPolicies(ctx context.Context, _ client.Object) []reconcile.Request {
	var list fpv1.FleetAccessPolicyList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for _, p := range list.Items {
		if r.watches(p.Namespace) {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: p.Name}})
		}
	}
	return out
}
