package domain_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// repInvList is a domain.InventoryRepository that only lists.
type repInvList struct {
	domain.InventoryRepository
	items []*storagepb.Inventory
	err   error
}

func (l *repInvList) List(context.Context, string, int, string) (domain.InventoryPage, error) {
	return domain.InventoryPage{Items: l.items}, l.err
}

// repLog is a domain.AccessLog that records touches.
type repLog struct {
	reads map[string]time.Time
	err   error
}

func (l *repLog) Touch(_ context.Context, bucketID, key string, t time.Time) error {
	if l.err != nil {
		return l.err
	}
	l.reads[bucketID+"/"+key] = t
	return nil
}

func (l *repLog) LastAccess(context.Context, string, string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}
func (l *repLog) Forget(context.Context, string) error { return nil }

func repInventory(enabled bool, fields ...storagepb.InventorySpec_OptionalField) *storagepb.Inventory {
	return &storagepb.Inventory{
		Metadata: &commonpb.ResourceMetadata{Id: "inv", Name: "n"},
		Spec:     &storagepb.InventorySpec{Enabled: enabled, OptionalFields: fields},
	}
}

func TestAccessTrackerTracksOnlyWhileConfigured(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	lastAccess := storagepb.InventorySpec_LAST_ACCESS
	tests := []struct {
		name  string
		items []*storagepb.Inventory
		err   error
		want  bool
	}{
		{"no inventory", nil, nil, false},
		{"without the field", []*storagepb.Inventory{repInventory(true, storagepb.InventorySpec_TAGS)}, nil, false},
		{"disabled inventory", []*storagepb.Inventory{repInventory(false, lastAccess)}, nil, false},
		{"enabled with the field", []*storagepb.Inventory{repInventory(true, storagepb.InventorySpec_TAGS), repInventory(true, lastAccess)}, nil, true},
		{"listing fails", nil, errors.New("boom"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			log := &repLog{reads: map[string]time.Time{}}
			tr := domain.NewAccessTracker(log, &repInvList{items: tc.items, err: tc.err}, clock.NewManual(now), slog.New(slog.DiscardHandler))
			tr.Read(t.Context(), "b1", "k")
			got, ok := log.reads["b1/k"]
			if ok != tc.want || ok && !got.Equal(now) {
				t.Fatalf("recorded = %v at %v, want %v at %v", ok, got, tc.want, now)
			}
		})
	}
}

func TestAccessTrackerSwallowsLogErrors(t *testing.T) {
	t.Parallel()
	log := &repLog{reads: map[string]time.Time{}, err: errors.New("disk full")}
	inv := &repInvList{items: []*storagepb.Inventory{repInventory(true, storagepb.InventorySpec_LAST_ACCESS)}}
	domain.NewAccessTracker(log, inv, clock.NewManual(time.Unix(0, 0)), slog.New(slog.DiscardHandler)).Read(t.Context(), "b", "k")
}
