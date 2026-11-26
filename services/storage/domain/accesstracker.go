package domain

import (
	"context"
	"log/slog"
	"slices"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
)

// trackerPageSize is how many inventories one List call fetches; a bucket has
// at most MaxInventoriesPerBucket, so one page holds them all.
const trackerPageSize = MaxInventoriesPerBucket + 1

// AccessTracker records object reads while an enabled inventory of the bucket
// selects the Last access time field. Tracking starts with such a
// configuration and is not retroactive; when none remains, reads are no
// longer recorded.
type AccessTracker struct {
	log         AccessLog
	inventories InventoryRepository
	clk         clock.Clock
	logger      *slog.Logger
}

// NewAccessTracker returns a tracker. Errors never reach callers: they are
// logged to logger, because a failing log must not fail an S3 read.
func NewAccessTracker(log AccessLog, inventories InventoryRepository, clk clock.Clock, logger *slog.Logger) *AccessTracker {
	return &AccessTracker{log: log, inventories: inventories, clk: clk, logger: logger}
}

// Read notes that key of bucketID was read just now.
func (t *AccessTracker) Read(ctx context.Context, bucketID, key string) {
	page, err := t.inventories.List(ctx, bucketID, trackerPageSize, "")
	if err != nil {
		t.logger.WarnContext(ctx, "last access: list inventories", "bucket", bucketID, "error", err)
		return
	}
	if !slices.ContainsFunc(page.Items, tracksLastAccess) {
		return
	}
	if err := t.log.Touch(ctx, bucketID, key, t.clk.Now()); err != nil {
		t.logger.WarnContext(ctx, "last access: record read", "bucket", bucketID, "key", key, "error", err)
	}
}

func tracksLastAccess(inv *storagepb.Inventory) bool {
	spec := inv.GetSpec()
	return spec.GetEnabled() && slices.Contains(spec.GetOptionalFields(), storagepb.InventorySpec_LAST_ACCESS)
}
