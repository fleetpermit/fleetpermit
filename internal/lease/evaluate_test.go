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

package lease

import (
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
)

const (
	sre      = "spiffe://cluster.local/ns/agents/sa/sre-agent"
	security = "spiffe://cluster.local/ns/agents/sa/security-agent"
)

var created = time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)

func policy() *fpv1.FleetAccessPolicy {
	return &fpv1.FleetAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "sre-remediation", Namespace: "fleet"},
		Spec: fpv1.FleetAccessPolicySpec{
			Subjects:    []fpv1.Subject{{SPIFFEID: sre}},
			Permissions: []fpv1.Permission{{Tool: "get_cluster_health"}, {Tool: "restart_workload"}},
			Lease: fpv1.LeaseSettings{
				DefaultDuration: dur(15 * time.Minute),
				MaxDuration:     dur(30 * time.Minute),
			},
		},
	}
}

func lease(mut ...func(*fpv1.ToolAccessLease)) *fpv1.ToolAccessLease {
	l := &fpv1.ToolAccessLease{
		ObjectMeta: metav1.ObjectMeta{Name: "incident-42", Namespace: "fleet", CreationTimestamp: metav1.NewTime(created)},
		Spec: fpv1.ToolAccessLeaseSpec{
			PolicyRef:   fpv1.LocalObjectReference{Name: "sre-remediation"},
			Subject:     fpv1.Subject{SPIFFEID: sre},
			Permissions: []fpv1.Permission{{Tool: "restart_workload"}},
		},
	}
	for _, m := range mut {
		m(l)
	}
	return l
}

func dur(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

var placed = []string{"cluster-west", "cluster-east"}

func TestEvaluateActiveLease(t *testing.T) {
	d := Evaluate(policy(), lease(), placed, created.Add(time.Minute))
	if !d.Active() || d.Denied || d.Expired {
		t.Fatalf("expected active lease, got %+v", d)
	}
	if want := created.Add(15 * time.Minute); !d.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %s, want %s (default duration)", d.ExpiresAt, want)
	}
	if !slices.Equal(d.Clusters, []string{"cluster-east", "cluster-west"}) {
		t.Errorf("Clusters = %v, want sorted placement", d.Clusters)
	}
	if !slices.Equal(d.Tools, []string{"restart_workload"}) {
		t.Errorf("Tools = %v", d.Tools)
	}
}

func TestEvaluateDenials(t *testing.T) {
	cases := []struct {
		name   string
		policy *fpv1.FleetAccessPolicy
		lease  *fpv1.ToolAccessLease
		reason string
	}{
		{"policy missing", nil, lease(), fpv1.ReasonPolicyNotFound},
		{"wrong subject", policy(), lease(func(l *fpv1.ToolAccessLease) { l.Spec.Subject.SPIFFEID = security }), fpv1.ReasonSubjectNotAllowed},
		{"permission escalation", policy(), lease(func(l *fpv1.ToolAccessLease) {
			l.Spec.Permissions = []fpv1.Permission{{Tool: "restart_workload"}, {Tool: "read_secret"}}
		}), fpv1.ReasonPermissionNotAllowed},
		{"duration escalation", policy(), lease(func(l *fpv1.ToolAccessLease) { l.Spec.Duration = dur(time.Hour) }), fpv1.ReasonDurationExceedsMax},
		{"zero duration", policy(), lease(func(l *fpv1.ToolAccessLease) { l.Spec.Duration = dur(0) }), fpv1.ReasonDurationExceedsMax},
		{"negative duration", policy(), lease(func(l *fpv1.ToolAccessLease) { l.Spec.Duration = dur(-time.Minute) }), fpv1.ReasonDurationExceedsMax},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Evaluate(tc.policy, tc.lease, placed, created)
			if !d.Denied || d.Reason != tc.reason {
				t.Fatalf("got denied=%v reason=%q, want denied reason %q", d.Denied, d.Reason, tc.reason)
			}
			if d.Active() || len(d.Clusters) != 0 || len(d.Tools) != 0 {
				t.Fatalf("a denied lease must grant nothing, got %+v", d)
			}
		})
	}
}

