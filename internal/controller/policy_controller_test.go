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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
	"github.com/fleetpermit/fleetpermit/internal/enforcement"
	"github.com/fleetpermit/fleetpermit/internal/enforcement/agenticnetworking"
	"github.com/fleetpermit/fleetpermit/internal/lease"
	"github.com/fleetpermit/fleetpermit/internal/metrics"
	"github.com/fleetpermit/fleetpermit/internal/placement"
)

const sreID = "spiffe://cluster.local/ns/agents/sa/sre-agent"

// fakePlacement selects a fixed set of clusters and reports every delivered
// cluster as enforcing its content, as if the work agent acknowledged at once.
type fakePlacement struct {
	placed      []string
	delivered   map[string]enforcement.Result
	withdrawals int
}

func (f *fakePlacement) SelectedClusters(context.Context, *fpv1.FleetAccessPolicy) ([]string, error) {
	return f.placed, nil
}

func (f *fakePlacement) Apply(_ context.Context, _ *fpv1.FleetAccessPolicy, c string, res enforcement.Result) error {
	if f.delivered == nil {
		f.delivered = map[string]enforcement.Result{}
	}
	f.delivered[c] = res
	return nil
}

func (f *fakePlacement) Remove(_ context.Context, _ *fpv1.FleetAccessPolicy, c string) error {
	delete(f.delivered, c)
	return nil
}

// Withdraw forgets every delivery when the policy is gone (keep is empty);
// the fake holds no deliveries of earlier policies with the same name.
func (f *fakePlacement) Withdraw(_ context.Context, _ types.NamespacedName, keep types.UID) error {
	if keep == "" {
		f.withdrawals++
		f.delivered = nil
	}
	return nil
}

func (f *fakePlacement) Observe(context.Context, *fpv1.FleetAccessPolicy) (map[string]placement.ClusterState, error) {
	out := map[string]placement.ClusterState{}
	for c, res := range f.delivered {
		out[c] = placement.ClusterState{Cluster: c, Ready: true, Reason: "Enforced", Digest: res.Digest}
	}
	return out, nil
}

func newTestReconciler(t *testing.T, pl placement.Provider, now time.Time, objs ...client.Object) (*PolicyReconciler, client.Client) {
	t.Helper()
	return newInterceptedReconciler(t, pl, now, interceptor.Funcs{}, objs...)
}

// newInterceptedReconciler is newTestReconciler with client calls routed
// through funcs, for example to inject errors.
func newInterceptedReconciler(t *testing.T, pl placement.Provider, now time.Time, funcs interceptor.Funcs, objs ...client.Object) (*PolicyReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := fpv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&fpv1.FleetAccessPolicy{}, &fpv1.ToolAccessLease{}).
		WithIndex(&fpv1.ToolAccessLease{}, PolicyIndexField, func(o client.Object) []string {
			return []string{o.(*fpv1.ToolAccessLease).Spec.PolicyRef.Name}
		}).
		WithInterceptorFuncs(funcs).
		WithObjects(objs...).Build()
	return &PolicyReconciler{Client: c, Placement: pl, Renderer: agenticnetworking.Renderer{}, Now: func() time.Time { return now }}, c
}

func testPolicy(name string) *fpv1.FleetAccessPolicy {
	return &fpv1.FleetAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "fleet", Name: name, UID: types.UID(name + "-uid")},
		Spec: fpv1.FleetAccessPolicySpec{
			Subjects:    []fpv1.Subject{{SPIFFEID: sreID}},
			Placement:   fpv1.PlacementSpec{PlacementRef: fpv1.LocalObjectReference{Name: "production"}},
			Target:      fpv1.TargetSpec{Namespace: "mcp-tools", Ref: fpv1.TargetRef{Name: "fleet-tools"}},
			Permissions: []fpv1.Permission{{Tool: "get_cluster_health"}, {Tool: "restart_workload"}},
		},
	}
}

func reconcileKey(t *testing.T, r *PolicyReconciler, key types.NamespacedName) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func key(o client.Object) types.NamespacedName {
	return types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}
}

