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

package ocm

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	clusterv1beta1 "open-cluster-management.io/api/cluster/v1beta1"
	workv1 "open-cluster-management.io/api/work/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
	"github.com/fleetpermit/fleetpermit/internal/enforcement"
	"github.com/fleetpermit/fleetpermit/internal/placement"
)

func work(gen int64, applied *metav1.ConditionStatus, appliedGen int64, feedback ...string) *workv1.ManifestWork {
	w := &workv1.ManifestWork{ObjectMeta: metav1.ObjectMeta{Namespace: "cluster-east", Name: "w", Generation: gen,
		Annotations: map[string]string{AnnotationDigest: "sha256:abc"}}}
	w.Spec.Workload.Manifests = []workv1.Manifest{{}}
	if applied != nil {
		w.Status.Conditions = []metav1.Condition{{Type: workv1.WorkApplied, Status: *applied, ObservedGeneration: appliedGen, Message: "detail"}}
	}
	for _, f := range feedback {
		m := workv1.ManifestCondition{ResourceMeta: workv1.ManifestResourceMeta{Kind: "XAccessPolicy", Name: "p"}}
		if f != "" {
			v, d := f, "sha256:abc"
			m.StatusFeedbacks.Values = []workv1.FeedbackValue{
				{Name: FeedbackAccepted, Value: workv1.FieldValue{Type: workv1.String, String: &v}},
				{Name: FeedbackDigest, Value: workv1.FieldValue{Type: workv1.String, String: &d}},
			}
		}
		w.Status.ResourceStatus.Manifests = append(w.Status.ResourceStatus.Manifests, m)
	}
	return w
}

// withoutDigest drops the content digest feedback from every manifest.
func withoutDigest(w *workv1.ManifestWork) *workv1.ManifestWork {
	for i := range w.Status.ResourceStatus.Manifests {
		values := w.Status.ResourceStatus.Manifests[i].StatusFeedbacks.Values
		w.Status.ResourceStatus.Manifests[i].StatusFeedbacks.Values = slices.DeleteFunc(values, func(v workv1.FeedbackValue) bool { return v.Name == FeedbackDigest })
	}
	return w
}

// withFeedback adds another status feedback value to every manifest, as an
// extra feedback rule on the ManifestWork would.
func withFeedback(w *workv1.ManifestWork, name, value string) *workv1.ManifestWork {
	for i := range w.Status.ResourceStatus.Manifests {
		v := value
		m := &w.Status.ResourceStatus.Manifests[i]
		m.StatusFeedbacks.Values = append(m.StatusFeedbacks.Values, workv1.FeedbackValue{Name: name, Value: workv1.FieldValue{Type: workv1.String, String: &v}})
	}
	return w
}

func TestWorkState(t *testing.T) {
	yes, no := metav1.ConditionTrue, metav1.ConditionFalse
	cases := []struct {
		name   string
		w      *workv1.ManifestWork
		ready  bool
		reason string
	}{
		{"not applied yet", work(1, nil, 0), false, ReasonApplying},
		{"stale generation", work(2, &yes, 1, "True"), false, ReasonApplying},
		{"apply failed", work(1, &no, 1), false, ReasonApplyFailed},
		{"no per-resource status", work(1, &yes, 1), false, ReasonAwaitingAcceptance},
		{"no feedback yet", work(1, &yes, 1, ""), false, ReasonAwaitingAcceptance},
		{"rejected by enforcement", work(1, &yes, 1, "False"), false, ReasonRejected},
		{"accepted but no digest reported", withoutDigest(work(1, &yes, 1, "True")), false, ReasonAwaitingAcceptance},
		{"acceptance reported twice", withFeedback(work(1, &yes, 1, "False"), FeedbackAccepted, "True"), false, ReasonAwaitingAcceptance},
		{"digest reported twice", withFeedback(work(1, &yes, 1, "True"), FeedbackDigest, "sha256:abc"), false, ReasonAwaitingAcceptance},
		{"enforced", work(1, &yes, 1, "True"), true, ReasonEnforced},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := WorkState(tc.w)
			if st.Ready != tc.ready || st.Reason != tc.reason {
				t.Fatalf("got ready=%v reason=%s, want ready=%v reason=%s", st.Ready, st.Reason, tc.ready, tc.reason)
			}
			if st.Cluster != "cluster-east" || st.Digest != "sha256:abc" {
				t.Fatalf("cluster/digest not propagated: %+v", st)
			}
		})
	}
}

