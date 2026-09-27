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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	removeAfter(t, p)
	// The target's group and the default lease duration are derived rather
	// than stored: the group from the kind, the duration from the maximum.
	s := p.Spec
	if s.Placement.Provider != "ocm" || s.Target.Protocol != "MCP" || s.Target.Ref.Kind != "XBackend" ||
		s.Target.Ref.Group != "" || s.Target.Ref.ResolvedGroup() != "agentic.networking.x-k8s.io" ||
		s.Enforcement.Provider != "kubernetes-agentic-networking" || s.Enforcement.FailMode != "Closed" || !p.LeaseRequired() ||
		s.Lease.DefaultDuration != nil || p.DefaultDuration() != 15*time.Minute ||
		s.Lease.MaxDuration == nil || s.Lease.MaxDuration.Duration != time.Hour {
		t.Fatalf("unexpected defaults: %+v", s)
	}

	// A policy applied without a lease block: the stored defaults must not
	// pin a default duration, so a later, shorter maximum is accepted.
	applied := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "fleetpermit.github.io/v1alpha1", "kind": "FleetAccessPolicy",
		"metadata": map[string]any{"namespace": "api-defaults", "name": "no-lease-block"},
		"spec": map[string]any{
			"subjects":    []any{map[string]any{"spiffeID": sreID}},
			"placement":   map[string]any{"placementRef": map[string]any{"name": "p"}},
			"target":      map[string]any{"namespace": "mcp-tools", "ref": map[string]any{"name": "fleet-tools"}},
			"permissions": []any{map[string]any{"tool": "get_cluster_health"}},
		},
	}}
	ctx := context.Background()
	if err := k8s.Create(ctx, applied); err != nil {
		t.Fatalf("policy without a lease block rejected: %v", err)
	}
	removeAfter(t, applied)
	lease, _, _ := unstructured.NestedMap(applied.Object, "spec", "lease")
	if lease["required"] != true || lease["maxDuration"] != "1h" || lease["defaultDuration"] != nil {
		t.Fatalf("unexpected lease defaults %v", lease)
	}
	patch := client.RawPatch(types.MergePatchType, []byte(`{"spec":{"lease":{"maxDuration":"10m"}}}`))
	if err := k8s.Patch(ctx, applied, patch); err != nil {
		t.Fatalf("lowering maxDuration below 15m was rejected: %v", err)
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
		{"Gateway kind in the agentic networking group", func(p *fpv1.FleetAccessPolicy) {
			p.Spec.Target.Ref.Group, p.Spec.Target.Ref.Kind = "agentic.networking.x-k8s.io", "Gateway"
		}, "target.ref must be"},
		{"XBackend kind in the Gateway API group", func(p *fpv1.FleetAccessPolicy) { p.Spec.Target.Ref.Group = "gateway.networking.k8s.io" }, "target.ref must be"},
		{"standing policy with six subjects", func(p *fpv1.FleetAccessPolicy) {
			no := false
			p.Spec.Lease.Required = &no
			p.Spec.Subjects = subjects(6)
		}, "at most 5 subjects"},
		{"bad target namespace", func(p *fpv1.FleetAccessPolicy) { p.Spec.Target.Namespace = "Not_A_Namespace" }, "namespace"},
		{"default above max", func(p *fpv1.FleetAccessPolicy) { p.Spec.Lease.DefaultDuration.Duration = time.Hour }, "defaultDuration must not exceed maxDuration"},
		{"max above 24h", func(p *fpv1.FleetAccessPolicy) {
			p.Spec.Lease.MaxDuration.Duration = 25 * time.Hour
			p.Spec.Lease.DefaultDuration.Duration = time.Hour
		}, "maxDuration must not exceed 24h"},
		{"default below 10s", func(p *fpv1.FleetAccessPolicy) { p.Spec.Lease.DefaultDuration.Duration = time.Second }, "at least 10s"},
		{"max below 10s", func(p *fpv1.FleetAccessPolicy) {
			p.Spec.Lease = fpv1.LeaseSettings{MaxDuration: &metav1.Duration{Duration: 5 * time.Second}}
		}, "maxDuration must be at least 10s"},
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

func subjects(n int) []fpv1.Subject {
	out := make([]fpv1.Subject, n)
	for i := range out {
		out[i].SPIFFEID = fmt.Sprintf("spiffe://cluster.local/ns/agents/sa/agent-%d", i)
	}
	return out
}

// TestAPIAcceptsValidTargetsAndSubjectCounts checks the accepting side of the
// target and standing-subject rules, including objects that rely on defaults.
func TestAPIAcceptsValidTargetsAndSubjectCounts(t *testing.T) {
	createNamespace(t, "api-accept")
	no := false
	cases := []struct {
		name   string
		mutate func(*fpv1.FleetAccessPolicy)
	}{
		{"XBackend target with defaults", func(p *fpv1.FleetAccessPolicy) { p.Spec.Target.Ref = fpv1.TargetRef{Name: "fleet-tools"} }},
		{"Gateway target", func(p *fpv1.FleetAccessPolicy) {
			p.Spec.Target.Ref = fpv1.TargetRef{Group: "gateway.networking.k8s.io", Kind: "Gateway", Name: "agentic-gateway"}
		}},
		{"Gateway target without a group", func(p *fpv1.FleetAccessPolicy) {
			p.Spec.Target.Ref = fpv1.TargetRef{Kind: "Gateway", Name: "agentic-gateway"}
		}},
		{"maximum duration below the default duration's default", func(p *fpv1.FleetAccessPolicy) {
			p.Spec.Lease = fpv1.LeaseSettings{MaxDuration: &metav1.Duration{Duration: 10 * time.Minute}}
		}},
		{"standing policy with five subjects", func(p *fpv1.FleetAccessPolicy) {
			p.Spec.Lease.Required = &no
			p.Spec.Subjects = subjects(5)
		}},
		{"lease policy with sixteen subjects", func(p *fpv1.FleetAccessPolicy) { p.Spec.Subjects = subjects(16) }},
		{"defaulted lease settings with six subjects", func(p *fpv1.FleetAccessPolicy) {
			p.Spec.Lease = fpv1.LeaseSettings{}
			p.Spec.Subjects = subjects(6)
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validPolicy("api-accept", fmt.Sprintf("ok-%d", i))
			tc.mutate(p)
			if err := k8s.Create(context.Background(), p); err != nil {
				t.Fatalf("valid policy rejected: %v", err)
			}
			removeAfter(t, p)
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

	short := base("too-short")
	short.Spec.Duration = &metav1.Duration{Duration: 5 * time.Second}
	if err := k8s.Create(ctx, short); err == nil || !strings.Contains(err.Error(), "at least 10s") {
		t.Fatalf("a 5s lease must be rejected at admission, got %v", err)
	}

	l := base("immutable")
	if err := k8s.Create(ctx, l); err != nil {
		t.Fatal(err)
	}
	removeAfter(t, l)
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

	// A Go client writes a duration applied as "30m" back as "30m0s". The
	// spec is unchanged, so the update must be accepted.
	applied := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "fleetpermit.github.io/v1alpha1", "kind": "ToolAccessLease",
		"metadata": map[string]any{"namespace": "api-lease", "name": "applied-as-yaml"},
		"spec": map[string]any{
			"policyRef": map[string]any{"name": "p"}, "subject": map[string]any{"spiffeID": sreID},
			"permissions": []any{map[string]any{"tool": "restart_workload"}}, "duration": "30m",
			"clusters": []any{"cluster-east"}, "reason": "INC-7",
		},
	}}
	if err := k8s.Create(ctx, applied); err != nil {
		t.Fatal(err)
	}
	removeAfter(t, applied)
	var typed fpv1.ToolAccessLease
	if err := k8s.Get(ctx, clientKey(applied), &typed); err != nil {
		t.Fatal(err)
	}
	typed.Labels = map[string]string{"team": "sre"}
	if err := k8s.Update(ctx, &typed); err != nil {
		t.Fatalf("an update that leaves the spec unchanged was rejected: %v", err)
	}
	for name, change := range map[string]func(*fpv1.ToolAccessLeaseSpec){
		"duration":         func(s *fpv1.ToolAccessLeaseSpec) { s.Duration = &metav1.Duration{Duration: 31 * time.Minute} },
		"removed duration": func(s *fpv1.ToolAccessLeaseSpec) { s.Duration = nil },
		"clusters":         func(s *fpv1.ToolAccessLeaseSpec) { s.Clusters = []string{"cluster-west"} },
		"removed clusters": func(s *fpv1.ToolAccessLeaseSpec) { s.Clusters = nil },
		"reason":           func(s *fpv1.ToolAccessLeaseSpec) { s.Reason = "INC-8" },
		"subject":          func(s *fpv1.ToolAccessLeaseSpec) { s.Subject.SPIFFEID = securityID },
		"policy":           func(s *fpv1.ToolAccessLeaseSpec) { s.PolicyRef.Name = "q" },
	} {
		var cur fpv1.ToolAccessLease
		if err := k8s.Get(ctx, clientKey(applied), &cur); err != nil {
			t.Fatal(err)
		}
		change(&cur.Spec)
		if err := k8s.Update(ctx, &cur); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Errorf("changing the %s must be rejected as immutable, got %v", name, err)
		}
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
			removeAfter(t, u)
		})
	}
}
