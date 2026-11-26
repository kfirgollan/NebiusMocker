package domain

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// maxBatchGet is the most IDs BatchGet accepts (instance_service.proto).
const maxBatchGet = 1000

// InstanceDeps are the dependencies of InstanceService. All are required.
type InstanceDeps struct {
	Instances InstanceRepository
	// OpIndex records instance operations per project (ListOperationsByParent).
	OpIndex   OperationIndex
	Projects  Projects
	Platforms PlatformResolver
	Network   Networking
	// Disks validates managed disk specs with the DiskService rules.
	Disks *DiskService
	// GpuClusters is joined by instances with spec.gpu_cluster.
	GpuClusters GpuClusterMembership
	// NVLGroups is joined by instances with spec.nvl_instance_group_id.
	NVLGroups NVLMembership
	// Reservations draws GPUs from capacity block groups
	// (spec.reservation_policy, instance_reservation.go).
	Reservations Reservations
	// Filesystems records spec.filesystems attachments.
	Filesystems FilesystemAttachments
	// Nodes records the instances managed by mk8s node groups (nodes.go).
	Nodes NodeRepository
	Perms CallerPermissions
	// Quota admits instance creations, resource changes and the managed disks
	// they create.
	Quota QuotaGate
	Ops   Operations
	IDs   IDGenerator
	Clock clock.Clock
	// PublicIPReleaseAfter is how long a STOPPED instance keeps a dynamic
	// public address (docs: one hour); must be positive.
	PublicIPReleaseAfter time.Duration
}

func (d InstanceDeps) validate() error {
	if d.Instances == nil || d.OpIndex == nil || d.Projects == nil || d.Platforms == nil || d.Network == nil ||
		d.Disks == nil || d.GpuClusters == nil || d.NVLGroups == nil || d.Reservations == nil || d.Filesystems == nil || d.Nodes == nil || d.Perms == nil || d.Quota == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return errors.New("compute: instance service dependencies must not be nil")
	}
	if d.PublicIPReleaseAfter <= 0 {
		return errors.New("compute: the public IP release period must be positive")
	}
	return nil
}

// InstanceService implements nebius.compute.v1.InstanceService. It is a
// management plane only: nothing boots. Instances move through their
// lifecycle states, attach disks (managed disks are created and deleted with
// the instance) and get VPC addresses for their network interfaces.
type InstanceService struct {
	d InstanceDeps
	// nics serializes NIC changes of operations with SweepPublicIPs (see withNICs).
	nics sync.Mutex
	// nodes serializes CreateNode, so it is idempotent by (node group, name).
	nodes sync.Mutex
}

// NewInstanceService wires an InstanceService.
func NewInstanceService(d InstanceDeps) (*InstanceService, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	return &InstanceService{d: d}, nil
}

// CreateInstanceCmd is the input of InstanceService.Create.
type CreateInstanceCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *computepb.InstanceSpec
	Request  proto.Message // echoed in Operation.request
}

// UpdateInstanceCmd is the input of InstanceService.Update (full replace
// guarded by a reset mask).
type UpdateInstanceCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *computepb.InstanceSpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// BatchResult is one entry of a BatchGet: the instance, or why it could not
// be read.
type BatchResult struct {
	Instance *computepb.Instance
	Err      error
}

// Get returns an instance by ID.
func (s *InstanceService) Get(ctx context.Context, id string) (*computepb.Instance, error) {
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}
	return s.d.Instances.Get(ctx, id)
}

// GetByName returns an instance by its name within a project.
func (s *InstanceService) GetByName(ctx context.Context, parentID, name string) (*computepb.Instance, error) {
	if parentID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	return s.d.Instances.GetByName(ctx, parentID, name)
}

