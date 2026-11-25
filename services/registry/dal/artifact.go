package dal

import (
	"context"
	"fmt"

	registrypb "github.com/nebius/gosdk/proto/nebius/registry/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/registry/domain"
)

// ArtifactIndex maps artifact IDs to (registry, repository, digest). Each
// record is a registry.v1.Artifact holding only id, name (repository) and
// digest; its store name "<repository>@<digest>" is unique per registry.
type ArtifactIndex struct{ st store.Store }

var _ domain.ArtifactIndex = (*ArtifactIndex)(nil)

// NewArtifactIndex returns an ArtifactIndex over st.
func NewArtifactIndex(st store.Store) *ArtifactIndex { return &ArtifactIndex{st: st} }

func indexKey(repository, digest string) string { return repository + "@" + digest }

func newArtifact() *registrypb.Artifact { return &registrypb.Artifact{} }

// Lookup returns the ID assigned to ref, or errs.NotFound.
func (x *ArtifactIndex) Lookup(ctx context.Context, ref domain.ArtifactRef) (string, error) {
	key := indexKey(ref.Repository, ref.Digest)
	var rec store.Record
	err := x.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.ByName(domain.KindArtifact, ref.RegistryID, key)
		return gerr
	})
	if err != nil {
		return "", toDomain(err, key)
	}
	return rec.ID, nil
}

// Assign stores id for ref; errs.AlreadyExists if ref already has an ID.
func (x *ArtifactIndex) Assign(ctx context.Context, id string, ref domain.ArtifactRef) error {
	data, err := marshal.Marshal(&registrypb.Artifact{Id: id, Name: ref.Repository, Digest: ref.Digest})
	if err != nil {
		return fmt.Errorf("encode artifact %s: %w", id, err)
	}
	err = x.st.Update(ctx, func(tx store.Tx) error {
		if _, getErr := tx.Get(domain.KindArtifact, id); getErr == nil {
			return errs.AlreadyExists(id, "artifact already exists")
		}
		_, putErr := tx.Put(store.Record{
			Kind: domain.KindArtifact, ID: id, ParentID: ref.RegistryID, Name: indexKey(ref.Repository, ref.Digest), Data: data,
		})
		return putErr
	})
	return toDomain(err, id)
}

// Get returns the ref of an artifact ID, or errs.NotFound.
func (x *ArtifactIndex) Get(ctx context.Context, id string) (domain.ArtifactRef, error) {
	var rec store.Record
	err := x.st.View(ctx, func(tx store.Tx) error {
		var gerr error
		rec, gerr = tx.Get(domain.KindArtifact, id)
		return gerr
	})
	if err != nil {
		return domain.ArtifactRef{}, toDomain(err, id)
	}
	a, err := decode(rec, newArtifact)
	if err != nil {
		return domain.ArtifactRef{}, err
	}
	return domain.ArtifactRef{RegistryID: rec.ParentID, Repository: a.GetName(), Digest: a.GetDigest()}, nil
}

// Delete forgets one artifact ID, or returns errs.NotFound.
func (x *ArtifactIndex) Delete(ctx context.Context, id string) error {
	err := x.st.Update(ctx, func(tx store.Tx) error { return tx.Delete(domain.KindArtifact, id) })
	return toDomain(err, id)
}

// DeleteRegistry forgets every artifact ID of a registry.
func (x *ArtifactIndex) DeleteRegistry(ctx context.Context, registryID string) error {
	err := x.st.Update(ctx, func(tx store.Tx) error {
		recs, err := tx.List(store.ListQuery{Kind: domain.KindArtifact, ParentID: registryID})
		if err != nil {
			return err
		}
		for _, r := range recs {
			if err := tx.Delete(domain.KindArtifact, r.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return toDomain(err, registryID)
}
