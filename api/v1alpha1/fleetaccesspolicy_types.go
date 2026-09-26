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

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Placement providers.
const (
	// PlacementProviderOCM resolves clusters from an Open Cluster Management
	// Placement and distributes grants with ManifestWork.
	PlacementProviderOCM = "ocm"
)

// Enforcement providers.
const (
	// EnforcementProviderAgenticNetworking renders XAccessPolicy objects from
	// Kubernetes SIG Network's kube-agentic-networking project.
	EnforcementProviderAgenticNetworking = "kubernetes-agentic-networking"
)

// FailModeClosed means that errors, missing inputs and unreachable components
// lead FleetPermit to withdraw or withhold grants, never to add them. A
// backend with no grant at all stays closed only while the default-deny
// anchor policy is installed, because the upstream data plane allows every
// call to a backend that has no XAccessPolicy.
const FailModeClosed = "Closed"

// FleetAccessPolicySpec is the maximum authority that leases may activate.
type FleetAccessPolicySpec struct {
	// Subjects lists the workload identities this policy may grant to.
	// +required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=spiffeID
	Subjects []Subject `json:"subjects"`

	// Placement selects the clusters on which grants may exist.
	// +required
	Placement PlacementSpec `json:"placement"`

	// Target identifies the tool server that enforces the grants on each
	// selected cluster.
	// +required
	Target TargetSpec `json:"target"`

	// Permissions is the set of tools that leases may request.
	// +required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=tool
	Permissions []Permission `json:"permissions"`

	// Lease controls time-bound activation.
	// +optional
	// +kubebuilder:default={required:true,defaultDuration:"15m",maxDuration:"1h"}
	Lease LeaseSettings `json:"lease,omitempty"`

	// Enforcement selects how grants are enforced on each cluster.
	// +optional
	// +kubebuilder:default={}
	Enforcement EnforcementSpec `json:"enforcement,omitempty"`
}

// PlacementSpec selects target clusters.
type PlacementSpec struct {
	// Provider resolves the placement. Only "ocm" is implemented.
	// +optional
	// +kubebuilder:default=ocm
	// +kubebuilder:validation:Enum=ocm
	Provider string `json:"provider,omitempty"`

	// PlacementRef names a Placement in the same namespace as this policy.
	// +required
	PlacementRef LocalObjectReference `json:"placementRef"`
}

// TargetSpec identifies the tool server on each managed cluster.
type TargetSpec struct {
	// Protocol is the agent-to-tool protocol. Only "MCP" is implemented.
	// +optional
	// +kubebuilder:default=MCP
	// +kubebuilder:validation:Enum=MCP
	Protocol string `json:"protocol,omitempty"`

	// Namespace on each managed cluster that contains the target.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace"`

	// Ref is the resource the rendered authorization policy attaches to.
	// +required
	Ref TargetRef `json:"ref"`
}

