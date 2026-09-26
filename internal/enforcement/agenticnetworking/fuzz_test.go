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
	"regexp"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/fleetpermit/fleetpermit/internal/enforcement"
)

// celStringLiteral matches one single-quoted CEL string literal without escapes.
var celStringLiteral = regexp.MustCompile(`^'[A-Za-z0-9_.-]*'$`)

// FuzzRenderNeverEmitsUnsafeCEL checks that, whatever tool names and subjects
// reach the renderer, it either rejects them or emits CEL in which every tool
// is a plain string literal: no input can change the meaning of a rule.
func FuzzRenderNeverEmitsUnsafeCEL(f *testing.F) {
	for _, seed := range []string{"restart_workload", "x') || true || ('", `a\b`, "", strings.Repeat("a", 30), "tool name", "ok.tool-1"} {
		f.Add(seed, "spiffe://cluster.local/ns/agents/sa/sre-agent")
	}
	f.Add("ok", "spiffe://Bad Domain/x")
	f.Fuzz(func(t *testing.T, tool, subject string) {
		g := enforcement.Grant{ID: "g", Lease: "l", Subject: subject, Tools: []string{tool}, ExpiresAt: time.Unix(1_800_000_000, 0)}
		res, err := Renderer{}.Render(enforcement.Request{Policy: testPolicy(), Cluster: "c", Grants: []enforcement.Grant{g}})
		if err != nil {
			return // rejected input is the safe outcome
		}
		if len(res.Objects) != 1 {
			t.Fatalf("expected one object, got %d", len(res.Objects))
		}
		ruleList, _, _ := unstructured.NestedSlice(res.Objects[0].Object, "spec", "rules")
		for _, r := range ruleList {
			expr, found, _ := unstructured.NestedString(r.(map[string]any), "authorization", "cel", "expression")
			if !found {
				continue
			}
			const prefix, sep = "request.mcp.tool_name in [", "] && request.time < timestamp('"
			if !strings.HasPrefix(expr, prefix) || !strings.Contains(expr, sep) {
				t.Fatalf("unexpected expression shape: %q", expr)
			}
			list := strings.TrimPrefix(expr[:strings.Index(expr, sep)], prefix)
			for _, lit := range strings.Split(list, ", ") {
				if !celStringLiteral.MatchString(lit) {
					t.Fatalf("tool %q produced a non-literal CEL fragment %q in %q", tool, lit, expr)
				}
			}
		}
	})
}
