package domain_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/storage/domain"
)

// startRunner runs the transfer runner until the returned stop is called.
func (f *trFixture) startRunner(t *testing.T) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.svc.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

func (f *trFixture) history(t *testing.T, id string) []domain.Iteration {
	t.Helper()
	page, err := f.svc.History(t.Context(), id, domain.TransferAPIV1, 0, "")
	if err != nil {
		t.Fatalf("History(%s): %v", id, err)
	}
	return page.Items
}

func trOK(t *testing.T, op interface{ Done() bool }, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if !op.Done() {
		t.Fatal("operation not finished")
	}
}

func TestTransferRunAfterOneIteration(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newTrFixture(t, 0)
		stop := f.startRunner(t)
		defer stop()
		id := f.createV1(t, "t", trV1Spec())
		synctest.Wait()
		calls := f.eng.callsSnapshot()
		if len(calls) != 1 || calls[0].seq != 1 {
			t.Fatalf("engine calls = %+v, want one with seq 1", calls)
		}
		if p := calls[0].plan; p.Source.Creds.Secret != "SRCSECRET" || p.Destination.Creds.Secret != "DSTSECRET" {
			t.Errorf("the engine needs the secrets: %+v", p)
		}
		rec := f.get(t, id)
		if rec.Status.State != domain.TransferStopped {
			t.Errorf("state = %v, want STOPPED after one iteration", rec.Status.State)
		}
		if it := rec.Status.LastIteration; it == nil || it.Seq != 1 || it.State != domain.IterationCompleted || it.Transferred != 1 {
			t.Errorf("last iteration = %+v", it)
		}
		if h := f.history(t, id); len(h) != 1 || h[0].State != domain.IterationCompleted {
			t.Errorf("history = %+v", h)
		}
		// Resume starts a new iteration at once.
		op, err := f.svc.Resume(t.Context(), id, domain.TransferAPIV1, nil)
		trOK(t, op, err)
		synctest.Wait()
		if calls := f.eng.callsSnapshot(); len(calls) != 2 || calls[1].seq != 2 {
			t.Fatalf("after Resume engine calls = %+v, want a second with seq 2", calls)
		}
		if st := f.get(t, id).Status.State; st != domain.TransferStopped {
			t.Errorf("state after the resumed iteration = %v, want STOPPED", st)
		}
	})
}

func TestTransferRunNEmptyIterationsAndInterval(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newTrFixture(t, 0)
		f.eng.setRun(trEmptyRun)
		stop := f.startRunner(t)
		defer stop()
		const interval = 10 * time.Minute
		spec := trV1Spec()
		spec.StopCondition = &storagepb.TransferSpec_AfterNEmptyIterations{AfterNEmptyIterations: &storagepb.TransferSpec_StopConditionAfterNEmptyIterations{EmptyIterationsThreshold: 2}}
		spec.InterIterationInterval = durationpb.New(interval)
		id := f.createV1(t, "t", spec)
		synctest.Wait()
		rec := f.get(t, id)
		if n := len(f.eng.callsSnapshot()); n != 1 || rec.Status.State != domain.TransferActive || rec.EmptyStreak != 1 {
			t.Fatalf("after the first empty iteration: %d calls, %+v streak %d", n, rec.Status, rec.EmptyStreak)
		}
		if !rec.NextRun.Equal(t0.Add(interval)) {
			t.Errorf("next run = %s, want end + interval", rec.NextRun)
		}
		f.clk.Advance(interval - time.Second)
		synctest.Wait()
		if n := len(f.eng.callsSnapshot()); n != 1 {
			t.Fatalf("an iteration started %s before the interval passed (%d calls)", time.Second, n)
		}
		f.clk.Advance(time.Second)
		synctest.Wait()
		calls := f.eng.callsSnapshot()
		if len(calls) != 2 {
			t.Fatalf("engine calls = %d, want 2 after the interval", len(calls))
		}
		if !calls[1].watermark.Equal(t0) {
			t.Errorf("second iteration watermark = %s, want the first iteration's", calls[1].watermark)
		}
		if st := f.get(t, id).Status.State; st != domain.TransferStopped {
			t.Errorf("state = %v, want STOPPED after two empty iterations", st)
		}
		h := f.history(t, id)
		if len(h) != 2 || h[0].Seq != 2 || h[1].Seq != 1 {
			t.Errorf("history = %+v, want newest first", h)
		}
		page, err := f.svc.History(t.Context(), id, domain.TransferAPIV1, 1, "")
		if err != nil || len(page.Items) != 1 || page.Items[0].Seq != 2 || page.NextPageToken == "" {
			t.Fatalf("first history page = %+v, %v", page, err)
		}
		page, err = f.svc.History(t.Context(), id, domain.TransferAPIV1, 1, page.NextPageToken)
		if err != nil || len(page.Items) != 1 || page.Items[0].Seq != 1 {
			t.Errorf("second history page = %+v, %v", page, err)
		}
		f.clk.Advance(time.Hour)
		synctest.Wait()
		if n := len(f.eng.callsSnapshot()); n != 2 {
			t.Errorf("a stopped transfer ran again (%d calls)", n)
		}
	})
}