func TestWorkNameBoundedAndUnique(t *testing.T) {
	p := &fpv1.FleetAccessPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "fleet", Name: strings.Repeat("a", 200)}}
	if n := WorkName(p); len(n) > 63 || !strings.HasPrefix(n, "fleetpermit-") {
		t.Fatalf("bad name %q", n)
	}
	q := p.DeepCopy()
	q.Namespace = "other"
	if WorkName(p) == WorkName(q) {
		t.Fatal("names must differ across namespaces")
	}
}

func TestSameManifestsIgnoresKeyOrder(t *testing.T) {
	a := []workv1.Manifest{{RawExtension: rawJSON(`{"b":1,"a":{"y":2,"x":1}}`)}}
	b := []workv1.Manifest{{RawExtension: rawJSON(`{"a":{"x":1,"y":2},"b":1}`)}}
	c := []workv1.Manifest{{RawExtension: rawJSON(`{"a":{"x":1,"y":3},"b":1}`)}}
	if !sameManifests(a, b) {
		t.Fatal("key order must not matter")
	}
	if sameManifests(a, c) || sameManifests(a, append(b, b...)) {
		t.Fatal("different content must be detected")
	}
}

func TestDesiredWorkServerSideApplyAndFeedback(t *testing.T) {
	p := &fpv1.FleetAccessPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "fleet", Name: "sre", UID: "uid-1"}}
	obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "x"}}}
	res := enforcement.Result{Objects: []*unstructured.Unstructured{obj}, Digest: "sha256:1",
		Resources: []enforcement.Resource{{Group: "g", Resource: "r", Namespace: "n", Name: "x"}}}
	w, err := (&Provider{}).desiredWork(p, "cluster-east", res)
	if err != nil {
		t.Fatal(err)
	}
	if w.Namespace != "cluster-east" || w.Labels[LabelPolicyUID] != "uid-1" || w.Annotations[AnnotationDigest] != "sha256:1" {
		t.Fatalf("unexpected metadata %+v", w.ObjectMeta)
	}
	mc := w.Spec.ManifestConfigs[0]
	if mc.UpdateStrategy.Type != workv1.UpdateStrategyTypeServerSideApply || !mc.UpdateStrategy.ServerSideApply.Force ||
		!strings.HasPrefix(mc.UpdateStrategy.ServerSideApply.FieldManager, "work-agent") {
		t.Fatalf("unexpected update strategy %+v", mc.UpdateStrategy)
	}
	if mc.FeedbackRules[0].JsonPaths[0].Name != FeedbackAccepted {
		t.Fatalf("missing acceptance feedback rule")
	}
	if w.Spec.DeleteOption.PropagationPolicy != workv1.DeletePropagationPolicyTypeForeground {
		t.Fatalf("delete option must be Foreground")
	}
}

func rawJSON(s string) runtime.RawExtension { return runtime.RawExtension{Raw: []byte(s)} }

