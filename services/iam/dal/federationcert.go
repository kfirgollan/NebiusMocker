package dal

import (
	"context"
	"fmt"

	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/store"
	"github.com/kfirgollan/NebiusMocker/services/iam/domain"
)

// FederationCertificateRepo stores federation certificates. Names are
// optional: the store enforces per-federation uniqueness only when non-empty.
type FederationCertificateRepo struct{ st store.Store }

var _ domain.FederationCertificateRepository = (*FederationCertificateRepo)(nil)

// NewFederationCertificateRepo returns a FederationCertificateRepo over st.
func NewFederationCertificateRepo(st store.Store) *FederationCertificateRepo {
	return &FederationCertificateRepo{st: st}
}

func newFederationCertificate() *iampb.FederationCertificate { return &iampb.FederationCertificate{} }

// Get returns the certificate or errs.NotFound.
func (r *FederationCertificateRepo) Get(ctx context.Context, id string) (*iampb.FederationCertificate, error) {
	return get(ctx, r.st, domain.KindFederationCertificate, id, newFederationCertificate)
}

// List returns one page of a federation's certificates in creation order.
func (r *FederationCertificateRepo) List(ctx context.Context, federationID string, pageSize int, pageToken string) (domain.FederationCertificatePage, error) {
	items, next, err := list(ctx, r.st, domain.KindFederationCertificate, federationID, pageSize, pageToken, newFederationCertificate)
	if err != nil {
		return domain.FederationCertificatePage{}, err
	}
	return domain.FederationCertificatePage{Items: items, NextPageToken: next}, nil
}

// ListAll returns every certificate of a federation in creation order.
func (r *FederationCertificateRepo) ListAll(ctx context.Context, federationID string) ([]*iampb.FederationCertificate, error) {
	var out []*iampb.FederationCertificate
	err := r.st.View(ctx, func(tx store.Tx) error {
		var err error
		out, err = siblings(tx, domain.KindFederationCertificate, federationID, newFederationCertificate)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("certificates of federation %s: %w", federationID, err)
	}
	return out, nil
}

// Create inserts a certificate.
func (r *FederationCertificateRepo) Create(ctx context.Context, c *iampb.FederationCertificate) error {
	return write(ctx, r.st, domain.KindFederationCertificate, c, true, nil)
}

// Update replaces an existing certificate.
func (r *FederationCertificateRepo) Update(ctx context.Context, c *iampb.FederationCertificate) error {
	return write(ctx, r.st, domain.KindFederationCertificate, c, false, nil)
}

// Delete removes the certificate or returns errs.NotFound.
func (r *FederationCertificateRepo) Delete(ctx context.Context, id string) error {
	return remove(ctx, r.st, domain.KindFederationCertificate, id)
}

// DeleteByFederation removes every certificate of a federation in one transaction.
func (r *FederationCertificateRepo) DeleteByFederation(ctx context.Context, federationID string) error {
	err := r.st.Update(ctx, func(tx store.Tx) error {
		recs, err := tx.List(store.ListQuery{Kind: domain.KindFederationCertificate, ParentID: federationID})
		if err != nil {
			return err
		}
		for _, rec := range recs {
			if err := tx.Delete(rec.Kind, rec.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete certificates of federation %s: %w", federationID, toDomain(err, federationID))
	}
	return nil
}

// ReplaceAll deletes remove, updates update and creates create in one
// transaction, so a failure (for example a duplicate name) leaves no trace.
func (r *FederationCertificateRepo) ReplaceAll(ctx context.Context, federationID string, remove []string, update, create []*iampb.FederationCertificate) error {
	err := r.st.Update(ctx, func(tx store.Tx) error {
		for _, id := range remove {
			if err := tx.Delete(domain.KindFederationCertificate, id); err != nil {
				return err
			}
		}
		for _, c := range update {
			if err := putCertificate(tx, federationID, c, true); err != nil {
				return err
			}
		}
		for _, c := range create {
			if err := putCertificate(tx, federationID, c, false); err != nil {
				return err
			}
		}
		return nil
	})
	return toDomain(err, federationID)
}

// putCertificate writes c inside tx; mustExist selects update over create.
func putCertificate(tx store.Tx, federationID string, c *iampb.FederationCertificate, mustExist bool) error {
	md := c.GetMetadata()
	_, getErr := tx.Get(domain.KindFederationCertificate, md.GetId())
	switch {
	case mustExist && getErr != nil:
		return getErr
	case !mustExist && getErr == nil:
		return errs.AlreadyExists(md.GetId(), "federation certificate already exists")
	}
	data, err := marshal.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode federation certificate %s: %w", md.GetId(), err)
	}
	_, err = tx.Put(store.Record{
		Kind: domain.KindFederationCertificate, ID: md.GetId(), ParentID: federationID, Name: md.GetName(), Data: data,
	})
	return err
}
