package operations_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
)

// A detached Work that blocks keeps its resource locked but does not hold up
// other operations, inline or delayed.
func TestDetachedDoesNotBlockOthers(t *testing.T) {
	t.Parallel()
	for _, delay := range []time.Duration{0, time.Minute} {
		synctest.Test(t, func(t *testing.T) {
			e := newEngine(t, clock.Real{}, delay)
			e.Start(t.Context())
			defer e.Stop()

			release := make(chan struct{})
			slowErr := errors.New("backend gave up")
			slow, err := e.Submit(t.Context(), operations.Spec{
				ResourceID: "cluster-1", Detached: true,
				Work: func(ctx context.Context) error {
					select {
					case <-release:
						return slowErr
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			})
			if err != nil {
				t.Fatalf("Submit detached: %v", err)
			}
			if slow.Done() {
				t.Fatal("detached operation finished before its work")
			}

			fast, err := e.Submit(t.Context(), operations.Spec{ResourceID: "cluster-2", Work: func(context.Context) error { return nil }})
			if err != nil {
				t.Fatalf("Submit other: %v", err)
			}
			time.Sleep(delay) //nolint:forbidigo // advances virtual time inside the synctest bubble
			synctest.Wait()
			if got, _ := e.Get(t.Context(), fast.ID); !got.Done() {
				t.Fatalf("delay %s: other operation blocked by the detached one", delay)
			}
			if _, err := e.Submit(t.Context(), operations.Spec{ResourceID: "cluster-1"}); !errs.IsKind(err, errs.KindOperationConflict) {
				t.Fatalf("delay %s: second operation on the busy resource: err = %v, want OperationConflict", delay, err)
			}

			close(release)
			synctest.Wait()
			got, err := e.Get(t.Context(), slow.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Done() || !errors.Is(got.Err, slowErr) {
				t.Fatalf("delay %s: detached op = %+v, want finished with the work error", delay, got)
			}
			if _, err := e.Submit(t.Context(), operations.Spec{ResourceID: "cluster-1"}); err != nil {
				t.Fatalf("delay %s: resource stayed locked: %v", delay, err)
			}
		})
	}
}

// Stop cancels detached work and leaves the operation unfinished.
func TestDetachedStopLeavesUnfinished(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		e := newEngine(t, clock.Real{}, 0)
		e.Start(t.Context())
		sawCancel := make(chan struct{})
		op, err := e.Submit(t.Context(), operations.Spec{
			ResourceID: "cluster-1", Detached: true,
			Work: func(ctx context.Context) error {
				<-ctx.Done()
				close(sawCancel)
				return ctx.Err()
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		e.Stop()
		<-sawCancel
		got, err := e.Get(t.Context(), op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Done() {
			t.Fatalf("interrupted op = %+v, want unfinished", got)
		}
	})
}

func TestDetachedRequiresStart(t *testing.T) {
	t.Parallel()
	e := newEngine(t, clock.NewManual(t0), 0)
	_, err := e.Submit(t.Context(), operations.Spec{ResourceID: "cluster-1", Detached: true})
	if !errors.Is(err, operations.ErrNotStarted) {
		t.Fatalf("err = %v, want ErrNotStarted", err)
	}
	if _, err := e.Submit(t.Context(), operations.Spec{ResourceID: "cluster-1"}); err != nil {
		t.Fatalf("resource stayed locked after the refused detached submit: %v", err)
	}
}
