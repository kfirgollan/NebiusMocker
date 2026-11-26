package domain

import (
	"context"
	"fmt"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
)

var _ peer.SystemAccessKeys = (*AccessKeyService)(nil)

// CreateSystemAccessKey implements peer.SystemAccessKeys: it issues an
// active, inline-delivered key for the service account through the same
// path as AccessKeyService.Create, synchronously and without an IAM
// check (the calling service authorized its own request). The result is an
// ordinary access key: AccessKeyService lists it and the S3 front accepts it.
func (s *AccessKeyService) CreateSystemAccessKey(ctx context.Context, spec peer.SystemAccessKeySpec) (peer.SystemAccessKey, error) {
	k, err := s.newKey(ctx, CreateAccessKeyCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: spec.ParentID, Name: spec.Name},
		Spec: &iamv2pb.AccessKeySpec{
			Description:        spec.Description,
			Account:            &iampb.Account{Type: &iampb.Account_ServiceAccount_{ServiceAccount: &iampb.Account_ServiceAccount{Id: spec.AccountID}}},
			SecretDeliveryMode: iamv2pb.SecretDeliveryMode_INLINE,
		},
	})
	if err != nil {
		return peer.SystemAccessKey{}, err
	}
	if err := s.keys.Create(ctx, k); err != nil {
		return peer.SystemAccessKey{}, fmt.Errorf("create system access key %s: %w", spec.Name, err)
	}
	return peer.SystemAccessKey{
		KeyID: k.GetMetadata().GetId(), AccessKeyID: k.GetStatus().GetAwsAccessKeyId(), Secret: k.GetStatus().GetSecret(),
	}, nil
}

// DeleteSystemAccessKey implements peer.SystemAccessKeys; a missing key is
// not an error.
func (s *AccessKeyService) DeleteSystemAccessKey(ctx context.Context, keyID string) error {
	err := s.keys.Delete(ctx, keyID)
	if errs.IsKind(err, errs.KindNotFound) {
		return nil
	}
	return err
}
