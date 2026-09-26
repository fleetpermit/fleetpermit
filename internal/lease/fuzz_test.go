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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
)

// FuzzEvaluateNeverWidens checks the core invariant: whatever a lease
// requests, an active decision only contains tools the policy permits,
// clusters the placement selects, a subject the policy lists and an expiry
// no later than creation plus the policy maximum.
func FuzzEvaluateNeverWidens(f *testing.F) {
	f.Add("restart_workload,read_secret", "cluster-east,cluster-edge", int64(60), sre, int64(0))
	f.Add("get_cluster_health", "", int64(3600), security, int64(10))
	f.Add("", "cluster-west", int64(-5), sre, int64(1_000_000))
	f.Fuzz(func(t *testing.T, toolsCSV, clustersCSV string, durSeconds int64, subject string, elapsed int64) {
		p := policy()
		l := lease(func(l *fpv1.ToolAccessLease) {
			l.Spec.Subject.SPIFFEID = subject
			l.Spec.Permissions = nil
			for _, tool := range strings.Split(toolsCSV, ",") {
				l.Spec.Permissions = append(l.Spec.Permissions, fpv1.Permission{Tool: tool})
			}
			if clustersCSV != "" {
				l.Spec.Clusters = strings.Split(clustersCSV, ",")
			}
			l.Spec.Duration = &metav1.Duration{Duration: time.Duration(durSeconds) * time.Second}
		})
		d := Evaluate(p, l, placed, created.Add(time.Duration(elapsed)*time.Second))
		if !d.Active() {
			return
		}
		if subject != sre {
			t.Fatalf("subject %q activated", subject)
		}
		for _, tool := range d.Tools {
			if !slices.ContainsFunc(p.Spec.Permissions, func(pp fpv1.Permission) bool { return pp.Tool == tool }) {
				t.Fatalf("tool %q is not permitted by the policy", tool)
			}
		}
		for _, c := range d.Clusters {
			if !slices.Contains(placed, c) {
				t.Fatalf("cluster %q is outside the placement", c)
			}
		}
		if d.ExpiresAt.After(created.Add(p.MaxDuration())) {
			t.Fatalf("expiry %s exceeds the policy maximum", d.ExpiresAt)
		}
	})
}