// TestStandingGrantsBeyondCapacityAreReported checks that a standing policy
// whose subjects do not all fit the enforcement layer's rule limit is reported
// Degraded/CapacityExceeded instead of Ready.
func TestStandingGrantsBeyondCapacityAreReported(t *testing.T) {
	no := false
	p := testPolicy("readonly")
	p.Spec.Lease.Required = &no
	p.Spec.Subjects = nil
	for i := 0; i < 6; i++ {
		p.Spec.Subjects = append(p.Spec.Subjects, fpv1.Subject{SPIFFEID: fmt.Sprintf("spiffe://cluster.local/ns/agents/sa/agent-%d", i)})
	}
	r, c := newTestReconciler(t, &fakePlacement{placed: []string{"cluster-east", "cluster-west"}}, time.Now(), p)
	reconcileKey(t, r, key(p))

	var got fpv1.FleetAccessPolicy
	if err := c.Get(context.Background(), key(p), &got); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, fpv1.ConditionReady)
	degraded := meta.FindStatusCondition(got.Status.Conditions, fpv1.ConditionDegraded)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != fpv1.ReasonCapacityExceeded {
		t.Fatalf("Ready = %+v, want False/CapacityExceeded", ready)
	}
	if degraded == nil || degraded.Status != metav1.ConditionTrue || degraded.Reason != fpv1.ReasonCapacityExceeded ||
		!strings.Contains(degraded.Message, "cluster-east, cluster-west") {
		t.Fatalf("Degraded = %+v, want True/CapacityExceeded listing both clusters", degraded)
	}
}

// TestLeasePastItsRecordedExpiryIsExpiredWhenItsPolicyIsMissing checks that a
// lease whose recorded expiry has passed is marked Expired, not Denied, when
// its policy no longer exists.
func TestLeasePastItsRecordedExpiryIsExpiredWhenItsPolicyIsMissing(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	expiresAt := metav1.NewTime(now.Add(-time.Minute))
	l := &fpv1.ToolAccessLease{
		ObjectMeta: metav1.ObjectMeta{Namespace: "fleet", Name: "incident-1", CreationTimestamp: metav1.NewTime(now.Add(-11 * time.Minute))},
		Spec: fpv1.ToolAccessLeaseSpec{
			PolicyRef: fpv1.LocalObjectReference{Name: "deleted"}, Subject: fpv1.Subject{SPIFFEID: sreID},
			Permissions: []fpv1.Permission{{Tool: "restart_workload"}},
		},
		Status: fpv1.ToolAccessLeaseStatus{
			Phase: fpv1.LeaseActive, ExpiresAt: &expiresAt, Clusters: []string{"cluster-east"}, ClusterCount: 1,
			Conditions: []metav1.Condition{{Type: fpv1.ConditionReady, Status: metav1.ConditionTrue, Reason: fpv1.ReasonLeaseActive, LastTransitionTime: metav1.NewTime(now.Add(-10 * time.Minute))}},
		},
	}
	r, c := newTestReconciler(t, &fakePlacement{}, now, l)
	reconcileKey(t, r, types.NamespacedName{Namespace: "fleet", Name: "deleted"})

	var got fpv1.ToolAccessLease
	if err := c.Get(context.Background(), key(l), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != fpv1.LeaseExpired || !meta.IsStatusConditionTrue(got.Status.Conditions, fpv1.ConditionExpired) {
		t.Fatalf("phase %s, conditions %+v; want Expired", got.Status.Phase, got.Status.Conditions)
	}
	if meta.IsStatusConditionTrue(got.Status.Conditions, fpv1.ConditionDenied) {
		t.Fatalf("an expired lease must not be denied: %+v", got.Status.Conditions)
	}
}

// TestLeaseStatusPatchNeverOverwritesANewerState checks that a lease status
// written from a stale read fails with a conflict instead of overwriting a
// newer state, such as a terminal one.
func TestLeaseStatusPatchNeverOverwritesANewerState(t *testing.T) {
	ctx := context.Background()
	l := &fpv1.ToolAccessLease{
		ObjectMeta: metav1.ObjectMeta{Namespace: "fleet", Name: "incident-2"},
		Spec: fpv1.ToolAccessLeaseSpec{
			PolicyRef: fpv1.LocalObjectReference{Name: "sre"}, Subject: fpv1.Subject{SPIFFEID: sreID},
			Permissions: []fpv1.Permission{{Tool: "restart_workload"}},
		},
		Status: fpv1.ToolAccessLeaseStatus{Phase: fpv1.LeaseActive},
	}
	r, c := newTestReconciler(t, &fakePlacement{}, time.Now(), l)
	var stale fpv1.ToolAccessLease
	if err := c.Get(ctx, key(l), &stale); err != nil {
		t.Fatal(err)
	}
	// Another writer records the lease as expired after the stale read.
	var cur fpv1.ToolAccessLease
	if err := c.Get(ctx, key(l), &cur); err != nil {
		t.Fatal(err)
	}
	cur.Status.Phase = fpv1.LeaseExpired
	if err := c.Status().Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}

	st := stale.Status.DeepCopy()
	st.Clusters, st.ClusterCount = []string{"cluster-east"}, 1
	if err := r.patchLeaseStatus(ctx, &stale, st); !apierrors.IsConflict(err) {
		t.Fatalf("patching from a stale read returned %v, want a conflict", err)
	}
	if err := c.Get(ctx, key(l), &cur); err != nil {
		t.Fatal(err)
	}
	if cur.Status.Phase != fpv1.LeaseExpired || len(cur.Status.Clusters) != 0 {
		t.Fatalf("the newer status was overwritten: %+v", cur.Status)
	}
}