// List returns one page of a project's instances in creation order.
func (s *InstanceService) List(ctx context.Context, parentID string, size int64, token string) (InstancePage, error) {
	if parentID == "" {
		return InstancePage{}, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	n, err := pageSize(size)
	if err != nil {
		return InstancePage{}, err
	}
	return s.d.Instances.List(ctx, parentID, n, token)
}

// BatchGet reads up to 1000 instances. Each ID is authorized on its own
// (compute.instances.get); an ID that is missing or not readable gets an
// error in place of the instance.
func (s *InstanceService) BatchGet(ctx context.Context, ids []string) (map[string]BatchResult, error) {
	if len(ids) == 0 {
		return nil, errs.BadRequest(errs.Violation{Field: "instance_ids", Message: "required"})
	}
	if len(ids) > maxBatchGet {
		return nil, errs.BadRequest(errs.Violation{Field: "instance_ids", Message: fmt.Sprintf("at most %d IDs", maxBatchGet)})
	}
	out := make(map[string]BatchResult, len(ids))
	for _, id := range ids {
		if _, done := out[id]; done {
			continue
		}
		if err := s.d.Perms.Require(ctx, "compute.instances.get", id); err != nil {
			out[id] = BatchResult{Err: err}
			continue
		}
		inst, err := s.d.Instances.Get(ctx, id)
		out[id] = BatchResult{Instance: inst, Err: err}
	}
	return out, nil
}

// Create validates the request, then (in the operation's Prepare) binds the
// NICs' addresses, creates managed disks, attaches existing disks and stores
// the instance as CREATING. Work makes it RUNNING, or STOPPED when
// spec.stopped is set.
func (s *InstanceService) Create(ctx context.Context, cmd CreateInstanceCmd) (*operations.Operation, error) {
	inst, err := s.newInstance(ctx, cmd)
	if err != nil {
		return nil, err
	}
	id, parent := inst.GetMetadata().GetId(), inst.GetMetadata().GetParentId()
	return s.submit(ctx, parent, operations.Spec{
		ResourceID: id, Description: "Create instance", CreatedBy: reqctx.PrincipalFrom(ctx).ID, Request: cmd.Request,
		Prepare: func(ctx context.Context) error { return s.prepareCreate(ctx, inst) },
		Work: func(ctx context.Context) error {
			return s.mutate(ctx, id, func(_ InstanceTx, cur *computepb.Instance) error {
				if cur.GetStatus().GetState() == computepb.InstanceStatus_CREATING {
					setInstanceState(cur, restingState(cur.GetSpec()))
				}
				return nil
			})
		},
	})
}

// Update applies a full-replace update with reset-mask semantics. See
// updatePlan for what may change and when.
func (s *InstanceService) Update(ctx context.Context, cmd UpdateInstanceCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	stored, err := s.d.Instances.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.refuseManaged(ctx, id); err != nil {
		return nil, err
	}
	var target computepb.InstanceStatus_InstanceState
	return s.submit(ctx, stored.GetMetadata().GetParentId(), operations.Spec{
		ResourceID: id, Description: "Update instance", CreatedBy: reqctx.PrincipalFrom(ctx).ID, Request: cmd.Request,
		Prepare: func(ctx context.Context) error {
			var err error
			target, err = s.applyUpdate(ctx, cmd)
			return err
		},
		Work: func(ctx context.Context) error { return s.settle(ctx, id, target) },
	})
}

// Delete marks the instance DELETING; Work releases its addresses, detaches
// existing disks, deletes managed disks and removes the instance. It fails
// with FAILED_PRECONDITION while a managed disk has forbid_deletion set.
func (s *InstanceService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}
	stored, err := s.d.Instances.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.refuseManaged(ctx, id); err != nil {
		return nil, err
	}
	return s.submit(ctx, stored.GetMetadata().GetParentId(), operations.Spec{
		ResourceID: id, Description: "Delete instance", CreatedBy: reqctx.PrincipalFrom(ctx).ID, Request: request,
		Prepare: func(ctx context.Context) error {
			return s.withNICs(func() error {
				return s.mutate(ctx, id, func(tx InstanceTx, cur *computepb.Instance) error {
					if err := checkDeletable(tx, cur); err != nil {
						return err
					}
					setInstanceState(cur, computepb.InstanceStatus_DELETING)
					return nil
				})
			})
		},
		Work: func(ctx context.Context) error { return s.remove(ctx, id) },
	})
}

