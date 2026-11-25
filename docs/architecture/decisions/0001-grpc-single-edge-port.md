# ADR-0001: Serve every service on one gRPC edge port

**Context.** Real Nebius exposes about 30 hostnames (`compute.api.nebius.cloud`, `cpl.iam.api.nebius.cloud`, …). LocalStack's
single edge port (4566) is a big part of why it is easy to use. gRPC routes by `/package.Service/Method`, and every Nebius
service name is globally unique. The gosdk resolver chain supports a `"*"` prefix resolver that maps every service ID to one
address (`gosdk/conn/resolvers.go: NewBasicResolver`), and the Terraform provider exposes it as `resolvers = {"*" = …}`.

**Decision.** One `grpc.Server` on `:4566` serves every enabled service. Routing ignores `:authority`. Per-service ports
and wildcard-TLS virtual hosting are optional extras.

**Consequences.** Client configuration is one resolver line plus `insecure`. `OperationService` must resolve operations
from any module, because clients call it on "the endpoint of the originating service", which is now the same address
for all services. Operation IDs therefore encode their owning module.
