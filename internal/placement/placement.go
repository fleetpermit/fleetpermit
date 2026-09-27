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

// Package placement defines the seam between FleetPermit and the multicluster
// system that selects clusters and delivers content to them.
package placement

import (
	"context"
	"errors"

	"k8s.io/apimachinery/pkg/types"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
	"github.com/fleetpermit/fleetpermit/internal/enforcement"
)

// ErrPlacementNotFound is returned when the referenced placement does not exist.
var ErrPlacementNotFound = errors.New("placement not found")

// ErrStillDeleting is returned by Apply while the cluster's previous delivery
// is still being deleted. Delivery is in progress, not failed: Apply succeeds
// once the deletion has completed.
var ErrStillDeleting = errors.New("the previous delivery is still being deleted")

// ErrClusterUnavailable is returned by Apply when delivery waits for a change
// that only the cluster can make, such as removing its previous delivery, and
// the cluster is not available. Nothing changes until it reconnects.
var ErrClusterUnavailable = errors.New("the managed cluster is not available")

// ClusterState is the observed delivery state of one cluster.
type ClusterState struct {
	Cluster string
	// Ready is true when the cluster runs the content identified by Digest
	// and the enforcement layer accepted it.
	Ready   bool
	Reason  string
	Message string
	Digest  string
}

// Provider selects clusters for a policy and delivers enforcement content.
type Provider interface {
	// SelectedClusters returns the sorted cluster names chosen for the policy.
	SelectedClusters(ctx context.Context, policy *fpv1.FleetAccessPolicy) ([]string, error)
	// Apply makes the content on the cluster match res. Apply is idempotent.
	Apply(ctx context.Context, policy *fpv1.FleetAccessPolicy, cluster string, res enforcement.Result) error
	// Remove deletes everything the policy delivered to the cluster.
	Remove(ctx context.Context, policy *fpv1.FleetAccessPolicy, cluster string) error
	// Withdraw deletes everything delivered, on every cluster, for policies
	// with this namespace and name except the one with UID keep: all of it
	// when the policy no longer exists (keep is empty), or what an earlier
	// policy with the same name left behind. It reports the clusters that
	// still hold such content until it is gone.
	Withdraw(ctx context.Context, policy types.NamespacedName, keep types.UID) (map[string]ClusterState, error)
	// Purge deletes everything delivered, on every cluster, for the policy
	// with this namespace and name (and UID, when known), finding content
	// that normal reconciles may not see. It reports the clusters that still
	// hold such content until it is gone.
	Purge(ctx context.Context, policy types.NamespacedName, uid types.UID) (map[string]ClusterState, error)
	// Observe reports every cluster that holds content for the policy,
	// including content that is still being deleted.
	Observe(ctx context.Context, policy *fpv1.FleetAccessPolicy) (map[string]ClusterState, error)
}
