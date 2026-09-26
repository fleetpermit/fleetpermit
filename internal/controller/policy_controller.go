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
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	"github.com/fleetpermit/fleetpermit/internal/placement/ocm"
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
	// failureRequeue is the first retry after delivery to a cluster failed;
	// it doubles while the failure persists, up to maxRequeue.
	failureRequeue = time.Second
	// maxStatusClusters is the API's limit on status.clusters.
	maxStatusClusters = 512
	// policyGracePeriod is how long a lease created before its policy waits
	// for the policy, as Pending, before it is denied.
	policyGracePeriod = 5 * time.Minute
)

// Cluster states reported by the controller itself.
const (
	reasonDeliveryFailed   = "DeliveryFailed"
	reasonDelivering       = "Delivering"
	reasonRevoking         = "Revoking"
	reasonUpdating         = "Updating"
	reasonNothingToEnforce = "NothingToEnforce"
)

var tracer = otel.Tracer("github.com/fleetpermit/fleetpermit/internal/controller")

// PolicyReconciler reconciles FleetAccessPolicies and their leases.
type PolicyReconciler struct {
	client.Client
	// APIReader reads from the API server instead of the cache. It confirms
	// that a policy is gone before its leases are denied and its deliveries
	// deleted. Defaults to the client.
	APIReader client.Reader
	Placement placement.Provider
	Renderer  enforcement.Renderer
	// WatchNamespace, when set, restricts reconciliation to policies in that
	// namespace, the only one whose FleetPermit objects the cache holds.
	WatchNamespace string
	// Now returns the current time. Tests replace it with a fake clock.
	Now func() time.Time

	mu           sync.Mutex
	activeByKey  map[types.NamespacedName]int
	clustersByKy map[types.NamespacedName]int
	// readySeen holds, per policy, the leases this process has seen Ready,
	// so propagation is observed once per lease and not again when a lease
	// returns to Ready after another lease or the placement changed.
	readySeen map[types.NamespacedName]map[types.UID]bool
	// placements holds, per policy, the clusters selected at its last
	// reconcile, so that only real placement changes are counted.
	placements map[types.NamespacedName][]string
	// failures counts, per policy, consecutive reconciles in which delivery
	// to a cluster failed; it sets the retry backoff.
	failures map[types.NamespacedName]int
}

// +kubebuilder:rbac:groups=fleetpermit.github.io,resources=fleetaccesspolicies,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=fleetpermit.github.io,resources=toolaccessleases,verbs=get;list;watch
// +kubebuilder:rbac:groups=fleetpermit.github.io,resources=fleetaccesspolicies/status;toolaccessleases/status,verbs=get;patch
// +kubebuilder:rbac:groups=cluster.open-cluster-management.io,resources=placements;placementdecisions;managedclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=work.open-cluster-management.io,resources=manifestworks,verbs=get;list;watch;create;patch;delete

// Reconcile brings the fleet in line with one FleetAccessPolicy.
func (r *PolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	if !r.watches(req.Namespace) {
		// The cache holds no FleetPermit objects from other namespaces.
		return ctrl.Result{}, nil
	}
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
			return r.reconcileMissing(ctx, req.NamespacedName)
		}
		return ctrl.Result{}, err
	}

	if !policy.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &policy)
	}
	if !controllerutil.ContainsFinalizer(&policy, Finalizer) {
		if err := r.patchFinalizer(ctx, &policy, controllerutil.AddFinalizer); err != nil {
			return ctrl.Result{}, err
		}
	}
	return r.reconcilePolicy(ctx, &policy)
}

