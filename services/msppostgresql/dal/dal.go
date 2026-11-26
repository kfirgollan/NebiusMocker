// Package dal implements the Managed PostgreSQL domain repositories over
// pkg/store. It is the only msppostgresql package that touches storage.
package dal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

var marshal = proto.MarshalOptions{Deterministic: true}

// Record kinds of the records this package keeps besides the clusters.
const (
	kindBackup         = domain.KindBackup
	kindNetworkBinding = "postgresqlnetbinding"
)

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

func encodePageToken(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func decodePageToken(token string) (string, error) {
	if token == "" {
		return "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", errs.BadRequest(errs.Violation{Field: "page_token", Message: "malformed page token"})
	}
	return string(b), nil
}

// ClusterRepo stores clusters (parent: project) as proto-marshaled records,
// with names unique per project.
type ClusterRepo struct{ st store.Store }

var _ domain.ClusterRepository = (*ClusterRepo)(nil)

// NewClusterRepo returns the cluster repository over st.
func NewClusterRepo(st store.Store) *ClusterRepo { return &ClusterRepo{st: st} }

func decodeCluster(rec store.Record) (*pgpb.Cluster, error) {
	c := &pgpb.Cluster{}
	if err := proto.Unmarshal(rec.Data, c); err != nil {
		return nil, fmt.Errorf("decode %s %s: %w", rec.Kind, rec.ID, err)
	}
	return c, nil
}

func decodeClusters(recs []store.Record) ([]*pgpb.Cluster, error) {
	out := make([]*pgpb.Cluster, 0, len(recs))
	for _, rec := range recs {
		c, err := decodeCluster(rec)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// Get returns the cluster or errs.NotFound.
func (r *ClusterRepo) Get(ctx context.Context, id string) (*pgpb.Cluster, error) {
	return r.one(ctx, id, func(tx store.Tx) (store.Record, error) { return tx.Get(domain.KindCluster, id) })
}

// GetByName returns the cluster named name in the project, or errs.NotFound.
func (r *ClusterRepo) GetByName(ctx context.Context, parentID, name string) (*pgpb.Cluster, error) {
	return r.one(ctx, name, func(tx store.Tx) (store.Record, error) { return tx.ByName(domain.KindCluster, parentID, name) })
}

func (r *ClusterRepo) one(ctx context.Context, key string, get func(store.Tx) (store.Record, error)) (*pgpb.Cluster, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = get(tx)
		return gerr
	})
	if err != nil {
		return nil, toDomain(err, key)
	}
	return decodeCluster(rec)
}

// All returns every cluster in creation order.
func (r *ClusterRepo) All(ctx context.Context) ([]*pgpb.Cluster, error) {
	var recs []store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(store.ListQuery{Kind: domain.KindCluster, AnyParent: true})
		return lerr
	})
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	return decodeClusters(recs)
}

// List returns one page of the project's clusters in creation order.
func (r *ClusterRepo) List(ctx context.Context, parentID string, pageSize int, pageToken string) (domain.Page[*pgpb.Cluster], error) {
	tok, err := decodePageToken(pageToken)
	if err != nil {
		return domain.Page[*pgpb.Cluster]{}, err
	}
	after, perr := strconv.ParseUint(tok, 10, 64)
	if tok != "" && perr != nil {
		return domain.Page[*pgpb.Cluster]{}, errs.BadRequest(errs.Violation{Field: "page_token", Message: "malformed page token"})
	}
	var recs []store.Record
	err = r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(store.ListQuery{Kind: domain.KindCluster, ParentID: parentID, AfterSeq: after, Limit: pageSize + 1})
		return lerr
	})
	if err != nil {
		return domain.Page[*pgpb.Cluster]{}, fmt.Errorf("list clusters in %s: %w", parentID, err)
	}
	var page domain.Page[*pgpb.Cluster]
	if len(recs) > pageSize {
		recs = recs[:pageSize]
		page.NextPageToken = encodePageToken(strconv.FormatUint(recs[len(recs)-1].Seq, 10))
	}
	page.Items, err = decodeClusters(recs)
	return page, err
}

// Create inserts a cluster; errs.AlreadyExists if the ID exists or the name
// is taken in the project.
func (r *ClusterRepo) Create(ctx context.Context, c *pgpb.Cluster) error {
	return r.write(ctx, c, true)
}

// Update replaces an existing cluster; errs.NotFound if it does not exist.
func (r *ClusterRepo) Update(ctx context.Context, c *pgpb.Cluster) error {
	return r.write(ctx, c, false)
}