// checkDeletable refuses to delete an instance in a transitional state, or
// one whose managed disks have forbid_deletion set (instance.proto).
func checkDeletable(tx InstanceTx, cur *computepb.Instance) error {
	id := cur.GetMetadata().GetId()
	if st := cur.GetStatus().GetState(); !isResting(st) && st != computepb.InstanceStatus_ERROR {
		return errs.BadState(id, fmt.Sprintf("instance is %s: it can be deleted when RUNNING, STOPPED or ERROR", st))
	}
	for _, a := range cur.GetStatus().GetDiskAttachments() {
		if !a.GetIsManaged() {
			continue
		}
		d, err := tx.Disk(a.GetId())
		if errs.IsKind(err, errs.KindNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if d.GetSpec().GetForbidDeletion() {
			return errs.BadState(a.GetId(), fmt.Sprintf(
				"managed disk %s has forbid_deletion set: the instance cannot be deleted", a.GetName()))
		}
	}
	return nil
}

// isResting reports whether state is RUNNING or STOPPED.
func isResting(state computepb.InstanceStatus_InstanceState) bool {
	return state == computepb.InstanceStatus_RUNNING || state == computepb.InstanceStatus_STOPPED
}

// powerMove is one Start or Stop transition.
type powerMove struct {
	from, via, to computepb.InstanceStatus_InstanceState
	verb, what    string
}

var (
	startMove = powerMove{
		from: computepb.InstanceStatus_STOPPED, via: computepb.InstanceStatus_STARTING, to: computepb.InstanceStatus_RUNNING,
		verb: "started", what: "Start instance",
	}
	stopMove = powerMove{
		from: computepb.InstanceStatus_RUNNING, via: computepb.InstanceStatus_STOPPING, to: computepb.InstanceStatus_STOPPED,
		verb: "stopped", what: "Stop instance",
	}
)

// Start starts a STOPPED instance: STARTING, then RUNNING.
func (s *InstanceService) Start(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.power(ctx, id, request, startMove)
}

// Stop stops a RUNNING instance: STOPPING, then STOPPED. A dynamic public
// address (public_ip_address without allocation_id and static=false) is kept
// for PublicIPReleaseAfter and released after that (see SweepPublicIPs); a
// Start within that period keeps it, a later one assigns a new one.
func (s *InstanceService) Stop(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.power(ctx, id, request, stopMove)
}

func (s *InstanceService) power(ctx context.Context, id string, request proto.Message, m powerMove) (*operations.Operation, error) {
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "id", Message: "required"})
	}
	stored, err := s.d.Instances.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.refuseManaged(ctx, id); err != nil {
		return nil, err
	}
	return s.submit(ctx, stored.GetMetadata().GetParentId(), operations.Spec{
		ResourceID: id, Description: m.what, CreatedBy: reqctx.PrincipalFrom(ctx).ID, Request: request,
		Prepare: func(ctx context.Context) error { return s.withNICs(func() error { return s.preparePower(ctx, id, m) }) },
		Work:    func(ctx context.Context) error { return s.settle(ctx, id, m.to) },
	})
}

// preparePower checks and binds the NICs of a Start or Stop. It releases
// dynamic public addresses that are due first, so a Start after the grace
// period never reuses them. The caller holds the NIC lock.
func (s *InstanceService) preparePower(ctx context.Context, id string, m powerMove) error {
	if _, err := s.releaseExpiredPublic(ctx, id); err != nil {
		return err
	}
	cur, err := s.d.Instances.Get(ctx, id)
	if err != nil {
		return err
	}
	if st := cur.GetStatus().GetState(); st != m.from {
		return errs.BadState(id, fmt.Sprintf("instance is %s: only a %s instance can be %s", st, m.from, m.verb))
	}
	held := cur.GetStatus().GetReservationId()
	reservation := held
	if m.to == computepb.InstanceStatus_RUNNING {
		if reservation, err = s.reservationOnStart(ctx, cur); err != nil {
			return err
		}
	}
	nics, err := s.bindAll(ctx, cur, cur.GetSpec(), m.to, nil)
	if err != nil {
		return errors.Join(err, s.restoreReservation(ctx, cur, held, reservation))
	}
	err = s.mutate(ctx, id, func(_ InstanceTx, cur *computepb.Instance) error {
		cur.Status.NetworkInterfaces = nics
		cur.Status.ReservationId = reservation
		setInstanceState(cur, m.via)
		return nil
	})
	if err != nil {
		_, restoreErr := s.bindAll(ctx, cur, cur.GetSpec(), m.from, nil)
		return errors.Join(err, restoreErr, s.restoreReservation(ctx, cur, held, reservation))
	}
	return nil
}