// patchFinalizer adds or removes FleetPermit's finalizer with a merge patch
// that changes nothing else. An Update would write the whole object back and
// re-encode its durations ("15m" as "15m0s"), so the API server would
// validate a spec that an earlier CRD version accepted against rules added
// since, and could refuse the change, leaving the policy impossible to delete.
func (r *PolicyReconciler) patchFinalizer(ctx context.Context, p *fpv1.FleetAccessPolicy, change func(client.Object, string) bool) error {
	base := p.DeepCopy()
	change(p, Finalizer)
	return r.Patch(ctx, p, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
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
	// delivering holds clusters whose previous delivery is still being
	// deleted; they are delivered to once it is gone.
	delivering map[string]string
	// unavailable holds clusters whose delivery waits for the cluster to
	// reconnect.
	unavailable map[string]string
	// standingDropped lists the clusters on which a standing grant did not
	// fit the enforcement layer's rule limit.
	standingDropped []string
}

func (r *PolicyReconciler) reconcilePolicy(ctx context.Context, p *fpv1.FleetAccessPolicy) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	key := types.NamespacedName{Namespace: p.Namespace, Name: p.Name}
	s := snapshot{
		now:           r.now(),
		decisions:     map[types.UID]lease.Decision{},
		grants:        map[string][]enforcement.Grant{},
		results:       map[string]enforcement.Result{},
		droppedOn:     map[types.UID][]string{},
		renderedOn:    map[types.UID][]string{},
		applyFailures: map[string]string{},
		delivering:    map[string]string{},
		unavailable:   map[string]string{},
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
	if s.placementErr == nil && r.placementChanged(key, s.placed) {
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
			if _, isLease := s.decisions[types.UID(id)]; !isLease && !containsString(s.standingDropped, c) {
				s.standingDropped = append(s.standingDropped, c)
			}
		}
		switch err := r.Placement.Apply(ctx, p, c, res); {
		case errors.Is(err, placement.ErrStillDeleting):
			// Not a failure: delivery resumes once the deletion completes.
			s.delivering[c] = err.Error()
		case errors.Is(err, placement.ErrClusterUnavailable):
			// Nothing changes until the cluster reconnects, which the
			// ManagedCluster watch reports.
			s.unavailable[c] = err.Error()
		case err != nil:
			s.applyFailures[c] = err.Error()
			logger.Error(err, "delivering grants failed", "cluster", c)
		}
	}

	// 5. Withdraw everything from clusters that are no longer selected. They
	// stay in the observed state, and are reported as being revoked, until
	// their content is gone.
	observed, err := r.Placement.Observe(ctx, p)
	if err != nil {
		return ctrl.Result{}, err
	}
	for c := range observed {
		if !containsString(s.placed, c) {
			if err := r.Placement.Remove(ctx, p, c); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	s.observed = observed
	// An earlier policy with this namespace and name, deleted without its
	// finalizer, may have left deliveries behind, including on clusters this
	// policy is not placed on.
	if err := r.Placement.Withdraw(ctx, key, p.UID); err != nil {
		return ctrl.Result{}, err
	}

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
	if backoff := r.deliveryBackoff(key, len(s.applyFailures) > 0); backoff > 0 && (requeue == 0 || backoff < requeue) {
		// Delivery failed for at least one cluster: retry soon, backing off
		// while the failure persists.
		requeue = backoff
	}
	if requeue == 0 || requeue > maxRequeue {
		requeue = maxRequeue
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// clusterInSync reports whether a cluster runs exactly the desired content.
func (s *snapshot) clusterInSync(c string) (bool, string, string) {
	if msg, failed := s.applyFailures[c]; failed {
		return false, reasonDeliveryFailed, msg
	}
	if msg, down := s.unavailable[c]; down {
		return false, ocm.ReasonClusterUnavailable, msg
	}
	res, desired := s.results[c]
	obs, present := s.observed[c]
	if present && obs.Reason == ocm.ReasonClusterUnavailable {
		// Nothing changes on the cluster until it reconnects.
		return false, obs.Reason, obs.Message
	}
	if msg, waiting := s.delivering[c]; waiting {
		return false, reasonDelivering, msg
	}
	wantContent := desired && len(res.Objects) > 0
	switch {
	case !wantContent && !present:
		return true, reasonNothingToEnforce, "no active grants for this cluster"
	case !wantContent && present:
		return false, reasonRevoking, "withdrawing previously delivered grants"
	case wantContent && !present:
		return false, reasonDelivering, "grants are being delivered"
	case obs.Digest != res.Digest:
		return false, reasonUpdating, "the cluster holds a previous revision"
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
		// Metrics are recorded once the status is written, so that a lease is
		// not counted twice when the write fails and the reconcile is retried.
		var record []func()

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
				took := s.now.Sub(revocationStart(l, d, s.now)).Seconds()
				record = append(record, func() { metrics.LeaseRevocation.Observe(took) })
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
				setCond(&st.Conditions, gen, fpv1.ConditionProgressing, true, reasonRevoking, "withdrawing grants from: "+strings.Join(pending, ", "))
			} else {
				setCond(&st.Conditions, gen, fpv1.ConditionProgressing, false, reason, "grants withdrawn from every cluster")
			}
			setCond(&st.Conditions, gen, fpv1.ConditionDegraded, false, reason, "")
			if d.Denied && !wasDenied {
				record = append(record, metrics.DeniedLeases.WithLabelValues(d.Reason).Inc)
			}
			if d.Expired && !wasExpired {
				record = append(record, metrics.ExpiredLeases.Inc)
			}
		default:
			setCond(&st.Conditions, gen, fpv1.ConditionDenied, false, fpv1.ReasonAllowed, "lease satisfied the policy")
			setCond(&st.Conditions, gen, fpv1.ConditionExpired, false, fpv1.ReasonNotExpired, "expires at "+d.ExpiresAt.Format(time.RFC3339))
			rendered := sortedCopy(s.renderedOn[l.UID])
			dropped := sortedCopy(s.droppedOn[l.UID])
			// A cluster that left the placement holds the grant until its
			// delivery is gone, so it stays listed until then. It does not
			// count towards Ready.
			st.Clusters = rendered
			for _, c := range l.Status.Clusters {
				if _, held := s.observed[c]; held && !containsString(s.placed, c) {
					st.Clusters = append(st.Clusters, c)
				}
			}
			sort.Strings(st.Clusters)
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
				if deliveryFailed(reason) {
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
					took := s.now.Sub(l.CreationTimestamp.Time).Seconds()
					record = append(record, func() { metrics.PolicyPropagation.Observe(took) })
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
		for _, f := range record {
			f()
		}
		// A lease waiting for clusters needs no requeue of its own: those
		// clusters make the policy progressing. Failed clusters wait for
		// their own events or the delivery backoff.
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
		case deliveryFailed(reason):
			failed = append(failed, c)
		default:
			waiting = append(waiting, c)
		}
	}
	// Clusters that are no longer selected are listed until their content
	// is gone.
	for c := range s.observed {
		if !containsString(s.placed, c) {
			_, reason, msg := s.clusterInSync(c)
			st.Clusters = append(st.Clusters, fpv1.ClusterStatus{Name: c, Reason: reason, Message: msg})
			if deliveryFailed(reason) {
				failed = append(failed, c)
			} else {
				waiting = append(waiting, c)
			}
		}
	}
	sort.Slice(st.Clusters, func(i, j int) bool { return st.Clusters[i].Name < st.Clusters[j].Name })
	sort.Strings(waiting)
	sort.Strings(failed)
	truncated := ""
	if len(st.Clusters) > maxStatusClusters {
		truncated = fmt.Sprintf("; status.clusters lists %d of %d clusters, those not ready first", maxStatusClusters, len(st.Clusters))
		sort.SliceStable(st.Clusters, func(i, j int) bool { return !st.Clusters[i].Ready && st.Clusters[j].Ready })
		st.Clusters = st.Clusters[:maxStatusClusters]
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

	setReady := func(status bool, reason, msg string) {
		setCond(&st.Conditions, gen, fpv1.ConditionReady, status, reason, msg+truncated)
	}
	progressing := len(waiting) > 0
	switch {
	case s.placementErr != nil:
		setReady(false, fpv1.ReasonPlacementNotFound, s.placementErr.Error()+"; no grants are delivered")
		setCond(&st.Conditions, gen, fpv1.ConditionDegraded, true, fpv1.ReasonPlacementNotFound, s.placementErr.Error())
	case len(s.standingDropped) > 0:
		msg := "standing grants exceed the enforcement layer's rule limit on: " + strings.Join(s.standingDropped, ", ")
		setReady(false, fpv1.ReasonCapacityExceeded, msg)
		setCond(&st.Conditions, gen, fpv1.ConditionDegraded, true, fpv1.ReasonCapacityExceeded, msg)
	case len(failed) > 0:
		setReady(false, fpv1.ReasonClustersFailed, "clusters not in the desired state: "+strings.Join(failed, ", "))
		setCond(&st.Conditions, gen, fpv1.ConditionDegraded, true, fpv1.ReasonClustersFailed, strings.Join(failed, ", "))
	case progressing:
		setReady(false, fpv1.ReasonRollingOut, "waiting for: "+strings.Join(waiting, ", "))
		setCond(&st.Conditions, gen, fpv1.ConditionDegraded, false, fpv1.ReasonReconciled, "")
	case len(s.placed) == 0:
		setReady(true, fpv1.ReasonNoClustersSelected, "the placement selects no clusters; nothing is granted")
		setCond(&st.Conditions, gen, fpv1.ConditionDegraded, false, fpv1.ReasonNoClustersSelected, "")
	default:
		setReady(true, fpv1.ReasonReconciled, fmt.Sprintf("%d cluster(s) in the desired state", ready))
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
	r.forget(types.NamespacedName{Namespace: p.Namespace, Name: p.Name})
	if controllerutil.ContainsFinalizer(p, Finalizer) {
		return r.patchFinalizer(ctx, p, controllerutil.RemoveFinalizer)
	}
	return nil
}

// reconcileMissing handles a policy that does not exist: it withdraws what
// was delivered for it (its finalizer may have been removed by hand) and
// updates the leases that reference it.
func (r *PolicyReconciler) reconcileMissing(ctx context.Context, key types.NamespacedName) (ctrl.Result, error) {
	// The cache can lag behind the API server: confirm that the policy is
	// gone before withdrawing its deliveries or denying its leases. If it
	// exists, its watch event reconciles it once the cache catches up.
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, key, &fpv1.FleetAccessPolicy{}); !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	r.forget(key)
	if err := r.Placement.Withdraw(ctx, key, ""); err != nil {
		return ctrl.Result{}, err
	}
	next, err := r.updateOrphanLeases(ctx, key)
	return ctrl.Result{RequeueAfter: next}, err
}

// updateOrphanLeases updates the leases of a policy that does not exist and
// returns when to check them again. A lease that was never evaluated against
// the policy stays Pending for policyGracePeriod after its creation, because
// GitOps tools may apply a lease before its policy, and activates if the
// policy appears in that time; it expires if its requested duration runs out
// first, and is denied when the grace period ends. A lease that was evaluated
// is denied, or marked Expired if its recorded expiry has passed. Denied and
// Expired remain terminal.
func (r *PolicyReconciler) updateOrphanLeases(ctx context.Context, key types.NamespacedName) (time.Duration, error) {
	var list fpv1.ToolAccessLeaseList
	if err := r.List(ctx, &list, client.InNamespace(key.Namespace), client.MatchingFields{PolicyIndexField: key.Name}); err != nil {
		return 0, err
	}
	now := r.now()
	var next time.Duration
	for i := range list.Items {
		l := &list.Items[i]
		d := sticky(l, lease.Evaluate(nil, l, nil, now))
		wasDenied := hasTrue(l.Status.Conditions, fpv1.ConditionDenied)
		waiting := !wasDenied && neverEvaluated(l)
		graceEnds := l.CreationTimestamp.Add(policyGracePeriod)
		var requestedEnd time.Time
		if l.Spec.Duration != nil {
			requestedEnd = l.CreationTimestamp.Add(l.Spec.Duration.Duration).UTC().Truncate(time.Second)
		}
		st := l.Status.DeepCopy()
		st.ObservedGeneration = l.Generation
		st.Clusters = nil
		st.ClusterCount = 0
		var record []func()
		expire := func(at time.Time) {
			d.Reason, d.Message = fpv1.ReasonLeaseExpired, "lease expired at "+at.UTC().Format(time.RFC3339)
			st.Phase = fpv1.LeaseExpired
			setCond(&st.Conditions, l.Generation, fpv1.ConditionExpired, true, d.Reason, d.Message)
			record = append(record, metrics.ExpiredLeases.Inc)
		}
		switch {
		case d.Expired:
			st.Phase = fpv1.LeaseExpired
			setCond(&st.Conditions, l.Generation, fpv1.ConditionExpired, true, d.Reason, d.Message)
		case !wasDenied && st.ExpiresAt != nil && !now.Before(st.ExpiresAt.Time):
			// The lease expired before its policy disappeared.
			expire(st.ExpiresAt.Time)
		case waiting && !requestedEnd.IsZero() && !now.Before(requestedEnd):
			// Its requested duration ran out while it waited for the policy.
			expire(requestedEnd)
		case waiting && now.Before(graceEnds):
			d.Message += "; the lease activates if the policy is created before " + graceEnds.UTC().Format(time.RFC3339)
			st.Phase = fpv1.LeasePending
			check := graceEnds
			if !requestedEnd.IsZero() && requestedEnd.Before(check) {
				check = requestedEnd
			}
			if until := check.Sub(now) + 500*time.Millisecond; next == 0 || until < next {
				next = until
			}
		default:
			if !wasDenied {
				record = append(record, metrics.DeniedLeases.WithLabelValues(d.Reason).Inc)
			}
			st.Phase = fpv1.LeaseDenied
			setCond(&st.Conditions, l.Generation, fpv1.ConditionDenied, true, d.Reason, d.Message)
		}
		setCond(&st.Conditions, l.Generation, fpv1.ConditionReady, false, d.Reason, d.Message)
		setCond(&st.Conditions, l.Generation, fpv1.ConditionProgressing, false, d.Reason, "")
		if err := r.patchLeaseStatus(ctx, l, st); err != nil {
			return 0, err
		}
		for _, f := range record {
			f()
		}
	}
	return next, nil
}

// neverEvaluated reports whether a lease has never been evaluated against an
// existing policy: it has no recorded expiry and no state other than waiting
// for its policy.
func neverEvaluated(l *fpv1.ToolAccessLease) bool {
	if l.Status.ExpiresAt != nil || (l.Status.Phase != "" && l.Status.Phase != fpv1.LeasePending) {
		return false
	}
	for _, t := range []string{fpv1.ConditionDenied, fpv1.ConditionExpired, fpv1.ConditionReady} {
		if c := findCond(l.Status.Conditions, t); c != nil && (c.Status == metav1.ConditionTrue || c.Reason != fpv1.ReasonPolicyNotFound) {
			return false
		}
	}
	return true
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
	r.publishGauges()
}

// publishGauges sets the gauges to their totals over all policies. The
// caller holds r.mu.
func (r *PolicyReconciler) publishGauges() {
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

// forget drops everything this process holds for a policy that is gone.
func (r *PolicyReconciler) forget(key types.NamespacedName) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.activeByKey, key)
	delete(r.clustersByKy, key)
	delete(r.readySeen, key)
	delete(r.placements, key)
	delete(r.failures, key)
	r.publishGauges()
}

func (r *PolicyReconciler) readyLeases(key types.NamespacedName) map[types.UID]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.readySeen[key]
}

// setReadyLeases replaces the policy's set with the leases that are still
// live, which keeps it bounded.
func (r *PolicyReconciler) setReadyLeases(key types.NamespacedName, leases map[types.UID]bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.readySeen == nil {
		r.readySeen = map[types.NamespacedName]map[types.UID]bool{}
	}
	r.readySeen[key] = leases
}

// placementChanged records the clusters selected for a policy and reports
// whether they differ from those at its previous reconcile in this process.
func (r *PolicyReconciler) placementChanged(key types.NamespacedName, placed []string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, seen := r.placements[key]
	if r.placements == nil {
		r.placements = map[types.NamespacedName][]string{}
	}
	r.placements[key] = slices.Clone(placed)
	return seen && !slices.Equal(prev, placed)
}

// deliveryBackoff returns when to retry a policy after a reconcile in which
// delivery failed: failureRequeue, doubled for each consecutive failed
// reconcile up to maxRequeue. A reconcile without failures resets it and
// returns zero.
func (r *PolicyReconciler) deliveryBackoff(key types.NamespacedName, failed bool) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !failed {
		delete(r.failures, key)
		return 0
	}
	if r.failures == nil {
		r.failures = map[types.NamespacedName]int{}
	}
	n := r.failures[key]
	r.failures[key] = n + 1
	return min(failureRequeue<<min(n, 8), maxRequeue)
}

// revocationStart is when a terminal lease stopped granting: when it was
// denied, or when it expired. A lease that sticky reports as expired may have
// no evaluated expiry; its recorded expiry is used then, and failing that the
// time it was marked Expired.
func revocationStart(l *fpv1.ToolAccessLease, d lease.Decision, now time.Time) time.Time {
	switch {
	case d.Denied:
		return conditionTime(l.Status.Conditions, fpv1.ConditionDenied, now)
	case !d.ExpiresAt.IsZero():
		return d.ExpiresAt
	case l.Status.ExpiresAt != nil:
		return l.Status.ExpiresAt.Time
	default:
		return conditionTime(l.Status.Conditions, fpv1.ConditionExpired, now)
	}
}

// deliveryFailed reports whether a cluster's reason is a failure rather than
// a rollout or withdrawal in progress.
func deliveryFailed(reason string) bool {
	switch reason {
	case reasonDeliveryFailed, ocm.ReasonApplyFailed, ocm.ReasonRejected, ocm.ReasonClusterUnavailable:
		return true
	}
	return false
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
