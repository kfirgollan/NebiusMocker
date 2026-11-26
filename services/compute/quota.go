package compute

import (
	"fmt"

	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/peer"
	"github.com/kfirgollan/NebiusMocker/services/compute/dal"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
)

// provideQuotaUsage makes the compute quota consumption (instances, disks,
// snapshots, filesystems, GPU clusters) known to the quotas module.
func provideQuotaUsage(deps mocker.Deps, platforms domain.PlatformResolver) error {
	src := domain.NewQuotaUsage(dal.NewQuotaRecords(deps.Store), platforms)
	if err := peer.Provide[peer.QuotaUsageSource](deps.Peers, peer.QuotaUsageKey("compute"), src); err != nil {
		return fmt.Errorf("compute: %w", err)
	}
	return nil
}
