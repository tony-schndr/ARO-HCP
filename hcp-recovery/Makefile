SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec
HCP_RECOVERY_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))

-include ../setup-env.mk
-include ../setup-templatize-env.mk
-include ../.bingo/Variables.mk

# Image
ARO_HCP_REVISION ?= $(shell git rev-parse HEAD)
ARO_HCP_COMMIT_TIME ?= $(shell TZ=UTC git show -s --format=%cd --date=iso-strict-local ${ARO_HCP_REVISION})
HCP_RECOVERY_IMAGE_TAG ?= $(or $(IMAGE_TAG),$(shell DETECT_DIRTY_GIT_WORKTREE=${DETECT_DIRTY_GIT_WORKTREE} DEPLOY_ENV=${DEPLOY_ENV} ../generate-tag.sh))
HCP_RECOVERY_GENERATED_IMAGE_REPOSITORY = $(shell DEPLOY_ENV=${DEPLOY_ENV} BASELINE_REPO=${HCP_RECOVERY_IMAGE_REPOSITORY} ../generate-repo.sh)
HCP_RECOVERY_TAGGED_IMAGE ?= $(ARO_HCP_IMAGE_REGISTRY)/$(HCP_RECOVERY_GENERATED_IMAGE_REPOSITORY):$(HCP_RECOVERY_IMAGE_TAG)

HELMCHART_DIR = deploy
BINARY = bin/hcp-recovery
HCP_RECOVERY_SOURCES = $(shell find . -name '*.go' -o -name 'go.mod' -o -name 'go.sum')
LDFLAGS = -ldflags "\
	-X github.com/Azure/ARO-HCP/internal/version.CommitSHA=${ARO_HCP_REVISION}"

# K8S Codegen
KUBE_CODEGEN_SH=../hack/kube-codegen.sh

update-codegen:
	KUBE_CODEGEN_TAG=v0.35.3 KUBE_CODEGEN_SH=$(KUBE_CODEGEN_SH) ./hack/update-codegen.sh
	make -C .. fmt
.PHONY: update-codegen

# Default target
.DEFAULT_GOAL := build

build: $(BINARY)
.PHONY: build

$(BINARY): $(HCP_RECOVERY_SOURCES)
	go build $(LDFLAGS) -o $(BINARY) .

run:
	go run . controller --namespace ${NAMESPACE} --log-verbosity 2
.PHONY: run

image:
	rm -f $(BINARY)
	cd $(HCP_RECOVERY_DIR)/.. && \
	$(CONTAINER_ENGINE) build . --file hcp-recovery/Dockerfile \
		--build-arg PLATFORM=linux/amd64 \
		--build-arg ARO_HCP_REVISION=${ARO_HCP_REVISION} \
		--build-arg ARO_HCP_COMMIT_TIME=${ARO_HCP_COMMIT_TIME} \
		--tag ${HCP_RECOVERY_TAGGED_IMAGE}
.PHONY: image

build-and-push: image $(ORAS) $(YQ)
	$(acr-login)
	@LOCAL_DIGEST=$$($(CONTAINER_ENGINE) image inspect --format '{{.Id}}' ${HCP_RECOVERY_TAGGED_IMAGE} | sed 's/sha256://'); \
	REMOTE_DIGEST=$$($(ORAS) manifest fetch ${HCP_RECOVERY_TAGGED_IMAGE} 2>/dev/null | $(YQ) .config.digest | sed 's/sha256://'); \
	if [ "$$LOCAL_DIGEST" = "$$REMOTE_DIGEST" ]; then \
		echo "Image ${HCP_RECOVERY_TAGGED_IMAGE} already up to date in registry, skipping push"; \
	else \
		$(CONTAINER_ENGINE) push ${HCP_RECOVERY_TAGGED_IMAGE}; \
	fi
.PHONY: build-and-push

# Unix timestamp for this make run; computed once so multiple targets see the same value.
MAKE_RUN_TS := $(shell date +%s)
OVERRIDE_CONFIG_FILE ?= /tmp/hcp-recovery-override-config-$(MAKE_RUN_TS).yaml

record-override: $(YQ) $(ORAS)
	@az acr login --name ${ARO_HCP_IMAGE_ACR} --expose-token --output tsv --query accessToken | \
		$(ORAS) login ${ARO_HCP_IMAGE_REGISTRY} \
			--username 00000000-0000-0000-0000-000000000000 \
			--password-stdin

	@DIGEST=$$($(ORAS) manifest fetch --descriptor ${HCP_RECOVERY_TAGGED_IMAGE} | $(YQ) .digest); \
	$(YQ) eval -n "\
		.clouds.dev.environments.$(DEPLOY_ENV).defaults.hcpRecovery.image.registry = \"$(ARO_HCP_IMAGE_REGISTRY)\" | \
		.clouds.dev.environments.$(DEPLOY_ENV).defaults.hcpRecovery.image.repository = \"$(HCP_RECOVERY_GENERATED_IMAGE_REPOSITORY)\" | \
		.clouds.dev.environments.$(DEPLOY_ENV).defaults.hcpRecovery.image.digest = \"$$DIGEST\" \
	" > $(OVERRIDE_CONFIG_FILE)
.PHONY: record-override

# Dev work

record-latest-override: $(YQ)
	@../hack/record-latest-image-override.sh \
		"$(ARO_HCP_IMAGE_ACR)" "$(ARO_HCP_IMAGE_REGISTRY)" \
		"$(HCP_RECOVERY_IMAGE_REPOSITORY)" "$(DEPLOY_ENV)" \
		"hcpRecovery" "$(OVERRIDE_CONFIG_FILE)" "$(YQ)"
.PHONY: record-latest-override

deploy: build-and-push
	$(MAKE) record-override OVERRIDE_CONFIG_FILE=$(OVERRIDE_CONFIG_FILE)
	$(MAKE) -C .. pipeline/HcpRecovery DEPLOY_ENV=$(DEPLOY_ENV) OVERRIDE_CONFIG_FILE=$(OVERRIDE_CONFIG_FILE)
.PHONY: deploy
