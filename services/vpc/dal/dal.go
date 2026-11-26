// Package dal implements the VPC domain repositories over pkg/store.
// It is the only VPC package that touches storage.
package dal

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"

	"google.golang.org/protobuf/proto"

	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	vpcv1alpha1pb "github.com/nebius/gosdk/proto/nebius/vpc/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/vpc/domain"
)

var marshal = proto.MarshalOptions{Deterministic: true}

// Repo stores one VPC resource kind as proto-marshaled records.
type Repo[T domain.Resource] struct {
	st     store.Store
	kind   string
	newMsg func() T
}

// NewRepo returns a repository for kind over st.
func NewRepo[T domain.Resource](st store.Store, kind string, newMsg func() T) *Repo[T] {
	return &Repo[T]{st: st, kind: kind, newMsg: newMsg}
}

// NewRepos returns one repository per VPC kind over st.
func NewRepos(st store.Store) domain.Repos {
	return domain.Repos{
		Pools:          NewRepo(st, domain.KindPool, func() *vpcpb.Pool { return &vpcpb.Pool{} }),
		Networks:       NewRepo(st, domain.KindNetwork, func() *vpcpb.Network { return &vpcpb.Network{} }),
		Subnets:        NewRepo(st, domain.KindSubnet, func() *vpcpb.Subnet { return &vpcpb.Subnet{} }),
		Allocations:    NewRepo(st, domain.KindAllocation, func() *vpcpb.Allocation { return &vpcpb.Allocation{} }),
		RouteTables:    NewRepo(st, domain.KindRouteTable, func() *vpcpb.RouteTable { return &vpcpb.RouteTable{} }),
		Routes:         NewRepo(st, domain.KindRoute, func() *vpcpb.Route { return &vpcpb.Route{} }),
		SecurityGroups: NewRepo(st, domain.KindSecurityGroup, func() *vpcpb.SecurityGroup { return &vpcpb.SecurityGroup{} }),
		SecurityRules:  NewRepo(st, domain.KindSecurityRule, func() *vpcpb.SecurityRule { return &vpcpb.SecurityRule{} }),
		Scopes:         NewRepo(st, domain.KindScope, func() *vpcv1alpha1pb.Scope { return &vpcv1alpha1pb.Scope{} }),
	}
}

var _ domain.Repository[*vpcpb.Pool] = (*Repo[*vpcpb.Pool])(nil)

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

// Get returns the resource or errs.NotFound.
func (r *Repo[T]) Get(ctx context.Context, id string) (T, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(r.kind, id)
		return gerr
	})
	if err != nil {
		var zero T
		return zero, toDomain(err, id)
	}
	return r.decode(rec)
}

// GetByName returns the resource named name in parentID or errs.NotFound.
func (r *Repo[T]) GetByName(ctx context.Context, parentID, name string) (T, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.ByName(r.kind, parentID, name)
		return gerr
	})
	if err != nil {
		var zero T
		return zero, toDomain(err, name)
	}
	return r.decode(rec)
}

// List returns one page of the resources under parentID in creation order.
func (r *Repo[T]) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.Page[T], error) {
	return r.page(ctx, store.ListQuery{Kind: r.kind, ParentID: parentID}, func(T) bool { return true }, pageSize, pageToken)
}

// ListWhere returns one page of the matching resources (any parent).
func (r *Repo[T]) ListWhere(ctx context.Context, match func(T) bool, pageSize int, pageToken string) (domain.Page[T], error) {
	return r.page(ctx, store.ListQuery{Kind: r.kind, AnyParent: true}, match, pageSize, pageToken)
}

// All returns every matching resource (any parent) in creation order.
func (r *Repo[T]) All(ctx context.Context, match func(T) bool) ([]T, error) {
	page, err := r.page(ctx, store.ListQuery{Kind: r.kind, AnyParent: true}, match, 0, "")
	return page.Items, err
}

// page scans records after the token and keeps pageSize matches (0: all).
func (r *Repo[T]) page(ctx context.Context, q store.ListQuery, match func(T) bool, pageSize int, pageToken string) (domain.Page[T], error) {
	after, err := decodePageToken(pageToken)
	if err != nil {
		return domain.Page[T]{}, err
	}
	q.AfterSeq = after
	var recs []store.Record
	err = r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(q)
		return lerr
	})
	if err != nil {
		return domain.Page[T]{}, fmt.Errorf("list %s: %w", r.kind, err)
	}
	var (
		out     domain.Page[T]
		lastSeq uint64
	)
	for _, rec := range recs {
		m, err := r.decode(rec)
		if err != nil {
			return domain.Page[T]{}, err
		}
		if !match(m) {
			continue
		}
		if pageSize > 0 && len(out.Items) == pageSize {
			out.NextPageToken = encodePageToken(lastSeq)
			return out, nil
		}
		out.Items = append(out.Items, m)
		lastSeq = rec.Seq
	}
	return out, nil
}

// Create inserts a new resource; the name must be unique in its parent.
func (r *Repo[T]) Create(ctx context.Context, m T) error { return r.write(ctx, m, true) }

// Update replaces an existing resource.
func (r *Repo[T]) Update(ctx context.Context, m T) error { return r.write(ctx, m, false) }

func (r *Repo[T]) write(ctx context.Context, m T, create bool) error {
	md := m.GetMetadata()
	data, err := marshal.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode %s %s: %w", r.kind, md.GetId(), err)
	}
	err = r.st.Update(ctx, func(tx store.Tx) error {
		_, getErr := tx.Get(r.kind, md.GetId())
		switch {
		case create && getErr == nil:
			return errs.AlreadyExists(md.GetId(), r.kind+" already exists")
		case !create && getErr != nil:
			return getErr
		}
		_, putErr := tx.Put(store.Record{Kind: r.kind, ID: md.GetId(), ParentID: md.GetParentId(), Name: md.GetName(), Data: data})
		return putErr
	})
	return toDomain(err, md.GetId())
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
