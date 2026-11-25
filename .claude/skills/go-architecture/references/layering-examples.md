# Layering — worked examples

## Where does this code go?

| Code | Layer | Why |
|---|---|---|
| Parse `X-ResetMask` header | `pkg/interceptors` | Transport concern |
| Convert `CreateDiskRequest` → `domain.CreateDiskCmd` | `api` | Pure mapping |
| "Disk size must be a multiple of block size" | `domain` | Business rule |
| "Disk size must be ≤ 1024 bytes in name" (from `buf.validate`) | `pkg/interceptors` (protovalidate) | Declarative, generic |
| Map `errs.KindNotFound` → `codes.NotFound` + `ResourceNotFound` | `pkg/serviceerror` (called from `api`) | Transport |
| Look up disks by `(parent_id, name)` index | `dal` | Storage access |
| Choose the lowest free IP in a subnet | `domain` | Business rule (the *index scan* it needs is a `dal` method) |
| Retry a transaction on conflict | `dal` | Storage concern |
| Decide whether a delete must cascade | `domain` | Business rule |
| Advance a lifecycle step after a delay | `domain` (via `pkg/lifecycle` + `clock.Clock`) | Business rule |
| JSON for the admin API | `pkg/admin` | Transport |

## A full slice: compute disk Delete

```go
// api/disk_handler.go — view
func (h *DiskHandler) Delete(ctx context.Context, req *computepb.DeleteDiskRequest) (*commonpb.Operation, error) {
    op, err := h.svc.DeleteDisk(ctx, req.GetId())
    if err != nil {
        return nil, serviceerror.FromDomain(ctx, err)
    }
    return toOperationProto(op), nil
}
```

```go
// domain/disk.go — logic
func (s *DiskService) DeleteDisk(ctx context.Context, id string) (*operations.Op, error) {
    disk, err := s.disks.Get(ctx, id)
    if err != nil {
        return nil, fmt.Errorf("get disk %s: %w", id, err) // repo returns errs.NotFound(id)
    }
    if disk.GetSpec().GetForbidDeletion() {
        return nil, errs.BadState(id, "disk has forbid_deletion set")
    }
    if att := disk.GetStatus().GetReadWriteAttachment(); att != "" {
        return nil, errs.BadState(id, "disk is attached to "+att)
    }
    return s.ops.Start(ctx, operations.Delete(disk, diskLifecycle))
}
```

```go
// dal/disk_repo.go — data access
type DiskRepo struct{ st store.Store }

var _ domain.DiskRepository = (*DiskRepo)(nil)

func (r *DiskRepo) Get(ctx context.Context, id string) (*computepb.Disk, error) {
    var out *computepb.Disk
    err := r.st.View(ctx, func(tx store.Tx) error {
        rec, err := tx.Get(kindDisk, id)
        if err != nil {
            return err // store returns errs.NotFound already
        }
        out = new(computepb.Disk)
        return proto.Unmarshal(rec.Message, out)
    })
    return out, err
}
```

```go
// domain/disk_test.go — unit test with a fake, no storage, no gRPC
func TestDeleteDisk_ForbidDeletion(t *testing.T) {
    repo := newFakeDiskRepo(diskWith(func(d *computepb.Disk) { d.Spec.ForbidDeletion = true }))
    svc := domain.NewDiskService(repo, fakeOps{}, clock.NewManual(t0))

    _, err := svc.DeleteDisk(t.Context(), "computedisk-1")

    if !errs.IsKind(err, errs.KindBadState) {
        t.Fatalf("want BadState, got %v", err)
    }
}
```

## Anti-patterns we reject in review

- **Handler calls repository**: `h.repo.Get(...)` inside `api` — bypasses domain rules and authz targeting.
- **Domain returns `status.Error`**: couples logic to gRPC; tests then assert on codes instead of meaning.
- **"Manager" god-object** holding every repo of an area: split by resource kind.
- **Interface beside its only implementation** in `dal`: move it to the consumer (`domain/ports.go`).
- **Cross-area import** `services/compute/domain` → `services/vpc/domain`: use a `SubnetLookup` port + `pkg/peer`.
- **Package-level `var db *bolt.DB`**: all state hangs off constructed structs.