func TestDriftDetection(t *testing.T) {
	yes, no := metav1.ConditionTrue, metav1.ConditionFalse
	w := work(1, &yes, 1, "True")
	if d := Drift(w); d != "" {
		t.Fatalf("no drift expected, got %q", d)
	}
	missing := w.DeepCopy()
	missing.Status.Conditions = append(missing.Status.Conditions, metav1.Condition{Type: workv1.WorkAvailable, Status: no, ObservedGeneration: 1})
	if Drift(missing) == "" || WorkState(missing).Reason != ReasonDrifted {
		t.Fatal("a missing delivered object must be reported as drift")
	}
	edited := w.DeepCopy()
	other := "sha256:tampered"
	edited.Status.ResourceStatus.Manifests[0].StatusFeedbacks.Values = append(edited.Status.ResourceStatus.Manifests[0].StatusFeedbacks.Values,
		workv1.FeedbackValue{Name: FeedbackDigest, Value: workv1.FieldValue{Type: workv1.String, String: &other}})
	if Drift(edited) == "" {
		t.Fatal("a digest mismatch must be reported as drift")
	}
	// work() reports the matching digest.
	if Drift(w) != "" || !WorkState(w).Ready {
		t.Fatal("a matching digest is not drift")
	}
}

func TestResyncIsRateLimited(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	o := &Provider{Now: func() time.Time { return now }}
	w := &workv1.ManifestWork{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}}}
	if !o.resyncDue(w) {
		t.Fatal("first resync must be allowed")
	}
	w.Labels[LabelResync] = strconv.FormatInt(now.Add(-5*time.Second).Unix(), 10)
	if o.resyncDue(w) {
		t.Fatal("resync within the backoff must be suppressed")
	}
	w.Labels[LabelResync] = strconv.FormatInt(now.Add(-resyncBackoff).Unix(), 10)
	if !o.resyncDue(w) {
		t.Fatal("resync after the backoff must be allowed")
	}
}

// ownedWork returns a ManifestWork in the cluster namespace labelled as
// belonging to the policy UID.
func ownedWork(cluster, name, ownerUID string) *workv1.ManifestWork {
	return &workv1.ManifestWork{ObjectMeta: metav1.ObjectMeta{
		Namespace: cluster, Name: name, UID: types.UID(cluster + "/" + name),
		Labels: map[string]string{LabelManagedBy: ManagedByValue, LabelPolicyUID: ownerUID},
	}}
}

func fakeProvider(t *testing.T, objs ...client.Object) (*Provider, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := workv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	if err := clusterv1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	if err := clusterv1beta1.Install(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithIndex(&workv1.ManifestWork{}, IndexPolicy, workPolicy).WithObjects(objs...).Build()
	return &Provider{Client: c}, c
}

func managedCluster(name string, available bool) *clusterv1.ManagedCluster {
	status := metav1.ConditionTrue
	if !available {
		status = metav1.ConditionUnknown
	}
	return &clusterv1.ManagedCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: clusterv1.ManagedClusterStatus{Conditions: []metav1.Condition{
			{Type: clusterv1.ManagedClusterConditionAvailable, Status: status, Reason: "Test"},
		}},
	}
}

func workNames(t *testing.T, c client.Client) []string {
	t.Helper()
	var list workv1.ManifestWorkList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, w := range list.Items {
		names = append(names, w.Namespace+"/"+w.Name)
	}
	sort.Strings(names)
	return names
}

func TestRemoveDeletesOnlyThisPolicysWorksOnThatCluster(t *testing.T) {
	p := &fpv1.FleetAccessPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "sre", UID: "policy-a"}}
	current := WorkName(p)
	o, c := fakeProvider(t,
		ownedWork("cluster-east", current, "policy-a"),
		ownedWork("cluster-east", "fleetpermit-sre-0123abcd", "policy-a"), // earlier naming scheme
		ownedWork("cluster-east", "fleetpermit-other-1111", "policy-b"),
		ownedWork("cluster-west", current, "policy-b"), // same name, another policy
		ownedWork("cluster-edge", current, "policy-a"),
	)
	if err := o.Remove(context.Background(), p, "cluster-east"); err != nil {
		t.Fatal(err)
	}
	if err := o.Remove(context.Background(), p, "cluster-west"); err != nil {
		t.Fatal(err)
	}
	want := []string{"cluster-east/fleetpermit-other-1111", "cluster-edge/" + current, "cluster-west/" + current}
	if got := workNames(t, c); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("remaining works:\n got %v\nwant %v", got, want)
	}
}