func (r *ClusterRepo) write(ctx context.Context, c *pgpb.Cluster, create bool) error {
	md := c.GetMetadata()
	data, err := marshal.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode cluster %s: %w", md.GetId(), err)
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

// BackupRepo stores backups, keyed by backup ID under their source cluster.
type BackupRepo struct{ st store.Store }

var _ domain.BackupRepository = (*BackupRepo)(nil)

// NewBackupRepo returns the backup repository over st.
func NewBackupRepo(st store.Store) *BackupRepo { return &BackupRepo{st: st} }

// envelope is the stored form of a BackupRecord.
type envelope struct {
	Backup        []byte `json:"backup"`
	Cluster       []byte `json:"cluster,omitempty"`
	ProjectID     string `json:"project_id"`
	Done          bool   `json:"done"`
	RetentionDays int    `json:"retention_days"`
}

func encodeBackup(b domain.BackupRecord) ([]byte, error) {
	var e envelope
	var err error
	if e.Backup, err = marshal.Marshal(b.Backup); err != nil {
		return nil, err
	}
	if b.Cluster != nil {
		if e.Cluster, err = marshal.Marshal(b.Cluster); err != nil {
			return nil, err
		}
	}
	e.ProjectID, e.Done, e.RetentionDays = b.ProjectID, b.Done, b.RetentionDays
	return json.Marshal(e)
}

func decodeBackup(rec store.Record) (domain.BackupRecord, error) {
	var e envelope
	if err := json.Unmarshal(rec.Data, &e); err != nil {
		return domain.BackupRecord{}, fmt.Errorf("decode backup %s: %w", rec.ID, err)
	}
	out := domain.BackupRecord{Backup: &pgpb.Backup{}, ProjectID: e.ProjectID, Done: e.Done, RetentionDays: e.RetentionDays}
	if err := proto.Unmarshal(e.Backup, out.Backup); err != nil {
		return domain.BackupRecord{}, fmt.Errorf("decode backup %s: %w", rec.ID, err)
	}
	if len(e.Cluster) > 0 {
		out.Cluster = &pgpb.Cluster{}
		if err := proto.Unmarshal(e.Cluster, out.Cluster); err != nil {
			return domain.BackupRecord{}, fmt.Errorf("decode backup %s cluster: %w", rec.ID, err)
		}
	}
	return out, nil
}

// Get returns the backup of the cluster, or errs.NotFound.
func (r *BackupRepo) Get(ctx context.Context, clusterID, backupID string) (domain.BackupRecord, error) {
	var rec store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(kindBackup, backupID)
		return gerr
	})
	if err != nil {
		return domain.BackupRecord{}, toDomain(err, backupID)
	}
	if rec.ParentID != clusterID {
		return domain.BackupRecord{}, errs.NotFound(backupID)
	}
	return decodeBackup(rec)
}

// All returns every backup.
func (r *BackupRepo) All(ctx context.Context) ([]domain.BackupRecord, error) {
	return r.list(ctx, store.ListQuery{Kind: kindBackup, AnyParent: true}, func(domain.BackupRecord) bool { return true })
}

func (r *BackupRepo) list(ctx context.Context, q store.ListQuery, keep func(domain.BackupRecord) bool) ([]domain.BackupRecord, error) {
	var recs []store.Record
	err := r.st.View(ctx, func(tx store.Tx) error {
		var lerr error
		recs, lerr = tx.List(q)
		return lerr
	})
	if err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	var out []domain.BackupRecord
	for _, rec := range recs {
		b, err := decodeBackup(rec)
		if err != nil {
			return nil, err
		}
		if keep(b) {
			out = append(out, b)
		}
	}
	return out, nil
}

// ListByProject returns one page of a project's backups, newest first.
func (r *BackupRepo) ListByProject(ctx context.Context, projectID string, pageSize int, pageToken string) (domain.Page[domain.BackupRecord], error) {
	all, err := r.list(ctx, store.ListQuery{Kind: kindBackup, AnyParent: true}, func(b domain.BackupRecord) bool { return b.ProjectID == projectID })
	if err != nil {
		return domain.Page[domain.BackupRecord]{}, err
	}
	return paginate(all, pageSize, pageToken)
}

// ListByCluster returns one page of a cluster's backups, newest first.
func (r *BackupRepo) ListByCluster(ctx context.Context, clusterID string, pageSize int, pageToken string) (domain.Page[domain.BackupRecord], error) {
	all, err := r.list(ctx, store.ListQuery{Kind: kindBackup, ParentID: clusterID}, func(domain.BackupRecord) bool { return true })
	if err != nil {
		return domain.Page[domain.BackupRecord]{}, err
	}
	return paginate(all, pageSize, pageToken)
}

// sortKey orders backups: creation_start descending, then cluster ID and
// backup ID ascending (backup.proto ListBackupsResponse). The page token is
// the key of the last item of the previous page.
func sortKey(b domain.BackupRecord) string {
	return fmt.Sprintf("%020d|%s|%s", int64(1)<<62-b.Backup.GetCreationStart().AsTime().UnixNano(), b.Backup.GetSourceClusterId(), b.Backup.GetId())
}

func paginate(all []domain.BackupRecord, pageSize int, pageToken string) (domain.Page[domain.BackupRecord], error) {
	after, err := decodePageToken(pageToken)
	if err != nil {
		return domain.Page[domain.BackupRecord]{}, err
	}
	slices.SortFunc(all, func(a, b domain.BackupRecord) int { return strings.Compare(sortKey(a), sortKey(b)) })
	start := 0
	if after != "" {
		if start = slices.IndexFunc(all, func(b domain.BackupRecord) bool { return sortKey(b) > after }); start < 0 {
			start = len(all)
		}
	}
	rest := all[start:]
	var page domain.Page[domain.BackupRecord]
	if len(rest) > pageSize {
		rest = rest[:pageSize]
		page.NextPageToken = encodePageToken(sortKey(rest[len(rest)-1]))
	}
	page.Items = rest
	return page, nil
}

// Put inserts or replaces a backup record.
func (r *BackupRepo) Put(ctx context.Context, b domain.BackupRecord) error {
	data, err := encodeBackup(b)
	if err != nil {
		return fmt.Errorf("encode backup %s: %w", b.Backup.GetId(), err)
	}
	rec := store.Record{Kind: kindBackup, ID: b.Backup.GetId(), ParentID: b.Backup.GetSourceClusterId(), Data: data}
	return r.st.Update(ctx, func(tx store.Tx) error {
		_, perr := tx.Put(rec)
		return perr
	})
}

// Delete removes a record; a missing one is not an error.
func (r *BackupRepo) Delete(ctx context.Context, clusterID, backupID string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error {
		rec, gerr := tx.Get(kindBackup, backupID)
		if gerr != nil || rec.ParentID != clusterID {
			return gerr
		}
		return tx.Delete(kindBackup, backupID)
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

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