// TargetRef references a Gateway or an XBackend on the managed cluster.
type TargetRef struct {
	// Group of the target resource.
	// +optional
	// +kubebuilder:default=agentic.networking.x-k8s.io
	// +kubebuilder:validation:Enum=agentic.networking.x-k8s.io;gateway.networking.k8s.io
	Group string `json:"group,omitempty"`

	// Kind of the target resource.
	// +optional
	// +kubebuilder:default=XBackend
	// +kubebuilder:validation:Enum=XBackend;Gateway
	Kind string `json:"kind,omitempty"`

	// Name of the target resource.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// LeaseSettings controls time-bound activation of permissions.
// +kubebuilder:validation:XValidation:rule="!has(self.defaultDuration) || !has(self.maxDuration) || duration(self.defaultDuration) <= duration(self.maxDuration)",message="defaultDuration must not exceed maxDuration"
// +kubebuilder:validation:XValidation:rule="!has(self.maxDuration) || duration(self.maxDuration) <= duration('24h')",message="maxDuration must not exceed 24h"
// +kubebuilder:validation:XValidation:rule="!has(self.defaultDuration) || duration(self.defaultDuration) >= duration('10s')",message="defaultDuration must be at least 10s"
type LeaseSettings struct {
	// Required means permissions are only usable through an active
	// ToolAccessLease. When false, every subject holds the permissions on
	// every selected cluster for as long as the policy exists.
	// +optional
	// +kubebuilder:default=true
	Required *bool `json:"required,omitempty"`

	// DefaultDuration is used when a lease does not request a duration.
	// +optional
	// +kubebuilder:default="15m"
	DefaultDuration *metav1.Duration `json:"defaultDuration,omitempty"`

	// MaxDuration is the longest duration a lease may request.
	// +optional
	// +kubebuilder:default="1h"
	MaxDuration *metav1.Duration `json:"maxDuration,omitempty"`
}

// EnforcementSpec selects the enforcement provider.
type EnforcementSpec struct {
	// Provider renders grants into enforceable objects.
	// +optional
	// +kubebuilder:default=kubernetes-agentic-networking
	// +kubebuilder:validation:Enum=kubernetes-agentic-networking
	Provider string `json:"provider,omitempty"`

	// FailMode is always Closed. The field exists so the behaviour is
	// explicit in every policy; an open mode is intentionally not offered.
	// +optional
	// +kubebuilder:default=Closed
	// +kubebuilder:validation:Enum=Closed
	FailMode string `json:"failMode,omitempty"`
}

// ClusterStatus reports the state of one target cluster.
type ClusterStatus struct {
	// Name of the managed cluster.
	Name string `json:"name"`

	// Ready is true when the cluster has applied the current content digest
	// and the enforcement layer accepted it.
	Ready bool `json:"ready"`

	// Reason is a CamelCase summary of the cluster state.
	// +optional
	Reason string `json:"reason,omitempty"`

	// Message is a human readable detail.
	// +optional
	Message string `json:"message,omitempty"`

	// Grants is the number of lease grants rendered for this cluster.
	// +optional
	Grants int32 `json:"grants,omitempty"`

	// ContentDigest is the SHA-256 digest of the desired enforcement content.
	// +optional
	ContentDigest string `json:"contentDigest,omitempty"`
}

// FleetAccessPolicyStatus is the observed state of a FleetAccessPolicy.
type FleetAccessPolicyStatus struct {
	// ObservedGeneration is the generation last processed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions: Ready, Progressing and Degraded.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// SelectedClusters is the number of clusters chosen by the placement.
	// +optional
	SelectedClusters int32 `json:"selectedClusters,omitempty"`

	// ReadyClusters is the number of selected clusters in the desired state.
	// +optional
	ReadyClusters int32 `json:"readyClusters,omitempty"`

	// ClusterSummary is ReadyClusters/SelectedClusters, for kubectl output.
	// +optional
	ClusterSummary string `json:"clusterSummary,omitempty"`

	// ActiveLeases is the number of leases currently granting authority.
	// +optional
	ActiveLeases int32 `json:"activeLeases,omitempty"`

	// Clusters holds per-cluster detail, sorted by name.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=512
	Clusters []ClusterStatus `json:"clusters,omitempty"`
}

// FleetAccessPolicy defines who may call which tools, on which clusters, and
// for how long. It is the ceiling: a ToolAccessLease can activate a subset of
// it for a bounded time, never more.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=fap,categories=fleetpermit
// +kubebuilder:printcolumn:name="Clusters",type=string,JSONPath=`.status.clusterSummary`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Active-Leases",type=integer,JSONPath=`.status.activeLeases`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type FleetAccessPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec FleetAccessPolicySpec `json:"spec"`
	// +optional
	Status FleetAccessPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FleetAccessPolicyList contains a list of FleetAccessPolicy.
type FleetAccessPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FleetAccessPolicy `json:"items"`
}

// LeaseRequired reports whether permissions need an active lease.
func (p *FleetAccessPolicy) LeaseRequired() bool {
	return p.Spec.Lease.Required == nil || *p.Spec.Lease.Required
}

// Default durations, used when the API server has not defaulted the fields.
const (
	DefaultLeaseDuration    = 15 * time.Minute
	DefaultMaxLeaseDuration = time.Hour
)

// DefaultDuration is the lease duration used when a lease requests none.
func (p *FleetAccessPolicy) DefaultDuration() time.Duration {
	if d := p.Spec.Lease.DefaultDuration; d != nil {
		return d.Duration
	}
	return DefaultLeaseDuration
}

// MaxDuration is the longest duration a lease may request.
func (p *FleetAccessPolicy) MaxDuration() time.Duration {
	if d := p.Spec.Lease.MaxDuration; d != nil {
		return d.Duration
	}
	return DefaultMaxLeaseDuration
}

func init() {
	SchemeBuilder.Register(&FleetAccessPolicy{}, &FleetAccessPolicyList{})
}