func TestTransferNonEmptyIterationResetsStreak(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newTrFixture(t, 0)
		stop := f.startRunner(t)
		defer stop()
		spec := trV1Spec()
		spec.StopCondition = &storagepb.TransferSpec_AfterNEmptyIterations{AfterNEmptyIterations: &storagepb.TransferSpec_StopConditionAfterNEmptyIterations{EmptyIterationsThreshold: 1}}
		spec.InterIterationInterval = durationpb.New(time.Minute)
		id := f.createV1(t, "t", spec) // the default engine run transfers one object
		synctest.Wait()
		if rec := f.get(t, id); rec.Status.State != domain.TransferActive || rec.EmptyStreak != 0 {
			t.Fatalf("after a non-empty iteration: %+v streak %d", rec.Status, rec.EmptyStreak)
		}
		f.eng.setRun(trEmptyRun)
		f.clk.Advance(time.Minute)
		synctest.Wait()
		if rec := f.get(t, id); rec.Status.State != domain.TransferStopped {
			t.Errorf("after an empty iteration with threshold 1: %+v", rec.Status)
		}
	})
}

func TestTransferStopWhileWaiting(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newTrFixture(t, 0)
		stop := f.startRunner(t)
		defer stop()
		id := f.createV1(t, "t", trInfinite(trV1Spec(), time.Hour))
		synctest.Wait()
		if st := f.get(t, id).Status.State; st != domain.TransferActive {
			t.Fatalf("infinite transfer state = %v, want ACTIVE", st)
		}
		op, err := f.svc.Stop(t.Context(), id, domain.TransferAPIV1, nil)
		trOK(t, op, err)
		if st := f.get(t, id).Status.State; st != domain.TransferStopped {
			t.Errorf("state = %v, want STOPPED immediately", st)
		}
		f.clk.Advance(2 * time.Hour)
		synctest.Wait()
		if n := len(f.eng.callsSnapshot()); n != 1 {
			t.Errorf("a stopped transfer ran again (%d calls)", n)
		}
	})
}

func TestTransferStopWhileRunning(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newTrFixture(t, 0)
		var seen domain.TransferState
		var id string
		f.eng.setRun(func(ctx context.Context, seq int64, p func(domain.Iteration)) domain.Iteration {
			it := trBlockRun(ctx, seq, p)
			rec, _ := f.svc.Get(context.WithoutCancel(ctx), id, domain.TransferAPIV1)
			seen = rec.Status.State // the state while the iteration winds down
			return it
		})
		id = f.createV1(t, "t", trInfinite(trV1Spec(), time.Hour))
		stop := f.startRunner(t)
		defer stop()
		synctest.Wait()
		rec := f.get(t, id)
		if it := rec.Status.LastIteration; rec.Status.State != domain.TransferActive || it == nil || it.State != domain.IterationInProgress {
			t.Fatalf("running transfer = %+v", rec.Status)
		}
		if _, err := f.svc.Resume(t.Context(), id, domain.TransferAPIV1, nil); !errs.IsKind(err, errs.KindBadState) {
			t.Errorf("Resume while running: %v, want BadState", err)
		}
		op, err := f.svc.Stop(t.Context(), id, domain.TransferAPIV1, nil)
		trOK(t, op, err)
		if op.Err != nil {
			t.Fatal(op.Err)
		}
		if seen != domain.TransferStopping {
			t.Errorf("state during the interruption = %v, want STOPPING", seen)
		}
		rec = f.get(t, id)
		if rec.Status.State != domain.TransferStopped || rec.Status.LastIteration.State != domain.IterationInterrupted {
			t.Errorf("after Stop = %+v / %+v, want STOPPED with an INTERRUPTED iteration", rec.Status, rec.Status.LastIteration)
		}
		if h := f.history(t, id); len(h) != 1 || h[0].State != domain.IterationInterrupted {
			t.Errorf("history = %+v", h)
		}
	})
}

