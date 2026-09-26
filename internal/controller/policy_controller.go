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

// Package controller reconciles FleetAccessPolicy and ToolAccessLease objects.
//
// A single reconciler is keyed by FleetAccessPolicy. Leases, placement
// decisions, delivered ManifestWorks and cluster availability all map back to
// the policy they belong to, so every decision about a policy is made in one
// place, from one consistent snapshot.
package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
	"github.com/fleetpermit/fleetpermit/internal/enforcement"
	"github.com/fleetpermit/fleetpermit/internal/lease"
	"github.com/fleetpermit/fleetpermit/internal/metrics"
	"github.com/fleetpermit/fleetpermit/internal/placement"
)

// Finalizer ensures delivered content is withdrawn before a policy disappears.
const Finalizer = "fleetpermit.github.io/cleanup"

// PolicyIndexField indexes leases by the policy they reference.
const PolicyIndexField = "spec.policyRef.name"

const (
	// maxRequeue bounds how long a policy goes without a full re-evaluation.
	maxRequeue = 2 * time.Minute
	// progressRequeue is used while a rollout or revocation is in flight.
	progressRequeue = 5 * time.Second
	// failureRequeue is used when delivery to a cluster failed.
	failureRequeue = time.Second
)

var tracer = otel.Tracer("github.com/fleetpermit/fleetpermit/internal/controller")

// PolicyReconciler reconciles FleetAccessPolicies and their leases.
type PolicyReconciler struct {
	client.Client
	Placement placement.Provider
	Renderer  enforcement.Renderer
	// Now returns the current time. Tests replace it with a fake clock.
	Now func() time.Time

	mu           sync.Mutex
	activeByKey  map[types.NamespacedName]int
	clustersByKy map[types.NamespacedName]int
	// readySeen holds, per policy, the leases this process has seen Ready,
	// so propagation is observed once per lease and not again when a lease
	// returns to Ready after another lease or the placement changed.
	readySeen map[types.NamespacedName]map[types.UID]bool
}

// +kubebuilder:rbac:groups=fleetpermit.github.io,resources=fleetaccesspolicies,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=fleetpermit.github.io,resources=toolaccessleases,verbs=get;list;watch
// +kubebuilder:rbac:groups=fleetpermit.github.io,resources=fleetaccesspolicies/status;toolaccessleases/status,verbs=get;patch
// +kubebuilder:rbac:groups=fleetpermit.github.io,resources=fleetaccesspolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=cluster.open-cluster-management.io,resources=placements;placementdecisions;managedclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=work.open-cluster-management.io,resources=manifestworks,verbs=get;list;watch;create;patch;delete

// Reconcile brings the fleet in line with one FleetAccessPolicy.
func (r *PolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	ctx, span := tracer.Start(ctx, "fleetpermit.reconcile.policy")
	span.SetAttributes(attribute.String("fleetpermit.policy", req.String()))
	defer func() {
		result := "success"
		if err != nil {
			result = "error"
			metrics.ReconcileErrors.Inc()
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		metrics.ReconcileTotal.WithLabelValues(result).Inc()
		span.End()
	}()

	var policy fpv1.FleetAccessPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			r.recordGauges(req.NamespacedName, 0, 0)
			return ctrl.Result{}, r.denyOrphanLeases(ctx, req.NamespacedName)
		}
		return ctrl.Result{}, err
	}

	if !policy.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &policy)
	}
	if controllerutil.AddFinalizer(&policy, Finalizer) {
		if err := r.Update(ctx, &policy); err != nil {
			return ctrl.Result{}, err
		}
	}
	return r.reconcilePolicy(ctx, &policy)
}

// snapshot captures everything decided in one reconcile.
type snapshot struct {
	now           time.Time
	placed        []string
	placementErr  error
	leases        []fpv1.ToolAccessLease
	decisions     map[types.UID]lease.Decision
	grants        map[string][]enforcement.Grant
	results       map[string]enforcement.Result
	observed      map[string]placement.ClusterState
	droppedOn     map[types.UID][]string
	renderedOn    map[types.UID][]string
	applyFailures map[string]string
}

