//go:build integration

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

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
	"github.com/fleetpermit/fleetpermit/internal/metrics"
)

type scaleRow struct {
	Clusters            int     `json:"clusters"`
	ActivationMs        int64   `json:"activationMs"`
	ReadyMs             int64   `json:"readyMs"`
	RevocationMs        int64   `json:"revocationMs"`
	Reconciles          int     `json:"reconciles"`
	ReconcilesPerSecond float64 `json:"reconcilesPerSecond"`
}

// TestScaleSimulation measures controller behaviour for 10, 25, 50 and 100
// logical clusters. It uses a real kube-apiserver and etcd (envtest) but NO
// real managed clusters: the OCM work agent is simulated. Results are written
// to test-results/scale-simulation.json and must never be presented as
// multi-cluster measurements.
func TestScaleSimulation(t *testing.T) {
	if os.Getenv("FP_SCALE") == "" {
		t.Skip("set FP_SCALE=1 (make benchmark) to run the scale simulation")
	}
	ctx := context.Background()
	stop := startController(t, time.Now)
	defer stop()
	pollInterval = 10 * time.Millisecond
	defer func() { pollInterval = 100 * time.Millisecond }()

	var rows []scaleRow
	for _, n := range []int{10, 25, 50, 100} {
		ns := fmt.Sprintf("scale-%d", n)
		var clusters []string
		for i := 0; i < n; i++ {
			clusters = append(clusters, fmt.Sprintf("s%d-c%03d", n, i))
		}
		f := newFleet(t, ns, clusters, clusters)
		p := f.policy("sre-remediation")
		if err := k8s.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		eventually(t, 60*time.Second, "policy observed", func() error {
			var got fpv1.FleetAccessPolicy
			if err := k8s.Get(ctx, clientKey(p), &got); err != nil {
				return err
			}
			if got.Status.SelectedClusters != int32(n) {
				return fmt.Errorf("selected %d", got.Status.SelectedClusters)
			}
			return nil
		})

		before := testutil.ToFloat64(metrics.ReconcileTotal.WithLabelValues("success"))
		start := time.Now()
		l := f.lease("scale", p.Name, 10*time.Minute, "restart_workload")
		if err := k8s.Create(ctx, l); err != nil {
			t.Fatal(err)
		}
		eventually(t, 120*time.Second, "the grant on every cluster", func() error {
			if got := len(grantWorks(t, p)); got != n {
				return fmt.Errorf("%d/%d clusters", got, n)
			}
			return nil
		})
		activation := time.Since(start)
		eventually(t, 120*time.Second, "lease Ready", func() error {
			ackWorks(t)
			if got := getLease(t, l); got.Status.Phase != fpv1.LeaseActive || int(got.Status.ClusterCount) != n {
				return fmt.Errorf("phase %s clusters %d", got.Status.Phase, got.Status.ClusterCount)
			}
			for _, c := range getLease(t, l).Status.Conditions {
				if c.Type == fpv1.ConditionReady && c.Status == "True" {
					return nil
				}
			}
			return fmt.Errorf("not ready")
		})
		ready := time.Since(start)

		revokeStart := time.Now()
		if err := k8s.Delete(ctx, l); err != nil {
			t.Fatal(err)
		}
		eventually(t, 120*time.Second, "grants withdrawn", func() error {
			if got := len(grantWorks(t, p)); got != 0 {
				return fmt.Errorf("%d clusters still hold the grant", got)
			}
			return nil
		})
		revocation := time.Since(revokeStart)
		elapsed := time.Since(start)
		reconciles := int(testutil.ToFloat64(metrics.ReconcileTotal.WithLabelValues("success")) - before)
		row := scaleRow{
			Clusters: n, ActivationMs: activation.Milliseconds(), ReadyMs: ready.Milliseconds(),
			RevocationMs: revocation.Milliseconds(), Reconciles: reconciles,
			ReconcilesPerSecond: float64(reconciles) / elapsed.Seconds(),
		}
		t.Logf("%+v", row)
		rows = append(rows, row)
	}

	out := map[string]any{
		"kind":        "simulated-controller-scale",
		"generatedAt": time.Now().UTC().Format(time.RFC3339),
		"environment": map[string]any{
			"description": "envtest kube-apiserver + etcd on one host; simulated OCM work agent; no real managed clusters",
			"goos":        runtime.GOOS, "goarch": runtime.GOARCH, "cpus": runtime.NumCPU(),
			"pollIntervalMs": pollInterval.Milliseconds(),
			"note":           "activation = lease created until every cluster's ManifestWork carries the grant; ready = until the lease reports Ready after simulated acknowledgement; revocation = lease deleted until no ManifestWork carries it",
		},
		"rows": rows,
	}
	dir := os.Getenv("FP_RESULTS_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", "test-results")
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "scale-simulation.json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
