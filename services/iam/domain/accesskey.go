package domain

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

// AccessKeyAlgorithm is reported in status.algorithm: S3 SigV4 keys are HMAC secrets.
const AccessKeyAlgorithm = "HMAC-SHA256"

// AccessKeyService implements nebius.iam.v2.AccessKeyService: AWS-style keys
// that authenticate S3 requests to Object Storage.
type AccessKeyService struct {
	keys     AccessKeyRepository
	sas      ServiceAccountRepository
	projects ContainerRepository
	ops      Operations
	ids      IDGenerator
	clk      clock.Clock
	material KeyMaterial
}

var _ peer.AccessKeys = (*AccessKeyService)(nil)

// AccessKeyDeps are the dependencies of an AccessKeyService. Material is
// optional (default: GenerateKeyMaterial); the rest are required.
type AccessKeyDeps struct {
	AccessKeys      AccessKeyRepository
	ServiceAccounts ServiceAccountRepository
	Projects        ContainerRepository
	Ops             Operations
	IDs             IDGenerator
	Clock           clock.Clock
	Material        KeyMaterial
}

// NewAccessKeyService wires an AccessKeyService.
func NewAccessKeyService(d AccessKeyDeps) (*AccessKeyService, error) {
	if d.AccessKeys == nil || d.ServiceAccounts == nil || d.Projects == nil || d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("iam: access key service dependencies must not be nil")
	}
	if d.Material == nil {
		d.Material = GenerateKeyMaterial
	}
	return &AccessKeyService{
		keys: d.AccessKeys, sas: d.ServiceAccounts, projects: d.Projects,
		ops: d.Ops, ids: d.IDs, clk: d.Clock, material: d.Material,
	}, nil
}

// GenerateKeyMaterial returns a random AWS-style key pair: a 20-character
// upper-case ID starting with "NMK" and a 40-character secret.
func GenerateKeyMaterial() (awsAccessKeyID, secret string, err error) {
	const idAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	b := make([]byte, 17+30)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate access key: %w", err)
	}
	var id strings.Builder
	id.WriteString("NMK")
	for _, c := range b[:17] {
		id.WriteByte(idAlphabet[int(c)%len(idAlphabet)])
	}
	return id.String(), base64.RawURLEncoding.EncodeToString(b[17:])[:40], nil
}

// CreateAccessKeyCmd is the input of Create.
type CreateAccessKeyCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *iamv2pb.AccessKeySpec
	Request  proto.Message
}

// UpdateAccessKeyCmd is the input of Update.
type UpdateAccessKeyCmd struct {
	Metadata  *commonpb.ResourceMetadata
	Spec      *iamv2pb.AccessKeySpec
	ResetMask *mask.Mask
	Request   proto.Message
}

// Secret is the result of GetSecret.
type Secret struct {
	AwsAccessKeyID string
	Secret         string
}

// Get returns a key as clients see it (see view).
func (s *AccessKeyService) Get(ctx context.Context, id string) (*iamv2pb.AccessKey, error) {
	k, err := s.keys.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.view(k), nil
}

// GetByName returns a key by name within its parent.
func (s *AccessKeyService) GetByName(ctx context.Context, parentID, name string) (*iamv2pb.AccessKey, error) {
	k, err := s.keys.GetByName(ctx, parentID, name)
	if err != nil {
		return nil, err
	}
	return s.view(k), nil
}

// GetByAwsID returns a key by its AWS access key ID.
func (s *AccessKeyService) GetByAwsID(ctx context.Context, awsID string) (*iamv2pb.AccessKey, error) {
	k, err := s.keys.GetByAwsID(ctx, awsID)
	if err != nil {
		return nil, err
	}
	return s.view(k), nil
}

// GetSecret reveals the key's secret.
func (s *AccessKeyService) GetSecret(ctx context.Context, id string) (Secret, error) {
	k, err := s.keys.Get(ctx, id)
	if err != nil {
		return Secret{}, err
	}
	return Secret{AwsAccessKeyID: k.GetStatus().GetAwsAccessKeyId(), Secret: k.GetStatus().GetSecret()}, nil
}

// List returns one page of a parent's keys.
func (s *AccessKeyService) List(ctx context.Context, parentID string, pageSize int64, pageToken string) (AccessKeyPage, error) {
	size, err := pageSizeOf(pageSize)
	if err != nil {
		return AccessKeyPage{}, err
	}
	page, err := s.keys.List(ctx, parentID, size, pageToken)
	if err != nil {
		return AccessKeyPage{}, err
	}
	for i, k := range page.Items {
		page.Items[i] = s.view(k)
	}
	return page, nil
}

