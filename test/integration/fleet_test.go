//go:build integration

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

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	clusterv1beta1 "open-cluster-management.io/api/cluster/v1beta1"
	workv1 "open-cluster-management.io/api/work/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
	"github.com/fleetpermit/fleetpermit/internal/controller"
	"github.com/fleetpermit/fleetpermit/internal/enforcement/agenticnetworking"
	"github.com/fleetpermit/fleetpermit/internal/metrics"
	"github.com/fleetpermit/fleetpermit/internal/placement/ocm"
)

// fleet is a simulated OCM hub: managed clusters, a placement and its decision.
type fleet struct {
	ns        string
	placement string
	clusters  []string
}

func newFleet(t testing.TB, ns string, clusters []string, selected []string) *fleet {
	t.Helper()
	ctx := context.Background()
	createNamespace(t, ns)
	for _, c := range clusters {
		createNamespace(t, c)
		mc := &clusterv1.ManagedCluster{ObjectMeta: metav1.ObjectMeta{Name: c}, Spec: clusterv1.ManagedClusterSpec{HubAcceptsClient: true}}
		if err := k8s.Create(ctx, mc); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
		setClusterAvailable(t, c, true)
	}
	f := &fleet{ns: ns, placement: "production-clusters", clusters: clusters}
	t.Cleanup(func() { f.cleanUp(t) })
	pl := &clusterv1beta1.Placement{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: f.placement}}
	if err := k8s.Create(ctx, pl); err != nil {
		t.Fatal(err)
	}
	// OCM's placement controller labels the decisions it writes and makes
	// the Placement their controller.
	pd := &clusterv1beta1.PlacementDecision{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: f.placement + "-decision-1",
		Labels:          map[string]string{clusterv1beta1.PlacementLabel: f.placement},
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(pl, clusterv1beta1.SchemeGroupVersion.WithKind("Placement"))},
	}}
	if err := k8s.Create(ctx, pd); err != nil {
		t.Fatal(err)
	}
	f.selectClusters(t, selected...)
	return f
}

// cleanUp removes what a fleet test created, in the fleet's namespace and
// its clusters' namespaces, after the test's controller has stopped. The
// namespaces themselves stay: envtest never finishes deleting a namespace.
func (f *fleet) cleanUp(t testing.TB) {
	t.Helper()
	ctx := context.Background()
	lists := []client.ObjectList{
		&fpv1.ToolAccessLeaseList{}, &fpv1.FleetAccessPolicyList{},
		&clusterv1beta1.PlacementDecisionList{}, &clusterv1beta1.PlacementList{},
	}
	for _, list := range lists {
		if err := k8s.List(ctx, list, client.InNamespace(f.ns)); err != nil {
			t.Error(err)
			continue
		}
		_ = meta.EachListItem(list, func(o runtime.Object) error {
			remove(t, o.(client.Object))
			return nil
		})
	}
	// One list for every cluster: a list per cluster namespace is slow with
	// hundreds of clusters.
	var works workv1.ManifestWorkList
	if err := k8s.List(ctx, &works); err != nil {
		t.Error(err)
	}
	for i := range works.Items {
		if slices.Contains(f.clusters, works.Items[i].Namespace) {
			remove(t, &works.Items[i])
		}
	}
	for _, c := range f.clusters {
		remove(t, &clusterv1.ManagedCluster{ObjectMeta: metav1.ObjectMeta{Name: c}})
	}
}

func setClusterAvailable(t testing.TB, name string, available bool) {
	t.Helper()
	ctx := context.Background()
	var mc clusterv1.ManagedCluster
	if err := k8s.Get(ctx, types.NamespacedName{Name: name}, &mc); err != nil {
		t.Fatal(err)
	}
	status := metav1.ConditionTrue
	if !available {
		status = metav1.ConditionUnknown
	}
	meta.SetStatusCondition(&mc.Status.Conditions, metav1.Condition{
		Type: clusterv1.ManagedClusterConditionAvailable, Status: status, Reason: "Test", Message: "set by test",
	})
	if err := k8s.Status().Update(ctx, &mc); err != nil {
		t.Fatal(err)
	}
}

func (f *fleet) selectClusters(t testing.TB, clusters ...string) {
	t.Helper()
	ctx := context.Background()
	var pd clusterv1beta1.PlacementDecision
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: f.ns, Name: f.placement + "-decision-1"}, &pd); err != nil {
		t.Fatal(err)
	}
	pd.Status.Decisions = nil
	for _, c := range clusters {
		pd.Status.Decisions = append(pd.Status.Decisions, clusterv1beta1.ClusterDecision{ClusterName: c, Reason: "selected"})
	}
	if err := k8s.Status().Update(ctx, &pd); err != nil {
		t.Fatal(err)
	}
}

func (f *fleet) policy(name string, mutate ...func(*fpv1.FleetAccessPolicy)) *fpv1.FleetAccessPolicy {
	p := validPolicy(f.ns, name)
	p.Spec.Placement.PlacementRef.Name = f.placement
	for _, m := range mutate {
		m(p)
	}
	return p
}

func (f *fleet) lease(name, policy string, d time.Duration, tools ...string) *fpv1.ToolAccessLease {
	l := &fpv1.ToolAccessLease{
		ObjectMeta: metav1.ObjectMeta{Namespace: f.ns, Name: name},
		Spec: fpv1.ToolAccessLeaseSpec{
			PolicyRef: fpv1.LocalObjectReference{Name: policy},
			Subject:   fpv1.Subject{SPIFFEID: sreID},
			Duration:  &metav1.Duration{Duration: d},
		},
	}
	for _, tool := range tools {
		l.Spec.Permissions = append(l.Spec.Permissions, fpv1.Permission{Tool: tool})
	}
	return l
}

// works returns cluster -> ManifestWork for a policy.
func works(t testing.TB, p *fpv1.FleetAccessPolicy) map[string]workv1.ManifestWork {
	t.Helper()
	var list workv1.ManifestWorkList
	if err := k8s.List(context.Background(), &list, client.MatchingLabels{ocm.LabelPolicyUID: string(p.UID)}); err != nil {
		t.Fatal(err)
	}
	out := map[string]workv1.ManifestWork{}
	for _, w := range list.Items {
		if w.DeletionTimestamp.IsZero() {
			out[w.Namespace] = w
		}
	}
	return out
}

// grantWorks returns cluster -> ManifestWork for works that carry at least
// one grant (placed clusters without grants hold an inert policy).
func grantWorks(t testing.TB, p *fpv1.FleetAccessPolicy) map[string]workv1.ManifestWork {
	t.Helper()
	out := map[string]workv1.ManifestWork{}
	for c, w := range works(t, p) {
		if !strings.Contains(manifestJSON(w), agenticnetworking.InertRuleName) {
			out[c] = w
		}
	}
	return out
}

func clusterSet(m map[string]workv1.ManifestWork) string {
	var names []string
	for c := range m {
		names = append(names, c)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// ackOption changes what the simulated work agent does.
type ackOption int

// holdDeletion makes the simulated work agent add the OCM work agent's
// finalizer to every work, so a deleted ManifestWork remains, marked for
// deletion, until releaseWorks, as it does while a real agent removes the
// delivered objects from the managed cluster.
const holdDeletion ackOption = 1

// ackWorks plays the role of the OCM work agent and the enforcement
// controller: it marks every FleetPermit ManifestWork as applied and
// accepted, and reports the delivered object's content digest.
func ackWorks(t testing.TB, opts ...ackOption) {
	t.Helper()
	ctx := context.Background()
	var list workv1.ManifestWorkList
	if err := k8s.List(ctx, &list, client.MatchingLabels{ocm.LabelManagedBy: ocm.ManagedByValue}); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		w := &list.Items[i]
		if slices.Contains(opts, holdDeletion) && w.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(w, workv1.ManifestWorkFinalizer) {
			controllerutil.AddFinalizer(w, workv1.ManifestWorkFinalizer)
			if err := k8s.Update(ctx, w); apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				continue // the next round retries
			} else if err != nil {
				t.Fatal(err)
			}
		}
		if c := meta.FindStatusCondition(w.Status.Conditions, workv1.WorkApplied); c != nil && c.ObservedGeneration == w.Generation {
			continue
		}
		for _, typ := range []string{workv1.WorkApplied, workv1.WorkAvailable} {
			meta.SetStatusCondition(&w.Status.Conditions, metav1.Condition{
				Type: typ, Status: metav1.ConditionTrue, Reason: "Test", ObservedGeneration: w.Generation,
			})
		}
		w.Status.ResourceStatus.Manifests = nil
		accepted := "True"
		for j, m := range w.Spec.Workload.Manifests {
			var obj struct {
				Metadata struct {
					Name, Namespace string
					Annotations     map[string]string
				} `json:"metadata"`
			}
			_ = json.Unmarshal(m.Raw, &obj)
			values := []workv1.FeedbackValue{{Name: ocm.FeedbackAccepted, Value: workv1.FieldValue{Type: workv1.String, String: &accepted}}}
			if d, ok := obj.Metadata.Annotations[agenticnetworking.AnnotationDigest]; ok {
				values = append(values, workv1.FeedbackValue{Name: ocm.FeedbackDigest, Value: workv1.FieldValue{Type: workv1.String, String: &d}})
			}
			w.Status.ResourceStatus.Manifests = append(w.Status.ResourceStatus.Manifests, workv1.ManifestCondition{
				ResourceMeta: workv1.ManifestResourceMeta{Ordinal: int32(j), Group: "agentic.networking.x-k8s.io", Version: "v1alpha1",
					Kind: "XAccessPolicy", Resource: "xaccesspolicies", Name: obj.Metadata.Name, Namespace: obj.Metadata.Namespace},
				StatusFeedbacks: workv1.StatusFeedbackResult{Values: values},
				Conditions:      []metav1.Condition{{Type: workv1.ManifestApplied, Status: metav1.ConditionTrue, Reason: "Test", LastTransitionTime: metav1.Now()}},
			})
		}
		if err := k8s.Status().Update(ctx, w); err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
	}
}

