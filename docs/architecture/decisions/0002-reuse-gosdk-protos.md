# ADR-0002: Reuse `gosdk` generated types and server stubs

**Context.** `github.com/nebius/gosdk/proto/...` (MIT) contains the Go message types **and** the `*_grpc.pb.go` server
interfaces (`DiskServiceServer`, `RegisterDiskServiceServer`) for every service in `nebius/api`. gosdk also ships the
reset-mask implementation (`proto/fieldmask/protobuf.PatchWithResetMask`) and the NID parser (`nid`).

**Decision.** Import gosdk for protos, stubs, masks and NIDs. Do not vendor or regenerate the protos. The emulated API
version is whatever gosdk version is pinned in `go.mod`.

**Consequences.** The server and the client use the same reset-mask code, so they agree on Update semantics.
Keeping up with upstream is a dependency bump. If gosdk ever stopped publishing server stubs, we would generate them
from `nebius/api` with `buf` (a fallback already documented in `tools/apigen`).
