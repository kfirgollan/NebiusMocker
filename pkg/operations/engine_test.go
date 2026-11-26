package operations_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/goleak"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/nid"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/operations/dal"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newEngine(t *testing.T, clk clock.Clock, delay time.Duration) *operations.Engine {
	t.Helper()
	ids, err := nid.NewGenerator(nid.Sequential, "e00", 0)
	if err != nil {
		t.Fatal(err)
	}
	e, err := operations.NewEngine(dal.NewMemory(), clk, ids, delay, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestInlineCompletion(t *testing.T) {
	t.Parallel()
	e := newEngine(t, clock.NewManual(t0), 0)
	ran := false
	op, err := e.Submit(t.Context(), operations.Spec{
		ResourceID: "group-1",
		Work:       func(context.Context) error { ran = true; return nil },
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !op.Done() || !ran || op.Err != nil {
		t.Fatalf("op = %+v, ran = %v; want finished successfully", op, ran)
	}
	// The lock is released, so the next operation on the resource is accepted.
	if _, err := e.Submit(t.Context(), operations.Spec{ResourceID: "group-1"}); err != nil {
		t.Fatalf("second Submit: %v", err)
	}
}

func TestPrepareErrorReturnedAndUnlocks(t *testing.T) {
	t.Parallel()
	e := newEngine(t, clock.NewManual(t0), 0)
	bad := errs.BadRequest(errs.Violation{Field: "metadata.name", Message: "bad"})
	_, err := e.Submit(t.Context(), operations.Spec{
		ResourceID: "group-1",
		Prepare:    func(context.Context) error { return bad },
	})
	if !errs.IsKind(err, errs.KindBadRequest) {
		t.Fatalf("Submit err = %v, want BadRequest", err)
	}
	if _, err := e.Submit(t.Context(), operations.Spec{ResourceID: "group-1"}); err != nil {
		t.Fatalf("resource stayed locked after a failed Prepare: %v", err)
	}
}

func TestDelayedCompletionAndConflict(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		e := newEngine(t, clock.Real{}, time.Minute)
		e.Start(t.Context())
		defer e.Stop()

		workErr := errors.New("disk exploded")
		op, err := e.Submit(t.Context(), operations.Spec{
			ResourceID: "group-1",
			Work:       func(context.Context) error { return workErr },
		})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		if op.Done() {
			t.Fatal("delayed operation finished immediately")
		}

		_, err = e.Submit(t.Context(), operations.Spec{ResourceID: "group-1"})
		if !errs.IsKind(err, errs.KindOperationConflict) {
			t.Fatalf("concurrent Submit err = %v, want OperationConflict", err)
		}

		time.Sleep(time.Minute) //nolint:forbidigo // advances virtual time inside the synctest bubble
		synctest.Wait()

		got, err := e.Get(t.Context(), op.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !got.Done() || !errors.Is(got.Err, workErr) {
			t.Fatalf("op after delay = %+v, want finished with work error", got)
		}
		if _, err := e.Submit(t.Context(), operations.Spec{ResourceID: "group-1"}); err != nil {
			t.Fatalf("resource stayed locked after completion: %v", err)
		}
	})
}

func TestLockIDSerializesWithTheParent(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		e := newEngine(t, clock.Real{}, time.Minute)
		e.Start(t.Context())
		defer e.Stop()

		op, err := e.Submit(t.Context(), operations.Spec{ResourceID: "backup-1", LockID: "cluster-1"})
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		if op.ResourceID != "backup-1" {
			t.Fatalf("ResourceID = %q, want the new sub-resource backup-1", op.ResourceID)
		}
		// The parent is locked; another sub-resource of it waits too.
		for _, spec := range []operations.Spec{{ResourceID: "cluster-1"}, {ResourceID: "backup-2", LockID: "cluster-1"}} {
			if _, err := e.Submit(t.Context(), spec); !errs.IsKind(err, errs.KindOperationConflict) {
				t.Fatalf("Submit(%+v) err = %v, want OperationConflict", spec, err)
			}
		}

		time.Sleep(time.Minute) //nolint:forbidigo // advances virtual time inside the synctest bubble
		synctest.Wait()

		if _, err := e.Submit(t.Context(), operations.Spec{ResourceID: "cluster-1"}); err != nil {
			t.Fatalf("parent stayed locked after completion: %v", err)
		}
	})
}

func TestDelayedSubmitRequiresStart(t *testing.T) {
	t.Parallel()
	e := newEngine(t, clock.NewManual(t0), time.Second)
	_, err := e.Submit(t.Context(), operations.Spec{ResourceID: "group-1"})
	if !errors.Is(err, operations.ErrNotStarted) {
		t.Fatalf("err = %v, want ErrNotStarted", err)
	}
}