func (r *PolicyReconciler) reconcilePolicy(ctx context.Context, p *fpv1.FleetAccessPolicy) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	s := snapshot{
		now:           r.now(),
		decisions:     map[types.UID]lease.Decision{},
		grants:        map[string][]enforcement.Grant{},
		results:       map[string]enforcement.Result{},
		droppedOn:     map[types.UID][]string{},
		renderedOn:    map[types.UID][]string{},
		applyFailures: map[string]string{},
	}

	// 1. Resolve placement. A missing placement is definitive: withdraw
	// everything. Any other error is retried without changing state; existing
	// grants remain bounded by their data-plane expiry.
	placed, err := r.Placement.SelectedClusters(ctx, p)
	switch {
	case errors.Is(err, placement.ErrPlacementNotFound):
		s.placementErr = err
	case err != nil:
		return ctrl.Result{}, err
	default:
		s.placed = placed
	}
	if prev := p.Status.Clusters; s.placementErr == nil && placementChanged(prev, s.placed) && p.Status.ObservedGeneration != 0 {
		metrics.PlacementChanges.Inc()
	}

	// 2. Evaluate every lease that references the policy.
	var list fpv1.ToolAccessLeaseList
	if err := r.List(ctx, &list, client.InNamespace(p.Namespace), client.MatchingFields{PolicyIndexField: p.Name}); err != nil {
		return ctrl.Result{}, err
	}
	s.leases = list.Items
	sort.Slice(s.leases, func(i, j int) bool { return leaseLess(&s.leases[i], &s.leases[j]) })
	for i := range s.leases {
		l := &s.leases[i]
		d := lease.Evaluate(p, l, s.placed, s.now)
		d = sticky(l, d)
		s.decisions[l.UID] = d
	}

	// 3. Build the grants for every selected cluster, in priority order:
	// standing grants first, then leases oldest first.
	if !p.LeaseRequired() {
		tools := make([]string, 0, len(p.Spec.Permissions))
		for _, perm := range p.Spec.Permissions {
			tools = append(tools, perm.Tool)
		}
		sort.Strings(tools)
		for _, c := range s.placed {
			for _, sub := range p.Spec.Subjects {
				s.grants[c] = append(s.grants[c], enforcement.Grant{ID: sub.SPIFFEID, Subject: sub.SPIFFEID, Tools: tools})
			}
		}
	}
	for i := range s.leases {
		l := &s.leases[i]
		d := s.decisions[l.UID]
		if !d.Active() {
			continue
		}
		for _, c := range d.Clusters {
			s.grants[c] = append(s.grants[c], enforcement.Grant{
				ID: string(l.UID), Lease: l.Name, Subject: l.Spec.Subject.SPIFFEID, Tools: d.Tools, ExpiresAt: d.ExpiresAt,
			})
		}
	}

	// 4. Render and deliver to every selected cluster. A cluster without
	// grants receives an inert policy that allows nothing.
	for _, c := range s.placed {
		_, cspan := tracer.Start(ctx, "fleetpermit.render.cluster")
		cspan.SetAttributes(attribute.String("fleetpermit.cluster", c), attribute.Int("fleetpermit.grants", len(s.grants[c])))
		res, err := r.Renderer.Render(enforcement.Request{Policy: p, Cluster: c, Grants: s.grants[c]})
		if err != nil {
			// Rendering errors withdraw the cluster's content (fail closed).
			cspan.RecordError(err)
			cspan.End()
			logger.Error(err, "rendering failed; withdrawing grants", "cluster", c)
			s.applyFailures[c] = err.Error()
			if rerr := r.Placement.Remove(ctx, p, c); rerr != nil {
				return ctrl.Result{}, rerr
			}
			continue
		}
		cspan.SetAttributes(attribute.String("fleetpermit.content_digest", res.Digest))
		cspan.End()
		s.results[c] = res
		for _, id := range res.Rendered {
			s.renderedOn[types.UID(id)] = append(s.renderedOn[types.UID(id)], c)
		}
		for _, id := range res.Dropped {
			s.droppedOn[types.UID(id)] = append(s.droppedOn[types.UID(id)], c)
		}
		if err := r.Placement.Apply(ctx, p, c, res); err != nil {
			s.applyFailures[c] = err.Error()
			logger.Error(err, "delivering grants failed", "cluster", c)
		}
	}

	// 5. Withdraw everything from clusters that are no longer selected.
	observed, err := r.Placement.Observe(ctx, p)
	if err != nil {
		return ctrl.Result{}, err
	}
	for c := range observed {
		if _, selected := s.results[c]; !selected && !containsString(s.placed, c) {
			if err := r.Placement.Remove(ctx, p, c); err != nil {
				return ctrl.Result{}, err
			}
			delete(observed, c)
		}
	}
	s.observed = observed

	// 6. Report.
	requeue, err := r.updateLeaseStatuses(ctx, p, &s)
	if err != nil {
		return ctrl.Result{}, err
	}
	progressing, err := r.updatePolicyStatus(ctx, p, &s)
	if err != nil {
		return ctrl.Result{}, err
	}
	if progressing && (requeue == 0 || requeue > progressRequeue) {
		requeue = progressRequeue
	}
	if len(s.applyFailures) > 0 {
		// Delivery failed for at least one cluster; retry soon rather than
		// at the next progress check.
		requeue = failureRequeue
	}
	if requeue == 0 || requeue > maxRequeue {
		requeue = maxRequeue
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// clusterInSync reports whether a cluster runs exactly the desired content.
func (s *snapshot) clusterInSync(c string) (bool, string, string) {
	if msg, failed := s.applyFailures[c]; failed {
		return false, "DeliveryFailed", msg
	}
	res, desired := s.results[c]
	obs, present := s.observed[c]
	wantContent := desired && len(res.Objects) > 0
	switch {
	case !wantContent && !present:
		return true, "NothingToEnforce", "no active grants for this cluster"
	case !wantContent && present:
		return false, "Revoking", "withdrawing previously delivered grants"
	case wantContent && !present:
		return false, "Delivering", "grants are being delivered"
	case obs.Digest != res.Digest:
		return false, "Updating", "the cluster holds a previous revision"
	case !obs.Ready:
		return false, obs.Reason, obs.Message
	default:
		return true, obs.Reason, obs.Message
	}
}

func (r *PolicyReconciler) updateLeaseStatuses(ctx context.Context, p *fpv1.FleetAccessPolicy, s *snapshot) (time.Duration, error) {
	var next time.Duration
	active := 0
	key := types.NamespacedName{Namespace: p.Namespace, Name: p.Name}
	seenBefore := r.readyLeases(key)
	seenNow := map[types.UID]bool{}
	for i := range s.leases {
		l := &s.leases[i]
		d := s.decisions[l.UID]
		st := l.Status.DeepCopy()
		st.ObservedGeneration = l.Generation
		wasDenied := hasTrue(l.Status.Conditions, fpv1.ConditionDenied)
		wasExpired := hasTrue(l.Status.Conditions, fpv1.ConditionExpired)
		wasReady := hasTrue(l.Status.Conditions, fpv1.ConditionReady)
		gen := l.Generation

		if !d.ExpiresAt.IsZero() {
			t := metav1Time(d.ExpiresAt)
			st.ExpiresAt = &t
		}

		switch {
		case d.Denied || d.Expired:
			phase, cond, reason := fpv1.LeaseDenied, fpv1.ConditionDenied, d.Reason
			if !d.Denied {
				phase, cond = fpv1.LeaseExpired, fpv1.ConditionExpired
			}
			// Grants remain listed until every cluster has withdrawn them.
			var pending []string
			for _, c := range st.Clusters {
				if ok, _, _ := s.clusterInSync(c); !ok {
					pending = append(pending, c)
				}
			}
			// Observe once, when the last cluster has withdrawn the grant. The
			// phase is usually already Denied or Expired by then, because the
			// withdrawal takes more than one reconcile.
			if len(pending) == 0 && len(st.Clusters) > 0 {
				ref := s.now
				if d.Expired {
					ref = d.ExpiresAt
				}
				if d.Denied {
					ref = conditionTime(l.Status.Conditions, fpv1.ConditionDenied, s.now)
				}
				metrics.LeaseRevocation.Observe(s.now.Sub(ref).Seconds())
			}
			st.Phase = phase
			st.Clusters = pending
			setCond(&st.Conditions, gen, cond, true, reason, d.Message)
			if d.Denied {
				setCond(&st.Conditions, gen, fpv1.ConditionExpired, false, fpv1.ReasonNotExpired, "lease was denied")
			} else {
				setCond(&st.Conditions, gen, fpv1.ConditionDenied, false, fpv1.ReasonAllowed, "lease satisfied the policy")
			}
			setCond(&st.Conditions, gen, fpv1.ConditionReady, false, reason, d.Message)
			if len(pending) > 0 {
				setCond(&st.Conditions, gen, fpv1.ConditionProgressing, true, "Revoking", "withdrawing grants from: "+strings.Join(pending, ", "))
			} else {
				setCond(&st.Conditions, gen, fpv1.ConditionProgressing, false, reason, "grants withdrawn from every cluster")
			}
			setCond(&st.Conditions, gen, fpv1.ConditionDegraded, false, reason, "")
			if d.Denied && !wasDenied {
				metrics.DeniedLeases.WithLabelValues(d.Reason).Inc()
			}
			if d.Expired && !wasExpired {
				metrics.ExpiredLeases.Inc()
			}
		default:
			setCond(&st.Conditions, gen, fpv1.ConditionDenied, false, fpv1.ReasonAllowed, "lease satisfied the policy")
			setCond(&st.Conditions, gen, fpv1.ConditionExpired, false, fpv1.ReasonNotExpired, "expires at "+d.ExpiresAt.Format(time.RFC3339))
			rendered := sortedCopy(s.renderedOn[l.UID])
			dropped := sortedCopy(s.droppedOn[l.UID])
			st.Clusters = rendered
			if until := d.ExpiresAt.Sub(s.now); until > 0 && (next == 0 || until < next) {
				next = until + 500*time.Millisecond
			}
			if len(rendered) == 0 {
				// The lease grants nothing anywhere: no requested cluster is
				// placed, or the rule limit was reached on every one of them.
				st.Phase = fpv1.LeasePending
				if wasReady || seenBefore[l.UID] {
					seenNow[l.UID] = true
				}
				reason, msg := d.Reason, d.Message
				if len(dropped) > 0 {
					reason = fpv1.ReasonCapacityExceeded
					msg = "the enforcement layer's rule limit was reached on: " + strings.Join(dropped, ", ")
				}
				setCond(&st.Conditions, gen, fpv1.ConditionReady, false, reason, msg)
				setCond(&st.Conditions, gen, fpv1.ConditionProgressing, false, reason, msg)
				if len(dropped) > 0 {
					setCond(&st.Conditions, gen, fpv1.ConditionDegraded, true, reason, msg)
				} else {
					setCond(&st.Conditions, gen, fpv1.ConditionDegraded, false, reason, "")
				}
				break
			}
			active++
			st.Phase = fpv1.LeaseActive
			var waiting, failed []string
			for _, c := range rendered {
				ok, reason, msg := s.clusterInSync(c)
				if ok {
					continue
				}
				if reason == "DeliveryFailed" || reason == "ApplyFailed" || reason == "RejectedByEnforcement" || reason == "ClusterUnavailable" {
					failed = append(failed, c+": "+msg)
				} else {
					waiting = append(waiting, c)
				}
			}
			msg := d.Message
			if len(d.OutsidePlacement) > 0 {
				msg += "; ignored clusters outside the placement: " + strings.Join(d.OutsidePlacement, ", ")
			}
			ready := len(waiting) == 0 && len(failed) == 0 && len(dropped) == 0
			if ready || wasReady || seenBefore[l.UID] {
				seenNow[l.UID] = true
			}
			if ready {
				setCond(&st.Conditions, gen, fpv1.ConditionReady, true, fpv1.ReasonLeaseActive, msg)
				if !wasReady && !seenBefore[l.UID] {
					metrics.PolicyPropagation.Observe(s.now.Sub(l.CreationTimestamp.Time).Seconds())
				}
			} else {
				setCond(&st.Conditions, gen, fpv1.ConditionReady, false, fpv1.ReasonRollingOut, msg)
			}
			if len(waiting) > 0 {
				setCond(&st.Conditions, gen, fpv1.ConditionProgressing, true, fpv1.ReasonRollingOut, "waiting for: "+strings.Join(waiting, ", "))
			} else {
				setCond(&st.Conditions, gen, fpv1.ConditionProgressing, false, fpv1.ReasonReconciled, "rollout complete")
			}
			switch {
			case len(dropped) > 0:
				setCond(&st.Conditions, gen, fpv1.ConditionDegraded, true, fpv1.ReasonCapacityExceeded,
					"the enforcement layer's rule limit was reached on: "+strings.Join(dropped, ", "))
			case len(failed) > 0:
				setCond(&st.Conditions, gen, fpv1.ConditionDegraded, true, fpv1.ReasonClustersFailed, strings.Join(failed, "; "))
			default:
				setCond(&st.Conditions, gen, fpv1.ConditionDegraded, false, fpv1.ReasonReconciled, "")
			}
		}
		st.ClusterCount = int32(len(st.Clusters))
		if err := r.patchLeaseStatus(ctx, l, st); err != nil {
			return 0, err
		}
		if st.Phase == fpv1.LeaseActive || len(st.Clusters) > 0 {
			if next == 0 || next > progressRequeue {
				if !hasTrue(st.Conditions, fpv1.ConditionReady) || st.Phase != fpv1.LeaseActive {
					next = progressRequeue
				}
			}
		}
	}
	r.setReadyLeases(key, seenNow)
	authorized := 0
	for _, res := range s.results {
		if len(res.Rendered) > 0 {
			authorized++
		}
	}
	r.recordGauges(key, active, authorized)
	return next, nil
}

func (r *PolicyReconciler) updatePolicyStatus(ctx context.Context, p *fpv1.FleetAccessPolicy, s *snapshot) (bool, error) {
	st := p.Status.DeepCopy()
	gen := p.Generation
	st.ObservedGeneration = gen
	st.Clusters = nil
	var ready int32
	var waiting, failed []string
	for _, c := range s.placed {
		ok, reason, msg := s.clusterInSync(c)
		cs := fpv1.ClusterStatus{Name: c, Ready: ok, Reason: reason, Message: msg}
		if res, found := s.results[c]; found {
			cs.ContentDigest = res.Digest
			cs.Grants = int32(len(res.Rendered))
		}
		st.Clusters = append(st.Clusters, cs)
		switch {
		case ok:
			ready++
		case reason == "DeliveryFailed" || reason == "ApplyFailed" || reason == "RejectedByEnforcement" || reason == "ClusterUnavailable":
			failed = append(failed, c)
		default:
			waiting = append(waiting, c)
		}
	}
	for c := range s.observed {
		if !containsString(s.placed, c) {
			waiting = append(waiting, c)
		}
	}
	st.SelectedClusters = int32(len(s.placed))
	st.ReadyClusters = ready
	st.ClusterSummary = fmt.Sprintf("%d/%d", ready, len(s.placed))
	var activeLeases int32
	for i := range s.leases {
		if s.decisions[s.leases[i].UID].Active() && len(s.renderedOn[s.leases[i].UID]) > 0 {
			activeLeases++
		}
	}
	st.ActiveLeases = activeLeases

	progressing := len(waiting) > 0
	switch {
	case s.placementErr != nil:
		setCond(&st.Conditions, gen, fpv1.ConditionReady, false, fpv1.ReasonPlacementNotFound, s.placementErr.Error()+"; no grants are delivered")
		setCond(&st.Conditions, gen, fpv1.ConditionDegraded, true, fpv1.ReasonPlacementNotFound, s.placementErr.Error())
	case len(failed) > 0:
		setCond(&st.Conditions, gen, fpv1.ConditionReady, false, fpv1.ReasonClustersFailed, "clusters not in the desired state: "+strings.Join(failed, ", "))
		setCond(&st.Conditions, gen, fpv1.ConditionDegraded, true, fpv1.ReasonClustersFailed, strings.Join(failed, ", "))
	case progressing:
		setCond(&st.Conditions, gen, fpv1.ConditionReady, false, fpv1.ReasonRollingOut, "waiting for: "+strings.Join(waiting, ", "))
		setCond(&st.Conditions, gen, fpv1.ConditionDegraded, false, fpv1.ReasonReconciled, "")
	case len(s.placed) == 0:
		setCond(&st.Conditions, gen, fpv1.ConditionReady, true, fpv1.ReasonNoClustersSelected, "the placement selects no clusters; nothing is granted")
		setCond(&st.Conditions, gen, fpv1.ConditionDegraded, false, fpv1.ReasonNoClustersSelected, "")
	default:
		setCond(&st.Conditions, gen, fpv1.ConditionReady, true, fpv1.ReasonReconciled, fmt.Sprintf("%d cluster(s) in the desired state", ready))
		setCond(&st.Conditions, gen, fpv1.ConditionDegraded, false, fpv1.ReasonReconciled, "")
	}
	if progressing {
		setCond(&st.Conditions, gen, fpv1.ConditionProgressing, true, fpv1.ReasonRollingOut, "waiting for: "+strings.Join(waiting, ", "))
	} else {
		setCond(&st.Conditions, gen, fpv1.ConditionProgressing, false, fpv1.ReasonReconciled, "no rollout in progress")
	}
	return progressing, r.patchPolicyStatus(ctx, p, st)
}

func (r *PolicyReconciler) finalize(ctx context.Context, p *fpv1.FleetAccessPolicy) error {
	observed, err := r.Placement.Observe(ctx, p)
	if err != nil {
		return err
	}
	for c := range observed {
		if err := r.Placement.Remove(ctx, p, c); err != nil {
			return err
		}
	}
	r.recordGauges(types.NamespacedName{Namespace: p.Namespace, Name: p.Name}, 0, 0)
	r.setReadyLeases(types.NamespacedName{Namespace: p.Namespace, Name: p.Name}, nil)
	if controllerutil.RemoveFinalizer(p, Finalizer) {
		return r.Update(ctx, p)
	}
	return nil
}

// denyOrphanLeases marks leases whose policy does not exist as Denied.
func (r *PolicyReconciler) denyOrphanLeases(ctx context.Context, key types.NamespacedName) error {
	var list fpv1.ToolAccessLeaseList
	if err := r.List(ctx, &list, client.InNamespace(key.Namespace), client.MatchingFields{PolicyIndexField: key.Name}); err != nil {
		return err
	}
	now := r.now()
	for i := range list.Items {
		l := &list.Items[i]
		d := sticky(l, lease.Evaluate(nil, l, nil, now))
		st := l.Status.DeepCopy()
		st.ObservedGeneration = l.Generation
		st.Clusters = nil
		st.ClusterCount = 0
		if d.Expired {
			st.Phase = fpv1.LeaseExpired
			setCond(&st.Conditions, l.Generation, fpv1.ConditionExpired, true, d.Reason, d.Message)
		} else {
			if !hasTrue(l.Status.Conditions, fpv1.ConditionDenied) {
				metrics.DeniedLeases.WithLabelValues(d.Reason).Inc()
			}
			st.Phase = fpv1.LeaseDenied
			setCond(&st.Conditions, l.Generation, fpv1.ConditionDenied, true, d.Reason, d.Message)
		}
		setCond(&st.Conditions, l.Generation, fpv1.ConditionReady, false, d.Reason, d.Message)
		setCond(&st.Conditions, l.Generation, fpv1.ConditionProgressing, false, d.Reason, "")
		if err := r.patchLeaseStatus(ctx, l, st); err != nil {
			return err
		}
	}
	return nil
}

func (r *PolicyReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *PolicyReconciler) recordGauges(key types.NamespacedName, active, clusters int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeByKey == nil {
		r.activeByKey = map[types.NamespacedName]int{}
		r.clustersByKy = map[types.NamespacedName]int{}
	}
	r.activeByKey[key] = active
	r.clustersByKy[key] = clusters
	totalActive, totalClusters := 0, 0
	for _, v := range r.activeByKey {
		totalActive += v
	}
	for _, v := range r.clustersByKy {
		totalClusters += v
	}
	metrics.ActiveLeases.Set(float64(totalActive))
	metrics.AuthorizedClusters.Set(float64(totalClusters))
}

func (r *PolicyReconciler) readyLeases(key types.NamespacedName) map[types.UID]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readySeen[key]
}

