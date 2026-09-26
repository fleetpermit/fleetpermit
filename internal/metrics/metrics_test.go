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

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

func TestMetricsRegisteredWithStableNames(t *testing.T) {
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range families {
		got[f.GetName()] = true
	}
	for _, name := range []string{
		"fleetpermit_reconcile_total", "fleetpermit_reconcile_errors_total", "fleetpermit_active_leases",
		"fleetpermit_expired_leases_total", "fleetpermit_denied_leases_total", "fleetpermit_authorized_clusters",
		"fleetpermit_policy_propagation_seconds", "fleetpermit_lease_revocation_seconds", "fleetpermit_placement_changes_total",
	} {
		if !got[name] {
			t.Errorf("metric %s is not exported", name)
		}
	}
}

func TestLabelledSeriesExistBeforeFirstObservation(t *testing.T) {
	if n := testutil.CollectAndCount(DeniedLeases); n != len(DenialReasons) {
		t.Fatalf("denied_leases_total has %d series, want %d zero-valued series", n, len(DenialReasons))
	}
	if n := testutil.CollectAndCount(ReconcileTotal); n != 2 {
		t.Fatalf("reconcile_total has %d series, want success and error", n)
	}
}

func TestNoHighCardinalityLabels(t *testing.T) {
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"result": true, "reason": true}
	for _, f := range families {
		if len(f.GetName()) < 12 || f.GetName()[:12] != "fleetpermit_" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if !allowed[l.GetName()] {
					t.Errorf("%s uses label %q; identities, leases and clusters must not be labels", f.GetName(), l.GetName())
				}
			}
		}
	}
}