// ListByAccount returns every key of an account.
func (s *AccessKeyService) ListByAccount(ctx context.Context, account *iampb.Account) ([]*iamv2pb.AccessKey, error) {
	accountID, err := accountIDOf(account, "account")
	if err != nil {
		return nil, err
	}
	keys, err := s.keys.ListByAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	for i, k := range keys {
		keys[i] = s.view(k)
	}
	return keys, nil
}

// Create issues a new active key for a service account.
func (s *AccessKeyService) Create(ctx context.Context, cmd CreateAccessKeyCmd) (*operations.Operation, error) {
	k, err := s.newKey(ctx, cmd)
	if err != nil {
		return nil, err
	}
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  k.GetMetadata().GetId(),
		Description: "Create access key",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.keys.Create(ctx, k) },
	})
}

// Update changes name, labels and description; the account, expiry and
// delivery mode are immutable.
func (s *AccessKeyService) Update(ctx context.Context, cmd UpdateAccessKeyCmd) (*operations.Operation, error) {
	id := cmd.Metadata.GetId()
	if id == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "required"})
	}
	return s.submit(ctx, id, "Update access key", cmd.Request, func(ctx context.Context) error { return s.applyUpdate(ctx, cmd) })
}

// Delete removes a key.
func (s *AccessKeyService) Delete(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Delete access key",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			_, err := s.keys.Get(ctx, id)
			return err
		},
		Work: func(ctx context.Context) error { return s.keys.Delete(ctx, id) },
	})
}

// Activate makes a key usable again.
func (s *AccessKeyService) Activate(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.submit(ctx, id, "Activate access key", request, func(ctx context.Context) error {
		return s.setState(ctx, id, iamv2pb.AccessKeyStatus_ACTIVE)
	})
}

// Deactivate stops a key from authenticating requests.
func (s *AccessKeyService) Deactivate(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.submit(ctx, id, "Deactivate access key", request, func(ctx context.Context) error {
		return s.setState(ctx, id, iamv2pb.AccessKeyStatus_INACTIVE)
	})
}

// ResolveAccessKey implements peer.AccessKeys for S3 data planes: only
// active, unexpired keys of existing, active service accounts resolve.
func (s *AccessKeyService) ResolveAccessKey(ctx context.Context, awsID string) (peer.AccessKey, error) {
	invalid := errs.Unauthenticated("the access key does not exist or is not active")
	k, err := s.keys.GetByAwsID(ctx, awsID)
	if errs.IsKind(err, errs.KindNotFound) {
		return peer.AccessKey{}, invalid
	}
	if err != nil {
		return peer.AccessKey{}, err
	}
	if s.view(k).GetStatus().GetState() != iamv2pb.AccessKeyStatus_ACTIVE {
		return peer.AccessKey{}, invalid
	}
	principal := k.GetSpec().GetAccount().GetServiceAccount().GetId()
	if principal == "" {
		principal = k.GetSpec().GetAccount().GetUserAccount().GetId()
	} else if sa, err := s.sas.Get(ctx, principal); err != nil || !sa.GetStatus().GetActive() {
		return peer.AccessKey{}, invalid
	}
	return peer.AccessKey{
		AccessKeyID: awsID, Secret: k.GetStatus().GetSecret(), PrincipalID: principal, KeyID: k.GetMetadata().GetId(),
	}, nil
}

func (s *AccessKeyService) submit(ctx context.Context, id, desc string, request proto.Message, prepare func(context.Context) error) (*operations.Operation, error) {
	return s.ops.Submit(ctx, operations.Spec{
		ResourceID: id, Description: desc, CreatedBy: reqctx.PrincipalFrom(ctx).ID, Request: request, Prepare: prepare,
	})
}

