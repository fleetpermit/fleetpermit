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
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
	"github.com/fleetpermit/fleetpermit/internal/enforcement"
	"github.com/fleetpermit/fleetpermit/internal/enforcement/agenticnetworking"
)

func clientKey(o client.Object) types.NamespacedName {
	return types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}
}

// TestRenderedPoliciesAreValidUpstreamObjects creates every shape of rendered
// XAccessPolicy against the pinned upstream CRD, so schema or CEL validation
// changes upstream are caught here rather than on a managed cluster.
func TestRenderedPoliciesAreValidUpstreamObjects(t *testing.T) {
	ctx := context.Background()
	createNamespace(t, "mcp-tools")
	expiry := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	other := "spiffe://cluster.local/ns/agents/sa/other"

	var many []enforcement.Grant
	for i := 0; i < 12; i++ {
		many = append(many, enforcement.Grant{ID: fmt.Sprintf("uid-%d", i), Lease: fmt.Sprintf("l%d", i), Subject: sreID, Tools: []string{"restart_workload"}, ExpiresAt: expiry})
	}
	cases := map[string]struct {
		target fpv1.TargetRef
		grants []enforcement.Grant
	}{
		"single lease": {fpv1.TargetRef{Name: "fleet-tools"}, []enforcement.Grant{
			{ID: "a", Lease: "a", Subject: sreID, Tools: []string{"restart_workload"}, ExpiresAt: expiry}}},
		"two subjects, several tools": {fpv1.TargetRef{Name: "fleet-tools"}, []enforcement.Grant{
			{ID: "a", Lease: "a", Subject: sreID, Tools: []string{"get_cluster_health", "restart_workload"}, ExpiresAt: expiry},
			{ID: "b", Lease: "b", Subject: other, Tools: []string{"get_cluster_health"}, ExpiresAt: expiry}}},
		"standing grant": {fpv1.TargetRef{Name: "fleet-tools"}, []enforcement.Grant{
			{ID: sreID, Subject: sreID, Tools: []string{"get_cluster_health"}}}},
		"gateway target": {fpv1.TargetRef{Group: "gateway.networking.k8s.io", Kind: "Gateway", Name: "agentic-gateway"}, []enforcement.Grant{
			{ID: "a", Lease: "a", Subject: sreID, Tools: []string{"restart_workload"}, ExpiresAt: expiry}}},
		"at rule capacity": {fpv1.TargetRef{Name: "fleet-tools"}, many},
		// A placed cluster without grants receives the inert policy.
		"no grants (inert policy)": {fpv1.TargetRef{Name: "fleet-tools"}, nil},
	}
	i := 0
	for name, tc := range cases {
		i++
		t.Run(name, func(t *testing.T) {
			p := &fpv1.FleetAccessPolicy{
				ObjectMeta: metav1.ObjectMeta{Namespace: "fleet", Name: fmt.Sprintf("schema-%d", i), UID: types.UID(fmt.Sprintf("uid-%d", i))},
				Spec:       fpv1.FleetAccessPolicySpec{Target: fpv1.TargetSpec{Namespace: "mcp-tools", Ref: tc.target}},
			}
			res, err := agenticnetworking.Renderer{}.Render(enforcement.Request{Policy: p, Cluster: "cluster-east", Grants: tc.grants})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Objects) != 1 {
				t.Fatalf("expected one object, got %d", len(res.Objects))
			}
			if err := k8s.Create(ctx, res.Objects[0]); err != nil {
				t.Fatalf("upstream XAccessPolicy CRD rejected the rendered object: %v", err)
			}
		})
	}
}

// TestDefaultDenyAnchorIsValid checks the shipped anchor manifest against the
// upstream schema.
func TestDefaultDenyAnchorIsValid(t *testing.T) {
	createNamespace(t, "mcp-tools")
	b, err := os.ReadFile(filepath.Join("..", "..", "config", "managed-cluster", "default-deny-anchor.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(b, &u.Object); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Create(context.Background(), u); err != nil {
		t.Fatalf("anchor rejected by the upstream CRD: %v", err)
	}
}