// TestFinalizeForgetsThePolicy checks that a deleted policy leaves nothing
// behind in the reconciler's per-policy state.
func TestFinalizeForgetsThePolicy(t *testing.T) {
	ctx := context.Background()
	p := testPolicy("sre")
	r, c := newTestReconciler(t, &fakePlacement{placed: []string{"cluster-east"}}, time.Now(), p)
	reconcileKey(t, r, key(p))
	if _, ok := r.activeByKey[key(p)]; !ok {
		t.Fatal("expected per-policy gauges after a reconcile")
	}
	if err := c.Get(ctx, key(p), p); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	reconcileKey(t, r, key(p))
	if err := c.Get(ctx, key(p), p); !apierrors.IsNotFound(err) {
		t.Fatalf("policy not finalized: %v", err)
	}
	_, active := r.activeByKey[key(p)]
	_, clusters := r.clustersByKy[key(p)]
	_, seen := r.readySeen[key(p)]
	if active || clusters || seen {
		t.Fatalf("finalized policy still tracked: active=%v clusters=%v readySeen=%v", active, clusters, seen)
	}
}

func TestConditionTime(t *testing.T) {
	at := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	fallback := at.Add(time.Hour)
	conds := []metav1.Condition{
		{Type: fpv1.ConditionDenied, Status: metav1.ConditionTrue, LastTransitionTime: metav1.NewTime(at)},
		{Type: fpv1.ConditionExpired, Status: metav1.ConditionFalse, LastTransitionTime: metav1.NewTime(at)},
	}
	if got := conditionTime(conds, fpv1.ConditionDenied, fallback); !got.Equal(at) {
		t.Fatalf("a True condition: got %s, want its transition time %s", got, at)
	}
	if got := conditionTime(conds, fpv1.ConditionExpired, fallback); !got.Equal(fallback) {
		t.Fatalf("a False condition: got %s, want the fallback", got)
	}
	if got := conditionTime(conds, fpv1.ConditionReady, fallback); !got.Equal(fallback) {
		t.Fatalf("a missing condition: got %s, want the fallback", got)
	}
}

