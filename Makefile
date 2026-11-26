# cluster-vending. `make help` lists the targets.

SHELL := bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

MODULE   := github.com/yindia/cluster-vending
BIN_DIR  := bin
BINARY   := $(BIN_DIR)/cluster-vending
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS  := -s -w -X $(MODULE)/pkg/version.Version=$(VERSION) -X $(MODULE)/pkg/version.Commit=$(COMMIT)

# go.mod asks for a newer Go than some installs ship (conda-forge's, in a
# pixi shell, is older and sets GOTOOLCHAIN=local). With local, go refuses
# instead of fetching the toolchain go.mod names; let it fetch it. Any other
# setting (auto, go1.x.y, path) is kept.
ifeq ($(GOTOOLCHAIN),local)
export GOTOOLCHAIN := auto
endif

GOLANGCI_LINT_VERSION ?= v2.15.0
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || echo $(BIN_DIR)/golangci-lint)

KIND_CLUSTER ?= cluster-vending
KUBE_CONTEXT ?= kind-$(KIND_CLUSTER)

# The Pluto release the deprecated-API rules are taken from (make deprecation-rules).
PLUTO_VERSION ?= v5.24.5

# Buf breaking-change baseline, e.g. make breaking AGAINST='.git#branch=main'
AGAINST ?= .git\#branch=main

##@ General

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n"} \
	  /^[a-zA-Z0-9_-]+:.*##/ { printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2 } \
	  /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

.PHONY: all
all: generate controller-apis mocks fmt lint test build ## Generate, format, lint, test and build

.PHONY: check
check: lint vet test ## What CI runs: lint, vet and unit tests

##@ Protobuf

.PHONY: generate
generate: ## Generate Go and UI TypeScript from idl/ (buf, remote plugins)
	buf dep update
	buf generate

.PHONY: buf-lint
buf-lint: ## Lint and format-check the protos
	buf format --diff --exit-code
	buf lint

.PHONY: buf-format
buf-format: ## Format the protos in place
	buf format -w

.PHONY: breaking
breaking: ## Check the protos for breaking changes against AGAINST
	buf breaking --against '$(AGAINST)'

##@ Go

.PHONY: fmt
fmt: $(GOLANGCI_LINT) buf-format ## Format Go code (gofmt + goimports) and protos
	$(GOLANGCI_LINT) fmt ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: buf-lint go-lint ## Lint protos and Go code

.PHONY: go-lint
go-lint: $(GOLANGCI_LINT) ## Lint Go code with golangci-lint (.golangci.yml)
	$(GOLANGCI_LINT) run ./...

.PHONY: lint-fix
lint-fix: $(GOLANGCI_LINT) ## Lint Go code and apply automatic fixes
	$(GOLANGCI_LINT) run --fix ./...

.PHONY: mocks
mocks: ## Generate the repository mocks (go.uber.org/mock, pkg/database/repository/mock)
	go generate ./pkg/database/repository/...

.PHONY: controller-apis
controller-apis: ## Generate the deepcopy code, CRDs and RBAC of the controller's API (controller-gen, api/ and config/)
	go tool controller-gen object:headerFile=hack/boilerplate.go.txt paths=./api/...
	go tool controller-gen crd paths=./api/... output:crd:dir=config/crd/bases
	go tool controller-gen rbac:roleName=manager-role paths=./pkg/controller/... output:rbac:dir=config/rbac

.PHONY: deprecation-rules
deprecation-rules: ## Refresh the deprecated-API rules (pkg/deprecation/data/versions.yaml) from Pluto at PLUTO_VERSION
	{ printf '%s\n' \
	  '# Deprecated and removed API versions, by the release that deprecates and' \
	  '# removes them.' \
	  '#' \
	  '# Source:  https://github.com/FairwindsOps/pluto/blob/$(PLUTO_VERSION)/versions.yaml' \
	  '# License: Apache-2.0 (Copyright FairwindsOps Inc)' \
	  '# Refresh: make deprecation-rules PLUTO_VERSION=<tag>' \
	  '#' \
	  '# The content below this header is that file, unchanged. Do not edit it' \
	  '# here: pkg/deprecation reads it as it is.'; \
	  curl -fsSL https://raw.githubusercontent.com/FairwindsOps/pluto/$(PLUTO_VERSION)/versions.yaml; \
	} > pkg/deprecation/data/versions.yaml.new
	mv pkg/deprecation/data/versions.yaml.new pkg/deprecation/data/versions.yaml
	@echo "Now set DataSource in pkg/deprecation/deprecation.go to $(PLUTO_VERSION) and run: go test ./pkg/deprecation/ ./pkg/controller/"

.PHONY: tidy
tidy: ## Run go mod tidy
	go mod tidy

$(BIN_DIR)/golangci-lint:
	GOBIN=$(abspath $(BIN_DIR)) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

##@ CloudFormation (the IAM roles of a tenant account and of the management account; Python, requirements.txt)

PYTHON ?= python3

