# Example deployment workflow (design §5.3, §15): foundation -> platform ->
# organisation, then a fresh readiness verification.
#
#   make plan       plan every stage (later stages need earlier ones applied)
#   make apply      plan and apply each stage in order, then `orgctl verify`
#   make status     show organisations, seats and readiness
#   make teardown   destroy organisation, platform, foundation (keeps the cluster)
#   make validate   terraform validate every root (no cluster access)
#
# Every stage is its own target. Every recipe line runs under
# `set -euo pipefail` (spelled out per line so that the GNU Make 3.81 shipped
# with macOS behaves like newer versions), so a failed stage stops the run
# with its exit code even when its output is piped to a log.
# Plans are saved to out/<stage>.tfplan (and rendered to out/<stage>.plan.txt);
# logs go to out/<stage>.<step>.log.
# The ambient kubectl context is never used: everything pins kind-steadmesh.
# ~/.terraformrc is never touched: TF_CLI_CONFIG_FILE points at a generated
# CLI config whose dev_overrides select the locally built provider.

SHELL := /bin/bash
.DELETE_ON_ERROR:

ROOT    := $(abspath $(CURDIR)/..)
BIN     := $(ROOT)/bin
OUT     := $(CURDIR)/out
CLUSTER := steadmesh
KCTX    := kind-$(CLUSTER)
ORG_NS  ?= steadmesh-example
KUBECTL := kubectl --context $(KCTX)
ORGCTL  := $(BIN)/orgctl
TF      ?= terraform
STAGES  := foundation platform organisation
STRICT  := set -euo pipefail;

export TF_CLI_CONFIG_FILE := $(OUT)/terraformrc
export TF_IN_AUTOMATION := 1
export TF_INPUT := 0

.PHONY: plan apply status teardown validate verify cluster provider orgctl tfrc clean wait-crds mirror \
	$(addprefix init-,$(STAGES)) $(addprefix plan-,$(STAGES)) \
	$(addprefix apply-,$(STAGES)) $(addprefix destroy-,$(STAGES)) $(addprefix validate-,$(STAGES))

plan: plan-foundation plan-platform plan-organisation

apply: apply-organisation verify

status: tfrc orgctl
	@$(STRICT) $(KUBECTL) get agentorganizations,agentseats -A -o wide 2>&1 | tee $(OUT)/status.kubectl.log
	@$(STRICT) $(ORGCTL) status --context $(KCTX) --namespace $(ORG_NS) 2>&1 | tee $(OUT)/status.orgctl.log

# A fresh readiness check after every apply, including no-change applies (§5.4, A25).
verify: tfrc orgctl
	@$(STRICT) $(ORGCTL) verify --context $(KCTX) --namespace $(ORG_NS) 2>&1 | tee $(OUT)/verify.log

teardown: destroy-foundation

validate: $(addprefix validate-,$(STAGES))

clean:
	rm -rf $(OUT)

# ---------------------------------------------------------------- prerequisites

cluster:
	@$(STRICT) if ! kind get clusters 2>/dev/null | grep -qx '$(CLUSTER)'; then \
	  kind create cluster --config $(ROOT)/hack/kind-config.yaml; \
	fi
	@$(STRICT) $(KUBECTL) cluster-info

provider:
	@$(STRICT) $(MAKE) --no-print-directory -C $(ROOT) provider

# Rebuilt on every use so status/verify never run a stale binary.
orgctl:
	@$(STRICT) $(MAKE) --no-print-directory -C $(ROOT) orgctl

# CLI config selecting the locally built provider. Generated; never ~/.terraformrc.
# dev_overrides decide which binary runs. Terraform 1.5 `init` still resolves
# overridden providers, so the same binary is also offered through a local
# filesystem mirror (unpacked layout) that only serves darcys22/steadmesh.
MIRROR      := $(OUT)/mirror
PLATFORM    := $(shell go env GOOS 2>/dev/null || uname -s | tr A-Z a-z)_$(shell go env GOARCH 2>/dev/null || uname -m)
DEV_VERSION := 0.1.0
MIRROR_PKG  := $(MIRROR)/registry.terraform.io/darcys22/steadmesh/$(DEV_VERSION)/$(PLATFORM)

tfrc:
	@$(STRICT) mkdir -p $(OUT)
	@$(STRICT) printf '%s\n' \
	  'provider_installation {' \
	  '  dev_overrides {' \
	  '    "darcys22/steadmesh" = "$(BIN)"' \
	  '  }' \
	  '  filesystem_mirror {' \
	  '    path    = "$(MIRROR)"' \
	  '    include = ["darcys22/steadmesh"]' \
	  '  }' \
	  '  direct {' \
	  '    exclude = ["darcys22/steadmesh"]' \
	  '  }' \
	  '}' > $(TF_CLI_CONFIG_FILE)

