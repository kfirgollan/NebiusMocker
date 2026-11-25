package domain

import (
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"
)

const day = 24 * time.Hour

func rotatingSpec(period time.Duration) *kmspb.SymmetricKeySpec {
	return &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_256, RotationPeriod: durationpb.New(period)}
}

// ctVersion is the key version recorded in a symmetric ciphertext.
func ctVersion(ct []byte) uint32 { return binary.BigEndian.Uint32(ct[1:5]) }

func encryptVersion(t *testing.T, e *symEnv, id string) (ct []byte, version uint32) {
	t.Helper()
	ct, err := e.svc.Encrypt(t.Context(), id, []byte("data"), nil)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	return ct, ctVersion(ct)
}

// wantKeyAt checks resource_version and updated_at of the ACTIVE key.
func wantKeyAt(t *testing.T, e *symEnv, id string, version int64, updated time.Time) {
	t.Helper()
	k, err := e.svc.Get(t.Context(), id, ActiveOnly)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	md := k.GetMetadata()
	if md.GetResourceVersion() != version || !md.GetUpdatedAt().AsTime().Equal(updated) {
		t.Fatalf("resource_version, updated_at = %d, %v; want %d, %v",
			md.GetResourceVersion(), md.GetUpdatedAt().AsTime(), version, updated)
	}
}

func TestAutoRotationWhenPeriodElapses(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	id := createSym(t, e, "k", rotatingSpec(2*day))
	old, v := encryptVersion(t, e, id)
	if v != 1 {
		t.Fatalf("first version = %d, want 1", v)
	}

	e.clk.Advance(2*day - time.Second)
	if _, v = encryptVersion(t, e, id); v != 1 {
		t.Fatalf("version before the period elapsed = %d, want 1", v)
	}
	wantKeyAt(t, e, id, 1, t0)

	// Encrypt is the first access after the boundary: it already uses the
	// new primary version.
	e.clk.Advance(time.Second)
	fresh, v := encryptVersion(t, e, id)
	if v != 2 {
		t.Fatalf("version after the period elapsed = %d, want 2", v)
	}
	wantKeyAt(t, e, id, 2, t0.Add(2*day))
	for ct, want := range map[string]string{string(old): "data", string(fresh): "data"} {
		if got, err := e.svc.Decrypt(t.Context(), id, []byte(ct), nil); err != nil || string(got) != want {
			t.Errorf("Decrypt(v%d) = %q, %v", ctVersion([]byte(ct)), got, err)
		}
	}
}

func TestAutoRotationCatchesUpEveryPeriod(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	id := createSym(t, e, "k", rotatingSpec(2*day))
	old, _ := encryptVersion(t, e, id)

	// Three boundaries (2d, 4d, 6d) passed without any access.
	e.clk.Advance(7 * day)
	page, err := e.svc.List(t.Context(), testProject, 0, "", ActiveOnly)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("List = %v, %v", page.Items, err)
	}
	if md := page.Items[0].GetMetadata(); md.GetResourceVersion() != 4 || !md.GetUpdatedAt().AsTime().Equal(t0.Add(6*day)) {
		t.Fatalf("listed key version %d updated %v, want 4 at +6d", md.GetResourceVersion(), md.GetUpdatedAt().AsTime())
	}
	if _, v := encryptVersion(t, e, id); v != 4 {
		t.Errorf("version = %d, want 4", v)
	}
	if _, err := e.svc.Decrypt(t.Context(), id, old, nil); err != nil {
		t.Errorf("Decrypt of version 1: %v", err)
	}
	// The next boundary is 8d.
	e.clk.Advance(day - time.Second)
	wantKeyAt(t, e, id, 4, t0.Add(6*day))
	e.clk.Advance(time.Second)
	wantKeyAt(t, e, id, 5, t0.Add(8*day))
}

func TestAutoRotationOnGetByName(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	createSym(t, e, "k", rotatingSpec(day))
	e.clk.Advance(day)
	k, err := e.svc.GetByName(t.Context(), testProject, "k")
	if err != nil || k.GetMetadata().GetResourceVersion() != 2 {
		t.Fatalf("GetByName = %v, %v; want resource_version 2", k, err)
	}
}

func TestManualRotationRestartsSchedule(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	id := createSym(t, e, "k", rotatingSpec(2*day))
	e.clk.Advance(day)
	if _, err := e.svc.Rotate(t.Context(), id, nil); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	wantKeyAt(t, e, id, 2, t0.Add(day))

	// 2d after creation is no longer a boundary: the schedule runs from the
	// manual rotation.
	e.clk.Advance(day + 12*time.Hour)
	wantKeyAt(t, e, id, 2, t0.Add(day))
	e.clk.Advance(12 * time.Hour)
	wantKeyAt(t, e, id, 3, t0.Add(3*day))
	if _, v := encryptVersion(t, e, id); v != 3 {
		t.Errorf("version = %d, want 3", v)
	}
}