func TestEvaluateDurationAtMaximumIsAllowed(t *testing.T) {
	d := Evaluate(policy(), lease(func(l *fpv1.ToolAccessLease) { l.Spec.Duration = dur(30 * time.Minute) }), placed, created)
	if d.Denied {
		t.Fatalf("duration equal to maxDuration must be allowed: %s", d.Message)
	}
}

func TestEvaluateExpiryBoundary(t *testing.T) {
	l := lease(func(l *fpv1.ToolAccessLease) { l.Spec.Duration = dur(time.Minute) })
	if d := Evaluate(policy(), l, placed, created.Add(59*time.Second)); !d.Active() {
		t.Fatalf("lease must be active one second before expiry: %+v", d)
	}
	for _, at := range []time.Duration{time.Minute, time.Minute + time.Second, time.Hour} {
		d := Evaluate(policy(), l, placed, created.Add(at))
		if !d.Expired || d.Active() || d.Reason != fpv1.ReasonLeaseExpired {
			t.Fatalf("lease must be expired at +%s: %+v", at, d)
		}
	}
}

func TestEvaluateCannotBroadenPlacement(t *testing.T) {
	l := lease(func(l *fpv1.ToolAccessLease) {
		l.Spec.Clusters = []string{"cluster-east", "cluster-edge"}
	})
	d := Evaluate(policy(), l, placed, created)
	if !slices.Equal(d.Clusters, []string{"cluster-east"}) {
		t.Fatalf("Clusters = %v, want only the placed cluster-east", d.Clusters)
	}
	if !slices.Equal(d.OutsidePlacement, []string{"cluster-edge"}) {
		t.Fatalf("OutsidePlacement = %v, want cluster-edge", d.OutsidePlacement)
	}
}

func TestEvaluateOnlyOutsidePlacementGrantsNothing(t *testing.T) {
	l := lease(func(l *fpv1.ToolAccessLease) { l.Spec.Clusters = []string{"cluster-edge"} })
	d := Evaluate(policy(), l, placed, created)
	if d.Active() || d.Reason != fpv1.ReasonNoEligibleClusters {
		t.Fatalf("expected no eligible clusters, got %+v", d)
	}
}

func TestEvaluateEmptyPlacementGrantsNothing(t *testing.T) {
	d := Evaluate(policy(), lease(), nil, created)
	if d.Active() {
		t.Fatalf("a lease must not be active without placed clusters: %+v", d)
	}
}

func TestEvaluateFollowsPlacementChanges(t *testing.T) {
	d := Evaluate(policy(), lease(), []string{"cluster-east"}, created)
	if !slices.Equal(d.Clusters, []string{"cluster-east"}) {
		t.Fatalf("Clusters = %v", d.Clusters)
	}
	d = Evaluate(policy(), lease(), []string{"cluster-east", "cluster-edge"}, created)
	if !slices.Equal(d.Clusters, []string{"cluster-east", "cluster-edge"}) {
		t.Fatalf("a cluster joining the placement should receive the grant, got %v", d.Clusters)
	}
}

func TestEvaluateIsDeterministic(t *testing.T) {
	l := lease(func(l *fpv1.ToolAccessLease) {
		l.Spec.Permissions = []fpv1.Permission{{Tool: "restart_workload"}, {Tool: "get_cluster_health"}}
	})
	a := Evaluate(policy(), l, []string{"b", "a", "c"}, created)
	b := Evaluate(policy(), l, []string{"c", "a", "b", "a"}, created)
	if !slices.Equal(a.Clusters, b.Clusters) || !slices.Equal(a.Tools, b.Tools) || !a.ExpiresAt.Equal(b.ExpiresAt) {
		t.Fatalf("evaluation is not order independent: %+v vs %+v", a, b)
	}
	if !slices.Equal(a.Tools, []string{"get_cluster_health", "restart_workload"}) {
		t.Fatalf("tools not sorted: %v", a.Tools)
	}
}

