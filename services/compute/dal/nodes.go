package dal

import (
	"context"
	"errors"
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// kindNode is the store kind of managed-node ownership records (not a NID
// type). Record ID = instance ID, ParentID = node group ID, Name = node name
// (unique per node group), Data = cluster ID.
const kindNode = "computenode"

// NodeRepo records which instances are nodes of mk8s node groups.
type NodeRepo struct{ st store.Store }

var _ domain.NodeRepository = (*NodeRepo)(nil)

// NewNodeRepo returns a NodeRepo over st.
func NewNodeRepo(st store.Store) *NodeRepo { return &NodeRepo{st: st} }

func decodeNode(rec store.Record) domain.NodeRecord {
	return domain.NodeRecord{InstanceID: rec.ID, NodeGroupID: rec.ParentID, Name: rec.Name, ClusterID: string(rec.Data)}
}

// Owner implements domain.NodeRepository.
func (r *NodeRepo) Owner(ctx context.Context, instanceID string) (domain.NodeRecord, bool, error) {
	return viewNode(ctx, r.st, func(tx store.Tx) (store.Record, error) { return tx.Get(kindNode, instanceID) })
}

// ByName implements domain.NodeRepository.
func (r *NodeRepo) ByName(ctx context.Context, nodeGroupID, name string) (domain.NodeRecord, bool, error) {
	return viewNode(ctx, r.st, func(tx store.Tx) (store.Record, error) { return tx.ByName(kindNode, nodeGroupID, name) })
}

func viewNode(ctx context.Context, st store.Store, find func(tx store.Tx) (store.Record, error)) (domain.NodeRecord, bool, error) {
	var rec store.Record
	err := st.View(ctx, func(tx store.Tx) error {
		var err error
		rec, err = find(tx)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return domain.NodeRecord{}, false, nil
	}
	if err != nil {
		return domain.NodeRecord{}, false, fmt.Errorf("read node record: %w", err)
	}
	return decodeNode(rec), true, nil
}

// List implements domain.NodeRepository.
func (r *NodeRepo) List(ctx context.Context, nodeGroupID string) ([]domain.NodeRecord, error) {
	var recs []store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var err error
		recs, err = tx.List(store.ListQuery{Kind: kindNode, ParentID: nodeGroupID})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list nodes of node group %s: %w", nodeGroupID, err)
	}
	out := make([]domain.NodeRecord, len(recs))
	for i, rec := range recs {
		out[i] = decodeNode(rec)
	}
	return out, nil
}

// Put implements domain.NodeRepository.
func (r *NodeRepo) Put(ctx context.Context, n domain.NodeRecord) error {
	err := r.st.Update(ctx, func(tx store.Tx) error {
		_, err := tx.Put(store.Record{Kind: kindNode, ID: n.InstanceID, ParentID: n.NodeGroupID, Name: n.Name, Data: []byte(n.ClusterID)})
		return err
	})
	if errors.Is(err, store.ErrNameTaken) {
		return errs.AlreadyExists(n.InstanceID, fmt.Sprintf("node group %s already has a node named %q", n.NodeGroupID, n.Name))
	}
	if err != nil {
		return fmt.Errorf("record node %s: %w", n.InstanceID, err)
	}
	return nil
}

// Delete implements domain.NodeRepository.
func (r *NodeRepo) Delete(ctx context.Context, instanceID string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error { return tx.Delete(kindNode, instanceID) })
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("drop node record %s: %w", instanceID, err)
	}
	return nil
}
