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

package v1alpha1

// Subject identifies a workload that may call tools.
//
// Identities are SPIFFE IDs. FleetPermit never issues identities itself; it
// relies on the enforcement layer to authenticate the caller (for the
// kubernetes-agentic-networking provider: the mTLS peer certificate).
type Subject struct {
	// SPIFFEID is the SPIFFE ID of the calling workload, for example
	// spiffe://cluster.local/ns/agents/sa/sre-agent.
	// The pattern matches the one accepted by the upstream XAccessPolicy API.
	// +required
	// +kubebuilder:validation:MinLength=10
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^spiffe://[a-z0-9._-]+(/[A-Za-z0-9._-]+)*$`
	SPIFFEID string `json:"spiffeID"`
}

// Permission names one tool that may be invoked.
type Permission struct {
	// Tool is the MCP tool name, as sent in params.name of a tools/call request.
	// The character set is deliberately narrow: tool names are embedded in
	// rendered authorization rules and must never be able to alter their meaning.
	// The 20-character limit mirrors the upstream MCP params matcher.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=20
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9_.-]*$`
	Tool string `json:"tool"`
}

// LocalObjectReference references an object in the same namespace.
type LocalObjectReference struct {
	// Name of the referenced object.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// Standard condition types used by FleetPermit resources.
const (
	// ConditionReady is True when the desired authorization state has been
	// applied on every target cluster.
	ConditionReady = "Ready"
	// ConditionProgressing is True while a rollout or revocation is in flight.
	ConditionProgressing = "Progressing"
	// ConditionDegraded is True when at least one target cluster could not be
	// brought to the desired state.
	ConditionDegraded = "Degraded"
	// ConditionExpired is True once a ToolAccessLease has passed its expiry time.
	ConditionExpired = "Expired"
	// ConditionDenied is True when a ToolAccessLease violates its policy.
	ConditionDenied = "Denied"
)

// Condition reasons.
const (
	ReasonReconciled           = "Reconciled"
	ReasonRollingOut           = "RollingOut"
	ReasonClustersFailed       = "ClustersFailed"
	ReasonPlacementNotFound    = "PlacementNotFound"
	ReasonNoClustersSelected   = "NoClustersSelected"
	ReasonPolicyNotFound       = "PolicyNotFound"
	ReasonSubjectNotAllowed    = "SubjectNotAllowed"
	ReasonPermissionNotAllowed = "PermissionNotAllowed"
	ReasonDurationExceedsMax   = "DurationExceedsMaximum"
	ReasonLeaseExpired         = "LeaseExpired"
	ReasonLeaseActive          = "LeaseActive"
	ReasonNotExpired           = "NotExpired"
	ReasonAllowed              = "Allowed"
	ReasonNoEligibleClusters   = "NoEligibleClusters"
	ReasonCapacityExceeded     = "CapacityExceeded"
	ReasonDeleting             = "Deleting"
)
