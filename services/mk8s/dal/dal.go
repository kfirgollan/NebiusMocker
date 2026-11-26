// Package dal implements the mk8s domain repositories over pkg/store. It is
// the only mk8s package that touches storage.
package dal

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"

	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/domain"
)

var marshal = proto.MarshalOptions{Deterministic: true}

// resource is the common shape of Cluster and NodeGroup.
type resource interface {
	proto.Message
	GetMetadata() *commonpb.ResourceMetadata
}

// Repo stores one mk8s resource kind as proto-marshaled records, with names
// unique per parent.
type Repo[T resource] struct {
	st     store.Store
	kind   string
	newMsg func() T
}

var (
	_ domain.ClusterRepository   = (*ClusterRepo)(nil)
	_ domain.NodeGroupRepository = (*NodeGroupRepo)(nil)
)

// ClusterRepo stores clusters (parent: project).
type ClusterRepo struct{ Repo[*mk8spb.Cluster] }

// NodeGroupRepo stores node groups (parent: cluster).
type NodeGroupRepo struct{ Repo[*mk8spb.NodeGroup] }

// NewClusterRepo returns the cluster repository over st.
func NewClusterRepo(st store.Store) *ClusterRepo {
	return &ClusterRepo{Repo[*mk8spb.Cluster]{st: st, kind: domain.KindCluster, newMsg: func() *mk8spb.Cluster { return &mk8spb.Cluster{} }}}
}

// NewNodeGroupRepo returns the node group repository over st.
func NewNodeGroupRepo(st store.Store) *NodeGroupRepo {
	return &NodeGroupRepo{Repo[*mk8spb.NodeGroup]{st: st, kind: domain.KindNodeGroup, newMsg: func() *mk8spb.NodeGroup { return &mk8spb.NodeGroup{} }}}
}

// All returns every node group of the cluster in creation order.
func (r *NodeGroupRepo) All(ctx context.Context, clusterID string) ([]*mk8spb.NodeGroup, error) {
	var recs []store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(store.ListQuery{Kind: r.kind, ParentID: clusterID})
		return lerr
	})
	if err != nil {
		return nil, fmt.Errorf("list %s in %s: %w", r.kind, clusterID, err)
	}
	return r.decodeAll(recs)
}

// toDomain maps storage errors to domain error kinds.
func toDomain(err error, id string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound):
		return errs.NotFound(id)
	case errors.Is(err, store.ErrNameTaken):
		existing := ""
		if nt, ok := errors.AsType[*store.NameTakenError](err); ok {
			existing = nt.ExistingID
		}
		return errs.AlreadyExists(existing, "a resource with this name already exists in the parent")
	default:
		return err
	}
}

func (r *Repo[T]) decode(rec store.Record) (T, error) {
	m := r.newMsg()
	if err := proto.Unmarshal(rec.Data, m); err != nil {
		var zero T
		return zero, fmt.Errorf("decode %s %s: %w", rec.Kind, rec.ID, err)
	}
	return m, nil
}

func (r *Repo[T]) decodeAll(recs []store.Record) ([]T, error) {
	out := make([]T, 0, len(recs))
	for _, rec := range recs {
		m, err := r.decode(rec)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// Get returns the resource or errs.NotFound.
func (r *Repo[T]) Get(ctx context.Context, id string) (T, error) {
	return r.one(ctx, id, func(tx store.Tx) (store.Record, error) { return tx.Get(r.kind, id) })
}

// GetByName returns the resource named name in parentID, or errs.NotFound.
func (r *Repo[T]) GetByName(ctx context.Context, parentID, name string) (T, error) {
	return r.one(ctx, name, func(tx store.Tx) (store.Record, error) { return tx.ByName(r.kind, parentID, name) })
}

func (r *Repo[T]) one(ctx context.Context, key string, get func(store.Tx) (store.Record, error)) (T, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = get(tx)
		return gerr
	})
	if err != nil {
		var zero T
		return zero, toDomain(err, key)
	}
	return r.decode(rec)
}

