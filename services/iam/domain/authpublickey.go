package domain

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/auth"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// AuthPublicKeyService implements nebius.iam.v1.AuthPublicKeyService: public
// keys a service account signs its token-exchange assertions with.
type AuthPublicKeyService struct {
	keys     AuthPublicKeyRepository
	sas      ServiceAccountRepository
	projects ContainerRepository
	ops      Operations
	ids      IDGenerator
	clk      clock.Clock
	revoker  TokenRevoker
}

// AuthPublicKeyDeps are the dependencies of an AuthPublicKeyService. All are required.
type AuthPublicKeyDeps struct {
	Keys            AuthPublicKeyRepository
	ServiceAccounts ServiceAccountRepository
	Projects        ContainerRepository
	Ops             Operations
	IDs             IDGenerator
	Clock           clock.Clock
	Revoker         TokenRevoker
}

// NewAuthPublicKeyService wires an AuthPublicKeyService.
func NewAuthPublicKeyService(d AuthPublicKeyDeps) (*AuthPublicKeyService, error) {
	if d.Keys == nil || d.ServiceAccounts == nil || d.Projects == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil || d.Revoker == nil {
		return nil, errors.New("iam: auth public key service dependencies must not be nil")
	}
	return &AuthPublicKeyService{
		keys: d.Keys, sas: d.ServiceAccounts, projects: d.Projects, ops: d.Ops, ids: d.IDs, clk: d.Clock, revoker: d.Revoker,
	}, nil
}

// CreateAuthPublicKeyCmd is the input of Create.
type CreateAuthPublicKeyCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *iampb.AuthPublicKeySpec
	Request  proto.Message
}

// UpdateAuthPublicKeyCmd is the input of Update.
type UpdateAuthPublicKeyCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *iampb.AuthPublicKeySpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// Get returns a key as clients see it (see view).
func (s *AuthPublicKeyService) Get(ctx context.Context, id string) (*iampb.AuthPublicKey, error) {
	k, err := s.keys.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.view(k), nil
}

// Resolve returns a key with expiry applied; token exchange uses it to decide
// whether the key may still sign assertions.
func (s *AuthPublicKeyService) Resolve(ctx context.Context, id string) (*iampb.AuthPublicKey, error) {
	return s.Get(ctx, id)
}

// GetByName returns a key by name within its parent.
func (s *AuthPublicKeyService) GetByName(ctx context.Context, parentID, name string) (*iampb.AuthPublicKey, error) {
	if name == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "name", Message: "required"})
	}
	k, err := s.keys.GetByName(ctx, parentID, name)
	if err != nil {
		return nil, err
	}
	return s.view(k), nil
}

// List returns one page of a parent's keys.
func (s *AuthPublicKeyService) List(ctx context.Context, parentID string, pageSize int64, pageToken, filter string) (AuthPublicKeyPage, error) {
	if err := rejectFilter(filter); err != nil {
		return AuthPublicKeyPage{}, err
	}
	size, err := pageSizeOf(pageSize)
	if err != nil {
		return AuthPublicKeyPage{}, err
	}
	page, err := s.keys.List(ctx, parentID, size, pageToken)
	if err != nil {
		return AuthPublicKeyPage{}, err
	}
	for i, k := range page.Items {
		page.Items[i] = s.view(k)
	}
	return page, nil
}

// ListByAccount returns one page of a service account's keys, oldest first.
func (s *AuthPublicKeyService) ListByAccount(ctx context.Context, account *iampb.Account, pageSize int64, pageToken, filter string) (AuthPublicKeyPage, error) {
	if err := rejectFilter(filter); err != nil {
		return AuthPublicKeyPage{}, err
	}
	size, err := pageSizeOf(pageSize)
	if err != nil {
		return AuthPublicKeyPage{}, err
	}
	accountID, err := accountIDOf(account, "account")
	if err != nil {
		return AuthPublicKeyPage{}, err
	}
	all, err := s.keys.ListByAccount(ctx, accountID)
	if err != nil {
		return AuthPublicKeyPage{}, err
	}
	// The page token is the ID of the last returned key; ListByAccount
	// returns keys in creation order.
	if pageToken != "" {
		start := len(all)
		for i, k := range all {
			if k.GetMetadata().GetId() == pageToken {
				start = i + 1
				break
			}
		}
		all = all[start:]
	}
	var page AuthPublicKeyPage
	if len(all) > size {
		all = all[:size]
		page.NextPageToken = all[len(all)-1].GetMetadata().GetId()
	}
	for _, k := range all {
		page.Items = append(page.Items, s.view(k))
	}
	return page, nil
}

