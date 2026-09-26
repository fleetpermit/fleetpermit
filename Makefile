# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Every CI job calls a target in this file, so anything CI does can be run
# locally with the same command. Run `make help` for the list.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BIN_DIR := $(CURDIR)/bin
CONTROLLER_GEN_VERSION ?= v0.22.0
ENVTEST_K8S_VERSION ?= 1.35.0
ENVTEST_VERSION ?= release-0.25
GOVULNCHECK_VERSION ?= v1.1.4
BOM_VERSION ?= v0.8.0
KAN_VERSION ?= v0.2.0

CONTROLLER_GEN := $(BIN_DIR)/controller-gen
SETUP_ENVTEST := $(BIN_DIR)/setup-envtest

# Container engine: podman first, docker as a fallback.
CONTAINER_ENGINE ?= $(shell command -v podman >/dev/null 2>&1 && echo podman || echo docker)
IMAGE_REGISTRY ?= localhost/fleetpermit
IMAGE_TAG ?= dev
FP_CONTROLLER_IMAGE ?= $(IMAGE_REGISTRY)/fleetpermit-controller:$(IMAGE_TAG)
FP_TOOLS_IMAGE ?= $(IMAGE_REGISTRY)/demo-mcp-tools:$(IMAGE_TAG)
FP_PROBE_IMAGE ?= $(IMAGE_REGISTRY)/demo-probe:$(IMAGE_TAG)

