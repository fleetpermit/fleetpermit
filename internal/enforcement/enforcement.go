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

// Package enforcement defines the seam between FleetPermit's decisions and
// the data plane that enforces them on each cluster.
package enforcement

import (
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
)

// Grant is one unit of authority for one subject on one cluster.
type Grant struct {
	// ID is stable for the lifetime of the grant (derived from the lease UID,
	// or from the subject for standing grants).
	ID string
	// Lease is the lease name, empty for standing grants.
	Lease string
	// Subject is the SPIFFE ID of the caller.
	Subject string
	// Tools is the sorted set of tools the subject may call.
	Tools []string
	// ExpiresAt bounds the grant in the data plane. Zero means no expiry
	// (only used when a policy does not require leases).
	ExpiresAt time.Time
}

// Request describes everything that must be enforced on one cluster for one
// FleetAccessPolicy. Grants are in priority order.
type Request struct {
	Policy  *fpv1.FleetAccessPolicy
	Cluster string
	Grants  []Grant
}

// Resource identifies one rendered object on the managed cluster.
type Resource struct {
	Group     string
	Resource  string
	Namespace string
	Name      string
}

// Result is the rendered enforcement content for one cluster.
type Result struct {
	// Objects are the manifests to apply. Empty means nothing to enforce.
	Objects []*unstructured.Unstructured
	// Resources identifies the objects, for status feedback.
	Resources []Resource
	// Digest is the SHA-256 digest of the rendered content.
	Digest string
	// Rendered lists the grant IDs that were rendered.
	Rendered []string
	// Dropped lists grant IDs that did not fit the provider's capacity.
	Dropped []string
}

// Renderer turns grants into objects understood by an enforcement layer.
type Renderer interface {
	Render(Request) (Result, error)
}
