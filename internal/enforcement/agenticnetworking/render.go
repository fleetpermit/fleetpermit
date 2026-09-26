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

// Package agenticnetworking renders FleetPermit grants as XAccessPolicy
// objects for the Kubernetes SIG Network kube-agentic-networking project.
//
// Each lease grant becomes one CEL rule of the form
//
//	request.mcp.tool_name in ['restart_workload'] && request.time < timestamp('2026-09-26T10:15:00Z')
//
// so the gateway itself stops honouring the grant at expiry, independent of
// FleetPermit, Open Cluster Management or network connectivity to the hub.
// Upstream evaluates CEL rules for tools/call only; MCP session methods
// (initialize, tools/list, ping) are granted by a separate inline rule per
// subject that lives exactly as long as that subject holds a grant on the
// cluster.
package agenticnetworking

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	fpv1 "github.com/fleetpermit/fleetpermit/api/v1alpha1"
	"github.com/fleetpermit/fleetpermit/internal/digest"
	"github.com/fleetpermit/fleetpermit/internal/enforcement"
)

// Label and annotation keys written on rendered objects.
const (
	LabelManagedBy       = "app.kubernetes.io/managed-by"
	ManagedByValue       = "fleetpermit"
	LabelPolicyUID       = "fleetpermit.github.io/policy-uid"
	AnnotationPolicy     = "fleetpermit.github.io/policy"
	AnnotationPolicyUID  = "fleetpermit.github.io/policy-uid"
	AnnotationGeneration = "fleetpermit.github.io/policy-generation"
	AnnotationCluster    = "fleetpermit.github.io/cluster"
	AnnotationLeases     = "fleetpermit.github.io/lease-uids"
	AnnotationDigest     = "fleetpermit.github.io/content-digest"
	AnnotationExpiresAt  = "fleetpermit.github.io/expires-at"
)

var (
	// Same pattern the FleetPermit API enforces. Checked again here so that a
	// value that bypassed admission can never be spliced into a CEL string.
	toolPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,19}$`)
	spiffePattern = regexp.MustCompile(`^spiffe://[a-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)
)

// Renderer implements enforcement.Renderer for XAccessPolicy.
type Renderer struct{}

var _ enforcement.Renderer = Renderer{}

// ObjectName is the XAccessPolicy name for a policy. It is unique per hub
// namespace/name and bounded to 63 characters.
func ObjectName(policyNamespace, policyName string) string {
	name := "fleetpermit-" + policyName
	suffix := "-" + digest.Short(policyNamespace+"/"+policyName, 8)
	if len(name)+len(suffix) > 63 {
		name = strings.TrimRight(name[:63-len(suffix)], "-.")
	}
	return name + suffix
}

// Render builds at most one XAccessPolicy for the request.
func (Renderer) Render(req enforcement.Request) (enforcement.Result, error) {
	var res enforcement.Result
	p := req.Policy
	if p == nil {
		return res, fmt.Errorf("render: policy is required")
	}

	var rules []rule
	sessions := map[string]bool{}
	var leaseUIDs []string
	var latest time.Time

	for _, g := range req.Grants {
		if err := validateGrant(g); err != nil {
			return enforcement.Result{}, fmt.Errorf("render grant %s: %w", g.ID, err)
		}
		needed := 1
		if !sessions[g.Subject] {
			needed++
		}
		if len(rules)+needed > MaxRulesPerPolicy {
			res.Dropped = append(res.Dropped, g.ID)
			continue
		}
		if !sessions[g.Subject] {
			sessions[g.Subject] = true
			rules = append(rules, sessionRule(g.Subject))
		}
		rules = append(rules, grantRule(g))
		res.Rendered = append(res.Rendered, g.ID)
		if g.Lease != "" {
			leaseUIDs = append(leaseUIDs, g.ID)
		}
		if g.ExpiresAt.After(latest) {
			latest = g.ExpiresAt
		}
	}
	if len(rules) == 0 {
		// No grants: deliver an inert policy instead of nothing. The policy
		// stays wired to every placed cluster (and reported Ready only once
		// the enforcement layer accepts it), and a later grant or revocation
		// is an in-place update rather than a delete and re-create.
		rules = []rule{inertRule()}
	}

	ap := accessPolicy{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata: objectMeta{
			Name:      ObjectName(p.Namespace, p.Name),
			Namespace: p.Spec.Target.Namespace,
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
				LabelPolicyUID: string(p.UID),
			},
		},
		Spec: policySpec{
			TargetRefs: []targetRef{{
				Group: targetGroup(p.Spec.Target.Ref),
				Kind:  targetKind(p.Spec.Target.Ref),
				Name:  p.Spec.Target.Ref.Name,
			}},
			Action: "Allow",
			Rules:  rules,
		},
	}

	// The digest binds the enforced content to its source policy and cluster.
	d, err := digest.Of(struct {
		PolicyUID string     `json:"policyUID"`
		Cluster   string     `json:"cluster"`
		Namespace string     `json:"namespace"`
		Name      string     `json:"name"`
		Spec      policySpec `json:"spec"`
	}{string(p.UID), req.Cluster, ap.Metadata.Namespace, ap.Metadata.Name, ap.Spec})
	if err != nil {
		return enforcement.Result{}, err
	}

	ap.Metadata.Annotations = map[string]string{
		AnnotationPolicy:     p.Namespace + "/" + p.Name,
		AnnotationPolicyUID:  string(p.UID),
		AnnotationGeneration: fmt.Sprintf("%d", p.Generation),
		AnnotationCluster:    req.Cluster,
		AnnotationDigest:     d,
	}
	if len(leaseUIDs) > 0 {
		ap.Metadata.Annotations[AnnotationLeases] = strings.Join(leaseUIDs, ",")
	}
	if !latest.IsZero() {
		ap.Metadata.Annotations[AnnotationExpiresAt] = latest.UTC().Format(time.RFC3339)
	}

	obj, err := toUnstructured(ap)
	if err != nil {
		return enforcement.Result{}, err
	}
	res.Objects = []*unstructured.Unstructured{obj}
	res.Resources = []enforcement.Resource{{
		Group: Group, Resource: Resource, Namespace: ap.Metadata.Namespace, Name: ap.Metadata.Name,
	}}
	res.Digest = d
	return res, nil
}