func TestObserveReportsDeliveriesUnderAnEarlierName(t *testing.T) {
	p := &fpv1.FleetAccessPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "sre", UID: "policy-a"}}
	current := ownedWork("cluster-east", WorkName(p), "policy-a")
	current.Annotations = map[string]string{AnnotationDigest: "sha256:new"}
	o, _ := fakeProvider(t,
		current,
		ownedWork("cluster-east", "fleetpermit-sre-0123abcd", "policy-a"),
		ownedWork("cluster-west", "fleetpermit-sre-0123abcd", "policy-a"),
		ownedWork("cluster-edge", "fleetpermit-sre-0123abcd", "policy-b"),
	)
	got, err := o.Observe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("observed %d clusters, want cluster-east and cluster-west: %+v", len(got), got)
	}
	if got["cluster-east"].Digest != "sha256:new" {
		t.Fatalf("cluster-east must report the current work, got %+v", got["cluster-east"])
	}
	if w := got["cluster-west"]; w.Ready || w.Reason != ReasonSuperseded {
		t.Fatalf("cluster-west holds only an earlier-named work and must be reported superseded, got %+v", w)
	}
}

func TestSelectedClustersTrustsOnlyDecisionsOfThePlacement(t *testing.T) {
	p := &fpv1.FleetAccessPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "sre", UID: "policy-a"}}
	p.Spec.Placement.PlacementRef.Name = "production"
	pl := &clusterv1beta1.Placement{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "production", UID: "placement-1"}}
	other := &clusterv1beta1.Placement{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "other", UID: "placement-2"}}
	decision := func(name string, owner *clusterv1beta1.Placement, clusters ...string) *clusterv1beta1.PlacementDecision {
		d := &clusterv1beta1.PlacementDecision{ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: name, Labels: map[string]string{clusterv1beta1.PlacementLabel: "production"},
		}}
		if owner != nil {
			d.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(owner, clusterv1beta1.SchemeGroupVersion.WithKind("Placement"))}
		}
		for _, c := range clusters {
			d.Status.Decisions = append(d.Status.Decisions, clusterv1beta1.ClusterDecision{ClusterName: c})
		}
		return d
	}
	o, _ := fakeProvider(t, pl, other,
		decision("production-decision-1", pl, "cluster-east", "cluster-west"),
		decision("forged-without-owner", nil, "cluster-rogue"),
		decision("forged-with-another-owner", other, "cluster-foreign"),
	)
	got, err := o.SelectedClusters(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "cluster-east,cluster-west" {
		t.Fatalf("selected %v; decisions not controlled by the Placement must be ignored", got)
	}
}