func (s *AccessKeyService) setState(ctx context.Context, id string, state iamv2pb.AccessKeyStatus_State) error {
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

// view is the client-visible form of a stored key: the state reflects expiry,
// and the secret is shown inline only for INLINE (and unspecified, whose
// documented default is INLINE) delivery.
func (s *AccessKeyService) view(k *iamv2pb.AccessKey) *iamv2pb.AccessKey {
	out := proto.CloneOf(k)
	if exp := k.GetSpec().GetExpiresAt(); exp != nil && !exp.AsTime().After(s.clk.Now()) &&
		out.GetStatus().GetState() == iamv2pb.AccessKeyStatus_ACTIVE {
		out.Status.State = iamv2pb.AccessKeyStatus_EXPIRED
	}
	switch k.GetSpec().GetSecretDeliveryMode() {
	case iamv2pb.SecretDeliveryMode_UNSPECIFIED, iamv2pb.SecretDeliveryMode_INLINE:
	case iamv2pb.SecretDeliveryMode_EXPLICIT, iamv2pb.SecretDeliveryMode_MYSTERY_BOX:
		out.Status.Secret = ""
	}
	return out
}

func (s *AccessKeyService) newKey(ctx context.Context, cmd CreateAccessKeyCmd) (*iamv2pb.AccessKey, error) {
	md := cmd.Metadata
	if err := requireNoID(md); err != nil {
		return nil, err
	}
	parentID := md.GetParentId()
	if parentID == "" {
		return nil, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if _, err := s.projects.Get(ctx, parentID); err != nil {
		return nil, fmt.Errorf("access key parent %s: %w", parentID, err)
	}
	if err := s.checkSpec(ctx, cmd.Spec); err != nil {
		return nil, err
	}
	id, err := s.ids.New(KindAccessKey)
	if err != nil {
		return nil, fmt.Errorf("new access key id: %w", err)
	}
	awsID, secret, err := s.material()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(secret))
	return &iamv2pb.AccessKey{
		Metadata: newMetadata(id, parentID, md, s.clk.Now()),
		Spec:     proto.CloneOf(cmd.Spec),
		Status: &iamv2pb.AccessKeyStatus{
			State: iamv2pb.AccessKeyStatus_ACTIVE, AwsAccessKeyId: awsID, Secret: secret,
			Algorithm: AccessKeyAlgorithm, KeySize: int32(len(secret) * 8), //nolint:gosec // 40-byte secret, no overflow
			Fingerprint: hex.EncodeToString(sum[:16]),
		},
	}, nil
}

// checkSpec validates the account and the delivery mode of a new key.
func (s *AccessKeyService) checkSpec(ctx context.Context, spec *iamv2pb.AccessKeySpec) error {
	accountID, err := accountIDOf(spec.GetAccount(), "spec.account")
	if err != nil {
		return err
	}
	if saID := spec.GetAccount().GetServiceAccount().GetId(); saID != "" {
		if _, err := s.sas.Get(ctx, saID); err != nil {
			return fmt.Errorf("access key account %s: %w", accountID, err)
		}
	}
	if spec.GetSecretDeliveryMode() == iamv2pb.SecretDeliveryMode_MYSTERY_BOX {
		return errs.BadRequest(errs.Violation{
			Field: "spec.secret_delivery_mode", Message: "MYSTERY_BOX delivery is not supported by NebiusMocker yet; use INLINE or EXPLICIT",
		})
	}
	if exp := spec.GetExpiresAt(); exp != nil && !exp.AsTime().After(s.clk.Now()) {
		return errs.BadRequest(errs.Violation{Field: "spec.expires_at", Message: "must be in the future"})
	}
	return nil
}

// accountIDOf returns the user or service account ID of a, or a BadRequest.
func accountIDOf(a *iampb.Account, field string) (string, error) {
	if id := a.GetServiceAccount().GetId(); id != "" {
		return id, nil
	}
	if id := a.GetUserAccount().GetId(); id != "" {
		return id, nil
	}
	return "", errs.BadRequest(errs.Violation{Field: field, Message: "a service_account or user_account id is required"})
}

func (s *AccessKeyService) applyUpdate(ctx context.Context, cmd UpdateAccessKeyCmd) error {
	stored, err := s.keys.Get(ctx, cmd.Metadata.GetId())
	if err != nil {
		return err
	}
	if err := checkMetadataUpdate(stored.GetMetadata(), cmd.Metadata); err != nil {
		return err
	}
	if err := checkImmutableKeySpec(stored.GetSpec(), cmd.Spec); err != nil {
		return err
	}
	updated, err := patchResource(stored, &iamv2pb.AccessKey{
		Metadata: &commonpb.ResourceMetadata{Name: cmd.Metadata.GetName(), Labels: cmd.Metadata.GetLabels()},
		Spec:     &iamv2pb.AccessKeySpec{Description: cmd.Spec.GetDescription()},
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

// checkImmutableKeySpec rejects changes to account, expires_at and the
// delivery mode. Unset (zero) values in the request mean "unchanged".
func checkImmutableKeySpec(stored, req *iamv2pb.AccessKeySpec) error {
	if a := req.GetAccount(); a != nil && !proto.Equal(a, stored.GetAccount()) {
		return errs.BadRequest(errs.Violation{Field: "spec.account", Message: "field is immutable"})
	}
	if e := req.GetExpiresAt(); e != nil && !e.AsTime().Equal(stored.GetExpiresAt().AsTime()) {
		return errs.BadRequest(errs.Violation{Field: "spec.expires_at", Message: "field is immutable"})
	}
	if m := req.GetSecretDeliveryMode(); m != iamv2pb.SecretDeliveryMode_UNSPECIFIED && m != stored.GetSecretDeliveryMode() {
		return errs.BadRequest(errs.Violation{Field: "spec.secret_delivery_mode", Message: "field is immutable"})
	}
	return nil
}
