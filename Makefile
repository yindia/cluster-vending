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

GOLANGCI_LINT_VERSION ?= v2.15.0
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || echo $(BIN_DIR)/golangci-lint)

KIND_CLUSTER ?= cluster-vending
KUBE_CONTEXT ?= kind-$(KIND_CLUSTER)

# Throwaway Postgres for `make test-postgres`.
PG_CONTAINER ?= cv-test-postgres
PG_PORT      ?= 55432
PG_DSN       ?= postgres://postgres:postgres@127.0.0.1:$(PG_PORT)/postgres?sslmode=disable

# Buf breaking-change baseline, e.g. make breaking AGAINST='.git#branch=main'
AGAINST ?= .git\#branch=main

##@ General

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n"} \
	  /^[a-zA-Z0-9_-]+:.*##/ { printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2 } \
	  /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

.PHONY: all
all: generate apis controller-apis mocks fmt lint test build ## Generate, format, lint, test and build

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

.PHONY: apis
apis: ## Generate the deepcopy code and CRDs of the platform's own kinds (controller-gen, pkg/apis)
	go tool controller-gen object paths=./pkg/apis/...
	go tool controller-gen crd paths=./pkg/apis/... output:crd:dir=pkg/apis/platform/crds

.PHONY: controller-apis
controller-apis: ## Generate the deepcopy code, CRDs and RBAC of the controller's API (controller-gen, controller/)
	go tool controller-gen object:headerFile=controller/hack/boilerplate.go.txt paths=./controller/api/...
	go tool controller-gen crd paths=./controller/api/... output:crd:dir=controller/config/crd/bases
	go tool controller-gen rbac:roleName=manager-role paths=./controller/internal/... output:rbac:dir=controller/config/rbac

.PHONY: tidy
tidy: ## Run go mod tidy
	go mod tidy

$(BIN_DIR)/golangci-lint:
	GOBIN=$(abspath $(BIN_DIR)) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

##@ Test

.PHONY: test
test: ## Unit tests with the race detector (SQLite, no cluster needed)
	go test -race -count=1 ./...

.PHONY: cover
cover: ## Unit tests with a coverage report in bin/coverage.html
	@mkdir -p $(BIN_DIR)
	go test -race -count=1 -coverprofile=$(BIN_DIR)/coverage.out ./pkg/...
	go tool cover -func=$(BIN_DIR)/coverage.out | tail -1
	go tool cover -html=$(BIN_DIR)/coverage.out -o $(BIN_DIR)/coverage.html

.PHONY: postgres-up
postgres-up: ## Start a throwaway Postgres in docker on PG_PORT
	@docker start $(PG_CONTAINER) >/dev/null 2>&1 || \
	  docker run -d --name $(PG_CONTAINER) -e POSTGRES_PASSWORD=postgres -p $(PG_PORT):5432 postgres:17 >/dev/null
	@for i in $$(seq 1 30); do \
	  docker exec $(PG_CONTAINER) pg_isready -U postgres >/dev/null 2>&1 && exit 0; sleep 1; \
	done; echo "postgres did not become ready" >&2; exit 1

.PHONY: postgres-down
postgres-down: ## Remove the throwaway Postgres
	docker rm -f $(PG_CONTAINER)

.PHONY: test-postgres
test-postgres: postgres-up ## Database tests against Postgres
	CV_TEST_POSTGRES_DSN='$(PG_DSN)' go test -race -count=1 ./pkg/database/... ./pkg/server/...

.PHONY: test-all
test-all: test test-postgres ## Unit tests on SQLite and the database tests on Postgres

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
run: build ## Run the server against the kind cluster (hack/config.local.yaml)
	$(BINARY) serve --config hack/config.local.yaml

.PHONY: clean
clean: ## Remove build output and the local database
	go clean -testcache
	rm -f bin/cluster-vending bin/coverage.out bin/coverage.html bin/cluster-vending.db

##@ Image and deploy manifests

# The image of the single binary (serve and controller). A placeholder
# registry: override it, e.g. make docker-build IMAGE=registry.example.com/cluster-vending
IMAGE    ?= ghcr.io/yindia/cluster-vending
TAG      ?= $(VERSION)
# The Go version of go.mod, for the build stage of the Dockerfile.
GO_IMAGE_VERSION ?= $(shell awk '$$1 == "go" { print $$2; exit }' go.mod)
# kustomize, or the one built into kubectl.
KUSTOMIZE ?= $(shell command -v kustomize >/dev/null 2>&1 && echo 'kustomize build' || echo 'kubectl kustomize')
DEPLOY_DIRS := controller/config/default deploy/server/overlays/kubernetes-store deploy/server/overlays/postgres

.PHONY: docker-build
docker-build: ## Build the container image IMAGE:TAG (needs Docker; nothing is pushed)
	docker build --build-arg GO_VERSION=$(GO_IMAGE_VERSION) \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE):$(TAG) .

.PHONY: deploy-manifests
deploy-manifests: ## Render the controller's and the server's manifests into bin/deploy (nothing is applied)
	@mkdir -p $(BIN_DIR)/deploy
	@for dir in $(DEPLOY_DIRS); do \
	  out=$(BIN_DIR)/deploy/$$(echo $$dir | tr '/' '-').yaml; \
	  $(KUSTOMIZE) $$dir | sed 's|ghcr.io/yindia/cluster-vending:latest|$(IMAGE):$(TAG)|' > $$out; \
	  echo "$$dir -> $$out"; \
	done
