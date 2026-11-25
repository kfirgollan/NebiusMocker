# NebiusMocker developer workflow. There is no CI budget: `make check` is the
# gate, run automatically by the pre-commit hook (install with `make hooks`).

GO        ?= go
LINT      := $(GO) tool -modfile=tools/go.mod golangci-lint
BIN_DIR   := bin

.PHONY: all build fmt lint test e2e check hooks run tidy clean

all: check

build: ## Build all binaries into bin/
	$(GO) build -o $(BIN_DIR)/ ./cmd/...

fmt: ## Format code (gofumpt + gci)
	$(LINT) fmt ./...

lint: ## Lint, including depguard/forbidigo layering rules
	$(LINT) run ./...

test: ## Unit, contract and architecture tests with the race detector
	$(GO) test -race -shuffle=on -count=1 ./...

e2e: ## Terraform end-to-end tests: real terraform + official nebius provider
	$(GO) test -tags e2e -count=1 -v ./test/terraform/...

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