func validateGrant(g enforcement.Grant) error {
	if !spiffePattern.MatchString(g.Subject) {
		return fmt.Errorf("invalid SPIFFE ID %q", g.Subject)
	}
	if len(g.Tools) == 0 {
		return fmt.Errorf("grant has no tools")
	}
	for _, t := range g.Tools {
		if !toolPattern.MatchString(t) {
			return fmt.Errorf("invalid tool name %q", t)
		}
	}
	return nil
}

// InertRuleName names the single rule of a policy that grants nothing.
const InertRuleName = "no-active-grants"

// inertRule can never match: its source is an identity in a reserved,
// non-resolvable trust domain and its condition is false.
func inertRule() rule {
	return rule{
		Name:          InertRuleName,
		Source:        source{Type: "SPIFFE", SPIFFE: "spiffe://fleetpermit.invalid/no-active-grants"},
		Authorization: &authorization{Type: "CEL", CEL: &celRule{Expression: "false"}},
	}
}

// sessionRule lets a subject open an MCP session and list tools. It does not
// permit tools/call.
func sessionRule(subject string) rule {
	return rule{
		Name:   "session-" + digest.Short(subject, 10),
		Source: source{Type: "SPIFFE", SPIFFE: subject},
		Authorization: &authorization{
			Type: "Inline",
			MCP:  &mcp{MCPBaseProtocolMethodsOption: "MATCH_BASE_PROTOCOL_METHODS"},
		},
	}
}

// grantRule permits tools/call for the listed tools, until ExpiresAt if set.
func grantRule(g enforcement.Grant) rule {
	prefix := "lease-"
	if g.Lease == "" {
		prefix = "standing-"
	}
	return rule{
		Name:   prefix + digest.Short(g.ID, 10),
		Source: source{Type: "SPIFFE", SPIFFE: g.Subject},
		Authorization: &authorization{
			Type: "CEL",
			CEL:  &celRule{Expression: Expression(g.Tools, g.ExpiresAt)},
		},
	}
}

// Expression is the CEL expression for a set of tools and an optional expiry.
// Tool names are validated against a strict character set before use.
func Expression(tools []string, expiresAt time.Time) string {
	quoted := make([]string, len(tools))
	for i, t := range tools {
		quoted[i] = "'" + t + "'"
	}
	expr := "request.mcp.tool_name in [" + strings.Join(quoted, ", ") + "]"
	if !expiresAt.IsZero() {
		expr += " && request.time < timestamp('" + expiresAt.UTC().Format(time.RFC3339) + "')"
	}
	return expr
}

func targetGroup(r fpv1.TargetRef) string {
	if r.Group == "" {
		return Group
	}
	return r.Group
}

func targetKind(r fpv1.TargetRef) string {
	if r.Kind == "" {
		return "XBackend"
	}
	return r.Kind
}

func toUnstructured(v any) (*unstructured.Unstructured, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encoding XAccessPolicy: %w", err)
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(b); err != nil {
		return nil, fmt.Errorf("decoding XAccessPolicy: %w", err)
	}
	return u, nil
}
