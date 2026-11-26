// v1alpha1 is a second view of the v1 resources: same NIDs, same stored
// objects, converted by the functions in v1alpha1_convert.go.

package api

import (
	"context"

	commonv1alpha1pb "github.com/nebius/gosdk/proto/nebius/common/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	opsapi "github.com/kfirgollan/NebiusMocker/pkg/operations/api"
	"github.com/kfirgollan/NebiusMocker/pkg/serviceerror"
)

// alphaOpReply maps a started operation to its v1alpha1 proto.
//
//nolint:staticcheck // SA1019: common.v1alpha1.Operation is deprecated upstream, still returned by v1alpha1 APIs
func alphaOpReply(ctx context.Context) func(*operations.Operation, error) (*commonv1alpha1pb.Operation, error) {
	return func(op *operations.Operation, err error) (*commonv1alpha1pb.Operation, error) {
		if err != nil {
			return nil, serviceerror.FromDomain(ctx, err)
		}
		return opsapi.AlphaToProto(ctx, op), nil
	}
}