##@ General

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n"} /^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Generate deepcopy code, CRDs and RBAC from the Go types.
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd
	$(CONTROLLER_GEN) rbac:roleName=fleetpermit-controller paths=./internal/controller/... output:rbac:artifacts:config=config/rbac
	cp config/crd/*.yaml charts/fleetpermit/crds/
	cp config/rbac/role.yaml charts/fleetpermit/files/role.yaml

.PHONY: build
build: ## Build all binaries into bin/.
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN_DIR)/fleetpermit-controller ./cmd/fleetpermit-controller
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN_DIR)/demo-mcp-tools ./demo/tools/mcp-server
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN_DIR)/demo-probe ./demo/tools/probe

.PHONY: images
images: ## Build the controller and demo images with podman (or docker).
	$(CONTAINER_ENGINE) build -q --build-arg VERSION=$(VERSION) -t $(FP_CONTROLLER_IMAGE) .
	$(CONTAINER_ENGINE) build -q --build-arg VERSION=$(VERSION) --build-arg CMD=demo/tools/mcp-server --build-arg BIN=demo-mcp-tools -t $(FP_TOOLS_IMAGE) .
	$(CONTAINER_ENGINE) build -q --build-arg VERSION=$(VERSION) --build-arg CMD=demo/tools/probe --build-arg BIN=demo-probe -t $(FP_PROBE_IMAGE) .
	@# Podman keeps each build stage as an untagged image (over 1 GB each); remove
	@# only this Dockerfile's leftover builder images so repeated builds do not
	@# fill the engine's disk. Other images are never touched.
	-@$(CONTAINER_ENGINE) image prune -f --filter label=io.github.fleetpermit.stage=builder >/dev/null

##@ Verification

.PHONY: verify
verify: verify-fmt verify-vet verify-generate verify-manifests verify-helm verify-scripts verify-secrets verify-headers ## Run every static check.

.PHONY: verify-fmt
GO_FILES = $$(git ls-files -co --exclude-standard '*.go' 2>/dev/null | grep -v '^\.work/' || find . -name '*.go' -not -path './.work/*')

verify-fmt: ## Fail if any Go file is not gofmt-formatted.
	@files="$(GO_FILES)"; [[ -n "$$files" ]] || { echo "no Go files found"; exit 1; }; \
	out="$$(gofmt -l $$files)"; \
	if [[ -n "$$out" ]]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

.PHONY: verify-vet
verify-vet: ## Run go vet.
	go vet ./...

.PHONY: verify-generate
verify-generate: generate ## Fail if generated files are stale.
	@git diff --exit-code -- api config charts/fleetpermit/crds charts/fleetpermit/files || (echo "run 'make generate' and commit the result"; exit 1)

.PHONY: verify-manifests
verify-manifests: ## Validate YAML manifests and samples parse.
	./hack/verify-manifests.sh

.PHONY: verify-helm
verify-helm: ## Lint and render the Helm chart.
	helm lint charts/fleetpermit
	helm template fleetpermit charts/fleetpermit --namespace fleetpermit-system >/dev/null

.PHONY: verify-scripts
verify-scripts: ## Syntax-check shell scripts.
	@for f in $$(find demo hack -name '*.sh'); do bash -n "$$f"; done

.PHONY: verify-secrets
verify-secrets: ## Scan tracked files for credentials, personal data and absolute home paths.
	./hack/check-secrets.sh

.PHONY: verify-headers
verify-headers: ## Check license headers on Go files.
	@files="$(GO_FILES)"; [[ -n "$$files" ]] || { echo "no Go files found"; exit 1; }; \
	missing="$$(grep -L 'Licensed under the Apache License' $$files || true)"; \
	if [[ -n "$$missing" ]]; then echo "missing license header:"; echo "$$missing"; exit 1; fi

.PHONY: vulncheck
vulncheck: ## Audit dependencies for known vulnerabilities (govulncheck).
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: sbom
sbom: ## Generate an SPDX SBOM with the Kubernetes SIG Release bom tool.
	mkdir -p dist
	go run sigs.k8s.io/bom/cmd/bom@$(BOM_VERSION) generate --format json -n https://github.com/fleetpermit/fleetpermit -d . -o dist/fleetpermit.spdx.json

##@ Tests

.PHONY: test
test: test-unit test-integration ## Run unit and integration tests.

TEST_OUT := $(CURDIR)/.work/test

.PHONY: test-unit
test-unit: ## Run unit tests with the race detector and record results and coverage.
	mkdir -p $(TEST_OUT)
	go test -race -count=1 -json -coverpkg=./internal/... -coverprofile=$(TEST_OUT)/cover-unit.out \
	  $$(go list ./... | grep -v /test/) > $(TEST_OUT)/unit.jsonl || \
	  { jq -r 'select(.Action=="output") | .Output' $(TEST_OUT)/unit.jsonl | grep -v '^=== ' | tail -80; exit 1; }
	@echo "unit: $$(grep -c '"Action":"pass","Package":"[^"]*","Test"' $(TEST_OUT)/unit.jsonl) tests passed"

FUZZTIME ?= 30s

.PHONY: fuzz
fuzz: ## Fuzz the renderer's injection defence and lease evaluation (FUZZTIME=30s each).
	go test ./internal/enforcement/agenticnetworking/ -run '^$$' -fuzz FuzzRenderNeverEmitsUnsafeCEL -fuzztime $(FUZZTIME)
	go test ./internal/lease/ -run '^$$' -fuzz FuzzEvaluateNeverWidens -fuzztime $(FUZZTIME)

.PHONY: test-integration
test-integration: $(SETUP_ENVTEST) ## Run integration tests against a real kube-apiserver (envtest).
	mkdir -p $(TEST_OUT)
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(BIN_DIR)/envtest -p path)" \
	  go test -count=1 -json -tags integration -coverpkg=./internal/... -coverprofile=$(TEST_OUT)/cover-integration.out \
	  ./test/integration/... > $(TEST_OUT)/integration.jsonl || \
	  { jq -r 'select(.Action=="output") | .Output' $(TEST_OUT)/integration.jsonl | grep -v '^=== ' | tail -80; exit 1; }
	@echo "integration: $$(grep -c '"Action":"pass","Package":"[^"]*","Test"' $(TEST_OUT)/integration.jsonl) tests passed"

.PHONY: test-e2e
test-e2e: ## Run the multi-cluster end-to-end scenarios against the lab (make demo-up first).
	./test/e2e/run.sh

.PHONY: conformance
conformance: ## Run the upstream kube-agentic-networking conformance suite against a lab cluster.
	./hack/conformance.sh

.PHONY: upstream-canary
upstream-canary: ## Compare pins with the latest upstream releases and test against the latest upstream CRD.
	./hack/upstream-canary.sh

.PHONY: benchmark
benchmark: $(SETUP_ENVTEST) ## Run the controller scale simulation and, if the lab is up, the real-cluster latency benchmark.
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(BIN_DIR)/envtest -p path)" \
	  FP_SCALE=1 go test -count=1 -tags integration -run TestScaleSimulation ./test/integration/... -v -timeout 30m
	./test/e2e/benchmark.sh

SITE_DIR ?= $(abspath $(CURDIR)/../fleetpermit.github.io)

.PHONY: results
results: ## Merge test outputs into test-results/results.json, docs/results.md, the README and the website data.
	FP_SITE_DIR="$$( [ -d "$(SITE_DIR)/.git" ] && echo "$(SITE_DIR)" )" go run ./hack/results

##@ Lab

.PHONY: demo-up
demo-up: ## Create the 1 hub + 3 managed cluster lab and install everything.
	./demo/scripts/lab-up.sh all

.PHONY: demo-run
demo-run: ## Run the narrated authorization demo against the lab.
	./demo/run.sh

.PHONY: demo-videos
demo-videos: ## Record the three demo videos from real runs (asciinema, agg, ffmpeg; lab required).
	./hack/record-demos.sh

.PHONY: demo-down
demo-down: ## Delete the lab clusters (only those this lab created).
	./demo/scripts/lab-down.sh

##@ Tools

$(CONTROLLER_GEN):
	GOBIN=$(BIN_DIR) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

$(SETUP_ENVTEST):
	GOBIN=$(BIN_DIR) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)
