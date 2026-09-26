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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LeasePhase summarises the lease conditions for kubectl output.
// The conditions remain the source of truth.
// +kubebuilder:validation:Enum=Pending;Active;Expired;Denied
type LeasePhase string

const (
	LeasePending LeasePhase = "Pending"
	LeaseActive  LeasePhase = "Active"
	LeaseExpired LeasePhase = "Expired"
	LeaseDenied  LeasePhase = "Denied"
)

// ToolAccessLeaseSpec requests a subset of a policy for a bounded time.
type ToolAccessLeaseSpec struct {
	// PolicyRef names a FleetAccessPolicy in the same namespace.
	// +required
	PolicyRef LocalObjectReference `json:"policyRef"`

	// Subject must be one of the policy's subjects.
	// +required
	Subject Subject `json:"subject"`

	// Permissions must be a subset of the policy's permissions.
	// +required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=tool
	Permissions []Permission `json:"permissions"`

	// Duration of the lease, measured from its creation. Defaults to the
	// policy's defaultDuration and must not exceed its maxDuration. When it is
	// omitted, the default in effect at the lease's first evaluation is pinned
	// through status.expiresAt; later policy changes cannot extend it.
	// +optional
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('10s')",message="duration must be at least 10s"
	Duration *metav1.Duration `json:"duration,omitempty"`

	// Clusters optionally narrows the policy's placement to the named
	// clusters. A cluster outside the placement never receives a grant.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	Clusters []string `json:"clusters,omitempty"`

	// Reason is a free-text justification recorded for audit, such as an
	// incident or change ticket number.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	Reason string `json:"reason,omitempty"`
}

// ToolAccessLeaseStatus is the observed state of a lease.
type ToolAccessLeaseStatus struct {
	// ObservedGeneration is the generation last processed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase summarises the conditions.
	// +optional
	Phase LeasePhase `json:"phase,omitempty"`

	// ExpiresAt is creationTimestamp plus the effective duration. For a lease
	// without spec.duration, the effective duration is the policy default in
	// effect at the first evaluation, pinned here; policy changes never extend
	// it. Only the controller should be allowed to write lease status.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// Clusters that hold an active grant for this lease, sorted. After expiry
	// or denial, the clusters from which the grant is still being withdrawn.
	// +optional
	// +listType=set
	Clusters []string `json:"clusters,omitempty"`

	// ClusterCount is len(Clusters), for kubectl output.
	// +optional
	ClusterCount int32 `json:"clusterCount,omitempty"`

	// Conditions: Ready, Progressing, Degraded, Expired and Denied.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ToolAccessLease activates a subset of a FleetAccessPolicy for a bounded
// time. The spec is immutable: to change a lease, delete it and create a new
// one, so that every grant is traceable to exactly one request.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=tal,categories=fleetpermit
// +kubebuilder:printcolumn:name="Policy",type=string,JSONPath=`.spec.policyRef.name`
// +kubebuilder:printcolumn:name="Subject",type=string,JSONPath=`.spec.subject.spiffeID`,priority=1
// +kubebuilder:printcolumn:name="Clusters",type=integer,JSONPath=`.status.clusterCount`
// +kubebuilder:printcolumn:name="Expires-At",type=string,JSONPath=`.status.expiresAt`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ToolAccessLease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable; create a new ToolAccessLease instead"
	Spec ToolAccessLeaseSpec `json:"spec"`
	// +optional
	Status ToolAccessLeaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ToolAccessLeaseList contains a list of ToolAccessLease.
type ToolAccessLeaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ToolAccessLease `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ToolAccessLease{}, &ToolAccessLeaseList{})
}