// TestRevocationStart checks the reference time of
// fleetpermit_lease_revocation_seconds, including a lease that sticky reports
// as expired without an evaluated expiry (for example after its policy
// narrowed), which must not produce a sample measured from the zero time.
func TestRevocationStart(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	deniedAt, markedExpiredAt, expiry := now.Add(-3*time.Minute), now.Add(-2*time.Minute), now.Add(-5*time.Minute)
	recorded := metav1.NewTime(expiry)
	cond := func(typ string, at time.Time) metav1.Condition {
		return metav1.Condition{Type: typ, Status: metav1.ConditionTrue, LastTransitionTime: metav1.NewTime(at)}
	}
	withExpiry := leaseWith(cond(fpv1.ConditionExpired, markedExpiredAt))
	withExpiry.Status.ExpiresAt = &recorded
	cases := []struct {
		name string
		l    *fpv1.ToolAccessLease
		d    lease.Decision
		want time.Time
	}{
		{"denied: when it was denied", leaseWith(cond(fpv1.ConditionDenied, deniedAt)), lease.Decision{Denied: true}, deniedAt},
		{"denied in this reconcile", leaseWith(), lease.Decision{Denied: true}, now},
		{"expired: its expiry", leaseWith(), lease.Decision{Expired: true, ExpiresAt: expiry}, expiry},
		{"expired without an evaluated expiry: the recorded expiry", withExpiry, lease.Decision{Expired: true}, expiry},
		{"expired without any expiry: when it was marked Expired", leaseWith(cond(fpv1.ConditionExpired, markedExpiredAt)), lease.Decision{Expired: true}, markedExpiredAt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := revocationStart(tc.l, tc.d, now); !got.Equal(tc.want) {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestDeliveryBackoffGrowsAndResets(t *testing.T) {
	r := &PolicyReconciler{}
	k := types.NamespacedName{Namespace: "fleet", Name: "sre"}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		32 * time.Second, 64 * time.Second, maxRequeue, maxRequeue, maxRequeue}
	for i, w := range want {
		if got := r.deliveryBackoff(k, true); got != w {
			t.Fatalf("failure %d: backoff %s, want %s", i+1, got, w)
		}
	}
	if got := r.deliveryBackoff(k, false); got != 0 {
		t.Fatalf("a reconcile without failures must not back off, got %s", got)
	}
	if got := r.deliveryBackoff(k, true); got != failureRequeue {
		t.Fatalf("after a successful reconcile the first retry must be fast again, got %s", got)
	}
}

// TestPolicyAbsenceIsConfirmedBeforeLeasesAreDenied checks that a policy
// missing from the cache but present on the API server (the cache is behind)
// is not treated as deleted: its leases are not denied and nothing is
// withdrawn.
func TestPolicyAbsenceIsConfirmedBeforeLeasesAreDenied(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	p := testPolicy("sre")
	expiresAt := metav1.NewTime(now.Add(5 * time.Minute))
	l := &fpv1.ToolAccessLease{
		ObjectMeta: metav1.ObjectMeta{Namespace: "fleet", Name: "incident-3", CreationTimestamp: metav1.NewTime(now.Add(-5 * time.Minute))},
		Spec: fpv1.ToolAccessLeaseSpec{
			PolicyRef: fpv1.LocalObjectReference{Name: p.Name}, Subject: fpv1.Subject{SPIFFEID: sreID},
			Permissions: []fpv1.Permission{{Tool: "restart_workload"}},
		},
		Status: fpv1.ToolAccessLeaseStatus{Phase: fpv1.LeaseActive, ExpiresAt: &expiresAt},
	}
	fp := &fakePlacement{}
	r, c := newTestReconciler(t, fp, now, l)
	r.APIReader = fake.NewClientBuilder().WithScheme(c.Scheme()).WithObjects(p).Build()
	reconcileKey(t, r, key(p))

	var got fpv1.ToolAccessLease
	if err := c.Get(context.Background(), key(l), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != fpv1.LeaseActive || meta.IsStatusConditionTrue(got.Status.Conditions, fpv1.ConditionDenied) {
		t.Fatalf("a lease was denied because of a stale cache: %+v", got.Status)
	}
	if fp.withdrawals != 0 {
		t.Fatal("deliveries were withdrawn for a policy that exists")
	}
}

func newLease(name, policy, subject string, created time.Time, d time.Duration) *fpv1.ToolAccessLease {
	l := &fpv1.ToolAccessLease{
		ObjectMeta: metav1.ObjectMeta{Namespace: "fleet", Name: name, UID: types.UID(name + "-uid"), CreationTimestamp: metav1.NewTime(created)},
		Spec: fpv1.ToolAccessLeaseSpec{
			PolicyRef: fpv1.LocalObjectReference{Name: policy}, Subject: fpv1.Subject{SPIFFEID: subject},
			Permissions: []fpv1.Permission{{Tool: "restart_workload"}},
		},
	}
	if d > 0 {
		l.Spec.Duration = &metav1.Duration{Duration: d}
	}
	return l
}

// TestLeaseWaitsForAMissingPolicyForAGracePeriodOnly checks that a lease
// created before its policy waits as Pending only for a bounded time: it is
// denied once the grace period has passed without the policy, and expired
// once its requested duration has passed, whichever comes first.
func TestLeaseWaitsForAMissingPolicyForAGracePeriodOnly(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cases := []struct {
		name    string
		age, d  time.Duration
		phase   fpv1.LeasePhase
		reason  string
		requeue bool
	}{
		{"within the grace period", time.Minute, 10 * time.Minute, fpv1.LeasePending, fpv1.ReasonPolicyNotFound, true},
		{"without a duration, within the grace period", time.Minute, 0, fpv1.LeasePending, fpv1.ReasonPolicyNotFound, true},
		{"past the grace period", 6 * time.Minute, 30 * time.Minute, fpv1.LeaseDenied, fpv1.ReasonPolicyNotFound, false},
		{"without a duration, past the grace period", 6 * time.Minute, 0, fpv1.LeaseDenied, fpv1.ReasonPolicyNotFound, false},
		{"past its requested duration", 2 * time.Minute, time.Minute, fpv1.LeaseExpired, fpv1.ReasonLeaseExpired, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newLease("early", "missing", sreID, now.Add(-tc.age), tc.d)
			r, c := newTestReconciler(t, &fakePlacement{}, now, l)
			res := reconcileKey(t, r, types.NamespacedName{Namespace: "fleet", Name: "missing"})
			var got fpv1.ToolAccessLease
			if err := c.Get(context.Background(), key(l), &got); err != nil {
				t.Fatal(err)
			}
			ready := meta.FindStatusCondition(got.Status.Conditions, fpv1.ConditionReady)
			if got.Status.Phase != tc.phase || ready == nil || ready.Reason != tc.reason {
				t.Fatalf("phase %s, Ready %+v; want %s/%s", got.Status.Phase, ready, tc.phase, tc.reason)
			}
			if tc.requeue && (res.RequeueAfter <= 0 || res.RequeueAfter > policyGracePeriod) {
				t.Fatalf("a waiting lease must be checked again when its grace period ends, requeue %s", res.RequeueAfter)
			}
		})
	}
}

func propagationSamples(t *testing.T) uint64 {
	t.Helper()
	var m dto.Metric
	if err := metrics.PolicyPropagation.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}

// TestLeaseMetricsAreRecordedOnceWhenAStatusWriteConflicts checks that the
// denied, expired and propagation metrics count a lease once even when the
// first attempt to record its status fails and the reconcile is retried.
func TestLeaseMetricsAreRecordedOnceWhenAStatusWriteConflicts(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	p := testPolicy("sre")
	expired := newLease("a-expired", p.Name, sreID, now.Add(-20*time.Minute), 10*time.Minute)
	ready := newLease("b-ready", p.Name, sreID, now.Add(-2*time.Minute), 10*time.Minute)
	denied := newLease("c-denied", p.Name, "spiffe://cluster.local/ns/agents/sa/unknown", now.Add(-time.Minute), 10*time.Minute)
	failed := map[string]bool{}
	funcs := interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
		if _, isLease := obj.(*fpv1.ToolAccessLease); isLease && !failed[obj.GetName()] {
			failed[obj.GetName()] = true
			return apierrors.NewConflict(fpv1.GroupVersion.WithResource("toolaccessleases").GroupResource(), obj.GetName(), fmt.Errorf("changed"))
		}
		return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
	}}
	r, _ := newInterceptedReconciler(t, &fakePlacement{placed: []string{"cluster-east"}}, now, funcs, p, expired, ready, denied)

	deniedBefore := testutil.ToFloat64(metrics.DeniedLeases.WithLabelValues(fpv1.ReasonSubjectNotAllowed))
	expiredBefore := testutil.ToFloat64(metrics.ExpiredLeases)
	propagationBefore := propagationSamples(t)
	for i := 0; ; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key(p)}); err == nil {
			break
		} else if i == 10 {
			t.Fatal(err)
		}
	}
	if len(failed) != 3 {
		t.Fatalf("expected one failed status write per lease, got %v", failed)
	}
	if got := testutil.ToFloat64(metrics.DeniedLeases.WithLabelValues(fpv1.ReasonSubjectNotAllowed)) - deniedBefore; got != 1 {
		t.Errorf("denied leases counted %v times, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.ExpiredLeases) - expiredBefore; got != 1 {
		t.Errorf("expired leases counted %v times, want 1", got)
	}
	if got := propagationSamples(t) - propagationBefore; got != 1 {
		t.Errorf("propagation observed %d times, want 1", got)
	}
}