func TestTransferFailedIteration(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newTrFixture(t, 0)
		wantErr := domain.TransferError{Origin: domain.OriginSource, Code: "AccessDenied", Message: "denied"}
		f.eng.setRun(func(context.Context, int64, func(domain.Iteration)) domain.Iteration {
			e := wantErr
			return domain.Iteration{State: domain.IterationFailed, Error: &e}
		})
		stop := f.startRunner(t)
		defer stop()
		id := f.createV1(t, "t", trInfinite(trV1Spec(), time.Minute))
		synctest.Wait()
		rec := f.get(t, id)
		if rec.Status.State != domain.TransferFailed || rec.Status.Error == nil || *rec.Status.Error != wantErr {
			t.Fatalf("after a failed iteration = %+v, want FAILED with %+v", rec.Status, wantErr)
		}
		f.clk.Advance(time.Hour)
		synctest.Wait()
		if n := len(f.eng.callsSnapshot()); n != 1 {
			t.Errorf("a failed transfer ran again (%d calls)", n)
		}
		f.eng.setRun(nil)
		op, err := f.svc.Resume(t.Context(), id, domain.TransferAPIV1, nil)
		trOK(t, op, err)
		synctest.Wait()
		rec = f.get(t, id)
		if rec.Status.State != domain.TransferActive || rec.Status.Error != nil || rec.Status.LastIteration.Seq != 2 {
			t.Errorf("after Resume = %+v", rec.Status)
		}
	})
}

func TestTransferDeleteWhileRunning(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newTrFixture(t, 0)
		cancelled := false
		f.eng.setRun(func(ctx context.Context, seq int64, p func(domain.Iteration)) domain.Iteration {
			it := trBlockRun(ctx, seq, p)
			cancelled = true
			return it
		})
		stop := f.startRunner(t)
		defer stop()
		id := f.createV1(t, "t", trInfinite(trV1Spec(), time.Hour))
		synctest.Wait()
		op, err := f.svc.Delete(t.Context(), id, 0, domain.TransferAPIV1, nil)
		trOK(t, op, err)
		if op.Err != nil {
			t.Fatal(op.Err)
		}
		if !cancelled {
			t.Error("the running iteration was not interrupted")
		}
		if _, err := f.repo.Get(t.Context(), id); !errs.IsKind(err, errs.KindNotFound) {
			t.Errorf("after Delete: %v, want NotFound", err)
		}
		if _, err := f.repo.History(t.Context(), id, 10, ""); err != nil {
			t.Fatal(err)
		}
		if page, _ := f.repo.History(t.Context(), id, 10, ""); len(page.Items) != 0 {
			t.Errorf("history kept after Delete: %+v", page.Items)
		}
	})
}

func TestTransferProgressThrottled(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newTrFixture(t, 0)
		var id string
		var during int64
		f.eng.setRun(func(ctx context.Context, seq int64, progress func(domain.Iteration)) domain.Iteration {
			for i := int64(1); i <= 250; i++ {
				progress(domain.Iteration{Seq: seq, State: domain.IterationInProgress, Transferred: i})
			}
			rec, _ := f.svc.Get(ctx, id, domain.TransferAPIV1)
			during = rec.Status.LastIteration.Transferred
			return domain.Iteration{State: domain.IterationCompleted, Transferred: 250}
		})
		id = f.createV1(t, "t", trV1Spec())
		stop := f.startRunner(t)
		defer stop()
		synctest.Wait()
		if during != 200 {
			t.Errorf("stored progress = %d, want 200 (every 100 objects)", during)
		}
		if got := f.get(t, id).Status.LastIteration.Transferred; got != 250 {
			t.Errorf("final count = %d, want 250", got)
		}
	})
}

