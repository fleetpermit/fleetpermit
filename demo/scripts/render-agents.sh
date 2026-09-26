#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Prints the agent workloads: two identities (sre-agent and security-agent)
# whose SPIFFE X.509 credentials are issued by Kubernetes Pod Certificates.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

printf 'apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n' "${FP_AGENTS_NAMESPACE}"
for agent in sre-agent security-agent; do
cat <<YAML
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${agent}
  namespace: ${FP_AGENTS_NAMESPACE}
automountServiceAccountToken: false
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${agent}
  namespace: ${FP_AGENTS_NAMESPACE}
spec:
  replicas: 1
  selector: {matchLabels: {app: ${agent}}}
  template:
    metadata:
      labels: {app: ${agent}}
    spec:
      serviceAccountName: ${agent}
      automountServiceAccountToken: false
      securityContext: {runAsNonRoot: true, seccompProfile: {type: RuntimeDefault}}
      containers:
        - name: agent
          image: ${FP_PROBE_IMAGE}
          imagePullPolicy: Never
          command: ["/demo-probe", "-idle"]
          securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
          resources: {requests: {cpu: 5m, memory: 8Mi}, limits: {memory: 32Mi}}
          volumeMounts:
            - {name: identity, mountPath: /run/agent-identity-mtls, readOnly: true}
      volumes:
        - name: identity
          projected:
            sources:
              - clusterTrustBundle:
                  signerName: kube-agentic-networking.sigs.k8s.io/identity
                  labelSelector:
                    matchLabels:
                      kube-agentic-networking.sigs.k8s.io/canarying: live
                      kube-agentic-networking.sigs.k8s.io/workload-trust-domain: ${FP_TRUST_DOMAIN}
                      kube-agentic-networking.sigs.k8s.io/peer-trust-domain: ${FP_TRUST_DOMAIN}
                  path: ${FP_TRUST_DOMAIN}.trust-bundle.pem
              - podCertificate:
                  signerName: kube-agentic-networking.sigs.k8s.io/identity
                  keyType: ECDSAP256
                  credentialBundlePath: credential-bundle.pem
YAML
done
