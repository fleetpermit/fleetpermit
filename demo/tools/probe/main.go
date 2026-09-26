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

// Command demo-probe makes one deterministic MCP tool call through an
// agentic-networking Gateway and reports whether it was allowed.
//
// It authenticates with the SPIFFE X.509 identity that Kubernetes Pod
// Certificates mount into the pod, verifies the gateway against the SPIFFE
// trust bundle, runs the MCP initialize handshake, then calls tools/call. It
// prints one JSON line describing the outcome. A JSON-RPC error whose message
// reads as an authorization refusal (forbidden, denied, RBAC) is reported as
// DENY; the raw status and message are included in the output.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

// Outcome is printed as a single JSON line.
type Outcome struct {
	Cluster    string `json:"cluster,omitempty"`
	Tool       string `json:"tool"`
	Decision   string `json:"decision"` // ALLOW, DENY or ERROR
	Stage      string `json:"stage"`    // initialize or tools/call
	HTTPStatus int    `json:"httpStatus,omitempty"`
	Detail     string `json:"detail"`
	LatencyMS  int64  `json:"latencyMs"`
	Timestamp  string `json:"timestamp"`
}

func main() {
	target := flag.String("url", "", "MCP endpoint URL of the gateway, for example https://10.89.0.200:10001/mcp")
	tool := flag.String("tool", "", "tool to call")
	args := flag.String("args", "{}", "tool arguments as a JSON object")
	certDir := flag.String("cert-dir", "/run/agent-identity-mtls", "directory with credential-bundle.pem and <trust-domain>.trust-bundle.pem")
	trustDomain := flag.String("trust-domain", "cluster.local", "SPIFFE trust domain of the gateway")
	cluster := flag.String("cluster", "", "label for the target cluster in the output")
	expect := flag.String("expect", "", "exit non-zero unless the decision equals this value (ALLOW or DENY)")
	timeout := flag.Duration("timeout", 10*time.Second, "request timeout")
	idle := flag.Bool("idle", false, "block forever; lets the probe run as a long-lived agent pod that is exec'd into")
	flag.Parse()

	if *idle {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		<-ctx.Done()
		return
	}

	out := Outcome{Cluster: *cluster, Tool: *tool}
	start := time.Now()
	decide(&out, *target, *tool, *args, *certDir, *trustDomain, *timeout)
	out.LatencyMS = time.Since(start).Milliseconds()
	out.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(out)
	if *expect != "" && !strings.EqualFold(*expect, out.Decision) {
		os.Exit(1)
	}
}

func decide(out *Outcome, target, tool, args, certDir, trustDomain string, timeout time.Duration) {
	if target == "" || tool == "" {
		out.Decision, out.Detail = "ERROR", "both -url and -tool are required"
		return
	}
	var arguments map[string]any
	if err := json.Unmarshal([]byte(args), &arguments); err != nil {
		out.Decision, out.Detail = "ERROR", "invalid -args: "+err.Error()
		return
	}
	httpClient, err := newClient(certDir, trustDomain, timeout)
	if err != nil {
		out.Decision, out.Detail = "ERROR", err.Error()
		return
	}
	c := &mcpClient{http: httpClient, url: target}

	out.Stage = "initialize"
	status, resp, err := c.call("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "fleetpermit-probe", "version": version},
	}, true)
	if classify(out, status, resp, err) != "ALLOW" {
		return
	}
	_, _, _ = c.call("notifications/initialized", nil, false)

	out.Stage = "tools/call"
	status, resp, err = c.call("tools/call", map[string]any{"name": tool, "arguments": arguments}, true)
	classify(out, status, resp, err)
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// classify records the decision implied by one response.
func classify(out *Outcome, status int, resp *rpcResponse, err error) string {
	out.HTTPStatus = status
	switch {
	case err != nil:
		out.Decision, out.Detail = "ERROR", err.Error()
	case status == http.StatusForbidden || status == http.StatusUnauthorized:
		out.Decision, out.Detail = "DENY", fmt.Sprintf("gateway returned HTTP %d", status)
	case status != http.StatusOK:
		out.Decision, out.Detail = "ERROR", fmt.Sprintf("unexpected HTTP %d", status)
	case resp == nil:
		out.Decision, out.Detail = "ERROR", "empty response"
	case resp.Error != nil:
		msg := resp.Error.Message
		lower := strings.ToLower(msg)
		if strings.Contains(lower, "forbidden") || strings.Contains(lower, "denied") || strings.Contains(lower, "rbac") {
			out.Decision = "DENY"
		} else {
			out.Decision = "ERROR"
		}
		out.Detail = fmt.Sprintf("JSON-RPC error %d: %s", resp.Error.Code, msg)
	default:
		out.Decision, out.Detail = "ALLOW", summarize(resp.Result)
	}
	return out.Decision
}