func TestTransferMaxParallel(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newTrFixture(t, 1)
		f.eng.setRun(trBlockRun)
		stop := f.startRunner(t)
		defer stop()
		first := f.createV1(t, "a", trInfinite(trV1Spec(), time.Hour))
		second := f.createV1(t, "b", trInfinite(trV1Spec(), time.Hour))
		synctest.Wait()
		if n := len(f.eng.callsSnapshot()); n != 1 {
			t.Fatalf("%d iterations running, want 1 (MaxParallel)", n)
		}
		op, err := f.svc.Stop(t.Context(), first, domain.TransferAPIV1, nil)
		trOK(t, op, err)
		synctest.Wait()
		if n := len(f.eng.callsSnapshot()); n != 2 {
			t.Fatalf("%d iterations after a slot freed, want 2", n)
		}
		if it := f.get(t, second).Status.LastIteration; it == nil || it.State != domain.IterationInProgress {
			t.Errorf("second transfer = %+v, want running", it)
		}
	})
}

func TestTransferUpdateReschedules(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newTrFixture(t, 0)
		stop := f.startRunner(t)
		defer stop()
		id := f.createV1(t, "t", trInfinite(trV1Spec(), time.Hour))
		synctest.Wait()
		spec := f.get(t, id).V1.GetSpec()
		spec.InterIterationInterval = durationpb.New(5 * time.Minute)
		op, err := f.svc.UpdateV1(t.Context(), domain.UpdateTransferV1Cmd{Metadata: &commonpb.ResourceMetadata{Id: id}, Spec: spec, ResetMask: mask.New()})
		trOK(t, op, err)
		if op.Err != nil {
			t.Fatal(op.Err)
		}
		if next := f.get(t, id).NextRun; !next.Equal(t0.Add(5 * time.Minute)) {
			t.Errorf("next run = %s, want last end + the new interval", next)
		}
		f.clk.Advance(5 * time.Minute)
		synctest.Wait()
		if n := len(f.eng.callsSnapshot()); n != 2 {
			t.Errorf("%d iterations, want the second after the new interval", n)
		}
	})
}

func TestTransferRecovery(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newTrFixture(t, 0)
		ctx := t.Context()
		running := f.createV1(t, "running", trInfinite(trV1Spec(), time.Hour))
		stopping := f.createV1(t, "stopping", trInfinite(trV1Spec(), time.Hour))
		deleting := f.createV1(t, "deleting", trInfinite(trV1Spec(), time.Hour))
		// The state a crash leaves behind.
		inProgress := domain.Iteration{Seq: 1, State: domain.IterationInProgress, Start: t0}
		for id, state := range map[string]domain.TransferState{
			running: domain.TransferActive, stopping: domain.TransferStopping, deleting: domain.TransferDeleting,
		} {
			rec, _ := f.repo.Get(ctx, id)
			rec.Status.State, rec.Status.LastIteration = state, &inProgress
			if err := f.repo.Update(ctx, rec); err != nil {
				t.Fatal(err)
			}
			if err := f.repo.PutIteration(ctx, id, inProgress); err != nil {
				t.Fatal(err)
			}
		}
		f.clk.Advance(time.Minute)
		stop := f.startRunner(t)
		defer stop()
		synctest.Wait()
		if _, err := f.repo.Get(ctx, deleting); !errs.IsKind(err, errs.KindNotFound) {
			t.Errorf("DELETING transfer: %v, want deleted", err)
		}
		rec := f.get(t, stopping)
		if rec.Status.State != domain.TransferStopped || rec.Status.LastIteration.State != domain.IterationInterrupted {
			t.Errorf("STOPPING transfer after recovery = %+v / %+v", rec.Status, rec.Status.LastIteration)
		}
		if h := f.history(t, stopping); len(h) != 1 || h[0].State != domain.IterationInterrupted || !h[0].End.Equal(t0.Add(time.Minute)) {
			t.Errorf("STOPPING history = %+v", h)
		}
		h := f.history(t, running)
		if len(h) != 2 || h[1].State != domain.IterationInterrupted || h[0].Seq != 2 || h[0].State != domain.IterationCompleted {
			t.Errorf("ACTIVE transfer history = %+v, want the interrupted iteration and a new one", h)
		}
	})
}