// setReadyLeases replaces the policy's set with the leases that are still
// live, which keeps it bounded; nil drops the policy.
func (r *PolicyReconciler) setReadyLeases(key types.NamespacedName, leases map[types.UID]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if leases == nil {
		delete(r.readySeen, key)
		return
	}
	if r.readySeen == nil {
		r.readySeen = map[types.NamespacedName]map[types.UID]bool{}
	}
	r.readySeen[key] = leases
}

// sticky keeps Denied and Expired terminal: a lease that was denied or has
// expired never becomes active again, even if the policy later widens.
// The state recorded first wins: a lease that already expired stays Expired
// even if its policy is later deleted or narrowed.
func sticky(l *fpv1.ToolAccessLease, d lease.Decision) lease.Decision {
	if c := findCond(l.Status.Conditions, fpv1.ConditionDenied); c != nil && c.Status == "True" {
		if !d.Denied {
			d.Reason, d.Message = c.Reason, c.Message
		}
		d.Denied, d.Expired = true, false
		d.Tools, d.Clusters = nil, nil
		return d
	}
	if c := findCond(l.Status.Conditions, fpv1.ConditionExpired); c != nil && c.Status == "True" {
		d.Denied, d.Expired, d.Reason, d.Message = false, true, c.Reason, c.Message
		d.Tools, d.Clusters = nil, nil
	}
	return d
}

func leaseLess(a, b *fpv1.ToolAccessLease) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}

func placementChanged(prev []fpv1.ClusterStatus, now []string) bool {
	if len(prev) != len(now) {
		return true
	}
	for i := range prev {
		if prev[i].Name != now[i] {
			return true
		}
	}
	return false
}