func TestApplyRestoresTamperedWorkSpec(t *testing.T) {
	ctx := context.Background()
	p := &fpv1.FleetAccessPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "sre", UID: "policy-a"}}
	obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "x", "namespace": "n"}}}
	res := enforcement.Result{Objects: []*unstructured.Unstructured{obj}, Digest: "sha256:1",
		Resources: []enforcement.Resource{{Resource: "configmaps", Namespace: "n", Name: "x"}}}
	o, c := fakeProvider(t)
	if err := o.Apply(ctx, p, "cluster-east", res); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: "cluster-east", Name: WorkName(p)}
	var delivered workv1.ManifestWork
	if err := c.Get(ctx, key, &delivered); err != nil {
		t.Fatal(err)
	}
	want := &delivered.Spec
	cases := map[string]func(*workv1.ManifestWork){
		"orphaning delete option": func(w *workv1.ManifestWork) {
			w.Spec.DeleteOption = &workv1.DeleteOption{PropagationPolicy: workv1.DeletePropagationPolicyTypeOrphan}
		},
		"create-only update strategy": func(w *workv1.ManifestWork) {
			w.Spec.ManifestConfigs[0].UpdateStrategy = &workv1.UpdateStrategy{Type: workv1.UpdateStrategyTypeCreateOnly}
		},
		"added executor": func(w *workv1.ManifestWork) {
			w.Spec.Executor = &workv1.ManifestWorkExecutor{Subject: workv1.ManifestWorkExecutorSubject{
				Type: workv1.ExecutorSubjectTypeServiceAccount, ServiceAccount: &workv1.ManifestWorkSubjectServiceAccount{Namespace: "kube-system", Name: "powerful"},
			}}
		},
		// Fields FleetPermit leaves unset.
		"ignored fields": func(w *workv1.ManifestWork) {
			w.Spec.ManifestConfigs[0].UpdateStrategy.ServerSideApply.IgnoreFields = []workv1.IgnoreField{{
				Condition: workv1.IgnoreFieldsConditionOnSpokeChange, JSONPaths: []string{".data"},
			}}
		},
		"condition rules": func(w *workv1.ManifestWork) {
			w.Spec.ManifestConfigs[0].ConditionRules = []workv1.ConditionRule{{Condition: "Complete", Type: workv1.WellKnownConditionsType}}
		},
		"extra manifest config": func(w *workv1.ManifestWork) {
			w.Spec.ManifestConfigs = append(w.Spec.ManifestConfigs, workv1.ManifestConfigOption{
				ResourceIdentifier: workv1.ResourceIdentifier{Resource: "configmaps", Namespace: "n", Name: "y"},
				UpdateStrategy:     &workv1.UpdateStrategy{Type: workv1.UpdateStrategyTypeReadOnly},
			})
		},
		"extra feedback path": func(w *workv1.ManifestWork) {
			rule := &w.Spec.ManifestConfigs[0].FeedbackRules[0]
			rule.JsonPaths = append(rule.JsonPaths, workv1.JsonPath{Name: FeedbackAccepted, Path: ".metadata.name"})
		},
		"extra feedback rule": func(w *workv1.ManifestWork) {
			mc := &w.Spec.ManifestConfigs[0]
			mc.FeedbackRules = append(mc.FeedbackRules, workv1.FeedbackRule{Type: workv1.WellKnownStatusType})
		},
		"deletion TTL": func(w *workv1.ManifestWork) {
			ttl := int64(1)
			w.Spec.DeleteOption.TTLSecondsAfterFinished = &ttl
		},
		"selective orphaning": func(w *workv1.ManifestWork) {
			w.Spec.DeleteOption.SelectivelyOrphan = &workv1.SelectivelyOrphan{OrphaningRules: []workv1.OrphaningRule{{Resource: "configmaps", Namespace: "n", Name: "x"}}}
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			var w workv1.ManifestWork
			if err := c.Get(ctx, key, &w); err != nil {
				t.Fatal(err)
			}
			tamper(&w)
			if err := c.Update(ctx, &w); err != nil {
				t.Fatal(err)
			}
			if err := o.Apply(ctx, p, "cluster-east", res); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, key, &w); err != nil {
				t.Fatal(err)
			}
			if !equality.Semantic.DeepEqual(&w.Spec, want) {
				t.Fatalf("spec not restored:\n got %+v\nwant %+v", w.Spec, *want)
			}
		})
	}
}

func TestObserveReportsWorksBeingDeleted(t *testing.T) {
	p := &fpv1.FleetAccessPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "sre", UID: "policy-a"}}
	deleting := ownedWork("cluster-west", WorkName(p), "policy-a")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{workv1.ManifestWorkFinalizer}
	o, _ := fakeProvider(t, deleting)
	got, err := o.Observe(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	st, found := got["cluster-west"]
	if !found || st.Ready || st.Reason != ReasonDeleting {
		t.Fatalf("a work that is still being deleted must be reported as present and not ready, got %+v (found %v)", st, found)
	}
}

func annotatedWork(cluster, name, owner, policy string) *workv1.ManifestWork {
	w := ownedWork(cluster, name, owner)
	w.Annotations = map[string]string{AnnotationPolicy: policy}
	return w
}

