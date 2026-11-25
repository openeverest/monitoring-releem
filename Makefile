## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

# CONTAINER_TOOL defines the container tool to be used for building images.
CONTAINER_TOOL ?= docker

# Image URL to use for all building/pushing image targets.
IMG ?= ghcr.io/openeverest/monitoring-releem-dev:latest
_IMG_REPO = $(firstword $(subst :, ,$(IMG)))
_IMG_TAG  = $(lastword $(subst :, ,$(IMG)))

CHART_DIR ?= charts/monitoring-releem
RELEASE_NAMESPACE ?= everest-system

GOLANGCI_LINT_VERSION ?= v2.14.0

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: lint
lint: ## Run golangci-lint.
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

.PHONY: tidy-check
tidy-check: ## Verify go.mod and go.sum are tidy.
	go mod tidy
	git diff --exit-code -- go.mod go.sum

# Files make generate writes. classgen writes the stamp into Chart.yaml and
# the helper template the MonitoringClass includes to carry it.
GENERATED_FILES = $(CHART_DIR)/Chart.yaml $(CHART_DIR)/templates/_openeverest_contract.tpl

.PHONY: generate
generate: ## Stamp the chart with the OpenEverest contract version of the core in go.mod.
	go tool classgen stamp --chart-dir $(CHART_DIR)

.PHONY: verify
verify: generate ## Fail when generated files are stale or the chart's objects lack the stamp.
	@if [ -n "$$(git status --porcelain -- $(GENERATED_FILES))" ]; then \
		echo "Generated files are out of date: run 'make generate' and commit the result."; \
		git status --short -- $(GENERATED_FILES); \
		git --no-pager diff -- $(GENERATED_FILES); \
		exit 1; \
	fi
	go tool classgen verify --chart-dir $(CHART_DIR)

.PHONY: run
run: ## Run the controller against the current kubeconfig.
	go run ./cmd/controller

##@ Testing

.PHONY: test-unit
test-unit: ## Run unit tests.
	go test -race -coverprofile=coverage.out ./...

##@ Build

.PHONY: build
build: $(LOCALBIN) ## Build the controller binary.
	CGO_ENABLED=0 go build -o $(LOCALBIN)/controller ./cmd/controller

.PHONY: docker-build
docker-build: ## Build the controller image.
	$(CONTAINER_TOOL) build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push the controller image.
	$(CONTAINER_TOOL) push $(IMG)

##@ Helm

.PHONY: helm-lint
helm-lint: ## Lint the chart.
	helm lint $(CHART_DIR)

.PHONY: helm-template
helm-template: ## Render the chart.
	helm template monitoring-releem $(CHART_DIR) -n $(RELEASE_NAMESPACE)
