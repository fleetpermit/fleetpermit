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

	"k8s.io/apimachinery/pkg/api/equality"
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
	LabelResync   = "fleetpermit.github.io/resync"
	resyncBackoff = 10 * time.Second
)

// Reasons reported in placement.ClusterState.
const (
	ReasonDrifted            = "Drifted"
	ReasonEnforced           = "Enforced"
	ReasonApplying           = "Applying"
	ReasonApplyFailed        = "ApplyFailed"
	ReasonAwaitingAcceptance = "AwaitingAcceptance"
	ReasonRejected           = "RejectedByEnforcement"
	ReasonClusterUnavailable = "ClusterUnavailable"
	ReasonSuperseded         = "Superseded"
	ReasonDeleting           = "Deleting"
)

// Provider delivers FleetPermit content through Open Cluster Management.
type Provider struct {
	Client client.Client
	// Executor, when set, makes the OCM work agent check each delivered
	// object against this managed-cluster ServiceAccount's permissions before
	// applying it; the agent still writes with its own identity.
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
	suffix := "-" + digest.Short(p.Namespace+"/"+p.Name, 16)
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
		// OCM makes the Placement the controller of the decisions it writes;
		// the label alone can be set by anyone who may create decisions.
		if !metav1.IsControlledBy(&d, &pl) {
			continue
		}
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
		return o.pruneStale(ctx, p, cluster, desired.Name)
	}
	if err != nil {
		return fmt.Errorf("getting ManifestWork %s/%s: %w", cluster, desired.Name, err)
	}
	if !current.DeletionTimestamp.IsZero() {
		return o.stillDeleting(ctx, &current)
	}
	// Never take over a ManifestWork that belongs to another policy: refuse
	// and report instead of overwriting another tenant's grants. The one
	// exception is a work that an earlier policy with this namespace and
	// name left behind, because it was deleted without FleetPermit's
	// cleanup: it is deleted, and replaced once it is gone.
	if owner := current.Labels[LabelPolicyUID]; owner != string(p.UID) {
		if current.Labels[LabelManagedBy] != ManagedByValue || current.Annotations[AnnotationPolicy] != p.Namespace+"/"+p.Name {
			return fmt.Errorf("ManifestWork %s/%s exists and is not owned by this policy (owner policy UID %q); refusing to overwrite it", cluster, desired.Name, owner)
		}
		if err := o.deleteWork(ctx, &current); err != nil {
			return err
		}
		return o.stillDeleting(ctx, &current)
	}
	if current.Annotations[AnnotationDigest] == res.Digest &&
		current.Labels[LabelPolicyUID] == string(p.UID) &&
		current.Annotations[AnnotationWorkDigest] == desired.Annotations[AnnotationWorkDigest] &&
		sameManifests(current.Spec.Workload.Manifests, desired.Spec.Workload.Manifests) &&
		sameSpec(desired.Spec, current.Spec) {
		// Content is current on the hub. If the managed cluster reports that
		// the delivered object is missing or differs, ask the work agent to
		// re-apply now instead of waiting for its periodic resync.
		if Drift(&current) != "" && o.resyncDue(&current) {
			base := current.DeepCopy()
			current.Labels[LabelResync] = strconv.FormatInt(o.now().Unix(), 10)
			if err := o.Client.Patch(ctx, &current, client.MergeFrom(base)); err != nil {
				return fmt.Errorf("requesting re-apply of ManifestWork %s/%s: %w", cluster, desired.Name, err)
			}
		}
		return o.pruneStale(ctx, p, cluster, desired.Name)
	}
	// A merge patch without an optimistic lock: FleetPermit owns the spec,
	// labels and annotations, while the OCM work agent continuously writes
	// status. A read-modify-write Update from the informer cache would
	// conflict with those status writes and delay delivery.
	base := current.DeepCopy()
	current.Labels = desired.Labels
	current.Annotations = desired.Annotations
	current.Spec = desired.Spec
	if err := o.Client.Patch(ctx, &current, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("updating ManifestWork %s/%s: %w", cluster, desired.Name, err)
	}
	return o.pruneStale(ctx, p, cluster, desired.Name)
}

// pruneStale deletes this policy's ManifestWorks in the cluster namespace
// whose name is not the current one (for example after a naming change), so
// a cluster never holds two deliveries for one policy.
func (o *Provider) pruneStale(ctx context.Context, p *fpv1.FleetAccessPolicy, cluster, keep string) error {
	return o.deleteOwned(ctx, p, cluster, keep)
}

// Remove deletes every ManifestWork the policy owns in the cluster namespace,
// including deliveries under an earlier name. The OCM work agent then deletes
// the delivered objects from the managed cluster.
func (o *Provider) Remove(ctx context.Context, p *fpv1.FleetAccessPolicy, cluster string) error {
	return o.deleteOwned(ctx, p, cluster, "")
}

