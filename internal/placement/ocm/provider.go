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

// Package ocm implements placement.Provider with Open Cluster Management:
// clusters come from PlacementDecisions and content is delivered with one
// ManifestWork per (policy, cluster).
package ocm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
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
	"github.com/fleetpermit/fleetpermit/internal/digest"
	"github.com/fleetpermit/fleetpermit/internal/enforcement"
	"github.com/fleetpermit/fleetpermit/internal/placement"
)

// Labels and annotations on ManifestWorks created by FleetPermit.
const (
	LabelManagedBy   = "app.kubernetes.io/managed-by"
	ManagedByValue   = "fleetpermit"
	LabelPolicyUID   = "fleetpermit.github.io/policy-uid"
	AnnotationPolicy = "fleetpermit.github.io/policy"
	AnnotationDigest = "fleetpermit.github.io/content-digest"
	// AnnotationWorkDigest is the digest of the ManifestWork spec FleetPermit
	// generated; it is compared instead of the server-defaulted spec.
	AnnotationWorkDigest = "fleetpermit.github.io/work-digest"
	FieldManager         = "work-agent-fleetpermit" // OCM requires the work-agent prefix
	FeedbackAccepted     = "accepted"
	feedbackAcceptedPath = ".status.ancestors[0].conditions[0].status"
	FeedbackDigest       = "digest"
	feedbackDigestPath   = `.metadata.annotations.fleetpermit\.github\.io/content-digest`
	// LabelResync is changed to make the OCM work agent re-apply a work
	// immediately; OCM re-queues a ManifestWork when its labels change.
	LabelResync           = "fleetpermit.github.io/resync"
	resyncBackoff         = 10 * time.Second
	reasonDrifted         = "Drifted"
	reasonEnforced        = "Enforced"
	reasonApplying        = "Applying"
	reasonApplyFailed     = "ApplyFailed"
	reasonAwaitingAccept  = "AwaitingAcceptance"
	reasonRejected        = "RejectedByEnforcement"
	reasonClusterNotReady = "ClusterUnavailable"
)

// Provider delivers FleetPermit content through Open Cluster Management.
type Provider struct {
	Client client.Client
	// Executor, when set, makes the OCM work agent apply content as this
	// managed-cluster ServiceAccount instead of its own identity.
	Executor *workv1.ManifestWorkExecutor
	// Now returns the current time; defaults to time.Now.
	Now func() time.Time
}

func (o *Provider) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

var _ placement.Provider = &Provider{}

// WorkName is the ManifestWork name for a policy. Every cluster namespace
// holds at most one FleetPermit ManifestWork per policy.
func WorkName(p *fpv1.FleetAccessPolicy) string {
	name := "fleetpermit-" + p.Name
	suffix := "-" + digest.Short(p.Namespace+"/"+p.Name, 8)
	if len(name)+len(suffix) > 63 {
		name = strings.TrimRight(name[:63-len(suffix)], "-.")
	}
	return name + suffix
}

// SelectedClusters resolves the policy's Placement to cluster names.
func (o *Provider) SelectedClusters(ctx context.Context, p *fpv1.FleetAccessPolicy) ([]string, error) {
	ref := p.Spec.Placement.PlacementRef.Name
	var pl clusterv1beta1.Placement
	if err := o.Client.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: ref}, &pl); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s/%s", placement.ErrPlacementNotFound, p.Namespace, ref)
		}
		return nil, fmt.Errorf("getting placement %s/%s: %w", p.Namespace, ref, err)
	}
	var decisions clusterv1beta1.PlacementDecisionList
	if err := o.Client.List(ctx, &decisions, client.InNamespace(p.Namespace),
		client.MatchingLabels{clusterv1beta1.PlacementLabel: ref}); err != nil {
		return nil, fmt.Errorf("listing placement decisions for %s/%s: %w", p.Namespace, ref, err)
	}
	seen := map[string]bool{}
	var clusters []string
	for _, d := range decisions.Items {
		for _, dec := range d.Status.Decisions {
			if dec.ClusterName != "" && !seen[dec.ClusterName] {
				seen[dec.ClusterName] = true
				clusters = append(clusters, dec.ClusterName)
			}
		}
	}
	sort.Strings(clusters)
	return clusters, nil
}

