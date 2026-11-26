// Package dal implements the Managed MLflow domain repositories over
// pkg/store. It is the only mspmlflow package that touches storage.
package dal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"google.golang.org/protobuf/proto"

	mlflowpb "github.com/nebius/gosdk/proto/nebius/msp/mlflow/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow/domain"
)

var marshal = proto.MarshalOptions{Deterministic: true}

// kindInternals is the record kind of Internals, keyed by cluster ID.
const kindInternals = "mlflowinternals"

var (
	_ domain.ClusterRepository   = (*ClusterRepo)(nil)
	_ domain.InternalsRepository = (*InternalsRepo)(nil)
)

// ClusterRepo stores clusters (parent: project) as proto-marshaled
// records, with names unique per project.
type ClusterRepo struct{ st store.Store }

// NewClusterRepo returns the cluster repository over st.
func NewClusterRepo(st store.Store) *ClusterRepo { return &ClusterRepo{st: st} }

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

func decode(rec store.Record) (*mlflowpb.Cluster, error) {
	c := &mlflowpb.Cluster{}
	if err := proto.Unmarshal(rec.Data, c); err != nil {
		return nil, fmt.Errorf("decode %s %s: %w", rec.Kind, rec.ID, err)
	}
	return c, nil
}

// Get returns the cluster or errs.NotFound.
func (r *ClusterRepo) Get(ctx context.Context, id string) (*mlflowpb.Cluster, error) {
	return r.one(ctx, id, func(tx store.Tx) (store.Record, error) { return tx.Get(domain.KindCluster, id) })
}

// GetByName returns the cluster named name in parentID, or errs.NotFound.
func (r *ClusterRepo) GetByName(ctx context.Context, parentID, name string) (*mlflowpb.Cluster, error) {
	return r.one(ctx, name, func(tx store.Tx) (store.Record, error) { return tx.ByName(domain.KindCluster, parentID, name) })
}

func (r *ClusterRepo) one(ctx context.Context, key string, get func(store.Tx) (store.Record, error)) (*mlflowpb.Cluster, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = get(tx)
		return gerr
	})
	if err != nil {
		return nil, toDomain(err, key)
	}
	return decode(rec)
}

// List returns one page of the project's clusters in creation order.
func (r *ClusterRepo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.Page[*mlflowpb.Cluster], error) {
	after, err := decodePageToken(pageToken)
	if err != nil {
		return domain.Page[*mlflowpb.Cluster]{}, err
	}
	var recs []store.Record
	err = r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(store.ListQuery{Kind: domain.KindCluster, ParentID: parentID, AfterSeq: after, Limit: pageSize + 1})
		return lerr
	})
	if err != nil {
		return domain.Page[*mlflowpb.Cluster]{}, fmt.Errorf("list %s in %s: %w", domain.KindCluster, parentID, err)
	}
	var page domain.Page[*mlflowpb.Cluster]
	if len(recs) > pageSize {
		recs = recs[:pageSize]
		page.NextPageToken = encodePageToken(recs[len(recs)-1].Seq)
	}
	page.Items = make([]*mlflowpb.Cluster, 0, len(recs))
	for _, rec := range recs {
		c, err := decode(rec)
		if err != nil {
			return domain.Page[*mlflowpb.Cluster]{}, err
		}
		page.Items = append(page.Items, c)
	}
	return page, nil
}

// Create inserts a new cluster; errs.AlreadyExists if the ID exists or the
// name is taken in the project.
func (r *ClusterRepo) Create(ctx context.Context, c *mlflowpb.Cluster) error {
	return r.write(ctx, c, true)
}

// Update replaces an existing cluster; errs.NotFound if it does not exist.
func (r *ClusterRepo) Update(ctx context.Context, c *mlflowpb.Cluster) error {
	return r.write(ctx, c, false)
}

func (r *ClusterRepo) write(ctx context.Context, c *mlflowpb.Cluster, create bool) error {
	md := c.GetMetadata()
	data, err := marshal.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode %s %s: %w", domain.KindCluster, md.GetId(), err)
	}
	rec := store.Record{Kind: domain.KindCluster, ID: md.GetId(), ParentID: md.GetParentId(), Name: md.GetName(), Data: data}
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

// Delete removes the cluster or returns errs.NotFound.
func (r *ClusterRepo) Delete(ctx context.Context, id string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error { return tx.Delete(domain.KindCluster, id) })
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

// internalsRecord is the stored form of domain.Internals.
type internalsRecord struct {
	NetworkID string `json:"network_id,omitempty"`
	KeyID     string `json:"key_id,omitempty"`
	AWSKeyID  string `json:"aws_key_id,omitempty"`
	// Material is the S3 secret of the cluster's access key.
	Material string `json:"material,omitempty"`
}

// InternalsRepo stores what the domain keeps of a cluster beyond its API
// resource, keyed by cluster ID.
type InternalsRepo struct{ st store.Store }

// NewInternalsRepo returns the internals repository over st.
func NewInternalsRepo(st store.Store) *InternalsRepo { return &InternalsRepo{st: st} }

// Put records the cluster's internals, replacing an earlier record.
func (r *InternalsRepo) Put(ctx context.Context, clusterID string, in domain.Internals) error {
	data, err := json.Marshal(internalsRecord{
		NetworkID: in.NetworkID, KeyID: in.AccessKeyID, AWSKeyID: in.AWSAccessKeyID, Material: in.Secret,
	})
	if err != nil {
		return fmt.Errorf("encode internals of %s: %w", clusterID, err)
	}
	return r.st.Update(ctx, func(tx store.Tx) error {
		_, err := tx.Put(store.Record{Kind: kindInternals, ID: clusterID, ParentID: clusterID, Data: data})
		return err
	})
}

// Get returns the cluster's internals or errs.NotFound.
func (r *InternalsRepo) Get(ctx context.Context, clusterID string) (domain.Internals, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(kindInternals, clusterID)
		return gerr
	})
	if err != nil {
		return domain.Internals{}, toDomain(err, clusterID)
	}
	var v internalsRecord
	if err := json.Unmarshal(rec.Data, &v); err != nil {
		return domain.Internals{}, fmt.Errorf("decode internals of %s: %w", clusterID, err)
	}
	return domain.Internals{NetworkID: v.NetworkID, AccessKeyID: v.KeyID, AWSAccessKeyID: v.AWSKeyID, Secret: v.Material}, nil
}

// Delete forgets the cluster's internals; a missing record is not an error.
func (r *InternalsRepo) Delete(ctx context.Context, clusterID string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error { return tx.Delete(kindInternals, clusterID) })
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}