// releaseWorks plays the work agent finishing its cleanup: it removes the
// finalizer added by ackWorks(t, holdDeletion) from every ManifestWork, so
// works marked for deletion go away.
func releaseWorks(t testing.TB) {
	t.Helper()
	ctx := context.Background()
	var list workv1.ManifestWorkList
	if err := k8s.List(ctx, &list, client.MatchingLabels{ocm.LabelManagedBy: ocm.ManagedByValue}); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		w := &list.Items[i]
		if !controllerutil.ContainsFinalizer(w, workv1.ManifestWorkFinalizer) {
			continue
		}
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := k8s.Get(ctx, clientKey(w), w); err != nil {
				return err
			}
			if !controllerutil.RemoveFinalizer(w, workv1.ManifestWorkFinalizer) {
				return nil
			}
			return k8s.Update(ctx, w)
		})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
	}
}

func getLease(t testing.TB, l *fpv1.ToolAccessLease) *fpv1.ToolAccessLease {
	t.Helper()
	out := &fpv1.ToolAccessLease{}
	if err := k8s.Get(context.Background(), clientKey(l), out); err != nil {
		t.Fatal(err)
	}
	return out
}

// poke changes an annotation so the controller reconciles now, which is how
// tests make the reconciler observe a moved fake clock.
// It retries on conflicts, because the controller may patch the object's
// status between the read and the update.
func poke(t testing.TB, obj client.Object) {
	t.Helper()
	ctx := context.Background()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8s.Get(ctx, clientKey(obj), obj); err != nil {
			return err
		}
		a := obj.GetAnnotations()
		if a == nil {
			a = map[string]string{}
		}
		a["test.fleetpermit.github.io/poke"] = fmt.Sprint(time.Now().UnixNano())
		obj.SetAnnotations(a)
		return k8s.Update(ctx, obj)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// updatePolicy applies mutate to the current policy, retrying on conflicts
// with the controller's status writes.
func updatePolicy(t testing.TB, p *fpv1.FleetAccessPolicy, mutate func(*fpv1.FleetAccessPolicy)) {
	t.Helper()
	ctx := context.Background()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur fpv1.FleetAccessPolicy
		if err := k8s.Get(ctx, clientKey(p), &cur); err != nil {
			return err
		}
		mutate(&cur)
		return k8s.Update(ctx, &cur)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// updateWork applies mutate to the current ManifestWork, retrying on
// conflicts with the controller and the simulated work agent.
func updateWork(t testing.TB, w *workv1.ManifestWork, mutate func(*workv1.ManifestWork)) {
	t.Helper()
	ctx := context.Background()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur workv1.ManifestWork
		if err := k8s.Get(ctx, clientKey(w), &cur); err != nil {
			return err
		}
		mutate(&cur)
		return k8s.Update(ctx, &cur)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func waitLeasePhase(t testing.TB, l *fpv1.ToolAccessLease, phase fpv1.LeasePhase, reason string) *fpv1.ToolAccessLease {
	t.Helper()
	var got *fpv1.ToolAccessLease
	eventually(t, 15*time.Second, fmt.Sprintf("lease %s to be %s", l.Name, phase), func() error {
		ackWorks(t)
		got = getLease(t, l)
		if got.Status.Phase != phase {
			return fmt.Errorf("phase %q, conditions %+v", got.Status.Phase, got.Status.Conditions)
		}
		if reason != "" {
			ok := false
			for _, c := range got.Status.Conditions {
				if c.Reason == reason && c.Status == metav1.ConditionTrue {
					ok = true
				}
			}
			if !ok {
				return fmt.Errorf("no true condition with reason %s: %+v", reason, got.Status.Conditions)
			}
		}
		return nil
	})
	return got
}

func manifestJSON(w workv1.ManifestWork) string {
	var b strings.Builder
	for _, m := range w.Spec.Workload.Manifests {
		b.Write(m.Raw)
	}
	return b.String()
}

func TestLeaseLifecycleAcrossFleet(t *testing.T) {
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Second)
	clock := &fakeClock{t: start}
	stop := startController(t, clock.Now)
	defer stop()

	f := newFleet(t, "lifecycle", []string{"lc-east", "lc-west", "lc-edge"}, []string{"lc-east", "lc-west"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	// Scenario 4: a policy alone grants nothing.
	eventually(t, 10*time.Second, "policy status", func() error {
		ackWorks(t)
		var got fpv1.FleetAccessPolicy
		if err := k8s.Get(ctx, clientKey(p), &got); err != nil {
			return err
		}
		if got.Status.ClusterSummary != "2/2" || !meta.IsStatusConditionTrue(got.Status.Conditions, fpv1.ConditionReady) {
			return fmt.Errorf("status %+v", got.Status)
		}
		return nil
	})
	if w := grantWorks(t, p); len(w) != 0 {
		t.Fatalf("a policy without leases must grant nothing, got grants on %s", clusterSet(w))
	}
	eventually(t, 10*time.Second, "inert policies on the selected clusters only", func() error {
		if got := clusterSet(works(t, p)); got != "lc-east,lc-west" {
			return fmt.Errorf("works on %q", got)
		}
		return nil
	})

	// Scenario 1 and 6: a lease activates only on the placement's clusters.
	l := f.lease("incident-42", p.Name, 2*time.Minute, "restart_workload")
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "ManifestWorks on selected clusters", func() error {
		if got := clusterSet(works(t, p)); got != "lc-east,lc-west" {
			return fmt.Errorf("works on %q", got)
		}
		return nil
	})
	got := waitLeasePhase(t, l, fpv1.LeaseActive, fpv1.ReasonLeaseActive)
	if strings.Join(got.Status.Clusters, ",") != "lc-east,lc-west" || got.Status.ExpiresAt == nil {
		t.Fatalf("unexpected lease status %+v", got.Status)
	}
	wantExpiry := got.CreationTimestamp.Add(2 * time.Minute).UTC().Format(time.RFC3339)
	for c, w := range works(t, p) {
		body := manifestJSON(w)
		want := fmt.Sprintf("request.mcp.tool_name in ['restart_workload'] \\u0026\\u0026 request.time \\u003c timestamp('%s')", wantExpiry)
		if !strings.Contains(body, want) && !strings.Contains(body, strings.NewReplacer(`&`, "&", `<`, "<").Replace(want)) {
			t.Fatalf("%s: rendered policy lacks the time-bounded CEL rule: %s", c, body)
		}
		if !strings.Contains(body, sreID) || strings.Contains(body, "read_secret") {
			t.Fatalf("%s: unexpected rendered content %s", c, body)
		}
		if w.Annotations[ocm.AnnotationDigest] == "" {
			t.Fatalf("%s: ManifestWork has no content digest", c)
		}
	}

	// Scenario 5: the lease expires.
	clock.Set(got.CreationTimestamp.Add(2*time.Minute + time.Second))
	poke(t, l)
	waitLeasePhase(t, l, fpv1.LeaseExpired, fpv1.ReasonLeaseExpired)
	eventually(t, 10*time.Second, "grants withdrawn", func() error {
		if w := grantWorks(t, p); len(w) != 0 {
			return fmt.Errorf("grants remain on %s", clusterSet(w))
		}
		return nil
	})
	eventually(t, 10*time.Second, "revocation to be reported complete", func() error {
		ackWorks(t)
		final := getLease(t, l)
		if len(final.Status.Clusters) != 0 || meta.IsStatusConditionTrue(final.Status.Conditions, fpv1.ConditionProgressing) {
			return fmt.Errorf("revocation not complete: %+v", final.Status)
		}
		return nil
	})
}

func TestLeaseEscalationsAreDenied(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()

	f := newFleet(t, "escalation", []string{"es-east"}, []string{"es-east"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	perm := f.lease("wants-secrets", p.Name, time.Minute, "restart_workload", "read_secret")
	dur := f.lease("wants-an-hour", p.Name, time.Hour, "restart_workload")
	subj := f.lease("wrong-subject", p.Name, time.Minute, "restart_workload")
	subj.Spec.Subject.SPIFFEID = securityID
	outside := f.lease("outside-placement", p.Name, time.Minute, "restart_workload")
	outside.Spec.Clusters = []string{"es-edge"}
	for _, l := range []*fpv1.ToolAccessLease{perm, dur, subj, outside} {
		if err := k8s.Create(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	waitLeasePhase(t, perm, fpv1.LeaseDenied, fpv1.ReasonPermissionNotAllowed) // Scenario 8
	waitLeasePhase(t, dur, fpv1.LeaseDenied, fpv1.ReasonDurationExceedsMax)    // Scenario 9
	waitLeasePhase(t, subj, fpv1.LeaseDenied, fpv1.ReasonSubjectNotAllowed)    // Scenario 3
	waitLeasePhase(t, outside, fpv1.LeasePending, "")                          // placement expansion attempt
	time.Sleep(time.Second)
	if w := grantWorks(t, p); len(w) != 0 {
		t.Fatalf("denied leases must grant nothing, got grants on %s", clusterSet(w))
	}

	// A denied lease stays denied even if the policy later widens.
	updatePolicy(t, p, func(cur *fpv1.FleetAccessPolicy) {
		cur.Spec.Permissions = append(cur.Spec.Permissions, fpv1.Permission{Tool: "read_secret"})
	})
	time.Sleep(2 * time.Second)
	if got := getLease(t, perm); got.Status.Phase != fpv1.LeaseDenied {
		t.Fatalf("a denied lease must not activate after the policy widens: %s", got.Status.Phase)
	}
}

func TestPlacementChangesMoveGrants(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()

	f := newFleet(t, "placement", []string{"pl-east", "pl-west", "pl-edge"}, []string{"pl-east", "pl-west"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	l := f.lease("incident-7", p.Name, 5*time.Minute, "restart_workload")
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	waitLeasePhase(t, l, fpv1.LeaseActive, fpv1.ReasonLeaseActive)

	// Scenario 7: edge joins, west leaves.
	f.selectClusters(t, "pl-east", "pl-edge")
	eventually(t, 10*time.Second, "grants to follow the placement", func() error {
		if got := clusterSet(works(t, p)); got != "pl-east,pl-edge" {
			return fmt.Errorf("works on %q", got)
		}
		return nil
	})
	eventually(t, 10*time.Second, "lease status to follow the placement", func() error {
		ackWorks(t)
		if got := getLease(t, l); strings.Join(got.Status.Clusters, ",") != "pl-east,pl-edge" {
			return fmt.Errorf("lease clusters %v", got.Status.Clusters)
		}
		return nil
	})

	// An unavailable cluster is reported as degraded, not silently ready.
	setClusterAvailable(t, "pl-edge", false)
	eventually(t, 10*time.Second, "degraded status", func() error {
		var got fpv1.FleetAccessPolicy
		if err := k8s.Get(ctx, clientKey(p), &got); err != nil {
			return err
		}
		if !meta.IsStatusConditionTrue(got.Status.Conditions, fpv1.ConditionDegraded) {
			return fmt.Errorf("conditions %+v", got.Status.Conditions)
		}
		return nil
	})
	setClusterAvailable(t, "pl-edge", true)

	// Fail closed: deleting the placement withdraws every grant.
	if err := k8s.Delete(ctx, &clusterv1beta1.Placement{ObjectMeta: metav1.ObjectMeta{Namespace: f.ns, Name: f.placement}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "grants withdrawn after placement deletion", func() error {
		if w := works(t, p); len(w) != 0 {
			return fmt.Errorf("works remain on %s", clusterSet(w))
		}
		return nil
	})
}

func TestTamperedOrDeletedManifestWorkIsRestored(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()

	f := newFleet(t, "drift", []string{"dr-east"}, []string{"dr-east"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	l := f.lease("incident-9", p.Name, 5*time.Minute, "restart_workload")
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	waitLeasePhase(t, l, fpv1.LeaseActive, fpv1.ReasonLeaseActive)
	orig := works(t, p)["dr-east"]
	want := manifestJSON(orig)

	// Broaden the delivered policy directly on the hub.
	updateWork(t, &orig, func(w *workv1.ManifestWork) {
		w.Spec.Workload.Manifests[0].Raw = []byte(strings.Replace(string(w.Spec.Workload.Manifests[0].Raw), "restart_workload", "read_secret", 1))
	})
	eventually(t, 10*time.Second, "tampered ManifestWork to be restored", func() error {
		if got := manifestJSON(works(t, p)["dr-east"]); strings.Contains(got, "read_secret") || got == "" {
			return fmt.Errorf("still tampered: %s", got)
		}
		return nil
	})

	// Delete it outright.
	w := works(t, p)["dr-east"]
	if err := k8s.Delete(ctx, &w); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "deleted ManifestWork to be recreated", func() error {
		got, ok := works(t, p)["dr-east"]
		if !ok || got.UID == w.UID {
			return fmt.Errorf("not recreated yet")
		}
		if !jsonEqual(manifestJSON(got), want) {
			return fmt.Errorf("recreated with different content")
		}
		return nil
	})
}

func jsonEqual(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return a == b
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}

func TestConcurrentLeasesAreIndependent(t *testing.T) {
	ctx := context.Background()
	start := time.Now().UTC()
	clock := &fakeClock{t: start}
	stop := startController(t, clock.Now)
	defer stop()

	f := newFleet(t, "concurrent", []string{"cc-east"}, []string{"cc-east"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	short := f.lease("short", p.Name, time.Minute, "get_cluster_health")
	long := f.lease("long", p.Name, 5*time.Minute, "restart_workload")
	for _, l := range []*fpv1.ToolAccessLease{short, long} {
		if err := k8s.Create(ctx, l); err != nil {
			t.Fatal(err)
		}
	}
	waitLeasePhase(t, short, fpv1.LeaseActive, fpv1.ReasonLeaseActive)
	waitLeasePhase(t, long, fpv1.LeaseActive, fpv1.ReasonLeaseActive)
	body := manifestJSON(works(t, p)["cc-east"])
	if !strings.Contains(body, "get_cluster_health") || !strings.Contains(body, "restart_workload") {
		t.Fatalf("both grants must be rendered side by side: %s", body)
	}
	s := getLease(t, short)
	clock.Set(s.CreationTimestamp.Add(time.Minute + time.Second))
	poke(t, short)
	waitLeasePhase(t, short, fpv1.LeaseExpired, fpv1.ReasonLeaseExpired)
	eventually(t, 10*time.Second, "only the long lease to remain", func() error {
		body := manifestJSON(works(t, p)["cc-east"])
		if strings.Contains(body, "get_cluster_health") || !strings.Contains(body, "restart_workload") {
			return fmt.Errorf("unexpected content %s", body)
		}
		return nil
	})
	if got := getLease(t, long); got.Status.Phase != fpv1.LeaseActive {
		t.Fatalf("the long lease must stay active, got %s", got.Status.Phase)
	}
}

func TestControllerRestartKeepsGrants(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)

	f := newFleet(t, "restart", []string{"rs-east", "rs-west"}, []string{"rs-east", "rs-west"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	l := f.lease("incident-11", p.Name, 5*time.Minute, "restart_workload")
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	waitLeasePhase(t, l, fpv1.LeaseActive, fpv1.ReasonLeaseActive)
	before := works(t, p)
	stop()

	// Scenario 15: a fresh controller adopts existing state without
	// withdrawing or re-creating anything.
	stop = startController(t, clock.Now)
	defer stop()
	time.Sleep(3 * time.Second)
	after := works(t, p)
	for c, w := range before {
		a, ok := after[c]
		if !ok || a.UID != w.UID || a.Generation != w.Generation {
			t.Fatalf("%s: ManifestWork was disturbed by the restart", c)
		}
	}
	if got := getLease(t, l); got.Status.Phase != fpv1.LeaseActive {
		t.Fatalf("lease phase after restart: %s", got.Status.Phase)
	}
}

func TestPolicyDeletionWithdrawsAndDeniesLeases(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()

	f := newFleet(t, "deletion", []string{"de-east"}, []string{"de-east"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	l := f.lease("incident-12", p.Name, 5*time.Minute, "restart_workload")
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	waitLeasePhase(t, l, fpv1.LeaseActive, fpv1.ReasonLeaseActive)
	if err := k8s.Get(ctx, clientKey(p), p); err != nil {
		t.Fatal(err)
	}
	uid := p.UID
	if err := k8s.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "policy and its grants to be gone", func() error {
		var list workv1.ManifestWorkList
		if err := k8s.List(ctx, &list, client.MatchingLabels{ocm.LabelPolicyUID: string(uid)}); err != nil {
			return err
		}
		if len(list.Items) != 0 {
			return fmt.Errorf("%d works remain", len(list.Items))
		}
		if err := k8s.Get(ctx, clientKey(p), &fpv1.FleetAccessPolicy{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("policy still present (finalizer?): %v", err)
		}
		return nil
	})
	waitLeasePhase(t, l, fpv1.LeaseDenied, fpv1.ReasonPolicyNotFound)
}

func TestStandingPolicyWithoutLeases(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()

	f := newFleet(t, "standing", []string{"st-east"}, []string{"st-east"})
	no := false
	p := f.policy("read-only", func(p *fpv1.FleetAccessPolicy) {
		p.Spec.Lease.Required = &no
		p.Spec.Permissions = []fpv1.Permission{{Tool: "get_cluster_health"}}
	})
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "standing grant", func() error {
		body := manifestJSON(works(t, p)["st-east"])
		if !strings.Contains(body, "get_cluster_health") || strings.Contains(body, "request.time") {
			return fmt.Errorf("unexpected content %q", body)
		}
		return nil
	})
}

// TestMissingDeliveredObjectTriggersReapply checks that when OCM reports the
// delivered object missing on a managed cluster, FleetPermit asks the work
// agent to re-apply immediately (OCM re-queues works whose labels change).
func TestMissingDeliveredObjectTriggersReapply(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()

	f := newFleet(t, "reapply", []string{"ra-east"}, []string{"ra-east"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	l := f.lease("incident-13", p.Name, 5*time.Minute, "restart_workload")
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	waitLeasePhase(t, l, fpv1.LeaseActive, fpv1.ReasonLeaseActive)

	w := works(t, p)["ra-east"]
	if _, ok := w.Labels[ocm.LabelResync]; ok {
		t.Fatal("no resync expected before drift")
	}
	meta.SetStatusCondition(&w.Status.Conditions, metav1.Condition{
		Type: workv1.WorkAvailable, Status: metav1.ConditionFalse, Reason: "ResourcesNotAvailable", ObservedGeneration: w.Generation,
	})
	if err := k8s.Status().Update(ctx, &w); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "resync label on the ManifestWork", func() error {
		got := works(t, p)["ra-east"]
		if got.Labels[ocm.LabelResync] == "" {
			return fmt.Errorf("labels %v", got.Labels)
		}
		if got.Generation != w.Generation {
			return fmt.Errorf("the spec must not change when requesting a re-apply")
		}
		return nil
	})
	eventually(t, 10*time.Second, "lease to report the drift", func() error {
		got := getLease(t, l)
		for _, c := range got.Status.Conditions {
			if c.Type == fpv1.ConditionReady && c.Status == metav1.ConditionFalse {
				return nil
			}
		}
		return fmt.Errorf("conditions %+v", got.Status.Conditions)
	})
}

// TestDeliveryIsNotBlockedByConcurrentStatusWrites is a regression test for
// a lab finding: the OCM work agent writes ManifestWork status continuously,
// and a read-modify-write Update from the informer cache conflicted with
// those writes, delaying delivery until the next progress requeue (5s).
// FleetPermit now patches without an optimistic lock.
func TestDeliveryIsNotBlockedByConcurrentStatusWrites(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()

	f := newFleet(t, "status-churn", []string{"sc-east"}, []string{"sc-east"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "inert policy delivered", func() error {
		if len(works(t, p)) != 1 {
			return fmt.Errorf("no work yet")
		}
		return nil
	})

	// Simulate the work agent: rewrite status every 10ms.
	churnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for i := 0; churnCtx.Err() == nil; i++ {
			for _, w := range works(t, p) {
				meta.SetStatusCondition(&w.Status.Conditions, metav1.Condition{
					Type: "StatusFeedbackSynced", Status: metav1.ConditionTrue, Reason: "Test", Message: fmt.Sprint(i),
				})
				_ = k8s.Status().Update(churnCtx, &w)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	start := time.Now()
	l := f.lease("under-churn", p.Name, 5*time.Minute, "restart_workload")
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	// The churn continues until the end of the test, so a delivery that
	// conflicts with status writes never completes. The bound is loose for
	// shared CI runners; an unobstructed delivery takes well under a second.
	eventually(t, 20*time.Second, "grant delivered despite status churn", func() error {
		if len(grantWorks(t, p)) != 1 {
			return fmt.Errorf("grant not delivered yet")
		}
		return nil
	})
	if took := time.Since(start); took > 15*time.Second {
		t.Fatalf("delivery took %s under concurrent status writes; conflicts are delaying it", took)
	}
}

// TestPolicyDefaultChangeCannotExtendLease is a regression test for an audit
// finding: a lease that relies on the policy's defaultDuration must keep the
// expiry it was issued with when the policy default is later raised.
func TestPolicyDefaultChangeCannotExtendLease(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()

	f := newFleet(t, "pin-default", []string{"pd-east"}, []string{"pd-east"})
	p := f.policy("sre-remediation", func(p *fpv1.FleetAccessPolicy) {
		p.Spec.Lease.DefaultDuration = &metav1.Duration{Duration: 2 * time.Minute}
	})
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	l := f.lease("uses-default", p.Name, time.Minute, "restart_workload")
	l.Spec.Duration = nil
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	active := waitLeasePhase(t, l, fpv1.LeaseActive, fpv1.ReasonLeaseActive)
	issued := active.Status.ExpiresAt.Time
	if want := active.CreationTimestamp.Add(2 * time.Minute); !issued.Equal(want) {
		t.Fatalf("issued expiry %s, want %s", issued, want)
	}

	updatePolicy(t, p, func(cur *fpv1.FleetAccessPolicy) {
		cur.Spec.Lease.DefaultDuration = &metav1.Duration{Duration: 9 * time.Minute}
	})
	time.Sleep(2 * time.Second)
	poke(t, l)
	time.Sleep(time.Second)
	got := getLease(t, l)
	if !got.Status.ExpiresAt.Time.Equal(issued) {
		t.Fatalf("raising the policy default moved the lease expiry from %s to %s", issued, got.Status.ExpiresAt.Time)
	}
	body := manifestJSON(works(t, p)["pd-east"])
	if !strings.Contains(body, issued.UTC().Format(time.RFC3339)) {
		t.Fatalf("the rendered rule no longer carries the issued expiry %s: %s", issued.UTC().Format(time.RFC3339), body)
	}
}

// TestForeignManifestWorkIsNotOverwritten is a regression test for an audit
// finding: FleetPermit must never take over a ManifestWork owned by another
// policy, even if the name matches.
func TestForeignManifestWorkIsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()

	f := newFleet(t, "ownership", []string{"ow-east"}, []string{"ow-east"})
	p := f.policy("sre-remediation")
	foreign := &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ow-east", Name: ocm.WorkName(p),
			Labels: map[string]string{ocm.LabelManagedBy: ocm.ManagedByValue, ocm.LabelPolicyUID: "someone-else"},
		},
		Spec: workv1.ManifestWorkSpec{Workload: workv1.ManifestsTemplate{Manifests: []workv1.Manifest{{
			RawExtension: runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"foreign","namespace":"default"}}`)},
		}}}},
	}
	if err := k8s.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the cluster to be reported as a delivery failure", func() error {
		var got fpv1.FleetAccessPolicy
		if err := k8s.Get(ctx, clientKey(p), &got); err != nil {
			return err
		}
		for _, c := range got.Status.Clusters {
			if c.Name == "ow-east" && c.Reason == "DeliveryFailed" && strings.Contains(c.Message, "not owned by this policy") {
				return nil
			}
		}
		return fmt.Errorf("clusters %+v", got.Status.Clusters)
	})
	var after workv1.ManifestWork
	if err := k8s.Get(ctx, clientKey(foreign), &after); err != nil {
		t.Fatal(err)
	}
	if after.Labels[ocm.LabelPolicyUID] != "someone-else" || !strings.Contains(string(after.Spec.Workload.Manifests[0].Raw), "foreign") {
		t.Fatalf("the foreign ManifestWork was modified: %+v", after)
	}
}

// TestEarlierNamedWorksAreRemoved covers deliveries under an earlier naming
// scheme (object names grew from 8 to 16 hex characters after v0.1.0): they
// are removed from clusters that are no longer placed and replaced on placed
// ones, so no grant is left behind after an upgrade.
func TestEarlierNamedWorksAreRemoved(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()

	f := newFleet(t, "earlier-names", []string{"en-east", "en-west"}, []string{"en-east"})
	no := false
	p := f.policy("sre-remediation", func(p *fpv1.FleetAccessPolicy) { p.Spec.Lease.Required = &no })
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the current work on en-east", func() error {
		if err := k8s.Get(ctx, clientKey(p), p); err != nil {
			return err
		}
		if _, ok := works(t, p)["en-east"]; !ok {
			return fmt.Errorf("no work yet")
		}
		return nil
	})
	var earlier []*workv1.ManifestWork
	for _, c := range []string{"en-east", "en-west"} {
		w := &workv1.ManifestWork{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: c, Name: "fleetpermit-sre-remediation-0123abcd",
				Labels: map[string]string{ocm.LabelManagedBy: ocm.ManagedByValue, ocm.LabelPolicyUID: string(p.UID)},
			},
			Spec: workv1.ManifestWorkSpec{Workload: workv1.ManifestsTemplate{Manifests: []workv1.Manifest{{
				RawExtension: runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"standing-grant","namespace":"default"}}`)},
			}}}},
		}
		if err := k8s.Create(ctx, w); err != nil {
			t.Fatal(err)
		}
		earlier = append(earlier, w)
	}
	poke(t, p)
	eventually(t, 10*time.Second, "earlier-named works to be removed", func() error {
		ackWorks(t)
		for _, w := range earlier {
			if err := k8s.Get(ctx, clientKey(w), &workv1.ManifestWork{}); !apierrors.IsNotFound(err) {
				return fmt.Errorf("%s/%s still present (%v)", w.Namespace, w.Name, err)
			}
		}
		return nil
	})
	if got := clusterSet(works(t, p)); got != "en-east" {
		t.Fatalf("only the current work on en-east must remain, got %q", got)
	}
}

// TestLeaseDroppedOnEveryClusterIsPending checks that a lease which does not
// fit the enforcement layer's rule limit on any cluster is reported Pending
// and Degraded/CapacityExceeded, not Active, and activates once capacity frees.
func TestLeaseDroppedOnEveryClusterIsPending(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()

	f := newFleet(t, "capacity", []string{"ca-east"}, []string{"ca-east"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	// One subject: one session rule plus nine grant rules fill the upstream
	// limit of ten rules, so the tenth lease (created last) does not fit.
	var leases []*fpv1.ToolAccessLease
	for i := 1; i <= 10; i++ {
		d := 10 * time.Minute
		if i == 1 {
			d = time.Minute
		}
		l := f.lease(fmt.Sprintf("l%02d", i), p.Name, d, "restart_workload")
		if err := k8s.Create(ctx, l); err != nil {
			t.Fatal(err)
		}
		leases = append(leases, l)
	}
	for _, l := range leases[:9] {
		waitLeasePhase(t, l, fpv1.LeaseActive, fpv1.ReasonLeaseActive)
	}
	last := waitLeasePhase(t, leases[9], fpv1.LeasePending, fpv1.ReasonCapacityExceeded)
	if last.Status.ClusterCount != 0 {
		t.Fatalf("a lease rendered nowhere must list no clusters, got %v", last.Status.Clusters)
	}
	eventually(t, 10*time.Second, "the policy to count nine active leases", func() error {
		var got fpv1.FleetAccessPolicy
		if err := k8s.Get(ctx, clientKey(p), &got); err != nil {
			return err
		}
		if got.Status.ActiveLeases != 9 {
			return fmt.Errorf("activeLeases %d", got.Status.ActiveLeases)
		}
		return nil
	})

	first := getLease(t, leases[0])
	clock.Set(first.CreationTimestamp.Add(time.Minute + time.Second))
	poke(t, leases[0])
	waitLeasePhase(t, leases[0], fpv1.LeaseExpired, fpv1.ReasonLeaseExpired)
	waitLeasePhase(t, leases[9], fpv1.LeaseActive, fpv1.ReasonLeaseActive)
}

// TestExpiredLeaseStaysExpiredWhenPolicyIsDeleted checks that deleting a
// policy denies only leases that are still live: a lease that already
// expired keeps phase Expired and is not also marked Denied.
func TestExpiredLeaseStaysExpiredWhenPolicyIsDeleted(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()

	f := newFleet(t, "expired-then-deleted", []string{"ed-east"}, []string{"ed-east"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	l := f.lease("incident-77", p.Name, time.Minute, "restart_workload")
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	got := waitLeasePhase(t, l, fpv1.LeaseActive, fpv1.ReasonLeaseActive)
	clock.Set(got.CreationTimestamp.Add(time.Minute + time.Second))
	poke(t, l)
	waitLeasePhase(t, l, fpv1.LeaseExpired, fpv1.ReasonLeaseExpired)

	if err := k8s.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the policy to be gone", func() error {
		if err := k8s.Get(ctx, clientKey(p), &fpv1.FleetAccessPolicy{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("policy still present: %v", err)
		}
		return nil
	})
	// Give the controller a chance to process the orphaned lease.
	poke(t, l)
	time.Sleep(2 * time.Second)
	after := getLease(t, l)
	if after.Status.Phase != fpv1.LeaseExpired {
		t.Fatalf("an expired lease must stay Expired after its policy is deleted, got %s: %+v", after.Status.Phase, after.Status.Conditions)
	}
	if c := meta.FindStatusCondition(after.Status.Conditions, fpv1.ConditionDenied); c != nil && c.Status == metav1.ConditionTrue {
		t.Fatalf("an expired lease must not also be Denied: %+v", c)
	}
}

func revocationSamples(t testing.TB) uint64 {
	t.Helper()
	var m dto.Metric
	if err := metrics.LeaseRevocation.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}

// TestRevocationIsMeasuredWhenGrantsAreWithdrawn checks that
// fleetpermit_lease_revocation_seconds records a sample once an expired
// lease's grant has been withdrawn from every cluster.
func TestRevocationIsMeasuredWhenGrantsAreWithdrawn(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()

	f := newFleet(t, "revocation-metric", []string{"rm-east", "rm-west"}, []string{"rm-east", "rm-west"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	l := f.lease("incident-88", p.Name, time.Minute, "restart_workload")
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	got := waitLeasePhase(t, l, fpv1.LeaseActive, fpv1.ReasonLeaseActive)
	before := revocationSamples(t)

	clock.Set(got.CreationTimestamp.Add(time.Minute + time.Second))
	poke(t, l)
	waitLeasePhase(t, l, fpv1.LeaseExpired, fpv1.ReasonLeaseExpired)
	eventually(t, 15*time.Second, "the grant to be withdrawn and the revocation measured", func() error {
		ackWorks(t)
		poke(t, l)
		cur := getLease(t, l)
		if len(cur.Status.Clusters) != 0 {
			return fmt.Errorf("still withdrawing from %v", cur.Status.Clusters)
		}
		if n := revocationSamples(t); n <= before {
			return fmt.Errorf("no revocation sample recorded (count %d)", n)
		}
		return nil
	})
}

func propagationSamples(t testing.TB) uint64 {
	t.Helper()
	var m dto.Metric
	if err := metrics.PolicyPropagation.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}

// TestPropagationIsObservedOncePerLease checks that
// fleetpermit_policy_propagation_seconds records a lease once, and not again
// when the lease returns to Ready after another lease changed the content.
func TestPropagationIsObservedOncePerLease(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()

	f := newFleet(t, "propagation-metric", []string{"pm-east"}, []string{"pm-east"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	first := f.lease("incident-91", p.Name, 10*time.Minute, "restart_workload")
	if err := k8s.Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	waitLeasePhase(t, first, fpv1.LeaseActive, fpv1.ReasonLeaseActive)
	before := propagationSamples(t)

	// A second lease changes the rendered content, so the first lease is
	// briefly not Ready while the new revision rolls out.
	second := f.lease("incident-92", p.Name, 10*time.Minute, "get_cluster_health")
	if err := k8s.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	waitLeasePhase(t, second, fpv1.LeaseActive, fpv1.ReasonLeaseActive)
	waitLeasePhase(t, first, fpv1.LeaseActive, fpv1.ReasonLeaseActive)
	poke(t, first)
	waitLeasePhase(t, first, fpv1.LeaseActive, fpv1.ReasonLeaseActive)

	if got := propagationSamples(t) - before; got != 1 {
		t.Fatalf("expected one new propagation sample (the second lease), got %d", got)
	}
}

func getPolicy(t testing.TB, p *fpv1.FleetAccessPolicy) *fpv1.FleetAccessPolicy {
	t.Helper()
	out := &fpv1.FleetAccessPolicy{}
	if err := k8s.Get(context.Background(), clientKey(p), out); err != nil {
		t.Fatal(err)
	}
	return out
}

// clusterStatus returns the policy's status entry for a cluster, or nil.
func clusterStatus(p *fpv1.FleetAccessPolicy, cluster string) *fpv1.ClusterStatus {
	for i := range p.Status.Clusters {
		if p.Status.Clusters[i].Name == cluster {
			return &p.Status.Clusters[i]
		}
	}
	return nil
}

// TestWithdrawalIsReportedUntilTheWorkIsGone checks that withdrawal from a
// cluster that left the placement is reported as in progress for as long as
// its ManifestWork is being deleted (the OCM work agent holds a finalizer
// until it has removed the delivered objects): the policy lists the cluster
// as Revoking, an expired lease keeps it as pending, and the revocation is
// measured only once the work is gone.
func TestWithdrawalIsReportedUntilTheWorkIsGone(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()
	t.Cleanup(func() { releaseWorks(t) })

	f := newFleet(t, "withdrawal", []string{"wd-east", "wd-west"}, []string{"wd-east", "wd-west"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	l := f.lease("incident-21", p.Name, time.Minute, "restart_workload")
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	got := waitLeasePhase(t, l, fpv1.LeaseActive, fpv1.ReasonLeaseActive)
	eventually(t, 10*time.Second, "the work agent's finalizer on both works", func() error {
		ackWorks(t, holdDeletion)
		held := 0
		for _, w := range works(t, p) {
			if controllerutil.ContainsFinalizer(&w, workv1.ManifestWorkFinalizer) {
				held++
			}
		}
		if held != 2 {
			return fmt.Errorf("%d of 2 works hold the finalizer", held)
		}
		return nil
	})
	before := revocationSamples(t)

	// The lease expires and wd-west leaves the placement at the same time.
	clock.Set(got.CreationTimestamp.Add(time.Minute + time.Second))
	f.selectClusters(t, "wd-east")
	withdrawing := func() error {
		ackWorks(t, holdDeletion)
		var w workv1.ManifestWork
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: "wd-west", Name: ocm.WorkName(p)}, &w); err != nil {
			return fmt.Errorf("the work on wd-west must still exist while it is being deleted: %v", err)
		}
		if w.DeletionTimestamp.IsZero() {
			return fmt.Errorf("the work on wd-west is not being deleted yet")
		}
		cur := getLease(t, l)
		if cur.Status.Phase != fpv1.LeaseExpired || strings.Join(cur.Status.Clusters, ",") != "wd-west" ||
			!meta.IsStatusConditionTrue(cur.Status.Conditions, fpv1.ConditionProgressing) {
			return fmt.Errorf("the lease must still be withdrawing from wd-west: %+v", cur.Status)
		}
		pol := getPolicy(t, p)
		cs := clusterStatus(pol, "wd-west")
		if cs == nil || cs.Ready || cs.Reason != "Revoking" {
			return fmt.Errorf("wd-west must be listed as Revoking: %+v", pol.Status.Clusters)
		}
		if !meta.IsStatusConditionTrue(pol.Status.Conditions, fpv1.ConditionProgressing) ||
			pol.Status.SelectedClusters != 1 || pol.Status.ClusterSummary != "1/1" {
			return fmt.Errorf("unexpected policy status %+v", pol.Status)
		}
		return nil
	}
	eventually(t, 15*time.Second, "wd-west to be reported as Revoking", withdrawing)
	// Nothing changes while the work agent holds the work.
	time.Sleep(2 * time.Second)
	poke(t, l)
	time.Sleep(time.Second)
	if err := withdrawing(); err != nil {
		t.Fatal(err)
	}
	if n := revocationSamples(t); n != before {
		t.Fatalf("the revocation was measured while the grant was still being withdrawn (%d samples, was %d)", n, before)
	}

	// The work agent finishes: the work is gone and the withdrawal completes.
	eventually(t, 15*time.Second, "the withdrawal to complete", func() error {
		releaseWorks(t)
		ackWorks(t)
		cur := getLease(t, l)
		if len(cur.Status.Clusters) != 0 || meta.IsStatusConditionTrue(cur.Status.Conditions, fpv1.ConditionProgressing) {
			return fmt.Errorf("lease still withdrawing: %+v", cur.Status)
		}
		pol := getPolicy(t, p)
		if clusterStatus(pol, "wd-west") != nil || meta.IsStatusConditionTrue(pol.Status.Conditions, fpv1.ConditionProgressing) ||
			!meta.IsStatusConditionTrue(pol.Status.Conditions, fpv1.ConditionReady) {
			return fmt.Errorf("policy still withdrawing: %+v", pol.Status)
		}
		if n := revocationSamples(t); n <= before {
			return fmt.Errorf("no revocation sample recorded (count %d)", n)
		}
		return nil
	})
}

// TestReplacedClusterWaitsForTheDeletingWork checks that a cluster placed
// again while its previous ManifestWork is still being deleted is reported
// as progressing, not as a delivery failure, and is delivered to once the
// deletion completes.
func TestReplacedClusterWaitsForTheDeletingWork(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()
	t.Cleanup(func() { releaseWorks(t) })

	f := newFleet(t, "replaced", []string{"rp-east", "rp-west"}, []string{"rp-east", "rp-west"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "both clusters ready with the work agent's finalizer", func() error {
		ackWorks(t, holdDeletion)
		pol := getPolicy(t, p)
		if pol.Status.ClusterSummary != "2/2" {
			return fmt.Errorf("clusters %s", pol.Status.ClusterSummary)
		}
		for c, w := range works(t, p) {
			if !controllerutil.ContainsFinalizer(&w, workv1.ManifestWorkFinalizer) {
				return fmt.Errorf("%s has no finalizer yet", c)
			}
		}
		return nil
	})

	f.selectClusters(t, "rp-east")
	eventually(t, 10*time.Second, "the work on rp-west to be deleting", func() error {
		var w workv1.ManifestWork
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: "rp-west", Name: ocm.WorkName(p)}, &w); err != nil {
			return err
		}
		if w.DeletionTimestamp.IsZero() {
			return fmt.Errorf("not deleting yet")
		}
		return nil
	})
	f.selectClusters(t, "rp-east", "rp-west")
	eventually(t, 10*time.Second, "rp-west to be reported as progressing", func() error {
		pol := getPolicy(t, p)
		cs := clusterStatus(pol, "rp-west")
		if cs == nil || cs.Ready || cs.Reason != "Delivering" {
			return fmt.Errorf("rp-west status %+v", cs)
		}
		if meta.IsStatusConditionTrue(pol.Status.Conditions, fpv1.ConditionDegraded) ||
			!meta.IsStatusConditionTrue(pol.Status.Conditions, fpv1.ConditionProgressing) {
			return fmt.Errorf("a deleting work is not a failure: %+v", pol.Status.Conditions)
		}
		return nil
	})

	eventually(t, 15*time.Second, "rp-west to be delivered again", func() error {
		releaseWorks(t)
		ackWorks(t)
		pol := getPolicy(t, p)
		if pol.Status.ClusterSummary != "2/2" || !meta.IsStatusConditionTrue(pol.Status.Conditions, fpv1.ConditionReady) {
			return fmt.Errorf("status %+v", pol.Status)
		}
		return nil
	})
}

// TestStatusClusterListIsCapped checks that a policy placed on more clusters
// than status.clusters may list (512) still reports its status: the counts
// stay exact, the Ready message says the list is truncated, and clusters
// that are not ready are listed first.
func TestStatusClusterListIsCapped(t *testing.T) {
	ctx := context.Background()
	stop := startController(t, time.Now)
	defer stop()

	const n = 513
	var clusters []string
	for i := 0; i < n; i++ {
		clusters = append(clusters, fmt.Sprintf("cap-%03d", i))
	}
	f := newFleet(t, "status-cap", clusters, clusters)
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, 90*time.Second, "exact counts and a capped cluster list", func() error {
		ackWorks(t)
		pol := getPolicy(t, p)
		if pol.Status.SelectedClusters != n || pol.Status.ReadyClusters != n || pol.Status.ClusterSummary != "513/513" {
			return fmt.Errorf("counts %d/%d (%q)", pol.Status.ReadyClusters, pol.Status.SelectedClusters, pol.Status.ClusterSummary)
		}
		if len(pol.Status.Clusters) != 512 {
			return fmt.Errorf("%d clusters listed", len(pol.Status.Clusters))
		}
		if c := meta.FindStatusCondition(pol.Status.Conditions, fpv1.ConditionReady); c == nil || !strings.Contains(c.Message, "512 of 513") {
			return fmt.Errorf("the Ready message must say the list is truncated: %+v", c)
		}
		return nil
	})

	// The last cluster by name, the one left out so far, fails to apply.
	last := clusters[n-1]
	var w workv1.ManifestWork
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: last, Name: ocm.WorkName(p)}, &w); err != nil {
		t.Fatal(err)
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8s.Get(ctx, clientKey(&w), &w); err != nil {
			return err
		}
		meta.SetStatusCondition(&w.Status.Conditions, metav1.Condition{
			Type: workv1.WorkApplied, Status: metav1.ConditionFalse, Reason: "Test", Message: "apply failed", ObservedGeneration: w.Generation,
		})
		return k8s.Status().Update(ctx, &w)
	})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the failing cluster to be listed first", func() error {
		pol := getPolicy(t, p)
		if len(pol.Status.Clusters) != 512 || pol.Status.ReadyClusters != n-1 {
			return fmt.Errorf("%d listed, %d ready", len(pol.Status.Clusters), pol.Status.ReadyClusters)
		}
		if first := pol.Status.Clusters[0]; first.Name != last || first.Ready {
			return fmt.Errorf("first listed cluster %+v", first)
		}
		return nil
	})
}

func placementChanges() float64 { return testutil.ToFloat64(metrics.PlacementChanges) }

// TestPlacementChangesAreCountedOnce checks that
// fleetpermit_placement_changes_total counts each change of the selected
// clusters once, however often the policy is reconciled afterwards.
func TestPlacementChangesAreCountedOnce(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()

	f := newFleet(t, "placement-metric", []string{"pc-east", "pc-west"}, []string{"pc-east", "pc-west"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	settled := func(summary string) func() error {
		return func() error {
			ackWorks(t)
			pol := getPolicy(t, p)
			if pol.Status.ClusterSummary != summary || len(pol.Status.Clusters) != int(pol.Status.SelectedClusters) ||
				!meta.IsStatusConditionTrue(pol.Status.Conditions, fpv1.ConditionReady) {
				return fmt.Errorf("status %+v", pol.Status)
			}
			return nil
		}
	}
	eventually(t, 10*time.Second, "both clusters ready", settled("2/2"))
	before := placementChanges()

	f.selectClusters(t, "pc-east")
	eventually(t, 10*time.Second, "pc-west to be dropped", settled("1/1"))
	poke(t, p)
	time.Sleep(time.Second)
	f.selectClusters(t, "pc-east", "pc-west")
	eventually(t, 10*time.Second, "pc-west to be added back", settled("2/2"))
	poke(t, p)
	time.Sleep(time.Second)
	if got := placementChanges() - before; got != 2 {
		t.Fatalf("one drop and one re-add must count 2 placement changes, got %v", got)
	}
}

// TestTamperedWorkSpecIsRestored checks that edits to the ManifestWork spec
// outside the manifests (delete option, update strategy, executor) are
// reverted, and that the restored work is then left alone.
func TestTamperedWorkSpecIsRestored(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()

	f := newFleet(t, "spec-drift", []string{"sd-east"}, []string{"sd-east"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	l := f.lease("incident-31", p.Name, 5*time.Minute, "restart_workload")
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	waitLeasePhase(t, l, fpv1.LeaseActive, fpv1.ReasonLeaseActive)

	w := works(t, p)["sd-east"]
	updateWork(t, &w, func(w *workv1.ManifestWork) {
		// Orphan: deleting the work would leave the grant on the cluster.
		w.Spec.DeleteOption = &workv1.DeleteOption{PropagationPolicy: workv1.DeletePropagationPolicyTypeOrphan}
		// CreateOnly: later revocations would never reach the cluster.
		w.Spec.ManifestConfigs[0].UpdateStrategy = &workv1.UpdateStrategy{Type: workv1.UpdateStrategyTypeCreateOnly}
		w.Spec.Executor = &workv1.ManifestWorkExecutor{Subject: workv1.ManifestWorkExecutorSubject{
			Type:           workv1.ExecutorSubjectTypeServiceAccount,
			ServiceAccount: &workv1.ManifestWorkSubjectServiceAccount{Namespace: "kube-system", Name: "powerful"},
		}}
	})
	var restored workv1.ManifestWork
	restoredSpec := func() error {
		restored = works(t, p)["sd-east"]
		s := restored.Spec
		if s.DeleteOption == nil || s.DeleteOption.PropagationPolicy != workv1.DeletePropagationPolicyTypeForeground {
			return fmt.Errorf("delete option %+v", s.DeleteOption)
		}
		if u := s.ManifestConfigs[0].UpdateStrategy; u == nil || u.Type != workv1.UpdateStrategyTypeServerSideApply || u.ServerSideApply == nil || !u.ServerSideApply.Force {
			return fmt.Errorf("update strategy %+v", u)
		}
		if s.Executor != nil {
			return fmt.Errorf("executor %+v", s.Executor)
		}
		if len(s.ManifestConfigs) != 1 || len(s.ManifestConfigs[0].ConditionRules) != 0 ||
			len(s.ManifestConfigs[0].UpdateStrategy.ServerSideApply.IgnoreFields) != 0 {
			return fmt.Errorf("manifest configs %+v", s.ManifestConfigs)
		}
		if s.DeleteOption.TTLSecondsAfterFinished != nil || s.DeleteOption.SelectivelyOrphan != nil {
			return fmt.Errorf("delete option %+v", s.DeleteOption)
		}
		return nil
	}
	eventually(t, 10*time.Second, "the work spec to be restored", restoredSpec)

	// Fields FleetPermit leaves unset: ignored fields keep edits made on the
	// managed cluster, a TTL or selective orphaning changes deletion, extra
	// configurations and condition rules change what the agent does.
	ttl := int64(1)
	updateWork(t, &w, func(w *workv1.ManifestWork) {
		mc := &w.Spec.ManifestConfigs[0]
		mc.UpdateStrategy.ServerSideApply.IgnoreFields = []workv1.IgnoreField{{
			Condition: workv1.IgnoreFieldsConditionOnSpokeChange, JSONPaths: []string{".spec.rules"},
		}}
		mc.ConditionRules = []workv1.ConditionRule{{Condition: "Complete", Type: workv1.WellKnownConditionsType}}
		w.Spec.DeleteOption.TTLSecondsAfterFinished = &ttl
		w.Spec.DeleteOption.SelectivelyOrphan = &workv1.SelectivelyOrphan{OrphaningRules: []workv1.OrphaningRule{{
			Group: "agentic.networking.x-k8s.io", Resource: "xaccesspolicies", Namespace: "mcp-tools", Name: "anything",
		}}}
		w.Spec.ManifestConfigs = append(w.Spec.ManifestConfigs, workv1.ManifestConfigOption{
			ResourceIdentifier: workv1.ResourceIdentifier{Group: "agentic.networking.x-k8s.io", Resource: "xaccesspolicies", Namespace: "mcp-tools", Name: "other"},
			UpdateStrategy:     &workv1.UpdateStrategy{Type: workv1.UpdateStrategyTypeReadOnly},
		})
	})
	eventually(t, 10*time.Second, "the unset work spec fields to be removed", restoredSpec)
	poke(t, p)
	time.Sleep(2 * time.Second)
	if after := works(t, p)["sd-east"]; after.Generation != restored.Generation {
		t.Fatalf("the restored work keeps being rewritten: generation %d, then %d", restored.Generation, after.Generation)
	}
}

// TestLeaseCreatedBeforeItsPolicyActivates checks the apply order of GitOps
// tools, which may create a lease before its policy: the lease waits as
// Pending/PolicyNotFound, is not denied, and activates once the policy exists.
func TestLeaseCreatedBeforeItsPolicyActivates(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()

	f := newFleet(t, "apply-order", []string{"ao-east"}, []string{"ao-east"})
	l := f.lease("early", "sre-remediation", 5*time.Minute, "restart_workload")
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	waiting := func() error {
		got := getLease(t, l)
		c := meta.FindStatusCondition(got.Status.Conditions, fpv1.ConditionReady)
		if got.Status.Phase != fpv1.LeasePending || c == nil || c.Reason != fpv1.ReasonPolicyNotFound {
			return fmt.Errorf("phase %q, conditions %+v", got.Status.Phase, got.Status.Conditions)
		}
		if meta.IsStatusConditionTrue(got.Status.Conditions, fpv1.ConditionDenied) {
			return fmt.Errorf("a lease whose policy does not exist yet must not be denied: %+v", got.Status.Conditions)
		}
		return nil
	}
	eventually(t, 10*time.Second, "the lease to wait for its policy", waiting)
	poke(t, l)
	time.Sleep(time.Second)
	if err := waiting(); err != nil {
		t.Fatal(err)
	}

	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	waitLeasePhase(t, l, fpv1.LeaseActive, fpv1.ReasonLeaseActive)
}

// TestWorksOfAPolicyDeletedWithoutItsFinalizerAreRemoved checks that a
// policy deleted without FleetPermit's cleanup (its finalizer was removed by
// hand) does not leave its grants behind: its ManifestWorks are deleted.
func TestWorksOfAPolicyDeletedWithoutItsFinalizerAreRemoved(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer func() { stop() }()

	f := newFleet(t, "orphaned-works", []string{"or-east", "or-west"}, []string{"or-east", "or-west"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	l := f.lease("incident-41", p.Name, 5*time.Minute, "restart_workload")
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	waitLeasePhase(t, l, fpv1.LeaseActive, fpv1.ReasonLeaseActive)
	p = getPolicy(t, p)
	if len(works(t, p)) != 2 {
		t.Fatalf("expected works on both clusters, got %s", clusterSet(works(t, p)))
	}

	// With the controller stopped, remove the finalizer and delete the policy.
	stop()
	updatePolicy(t, p, func(cur *fpv1.FleetAccessPolicy) { controllerutil.RemoveFinalizer(cur, controller.Finalizer) })
	if err := k8s.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the policy to be gone", func() error {
		if err := k8s.Get(ctx, clientKey(p), &fpv1.FleetAccessPolicy{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("policy still present: %v", err)
		}
		return nil
	})

	stop = startController(t, clock.Now)
	eventually(t, 15*time.Second, "the orphaned works to be removed", func() error {
		if w := works(t, p); len(w) != 0 {
			return fmt.Errorf("works remain on %s", clusterSet(w))
		}
		return nil
	})
	waitLeasePhase(t, l, fpv1.LeaseDenied, fpv1.ReasonPolicyNotFound)
}

// TestWatchNamespaceIgnoresOtherNamespaces checks --watch-namespace: a
// ManifestWork that names a policy in another namespace is ignored instead
// of failing reconciles for a namespace the cache does not hold, while
// policies in the watched namespace are reconciled as usual.
func TestWatchNamespaceIgnoresOtherNamespaces(t *testing.T) {
	ctx := context.Background()
	stop := startControllerIn(t, time.Now, "wn-home")
	defer stop()

	f := newFleet(t, "wn-home", []string{"wn-east"}, []string{"wn-east"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the watched policy to be ready", func() error {
		ackWorks(t)
		pol := getPolicy(t, p)
		if pol.Status.ClusterSummary != "1/1" || !meta.IsStatusConditionTrue(pol.Status.Conditions, fpv1.ConditionReady) {
			return fmt.Errorf("status %+v", pol.Status)
		}
		return nil
	})

	errorsBefore := testutil.ToFloat64(metrics.ReconcileErrors)
	createNamespace(t, "wn-other")
	foreign := &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "wn-east", Name: "fleetpermit-elsewhere",
			Labels:      map[string]string{ocm.LabelManagedBy: ocm.ManagedByValue, ocm.LabelPolicyUID: "elsewhere"},
			Annotations: map[string]string{ocm.AnnotationPolicy: "wn-other/sre-remediation"},
		},
		Spec: workv1.ManifestWorkSpec{Workload: workv1.ManifestsTemplate{Manifests: []workv1.Manifest{{
			RawExtension: runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"elsewhere","namespace":"default"}}`)},
		}}}},
	}
	if err := k8s.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if got := testutil.ToFloat64(metrics.ReconcileErrors) - errorsBefore; got != 0 {
		t.Fatalf("%v reconcile errors for a policy outside the watch namespace", got)
	}
}

func reconciles() float64 {
	return testutil.ToFloat64(metrics.ReconcileTotal.WithLabelValues("success")) +
		testutil.ToFloat64(metrics.ReconcileTotal.WithLabelValues("error"))
}

// TestPermanentDeliveryFailureBacksOff checks that a delivery failure that
// persists (the ManifestWork name is held by another policy) is retried with
// a growing delay rather than every second. The controller watches only the
// test's namespace, so every reconcile counted is this policy's.
func TestPermanentDeliveryFailureBacksOff(t *testing.T) {
	ctx := context.Background()
	stop := startControllerIn(t, time.Now, "backoff")
	defer stop()

	f := newFleet(t, "backoff", []string{"bo-east"}, []string{"bo-east"})
	p := f.policy("sre-remediation")
	foreign := &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "bo-east", Name: ocm.WorkName(p),
			Labels: map[string]string{ocm.LabelManagedBy: ocm.ManagedByValue, ocm.LabelPolicyUID: "someone-else"},
		},
		Spec: workv1.ManifestWorkSpec{Workload: workv1.ManifestsTemplate{Manifests: []workv1.Manifest{{
			RawExtension: runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"foreign","namespace":"default"}}`)},
		}}}},
	}
	if err := k8s.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the delivery failure to be reported", func() error {
		if cs := clusterStatus(getPolicy(t, p), "bo-east"); cs == nil || cs.Reason != "DeliveryFailed" {
			return fmt.Errorf("bo-east status %+v", cs)
		}
		return nil
	})
	before := reconciles()
	time.Sleep(10 * time.Second)
	if n := reconciles() - before; n > 5 {
		t.Fatalf("%v reconciles in 10s for a failure that persists; want at most 5", n)
	}
}

// worksOf returns every ManifestWork labelled with a policy UID, including
// works that are being deleted.
func worksOf(t testing.TB, uid types.UID) []workv1.ManifestWork {
	t.Helper()
	var list workv1.ManifestWorkList
	if err := k8s.List(context.Background(), &list, client.MatchingLabels{ocm.LabelPolicyUID: string(uid)}); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

// clusterReasons records, from a watch, every reason the policy's status
// gives for a cluster, so that a state written only briefly is not missed.
func clusterReasons(t testing.TB, p *fpv1.FleetAccessPolicy, cluster string) func() []string {
	t.Helper()
	wc, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	w, err := wc.Watch(context.Background(), &fpv1.FleetAccessPolicyList{}, client.InNamespace(p.Namespace))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
	var mu sync.Mutex
	var reasons []string
	go func() {
		for ev := range w.ResultChan() {
			pol, ok := ev.Object.(*fpv1.FleetAccessPolicy)
			if !ok || pol.Name != p.Name {
				continue
			}
			if cs := clusterStatus(pol, cluster); cs != nil {
				mu.Lock()
				reasons = append(reasons, cs.Reason)
				mu.Unlock()
			}
		}
	}()
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(reasons)
	}
}

// TestPolicyRecreatedUnderTheSameNameReplacesTheEarlierWorks covers a
// policy deleted without FleetPermit's cleanup (its finalizer removed by hand)
// and created again under the same name before the controller saw it gone.
// The earlier policy's works must not stay: on a cluster the new policy is
// placed on, the new delivery replaces it; on one it is not, it is deleted.
// The work agent holds both deletions, so the test can check what is
// reported meanwhile: the placed cluster as Delivering, never as a delivery
// failure; the other one as Revoking; and both as ClusterUnavailable, without
// polling, while their agents are offline.
func TestPolicyRecreatedUnderTheSameNameReplacesTheEarlierWorks(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startControllerIn(t, clock.Now, "recreated")
	defer func() { stop() }()
	t.Cleanup(func() { releaseWorks(t) })

	f := newFleet(t, "recreated", []string{"re-east", "re-west"}, []string{"re-east", "re-west"})
	no := false
	standing := func(subjects ...string) *fpv1.FleetAccessPolicy {
		return f.policy("standing", func(p *fpv1.FleetAccessPolicy) {
			p.Spec.Lease.Required = &no
			p.Spec.Subjects = nil
			for _, s := range subjects {
				p.Spec.Subjects = append(p.Spec.Subjects, fpv1.Subject{SPIFFEID: s})
			}
		})
	}
	earlier := standing(sreID, securityID)
	if err := k8s.Create(ctx, earlier); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the earlier policy on both clusters, held by the work agent", func() error {
		ackWorks(t, holdDeletion)
		if s := getPolicy(t, earlier).Status.ClusterSummary; s != "2/2" {
			return fmt.Errorf("clusters %q", s)
		}
		for c, w := range works(t, earlier) {
			if !controllerutil.ContainsFinalizer(&w, workv1.ManifestWorkFinalizer) {
				return fmt.Errorf("%s has no finalizer yet", c)
			}
		}
		return nil
	})
	earlierUID := getPolicy(t, earlier).UID

	// While the controller is down: remove the finalizer, delete the policy,
	// narrow the placement to re-east and create the policy again with only
	// one subject.
	stop()
	updatePolicy(t, earlier, func(cur *fpv1.FleetAccessPolicy) { controllerutil.RemoveFinalizer(cur, controller.Finalizer) })
	if err := k8s.Delete(ctx, earlier); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "the earlier policy to be gone", func() error {
		if err := k8s.Get(ctx, clientKey(earlier), &fpv1.FleetAccessPolicy{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("still present: %v", err)
		}
		return nil
	})
	f.selectClusters(t, "re-east")
	later := standing(sreID)
	if err := k8s.Create(ctx, later); err != nil {
		t.Fatal(err)
	}
	eastReasons := clusterReasons(t, later, "re-east")

	stop = startControllerIn(t, clock.Now, "recreated")
	eventually(t, 10*time.Second, "both earlier works to be deleting and reported as such", func() error {
		for _, w := range worksOf(t, earlierUID) {
			if w.DeletionTimestamp.IsZero() {
				return fmt.Errorf("the earlier work on %s is not being deleted", w.Namespace)
			}
		}
		pol := getPolicy(t, later)
		east, west := clusterStatus(pol, "re-east"), clusterStatus(pol, "re-west")
		if east == nil || east.Reason != "Delivering" || west == nil || west.Ready || west.Reason != "Revoking" {
			return fmt.Errorf("clusters %+v", pol.Status.Clusters)
		}
		if pol.Status.ClusterSummary != "0/1" || !meta.IsStatusConditionTrue(pol.Status.Conditions, fpv1.ConditionProgressing) {
			return fmt.Errorf("status %+v", pol.Status)
		}
		return nil
	})

	// Both agents go offline while the earlier works are held.
	setClusterAvailable(t, "re-east", false)
	setClusterAvailable(t, "re-west", false)
	eventually(t, 10*time.Second, "both clusters to be reported unavailable", func() error {
		pol := getPolicy(t, later)
		for _, c := range []string{"re-east", "re-west"} {
			if cs := clusterStatus(pol, c); cs == nil || cs.Reason != "ClusterUnavailable" || !strings.Contains(cs.Message, "reconnect") {
				return fmt.Errorf("%s status %+v", c, cs)
			}
		}
		if meta.IsStatusConditionTrue(pol.Status.Conditions, fpv1.ConditionProgressing) ||
			!meta.IsStatusConditionTrue(pol.Status.Conditions, fpv1.ConditionDegraded) {
			return fmt.Errorf("conditions %+v", pol.Status.Conditions)
		}
		return nil
	})
	before := reconciles()
	time.Sleep(15 * time.Second)
	if n := reconciles() - before; n > 1 {
		t.Fatalf("%v reconciles in 15s while every change waits for an unavailable cluster", n)
	}

	setClusterAvailable(t, "re-east", true)
	setClusterAvailable(t, "re-west", true)
	eventually(t, 20*time.Second, "the earlier policy's works to be gone and the new one delivered", func() error {
		releaseWorks(t)
		ackWorks(t)
		if left := worksOf(t, earlierUID); len(left) != 0 {
			return fmt.Errorf("%d works of the earlier policy remain, the first on %s", len(left), left[0].Namespace)
		}
		pol := getPolicy(t, later)
		if pol.Status.ClusterSummary != "1/1" || clusterStatus(pol, "re-west") != nil ||
			!meta.IsStatusConditionTrue(pol.Status.Conditions, fpv1.ConditionReady) {
			return fmt.Errorf("new policy status %+v", pol.Status)
		}
		w, ok := works(t, pol)["re-east"]
		if !ok || strings.Contains(manifestJSON(w), securityID) {
			return fmt.Errorf("re-east must hold only the new policy's grants")
		}
		return nil
	})
	if slices.Contains(eastReasons(), "DeliveryFailed") {
		t.Fatalf("re-east was reported as a delivery failure while the earlier work was replaced: %v", eastReasons())
	}
}

// TestForeignWorkBeingDeletedIsADeliveryFailure checks a ManifestWork of
// another owner that is being deleted at the policy's work name: it is a
// delivery failure, retried with the delivery backoff, not a delivery in
// progress polled every few seconds.
func TestForeignWorkBeingDeletedIsADeliveryFailure(t *testing.T) {
	ctx := context.Background()
	stop := startController(t, time.Now)
	defer stop()

	f := newFleet(t, "foreign-deleting", []string{"fd-east"}, []string{"fd-east"})
	p := f.policy("sre-remediation")
	foreign := &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fd-east", Name: ocm.WorkName(p), Finalizers: []string{workv1.ManifestWorkFinalizer},
			Labels: map[string]string{ocm.LabelManagedBy: ocm.ManagedByValue, ocm.LabelPolicyUID: "someone-else"},
		},
		Spec: workv1.ManifestWorkSpec{Workload: workv1.ManifestsTemplate{Manifests: []workv1.Manifest{{
			RawExtension: runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"foreign","namespace":"default"}}`)},
		}}}},
	}
	if err := k8s.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { remove(t, foreign) })
	if err := k8s.Delete(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "a delivery failure that is not progressing", func() error {
		pol := getPolicy(t, p)
		cs := clusterStatus(pol, "fd-east")
		if cs == nil || cs.Reason != "DeliveryFailed" || !strings.Contains(cs.Message, "not owned by this policy") {
			return fmt.Errorf("fd-east status %+v", cs)
		}
		if meta.IsStatusConditionTrue(pol.Status.Conditions, fpv1.ConditionProgressing) {
			return fmt.Errorf("conditions %+v", pol.Status.Conditions)
		}
		return nil
	})
}

// TestWithdrawalFromAnUnavailableClusterWaitsQuietly checks a cluster that
// left the placement while its agent is offline, at the moment a lease
// granted there expires: the cluster is reported as unavailable rather than
// Revoking forever, the lease keeps it as pending, the policy is not
// re-checked every few seconds while nothing can change, and once the
// cluster is back the withdrawal completes. Placing the cluster again while
// its work is still being deleted is reported the same way.
func TestWithdrawalFromAnUnavailableClusterWaitsQuietly(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startControllerIn(t, clock.Now, "offline")
	defer stop()
	t.Cleanup(func() { releaseWorks(t) })

	f := newFleet(t, "offline", []string{"of-east", "of-west"}, []string{"of-east", "of-west"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	l := f.lease("incident-61", p.Name, time.Minute, "restart_workload")
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	active := waitLeasePhase(t, l, fpv1.LeaseActive, fpv1.ReasonLeaseActive)
	eventually(t, 10*time.Second, "the work agent's finalizer on both works", func() error {
		ackWorks(t, holdDeletion)
		for c, w := range works(t, p) {
			if !controllerutil.ContainsFinalizer(&w, workv1.ManifestWorkFinalizer) {
				return fmt.Errorf("%s has no finalizer yet", c)
			}
		}
		return nil
	})

	setClusterAvailable(t, "of-west", false)
	clock.Set(active.CreationTimestamp.Add(time.Minute + time.Second))
	f.selectClusters(t, "of-east")
	unavailable := func() error {
		pol := getPolicy(t, p)
		cs := clusterStatus(pol, "of-west")
		if cs == nil || cs.Ready || cs.Reason != "ClusterUnavailable" || !strings.Contains(cs.Message, "reconnect") {
			return fmt.Errorf("of-west status %+v", cs)
		}
		if meta.IsStatusConditionTrue(pol.Status.Conditions, fpv1.ConditionProgressing) {
			return fmt.Errorf("nothing can progress while the cluster is away: %+v", pol.Status.Conditions)
		}
		return nil
	}
	eventually(t, 10*time.Second, "of-west to be reported unavailable", func() error {
		ackWorks(t, holdDeletion)
		if got := getLease(t, l); got.Status.Phase != fpv1.LeaseExpired || strings.Join(got.Status.Clusters, ",") != "of-west" {
			return fmt.Errorf("the expired lease must still be withdrawing from of-west: %+v", got.Status)
		}
		return unavailable()
	})
	before := reconciles()
	time.Sleep(15 * time.Second)
	if n := reconciles() - before; n > 1 {
		t.Fatalf("%v reconciles in 15s while the only change waits for an unavailable cluster", n)
	}

	f.selectClusters(t, "of-east", "of-west")
	eventually(t, 10*time.Second, "the re-placed cluster to be reported unavailable", unavailable)

	setClusterAvailable(t, "of-west", true)
	eventually(t, 15*time.Second, "delivery to of-west and the withdrawal to complete once it is back", func() error {
		releaseWorks(t)
		ackWorks(t)
		pol := getPolicy(t, p)
		if pol.Status.ClusterSummary != "2/2" || !meta.IsStatusConditionTrue(pol.Status.Conditions, fpv1.ConditionReady) {
			return fmt.Errorf("status %+v", pol.Status)
		}
		if got := getLease(t, l); len(got.Status.Clusters) != 0 {
			return fmt.Errorf("lease still withdrawing from %v", got.Status.Clusters)
		}
		return nil
	})
}

// TestActiveLeaseListsClustersStillBeingWithdrawn checks that an active
// lease keeps a cluster that left the placement in status.clusters while the
// grant is still being withdrawn from it, and that this does not make the
// lease not Ready.
func TestActiveLeaseListsClustersStillBeingWithdrawn(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{t: time.Now()}
	stop := startController(t, clock.Now)
	defer stop()
	t.Cleanup(func() { releaseWorks(t) })

	f := newFleet(t, "lease-withdrawal", []string{"lw-east", "lw-west"}, []string{"lw-east", "lw-west"})
	p := f.policy("sre-remediation")
	if err := k8s.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	l := f.lease("incident-51", p.Name, 10*time.Minute, "restart_workload")
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	waitLeasePhase(t, l, fpv1.LeaseActive, fpv1.ReasonLeaseActive)
	eventually(t, 10*time.Second, "the work agent's finalizer on both works", func() error {
		ackWorks(t, holdDeletion)
		for c, w := range works(t, p) {
			if !controllerutil.ContainsFinalizer(&w, workv1.ManifestWorkFinalizer) {
				return fmt.Errorf("%s has no finalizer yet", c)
			}
		}
		return nil
	})

	f.selectClusters(t, "lw-east")
	eventually(t, 10*time.Second, "lw-west to stay listed while its grant is withdrawn", func() error {
		ackWorks(t, holdDeletion)
		got := getLease(t, l)
		if strings.Join(got.Status.Clusters, ",") != "lw-east,lw-west" || got.Status.ClusterCount != 2 {
			return fmt.Errorf("lease clusters %v", got.Status.Clusters)
		}
		if got.Status.Phase != fpv1.LeaseActive || !meta.IsStatusConditionTrue(got.Status.Conditions, fpv1.ConditionReady) {
			return fmt.Errorf("the lease must stay Active and Ready: %+v", got.Status)
		}
		return nil
	})

	eventually(t, 15*time.Second, "lw-west to be dropped once the work is gone", func() error {
		releaseWorks(t)
		ackWorks(t)
		if got := getLease(t, l); strings.Join(got.Status.Clusters, ",") != "lw-east" {
			return fmt.Errorf("lease clusters %v", got.Status.Clusters)
		}
		return nil
	})
}