// settle moves a transitional instance to its resting state.
func (s *InstanceService) settle(ctx context.Context, id string, to computepb.InstanceStatus_InstanceState) error {
	return s.mutate(ctx, id, func(tx InstanceTx, cur *computepb.Instance) error {
		if st := cur.GetStatus().GetState(); to != computepb.InstanceStatus_UNSPECIFIED &&
			(st == computepb.InstanceStatus_UPDATING || st == computepb.InstanceStatus_STARTING || st == computepb.InstanceStatus_STOPPING) {
			setInstanceState(cur, to)
			return s.recordStop(tx, cur, to)
		}
		return nil
	})
}

// ListOperationsByParent returns the operations on instances of a project,
// newest first, including operations on instances deleted since.
func (s *InstanceService) ListOperationsByParent(ctx context.Context, parentID string, size int64, token string) (OperationPage, error) {
	if parentID == "" {
		return OperationPage{}, errs.BadRequest(errs.Violation{Field: "parent_id", Message: "required"})
	}
	n, err := pageSize(size)
	if err != nil {
		return OperationPage{}, err
	}
	offset, err := decodeOffset(token)
	if err != nil {
		return OperationPage{}, err
	}
	ids, err := s.d.OpIndex.IDs(ctx, parentID)
	if err != nil {
		return OperationPage{}, err
	}
	slices.Reverse(ids)
	var page OperationPage
	for i := offset; i < len(ids) && len(page.Items) < n; i++ {
		op, err := s.d.Ops.Get(ctx, ids[i])
		if err != nil {
			return OperationPage{}, err
		}
		page.Items = append(page.Items, op)
	}
	if next := offset + n; next < len(ids) {
		page.NextPageToken = encodeOffset(next)
	}
	return page, nil
}

func (s *InstanceService) submit(ctx context.Context, parentID string, spec operations.Spec) (*operations.Operation, error) {
	op, err := s.d.Ops.Submit(ctx, spec)
	if err != nil {
		return nil, err
	}
	if err := s.d.OpIndex.Add(ctx, parentID, op.ID); err != nil {
		return nil, fmt.Errorf("index operation %s: %w", op.ID, err)
	}
	return op, nil
}

// mutate loads the instance in a transaction, lets fn change it (and disks
// through tx) and stores it.
func (s *InstanceService) mutate(ctx context.Context, id string, fn func(tx InstanceTx, cur *computepb.Instance) error) error {
	return s.d.Instances.Txn(ctx, func(tx InstanceTx) error {
		cur, err := tx.Instance(id)
		if err != nil {
			return err
		}
		if err := fn(tx, cur); err != nil {
			return err
		}
		return tx.PutInstance(cur)
	})
}

// newInstance validates a Create request and builds the CREATING instance
// (without NIC or disk status, which Prepare fills in).
func (s *InstanceService) newInstance(ctx context.Context, cmd CreateInstanceCmd) (*computepb.Instance, error) {
	md, spec := cmd.Metadata, cmd.Spec
	if md.GetId() != "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	parentID := md.GetParentId()
	if parentID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if md.GetName() == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.name", Message: "required"})
	}
	if _, err := s.d.Projects.Parent(ctx, parentID); err != nil {
		return nil, fmt.Errorf("instance parent %s: %w", parentID, err)
	}
	if spec == nil {
		return nil, errs.BadRequest(errs.Violation{Field: "spec", Message: "required"})
	}
	if _, err := s.checkSpec(ctx, parentID, spec); err != nil {
		return nil, err
	}
	id, err := s.d.IDs.New(KindInstance)
	if err != nil {
		return nil, fmt.Errorf("new instance id: %w", err)
	}
	now := timestamppb.New(s.d.Clock.Now())
	return &computepb.Instance{
		Metadata: &commonpb.ResourceMetadata{
			Id: id, ParentId: parentID, Name: md.GetName(), Labels: maps.Clone(md.GetLabels()),
			ResourceVersion: 1, CreatedAt: now, UpdatedAt: now,
		},
		Spec:   proto.CloneOf(spec), // echoed as sent: everything synthesized lives in status
		Status: &computepb.InstanceStatus{State: computepb.InstanceStatus_CREATING, Reconciling: true},
	}, nil
}

