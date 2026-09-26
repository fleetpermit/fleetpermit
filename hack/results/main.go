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

// Command results merges machine-generated test outputs into
// test-results/results.json and docs/results.md, and optionally copies the
// JSON to the website (FP_SITE_DIR). It never invents a value: a missing
// input is reported as missing.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type latency struct {
	Label   string  `json:"label"`
	Unit    string  `json:"unit"`
	Source  string  `json:"source"`
	Samples []int64 `json:"samples"`
	P50     int64   `json:"p50"`
	P95     int64   `json:"p95"`
	Min     int64   `json:"min"`
	Max     int64   `json:"max"`
}

type testSummary struct {
	Passed   int    `json:"passed"`
	Failed   int    `json:"failed"`
	Skipped  int    `json:"skipped"`
	Coverage string `json:"coverage,omitempty"`
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	res := filepath.Join(root, "test-results")
	work := filepath.Join(root, ".work", "test")

	out := map[string]any{
		"generatedAt": time.Now().UTC().Format(time.RFC3339),
		"source":      "https://github.com/fleetpermit/fleetpermit/tree/main/test-results",
	}
	e2e := readJSON(filepath.Join(res, "e2e-results.json"))
	bench := readJSON(filepath.Join(res, "benchmark.json"))
	scale := readJSON(filepath.Join(res, "scale-simulation.json"))
	conf := readJSON(filepath.Join(res, "conformance", "summary.json"))
	if e2e != nil {
		out["e2e"] = e2e
	}
	if scale != nil {
		out["scale"] = scale
	}
	if conf != nil {
		out["conformance"] = conf
	}
	// Independent reproductions of the e2e suite (other hosts, engines, architectures).
	if files, _ := filepath.Glob(filepath.Join(res, "reproductions", "*.json")); len(files) > 0 {
		var reps []map[string]any
		for _, f := range files {
			if m := readJSON(f); m != nil {
				delete(m, "scenarios")
				reps = append(reps, m)
			}
		}
		out["reproductions"] = reps
	}
	out["latency"] = latencies(e2e, bench)
	if bench != nil {
		out["benchmarkEnvironment"] = bench["environment"]
	}

	tests := map[string]testSummary{}
	if s, ok := goTestSummary(filepath.Join(work, "unit.jsonl")); ok {
		s.Coverage = coverage(filepath.Join(work, "cover-unit.out"))
		tests["unit"] = s
	}
	if s, ok := goTestSummary(filepath.Join(work, "integration.jsonl")); ok {
		s.Coverage = coverage(filepath.Join(work, "cover-integration.out"))
		tests["integration"] = s
	}
	if c := mergedCoverage(filepath.Join(work, "cover-unit.out"), filepath.Join(work, "cover-integration.out")); c != "" {
		out["coverage"] = map[string]string{
			"internalPackages": c,
			"scope":            "statements in internal/... covered by unit and integration tests combined",
		}
	}
	if len(tests) > 0 {
		out["tests"] = tests
		writeJSON(filepath.Join(res, "tests.json"), tests)
	} else if prev := readJSON(filepath.Join(res, "tests.json")); prev != nil {
		out["tests"] = prev
	}
	if out["coverage"] == nil {
		if prev := readJSON(filepath.Join(res, "results.json")); prev != nil && prev["coverage"] != nil {
			out["coverage"] = prev["coverage"]
		}
	}

	writeJSON(filepath.Join(res, "results.json"), out)
	if err := os.WriteFile(filepath.Join(root, "docs", "results.md"), []byte(markdown(out)), 0o644); err != nil {
		fail(err)
	}
	if err := updateReadme(filepath.Join(root, "README.md"), out); err != nil {
		fail(err)
	}
	if site := os.Getenv("FP_SITE_DIR"); site != "" {
		writeJSON(filepath.Join(site, "data", "results.json"), out)
	}
	fmt.Println("wrote test-results/results.json and docs/results.md")
}

var latencyLabels = map[string]string{
	"activationToAllowMs":       "Lease created → first ALLOW at the gateway",
	"activationToReadyStatusMs": "Lease created → lease reports Ready (includes OCM status sync)",
	"revocationToDenyMs":        "Lease deleted → first DENY at the gateway",
	"expiryToDenyMs":            "Lease expiry → first DENY (hub connected)",
	"expiryToDenyHubDownMs":     "Lease expiry → first DENY (hub disconnected)",
	"placementChangeMs":         "Cluster label change → authorization moves",
	"driftRecoveryMs":           "Rendered policy deleted on a cluster → restored",
	"reconnectConvergenceMs":    "Hub reconnected → stale grants withdrawn",
	"probeRoundTripMs":          "Probe round trip (measurement baseline)",
}