func rejectFilter(filter string) error {
	if filter != "" {
		return errs.BadRequest(errs.Violation{Field: "filter", Message: "filtering is not supported"})
	}
	return nil
}

// Create registers a new active public key for a service account.
func (s *AuthPublicKeyService) Create(ctx context.Context, cmd CreateAuthPublicKeyCmd) (*operations.Operation, error) {
	k, err := s.newKey(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  k.GetMetadata().GetId(),
		Description: "Create auth public key",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.keys.Create(ctx, k) },
	})
}

// Update changes name, labels and description; the account, expiry and data
// are immutable.
func (s *AuthPublicKeyService) Update(ctx context.Context, cmd UpdateAuthPublicKeyCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	return s.submit(ctx, id, "Update auth public key", cmd.Request, func(ctx context.Context) error { return s.applyUpdate(ctx, cmd) })
}

// Delete removes a key and revokes the tokens exchanged with it.
func (s *AuthPublicKeyService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete auth public key",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			_, err := s.keys.Get(ctx, id)
			return err
		},
		Work: func(ctx context.Context) error {
			if err := s.keys.Delete(ctx, id); err != nil {
				return err
			}
			s.revoker.RevokeKey(id)
			return nil
		},
	})
}

// Activate makes a key usable again.
func (s *AuthPublicKeyService) Activate(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.submit(ctx, id, "Activate auth public key", request, func(ctx context.Context) error {
		return s.setState(ctx, id, iampb.AuthPublicKeyStatus_ACTIVE)
	})
}

// Deactivate stops a key from signing assertions and revokes the tokens
// exchanged with it.
func (s *AuthPublicKeyService) Deactivate(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.submit(ctx, id, "Deactivate auth public key", request, func(ctx context.Context) error {
		if err := s.setState(ctx, id, iampb.AuthPublicKeyStatus_INACTIVE); err != nil {
			return err
		}
		s.revoker.RevokeKey(id)
		return nil
	})
}

func (s *AuthPublicKeyService) submit(ctx context.Context, id, desc string, request proto.Message, prepare func(context.Context) error) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: desc, CreatedBy: reqctx.PrincipalFrom(ctx).ID, Request: request, Prepare: prepare,
	})
}

func (s *AuthPublicKeyService) setState(ctx context.Context, id string, state iampb.AuthPublicKeyStatus_State) error {
	k, err := s.keys.Get(ctx, id)
	if err != nil {
		return err
	}
	if k.GetStatus().GetState() == state {
		return nil
	}
	k.Status.State = state
	bumpVersion(k.GetMetadata(), s.clk.Now())
	return s.keys.Update(ctx, k)
}

// view is the client-visible form of a stored key: an active key past its
// expiry reports EXPIRED.
func (s *AuthPublicKeyService) view(k *iampb.AuthPublicKey) *iampb.AuthPublicKey {
	out := proto.CloneOf(k)
	if exp := k.GetSpec().GetExpiresAt(); exp != nil && !exp.AsTime().After(s.clk.Now()) &&
		out.GetStatus().GetState() == iampb.AuthPublicKeyStatus_ACTIVE {
		out.Status.State = iampb.AuthPublicKeyStatus_EXPIRED
	}
	return out
}

