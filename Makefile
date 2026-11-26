# NebiusMocker developer workflow. There is no CI budget: `make check` is the
# gate, run automatically by the pre-commit hook (install with `make hooks`).

GO        ?= go
LINT      := $(GO) tool -modfile=tools/go.mod golangci-lint
BIN_DIR   := bin

.PHONY: all build fmt lint test e2e e2e-storage e2e-images e2e-filesystems e2e-registry e2e-mk8s e2e-msp docker-image check hooks run tidy clean

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
	$(GO) test -tags e2e -count=1 -v ./test/terraform/... ./test/internal/nebiuscli/... ./test/kms/... ./test/cli/... ./test/computedisk/... ./test/computeinstance/... ./test/computegpucluster/... ./test/computenvl/... ./test/computefilesystem/... ./test/audit/...

# RustFS containers started by test/rustfsfixture carry this label. Running
# ones may belong to a concurrent suite (the fixture itself reaps containers of
# dead test processes), so only stopped leftovers are removed here.
RUSTFS_LABEL := nebius-mocker.test=rustfs
RUSTFS_CLEANUP := docker ps -aq --filter label=$(RUSTFS_LABEL) --filter status=exited --filter status=created | xargs docker rm -f -v >/dev/null 2>&1 || true
# Azurite (Azure Blob Storage emulator, a transfer source) from test/azuritefixture.
AZURITE_CLEANUP := docker ps -aq --filter label=nebius-mocker.test=azurite --filter status=exited --filter status=created | xargs docker rm -f -v >/dev/null 2>&1 || true

e2e-storage: ## RustFS/Azurite-backed tests (needs Docker); the containers are started and removed by the tests
	@$(RUSTFS_CLEANUP); $(AZURITE_CLEANUP)
	@status=0; $(GO) test -tags e2e_storage -count=1 -v ./pkg/objstore/... ./services/storage/... ./test/... || status=$$?; \
	$(RUSTFS_CLEANUP); $(AZURITE_CLEANUP); exit $$status

e2e-images: ## Compute images on RustFS with the Go SDK and the nebius CLI (needs Docker)
	@$(RUSTFS_CLEANUP)
	@status=0; $(GO) test -tags e2e_images -count=1 -v ./test/compute/... ./test/terraform/compute/... || status=$$?; \
	$(RUSTFS_CLEANUP); exit $$status

# e2e-filesystems mounts a shared filesystem with the official rclone image
# (FUSE in a container) through a filesystem bucket on the S3 front.
RCLONE_CLEANUP := docker ps -aq --filter label=nebius-mocker.test=rclone | xargs docker rm -f -v >/dev/null 2>&1 || true

e2e-filesystems: ## Shared filesystems on RustFS mounted with rclone via a filesystem bucket (needs Docker)
	@$(RUSTFS_CLEANUP); $(RCLONE_CLEANUP)
	@status=0; $(GO) test -tags e2e_filesystems -count=1 -v ./test/filesystems/... || status=$$?; \
	$(RCLONE_CLEANUP); $(RUSTFS_CLEANUP); exit $$status

IMAGE ?= nebius-mocker:dev

docker-image: ## Build the nebius-mocker container image ($(IMAGE)) for the local Docker daemon's architecture
	@arch=$$(docker version --format '{{.Server.Arch}}') && \
	CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -o $(BIN_DIR)/linux/nebius-mocker ./cmd/nebius-mocker && \
	docker build --quiet --tag $(IMAGE) --file deploy/docker/Dockerfile $(BIN_DIR)/linux >/dev/null && \
	echo "built $(IMAGE) (linux/$$arch)"

# e2e-registry builds the mocker image, starts a throwaway Harbor
# (deploy/harbor/harbor.sh), runs the CLI/docker/SDK tests against the mocker
# running as a container (the documented setup), and always removes it all.
REGISTRY_CLEANUP := docker ps -aq --filter label=nebius-mocker.test=registry | xargs docker rm -f -v >/dev/null 2>&1 || true

e2e-registry: ## Harbor-backed registry tests (needs Docker): CLI login + credential helper, docker push/pull, SDK artifacts
	@dir=$$(mktemp -d "$${TMPDIR:-/tmp}/nebius-mocker-harbor.XXXXXX"); image=nebius-mocker:e2e-$$$$; \
	trap './deploy/harbor/harbor.sh down "'"$$dir"'"; $(REGISTRY_CLEANUP); docker rmi -f '"$$image"' >/dev/null 2>&1 || true' EXIT INT TERM; \
	$(MAKE) --no-print-directory docker-image IMAGE=$$image && \
	./deploy/harbor/harbor.sh up "$$dir" && . "$$dir/env" && \
	NEBIUS_MOCKER_E2E_IMAGE=$$image $(GO) test -tags e2e_registry -count=1 -v ./test/registry/...

# e2e-mk8s runs Managed Kubernetes on the k3s backend: the in-process mocker
# starts real k3s servers and agents in Docker; the user's tools (nebius CLI,
# kubectl) run in a Linux container that trusts the mocker's TLS CA.
# deploy/k3s/reap.sh removes k3s leftovers of mockers that are gone.
MK8S_CLEANUP := docker ps -aq --filter label=nebius-mocker.test=mk8s | xargs docker rm -f -v >/dev/null 2>&1 || true; ./deploy/k3s/reap.sh

e2e-mk8s: ## Managed Kubernetes on k3s (needs Docker + terraform): Terraform, get-credentials, kubectl, IAM groups, capacity
	@$(MK8S_CLEANUP)
	@status=0; $(GO) test -tags e2e_mk8s -count=1 -timeout 60m -v ./test/mk8s/... || status=$$?; \
	$(MK8S_CLEANUP); exit $$status

# e2e-msp runs Managed PostgreSQL and Managed MLflow on the docker backends:
# the in-process mocker starts real PostgreSQL, PgBouncer and MLflow
# containers, and the user's tools (psql, nebius CLI, MLflow client, AWS CLI)
# reach them through the published endpoints with the mocker's CA. The two
# backend packages' own e2e_msp tests run first.
MSP_CLEANUP := for l in nebius-mocker.test=msp nebius-mocker.backend=msp-postgresql nebius-mocker.backend=msp-mlflow; do docker ps -aq --filter label=$$l | xargs docker rm -f -v >/dev/null 2>&1 || true; done

e2e-msp: ## Managed PostgreSQL + MLflow on Docker (needs Docker + terraform): Terraform, psql, CLI, MLflow client, S3
	@$(MSP_CLEANUP)
	@status=0; $(GO) test -tags e2e_msp -count=1 -timeout 60m -v ./services/msppostgresql/backend/... ./services/mspmlflow/backend/... ./test/msp/... || status=$$?; \
	$(MSP_CLEANUP); exit $$status

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
