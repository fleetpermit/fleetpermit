#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Prints the tool-side manifests for one managed cluster: the MCP tool server,
# the agentic-networking Gateway, HTTPRoute and XBackend, and the
# default-deny anchor. Usage: FP_CLUSTER_NAME=cluster-east render-workloads.sh
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
: "${FP_CLUSTER_NAME:?FP_CLUSTER_NAME is required}"

cat <<YAML
apiVersion: v1
kind: Namespace
metadata:
  name: ${FP_TOOLS_NAMESPACE}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${FP_BACKEND_NAME}
  namespace: ${FP_TOOLS_NAMESPACE}
  labels: {app: ${FP_BACKEND_NAME}}
spec:
  replicas: 1
  selector: {matchLabels: {app: ${FP_BACKEND_NAME}}}
  template:
    metadata:
      labels: {app: ${FP_BACKEND_NAME}}
    spec:
      automountServiceAccountToken: false
      securityContext: {runAsNonRoot: true, seccompProfile: {type: RuntimeDefault}}
      containers:
        - name: server
          image: ${FP_TOOLS_IMAGE}
          imagePullPolicy: Never
          command: ["/demo-mcp-tools"]
          env: [{name: CLUSTER_NAME, value: ${FP_CLUSTER_NAME}}]
          ports: [{containerPort: 3001}]
          readinessProbe: {httpGet: {path: /healthz, port: 3001}}
          securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
          resources: {requests: {cpu: 10m, memory: 16Mi}, limits: {memory: 64Mi}}
---
apiVersion: v1
kind: Service
metadata:
  name: ${FP_BACKEND_NAME}
  namespace: ${FP_TOOLS_NAMESPACE}
spec:
  selector: {app: ${FP_BACKEND_NAME}}
  ports: [{port: 3001, targetPort: 3001}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: ${FP_GATEWAY_NAME}
  namespace: ${FP_TOOLS_NAMESPACE}
spec:
  gatewayClassName: kube-agentic-networking
  listeners:
    - name: mcp
      protocol: HTTPS
      port: ${FP_GATEWAY_PORT}
      allowedRoutes: {namespaces: {from: Same}}
---
apiVersion: agentic.networking.x-k8s.io/v0alpha0
kind: XBackend
metadata:
  name: ${FP_BACKEND_NAME}
  namespace: ${FP_TOOLS_NAMESPACE}
spec:
  mcp:
    serviceName: ${FP_BACKEND_NAME}
    port: 3001
    path: /mcp
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: ${FP_BACKEND_NAME}
  namespace: ${FP_TOOLS_NAMESPACE}
spec:
  parentRefs: [{name: ${FP_GATEWAY_NAME}}]
  rules:
    - matches: [{path: {type: PathPrefix, value: /mcp}}]
      backendRefs:
        - {group: agentic.networking.x-k8s.io, kind: XBackend, name: ${FP_BACKEND_NAME}}
---
YAML
sed -e "s|namespace: mcp-tools|namespace: ${FP_TOOLS_NAMESPACE}|" -e "s|name: fleet-tools$|name: ${FP_BACKEND_NAME}|" \
  "${FP_ROOT}/config/managed-cluster/default-deny-anchor.yaml"
