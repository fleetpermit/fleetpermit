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

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
	"github.com/fleetpermit/fleetpermit/internal/enforcement"
)

// ErrPlacementNotFound is returned when the referenced placement does not exist.
var ErrPlacementNotFound = errors.New("placement not found")

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
	// Observe reports every cluster that currently holds content for the policy.
	Observe(ctx context.Context, policy *fpv1.FleetAccessPolicy) (map[string]ClusterState, error)
}