func TestAutoRotationsDoNotCountAgainstManualLimit(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	id := createSym(t, e, "k", rotatingSpec(day))
	e.clk.Advance((maxManualRotations + 5) * day)
	wantKeyAt(t, e, id, maxManualRotations+6, t0.Add((maxManualRotations+5)*day))
	if _, err := e.svc.Rotate(t.Context(), id, nil); err != nil {
		t.Fatalf("manual Rotate after %d automatic rotations: %v", maxManualRotations+5, err)
	}
}

func TestUpdateRotationPeriodReschedules(t *testing.T) {
	t.Parallel()
	update := func(t *testing.T, e *symEnv, id string, period time.Duration) {
		t.Helper()
		_, err := e.svc.Update(t.Context(), UpdateSymmetricKeyCmd{
			Metadata: &commonpb.ResourceMetadata{Id: id, ParentId: testProject, Name: "k"},
			Spec:     rotatingSpec(period),
		})
		if err != nil {
			t.Fatalf("Update(rotation_period=%v): %v", period, err)
		}
	}

	t.Run("lengthened", func(t *testing.T) {
		t.Parallel()
		e := newSymEnv(t)
		id := createSym(t, e, "k", rotatingSpec(2*day))
		e.clk.Advance(day)
		update(t, e, id, 10*day)
		wantKeyAt(t, e, id, 2, t0.Add(day))
		e.clk.Advance(2 * day) // past the old 2d boundary
		wantKeyAt(t, e, id, 2, t0.Add(day))
		e.clk.Advance(7 * day) // 10d after creation
		wantKeyAt(t, e, id, 3, t0.Add(10*day))
	})

	t.Run("shortened below the elapsed time", func(t *testing.T) {
		t.Parallel()
		e := newSymEnv(t)
		id := createSym(t, e, "k", rotatingSpec(10*day))
		e.clk.Advance(3 * day)
		update(t, e, id, 2*day)
		// creation + 2d is already past: the key rotates as soon as the new
		// period is set, not retroactively.
		wantKeyAt(t, e, id, 3, t0.Add(3*day))
		e.clk.Advance(2 * day)
		wantKeyAt(t, e, id, 4, t0.Add(5*day))
	})

	t.Run("update after a due rotation applies it first", func(t *testing.T) {
		t.Parallel()
		e := newSymEnv(t)
		id := createSym(t, e, "k", rotatingSpec(2*day))
		e.clk.Advance(3 * day)
		update(t, e, id, 10*day) // the 2d rotation happened before the update
		wantKeyAt(t, e, id, 3, t0.Add(3*day))
		e.clk.Advance(9*day - time.Second)
		wantKeyAt(t, e, id, 3, t0.Add(3*day))
		e.clk.Advance(time.Second) // 2d (last rotation) + 10d
		wantKeyAt(t, e, id, 4, t0.Add(12*day))
	})
}

func TestNoAutoRotationWhileScheduledForDeletion(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	ctx := t.Context()
	id := createSym(t, e, "k", rotatingSpec(2*day))
	if _, err := e.svc.Delete(ctx, id, nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	e.clk.Advance(7 * day)
	k, err := e.svc.Get(ctx, id, IncludeScheduled)
	if err != nil || k.GetMetadata().GetResourceVersion() != 1 {
		t.Fatalf("scheduled key = %v, %v; want resource_version 1 (no rotation)", k, err)
	}

	// Restored overdue, it rotates once, now, and the schedule restarts.
	if _, err := e.svc.Undelete(ctx, id, "", nil); err != nil {
		t.Fatalf("Undelete: %v", err)
	}
	wantKeyAt(t, e, id, 2, t0.Add(7*day))
	e.clk.Advance(2*day - time.Second)
	wantKeyAt(t, e, id, 2, t0.Add(7*day))
	e.clk.Advance(time.Second)
	wantKeyAt(t, e, id, 3, t0.Add(9*day))
}

func TestConcurrentAccessRotatesOnce(t *testing.T) {
	t.Parallel()
	e := newSymEnv(t)
	id := createSym(t, e, "k", rotatingSpec(day))
	e.clk.Advance(day)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if _, err := e.svc.Encrypt(t.Context(), id, []byte("x"), nil); err != nil {
				t.Errorf("Encrypt: %v", err)
			}
		})
	}
	wg.Wait()
	wantKeyAt(t, e, id, 2, t0.Add(day))
	if _, v := encryptVersion(t, e, id); v != 2 {
		t.Errorf("version = %d, want 2", v)
	}
}