.PHONY: cf-requirements
cf-requirements: ## Install the Python tools of the cf-* targets (requirements.txt; a virtualenv is advised)
	$(PYTHON) -m pip install -r requirements.txt

.PHONY: cf-generate
cf-generate: ## Generate the tenant and management account role templates into pkg/cloudformation/templates (scripts/generate.py)
	$(PYTHON) scripts/generate.py

.PHONY: cf-lint
cf-lint: cf-requirements ## Lint the role templates with cfn-lint
	cfn-lint pkg/cloudformation/templates/*.yaml

.PHONY: cf-format-check
cf-format-check: cf-requirements ## Check scripts/*.py (generate.py, management.py) are black-formatted
	black --check scripts/*.py

##@ Test

.PHONY: test
test: ## Unit tests with the race detector (a fake API server, no cluster needed)
	# pkg/reconcile/cluster takes over 10 minutes under -race; the default per-package
	# timeout is 10m.
	go test -race -count=1 -timeout 30m ./...

.PHONY: cover
cover: ## Unit tests with a coverage report in bin/coverage.html
	@mkdir -p $(BIN_DIR)
	go test -race -count=1 -coverprofile=$(BIN_DIR)/coverage.out ./pkg/...
	go tool cover -func=$(BIN_DIR)/coverage.out | tail -1
	go tool cover -html=$(BIN_DIR)/coverage.out -o $(BIN_DIR)/coverage.html

# The end-to-end harness (test/e2e) is behind the e2e build tag, so make check,
# test and lint never build or run it.
.PHONY: e2e
e2e: ## End-to-end tests on a sandbox AWS account (CV_E2E_* variables; creates real resources, costs money, takes hours: docs/architecture.md)
	CV_E2E=1 go test -tags e2e ./test/e2e/... -v -count=1 -timeout 4h

.PHONY: e2e-lint
e2e-lint: $(GOLANGCI_LINT) ## Vet and lint the end-to-end harness (nothing is run)
	go vet -tags e2e ./test/e2e/...
	$(GOLANGCI_LINT) run --build-tags e2e ./test/e2e/...

##@ Local cluster

.PHONY: kind-up
kind-up: ## Create a kind cluster with Crossplane (hack/start.sh)
	KIND_CLUSTER=$(KIND_CLUSTER) ./hack/start.sh

.PHONY: kind-down
kind-down: ## Delete the kind cluster (hack/stop.sh)
	KIND_CLUSTER=$(KIND_CLUSTER) ./hack/stop.sh

.PHONY: managed
managed: ## List every Crossplane managed resource on the kind cluster
	kubectl --context $(KUBE_CONTEXT) get managed -A

##@ Build and run

.PHONY: build
build: ## Build bin/cluster-vending
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) .

.PHONY: run
LOCAL_CONFIG ?= hack/config.local.yaml
run: build ## Run the API using local config (OIDC off unless auth is configured)
	$(BINARY) serve --config "$(LOCAL_CONFIG)"

.PHONY: run-ui keycloak
run-ui: ## Run the UI with the same local auth mode as the API
	cd ui && CV_CONFIG="$(abspath $(LOCAL_CONFIG))" npm run dev

keycloak: ## Start/configure local Keycloak only; does not start API or UI
	bash hack/keycloak.sh start

.PHONY: run-controller
run-controller: build ## Run the controller against the kind cluster (the same file as run, LOCAL_CONFIG); AWS keys from the environment
	$(BINARY) controller --config "$(LOCAL_CONFIG)"

.PHONY: run-live
run-live: build ## Run the live reader against the kind cluster (the same file as run, LOCAL_CONFIG): node lists and totals read live
	$(BINARY) live --config "$(LOCAL_CONFIG)"

.PHONY: clean
clean: ## Remove build output
	go clean -testcache
	rm -f bin/cluster-vending bin/coverage.out bin/coverage.html

##@ Image and deploy manifests

# The image of the single binary (serve and controller). A placeholder
# registry: override it, e.g. make docker-build IMAGE=registry.example.com/cluster-vending
IMAGE    ?= ghcr.io/yindia/cluster-vending
TAG      ?= $(VERSION)
# The Go version of go.mod, for the build stage of the Dockerfile.
GO_IMAGE_VERSION ?= $(shell awk '$$1 == "go" { print $$2; exit }' go.mod)
# kustomize, or the one built into kubectl.
KUSTOMIZE ?= $(shell command -v kustomize >/dev/null 2>&1 && echo 'kustomize build' || echo 'kubectl kustomize')
# The server's production overlay requires login (config/server/auth);
# config/server/base alone runs auth mode none, for local development only.
DEPLOY_DIRS := config/default config/server/auth

.PHONY: docker-build
docker-build: ## Build the container image IMAGE:TAG (needs Docker; nothing is pushed)
	docker build --build-arg GO_VERSION=$(GO_IMAGE_VERSION) \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE):$(TAG) .

.PHONY: deploy-manifests
deploy-manifests: ## Render the controller's and the server's manifests into bin/deploy (nothing is applied)
	@rm -rf $(BIN_DIR)/deploy && mkdir -p $(BIN_DIR)/deploy
	@for dir in $(DEPLOY_DIRS); do \
	  out=$(BIN_DIR)/deploy/$$(echo $$dir | tr '/' '-').yaml; \
	  $(KUSTOMIZE) $$dir | sed 's|ghcr.io/yindia/cluster-vending:latest|$(IMAGE):$(TAG)|' > $$out; \
	  echo "$$dir -> $$out"; \
	done

##@ Helm chart (charts/cluster-vending)

# The console's image (ui/Dockerfile, Next.js standalone), tagged like IMAGE.
UI_IMAGE ?= ghcr.io/yindia/cluster-vending-ui
HELM_CHART   := charts/cluster-vending
HELM_RELEASE ?= cluster-vending
HELM_NAMESPACE ?= cluster-vending-system
# lint and template need the controller's required value (a placeholder) and
# the API's login (ci/auth-values.yaml: the chart refuses an API without it).
HELM_LINT_SET := --set aws.managementRoleArn=arn:aws:iam::111122223333:role/crossplane-provider-aws -f $(HELM_CHART)/ci/auth-values.yaml
# A second kind cluster for the chart, never the development one (KIND_CLUSTER).
HELM_KIND_CLUSTER ?= cv-helm

.PHONY: docker-build-ui
docker-build-ui: ## Build the console image UI_IMAGE:TAG from ui/ (needs Docker; nothing is pushed)
	docker build -t $(UI_IMAGE):$(TAG) ui

.PHONY: helm-sync
helm-sync: ## Copy the RBAC and the alerts of config/ into the chart (files/); config/helm_chart_test.go checks the copies
	@mkdir -p $(HELM_CHART)/files/rbac
	cp config/rbac/role.yaml config/rbac/platform_role.yaml config/rbac/kubeconfig_reader_role.yaml \
	  config/rbac/usage_history_writer_role.yaml config/rbac/leader_election_role.yaml \
	  config/rbac/cluster_*_role.yaml config/rbac/network_*_role.yaml config/rbac/cloudaccount_*_role.yaml \
	  config/rbac/release_admin_role.yaml config/rbac/release_viewer_role.yaml $(HELM_CHART)/files/rbac/
	cp config/server/base/role.yaml $(HELM_CHART)/files/rbac/server_role.yaml
	cp config/live/role.yaml $(HELM_CHART)/files/rbac/live_role.yaml
	sed -n '/^spec:$$/,$$p' config/prometheus/rules.yaml | tail -n +2 | sed 's/^  //' > $(HELM_CHART)/files/prometheus-rules.yaml

.PHONY: prom-rules-check
prom-rules-check: ## Check the alerts with promtool (local, or the prom/prometheus image)
	@if command -v promtool >/dev/null; then promtool check rules $(HELM_CHART)/files/prometheus-rules.yaml; \
	else docker run --rm -v "$(CURDIR)/$(HELM_CHART)/files:/r:ro" --entrypoint promtool prom/prometheus:v3.5.0 check rules /r/prometheus-rules.yaml; fi

.PHONY: helm-lint
helm-lint: ## Lint the chart with the defaults and with each workload turned off
	helm lint --strict $(HELM_CHART) $(HELM_LINT_SET)
	@for w in server controller live ui; do \
	  echo "helm lint ($$w.enabled=false)"; \
	  helm lint --strict --quiet $(HELM_CHART) $(HELM_LINT_SET) --set $$w.enabled=false --set ui.apiBaseUrl=http://api.example:8080; \
	done

.PHONY: helm-template
helm-template: ## Render the chart into bin/helm/cluster-vending.yaml (nothing is applied)
	@mkdir -p $(BIN_DIR)/helm
	helm template $(HELM_RELEASE) $(HELM_CHART) -n $(HELM_NAMESPACE) $(HELM_LINT_SET) > $(BIN_DIR)/helm/cluster-vending.yaml
	@echo "$(HELM_CHART) -> $(BIN_DIR)/helm/cluster-vending.yaml"

.PHONY: kind-load
kind-load: ## Load IMAGE:TAG and UI_IMAGE:TAG into the kind cluster HELM_KIND_CLUSTER
	kind load docker-image $(IMAGE):$(TAG) $(UI_IMAGE):$(TAG) --name $(HELM_KIND_CLUSTER)

.PHONY: helm-install-kind
helm-install-kind: ## Install the chart on kind-HELM_KIND_CLUSTER with the loaded images (ci/kind-values.yaml; Crossplane first: make kind-up KIND_CLUSTER=cv-helm)
	helm upgrade --install $(HELM_RELEASE) $(HELM_CHART) --kube-context kind-$(HELM_KIND_CLUSTER) \
	  -n $(HELM_NAMESPACE) --create-namespace -f $(HELM_CHART)/ci/kind-values.yaml \
	  --set image.repository=$(IMAGE) --set image.tag=$(TAG) \
	  --set ui.image.repository=$(UI_IMAGE) --set ui.image.tag=$(TAG)
