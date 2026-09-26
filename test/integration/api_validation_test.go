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
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
)

const (
	sreID      = "spiffe://cluster.local/ns/agents/sa/sre-agent"
	securityID = "spiffe://cluster.local/ns/agents/sa/security-agent"
)

func validPolicy(ns, name string) *fpv1.FleetAccessPolicy {
	return &fpv1.FleetAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: fpv1.FleetAccessPolicySpec{
			Subjects:    []fpv1.Subject{{SPIFFEID: sreID}},
			Placement:   fpv1.PlacementSpec{PlacementRef: fpv1.LocalObjectReference{Name: "production-clusters"}},
			Target:      fpv1.TargetSpec{Namespace: "mcp-tools", Ref: fpv1.TargetRef{Name: "fleet-tools"}},
			Permissions: []fpv1.Permission{{Tool: "get_cluster_health"}, {Tool: "restart_workload"}},
			Lease: fpv1.LeaseSettings{
				DefaultDuration: &metav1.Duration{Duration: 5 * time.Minute},
				MaxDuration:     &metav1.Duration{Duration: 10 * time.Minute},
			},
		},
	}
}

func TestAPIDefaults(t *testing.T) {
	createNamespace(t, "api-defaults")
	p := &fpv1.FleetAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "api-defaults", Name: "minimal"},
		Spec: fpv1.FleetAccessPolicySpec{
			Subjects:    []fpv1.Subject{{SPIFFEID: sreID}},
			Placement:   fpv1.PlacementSpec{PlacementRef: fpv1.LocalObjectReference{Name: "p"}},
			Target:      fpv1.TargetSpec{Namespace: "mcp-tools", Ref: fpv1.TargetRef{Name: "fleet-tools"}},
			Permissions: []fpv1.Permission{{Tool: "get_cluster_health"}},
		},
	}
	if err := k8s.Create(context.Background(), p); err != nil {
		t.Fatalf("minimal policy rejected: %v", err)
	}
	s := p.Spec
	if s.Placement.Provider != "ocm" || s.Target.Protocol != "MCP" || s.Target.Ref.Kind != "XBackend" ||
		s.Target.Ref.Group != "agentic.networking.x-k8s.io" || s.Enforcement.Provider != "kubernetes-agentic-networking" ||
		s.Enforcement.FailMode != "Closed" || !p.LeaseRequired() ||
		s.Lease.DefaultDuration == nil || s.Lease.DefaultDuration.Duration != 15*time.Minute ||
		s.Lease.MaxDuration == nil || s.Lease.MaxDuration.Duration != time.Hour {
		t.Fatalf("unexpected defaults: %+v", s)
	}
}

