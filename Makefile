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
generate: ## Generate Go code from idl/ (buf, remote plugins)
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

##@ CloudFormation (the IAM roles of a tenant account; Python, requirements.txt)

PYTHON ?= python3

.PHONY: cf-requirements
cf-requirements: ## Install the Python tools of the cf-* targets (requirements.txt; a virtualenv is advised)
	$(PYTHON) -m pip install -r requirements.txt

.PHONY: cf-generate
cf-generate: ## Generate the account role templates into pkg/cloudformation/templates (scripts/generate.py)
	$(PYTHON) scripts/generate.py

.PHONY: cf-lint
cf-lint: cf-requirements ## Lint the account role templates with cfn-lint
	cfn-lint pkg/cloudformation/templates/*.yaml

.PHONY: cf-format-check
cf-format-check: cf-requirements ## Check scripts/generate.py is black-formatted
	black --check scripts/generate.py

##@ Test

.PHONY: test
test: ## Unit tests with the race detector (a fake API server, no cluster needed)
	go test -race -count=1 ./...

.PHONY: cover
cover: ## Unit tests with a coverage report in bin/coverage.html
	@mkdir -p $(BIN_DIR)
	go test -race -count=1 -coverprofile=$(BIN_DIR)/coverage.out ./pkg/...
	go tool cover -func=$(BIN_DIR)/coverage.out | tail -1
	go tool cover -html=$(BIN_DIR)/coverage.out -o $(BIN_DIR)/coverage.html

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
run: build ## Run the API server against the kind cluster (hack/config.local.yaml); start run-controller first
	$(BINARY) serve --config hack/config.local.yaml

.PHONY: run-controller
run-controller: build ## Run the controller against the kind cluster (hack/controller.local.yaml); AWS keys from the environment
	$(BINARY) controller --config hack/controller.local.yaml

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
DEPLOY_DIRS := config/default config/server/base

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

##@ Demo

# The demo is five phase tapes (hack/demo/0*.tape), each recorded on its
# own into bin/demo/<run>-<phase>.gif/.mp4 and resumable: a phase done
# already is skipped, one run again continues the same run (DEMO_RUN, or
# the newest unfinished one). REAL AWS. hack/demo/README.md.
DEMO_TAPES := $(wildcard hack/demo/0*.tape) hack/demo/setup.tape

.PHONY: demo demo-validate demo-account demo-network demo-cluster demo-upgrade demo-cleanup demo-status demo-video
demo-validate: ## Parse every demo tape (vhs); nothing runs
	@for t in $(DEMO_TAPES); do vhs validate $$t || exit 1; done

demo: build demo-validate ## Record the whole demo, phase by phase (~90 min): stops at the first phase that fails and says which make demo-<phase> to run again
	@hack/demo/run.sh all

demo-account: build ## Demo phase 1: account create with its CloudFormation role, until verified (needs DEMO_ACCOUNT_ID and AWS credentials of that account)
	@hack/demo/run.sh account

demo-network: build ## Demo phase 2: network create, until READY
	@hack/demo/run.sh network

demo-cluster: build ## Demo phase 3: cluster create on 1.36, alpha channel, automatic, tier 2, until RUNNING
	@hack/demo/run.sh cluster

demo-upgrade: build ## Demo phase 4: the automatic upgrade to 1.37 (the controller needs automatic-upgrades: true)
	@hack/demo/run.sh upgrade

demo-cleanup: build ## Demo phase 5: delete the cluster, the network, the account and its role's stack (needs DEMO_ACCOUNT_ID and AWS credentials)
	@hack/demo/run.sh cleanup

demo-status: build ## The current demo run's state (bin/demo/<run>.env), which phases are done, and each piece now
	@hack/demo/run.sh status

demo-video: ## Join the current run's phase videos into bin/demo/<run>.mp4 (ffmpeg; skipped without it)
	@hack/demo/run.sh video
