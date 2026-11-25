# NebiusMocker developer workflow. There is no CI budget: `make check` is the
# gate, run automatically by the pre-commit hook (install with `make hooks`).

GO        ?= go
LINT      := $(GO) tool -modfile=tools/go.mod golangci-lint
BIN_DIR   := bin

.PHONY: all build fmt lint test e2e e2e-storage e2e-images e2e-registry check hooks run tidy clean

all: check

build: ## Build all binaries into bin/
	$(GO) build -o $(BIN_DIR)/ ./cmd/...

fmt: ## Format code (gofumpt + gci)
	$(LINT) fmt ./...

lint: ## Lint, including depguard/forbidigo layering rules
	$(LINT) run ./...

test: ## Unit, contract and architecture tests with the race detector
	$(GO) test -race -shuffle=on -count=1 ./...

e2e: ## End-to-end tests: real terraform + official nebius provider, official nebius CLI
	$(GO) test -tags e2e -count=1 -v ./test/terraform/... ./test/internal/nebiuscli/... ./test/kms/... ./test/cli/...

# RustFS containers started by test/rustfsfixture carry this label. Running
# ones may belong to a concurrent suite (the fixture itself reaps containers of
# dead test processes), so only stopped leftovers are removed here.
RUSTFS_LABEL := nebius-mocker.test=rustfs
RUSTFS_CLEANUP := docker ps -aq --filter label=$(RUSTFS_LABEL) --filter status=exited --filter status=created | xargs docker rm -f -v >/dev/null 2>&1 || true

e2e-storage: ## RustFS-backed tests (needs Docker); RustFS is started and removed by the tests
	@$(RUSTFS_CLEANUP)
	@status=0; $(GO) test -tags e2e_storage -count=1 -v ./pkg/objstore/... ./test/... || status=$$?; \
	$(RUSTFS_CLEANUP); exit $$status

e2e-images: ## Compute images on RustFS with the Go SDK and the nebius CLI (needs Docker)
	@$(RUSTFS_CLEANUP)
	@status=0; $(GO) test -tags e2e_images -count=1 -v ./test/compute/... ./test/terraform/compute/... || status=$$?; \
	$(RUSTFS_CLEANUP); exit $$status

# e2e-registry starts a throwaway Harbor (test/registry/harbor/harbor.sh), runs
# the docker/CLI/SDK tests against it and always removes it again.
REGISTRY_CLEANUP := docker ps -aq --filter label=nebius-mocker.test=registry | xargs docker rm -f -v >/dev/null 2>&1 || true

e2e-registry: ## Harbor-backed registry tests (needs Docker): nebius CLI login, docker push/pull, SDK artifacts
	@dir=$$(mktemp -d "$${TMPDIR:-/tmp}/nebius-mocker-harbor.XXXXXX"); \
	trap './test/registry/harbor/harbor.sh down "'"$$dir"'"; $(REGISTRY_CLEANUP)' EXIT INT TERM; \
	./test/registry/harbor/harbor.sh up "$$dir" && . "$$dir/env" && \
	$(GO) test -tags e2e_registry -count=1 -v ./test/registry/...

check: lint test ## Everything the pre-commit hook runs

hooks: ## Install the git pre-commit hook
	git config core.hooksPath .githooks
	@echo "pre-commit hook installed (.githooks/pre-commit)"

run: ## Run the all-in-one server on :4566
	$(GO) run ./cmd/nebius-mocker

tidy:
	$(GO) mod tidy
	cd tools && $(GO) mod tidy

clean:
	rm -rf $(BIN_DIR)
