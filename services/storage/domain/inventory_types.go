package domain

import (
	"context"
	"time"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
)

// Inventory limits and timing (docs.nebius.com/object-storage/buckets/inventory).
const (
	// MaxInventoriesPerBucket: "up to five inventory configurations per bucket".
	MaxInventoriesPerBucket = 5
	// InventoryRetryInterval is how long a failed report waits before the
	// next attempt.
	InventoryRetryInterval = time.Hour
)

// InventoryState is the scheduler state of an inventory (not part of the API).
type InventoryState struct {
	// NextRun is when the next report is due.
	NextRun time.Time
	// LastRun is when the last report attempt ran (zero before the first).
	LastRun time.Time
	// CreatedBy is the principal that created or last updated the
	// configuration; reports are written on its behalf.
	CreatedBy string
}

// InventoryReport is one generated report: the objects written to the
// destination bucket.
type InventoryReport struct {
	ManifestKey string
	DataKeys    []string
	Objects     int64 // inventory rows
}

// ReportSource is what a report lists.
type ReportSource struct {
	Inventory *storagepb.Inventory
	// Source and Destination are the source (parent) and destination buckets.
	Source      *storagepb.Bucket
	Destination *storagepb.Bucket
	Now         time.Time
}

// ReportWriter generates inventory reports (inventory_report.go).
type ReportWriter interface {
	Write(ctx context.Context, src ReportSource) (InventoryReport, error)
}
