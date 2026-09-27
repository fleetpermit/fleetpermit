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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGoTestSummaryCountsPackageFailures checks that a package that fails
// without a failing test, or never reports a result, counts as a failure.
func TestGoTestSummaryCountsPackageFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unit.jsonl")
	writeFile(t, path, strings.Join([]string{
		`{"Action":"start","Package":"example.com/ok"}`,
		`{"Action":"run","Package":"example.com/ok","Test":"TestA"}`,
		`{"Action":"pass","Package":"example.com/ok","Test":"TestA"}`,
		`{"Action":"pass","Package":"example.com/ok"}`,
		// A failing test: the package failure is that test.
		`{"Action":"fail","Package":"example.com/failing","Test":"TestB"}`,
		`{"Action":"fail","Package":"example.com/failing"}`,
		// A panic outside a test: only the package fails.
		`{"Action":"output","Package":"example.com/panics","Output":"panic: boom\n"}`,
		`{"Action":"fail","Package":"example.com/panics"}`,
		// A build failure, reported as output.
		`{"ImportPath":"example.com/broken [example.com/broken.test]","Action":"build-fail"}`,
		`{"Action":"output","Package":"example.com/broken","Output":"FAIL\texample.com/broken [build failed]\n"}`,
		`{"Action":"skip","Package":"example.com/broken"}`,
		// A run cut short.
		`{"Action":"run","Package":"example.com/cut","Test":"TestC"}`,
	}, "\n")+"\n")
	s, ok, err := goTestSummary(path)
	if err != nil || !ok {
		t.Fatalf("summary: ok=%v err=%v", ok, err)
	}
	want := []string{"example.com/broken", "example.com/cut (did not finish)", "example.com/failing", "example.com/panics"}
	if s.Passed != 1 || s.Failed != 4 || !slices.Equal(s.FailedPackages, want) {
		t.Fatalf("got passed=%d failed=%d packages=%v; want passed=1 failed=4 packages=%v", s.Passed, s.Failed, s.FailedPackages, want)
	}
}

func TestGoTestSummaryRejectsMalformedInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unit.jsonl")
	writeFile(t, path, `{"Action":"pass","Package":"example.com/ok","Test":"TestA"}`+"\n"+`{"Action":"pa`)
	if _, _, err := goTestSummary(path); err == nil {
		t.Fatal("a truncated event must be an error")
	}
}

// TestCarriedForwardResultsAreMarked checks that test results and coverage
// this run did not produce are carried forward with the date they were
// produced, in results.json and in the generated text, and that the date
// survives another run.
func TestCarriedForwardResultsAreMarked(t *testing.T) {
	root := t.TempDir()
	res, work := filepath.Join(root, "test-results"), filepath.Join(root, ".work", "test")
	writeFile(t, filepath.Join(res, "tests.json"), `{"unit":{"passed":3,"failed":0,"skipped":0}}`)
	writeFile(t, filepath.Join(res, "results.json"),
		`{"generatedAt":"2026-09-20T10:00:00Z","coverage":{"internalPackages":"80.0%","scope":"statements"}}`)
	out := map[string]any{}
	if err := addTestResults(out, work, res); err != nil {
		t.Fatal(err)
	}
	c, _ := out["carriedForward"].(map[string]string)
	if c["tests"] != "2026-09-20T10:00:00Z" || c["coverage"] != "2026-09-20T10:00:00Z" {
		t.Fatalf("carried forward %v", out["carriedForward"])
	}
	if md := markdown(out); !strings.Contains(md, "carried forward from 2026-09-20T10:00:00Z, not rerun") {
		t.Fatalf("the text must say the results were not rerun:\n%s", md)
	}

	// A later run that again produces nothing keeps the original date.
	writeJSON(filepath.Join(res, "results.json"), map[string]any{"generatedAt": "2026-09-25T10:00:00Z",
		"coverage": out["coverage"], "carriedForward": out["carriedForward"]})
	again := map[string]any{}
	if err := addTestResults(again, work, res); err != nil {
		t.Fatal(err)
	}
	if c, _ := again["carriedForward"].(map[string]string); c["tests"] != "2026-09-20T10:00:00Z" {
		t.Fatalf("the carried-forward date moved: %v", again["carriedForward"])
	}

	// Results produced by this run are not marked.
	writeFile(t, filepath.Join(work, "unit.jsonl"), `{"Action":"pass","Package":"example.com/ok"}`+"\n")
	fresh := map[string]any{}
	if err := addTestResults(fresh, work, res); err != nil {
		t.Fatal(err)
	}
	if c, _ := fresh["carriedForward"].(map[string]string); c["tests"] != "" {
		t.Fatalf("fresh test results must not be marked carried forward: %v", c)
	}
}