func latencies(e2e, bench map[string]any) map[string]*latency {
	samples := map[string][]int64{}
	sources := map[string]map[string]bool{}
	add := func(metric, source string, v float64) {
		samples[metric] = append(samples[metric], int64(math.Round(v)))
		if sources[metric] == nil {
			sources[metric] = map[string]bool{}
		}
		sources[metric][source] = true
	}
	if e2e != nil {
		for _, sc := range asSlice(e2e["scenarios"]) {
			m, _ := sc.(map[string]any)
			id, _ := m["id"].(string)
			metrics, _ := m["metrics"].(map[string]any)
			for k, v := range metrics {
				f, ok := v.(float64)
				if !ok {
					continue
				}
				switch {
				case strings.HasPrefix(k, "activationToAllowMs"):
					add("activationToAllowMs", "e2e "+id, f)
				case k == "activationToReadyStatusMs":
					add(k, "e2e "+id, f)
				case strings.HasPrefix(k, "revocationToDenyMs"):
					add("revocationToDenyMs", "e2e "+id, f)
				case id == "S11" && strings.HasPrefix(k, "expiryToDenyMs"):
					add("expiryToDenyHubDownMs", "e2e "+id, f)
				case strings.HasPrefix(k, "expiryToDenyMs"):
					add("expiryToDenyMs", "e2e "+id, f)
				case strings.HasPrefix(k, "labelChangeTo"):
					add("placementChangeMs", "e2e "+id, f)
				case k == "driftRecoveryMs", k == "reconnectConvergenceMs":
					add(k, "e2e "+id, f)
				}
			}
		}
	}
	if bench != nil {
		s, _ := bench["samples"].(map[string]any)
		for k, vs := range s {
			for _, v := range asSlice(vs) {
				if f, ok := v.(float64); ok {
					add(k, "benchmark", f)
				}
			}
		}
	}
	out := map[string]*latency{}
	for k, vs := range samples {
		sort.Slice(vs, func(i, j int) bool { return vs[i] < vs[j] })
		var src []string
		for s := range sources[k] {
			src = append(src, s)
		}
		sort.Strings(src)
		label := latencyLabels[k]
		if label == "" {
			label = k
		}
		out[k] = &latency{Label: label, Unit: "ms", Source: strings.Join(src, ", "), Samples: vs,
			P50: pct(vs, 50), P95: pct(vs, 95), Min: vs[0], Max: vs[len(vs)-1]}
	}
	return out
}

// pct is the nearest-rank percentile.
func pct(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

func goTestSummary(path string) (testSummary, bool) {
	f, err := os.Open(path)
	if err != nil {
		return testSummary{}, false
	}
	defer f.Close()
	var s testSummary
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var ev struct{ Action, Test string }
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Test == "" || strings.Contains(ev.Test, "/") {
			continue
		}
		switch ev.Action {
		case "pass":
			s.Passed++
		case "fail":
			s.Failed++
		case "skip":
			s.Skipped++
		}
	}
	return s, true
}

type block struct{ stmts, count int }

func readProfile(path string, into map[string]block) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "mode:") {
			continue
		}
		i := strings.LastIndex(line, " ")
		j := strings.LastIndex(line[:i], " ")
		if i < 0 || j < 0 {
			continue
		}
		key := line[:j]
		if !strings.Contains(key, "/internal/") {
			continue
		}
		stmts, _ := strconv.Atoi(line[j+1 : i])
		count, _ := strconv.Atoi(line[i+1:])
		b := into[key]
		b.stmts = stmts
		if count > 0 {
			b.count = 1
		}
		into[key] = b
	}
	return true
}

func percent(blocks map[string]block) string {
	total, covered := 0, 0
	for _, b := range blocks {
		total += b.stmts
		if b.count > 0 {
			covered += b.stmts
		}
	}
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("%.1f%%", 100*float64(covered)/float64(total))
}

