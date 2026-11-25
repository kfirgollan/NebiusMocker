# 07 — State store

## Requirements

- Transactional multi-key writes. Create checks name uniqueness, reserves quota and inserts in one step.
- Indexed lookups: by ID, by (kind, parent, name), by parent (List), by referenced ID (dependents).
- Isolation and scoping by tenant, project and region, the way LocalStack scopes by account and region.
- Persistence that survives restarts when enabled.
- Fast reset for test suites: wipe all, or wipe one tenant.
- Export and import of snapshots, our version of LocalStack Cloud Pods.

## Interface

```go
package store

type Store interface {
    Update(ctx context.Context, fn func(tx Tx) error) error   // serializable read-write tx
    View(ctx context.Context, fn func(tx Tx) error) error     // read-only snapshot tx
    Snapshot(w io.Writer) error
    Restore(r io.Reader) error
    Reset(scope Scope) error
}

type Tx interface {
    Get(kind, id string) (*Record, error)
    Put(rec *Record) error                               // updates all indexes
    Delete(kind, id string) error
    ByName(kind, parentID, name string) (*Record, error)
    List(kind string, q Query) (Iterator, error)         // parent filter, label filter, cursor
    Dependents(id string) ([]Ref, error)                 // reverse reference index
}

type Record struct {
    Kind      string          // NID prefix, e.g. "vpcsubnet"
    ID        string
    ParentID  string
    Name      string
    Version   int64
    Message   []byte          // proto-marshaled resource (deterministic marshaling)
    Refs      []Ref           // outgoing references (subnet → network, instance → subnet, disk…)
    Scope     Scope           // tenant, project, region (derived from ancestry)
    Deleted   bool            // tombstone while DELETING / for soft-delete resources
}
```

Records are stored as **protobuf bytes**, not as Go structs. The store therefore stays independent of the many message
types, and snapshots are portable between versions as long as the protos stay wire-compatible.

### Collections besides resources

| Collection | Key | Purpose |
|---|---|---|
| `operations` | op ID | Operation messages and scheduler state |
| `idempotency` | (principal, method, key) | Response bytes + request hash, TTL |
| `quota_usage` | (scope, quota, region) | Counters |
| `tokens` | token ID | Revocations, refresh state |
| `locks` | resource ID | Active-operation lock ([05](05-api-conventions.md#concurrency)) |
| `journal` | sequence | Event log (optional; for `ListOperationsByParent` and debugging) |

### References and integrity

Modules declare reference fields per kind, for example `vpcsubnet.spec.network_id → vpcnetwork` or
`computeinstance.spec.network_interfaces[*].subnet_id → vpcsubnet`. The framework then:

- checks on Create/Update that referenced IDs exist and sit in an allowed scope (same project, or same tenant);
- maintains a reverse index;
- refuses Delete while dependents exist (`FAILED_PRECONDITION` + `BadResourceState`), unless the module declares cascade
  semantics (for example, deleting an mk8s cluster cascades to its node groups, which is what the real service does).

References that cross a split-container boundary go through `peer.Directory` with the same checks, so there is no
cross-container transaction. That is acceptable for an emulator. A race between deleting a network in container A and
creating a subnet in container B is not a scenario we promise to handle.

## Backends

| Backend | Config | Notes |
|---|---|---|
| `memory` (default) | — | In-process maps with copy-on-write snapshots for `View`. Fastest. Lost on exit unless persistence is on. |
| `bolt` | `NEBIUS_MOCKER_PERSISTENCE=1`, `NEBIUS_MOCKER_DATA_DIR=/var/lib/nebius-mocker` | `go.etcd.io/bbolt` embedded, single file per instance. No CGO, so the images stay static. |
| (later) `sqlite` | | Only if a query need appears that bolt can't serve well |

With `PERSISTENCE=1` the memory backend can also be used in **snapshot-on-shutdown** mode, which writes a snapshot on
SIGTERM and restores on boot. This mirrors LocalStack's default persistence strategy and keeps the hot path in memory.

## Scoping in split deployments

Each container owns the state for the modules it runs. **There is no shared database across containers.** IAM state lives
in the container that runs `iam`. Other containers query it through `peer` (token validation, group membership, permits)
and cache the answers with a short TTL. That keeps the deployment story simple and true to the real system's boundaries.
See [10](10-deployment-topologies.md).

## Snapshots

- `GET /_mocker/state/snapshot` streams a versioned archive with one section per module (`{module}/{collection}` → proto records).
- `POST /_mocker/state/snapshot` restores it. Modules not running in this container are skipped with a warning.
- `nebius-mocker snapshot save|load` is a CLI wrapper.
- A typical CI use: build a "baseline" tenant once (projects, networks, SA keys) and load it before each test in milliseconds.