// deleteOwned deletes the ManifestWorks in the cluster namespace that carry
// the policy's UID label, except the one named keep. Each delete is
// conditional on the listed object's UID, so a ManifestWork that belongs to
// another policy is never deleted, even if it reuses a name.
func (o *Provider) deleteOwned(ctx context.Context, p *fpv1.FleetAccessPolicy, cluster, keep string) error {
	var works workv1.ManifestWorkList
	if err := o.Client.List(ctx, &works, client.InNamespace(cluster), client.MatchingLabels{LabelPolicyUID: string(p.UID)}); err != nil {
		return fmt.Errorf("listing ManifestWorks in %s: %w", cluster, err)
	}
	for i := range works.Items {
		w := &works.Items[i]
		if w.Name == keep || !w.DeletionTimestamp.IsZero() {
			continue
		}
		if err := o.deleteWork(ctx, w); err != nil {
			return err
		}
	}
	return nil
}

// stillDeleting reports that the cluster's previous delivery, w, is being
// deleted. The work agent removes it, so on a cluster that is not available
// delivery waits for the cluster to reconnect.
func (o *Provider) stillDeleting(ctx context.Context, w *workv1.ManifestWork) error {
	if !o.clusterAvailable(ctx, w.Namespace) {
		return fmt.Errorf("ManifestWork %s/%s is being deleted and delivery waits for the cluster to reconnect: %w", w.Namespace, w.Name, placement.ErrClusterUnavailable)
	}
	return fmt.Errorf("ManifestWork %s/%s: %w; it is created again once the deletion completes", w.Namespace, w.Name, placement.ErrStillDeleting)
}

// IndexPolicy indexes FleetPermit's ManifestWorks by their policy
// annotation (namespace/name). Withdraw reads it; register it with IndexWorks
// on the cache the provider's client reads from.
const IndexPolicy = AnnotationPolicy

// IndexWorks registers the IndexPolicy field index.
func IndexWorks(ctx context.Context, indexer client.FieldIndexer) error {
	return indexer.IndexField(ctx, &workv1.ManifestWork{}, IndexPolicy, workPolicy)
}

func workPolicy(o client.Object) []string {
	if ref := o.GetAnnotations()[AnnotationPolicy]; ref != "" {
		return []string{ref}
	}
	return nil
}

// Withdraw deletes the ManifestWorks, on every cluster, that were delivered
// for policies with this namespace and name, except those of the policy with
// UID keep. The UID of a policy that no longer exists is unknown, so the works
// are found by their policy annotation.
func (o *Provider) Withdraw(ctx context.Context, policy types.NamespacedName, keep types.UID) error {
	var works workv1.ManifestWorkList
	if err := o.Client.List(ctx, &works, client.MatchingLabels{LabelManagedBy: ManagedByValue},
		client.MatchingFields{IndexPolicy: policy.String()}); err != nil {
		return fmt.Errorf("listing ManifestWorks: %w", err)
	}
	for i := range works.Items {
		w := &works.Items[i]
		if w.Annotations[AnnotationPolicy] != policy.String() || !w.DeletionTimestamp.IsZero() ||
			(keep != "" && w.Labels[LabelPolicyUID] == string(keep)) {
			continue
		}
		if err := o.deleteWork(ctx, w); err != nil {
			return err
		}
	}
	return nil
}

// deleteWork deletes w on condition that its UID is still the listed one, so
// an object that reuses the name is never deleted.
func (o *Provider) deleteWork(ctx context.Context, w *workv1.ManifestWork) error {
	uid := w.UID
	err := o.Client.Delete(ctx, w, client.Preconditions{UID: &uid})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("deleting ManifestWork %s/%s: %w", w.Namespace, w.Name, err)
	}
	return nil
}

// Observe reports, per cluster, the state of the ManifestWork that belongs to
// the policy. A cluster that only holds a delivery under an earlier name, or
// one that is still being deleted, is reported as not ready: the controller
// replaces it on placed clusters and removes it from all others, and the
// cluster counts as withdrawn only once the work is gone. Removing a work
// needs the work agent, so on a cluster that is not available this is
// reported as waiting for the cluster to reconnect.
func (o *Provider) Observe(ctx context.Context, p *fpv1.FleetAccessPolicy) (map[string]placement.ClusterState, error) {
	var works workv1.ManifestWorkList
	if err := o.Client.List(ctx, &works, client.MatchingLabels{LabelPolicyUID: string(p.UID)}); err != nil {
		return nil, fmt.Errorf("listing ManifestWorks: %w", err)
	}
	name := WorkName(p)
	out := make(map[string]placement.ClusterState, len(works.Items))
	leaving := func(w *workv1.ManifestWork, reason, msg, waiting string) {
		if _, seen := out[w.Namespace]; seen {
			return
		}
		st := placement.ClusterState{Cluster: w.Namespace, Reason: reason, Message: "ManifestWork " + w.Name + " " + msg}
		if !o.clusterAvailable(ctx, w.Namespace) {
			st.Reason, st.Message = ReasonClusterUnavailable, "ManifestWork "+w.Name+" "+waiting
		}
		out[w.Namespace] = st
	}
	for i := range works.Items {
		w := &works.Items[i]
		if !w.DeletionTimestamp.IsZero() {
			leaving(w, ReasonDeleting, "is being deleted; withdrawal from the cluster is in progress",
				"is being deleted; withdrawal waits for the managed cluster to reconnect")
			continue
		}
		if w.Name != name {
			leaving(w, ReasonSuperseded, "uses an earlier name and is being replaced",
				"uses an earlier name; its replacement waits for the managed cluster to reconnect")
			continue
		}
		st := WorkState(w)
		if !o.clusterAvailable(ctx, w.Namespace) {
			st.Ready = false
			st.Reason = ReasonClusterUnavailable
			st.Message = "managed cluster is not reporting as available; delivered status may be stale"
		}
		out[w.Namespace] = st
	}
	return out, nil
}