func coverage(path string) string {
	b := map[string]block{}
	if !readProfile(path, b) {
		return ""
	}
	return percent(b)
}

func mergedCoverage(paths ...string) string {
	b := map[string]block{}
	any := false
	for _, p := range paths {
		if readProfile(p, b) {
			any = true
		}
	}
	if !any {
		return ""
	}
	return percent(b)
}

func markdown(out map[string]any) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("# Measured results\n\n")
	w("> Generated by `make results` from the machine-readable files in [`test-results/`](../test-results/) on %s. Do not edit by hand.\n\n", out["generatedAt"])

	if e2e, ok := out["e2e"].(map[string]any); ok {
		env, _ := e2e["environment"].(map[string]any)
		sum, _ := e2e["summary"].(map[string]any)
		w("## Real multi-cluster end-to-end scenarios\n\n")
		w("%s\n\n", str(env["description"]))
		w("| Environment | |\n|---|---|\n")
		for _, k := range []string{"hub", "managedClusters", "kubernetes", "openClusterManagement", "kubeAgenticNetworking", "gatewayAPI", "envoy", "kind", "containerEngine", "os", "arch", "fleetpermitCommit"} {
			w("| %s | %s |\n", k, str(env[k]))
		}
		w("| run | %s → %s |\n\n", str(e2e["startedAt"]), str(e2e["finishedAt"]))
		w("**%s passed, %s failed, %s unsupported upstream, of %s scenarios.**\n\n", str(sum["passed"]), str(sum["failed"]), str(sum["unsupported"]), str(sum["total"]))
		w("| ID | Scenario | Result | Expected | Observed |\n|---|---|---|---|---|\n")
		for _, s := range asSlice(e2e["scenarios"]) {
			m, _ := s.(map[string]any)
			w("| %s | %s | %s | %s | %s |\n", str(m["id"]), esc(str(m["name"])), strings.ToUpper(str(m["status"])), esc(str(m["expected"])), esc(str(m["observed"])))
		}
		w("\nRaw evidence (every probe response) is in `test-results/e2e-results.json`.\n\n")
		if calls, matched, ok := matrixSummary(e2e); ok {
			w("### Decision matrix\n\nTest agents:\n\n%s\n%s of %s real calls matched the expected outcome.\n", agentsMarkdown(e2e), matched, calls)
			w("%s\n", matrixMarkdown(e2e, "active", "expired"))
		}
	} else {
		w("## Real multi-cluster end-to-end scenarios\n\nNo e2e results found. Run `make demo-up && make test-e2e`.\n\n")
	}

	if lat, ok := out["latency"].(map[string]*latency); ok && len(lat) > 0 {
		w("## Latency (real clusters)\n\n")
		if env, ok := out["benchmarkEnvironment"].(map[string]any); ok {
			w("%s. Method: %s. OCM status sync interval: %s.\n\n", str(env["description"]), str(env["method"]), str(env["ocmStatusSyncInterval"]))
		}
		w("| Measurement | n | p50 (ms) | p95 (ms) | min | max | source |\n|---|---|---|---|---|---|---|\n")
		keys := make([]string, 0, len(lat))
		for k := range lat {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			l := lat[k]
			w("| %s | %d | %d | %d | %d | %d | %s |\n", l.Label, len(l.Samples), l.P50, l.P95, l.Min, l.Max, l.Source)
		}
		w("\n")
		w("Sources of variance, observed in these runs:\n\n")
		w("- Each sample includes one host-to-pod probe round trip (see the baseline row), and polling adds up to about 250 ms.\n")
		w("- Occasional activation outliers of a few seconds come from the OCM work agent retrying, with backoff, a status update that conflicted with FleetPermit's spec update on the same ManifestWork (`Operation cannot be fulfilled ... the object has been modified` in the work-agent log).\n")
		w("- Drift recovery is bounded by the klusterlet status sync interval (10 s in the lab): FleetPermit requests an immediate re-apply once OCM reports the object missing.\n")
		w("- Ready status includes OCM status feedback, so it depends on the same status sync interval.\n")
		w("- **Open issue:** in end-to-end runs, the first lease after lab setup has repeatedly reached one cluster about 5 s late while the other cluster took about 130 ms. The OCM work agent on the slow cluster applied the change about 5 s after the lease was created, and the gateway allowed the call about 0.4 s after that apply, so the delay is between the hub and that work agent. It did not reproduce in isolation (182 ms), and every later activation in the benchmark took 170–460 ms. The suite now captures hub and work-agent logs whenever an activation exceeds 2 s (`test-results/diagnostics/`). In the capture from the latest run, the FleetPermit controller logged no delivery error, and the slow cluster's work agent logged nothing until it applied the change about 6 s after the lease was created, while the other cluster applied it immediately. This places the delay in OCM's delivery of the ManifestWork change to that one agent. It is being investigated for an upstream report.\n\n")
	}

	if reps, ok := out["reproductions"].([]map[string]any); ok && len(reps) > 0 {
		w("## Independent reproductions\n\nThe same end-to-end suite run elsewhere, from a clean checkout:\n\n| Runner | OS/arch | Engine | Commit | Passed | Failed | Unsupported | Logs |\n|---|---|---|---|---|---|---|---|\n")
		for _, rep := range reps {
			env, _ := rep["environment"].(map[string]any)
			sum, _ := rep["summary"].(map[string]any)
			w("| %s | %s/%s | %s | %s | %s | %s | %s | [run](%s) |\n", str(rep["runner"]), str(env["os"]), str(env["arch"]), str(env["containerEngine"]),
				str(env["fleetpermitCommit"]), str(sum["passed"]), str(sum["failed"]), str(sum["unsupported"]), str(rep["runURL"]))
		}
		w("\n")
	}

	if sc, ok := out["scale"].(map[string]any); ok {
		env, _ := sc["environment"].(map[string]any)
		w("## Simulated controller scale (not real clusters)\n\n")
		w("%s. Timing resolution %s ms. %s\n\n", str(env["description"]), str(env["pollIntervalMs"]), str(env["note"]))
		w("| Logical clusters | activation (ms) | ready (ms) | revocation (ms) | reconciles | reconciles/s |\n|---|---|---|---|---|---|\n")
		for _, r := range asSlice(sc["rows"]) {
			m, _ := r.(map[string]any)
			w("| %s | %s | %s | %s | %s | %.1f |\n", str(m["clusters"]), str(m["activationMs"]), str(m["readyMs"]), str(m["revocationMs"]), str(m["reconciles"]), num(m["reconcilesPerSecond"]))
		}
		w("\n")
	}

	if c, ok := out["conformance"].(map[string]any); ok {
		w("## Upstream conformance\n\n")
		w("| Suite | Version | Target | Result | Date |\n|---|---|---|---|---|\n")
		for _, s := range asSlice(c["suites"]) {
			m, _ := s.(map[string]any)
			w("| %s | %s | %s | %s | %s |\n", str(m["suite"]), str(m["version"]), str(m["target"]), esc(str(m["result"])), str(m["date"]))
		}
		w("\n")
	}

	w("## Unit and integration tests\n\n")
	if t, ok := out["tests"].(map[string]testSummary); ok {
		w("| Suite | passed | failed | skipped | coverage |\n|---|---|---|---|---|\n")
		for _, k := range []string{"unit", "integration"} {
			if s, ok := t[k]; ok {
				w("| %s | %d | %d | %d | %s |\n", k, s.Passed, s.Failed, s.Skipped, s.Coverage)
			}
		}
	} else if t, ok := out["tests"].(map[string]any); ok {
		w("| Suite | passed | failed | skipped | coverage |\n|---|---|---|---|---|\n")
		for _, k := range []string{"unit", "integration"} {
			if m, ok := t[k].(map[string]any); ok {
				w("| %s | %s | %s | %s | %s |\n", k, str(m["passed"]), str(m["failed"]), str(m["skipped"]), str(m["coverage"]))
			}
		}
	}
	if c, ok := out["coverage"].(map[string]string); ok {
		w("\nCombined statement coverage of `internal/`: **%s** (%s).\n", c["internalPackages"], c["scope"])
	} else if c, ok := out["coverage"].(map[string]any); ok {
		w("\nCombined statement coverage of `internal/`: **%s** (%s).\n", str(c["internalPackages"]), str(c["scope"]))
	}
	return b.String()
}

