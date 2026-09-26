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

package agenticnetworking

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
	"github.com/fleetpermit/fleetpermit/internal/enforcement"
)

const sre = "spiffe://cluster.local/ns/agents/sa/sre-agent"

var expiry = time.Date(2026, 9, 26, 10, 15, 0, 0, time.UTC)

func testPolicy() *fpv1.FleetAccessPolicy {
	return &fpv1.FleetAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "sre-remediation", Namespace: "fleet", UID: "policy-uid-1", Generation: 3},
		Spec: fpv1.FleetAccessPolicySpec{
			Target: fpv1.TargetSpec{Namespace: "mcp-tools", Ref: fpv1.TargetRef{Name: "fleet-tools"}},
		},
	}
}

func grant(id, subject string, tools ...string) enforcement.Grant {
	return enforcement.Grant{ID: id, Lease: "lease-" + id, Subject: subject, Tools: tools, ExpiresAt: expiry}
}

func rules(t *testing.T, u *unstructured.Unstructured) []any {
	t.Helper()
	r, found, err := unstructured.NestedSlice(u.Object, "spec", "rules")
	if err != nil || !found {
		t.Fatalf("spec.rules missing: %v", err)
	}
	return r
}

func TestRenderNoGrantsRendersInertPolicy(t *testing.T) {
	res, err := Renderer{}.Render(enforcement.Request{Policy: testPolicy(), Cluster: "cluster-east"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Objects) != 1 || res.Digest == "" || len(res.Rendered) != 0 {
		t.Fatalf("expected one inert object, got %d objects, rendered %v", len(res.Objects), res.Rendered)
	}
	r := rules(t, res.Objects[0])
	if len(r) != 1 {
		t.Fatalf("inert policy must have exactly one rule, got %d", len(r))
	}
	rule := r[0].(map[string]any)
	expr, _, _ := unstructured.NestedString(rule, "authorization", "cel", "expression")
	src, _, _ := unstructured.NestedString(rule, "source", "spiffe")
	if rule["name"] != InertRuleName || expr != "false" || !strings.HasPrefix(src, "spiffe://fleetpermit.invalid/") {
		t.Fatalf("inert rule must never match: %v", rule)
	}
	ann := res.Objects[0].GetAnnotations()
	if _, ok := ann[AnnotationLeases]; ok {
		t.Fatal("inert policy must not name leases")
	}
	if _, ok := ann[AnnotationExpiresAt]; ok {
		t.Fatal("inert policy must not carry an expiry")
	}
}

func TestRenderLeaseGrant(t *testing.T) {
	res, err := Renderer{}.Render(enforcement.Request{
		Policy: testPolicy(), Cluster: "cluster-east",
		Grants: []enforcement.Grant{grant("uid-a", sre, "restart_workload")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Objects) != 1 {
		t.Fatalf("expected one object, got %d", len(res.Objects))
	}
	u := res.Objects[0]
	if u.GetAPIVersion() != APIVersion || u.GetKind() != Kind {
		t.Fatalf("unexpected GVK %s %s", u.GetAPIVersion(), u.GetKind())
	}
	if u.GetNamespace() != "mcp-tools" {
		t.Fatalf("namespace = %q", u.GetNamespace())
	}
	ann := u.GetAnnotations()
	for _, k := range []string{AnnotationPolicy, AnnotationPolicyUID, AnnotationCluster, AnnotationLeases, AnnotationDigest, AnnotationExpiresAt} {
		if ann[k] == "" {
			t.Errorf("annotation %s missing", k)
		}
	}
	if ann[AnnotationDigest] != res.Digest || ann[AnnotationExpiresAt] != "2026-09-26T10:15:00Z" {
		t.Errorf("unexpected annotations %v", ann)
	}
	if u.GetLabels()[LabelManagedBy] != ManagedByValue {
		t.Errorf("managed-by label missing")
	}

	tr, _, _ := unstructured.NestedSlice(u.Object, "spec", "targetRefs")
	ref := tr[0].(map[string]any)
	if ref["group"] != Group || ref["kind"] != "XBackend" || ref["name"] != "fleet-tools" {
		t.Errorf("unexpected targetRef %v", ref)
	}
	if action, _, _ := unstructured.NestedString(u.Object, "spec", "action"); action != "Allow" {
		t.Errorf("action = %q", action)
	}

	r := rules(t, u)
	if len(r) != 2 {
		t.Fatalf("expected session + lease rule, got %d", len(r))
	}
	session := r[0].(map[string]any)
	if mcpOpt, _, _ := unstructured.NestedString(session, "authorization", "mcp", "mcpBaseProtocolMethodsOption"); mcpOpt != "MATCH_BASE_PROTOCOL_METHODS" {
		t.Errorf("session rule must match base protocol methods only, got %v", session)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(session, "authorization", "cel"); found {
		t.Errorf("session rule must not carry CEL")
	}
	leaseRule := r[1].(map[string]any)
	expr, _, _ := unstructured.NestedString(leaseRule, "authorization", "cel", "expression")
	want := "request.mcp.tool_name in ['restart_workload'] && request.time < timestamp('2026-09-26T10:15:00Z')"
	if expr != want {
		t.Errorf("expression = %q\nwant        %q", expr, want)
	}
	if spiffe, _, _ := unstructured.NestedString(leaseRule, "source", "spiffe"); spiffe != sre {
		t.Errorf("source = %q", spiffe)
	}
}

func TestRenderStandingGrantHasNoExpiry(t *testing.T) {
	g := enforcement.Grant{ID: sre, Subject: sre, Tools: []string{"get_cluster_health"}}
	res, err := Renderer{}.Render(enforcement.Request{Policy: testPolicy(), Cluster: "c", Grants: []enforcement.Grant{g}})
	if err != nil {
		t.Fatal(err)
	}
	r := rules(t, res.Objects[0])
	expr, _, _ := unstructured.NestedString(r[1].(map[string]any), "authorization", "cel", "expression")
	if strings.Contains(expr, "request.time") {
		t.Errorf("standing grant must not be time bounded: %q", expr)
	}
	name, _, _ := unstructured.NestedString(r[1].(map[string]any), "name")
	if !strings.HasPrefix(name, "standing-") {
		t.Errorf("rule name = %q", name)
	}
	if _, ok := res.Objects[0].GetAnnotations()[AnnotationExpiresAt]; ok {
		t.Errorf("standing-only content must not carry an expires-at annotation")
	}
}

func TestRenderOneSessionRulePerSubject(t *testing.T) {
	other := "spiffe://cluster.local/ns/agents/sa/other"
	res, err := Renderer{}.Render(enforcement.Request{Policy: testPolicy(), Cluster: "c", Grants: []enforcement.Grant{
		grant("a", sre, "restart_workload"),
		grant("b", sre, "get_cluster_health"),
		grant("c", other, "get_cluster_health"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(rules(t, res.Objects[0])); got != 5 {
		t.Fatalf("expected 2 session + 3 lease rules, got %d", got)
	}
}

func TestRenderCapacityIsDeterministic(t *testing.T) {
	var grants []enforcement.Grant
	for i := 0; i < 12; i++ {
		grants = append(grants, grant(fmt.Sprintf("g%02d", i), sre, "restart_workload"))
	}
	res, err := Renderer{}.Render(enforcement.Request{Policy: testPolicy(), Cluster: "c", Grants: grants})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(rules(t, res.Objects[0])); got != MaxRulesPerPolicy {
		t.Fatalf("rules = %d, want the upstream maximum %d", got, MaxRulesPerPolicy)
	}
	if len(res.Rendered) != 9 || len(res.Dropped) != 3 {
		t.Fatalf("rendered=%d dropped=%d, want 9 and 3", len(res.Rendered), len(res.Dropped))
	}
	if res.Dropped[0] != "g09" || res.Rendered[0] != "g00" {
		t.Fatalf("priority order not respected: rendered=%v dropped=%v", res.Rendered, res.Dropped)
	}
}

func TestRenderDigestIsStableAndBound(t *testing.T) {
	req := enforcement.Request{Policy: testPolicy(), Cluster: "cluster-east", Grants: []enforcement.Grant{grant("a", sre, "restart_workload")}}
	a, _ := Renderer{}.Render(req)
	b, _ := Renderer{}.Render(req)
	if a.Digest != b.Digest {
		t.Fatal("digest is not deterministic")
	}
	req.Cluster = "cluster-west"
	c, _ := Renderer{}.Render(req)
	if c.Digest == a.Digest {
		t.Fatal("digest must bind the target cluster")
	}
	req.Cluster = "cluster-east"
	req.Grants[0].ExpiresAt = expiry.Add(time.Minute)
	d, _ := Renderer{}.Render(req)
	if d.Digest == a.Digest {
		t.Fatal("digest must change when the expiry changes")
	}
	p := testPolicy()
	p.Generation = 99
	e, _ := Renderer{}.Render(enforcement.Request{Policy: p, Cluster: "cluster-east", Grants: []enforcement.Grant{grant("a", sre, "restart_workload")}})
	if e.Digest != a.Digest {
		t.Fatal("digest should depend on content, not on the policy generation")
	}
}

// TestRenderedObjectDoesNotDependOnPolicyGeneration checks that a policy edit
// that leaves a cluster's grants unchanged does not change what is delivered
// there, so it does not trigger a new rollout.
func TestRenderedObjectDoesNotDependOnPolicyGeneration(t *testing.T) {
	grants := []enforcement.Grant{grant("a", sre, "restart_workload")}
	a, err := Renderer{}.Render(enforcement.Request{Policy: testPolicy(), Cluster: "cluster-east", Grants: grants})
	if err != nil {
		t.Fatal(err)
	}
	p := testPolicy()
	p.Generation = 99
	b, err := Renderer{}.Render(enforcement.Request{Policy: p, Cluster: "cluster-east", Grants: grants})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.Objects, b.Objects) {
		t.Fatalf("rendered object changed with the policy generation:\n%v\n%v", a.Objects[0].GetAnnotations(), b.Objects[0].GetAnnotations())
	}
	if _, ok := b.Objects[0].GetAnnotations()["fleetpermit.github.io/policy-generation"]; ok {
		t.Fatal("the rendered object must not carry the policy generation")
	}
}

func TestRenderRejectsInjection(t *testing.T) {
	bad := []enforcement.Grant{
		grant("a", sre, "x') || true || ('"),
		grant("b", sre, "tool name"),
		grant("c", sre, `a\b`),
		grant("d", sre, strings.Repeat("a", 21)),
		grant("e", "spiffe://Bad Domain/x", "ok"),
		grant("f", "https://example.com", "ok"),
		{ID: "g", Subject: sre},
	}
	for _, g := range bad {
		if _, err := (Renderer{}).Render(enforcement.Request{Policy: testPolicy(), Cluster: "c", Grants: []enforcement.Grant{g}}); err == nil {
			t.Errorf("grant %s with tools=%v subject=%q must be rejected", g.ID, g.Tools, g.Subject)
		}
	}
}

func TestRenderGatewayTarget(t *testing.T) {
	p := testPolicy()
	p.Spec.Target.Ref = fpv1.TargetRef{Group: "gateway.networking.k8s.io", Kind: "Gateway", Name: "agentic-gw"}
	res, err := Renderer{}.Render(enforcement.Request{Policy: p, Cluster: "c", Grants: []enforcement.Grant{grant("a", sre, "t")}})
	if err != nil {
		t.Fatal(err)
	}
	tr, _, _ := unstructured.NestedSlice(res.Objects[0].Object, "spec", "targetRefs")
	ref := tr[0].(map[string]any)
	if ref["kind"] != "Gateway" || ref["group"] != "gateway.networking.k8s.io" {
		t.Fatalf("unexpected targetRef %v", ref)
	}
}

func TestObjectNameBounded(t *testing.T) {
	a := ObjectName("fleet", "sre-remediation")
	if !strings.HasPrefix(a, "fleetpermit-sre-remediation-") {
		t.Fatalf("unexpected name %q", a)
	}
	long := ObjectName("fleet", strings.Repeat("x", 250))
	if len(long) > 63 {
		t.Fatalf("name too long: %d", len(long))
	}
	if ObjectName("a", "p") == ObjectName("b", "p") {
		t.Fatal("names must differ across hub namespaces")
	}
}

func TestExpression(t *testing.T) {
	got := Expression([]string{"a", "b"}, time.Time{})
	if got != "request.mcp.tool_name in ['a', 'b']" {
		t.Fatalf("got %q", got)
	}
}