// clusterAvailable reports whether the ManagedCluster is Available. A cluster
// that cannot be read is treated as available, so its works' own status
// decides.
func (o *Provider) clusterAvailable(ctx context.Context, cluster string) bool {
	var mc clusterv1.ManagedCluster
	if err := o.Client.Get(ctx, types.NamespacedName{Name: cluster}, &mc); err != nil {
		return true
	}
	return meta.IsStatusConditionTrue(mc.Status.Conditions, clusterv1.ManagedClusterConditionAvailable)
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

// WorkState derives a cluster state from ManifestWork status. The cluster is
// ready only when the enforcement layer accepted every delivered object and
// each object reports the content digest the hub delivered.
func WorkState(w *workv1.ManifestWork) placement.ClusterState {
	want := w.Annotations[AnnotationDigest]
	st := placement.ClusterState{Cluster: w.Namespace, Digest: want}
	applied := meta.FindStatusCondition(w.Status.Conditions, workv1.WorkApplied)
	switch {
	case applied == nil || applied.ObservedGeneration != w.Generation:
		st.Reason, st.Message = ReasonApplying, "waiting for the work agent to apply the current generation"
		return st
	case applied.Status != metav1.ConditionTrue:
		st.Reason, st.Message = ReasonApplyFailed, applied.Message
		return st
	}
	if d := Drift(w); d != "" {
		st.Reason, st.Message = ReasonDrifted, d
		return st
	}
	manifests := w.Status.ResourceStatus.Manifests
	if len(manifests) < len(w.Spec.Workload.Manifests) {
		st.Reason, st.Message = ReasonAwaitingAcceptance, "waiting for per-resource status"
		return st
	}
	for _, m := range manifests {
		accepted, digest := "", ""
		for _, v := range m.StatusFeedbacks.Values {
			if v.Value.String == nil {
				continue
			}
			switch v.Name {
			case FeedbackAccepted:
				accepted = *v.Value.String
			case FeedbackDigest:
				digest = *v.Value.String
			}
		}
		switch {
		case accepted == "":
			st.Reason, st.Message = ReasonAwaitingAcceptance, fmt.Sprintf("%s %s has not been accepted by the enforcement controller yet", m.ResourceMeta.Kind, m.ResourceMeta.Name)
			return st
		case accepted != "True":
			st.Reason, st.Message = ReasonRejected, fmt.Sprintf("%s %s was not accepted by the enforcement controller", m.ResourceMeta.Kind, m.ResourceMeta.Name)
			return st
		case digest != want:
			// A differing digest is reported as drift above, so it is missing.
			st.Reason, st.Message = ReasonAwaitingAcceptance, fmt.Sprintf("%s %s has not reported its content digest yet", m.ResourceMeta.Kind, m.ResourceMeta.Name)
			return st
		}
	}
	st.Ready, st.Reason, st.Message = true, ReasonEnforced, "content applied and accepted"
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

// sameSpec reports whether current holds every spec field FleetPermit sets,
// with the same value, the same executor and the same number of manifest
// configurations, and none of the fields FleetPermit leaves unset that change
// what the work agent does: ignored fields, condition rules, a deletion TTL or
// selective orphaning. Other unset fields may carry server defaults. The
// manifests are compared by sameManifests.
func sameSpec(desired, current workv1.ManifestWorkSpec) bool {
	desired.Workload, current.Workload = workv1.ManifestsTemplate{}, workv1.ManifestsTemplate{}
	if len(current.ManifestConfigs) != len(desired.ManifestConfigs) {
		return false
	}
	for _, mc := range current.ManifestConfigs {
		if len(mc.ConditionRules) > 0 ||
			(mc.UpdateStrategy != nil && mc.UpdateStrategy.ServerSideApply != nil && len(mc.UpdateStrategy.ServerSideApply.IgnoreFields) > 0) {
			return false
		}
	}
	if d := current.DeleteOption; d != nil && (d.TTLSecondsAfterFinished != nil || d.SelectivelyOrphan != nil) {
		return false
	}
	return equality.Semantic.DeepDerivative(desired, current) &&
		equality.Semantic.DeepEqual(desired.Executor, current.Executor)
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
