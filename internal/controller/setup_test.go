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
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	workv1 "open-cluster-management.io/api/work/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
	"github.com/fleetpermit/fleetpermit/internal/placement/ocm"
)

func managedCluster(available metav1.ConditionStatus, heartbeat time.Time) *clusterv1.ManagedCluster {
	return &clusterv1.ManagedCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-east"},
		Status: clusterv1.ManagedClusterStatus{Conditions: []metav1.Condition{
			{Type: clusterv1.ManagedClusterConditionAvailable, Status: available, Reason: "ManagedClusterAvailable"},
			{Type: clusterv1.ManagedClusterConditionJoined, Status: metav1.ConditionTrue, Reason: "Joined", LastTransitionTime: metav1.NewTime(heartbeat)},
		}},
	}
}

// TestManagedClusterEventsAreFiltered checks that only ManagedCluster
// changes the controller reads reconcile every policy.
func TestManagedClusterEventsAreFiltered(t *testing.T) {
	t0 := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	up := managedCluster(metav1.ConditionTrue, t0)
	if !availabilityChanged.Create(event.CreateEvent{Object: up}) || !availabilityChanged.Delete(event.DeleteEvent{Object: up}) {
		t.Fatal("cluster creation and deletion must reconcile")
	}
	routine := managedCluster(metav1.ConditionTrue, t0.Add(time.Minute))
	routine.Status.Version.Kubernetes = "v1.35.0"
	if availabilityChanged.Update(event.UpdateEvent{ObjectOld: up, ObjectNew: routine}) {
		t.Fatal("a status update that keeps the cluster available must not reconcile every policy")
	}
	down := managedCluster(metav1.ConditionUnknown, t0)
	if !availabilityChanged.Update(event.UpdateEvent{ObjectOld: up, ObjectNew: down}) ||
		!availabilityChanged.Update(event.UpdateEvent{ObjectOld: down, ObjectNew: up}) {
		t.Fatal("a change of availability must reconcile")
	}
}

// TestMapFunctionsKeepToTheWatchNamespace checks that events are mapped only
// to policies in the watch namespace.
func TestMapFunctionsKeepToTheWatchNamespace(t *testing.T) {
	ctx := context.Background()
	r := &PolicyReconciler{WatchNamespace: "fleet"}
	work := func(policy string) *workv1.ManifestWork {
		return &workv1.ManifestWork{ObjectMeta: metav1.ObjectMeta{Namespace: "cluster-east", Name: "w",
			Annotations: map[string]string{ocm.AnnotationPolicy: policy}}}
	}
	if got := r.workToPolicy(ctx, work("elsewhere/sre")); len(got) != 0 {
		t.Fatalf("a work naming a policy outside the watch namespace was mapped: %v", got)
	}
	if got := r.workToPolicy(ctx, work("fleet/sre")); len(got) != 1 || got[0].Namespace != "fleet" || got[0].Name != "sre" {
		t.Fatalf("a work naming a watched policy was not mapped: %v", got)
	}
	l := &fpv1.ToolAccessLease{ObjectMeta: metav1.ObjectMeta{Namespace: "elsewhere", Name: "l"}}
	l.Spec.PolicyRef.Name = "sre"
	if got := r.leaseToPolicy(ctx, l); len(got) != 0 {
		t.Fatalf("a lease outside the watch namespace was mapped: %v", got)
	}
	if got := (&PolicyReconciler{}).workToPolicy(ctx, work("elsewhere/sre")); len(got) != 1 {
		t.Fatalf("without a watch namespace every policy is mapped, got %v", got)
	}
}
