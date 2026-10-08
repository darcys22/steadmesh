# Steadmesh. All cluster targets pin the kind context and
# never use the ambient kubectl context.
SHELL := /bin/bash
BIN := $(CURDIR)/bin
GO ?= go
KIND_CLUSTER ?= steadmesh
KCTX := kind-$(KIND_CLUSTER)
KUBECTL := kubectl --context $(KCTX)
TAG ?= dev
IMAGES := controller platform console seat-fake seat-claudecode seat-codex seat-pi seat-pi-browser fakes
ENVTEST_K8S ?= 1.37.0
TFPLUGINDOCS_VERSION := v0.25.0
export KUBEBUILDER_ASSETS = $(shell $(BIN)/setup-envtest use $(ENVTEST_K8S) -p path --bin-dir $(BIN)/envtest 2>/dev/null)

.PHONY: all generate build lint test test-integration conformance conformance-images live-harnesses live-github e2e demo release e2e-reset quickstart-test images kind-up kind-down kind-load provider provider-docs orgctl live tools

all: generate build test

tools:
	GOBIN=$(BIN) $(GO) install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1
	GOBIN=$(BIN) $(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
	GOBIN=$(BIN) $(GO) install github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs@$(TFPLUGINDOCS_VERSION)

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

# Harness conformance with the real pinned CLIs (claude, codex, pi) against a
# scripted model through the real model forwarder: on this machine, using
# binaries fetched and digest-checked by build/harness-bins.sh, and inside
# the seat images with their Linux binaries.
conformance:
	build/harness-bins.sh
	$(GO) test -count=1 -tags integration -run ModelConformance ./harnesses/...

CONFORMANCE_ARCH ?= $(shell $(GO) env GOARCH)
conformance-images:
	@mkdir -p out/conformance
	set -e; for h in claudecode:claude:seat-claudecode codex:codex:seat-codex pi:pi:seat-pi; do \
	  IFS=: read pkg bin img <<<"$$h"; \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$(CONFORMANCE_ARCH) $(GO) test -c -tags integration -o out/conformance/$$pkg.test ./harnesses/$$pkg; \
	  echo "==> $$img"; \
	  docker run --rm -v $(CURDIR)/out/conformance:/t:ro -e STEADMESH_$$(echo $$bin | tr a-z A-Z)_BIN=$$bin \
	    --entrypoint /t/$$pkg.test steadmesh/$$img:$(TAG) -test.run ModelConformance -test.count=1; \
	done

provider:
	$(GO) build -o $(BIN)/terraform-provider-steadmesh ./cmd/terraform-provider-steadmesh

# Terraform Registry documentation for the provider: provider/docs is rendered
# from the schema descriptions, provider/docs-templates and
# provider/docs-examples. The release publishes it to the provider repository.
provider-docs: $(BIN)/tfplugindocs
	$(BIN)/tfplugindocs generate --provider-dir cmd/terraform-provider-steadmesh --provider-name steadmesh \
		--rendered-provider-name Steadmesh --rendered-website-dir ../../provider/docs \
		--examples-dir ../../provider/docs-examples --website-source-dir ../../provider/docs-templates

$(BIN)/tfplugindocs:
	GOBIN=$(BIN) $(GO) install github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs@$(TFPLUGINDOCS_VERSION)

orgctl:
	$(GO) build -o $(BIN)/orgctl ./cmd/orgctl

images:
	build/build.sh $(IMAGES)

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
# Credentials for the live targets and the live demo come from the
# environment, or from .env in the repository root (ignored by git), which is
# sourced by the shell so quoted values work: KEY=value or export KEY=value.
LOADENV := set -a; if [ -f .env ]; then . ./.env; fi; set +a;

e2e: images provider orgctl
	$(MAKE) e2e-reset
	$(MAKE) kind-up kind-load
	KIND_CONTEXT=$(KCTX) $(GO) test -count=1 -tags e2e -run TestE2E -timeout 55m ./tests/e2e/...

# The completion demo (examples/mixed-harness) on a fresh cluster: Claude
# Code, Codex and Pi seats collaborate, with and without work items, through
# a Linear outage and a key rotation. DEMO_MODE=fakes (default) uses the
# model and GitHub fakes; DEMO_MODE=live the real services. The run records
# docs/demo/<mode>-results.json and renders docs/demo-results.html.
DEMO_MODE ?= fakes
demo: images provider orgctl
	$(MAKE) e2e-reset
	$(MAKE) kind-up kind-load
	$(LOADENV) DEMO_MODE=$(DEMO_MODE) KIND_CONTEXT=$(KCTX) $(GO) test -count=1 -tags e2e -run TestDemo -v -timeout 60m ./tests/e2e/...

# Cut a release: bump the minor version from the latest tag (BUMP=patch|major,
# or VERSION=X.Y.Z), pin the quickstart and provider docs, commit, tag and
# push main and the tag, which starts the release workflow. Asks before
# pushing unless YES=1; DRY_RUN=1 only shows what would change. See
# hack/release.sh.
release:
	hack/release.sh

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
	$(LOADENV) KIND_CONTEXT=$(KCTX) $(GO) test -count=1 -tags live -timeout 60m ./tests/live/...

# The github connector and sandbox credential flow on a real repository:
# GITHUB_TOKEN and GITHUB_TEST_REPO (see tests/live/README.md).
live-github:
	$(LOADENV) $(GO) test -count=1 -tags live -run LiveGitHub -v -timeout 10m ./tests/live/...

# Each real harness CLI on its real model endpoint, without a cluster:
# ANTHROPIC_API_KEY, OPENAI_API_KEY and/or SELFHOSTED_API_KEY (see tests/live/README.md).
live-harnesses:
	build/harness-bins.sh
	$(LOADENV) $(GO) test -count=1 -tags live -run LiveHarnesses -v -timeout 30m ./tests/live/...
