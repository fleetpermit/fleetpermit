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
	"sort"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	clusterv1beta1 "open-cluster-management.io/api/cluster/v1beta1"
	workv1 "open-cluster-management.io/api/work/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
	"github.com/fleetpermit/fleetpermit/internal/enforcement/agenticnetworking"
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
	pl := &clusterv1beta1.Placement{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: f.placement}}
	if err := k8s.Create(ctx, pl); err != nil {
		t.Fatal(err)
	}
	pd := &clusterv1beta1.PlacementDecision{ObjectMeta: metav1.ObjectMeta{
		Namespace: ns, Name: f.placement + "-decision-1",
		Labels: map[string]string{clusterv1beta1.PlacementLabel: f.placement},
	}}
	if err := k8s.Create(ctx, pd); err != nil {
		t.Fatal(err)
	}
	f.selectClusters(t, selected...)
	return f
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

// ackWorks plays the role of the OCM work agent and the enforcement
// controller: it marks every FleetPermit ManifestWork as applied and accepted.
func ackWorks(t testing.TB) {
	t.Helper()
	ctx := context.Background()
	var list workv1.ManifestWorkList
	if err := k8s.List(ctx, &list, client.MatchingLabels{ocm.LabelManagedBy: ocm.ManagedByValue}); err != nil {
		t.Fatal(err)
	}
	for i := range list.Items {
		w := &list.Items[i]
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
				Metadata struct{ Name, Namespace string } `json:"metadata"`
			}
			_ = json.Unmarshal(m.Raw, &obj)
			w.Status.ResourceStatus.Manifests = append(w.Status.ResourceStatus.Manifests, workv1.ManifestCondition{
				ResourceMeta: workv1.ManifestResourceMeta{Ordinal: int32(j), Group: "agentic.networking.x-k8s.io", Version: "v1alpha1",
					Kind: "XAccessPolicy", Resource: "xaccesspolicies", Name: obj.Metadata.Name, Namespace: obj.Metadata.Namespace},
				StatusFeedbacks: workv1.StatusFeedbackResult{Values: []workv1.FeedbackValue{{
					Name: ocm.FeedbackAccepted, Value: workv1.FieldValue{Type: workv1.String, String: &accepted},
				}}},
				Conditions: []metav1.Condition{{Type: workv1.ManifestApplied, Status: metav1.ConditionTrue, Reason: "Test", LastTransitionTime: metav1.Now()}},
			})
		}
		if err := k8s.Status().Update(ctx, w); err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
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
func poke(t testing.TB, obj client.Object) {
	t.Helper()
	ctx := context.Background()
	if err := k8s.Get(ctx, clientKey(obj), obj); err != nil {
		t.Fatal(err)
	}
	a := obj.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	a["test.fleetpermit.github.io/poke"] = fmt.Sprint(time.Now().UnixNano())
	obj.SetAnnotations(a)
	if err := k8s.Update(ctx, obj); err != nil {
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
	var cur fpv1.FleetAccessPolicy
	if err := k8s.Get(ctx, clientKey(p), &cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Permissions = append(cur.Spec.Permissions, fpv1.Permission{Tool: "read_secret"})
	if err := k8s.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
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
	tampered := orig.DeepCopy()
	tampered.Spec.Workload.Manifests[0].Raw = []byte(strings.Replace(string(tampered.Spec.Workload.Manifests[0].Raw), "restart_workload", "read_secret", 1))
	if err := k8s.Update(ctx, tampered); err != nil {
		t.Fatal(err)
	}
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
	eventually(t, 10*time.Second, "grant delivered despite status churn", func() error {
		if len(grantWorks(t, p)) != 1 {
			return fmt.Errorf("grant not delivered yet")
		}
		return nil
	})
	if took := time.Since(start); took > 3*time.Second {
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

	var cur fpv1.FleetAccessPolicy
	if err := k8s.Get(ctx, clientKey(p), &cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Lease.DefaultDuration = &metav1.Duration{Duration: 9 * time.Minute}
	if err := k8s.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
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
