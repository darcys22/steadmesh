# Steadmesh. All cluster targets pin the kind context and
# never use the ambient kubectl context.
SHELL := /bin/bash
BIN := $(CURDIR)/bin
GO ?= go
KIND_CLUSTER ?= steadmesh
KCTX := kind-$(KIND_CLUSTER)
KUBECTL := kubectl --context $(KCTX)
TAG ?= dev
IMAGES := controller platform console seat-fake seat-claudecode fakes
ENVTEST_K8S ?= 1.37.0
export KUBEBUILDER_ASSETS = $(shell $(BIN)/setup-envtest use $(ENVTEST_K8S) -p path --bin-dir $(BIN)/envtest 2>/dev/null)

.PHONY: all generate build lint test test-integration e2e e2e-reset quickstart-test images kind-up kind-down kind-load provider orgctl live tools

all: generate build test

tools:
	GOBIN=$(BIN) $(GO) install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1
	GOBIN=$(BIN) $(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

generate:
	$(BIN)/controller-gen object paths=./pkg/spec/... paths=./api/...
	$(BIN)/controller-gen crd paths=./api/... output:crd:dir=charts/platform/crds

build:
	$(GO) build ./...
	@mkdir -p $(BIN)
	$(GO) build -o $(BIN)/ ./cmd/...

lint:
	$(GO) vet ./...
	@test -z "$$(gofmt -l $$(git ls-files '*.go' 2>/dev/null || find . -name '*.go' -not -path './bin/*'))" || (gofmt -l . ; exit 1)

# Unit tests: no external services.
test:
	$(GO) test -short ./...

# Integration tests: Postgres via testcontainers, controller via envtest,
# provider via terraform-plugin-testing.
test-integration:
	$(GO) test -count=1 -tags integration -timeout 20m ./...

provider:
	$(GO) build -o $(BIN)/terraform-provider-steadmesh ./cmd/terraform-provider-steadmesh

orgctl:
	$(GO) build -o $(BIN)/orgctl ./cmd/orgctl

images:
	for i in $(IMAGES); do docker build -f build/$$i.Dockerfile -t steadmesh/$$i:$(TAG) . || exit 1; done

kind-up:
	kind get clusters | grep -qx $(KIND_CLUSTER) || kind create cluster --config hack/kind-config.yaml
	$(KUBECTL) cluster-info

kind-load: images
	for i in $(IMAGES); do kind load docker-image --name $(KIND_CLUSTER) steadmesh/$$i:$(TAG) || exit 1; done

kind-down:
	kind delete cluster --name $(KIND_CLUSTER)

# End-to-end on kind with the fake harness and fake Slack/Linear. Every run
# starts from a fresh cluster: leftover platform records (the connector
# ledger, messages) would not match the freshly started fakes. The example
# Terraform state describes the deleted cluster, so it goes too. The cluster
# is left running afterwards for inspection.
e2e: images provider orgctl
	$(MAKE) e2e-reset
	$(MAKE) kind-up kind-load
	KIND_CONTEXT=$(KCTX) $(GO) test -count=1 -tags e2e -timeout 40m ./tests/e2e/...

# quickstart/ as a user runs it (terraform init -from-module, two stages), on a
# fresh kind cluster, with local builds standing in for the published release.
quickstart-test: images provider
	$(MAKE) e2e-reset
	$(MAKE) kind-up kind-load
	hack/quickstart-test.sh

e2e-reset:
	kind delete cluster --name $(KIND_CLUSTER)
	rm -f examples/*/terraform.tfstate examples/*/terraform.tfstate.backup

# Live tests against real Slack, Linear and Anthropic. Requires credentials (see tests/live/README.md).
live: kind-up kind-load provider orgctl
	KIND_CONTEXT=$(KCTX) $(GO) test -count=1 -tags live -timeout 60m ./tests/live/...
