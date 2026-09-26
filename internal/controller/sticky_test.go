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

package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
	"github.com/fleetpermit/fleetpermit/internal/lease"
)

func leaseWith(conds ...metav1.Condition) *fpv1.ToolAccessLease {
	return &fpv1.ToolAccessLease{Status: fpv1.ToolAccessLeaseStatus{Conditions: conds}}
}

func TestStickyKeepsTheFirstTerminalState(t *testing.T) {
	expired := metav1.Condition{Type: fpv1.ConditionExpired, Status: metav1.ConditionTrue, Reason: fpv1.ReasonLeaseExpired, Message: "expired"}
	denied := metav1.Condition{Type: fpv1.ConditionDenied, Status: metav1.ConditionTrue, Reason: fpv1.ReasonSubjectNotAllowed, Message: "denied"}
	policyGone := lease.Decision{Denied: true, Reason: fpv1.ReasonPolicyNotFound}
	active := lease.Decision{Tools: []string{"restart_workload"}, Clusters: []string{"cluster-east"}}
	nowExpired := lease.Decision{Expired: true, Reason: fpv1.ReasonLeaseExpired}

	cases := []struct {
		name        string
		l           *fpv1.ToolAccessLease
		d           lease.Decision
		denied, exp bool
		reason      string
	}{
		{"expired lease whose policy was deleted stays Expired", leaseWith(expired), policyGone, false, true, fpv1.ReasonLeaseExpired},
		{"expired lease never re-activates", leaseWith(expired), active, false, true, fpv1.ReasonLeaseExpired},
		{"denied lease never re-activates", leaseWith(denied), active, true, false, fpv1.ReasonSubjectNotAllowed},
		{"denied lease that later passes its expiry stays only Denied", leaseWith(denied), nowExpired, true, false, fpv1.ReasonSubjectNotAllowed},
		{"no recorded terminal state keeps the decision", leaseWith(), policyGone, true, false, fpv1.ReasonPolicyNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sticky(tc.l, tc.d)
			if got.Denied != tc.denied || got.Expired != tc.exp || got.Reason != tc.reason {
				t.Fatalf("got denied=%v expired=%v reason=%s, want denied=%v expired=%v reason=%s",
					got.Denied, got.Expired, got.Reason, tc.denied, tc.exp, tc.reason)
			}
			if got.Active() {
				t.Fatal("a terminal lease must never be active")
			}
		})
	}
}
