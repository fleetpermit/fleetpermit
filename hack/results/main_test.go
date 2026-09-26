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

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReadProfileSkipsMalformedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cover.out")
	profile := "mode: set\nno-space-here\ngithub.com/fleetpermit/fleetpermit/internal/lease/evaluate.go:1.1,2.2 3 1\n"
	if err := os.WriteFile(path, []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	blocks := map[string]block{}
	if !readProfile(path, blocks) {
		t.Fatal("profile not read")
	}
	if got := percent(blocks); got != "100.0%" {
		t.Fatalf("coverage %q, want 100.0%% from the one valid line", got)
	}
}

func TestReadmeWithoutFinishedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "README.md")
	if err := os.WriteFile(path, []byte("intro\n<!-- results:start -->\n<!-- results:end -->\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := map[string]any{"e2e": map[string]any{"summary": map[string]any{}, "environment": map[string]any{}}}
	if err := updateReadme(path, out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "Real multi-cluster run") {
		t.Fatalf("results block not written:\n%s", b)
	}
}

func TestLatenciesExcludeTimedOutSamples(t *testing.T) {
	e2e := map[string]any{"scenarios": []any{
		map[string]any{"id": "S1", "metrics": map[string]any{"activationToAllowMsEast": 800.0, "activationToAllowMsWest": -1.0}},
		map[string]any{"id": "S5", "metrics": map[string]any{"expiryToDenyMs": -1.0}},
	}}
	bench := map[string]any{"samples": map[string]any{"activationToAllowMs": []any{900.0, -1.0}}}
	lat := latencies(e2e, bench)
	a := lat["activationToAllowMs"]
	if a == nil || !slices.Equal(a.Samples, []int64{800, 900}) || a.Min != 800 {
		t.Fatalf("timed-out samples (-1) must not be used: %+v", a)
	}
	if _, ok := lat["expiryToDenyMs"]; ok {
		t.Fatal("a measurement with only timed-out samples must not be reported")
	}
}
