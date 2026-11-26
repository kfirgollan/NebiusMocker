package iam

import (
	"context"

	"google.golang.org/grpc"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
)

// registerUsers seeds the tenant user accounts of the static principals and
// serves TenantUserAccountService, TenantUserAccountWithAttributesService and
// InvitationService.
func registerUsers(ctx context.Context, reg grpc.ServiceRegistrar, r repos, deps mocker.Deps) error {
	return nil
}
