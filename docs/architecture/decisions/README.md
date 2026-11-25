# Architecture Decision Records

Short records of decisions that shape the codebase. Format: context → decision → consequences.
To change a decision, add a new ADR that supersedes the old one. Do not rewrite history.

| ADR | Title | Status |
|---|---|---|
| [0001](0001-grpc-single-edge-port.md) | Serve every service on one gRPC edge port | Accepted |
| [0002](0002-reuse-gosdk-protos.md) | Reuse `gosdk` generated types and server stubs | Accepted |
| [0003](0003-single-go-module.md) | Single Go module with public `pkg/` libraries | Accepted |
| [0004](0004-generic-resource-framework.md) | Generic, descriptor-driven resource framework | Accepted |
| [0005](0005-authz-audit-default.md) | Authorization defaults to `audit` mode | Accepted |
| [0006](0006-no-shared-db-across-containers.md) | No shared database across containers; peers talk through public APIs | Accepted |
| [0007](0007-terraform-first-milestone.md) | Unmodified Terraform provider as the first acceptance target | Accepted |