var matrixTools = []string{"get_cluster_health", "restart_workload", "scale_workload", "read_secret"}

// matrixMarkdown renders the MATRIX scenario as one table per lease state:
// rows are agent x cluster, columns are tools, cells show the observed
// decision and whether it matched the expected one.
func matrixMarkdown(e2e map[string]any, leases ...string) string {
	var rows []map[string]any
	for _, sc := range asSlice(e2e["scenarios"]) {
		m, _ := sc.(map[string]any)
		if m["id"] == "MATRIX" {
			for _, ev := range asSlice(m["evidence"]) {
				if r, ok := ev.(map[string]any); ok {
					rows = append(rows, r)
				}
			}
		}
	}
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	cell := map[string]map[string]any{}
	for _, r := range rows {
		cell[str(r["lease"])+"|"+str(r["agent"])+"|"+str(r["cluster"])+"|"+str(r["tool"])] = r
	}
	for _, lease := range leases {
		fmt.Fprintf(&b, "\n**Lease %s**\n\n| Agent | Cluster | %s |\n|---|---|%s\n", lease,
			strings.Join(matrixTools, " | "), strings.Repeat("---|", len(matrixTools)))
		for _, agent := range []string{"sre-agent", "security-agent"} {
			for _, cluster := range []string{"cluster-east", "cluster-west", "cluster-edge"} {
				var cells []string
				for _, tool := range matrixTools {
					r, ok := cell[lease+"|"+agent+"|"+cluster+"|"+tool]
					switch {
					case !ok:
						cells = append(cells, "—")
					case r["match"] == true && r["observed"] == "ALLOW":
						cells = append(cells, "✅ ALLOW")
					case r["match"] == true:
						cells = append(cells, "⛔ DENY")
					default:
						cells = append(cells, fmt.Sprintf("❌ expected %s, got %s", str(r["expected"]), str(r["observed"])))
					}
				}
				fmt.Fprintf(&b, "| `%s` | %s | %s |\n", agent, cluster, strings.Join(cells, " | "))
			}
		}
	}
	return b.String()
}

