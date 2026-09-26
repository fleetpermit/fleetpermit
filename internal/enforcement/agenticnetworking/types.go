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

// The structs below mirror the subset of the upstream XAccessPolicy API that
// FleetPermit renders. They are pinned to kubernetes-sigs/kube-agentic-networking
// v0.2.0 (api/v1alpha1/accesspolicy_types.go). Importing the upstream module
// would pull in its Envoy control-plane dependencies, so the fields are
// mirrored here and the integration tests validate rendered objects against
// the upstream CRD schema.

const (
	// APIVersion of the upstream XAccessPolicy (experimental).
	APIVersion = "agentic.networking.x-k8s.io/v1alpha1"
	// Kind of the upstream access policy.
	Kind = "XAccessPolicy"
	// Group of the upstream API.
	Group = "agentic.networking.x-k8s.io"
	// Resource is the plural resource name.
	Resource = "xaccesspolicies"

	// MaxRulesPerPolicy is the upstream MaxItems for spec.rules.
	MaxRulesPerPolicy = 10
)

type objectMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type accessPolicy struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   objectMeta `json:"metadata"`
	Spec       policySpec `json:"spec"`
}

type policySpec struct {
	TargetRefs []targetRef `json:"targetRefs"`
	Action     string      `json:"action"`
	Rules      []rule      `json:"rules"`
}

type targetRef struct {
	Group string `json:"group"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
}

type rule struct {
	Name          string         `json:"name"`
	Source        source         `json:"source"`
	Authorization *authorization `json:"authorization,omitempty"`
}

type source struct {
	Type   string `json:"type"`
	SPIFFE string `json:"spiffe,omitempty"`
}

type authorization struct {
	Type string   `json:"type"`
	MCP  *mcp     `json:"mcp,omitempty"`
	CEL  *celRule `json:"cel,omitempty"`
}

type mcp struct {
	MCPBaseProtocolMethodsOption string `json:"mcpBaseProtocolMethodsOption,omitempty"`
}

type celRule struct {
	Expression string `json:"expression"`
}
