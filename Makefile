# NebiusMocker developer workflow. There is no CI budget: `make check` is the
# gate, run automatically by the pre-commit hook (install with `make hooks`).

GO        ?= go
LINT      := $(GO) tool -modfile=tools/go.mod golangci-lint
BIN_DIR   := bin

.PHONY: all build fmt lint test e2e e2e-storage e2e-images check hooks run tidy clean

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
	$(GO) test -tags e2e -count=1 -v ./test/terraform/... ./test/internal/nebiuscli/...

# RustFS containers started by test/rustfsfixture carry this label.
RUSTFS_LABEL := nebius-mocker.test=rustfs
RUSTFS_CLEANUP := docker ps -aq --filter label=$(RUSTFS_LABEL) | xargs docker rm -f -v >/dev/null 2>&1 || true

e2e-storage: ## RustFS-backed tests (needs Docker); RustFS is started and removed by the tests
	@$(RUSTFS_CLEANUP)
	@status=0; $(GO) test -tags e2e_storage -count=1 -v ./pkg/objstore/... ./test/... || status=$$?; \
	$(RUSTFS_CLEANUP); exit $$status

e2e-images: ## Compute images on RustFS with the Go SDK and the nebius CLI (needs Docker)
	@$(RUSTFS_CLEANUP)
	@status=0; $(GO) test -tags e2e_images -count=1 -v ./test/compute/... || status=$$?; \
	$(RUSTFS_CLEANUP); exit $$status

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
