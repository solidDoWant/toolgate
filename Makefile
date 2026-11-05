SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -euc

PROJECT_DIR := $(shell dirname $(realpath $(firstword $(MAKEFILE_LIST))))
BIN_DIR := $(PROJECT_DIR)/bin
CHART_DIR := deploy/charts/toolgate

VERSION ?= 0.0.0-dev
LDFLAGS := -s -w -X github.com/solidDoWant/toolgate/internal/version.version=$(VERSION)

# The phase whose checklist `make acceptance` scores (docs/acceptance/phase-$(PHASE).md).
PHASE ?= 1

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: fmt
fmt: ## Format Go code (gofmt + goimports through golangci-lint).
	golangci-lint fmt ./...

.PHONY: lint
lint: ## Run every linter and check (golangci-lint, go mod tidy, shellcheck, Python syntax, chart lint), then fail if any failed.
	@failed=""; \
	golangci-lint run ./... || failed="$$failed golangci-lint"; \
	go mod tidy -diff || failed="$$failed go-mod-tidy"; \
	shellcheck $$(git ls-files --cached --others --exclude-standard -- '*.sh' 'scripts/agents/sched-status') || failed="$$failed shellcheck"; \
	./scripts/check-python.sh || failed="$$failed python"; \
	$(MAKE) --no-print-directory chart-lint || failed="$$failed chart-lint"; \
	if [ -n "$$failed" ]; then echo "lint failed:$$failed" >&2; exit 1; fi

.PHONY: lint-fix
lint-fix: ## Run golangci-lint and apply its fixes.
	golangci-lint run --fix ./...

.PHONY: test
test: ## Run unit tests: Go (race detector), the agent scheduler and wait-ci tests, the test guard, the acceptance scoreboard, and the image build and release tests.
	go test -race -count=1 ./...
	./scripts/build-images_test.sh
	./scripts/release_test.sh
	./scripts/agents/scheduler_test.py
	./scripts/agents/wait-ci_test.sh
	./scripts/ci/test-guard_test.py
	./scripts/acceptance-scoreboard_test.py

.PHONY: test-guard
test-guard: ## Fail if HEAD deletes or skips an [AC-N-nn] scenario test since its merge base with BASE (default origin/master; CI passes the PR labels).
	./scripts/ci/test-guard.py --base $(or $(BASE),origin/master)

.PHONY: test-integration
test-integration: ## Run the integration-tagged tests (PostgreSQL with TLS client certificates and S3 in Docker, a fake OIDC provider and an envtest API server in process; they skip cleanly without Docker or KUBEBUILDER_ASSETS).
	timeout 30m go test -race -count=1 -tags integration -timeout 25m ./...

.PHONY: test-e2e
test-e2e: ## Run the e2e-tagged tests on a new kind cluster (Flux, PostgreSQL, S3, the chart with a fresh gateway image, the fake IdP and the test MCP server), then delete it.
	./scripts/e2e/run.sh ./scripts/test-e2e.sh

.PHONY: acceptance
acceptance: ## Run every [AC-…] test on a new kind cluster (as test-e2e) and print the scoreboard for docs/acceptance/phase-$(PHASE).md (written to build/acceptance/).
	PHASE=$(PHASE) ./scripts/e2e/run.sh ./scripts/acceptance.sh

.PHONY: docs-check
docs-check: ## Check the human-facing Markdown: links and anchors (lychee, offline), agent-prose tells, every docs/ page listed in docs/README.md.
	./scripts/docs/check.sh

.PHONY: docs-style-report
docs-style-report: ## Print the advisory style counts for the human-facing pages changed since origin/master. Never fails.
	./scripts/docs/style_report.sh

.PHONY: vulncheck
vulncheck: ## Report known vulnerabilities in Go dependencies and the standard library.
	govulncheck ./...

##@ Code Generation

.PHONY: generate
generate: ## Regenerate committed generated code (CRDs and deepcopy with controller-gen, queries with sqlc) for the inputs that exist.
	./scripts/generate.sh

.PHONY: generate-docs
generate-docs: ## Regenerate the gateway configuration reference (docs/admin/configuration.md) from the doc comments in internal/config, and the chart values reference (docs/admin/chart-values.md) from values.yaml.
	go run ./internal/config/docgen internal/config/config.go docs/admin/configuration.md
	./scripts/chart-values-docs.sh

.PHONY: check-generate
check-generate: ## Fail if `make generate` changes the tree (committed generated code is stale).
	./scripts/check-generate.sh

##@ Build

.PHONY: build
build: ## Build bin/toolgate.
	@mkdir -p "$(BIN_DIR)"
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o "$(BIN_DIR)/toolgate" ./cmd/toolgate

.PHONY: build-images
build-images: ## Build the gateway image ghcr.io/soliddowant/toolgate:$(VERSION) for linux/amd64 and arm64 (Nix dockerTools), check each binary with govulncheck -mode=binary, and load this machine's image into Docker. PUSH_ALL=true also pushes (releases only).
	VERSION="$(VERSION)" PUSH_ALL="$(or $(PUSH_ALL),false)" ./scripts/build-images.sh

.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf "$(BIN_DIR)" build

##@ Helm

.PHONY: chart-lint
chart-lint: ## Lint the Helm chart without a cluster: helm lint, the rendered-manifest tests in test/chart, and a check that docs/admin/chart-values.md is current.
	./scripts/chart-lint.sh

##@ Release

.PHONY: release
release: ## Release VERSION (dry run unless PUSH_ALL=true): build the image, chart and CLI binaries; with PUSH_ALL=true tag, push the tag and create the GitHub release with the CHANGELOG section as notes (the tag's workflow pushes the image and chart).
	VERSION="$(VERSION)" PUSH_ALL="$(or $(PUSH_ALL),false)" ./scripts/release.sh

##@ Dependencies

.PHONY: update-dependencies
update-dependencies: ## Update the Nix flake inputs and Go modules.
	nix flake update
	go get -u ./...
	go mod tidy
