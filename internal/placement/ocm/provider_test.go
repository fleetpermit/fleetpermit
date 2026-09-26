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
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	workv1 "open-cluster-management.io/api/work/v1"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
	"github.com/fleetpermit/fleetpermit/internal/enforcement"
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
			v := f
			m.StatusFeedbacks.Values = []workv1.FeedbackValue{{Name: FeedbackAccepted, Value: workv1.FieldValue{Type: workv1.String, String: &v}}}
		}
		w.Status.ResourceStatus.Manifests = append(w.Status.ResourceStatus.Manifests, m)
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
		{"not applied yet", work(1, nil, 0), false, reasonApplying},
		{"stale generation", work(2, &yes, 1, "True"), false, reasonApplying},
		{"apply failed", work(1, &no, 1), false, reasonApplyFailed},
		{"no per-resource status", work(1, &yes, 1), false, reasonAwaitingAccept},
		{"no feedback yet", work(1, &yes, 1, ""), false, reasonAwaitingAccept},
		{"rejected by enforcement", work(1, &yes, 1, "False"), false, reasonRejected},
		{"enforced", work(1, &yes, 1, "True"), true, reasonEnforced},
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
	if Drift(missing) == "" || WorkState(missing).Reason != reasonDrifted {
		t.Fatal("a missing delivered object must be reported as drift")
	}
	edited := w.DeepCopy()
	other := "sha256:tampered"
	edited.Status.ResourceStatus.Manifests[0].StatusFeedbacks.Values = append(edited.Status.ResourceStatus.Manifests[0].StatusFeedbacks.Values,
		workv1.FeedbackValue{Name: FeedbackDigest, Value: workv1.FieldValue{Type: workv1.String, String: &other}})
	if Drift(edited) == "" {
		t.Fatal("a digest mismatch must be reported as drift")
	}
	same := w.DeepCopy()
	d := "sha256:abc"
	same.Status.ResourceStatus.Manifests[0].StatusFeedbacks.Values = append(same.Status.ResourceStatus.Manifests[0].StatusFeedbacks.Values,
		workv1.FeedbackValue{Name: FeedbackDigest, Value: workv1.FieldValue{Type: workv1.String, String: &d}})
	if Drift(same) != "" || !WorkState(same).Ready {
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
