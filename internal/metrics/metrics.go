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

// Package metrics defines FleetPermit's Prometheus metrics.
//
// Labels are limited to low-cardinality values. Identities, lease names and
// cluster names are never used as label values.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// ReconcileTotal counts policy reconciliations by result (success|error).
	ReconcileTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "fleetpermit_reconcile_total",
		Help: "Policy reconciliations, by result.",
	}, []string{"result"})

	// ReconcileErrors counts failed reconciliations.
	ReconcileErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "fleetpermit_reconcile_errors_total",
		Help: "Policy reconciliations that returned an error.",
	})

	// ActiveLeases is the number of leases with a grant rendered for at least
	// one cluster. It counts desired authority: delivery to a cluster may
	// still be in progress (the lease's Ready condition confirms it).
	ActiveLeases = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "fleetpermit_active_leases",
		Help: "Leases with a grant rendered for at least one cluster; delivery may still be in progress (see the lease's Ready condition).",
	})

	// ExpiredLeases counts leases observed transitioning to Expired.
	ExpiredLeases = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "fleetpermit_expired_leases_total",
		Help: "Leases that transitioned to Expired.",
	})

	// DeniedLeases counts leases observed transitioning to Denied, by reason.
	DeniedLeases = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "fleetpermit_denied_leases_total",
		Help: "Leases that transitioned to Denied, by reason.",
	}, []string{"reason"})

	// AuthorizedClusters is the number of (policy, cluster) pairs with at
	// least one grant rendered. It counts desired authority: delivery to the
	// cluster may still be in progress (the cluster's Ready status confirms
	// it).
	AuthorizedClusters = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "fleetpermit_authorized_clusters",
		Help: "Policy/cluster pairs with at least one grant rendered; delivery may still be in progress (see the policy's per-cluster Ready status).",
	})

	// PolicyPropagation observes the time from lease creation to its first Ready
	// on every target cluster.
	PolicyPropagation = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "fleetpermit_policy_propagation_seconds",
		Help:    "Time from ToolAccessLease creation until it is first Ready on every target cluster (once per lease per controller process).",
		Buckets: []float64{0.5, 1, 2, 3, 5, 8, 13, 21, 34, 55, 89},
	})

	// LeaseRevocation observes the time from lease expiry or denial until the
	// grant is withdrawn from every cluster (the data plane stops honouring an
	// expired grant at expiry; this measures cleanup).
	LeaseRevocation = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "fleetpermit_lease_revocation_seconds",
		Help:    "Time from lease expiry or denial until its grants are removed from every cluster.",
		Buckets: []float64{0.5, 1, 2, 3, 5, 8, 13, 21, 34, 55, 89},
	})

	// PlacementChanges counts observed changes in a policy's selected clusters.
	PlacementChanges = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "fleetpermit_placement_changes_total",
		Help: "Observed changes to the set of clusters selected for a policy.",
	})
)

// DenialReasons are the reasons a lease can be denied; each is exported as a
// zero-valued series from startup so dashboards and alerts never see a gap.
var DenialReasons = []string{"PolicyNotFound", "SubjectNotAllowed", "PermissionNotAllowed", "DurationExceedsMaximum"}

func init() {
	ctrlmetrics.Registry.MustRegister(
		ReconcileTotal, ReconcileErrors, ActiveLeases, ExpiredLeases, DeniedLeases,
		AuthorizedClusters, PolicyPropagation, LeaseRevocation, PlacementChanges,
	)
	for _, r := range []string{"success", "error"} {
		ReconcileTotal.WithLabelValues(r)
	}
	for _, r := range DenialReasons {
		DeniedLeases.WithLabelValues(r)
	}
}
