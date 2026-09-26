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

// Command demo-mcp-tools is a deterministic MCP server used by the FleetPermit
// lab. Its tools only describe what they would do; they never touch the
// cluster. Every response is a pure function of the input and the
// CLUSTER_NAME environment variable, so demo runs are reproducible.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var version = "dev"

type empty struct{}

type workloadRef struct {
	Namespace string `json:"namespace" jsonschema:"namespace of the workload"`
	Name      string `json:"name" jsonschema:"name of the workload"`
}

type scaleInput struct {
	Namespace string `json:"namespace" jsonschema:"namespace of the workload"`
	Name      string `json:"name" jsonschema:"name of the workload"`
	Replicas  int    `json:"replicas" jsonschema:"desired replica count"`
}

type result struct {
	Cluster string `json:"cluster"`
	Tool    string `json:"tool"`
	Outcome string `json:"outcome"`
}

func text(cluster, tool, outcome string) (*mcp.CallToolResult, result, error) {
	r := result{Cluster: cluster, Tool: tool, Outcome: outcome}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("[%s] %s: %s", cluster, tool, outcome)}}}, r, nil
}

func newServer(cluster string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "fleetpermit-demo-tools", Version: version}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "get_cluster_health", Description: "Read-only: report cluster health (simulated)."},
		func(context.Context, *mcp.CallToolRequest, empty) (*mcp.CallToolResult, result, error) {
			return text(cluster, "get_cluster_health", "healthy (simulated)")
		})
	mcp.AddTool(s, &mcp.Tool{Name: "restart_workload", Description: "Privileged: restart a workload (simulated)."},
		func(_ context.Context, _ *mcp.CallToolRequest, in workloadRef) (*mcp.CallToolResult, result, error) {
			return text(cluster, "restart_workload", fmt.Sprintf("restart of %s/%s accepted (simulated)", in.Namespace, in.Name))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "scale_workload", Description: "Privileged: scale a workload (simulated)."},
		func(_ context.Context, _ *mcp.CallToolRequest, in scaleInput) (*mcp.CallToolResult, result, error) {
			return text(cluster, "scale_workload", fmt.Sprintf("scale of %s/%s to %d accepted (simulated)", in.Namespace, in.Name, in.Replicas))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "read_secret", Description: "Prohibited in the demo policy: read a secret (returns a fixed fake value)."},
		func(_ context.Context, _ *mcp.CallToolRequest, in workloadRef) (*mcp.CallToolResult, result, error) {
			return text(cluster, "read_secret", fmt.Sprintf("%s/%s = fake-demo-value", in.Namespace, in.Name))
		})
	return s
}

func main() {
	addr := flag.String("listen", ":3001", "listen address")
	path := flag.String("path", "/mcp", "MCP endpoint path")
	flag.Parse()

	cluster := os.Getenv("CLUSTER_NAME")
	if cluster == "" {
		cluster = "unknown"
	}
	server := newServer(cluster)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})

	mux := http.NewServeMux()
	mux.Handle(*path, handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	slog.Info("serving MCP", "addr", *addr, "path", *path, "cluster", cluster, "version", version)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