func TestAPIRejectsMalformedPolicies(t *testing.T) {
	createNamespace(t, "api-policy")
	cases := []struct {
		name   string
		mutate func(*fpv1.FleetAccessPolicy)
		want   string
	}{
		{"no subjects", func(p *fpv1.FleetAccessPolicy) { p.Spec.Subjects = nil }, "subjects"},
		{"not a SPIFFE ID", func(p *fpv1.FleetAccessPolicy) { p.Spec.Subjects[0].SPIFFEID = "https://evil.example/agent" }, "spiffeID"},
		{"uppercase trust domain", func(p *fpv1.FleetAccessPolicy) { p.Spec.Subjects[0].SPIFFEID = "spiffe://Fleet/agent" }, "spiffeID"},
		{"duplicate subject", func(p *fpv1.FleetAccessPolicy) { p.Spec.Subjects = append(p.Spec.Subjects, p.Spec.Subjects[0]) }, "Duplicate"},
		{"no permissions", func(p *fpv1.FleetAccessPolicy) { p.Spec.Permissions = nil }, "permissions"},
		{"CEL injection in tool", func(p *fpv1.FleetAccessPolicy) { p.Spec.Permissions[0].Tool = "x') || true || ('" }, "tool"},
		{"tool with space", func(p *fpv1.FleetAccessPolicy) { p.Spec.Permissions[0].Tool = "read secret" }, "tool"},
		{"tool too long", func(p *fpv1.FleetAccessPolicy) { p.Spec.Permissions[0].Tool = strings.Repeat("a", 21) }, "tool"},
		{"missing placement", func(p *fpv1.FleetAccessPolicy) { p.Spec.Placement.PlacementRef.Name = "" }, "placementRef"},
		{"unknown placement provider", func(p *fpv1.FleetAccessPolicy) { p.Spec.Placement.Provider = "other" }, "provider"},
		{"fail open", func(p *fpv1.FleetAccessPolicy) { p.Spec.Enforcement.FailMode = "Open" }, "failMode"},
		{"unknown target kind", func(p *fpv1.FleetAccessPolicy) { p.Spec.Target.Ref.Kind = "Service" }, "kind"},
		{"bad target namespace", func(p *fpv1.FleetAccessPolicy) { p.Spec.Target.Namespace = "Not_A_Namespace" }, "namespace"},
		{"default above max", func(p *fpv1.FleetAccessPolicy) { p.Spec.Lease.DefaultDuration.Duration = time.Hour }, "defaultDuration must not exceed maxDuration"},
		{"max above 24h", func(p *fpv1.FleetAccessPolicy) {
			p.Spec.Lease.MaxDuration.Duration = 25 * time.Hour
			p.Spec.Lease.DefaultDuration.Duration = time.Hour
		}, "maxDuration must not exceed 24h"},
		{"default below 10s", func(p *fpv1.FleetAccessPolicy) { p.Spec.Lease.DefaultDuration.Duration = time.Second }, "at least 10s"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validPolicy("api-policy", fmt.Sprintf("bad-%d", i))
			tc.mutate(p)
			err := k8s.Create(context.Background(), p)
			if err == nil {
				t.Fatalf("expected rejection")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestAPILeaseValidationAndImmutability(t *testing.T) {
	createNamespace(t, "api-lease")
	ctx := context.Background()
	base := func(name string) *fpv1.ToolAccessLease {
		d := metav1.Duration{Duration: time.Minute}
		return &fpv1.ToolAccessLease{
			ObjectMeta: metav1.ObjectMeta{Namespace: "api-lease", Name: name},
			Spec: fpv1.ToolAccessLeaseSpec{
				PolicyRef:   fpv1.LocalObjectReference{Name: "p"},
				Subject:     fpv1.Subject{SPIFFEID: sreID},
				Permissions: []fpv1.Permission{{Tool: "restart_workload"}},
				Duration:    &d,
			},
		}
	}
	bad := base("bad-tool")
	bad.Spec.Permissions[0].Tool = "a'b"
	if err := k8s.Create(ctx, bad); err == nil {
		t.Fatal("lease with an invalid tool name must be rejected")
	}
	empty := base("no-permissions")
	empty.Spec.Permissions = nil
	if err := k8s.Create(ctx, empty); err == nil {
		t.Fatal("lease without permissions must be rejected")
	}

	l := base("immutable")
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	l.Spec.Permissions = append(l.Spec.Permissions, fpv1.Permission{Tool: "read_secret"})
	if err := k8s.Update(ctx, l); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("widening a lease must be rejected as immutable, got %v", err)
	}
	if err := k8s.Get(ctx, clientKey(l), l); err != nil {
		t.Fatal(err)
	}
	d := metav1.Duration{Duration: 24 * time.Hour}
	l.Spec.Duration = &d
	if err := k8s.Update(ctx, l); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("extending a lease must be rejected as immutable, got %v", err)
	}
}

// TestSamplesAreValid creates every manifest in config/samples against the
// real API server with the FleetPermit and OCM CRDs installed.
func TestSamplesAreValid(t *testing.T) {
	createNamespace(t, "fleet")
	files, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no samples found: %v", err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			u := &unstructured.Unstructured{}
			if err := yaml.Unmarshal(b, &u.Object); err != nil {
				t.Fatal(err)
			}
			if err := k8s.Create(context.Background(), u); err != nil {
				t.Fatalf("sample %s rejected: %v", filepath.Base(f), err)
			}
		})
	}
}