func (s *AuthPublicKeyService) newKey(ctx context.Context, cmd CreateAuthPublicKeyCmd) (*iampb.AuthPublicKey, error) {
	md := cmd.Metadata
	if err := requireNoID(md); err != nil {
		return nil, err
	}
	parentID := md.GetParentId()
	if parentID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if _, err := s.projects.Get(ctx, parentID); err != nil {
		return nil, fmt.Errorf("auth public key parent %s: %w", parentID, err)
	}
	spec := cmd.Spec
	saID := spec.GetAccount().GetServiceAccount().GetId()
	if saID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "spec.account", Message: "only service account keys are supported"})
	}
	sa, err := s.sas.Get(ctx, saID)
	if err != nil {
		return nil, fmt.Errorf("auth public key account %s: %w", saID, err)
	}
	if sa.GetMetadata().GetParentId() != parentID {
		return nil, errs.BadRequest(errs.Violation{
			Field: "metadata.parent_id", Message: "must be the service account's project " + sa.GetMetadata().GetParentId(),
		})
	}
	if exp := spec.GetExpiresAt(); exp != nil && !exp.AsTime().After(s.clk.Now()) {
		return nil, errs.BadRequest(errs.Violation{Field: "spec.expires_at", Message: "must be in the future"})
	}
	if spec.GetData() == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "spec.data", Message: "required"})
	}
	pub, err := auth.ParsePublicKey(spec.GetData())
	if err != nil {
		return nil, errs.BadRequest(errs.Violation{Field: "spec.data", Message: err.Error()})
	}
	id, err := s.ids.New(KindAuthPublicKey)
	if err != nil {
		return nil, fmt.Errorf("new auth public key id: %w", err)
	}
	return &iampb.AuthPublicKey{
		Metadata: newMetadata(id, parentID, md, s.clk.Now()),
		Spec:     proto.CloneOf(spec),
		Status: &iampb.AuthPublicKeyStatus{
			State: iampb.AuthPublicKeyStatus_ACTIVE, Fingerprint: pub.Fingerprint, Algorithm: pub.Algorithm,
			KeySize: int32(pub.Bits), //nolint:gosec // RSA modulus sizes are far below int32
		},
	}, nil
}

func (s *AuthPublicKeyService) applyUpdate(ctx context.Context, cmd UpdateAuthPublicKeyCmd) error {
	stored, err := s.keys.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return err
	}
	if err := checkMetadataUpdate(stored.GetMetadata(), cmd.Metadata); err != nil {
		return err
	}
	if err := checkImmutableAuthKeySpec(stored.GetSpec(), cmd.Spec); err != nil {
		return err
	}
	updated, err := patchResource(stored, &iampb.AuthPublicKey{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     &iampb.AuthPublicKeySpec{Description: cmd.Spec.GetDescription()},
	}, cmd.ResetMask)
	if err != nil {
		return err
	}
	// Immutable spec fields and the whole status are server-owned after create.
	desc := updated.GetSpec().GetDescription()
	updated.Spec = proto.CloneOf(stored.GetSpec())
	updated.Spec.Description = desc
	updated.Status = proto.CloneOf(stored.GetStatus())
	if proto.Equal(updated, stored) {
		return nil
	}
	bumpVersion(updated.GetMetadata(), s.clk.Now())
	return s.keys.Update(ctx, updated)
}

// checkImmutableAuthKeySpec rejects changes to account, expires_at and data.
// Unset (zero) values in the request mean "unchanged".
func checkImmutableAuthKeySpec(stored, req *iampb.AuthPublicKeySpec) error {
	if a := req.GetAccount(); a != nil && !proto.Equal(a, stored.GetAccount()) {
		return errs.BadRequest(errs.Violation{Field: "spec.account", Message: "field is immutable"})
	}
	if e := req.GetExpiresAt(); e != nil && !e.AsTime().Equal(stored.GetExpiresAt().AsTime()) {
		return errs.BadRequest(errs.Violation{Field: "spec.expires_at", Message: "field is immutable"})
	}
	if d := req.GetData(); d != "" && d != stored.GetData() {
		return errs.BadRequest(errs.Violation{Field: "spec.data", Message: "field is immutable"})
	}
	return nil
}