mirror: provider tfrc
	@$(STRICT) mkdir -p $(MIRROR_PKG)
	@$(STRICT) cp $(BIN)/terraform-provider-steadmesh $(MIRROR_PKG)/terraform-provider-steadmesh_v$(DEV_VERSION)

# ---------------------------------------------------------------- per stage

# `terraform init` installs the registry providers (kubernetes, helm, random);
# the steadmesh provider comes from dev_overrides, which init skips.
# The lock file is regenerated on each init: the locally rebuilt steadmesh
# provider changes checksum on every build (the lock file is not committed).
tf_init     = $(STRICT) rm -f $(1)/.terraform.lock.hcl; $(TF) -chdir=$(1) init -input=false -no-color 2>&1 | tee $(OUT)/$(1).init.log
tf_validate = $(STRICT) $(TF) -chdir=$(1) validate -no-color 2>&1 | tee $(OUT)/$(1).validate.log
tf_plan     = $(STRICT) $(TF) -chdir=$(1) plan -input=false -no-color -out=$(OUT)/$(1).tfplan 2>&1 | tee $(OUT)/$(1).plan.log
tf_show     = $(STRICT) $(TF) -chdir=$(1) show -no-color $(OUT)/$(1).tfplan > $(OUT)/$(1).plan.txt
tf_apply    = $(STRICT) $(TF) -chdir=$(1) apply -input=false -no-color $(OUT)/$(1).tfplan 2>&1 | tee $(OUT)/$(1).apply.log
tf_output   = $(STRICT) $(TF) -chdir=$(1) output -no-color > $(OUT)/$(1).outputs.txt
tf_destroy  = $(STRICT) $(TF) -chdir=$(1) destroy -auto-approve -input=false -no-color 2>&1 | tee $(OUT)/$(1).destroy.log

# $(1) stage, $(2) extra prerequisites, $(3) stage applied before it, $(4) stage destroyed before it
define stage
init-$(1): tfrc $(2)
	@$$(call tf_init,$(1))

validate-$(1): init-$(1) $(2)
	@$$(call tf_validate,$(1))

plan-$(1): cluster init-$(1) $(2)
	@$$(call tf_plan,$(1))
	@$$(call tf_show,$(1))

apply-$(1): cluster init-$(1) $(2) $(if $(3),apply-$(3))
	@$$(call tf_plan,$(1))
	@$$(call tf_show,$(1))
	@$$(call tf_apply,$(1))
	@$$(call tf_output,$(1))

destroy-$(1): tfrc init-$(1) $(2) $(if $(4),destroy-$(4))
	@$$(call tf_destroy,$(1))
endef

$(eval $(call stage,foundation,,,platform))
$(eval $(call stage,platform,,foundation,organisation))
$(eval $(call stage,organisation,mirror,platform,))

# The organisation stage runs only once the management API is served.
apply-organisation: wait-crds
wait-crds: apply-platform
	@$(STRICT) $(KUBECTL) wait --for=condition=Established --timeout=120s \
	  crd/agentorganizations.steadmesh.io crd/agentseats.steadmesh.io 2>&1 | tee $(OUT)/platform.crds.log

# ---------------------------------------------------------------- offline check

# Plan every root against a temporary envtest API server (CRDs installed, no
# controller), using a kubeconfig whose context is named kind-steadmesh. This
# exercises the provider's compile/plan path and the modules' typed output
# without touching any real cluster. Nothing is applied.
ENVTEST_K8S ?= 1.37.0
plan-envtest: mirror init-foundation init-platform init-organisation
	@$(STRICT) assets=$$($(BIN)/setup-envtest use $(ENVTEST_K8S) -p path --bin-dir $(BIN)/envtest); \
	cd $(ROOT) && KUBEBUILDER_ASSETS=$$assets go build -tags integration -o $(OUT)/envtestserver ./provider/internal/envtestserver; \
	KUBEBUILDER_ASSETS=$$assets $(OUT)/envtestserver -out $(OUT)/envtest.kubeconfig > $(OUT)/envtest.log 2>&1 & pid=$$!; \
	trap 'kill -INT $$pid; wait $$pid || true' EXIT; \
	for i in $$(seq 1 60); do grep -q ready $(OUT)/envtest.log && break; sleep 1; done; \
	grep -q ready $(OUT)/envtest.log || { cat $(OUT)/envtest.log; exit 1; }; \
	for s in $(STAGES); do \
	  $(TF) -chdir=$(CURDIR)/$$s plan -input=false -no-color -lock=false -refresh=false \
	    -var kubeconfig_path=$(OUT)/envtest.kubeconfig 2>&1 | tee $(OUT)/$$s.envtest-plan.log; \
	done
.PHONY: plan-envtest