// Apply creates or updates the ManifestWork for the policy on the cluster.
func (o *Provider) Apply(ctx context.Context, p *fpv1.FleetAccessPolicy, cluster string, res enforcement.Result) error {
	desired, err := o.desiredWork(p, cluster, res)
	if err != nil {
		return err
	}
	var current workv1.ManifestWork
	err = o.Client.Get(ctx, client.ObjectKeyFromObject(desired), &current)
	if len(res.Objects) == 0 {
		// OCM's admission webhook rejects ManifestWorks without manifests.
		return fmt.Errorf("refusing to deliver an empty ManifestWork %s/%s", cluster, desired.Name)
	}
	if apierrors.IsNotFound(err) {
		if err := o.Client.Create(ctx, desired); err != nil {
			return fmt.Errorf("creating ManifestWork %s/%s: %w", cluster, desired.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting ManifestWork %s/%s: %w", cluster, desired.Name, err)
	}
	if !current.DeletionTimestamp.IsZero() {
		return fmt.Errorf("ManifestWork %s/%s is still being deleted; retrying", cluster, desired.Name)
	}
	if current.Annotations[AnnotationDigest] == res.Digest &&
		current.Labels[LabelPolicyUID] == string(p.UID) &&
		current.Annotations[AnnotationWorkDigest] == desired.Annotations[AnnotationWorkDigest] &&
		sameManifests(current.Spec.Workload.Manifests, desired.Spec.Workload.Manifests) {
		// Content is current on the hub. If the managed cluster reports that
		// the delivered object is missing or differs, ask the work agent to
		// re-apply now instead of waiting for its periodic resync.
		if Drift(&current) != "" && o.resyncDue(&current) {
			current.Labels[LabelResync] = strconv.FormatInt(o.now().Unix(), 10)
			if err := o.Client.Update(ctx, &current); err != nil {
				return fmt.Errorf("requesting re-apply of ManifestWork %s/%s: %w", cluster, desired.Name, err)
			}
		}
		return nil
	}
	current.Labels = desired.Labels
	current.Annotations = desired.Annotations
	current.Spec = desired.Spec
	if err := o.Client.Update(ctx, &current); err != nil {
		return fmt.Errorf("updating ManifestWork %s/%s: %w", cluster, desired.Name, err)
	}
	return nil
}

// Remove deletes the policy's ManifestWork from the cluster namespace. The OCM
// work agent then deletes the delivered objects from the managed cluster.
func (o *Provider) Remove(ctx context.Context, p *fpv1.FleetAccessPolicy, cluster string) error {
	w := &workv1.ManifestWork{ObjectMeta: metav1.ObjectMeta{Namespace: cluster, Name: WorkName(p)}}
	if err := o.Client.Delete(ctx, w); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting ManifestWork %s/%s: %w", cluster, w.Name, err)
	}
	return nil
}

// Observe reports the state of every ManifestWork that belongs to the policy.
func (o *Provider) Observe(ctx context.Context, p *fpv1.FleetAccessPolicy) (map[string]placement.ClusterState, error) {
	var works workv1.ManifestWorkList
	if err := o.Client.List(ctx, &works, client.MatchingLabels{LabelPolicyUID: string(p.UID)}); err != nil {
		return nil, fmt.Errorf("listing ManifestWorks: %w", err)
	}
	out := make(map[string]placement.ClusterState, len(works.Items))
	for i := range works.Items {
		w := &works.Items[i]
		if !w.DeletionTimestamp.IsZero() {
			continue
		}
		st := WorkState(w)
		var mc clusterv1.ManagedCluster
		if err := o.Client.Get(ctx, types.NamespacedName{Name: w.Namespace}, &mc); err == nil {
			if !meta.IsStatusConditionTrue(mc.Status.Conditions, clusterv1.ManagedClusterConditionAvailable) {
				st.Ready = false
				st.Reason = reasonClusterNotReady
				st.Message = "managed cluster is not reporting as available; delivered status may be stale"
			}
		}
		out[w.Namespace] = st
	}
	return out, nil
}

func (o *Provider) resyncDue(w *workv1.ManifestWork) bool {
	last, err := strconv.ParseInt(w.Labels[LabelResync], 10, 64)
	if err != nil {
		return true
	}
	return o.now().Sub(time.Unix(last, 0)) >= resyncBackoff
}

// Drift reports why the delivered content on the managed cluster no longer
// matches the ManifestWork, or "" if it matches as far as OCM reports.
// Detected: the object was deleted (Available=False) or its content digest
// annotation differs. An in-place spec edit that keeps the annotation is
// corrected by the OCM work agent's periodic server-side re-apply.
func Drift(w *workv1.ManifestWork) string {
	if c := meta.FindStatusCondition(w.Status.Conditions, workv1.WorkAvailable); c != nil &&
		c.Status == metav1.ConditionFalse && c.ObservedGeneration == w.Generation {
		return "delivered object is missing on the managed cluster"
	}
	want := w.Annotations[AnnotationDigest]
	for _, m := range w.Status.ResourceStatus.Manifests {
		for _, v := range m.StatusFeedbacks.Values {
			if v.Name == FeedbackDigest && v.Value.String != nil && *v.Value.String != want {
				return "delivered object's content digest differs from the hub"
			}
		}
	}
	return ""
}

// WorkState derives a cluster state from ManifestWork status.
func WorkState(w *workv1.ManifestWork) placement.ClusterState {
	st := placement.ClusterState{Cluster: w.Namespace, Digest: w.Annotations[AnnotationDigest]}
	applied := meta.FindStatusCondition(w.Status.Conditions, workv1.WorkApplied)
	switch {
	case applied == nil || applied.ObservedGeneration != w.Generation:
		st.Reason, st.Message = reasonApplying, "waiting for the work agent to apply the current generation"
		return st
	case applied.Status != metav1.ConditionTrue:
		st.Reason, st.Message = reasonApplyFailed, applied.Message
		return st
	}
	if d := Drift(w); d != "" {
		st.Reason, st.Message = reasonDrifted, d
		return st
	}
	manifests := w.Status.ResourceStatus.Manifests
	if len(manifests) < len(w.Spec.Workload.Manifests) {
		st.Reason, st.Message = reasonAwaitingAccept, "waiting for per-resource status"
		return st
	}
	for _, m := range manifests {
		accepted := ""
		for _, v := range m.StatusFeedbacks.Values {
			if v.Name == FeedbackAccepted && v.Value.String != nil {
				accepted = *v.Value.String
			}
		}
		switch accepted {
		case "True":
		case "":
			st.Reason, st.Message = reasonAwaitingAccept, fmt.Sprintf("%s %s has not been accepted by the enforcement controller yet", m.ResourceMeta.Kind, m.ResourceMeta.Name)
			return st
		default:
			st.Reason, st.Message = reasonRejected, fmt.Sprintf("%s %s was not accepted by the enforcement controller", m.ResourceMeta.Kind, m.ResourceMeta.Name)
			return st
		}
	}
	st.Ready, st.Reason, st.Message = true, reasonEnforced, "content applied and accepted"
	return st
}

func (o *Provider) desiredWork(p *fpv1.FleetAccessPolicy, cluster string, res enforcement.Result) (*workv1.ManifestWork, error) {
	w := &workv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      WorkName(p),
			Namespace: cluster,
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
				LabelPolicyUID: string(p.UID),
			},
			Annotations: map[string]string{
				AnnotationPolicy: p.Namespace + "/" + p.Name,
				AnnotationDigest: res.Digest,
			},
		},
		Spec: workv1.ManifestWorkSpec{
			Executor: o.Executor,
			DeleteOption: &workv1.DeleteOption{
				PropagationPolicy: workv1.DeletePropagationPolicyTypeForeground,
			},
		},
	}
	for _, obj := range res.Objects {
		raw, err := obj.MarshalJSON()
		if err != nil {
			return nil, fmt.Errorf("encoding manifest: %w", err)
		}
		w.Spec.Workload.Manifests = append(w.Spec.Workload.Manifests, workv1.Manifest{RawExtension: runtime.RawExtension{Raw: raw}})
	}
	for _, r := range res.Resources {
		w.Spec.ManifestConfigs = append(w.Spec.ManifestConfigs, workv1.ManifestConfigOption{
			ResourceIdentifier: workv1.ResourceIdentifier{Group: r.Group, Resource: r.Resource, Namespace: r.Namespace, Name: r.Name},
			FeedbackRules: []workv1.FeedbackRule{{
				Type: workv1.JSONPathsType,
				JsonPaths: []workv1.JsonPath{
					{Name: FeedbackAccepted, Path: feedbackAcceptedPath},
					{Name: FeedbackDigest, Path: feedbackDigestPath},
				},
			}},
			// Server-side apply with force: out-of-band edits on the managed
			// cluster are overwritten on the next resync (drift correction).
			UpdateStrategy: &workv1.UpdateStrategy{
				Type:            workv1.UpdateStrategyTypeServerSideApply,
				ServerSideApply: &workv1.ServerSideApplyConfig{Force: true, FieldManager: FieldManager},
			},
		})
	}
	d, err := digest.Of(w.Spec)
	if err != nil {
		return nil, err
	}
	w.Annotations[AnnotationWorkDigest] = d
	return w, nil
}

func sameManifests(a, b []workv1.Manifest) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(canonical(a[i].Raw), canonical(b[i].Raw)) {
			return false
		}
	}
	return true
}

func canonical(raw []byte) []byte {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}