// prepareCreate binds the NICs, then creates and attaches the disks and
// stores the instance in one transaction. A failed transaction releases the
// addresses again.
func (s *InstanceService) prepareCreate(ctx context.Context, inst *computepb.Instance) error {
	nics, err := s.bindAll(ctx, inst, inst.GetSpec(), restingState(inst.GetSpec()), nil)
	if err != nil {
		return err
	}
	inst.Status.NetworkInterfaces = nics
	id := inst.GetMetadata().GetId()
	undo := func(err error, fs []string) error {
		return errors.Join(err, s.detachFilesystems(ctx, id, fs), s.leaveGpuCluster(ctx, inst), s.leaveNVLGroup(ctx, inst),
			s.releaseReservation(ctx, id), s.releaseAll(ctx, id, inst.GetSpec()))
	}
	if err := s.joinGpuCluster(ctx, inst); err != nil {
		return undo(err, nil)
	}
	if err := s.joinNVLGroup(ctx, inst); err != nil {
		return undo(err, nil)
	}
	fs, err := s.attachFilesystems(ctx, inst, nil, inst.GetSpec())
	if err != nil {
		return undo(err, nil)
	}
	plan, err := s.planDisks(ctx, inst, nil, inst.GetSpec())
	if err != nil {
		return undo(err, fs)
	}
	if inst.Status.ReservationId, err = s.drawReservation(ctx, inst); err != nil {
		return undo(err, fs)
	}
	err = s.admitCreate(ctx, inst, plan, func(ctx context.Context) error {
		return s.d.Instances.Txn(ctx, func(tx InstanceTx) error {
			atts, err := s.applyDisks(tx, inst, plan)
			if err != nil {
				return err
			}
			inst.Status.DiskAttachments = atts
			return tx.CreateInstance(inst)
		})
	})
	if err != nil {
		return undo(err, fs)
	}
	return nil
}

// remove releases the instance's addresses, then detaches and deletes its
// disks and the instance in one transaction.
func (s *InstanceService) remove(ctx context.Context, id string) error {
	cur, err := s.d.Instances.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := s.releaseAll(ctx, id, cur.GetSpec()); err != nil {
		return err
	}
	if err := s.detachFilesystems(ctx, id, removedFilesystems(cur.GetSpec(), nil)); err != nil {
		return err
	}
	if err := s.leaveGpuCluster(ctx, cur); err != nil {
		return err
	}
	if err := s.leaveNVLGroup(ctx, cur); err != nil {
		return err
	}
	if err := s.releaseReservation(ctx, id); err != nil {
		return err
	}
	return s.d.Instances.Txn(ctx, func(tx InstanceTx) error {
		cur, err := tx.Instance(id)
		if err != nil {
			return err
		}
		plan := diskPlan{current: cur.GetStatus().GetDiskAttachments()}
		if _, err := s.applyDisks(tx, cur, plan); err != nil {
			return err
		}
		if err := tx.ClearStoppedAt(id); err != nil {
			return err
		}
		return tx.DeleteInstance(id)
	})
}

// restingState is where an instance settles for its spec.
func restingState(spec *computepb.InstanceSpec) computepb.InstanceStatus_InstanceState {
	if spec.GetStopped() {
		return computepb.InstanceStatus_STOPPED
	}
	return computepb.InstanceStatus_RUNNING
}

func setInstanceState(inst *computepb.Instance, state computepb.InstanceStatus_InstanceState) {
	if inst.GetStatus() == nil {
		inst.Status = &computepb.InstanceStatus{}
	}
	inst.Status.State = state
	inst.Status.Reconciling = !isResting(state) && state != computepb.InstanceStatus_ERROR
}