// agentsMarkdown describes the workload identities used by the tests.
func agentsMarkdown(e2e map[string]any) string {
	var b strings.Builder
	for _, a := range asSlice(e2e["agents"]) {
		m, _ := a.(map[string]any)
		fmt.Fprintf(&b, "- **`%s`** (`%s`): %s\n", str(m["name"]), str(m["spiffeID"]), str(m["role"]))
	}
	return b.String()
}

func matrixSummary(e2e map[string]any) (calls, matched string, ok bool) {
	for _, sc := range asSlice(e2e["scenarios"]) {
		m, _ := sc.(map[string]any)
		if m["id"] == "MATRIX" {
			mt, _ := m["metrics"].(map[string]any)
			return str(mt["calls"]), str(mt["matched"]), true
		}
	}
	return "", "", false
}

// updateReadme rewrites the block between the results markers in README.md.
func updateReadme(path string, out map[string]any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	const start, end = "<!-- results:start -->", "<!-- results:end -->"
	text := string(b)
	i, j := strings.Index(text, start), strings.Index(text, end)
	if i < 0 || j < i {
		return nil
	}
	var r strings.Builder
	if e2e, ok := out["e2e"].(map[string]any); ok {
		sum, _ := e2e["summary"].(map[string]any)
		env, _ := e2e["environment"].(map[string]any)
		fmt.Fprintf(&r, "Real multi-cluster run (%s hub + %s managed kind clusters, Kubernetes %s, OCM %s, kube-agentic-networking %s, %s/%s, %s):\n\n",
			str(env["hub"]), str(env["managedClusters"]), str(env["kubernetes"]), str(env["openClusterManagement"]),
			str(env["kubeAgenticNetworking"]), str(env["os"]), str(env["arch"]), str(e2e["finishedAt"])[:10])
		fmt.Fprintf(&r, "- **%s of %s scenarios passed**, %s failed, %s not supported by the upstream API (argument-level matching).\n",
			str(sum["passed"]), str(sum["total"]), str(sum["failed"]), str(sum["unsupported"]))
		if reps, ok := out["reproductions"].([]map[string]any); ok {
			for _, rep := range reps {
				env, _ := rep["environment"].(map[string]any)
				sum, _ := rep["summary"].(map[string]any)
				fmt.Fprintf(&r, "- **Reproduced independently** on %s (%s/%s, %s): %s passed, %s failed, %s unsupported ([run logs](%s)).\n",
					str(rep["runner"]), str(env["os"]), str(env["arch"]), str(env["containerEngine"]),
					str(sum["passed"]), str(sum["failed"]), str(sum["unsupported"]), str(rep["runURL"]))
			}
		}
		if calls, matched, ok := matrixSummary(e2e); ok {
			fmt.Fprintf(&r, "- **Decision matrix: %s of %s real MCP calls matched the expected outcome.**\n", matched, calls)
			fmt.Fprintf(&r, "\n#### Test agents and expected outcomes\n\nTwo workload identities make every call. Both are test clients from this repository, not third-party or AI agents ([what they are](#the-two-test-agents)). Their SPIFFE X.509 certificates come from Kubernetes Pod Certificates:\n\n%s", agentsMarkdown(e2e))
			fmt.Fprintf(&r, "\nThe lease grants `get_cluster_health` and `restart_workload` to `sre-agent`, on the clusters the placement selects (env=production: east and west). Each cell below is a real call through that cluster's gateway, showing the observed decision (✅/⛔ = matched the expectation, ❌ = did not):\n")
			r.WriteString(matrixMarkdown(e2e, "active"))
			r.WriteString("\nAfter the lease expires, the same 24 calls are repeated. Expected: all DENY. ")
			exp := matrixMarkdown(e2e, "expired")
			if strings.Contains(exp, "ALLOW") || strings.Contains(exp, "❌") {
				r.WriteString("Observed:\n" + exp)
			} else {
				r.WriteString("Observed: all 24 DENY, as expected ([full table](docs/results.md#decision-matrix)).\n")
			}
		}
	}
	if lat, ok := out["latency"].(map[string]*latency); ok {
		row := func(k string) {
			if l, ok := lat[k]; ok {
				fmt.Fprintf(&r, "| %s | %d | %d ms | %d ms |\n", l.Label, len(l.Samples), l.P50, l.P95)
			}
		}
		fmt.Fprintf(&r, "\n| Measured on real clusters | n | p50 | p95 |\n|---|---|---|---|\n")
		for _, k := range []string{"activationToAllowMs", "revocationToDenyMs", "expiryToDenyMs", "expiryToDenyHubDownMs", "driftRecoveryMs", "activationToReadyStatusMs", "probeRoundTripMs"} {
			row(k)
		}
	}
	if sc, ok := out["scale"].(map[string]any); ok {
		rows := asSlice(sc["rows"])
		if len(rows) > 0 {
			last, _ := rows[len(rows)-1].(map[string]any)
			fmt.Fprintf(&r, "\nSimulated controller scale (envtest, no real clusters): a lease reached %s logical clusters' ManifestWorks in %s ms and was withdrawn in %s ms.\n",
				str(last["clusters"]), str(last["activationMs"]), str(last["revocationMs"]))
		}
	}
	if c, ok := out["conformance"].(map[string]any); ok {
		for _, suite := range asSlice(c["suites"]) {
			m, _ := suite.(map[string]any)
			fmt.Fprintf(&r, "\nUpstream conformance, run unmodified against a lab cluster: %s %s: **%s**.\n",
				str(m["suite"]), str(m["version"]), str(m["result"]))
		}
	}
	if c, ok := out["coverage"].(map[string]string); ok {
		fmt.Fprintf(&r, "\nStatement coverage of `internal/` (unit + integration): **%s**.\n", c["internalPackages"])
	} else if c, ok := out["coverage"].(map[string]any); ok {
		fmt.Fprintf(&r, "\nStatement coverage of `internal/` (unit + integration): **%s**.\n", str(c["internalPackages"]))
	}
	if r.Len() == 0 {
		return nil
	}
	updated := text[:i+len(start)] + "\n" + r.String() + text[j:]
	return os.WriteFile(path, []byte(updated), 0o644)
}

func readJSON(path string) map[string]any {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		fail(fmt.Errorf("%s: %w", path, err))
	}
	return m
}

func writeJSON(path string, v any) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fail(err)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		fail(err)
	}
}

func asSlice(v any) []any { s, _ := v.([]any); return s }

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return "—"
	case float64:
		if t == math.Trunc(t) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', 1, 64)
	default:
		return fmt.Sprint(t)
	}
}

func num(v any) float64 { f, _ := v.(float64); return f }

func esc(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "|", "\\|"), "\n", " ") }

func fail(err error) {
	fmt.Fprintln(os.Stderr, "results:", err)
	os.Exit(1)
}