func TestWithdrawDeletesOnlyTheMissingPolicysWorks(t *testing.T) {
	o, c := fakeProvider(t,
		annotatedWork("cluster-east", "fleetpermit-sre-1", "policy-a", "team-a/sre"),
		annotatedWork("cluster-west", "fleetpermit-sre-1", "policy-a", "team-a/sre"),
		annotatedWork("cluster-east", "fleetpermit-other-1", "policy-b", "team-a/other"),
		annotatedWork("cluster-east", "fleetpermit-sre-2", "policy-c", "team-b/sre"),
		// Annotated for the policy but not FleetPermit's: another name, or a
		// namespace that is not a managed cluster's.
		annotatedWork("cluster-east", "not-fleetpermit", "policy-a", "team-a/sre"),
		annotatedWork("default", "fleetpermit-sre-1", "policy-a", "team-a/sre"),
		managedCluster("cluster-east", true), managedCluster("cluster-west", true),
	)
	if _, err := o.Withdraw(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "sre"}, ""); err != nil {
		t.Fatal(err)
	}
	want := []string{"cluster-east/fleetpermit-other-1", "cluster-east/fleetpermit-sre-2", "cluster-east/not-fleetpermit", "default/fleetpermit-sre-1"}
	if got := workNames(t, c); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("remaining works:\n got %v\nwant %v", got, want)
	}
}

func applyResult() enforcement.Result {
	obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "x", "namespace": "n"}}}
	return enforcement.Result{Objects: []*unstructured.Unstructured{obj}, Digest: "sha256:1",
		Resources: []enforcement.Resource{{Resource: "configmaps", Namespace: "n", Name: "x"}}}
}

// TestApplyReplacesTheWorkOfAnEarlierPolicyWithTheSameName covers a policy
// deleted without its finalizer and created again under the same name: the
// earlier policy's ManifestWork is deleted so that the new one can be
// delivered, and delivery is reported as in progress. A ManifestWork of any
// other owner is still refused.
func TestApplyReplacesTheWorkOfAnEarlierPolicyWithTheSameName(t *testing.T) {
	ctx := context.Background()
	p := &fpv1.FleetAccessPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "sre", UID: "policy-new"}}
	earlier := ownedWork("cluster-east", WorkName(p), "policy-old")
	earlier.Annotations = map[string]string{AnnotationPolicy: "team-a/sre"}
	unrelated := ownedWork("cluster-west", WorkName(p), "policy-other")
	unrelated.Annotations = map[string]string{AnnotationPolicy: "team-b/sre"}
	// A work that looks like an earlier delivery, in a namespace that is
	// not a managed cluster's: it is not deleted.
	outside := ownedWork("default", WorkName(p), "policy-old")
	outside.Annotations = map[string]string{AnnotationPolicy: "team-a/sre"}
	o, c := fakeProvider(t, earlier, unrelated, outside, managedCluster("cluster-east", true), managedCluster("cluster-west", true))

	if err := o.Apply(ctx, p, "cluster-east", applyResult()); !errors.Is(err, placement.ErrStillDeleting) {
		t.Fatalf("Apply over an earlier policy's work returned %v, want ErrStillDeleting", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(earlier), &workv1.ManifestWork{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the earlier policy's work was not deleted: %v", err)
	}
	if err := o.Apply(ctx, p, "cluster-west", applyResult()); err == nil || errors.Is(err, placement.ErrStillDeleting) ||
		!strings.Contains(err.Error(), "not owned by this policy") {
		t.Fatalf("Apply over another policy's work returned %v, want a refusal", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(unrelated), &workv1.ManifestWork{}); err != nil {
		t.Fatalf("another policy's work was touched: %v", err)
	}
	if err := o.Apply(ctx, p, "default", applyResult()); err == nil || errors.Is(err, placement.ErrStillDeleting) {
		t.Fatalf("Apply in a namespace that is not a managed cluster's returned %v, want a refusal", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(outside), &workv1.ManifestWork{}); err != nil {
		t.Fatalf("a work outside the managed clusters' namespaces was deleted: %v", err)
	}
}

// TestApplyReportsAWorkBeingDeletedAsInProgress checks that the policy's
// own ManifestWork, or one an earlier policy with the same name left behind,
// that is being deleted is reported as delivery in progress, so deleting and
// re-creating a policy is not shown as a failure.
func TestApplyReportsAWorkBeingDeletedAsInProgress(t *testing.T) {
	p := &fpv1.FleetAccessPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "sre", UID: "policy-new"}}
	deleting := ownedWork("cluster-east", WorkName(p), "policy-old")
	deleting.Annotations = map[string]string{AnnotationPolicy: "team-a/sre"}
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{workv1.ManifestWorkFinalizer}
	o, _ := fakeProvider(t, deleting, managedCluster("cluster-east", true))
	if err := o.Apply(context.Background(), p, "cluster-east", applyResult()); !errors.Is(err, placement.ErrStillDeleting) {
		t.Fatalf("Apply returned %v, want ErrStillDeleting", err)
	}

	// Only the work agent completes the deletion: on a cluster that is not
	// available, delivery waits for it to reconnect.
	o, _ = fakeProvider(t, deleting, managedCluster("cluster-east", false))
	if err := o.Apply(context.Background(), p, "cluster-east", applyResult()); !errors.Is(err, placement.ErrClusterUnavailable) {
		t.Fatalf("Apply on an unavailable cluster returned %v, want ErrClusterUnavailable", err)
	}
}

// TestApplyRefusesAForeignWorkBeingDeleted checks that a ManifestWork of
// another owner is refused even while it is being deleted: that is a
// delivery failure, retried with the delivery backoff, not delivery in
// progress.
func TestApplyRefusesAForeignWorkBeingDeleted(t *testing.T) {
	p := &fpv1.FleetAccessPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "sre", UID: "policy-new"}}
	foreign := ownedWork("cluster-east", WorkName(p), "someone-else")
	now := metav1.Now()
	foreign.DeletionTimestamp = &now
	foreign.Finalizers = []string{workv1.ManifestWorkFinalizer}
	o, _ := fakeProvider(t, foreign, managedCluster("cluster-east", true))
	err := o.Apply(context.Background(), p, "cluster-east", applyResult())
	if err == nil || errors.Is(err, placement.ErrStillDeleting) || !strings.Contains(err.Error(), "not owned by this policy") {
		t.Fatalf("Apply returned %v, want a refusal", err)
	}
}

