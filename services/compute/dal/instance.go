package dal

import (
	"context"
	"fmt"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// kindInstanceOp is the store kind of instance operation-index entries (not a NID type).
const kindInstanceOp = "computeinstance.op"

// InstanceRepo stores compute instances. Its transactions also reach the
// disks the instances attach (same store kinds as DiskRepo).
type InstanceRepo struct {
	r     protoRepo[*computepb.Instance]
	disks protoRepo[*computepb.Disk]
}

var _ domain.InstanceRepository = (*InstanceRepo)(nil)

// NewInstanceRepo returns an InstanceRepo over st.
func NewInstanceRepo(st store.Store) *InstanceRepo {
	return &InstanceRepo{
		r: protoRepo[*computepb.Instance]{
			st: st, kind: domain.KindInstance, noun: "instance", newT: func() *computepb.Instance { return &computepb.Instance{} },
		},
		disks: NewDiskRepo(st).r,
	}
}

// Get returns the instance or errs.NotFound.
func (r *InstanceRepo) Get(ctx context.Context, id string) (*computepb.Instance, error) {
	return r.r.get(ctx, id)
}

// GetByName returns the instance named name in parentID or errs.NotFound.
func (r *InstanceRepo) GetByName(ctx context.Context, parentID, name string) (*computepb.Instance, error) {
	return r.r.getByName(ctx, parentID, name)
}

// List returns one page of a parent's instances in creation order.
func (r *InstanceRepo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.InstancePage, error) {
	items, next, err := r.r.list(ctx, parentID, pageSize, pageToken, nil)
	return domain.InstancePage{Items: items, NextPageToken: next}, err
}

// Txn implements domain.InstanceRepository.
func (r *InstanceRepo) Txn(ctx context.Context, fn func(tx domain.InstanceTx) error) error {
	return r.r.st.Update(ctx, func(tx store.Tx) error {
		return fn(instanceTx{tx: tx, inst: r.r, disks: r.disks})
	})
}

// instanceTx adapts a store transaction to domain.InstanceTx.
type instanceTx struct {
	tx    store.Tx
	inst  protoRepo[*computepb.Instance]
	disks protoRepo[*computepb.Disk]
}

func (t instanceTx) Instance(id string) (*computepb.Instance, error) { return txGet(t.tx, t.inst, id) }

func (t instanceTx) CreateInstance(inst *computepb.Instance) error {
	return txCreate(t.tx, t.inst, inst)
}

func (t instanceTx) PutInstance(inst *computepb.Instance) error { return txPut(t.tx, t.inst, inst) }

func (t instanceTx) DeleteInstance(id string) error {
	return t.inst.toDomain(t.tx.Delete(t.inst.kind, id), id)
}

func (t instanceTx) Disk(id string) (*computepb.Disk, error) { return txGet(t.tx, t.disks, id) }

func (t instanceTx) CreateDisk(d *computepb.Disk) error { return txCreate(t.tx, t.disks, d) }

func (t instanceTx) PutDisk(d *computepb.Disk) error { return txPut(t.tx, t.disks, d) }

func (t instanceTx) DeleteDisk(id string) error {
	return t.disks.toDomain(t.tx.Delete(t.disks.kind, id), id)
}

func txGet[T resource](tx store.Tx, r protoRepo[T], id string) (T, error) {
	rec, err := tx.Get(r.kind, id)
	if err != nil {
		var zero T
		return zero, r.toDomain(err, id)
	}
	return r.decode(rec)
}

func txCreate[T resource](tx store.Tx, r protoRepo[T], item T) error {
	md := item.GetMetadata()
	if _, err := tx.Get(r.kind, md.GetId()); err == nil {
		return errs.AlreadyExists(md.GetId(), r.noun+" already exists")
	}
	return txWrite(tx, r, item)
}

func txPut[T resource](tx store.Tx, r protoRepo[T], item T) error {
	id := item.GetMetadata().GetId()
	if _, err := tx.Get(r.kind, id); err != nil {
		return r.toDomain(err, id)
	}
	return txWrite(tx, r, item)
}

func txWrite[T resource](tx store.Tx, r protoRepo[T], item T) error {
	md := item.GetMetadata()
	data, err := marshal.Marshal(item)
	if err != nil {
		return fmt.Errorf("encode %s %s: %w", r.noun, md.GetId(), err)
	}
	_, err = tx.Put(store.Record{Kind: r.kind, ID: md.GetId(), ParentID: md.GetParentId(), Name: md.GetName(), Data: data})
	return r.toDomain(err, md.GetId())
}

// InstanceOpIndex records which operations ran on instances of each parent,
// so ListOperationsByParent still finds operations of deleted instances.
type InstanceOpIndex struct{ st store.Store }

var _ domain.OperationIndex = (*InstanceOpIndex)(nil)

// NewInstanceOpIndex returns an InstanceOpIndex over st.
func NewInstanceOpIndex(st store.Store) *InstanceOpIndex { return &InstanceOpIndex{st: st} }

// Add implements domain.OperationIndex.
func (o *InstanceOpIndex) Add(ctx context.Context, parentID, operationID string) error {
	return o.st.Update(ctx, func(tx store.Tx) error {
		_, err := tx.Put(store.Record{Kind: kindInstanceOp, ID: operationID, ParentID: parentID})
		return err
	})
}

// IDs implements domain.OperationIndex.
func (o *InstanceOpIndex) IDs(ctx context.Context, parentID string) ([]string, error) {
	var recs []store.Record
	err := o.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(store.ListQuery{Kind: kindInstanceOp, ParentID: parentID})
		return lerr
	})
	if err != nil {
		return nil, fmt.Errorf("list instance operations of %s: %w", parentID, err)
	}
	ids := make([]string, 0, len(recs))
	for _, rec := range recs {
		ids = append(ids, rec.ID)
	}
	return ids, nil
}