func summarize(raw json.RawMessage) string {
	var r struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if json.Unmarshal(raw, &r) == nil && len(r.Content) > 0 {
		if r.IsError {
			return "tool error: " + r.Content[0].Text
		}
		return r.Content[0].Text
	}
	if len(raw) > 200 {
		raw = raw[:200]
	}
	return string(raw)
}

type mcpClient struct {
	http    *http.Client
	url     string
	session string
	id      int
}

func (c *mcpClient) call(method string, params any, expectReply bool) (int, *rpcResponse, error) {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	if expectReply {
		c.id++
		msg["id"] = c.id
	}
	body, _ := json.Marshal(msg)
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if method != "initialize" {
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	}
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	res, err := c.http.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return 0, nil, fmt.Errorf("request failed: %v", uerr.Err)
		}
		return 0, nil, err
	}
	defer res.Body.Close()
	if s := res.Header.Get("Mcp-Session-Id"); s != "" {
		c.session = s
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return res.StatusCode, nil, err
	}
	if !expectReply || res.StatusCode != http.StatusOK {
		return res.StatusCode, nil, nil
	}
	payload := data
	if strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		payload = nil
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "data:") {
				payload = []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
	}
	var r rpcResponse
	if err := json.Unmarshal(payload, &r); err != nil {
		return res.StatusCode, nil, fmt.Errorf("decoding JSON-RPC response: %v", err)
	}
	return res.StatusCode, &r, nil
}

// newClient builds an HTTP client that presents the pod's SPIFFE X.509 SVID
// and verifies the server's SVID against the trust bundle. SPIFFE SVIDs carry
// a URI SAN instead of a DNS name, so the chain and trust domain are verified
// explicitly rather than with hostname verification.
func newClient(certDir, trustDomain string, timeout time.Duration) (*http.Client, error) {
	bundle, err := os.ReadFile(filepath.Join(certDir, "credential-bundle.pem"))
	if err != nil {
		return nil, fmt.Errorf("reading workload credentials: %w", err)
	}
	var certPEM, keyPEM []byte
	for rest := bundle; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if strings.Contains(block.Type, "PRIVATE KEY") {
			keyPEM = append(keyPEM, pem.EncodeToMemory(block)...)
		} else {
			certPEM = append(certPEM, pem.EncodeToMemory(block)...)
		}
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("loading workload certificate: %w", err)
	}
	trust, err := os.ReadFile(filepath.Join(certDir, trustDomain+".trust-bundle.pem"))
	if err != nil {
		return nil, fmt.Errorf("reading trust bundle: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(trust) {
		return nil, errors.New("trust bundle contains no certificates")
	}
	verify := func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("server presented no certificate")
		}
		certs := make([]*x509.Certificate, len(raw))
		for i, r := range raw {
			c, err := x509.ParseCertificate(r)
			if err != nil {
				return err
			}
			certs[i] = c
		}
		inter := x509.NewCertPool()
		for _, c := range certs[1:] {
			inter.AddCert(c)
		}
		if _, err := certs[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
			return fmt.Errorf("server certificate not trusted: %w", err)
		}
		for _, u := range certs[0].URIs {
			if u.Scheme == "spiffe" && u.Host == trustDomain {
				return nil
			}
		}
		return fmt.Errorf("server certificate has no SPIFFE ID in trust domain %q", trustDomain)
	}
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		// Hostname verification is replaced by VerifyPeerCertificate above.
		InsecureSkipVerify:    true, //nolint:gosec // SPIFFE verification is performed in VerifyPeerCertificate.
		VerifyPeerCertificate: verify,
	}
	return &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: tlsConfig}}, nil
}
