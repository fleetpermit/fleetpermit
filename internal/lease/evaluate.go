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

// Package lease decides what a ToolAccessLease is allowed to activate.
//
// Evaluate is a pure function of the policy, the lease, the clusters the
// placement currently selects, and the current time. It never widens
// authority: every output is an intersection of what was requested and what
// the policy allows.
package lease

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
)

// Decision is the outcome of evaluating one lease.
type Decision struct {
	// Denied is true when the lease violates the policy. A denied lease grants nothing.
	Denied  bool
	Reason  string
	Message string

	// Expired is true once now >= ExpiresAt. An expired lease grants nothing.
	Expired bool

	// Duration is the effective duration and ExpiresAt the resulting expiry.
	Duration  time.Duration
	ExpiresAt time.Time

	// Tools is the sorted set of tools this lease activates.
	Tools []string

	// Clusters is the sorted set of clusters that may receive a grant:
	// the requested clusters intersected with the placement.
	Clusters []string

	// OutsidePlacement lists requested clusters that are not selected by the
	// placement and therefore receive nothing.
	OutsidePlacement []string
}

// Active reports whether the lease currently grants authority somewhere.
func (d Decision) Active() bool {
	return !d.Denied && !d.Expired && len(d.Clusters) > 0
}

// Evaluate applies the policy's rules to a lease.
func Evaluate(policy *fpv1.FleetAccessPolicy, l *fpv1.ToolAccessLease, placed []string, now time.Time) Decision {
	var d Decision

	if policy == nil {
		return deny(d, fpv1.ReasonPolicyNotFound, fmt.Sprintf("FleetAccessPolicy %q not found", l.Spec.PolicyRef.Name))
	}

	if !slices.ContainsFunc(policy.Spec.Subjects, func(s fpv1.Subject) bool {
		return s.SPIFFEID == l.Spec.Subject.SPIFFEID
	}) {
		return deny(d, fpv1.ReasonSubjectNotAllowed,
			fmt.Sprintf("subject %s is not listed in policy %s", l.Spec.Subject.SPIFFEID, policy.Name))
	}

	allowed := make(map[string]bool, len(policy.Spec.Permissions))
	for _, p := range policy.Spec.Permissions {
		allowed[p.Tool] = true
	}
	var tools, rejected []string
	for _, p := range l.Spec.Permissions {
		if !allowed[p.Tool] {
			rejected = append(rejected, p.Tool)
			continue
		}
		tools = append(tools, p.Tool)
	}
	if len(rejected) > 0 {
		sort.Strings(rejected)
		return deny(d, fpv1.ReasonPermissionNotAllowed,
			fmt.Sprintf("tools not permitted by policy %s: %s", policy.Name, strings.Join(rejected, ", ")))
	}
	d.Tools = sortedUnique(tools)

	d.Duration = policy.DefaultDuration()
	if l.Spec.Duration != nil {
		d.Duration = l.Spec.Duration.Duration
	}
	maxDuration := policy.MaxDuration()
	if d.Duration <= 0 {
		return deny(d, fpv1.ReasonDurationExceedsMax, "lease duration must be positive")
	}
	if maxDuration <= 0 || d.Duration > maxDuration {
		return deny(d, fpv1.ReasonDurationExceedsMax,
			fmt.Sprintf("requested duration %s exceeds policy maximum %s", d.Duration, maxDuration))
	}

	// Expiry is anchored to the server-assigned creation time, which the
	// requester cannot choose, and to an immutable spec.
	d.ExpiresAt = l.CreationTimestamp.Add(d.Duration).UTC().Truncate(time.Second)
	if !now.Before(d.ExpiresAt) {
		d.Expired = true
		d.Reason = fpv1.ReasonLeaseExpired
		d.Message = fmt.Sprintf("lease expired at %s", d.ExpiresAt.Format(time.RFC3339))
		return d
	}

	d.Clusters, d.OutsidePlacement = narrow(placed, l.Spec.Clusters)
	if len(d.Clusters) == 0 {
		d.Reason = fpv1.ReasonNoEligibleClusters
		d.Message = "no requested cluster is currently selected by the policy placement"
		return d
	}
	d.Reason = fpv1.ReasonLeaseActive
	d.Message = fmt.Sprintf("granting %s on %d cluster(s) until %s",
		strings.Join(d.Tools, ", "), len(d.Clusters), d.ExpiresAt.Format(time.RFC3339))
	return d
}

func deny(d Decision, reason, msg string) Decision {
	d.Denied = true
	d.Reason = reason
	d.Message = msg
	d.Tools = nil
	d.Clusters = nil
	return d
}

// narrow intersects the requested clusters with the placement. An empty
// request means "every placed cluster".
func narrow(placed, requested []string) (clusters, outside []string) {
	placed = sortedUnique(placed)
	if len(requested) == 0 {
		return placed, nil
	}
	in := make(map[string]bool, len(placed))
	for _, c := range placed {
		in[c] = true
	}
	for _, c := range sortedUnique(requested) {
		if in[c] {
			clusters = append(clusters, c)
		} else {
			outside = append(outside, c)
		}
	}
	return clusters, outside
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	sort.Strings(out)
	return slices.Compact(out)
}