// TestWithdrawKeepsTheCurrentPolicysWorks checks that Withdraw deletes what
// an earlier policy with the same name left behind, keeps the current
// policy's works, and reports the clusters that still hold a work being
// withdrawn, as unavailable where the cluster is.
func TestWithdrawKeepsTheCurrentPolicysWorks(t *testing.T) {
	held := annotatedWork("cluster-edge", "fleetpermit-sre-1", "policy-old", "team-a/sre")
	now := metav1.Now()
	held.DeletionTimestamp = &now
	held.Finalizers = []string{workv1.ManifestWorkFinalizer}
	o, c := fakeProvider(t,
		annotatedWork("cluster-east", "fleetpermit-sre-1", "policy-new", "team-a/sre"),
		annotatedWork("cluster-west", "fleetpermit-sre-1", "policy-old", "team-a/sre"),
		held,
		managedCluster("cluster-east", true), managedCluster("cluster-west", true), managedCluster("cluster-edge", false),
	)
	left, err := o.Withdraw(context.Background(), types.NamespacedName{Namespace: "team-a", Name: "sre"}, "policy-new")
	if err != nil {
		t.Fatal(err)
	}
	if got := workNames(t, c); strings.Join(got, " ") != "cluster-east/fleetpermit-sre-1 cluster-edge/fleetpermit-sre-1" {
		t.Fatalf("only the current policy's work and the one still being deleted must remain, got %v", got)
	}
	if len(left) != 2 || left["cluster-west"].Reason != ReasonDeleting || left["cluster-edge"].Reason != ReasonClusterUnavailable ||
		left["cluster-west"].Ready || !strings.Contains(left["cluster-edge"].Message, "reconnect") {
		t.Fatalf("clusters still being withdrawn from: %+v", left)
	}
}