func TestEvaluatePolicyNarrowingRevokesLease(t *testing.T) {
	p := policy()
	l := lease()
	if d := Evaluate(p, l, placed, created); !d.Active() {
		t.Fatalf("precondition: lease active")
	}
	p.Spec.Permissions = []fpv1.Permission{{Tool: "get_cluster_health"}}
	if d := Evaluate(p, l, placed, created); !d.Denied || d.Reason != fpv1.ReasonPermissionNotAllowed {
		t.Fatalf("removing a tool from the policy must deny the lease, got %+v", d)
	}
}

func TestEvaluateUsesBuiltInDefaultsWhenUnset(t *testing.T) {
	p := policy()
	p.Spec.Lease = fpv1.LeaseSettings{}
	d := Evaluate(p, lease(), placed, created)
	if d.Denied || d.Duration != fpv1.DefaultLeaseDuration {
		t.Fatalf("expected the built-in default duration, got %+v", d)
	}
	d = Evaluate(p, lease(func(l *fpv1.ToolAccessLease) { l.Spec.Duration = dur(2 * time.Hour) }), placed, created)
	if !d.Denied {
		t.Fatalf("the built-in maximum must still apply, got %+v", d)
	}
}

func TestEvaluatePinsDefaultDurationOnceRecorded(t *testing.T) {
	p := policy()
	l := lease() // no spec.duration: uses the policy default (15m)
	first := Evaluate(p, l, placed, created)
	if want := created.Add(15 * time.Minute); !first.ExpiresAt.Equal(want) {
		t.Fatalf("first expiry %s, want %s", first.ExpiresAt, want)
	}
	// The controller records the expiry in status.
	recorded := metav1.NewTime(first.ExpiresAt)
	l.Status.ExpiresAt = &recorded

	// Raising the policy default must not extend the issued lease.
	p.Spec.Lease.DefaultDuration = dur(29 * time.Minute)
	again := Evaluate(p, l, placed, created.Add(20*time.Minute))
	if !again.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("expiry moved from %s to %s after the policy default changed", first.ExpiresAt, again.ExpiresAt)
	}
	if !again.Expired || again.Active() {
		t.Fatalf("the lease must be expired at +20m under its original 15m duration: %+v", again)
	}

	// Lowering the maximum below the pinned duration denies it (narrowing only).
	p.Spec.Lease.MaxDuration = dur(10 * time.Minute)
	p.Spec.Lease.DefaultDuration = dur(5 * time.Minute)
	if d := Evaluate(p, l, placed, created); !d.Denied || d.Reason != fpv1.ReasonDurationExceedsMax {
		t.Fatalf("a pinned duration above a lowered maximum must be denied, got %+v", d)
	}
}

func TestEvaluateForgedStatusCannotExceedMaximum(t *testing.T) {
	l := lease()
	forged := metav1.NewTime(created.Add(48 * time.Hour))
	l.Status.ExpiresAt = &forged
	if d := Evaluate(policy(), l, placed, created); !d.Denied || d.Reason != fpv1.ReasonDurationExceedsMax {
		t.Fatalf("a recorded expiry beyond the policy maximum must be denied, got %+v", d)
	}
}

// TestDefaultDurationFollowsALowerMaximum checks a policy that sets only a
// maximum below the 15-minute default: a lease without a duration gets the
// maximum instead of being denied for exceeding it.
func TestDefaultDurationFollowsALowerMaximum(t *testing.T) {
	p := policy()
	p.Spec.Lease = fpv1.LeaseSettings{MaxDuration: dur(10 * time.Minute)}
	d := Evaluate(p, lease(), placed, created.Add(time.Minute))
	if d.Denied || !d.Active() {
		t.Fatalf("expected an active lease, got %+v", d)
	}
	if want := created.Add(10 * time.Minute); !d.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %s, want %s (the policy maximum)", d.ExpiresAt, want)
	}
}