// List returns one page of the parent's resources in creation order.
func (r *Repo[T]) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.Page[T], error) {
	after, err := decodePageToken(pageToken)
	if err != nil {
		return domain.Page[T]{}, err
	}
	var recs []store.Record
	err = r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(store.ListQuery{Kind: r.kind, ParentID: parentID, AfterSeq: after, Limit: pageSize + 1})
		return lerr
	})
	if err != nil {
		return domain.Page[T]{}, fmt.Errorf("list %s in %s: %w", r.kind, parentID, err)
	}
	var page domain.Page[T]
	if len(recs) > pageSize {
		recs = recs[:pageSize]
		page.NextPageToken = encodePageToken(recs[len(recs)-1].Seq)
	}
	page.Items, err = r.decodeAll(recs)
	return page, err
}

// Create inserts a new resource; errs.AlreadyExists if the ID exists or the
// name is taken in the parent.
func (r *Repo[T]) Create(ctx context.Context, m T) error { return r.write(ctx, m, true) }

// Update replaces an existing resource; errs.NotFound if it does not exist.
func (r *Repo[T]) Update(ctx context.Context, m T) error { return r.write(ctx, m, false) }

func (r *Repo[T]) write(ctx context.Context, m T, create bool) error {
	md := m.GetMetadata()
	data, err := marshal.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode %s %s: %w", r.kind, md.GetId(), err)
	}
	rec := store.Record{Kind: r.kind, ID: md.GetId(), ParentID: md.GetParentId(), Name: md.GetName(), Data: data}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		_, getErr := tx.Get(rec.Kind, rec.ID)
		switch {
		case create && getErr == nil:
			return errs.AlreadyExists(rec.ID, rec.Kind+" already exists")
		case !create && getErr != nil:
			return getErr
		}
		_, putErr := tx.Put(rec)
		return putErr
	})
	return toDomain(err, rec.ID)
}

// Delete removes the resource or returns errs.NotFound.
func (r *Repo[T]) Delete(ctx context.Context, id string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error { return tx.Delete(r.kind, id) })
	return toDomain(err, id)
}

func encodePageToken(seq uint64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatUint(seq, 10)))
}

func decodePageToken(token string) (uint64, error) {
	if token == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err == nil {
		if seq, perr := strconv.ParseUint(string(b), 10, 64); perr == nil {
			return seq, nil
		}
	}
	return 0, errs.BadRequest(errs.Violation{Field: "page_token", Message: "malformed page token"})
}

// kindNetworkBinding is the record kind holding a cluster's network.
const kindNetworkBinding = "mk8snetbinding"

// NetworkBindingRepo stores the network each cluster is bound to, keyed by
// cluster ID.
type NetworkBindingRepo struct{ st store.Store }

var _ domain.NetworkBindingRepository = (*NetworkBindingRepo)(nil)

// NewNetworkBindingRepo returns the network binding repository over st.
func NewNetworkBindingRepo(st store.Store) *NetworkBindingRepo { return &NetworkBindingRepo{st: st} }

// Set records the cluster's network, replacing an earlier record.
func (r *NetworkBindingRepo) Set(ctx context.Context, clusterID, networkID string) error {
	return r.st.Update(ctx, func(tx store.Tx) error {
		_, err := tx.Put(store.Record{Kind: kindNetworkBinding, ID: clusterID, ParentID: clusterID, Data: []byte(networkID)})
		return err
	})
}

// Get returns the cluster's network or errs.NotFound.
func (r *NetworkBindingRepo) Get(ctx context.Context, clusterID string) (string, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(kindNetworkBinding, clusterID)
		return gerr
	})
	if err != nil {
		return "", toDomain(err, clusterID)
	}
	return string(rec.Data), nil
}

// Delete forgets the cluster's network; a missing record is not an error.
func (r *NetworkBindingRepo) Delete(ctx context.Context, clusterID string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error { return tx.Delete(kindNetworkBinding, clusterID) })
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}
