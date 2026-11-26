package domain

import (
	"cmp"
	"context"
	"errors"
	"sync"
	"time"

	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	storagev1alpha1pb "github.com/nebius/gosdk/proto/nebius/storage/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

const (
	// runnerMaxWait bounds how long the runner sleeps between scans.
	runnerMaxWait = time.Minute
	// Progress of a running iteration is stored at most every
	// progressEvery objects or when progressPeriod has passed.
	progressEvery  = 100
	progressPeriod = time.Second
)

// PlanOf returns what a stored transfer does (with its secrets), whichever
// API version it was created with.
func (s *TransferService) PlanOf(rec *TransferRecord) (Plan, error) {
	switch {
	case rec.API == TransferAPIV1 && rec.V1.GetSpec() != nil:
		return s.planV1(rec.V1.GetSpec())
	case rec.API == TransferAPIV1Alpha1 && rec.Alpha.GetSpec() != nil:
		return s.planAlpha(rec.Alpha.GetSpec())
	}
	return Plan{}, errs.Internal("transfer " + rec.Metadata().GetId() + " has no spec")
}

func (s *TransferService) planV1(spec *storagepb.TransferSpec) (Plan, error) {
	p := Plan{
		DeleteInDestination: spec.GetEnableDeletesInDestination(),
		TouchUnmanaged:      spec.GetTouchUnmanaged(),
	}
	src := spec.GetSource()
	p.Source.Prefix = src.GetPrefix()
	switch {
	case src.GetNebius() != nil:
		n := src.GetNebius()
		p.Source.Kind, p.Source.Region, p.Source.Bucket = ProviderNebius, n.GetRegion(), n.GetBucketName()
		p.Source.Creds = v1Creds(n.GetAccessKey())
	case src.GetS3Compatible() != nil:
		c := src.GetS3Compatible()
		p.Source.Kind, p.Source.Endpoint, p.Source.Region, p.Source.Bucket = ProviderS3Compatible, c.GetEndpoint(), c.GetRegion(), c.GetBucketName()
		p.Source.Creds = v1Creds(c.GetAccessKey())
	case src.GetAzureBlobStorage() != nil:
		a := src.GetAzureBlobStorage()
		p.Source.Kind, p.Source.Endpoint, p.Source.Bucket = ProviderAzureBlob, a.GetEndpoint(), a.GetContainerName()
		p.Source.Creds = Credentials{Anonymous: true}
		if acc := a.GetAzureStorageAccount(); acc != nil {
			p.Source.Creds = Credentials{AzureAccount: acc.GetAccountName(), AzureKey: acc.GetAccessKey()}
		}
	default:
		return Plan{}, errs.Internal("transfer source has no provider")
	}
	dst := spec.GetDestination()
	p.Destination.Prefix = dst.GetPrefix()
	switch {
	case dst.GetNebius() != nil:
		n := dst.GetNebius()
		p.Destination.Kind, p.Destination.Region, p.Destination.Bucket = ProviderNebius, n.GetRegion(), n.GetBucketName()
		p.Destination.Creds = v1Creds(n.GetAccessKey())
	case dst.GetS3Compatible() != nil:
		c := dst.GetS3Compatible()
		p.Destination.Kind, p.Destination.Endpoint, p.Destination.Region, p.Destination.Bucket = ProviderS3Compatible, c.GetEndpoint(), c.GetRegion(), c.GetBucketName()
		p.Destination.Creds = v1Creds(c.GetAccessKey())
	default:
		return Plan{}, errs.Internal("transfer destination has no provider")
	}
	p.Overwrite = OverwriteNever
	if spec.GetOverwriteStrategy() == storagepb.TransferSpec_IF_NEWER {
		p.Overwrite = OverwriteIfNewer
	}
	p.RequestsPerSecond = cmp.Or(spec.GetLimiters().GetRequestsPerSecond(), DefaultTransferRequestsPerSecond)
	p.BandwidthBytesPerSecond = cmp.Or(spec.GetLimiters().GetBandwidthBytesPerSecond(), DefaultTransferBandwidth)
	switch {
	case spec.GetAfterOneIteration() != nil:
		p.Stop = StopCondition{Kind: StopAfterOneIteration}
	case spec.GetAfterNEmptyIterations() != nil:
		p.Stop = StopCondition{Kind: StopAfterNEmptyIterations, EmptyThreshold: spec.GetAfterNEmptyIterations().GetEmptyIterationsThreshold()}
	default:
		p.Stop = StopCondition{Kind: StopInfinite}
	}
	p.Interval = DefaultTransferInterval
	if d := spec.GetInterIterationInterval(); d != nil {
		p.Interval = d.AsDuration()
	}
	return p, nil
}

func v1Creds(ak *storagepb.TransferCredentialsAccessKey) Credentials {
	if ak == nil {
		return Credentials{Anonymous: true}
	}
	return Credentials{AccessKeyID: ak.GetAccessKeyId(), Secret: ak.GetSecretAccessKey()}
}

func alphaCreds(c *storagev1alpha1pb.TransferSpec_BucketCredentials) Credentials {
	switch {
	case c.GetAccessKey() != nil:
		return Credentials{AccessKeyID: c.GetAccessKey().GetAccessKeyId(), Secret: c.GetAccessKey().GetSecretAccessKey()}
	case c.GetAzureAccessKey() != nil:
		return Credentials{AzureAccount: c.GetAzureAccessKey().GetAccountName(), AzureKey: c.GetAzureAccessKey().GetAccessKey()}
	}
	return Credentials{Anonymous: true} // absent credentials are anonymous
}

func (s *TransferService) planAlpha(spec *storagev1alpha1pb.TransferSpec) (Plan, error) {
	src, dst := spec.GetSource(), spec.GetDestination()
	p := Plan{TouchUnmanaged: true}
	p.Source = Side{
		Kind: ProviderS3Compatible, Endpoint: src.GetEndpoint(), Region: src.GetRegion(), Bucket: src.GetBucketName(),
		Prefix: src.GetPrefix(), Creds: alphaCreds(src.GetCredentials()),
	}
	if src.GetCredentials().GetAzureAccessKey() != nil {
		p.Source.Kind, p.Source.Region = ProviderAzureBlob, ""
	}
	p.Destination = Side{Bucket: dst.GetBucketName(), Prefix: dst.GetPrefix(), Creds: alphaCreds(dst.GetCredentials())}
	if dst.GetEndpoint() == "" {
		p.Destination.Kind, p.Destination.Region = ProviderNebius, cmp.Or(dst.GetRegion(), s.region)
	} else {
		p.Destination.Kind, p.Destination.Endpoint, p.Destination.Region = ProviderS3Compatible, dst.GetEndpoint(), dst.GetRegion()
	}
	switch spec.GetOverwriteStrategy() {
	case storagev1alpha1pb.TransferSpec_IF_NEWER:
		p.Overwrite = OverwriteIfNewer
	case storagev1alpha1pb.TransferSpec_ALWAYS:
		p.Overwrite = OverwriteAlways
	case storagev1alpha1pb.TransferSpec_NEVER, storagev1alpha1pb.TransferSpec_OVERWRITE_STRATEGY_UNSPECIFIED:
		p.Overwrite = OverwriteNever
	default:
		p.Overwrite = OverwriteNever
	}
	p.RequestsPerSecond = cmp.Or(src.GetLimiters().GetRequestsPerSecond(), DefaultTransferRequestsPerSecond)
	p.BandwidthBytesPerSecond = cmp.Or(src.GetLimiters().GetBandwidthBytesPerSecond(), DefaultTransferBandwidth)
	switch {
	case spec.GetAfterOneIteration() != nil:
		p.Stop = StopCondition{Kind: StopAfterOneIteration}
	case spec.GetAfterNEmptyIterations() != nil:
		p.Stop = StopCondition{Kind: StopAfterNEmptyIterations, EmptyThreshold: spec.GetAfterNEmptyIterations().GetEmptyIterationsThreshold()}
	default:
		p.Stop = StopCondition{Kind: StopInfinite}
	}
	p.Interval = DefaultTransferInterval
	if d := spec.GetInterIterationInterval(); d != nil {
		p.Interval = d.AsDuration()
	}
	return p, nil
}

// Run is the transfer runner: it starts the iterations of ACTIVE transfers
// when they are due (at most MaxParallel at a time) and returns when ctx is
// done, after every iteration it started has stopped. The module runs it in
// one goroutine.
func (s *TransferService) Run(ctx context.Context) {
	if err := s.recoverState(ctx); err != nil {
		s.log.ErrorContext(ctx, "recover transfers", "error", err)
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		next := s.startDue(ctx, &wg)
		wait := runnerMaxWait
		if !next.IsZero() {
			wait = min(wait, next.Sub(s.clk.Now()))
		}
		select {
		case <-s.clk.After(wait):
		case <-s.wake:
		case <-ctx.Done():
			return
		}
	}
}

// recoverState repairs what a restart interrupted: running iterations are
// INTERRUPTED, STOPPING transfers STOPPED and DELETING ones deleted.
func (s *TransferService) recoverState(ctx context.Context) error {
	recs, err := s.repo.ListAll(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var errList []error
	for _, rec := range recs {
		id := rec.Metadata().GetId()
		if rec.Status.State == TransferDeleting {
			errList = append(errList, s.repo.Delete(ctx, id))
			continue
		}
		changed := false
		if it := rec.Status.LastIteration; it != nil && it.State == IterationInProgress {
			it.State, it.End = IterationInterrupted, s.clk.Now()
			errList = append(errList, s.repo.PutIteration(ctx, id, *it))
			changed = true
		}
		if rec.Status.State == TransferStopping {
			rec.Status.State = TransferStopped
			changed = true
		}
		if changed {
			errList = append(errList, s.repo.Update(ctx, rec))
		}
	}
	return errors.Join(errList...)
}

// startDue starts every due iteration it has a slot for and returns the
// earliest NextRun of the waiting ACTIVE transfers (zero when none).
func (s *TransferService) startDue(ctx context.Context, wg *sync.WaitGroup) time.Time {
	recs, err := s.repo.ListAll(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "list transfers", "error", err)
		return time.Time{}
	}
	now := s.clk.Now()
	var next time.Time
	for _, rec := range recs {
		if rec.Status.State != TransferActive {
			continue
		}
		if rec.NextRun.After(now) {
			if next.IsZero() || rec.NextRun.Before(next) {
				next = rec.NextRun
			}
			continue
		}
		if s.busy() {
			continue // an ending iteration wakes the runner
		}
		if err := s.start(ctx, wg, rec.Metadata().GetId()); err != nil {
			s.log.ErrorContext(ctx, "start transfer iteration", "transfer_id", rec.Metadata().GetId(), "error", err)
		}
	}
	return next
}

func (s *TransferService) busy() bool {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return len(s.running) >= s.maxParallel
}

// start records a new IN_PROGRESS iteration and runs it in a goroutine owned
// by wg. The record is re-read under mu, so a Stop or Delete either sees the
// iteration as running or happened before it started.
func (s *TransferService) start(ctx context.Context, wg *sync.WaitGroup, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.repo.Get(ctx, id)
	if err != nil {
		return err
	}
	now := s.clk.Now()
	if rec.Status.State != TransferActive || rec.NextRun.After(now) || s.runningOf(id) != nil {
		return nil
	}
	it := Iteration{Seq: 1, State: IterationInProgress, Start: now}
	if last := rec.Status.LastIteration; last != nil {
		it.Seq = last.Seq + 1
	}
	plan, planErr := s.PlanOf(rec)
	if err := s.repo.PutIteration(ctx, id, it); err != nil {
		return err
	}
	rec.Status.LastIteration = &it
	if err := s.repo.Update(ctx, rec); err != nil {
		return err
	}
	iterCtx, cancel := context.WithCancel(ctx)
	ri := &runningIteration{cancel: cancel, done: make(chan struct{})}
	s.runMu.Lock()
	s.running[id] = ri
	s.runMu.Unlock()
	watermark := rec.Watermark
	wg.Go(func() {
		defer cancel()
		var res Iteration
		if planErr != nil {
			res = it
			res.State, res.End = IterationFailed, s.clk.Now()
			res.Error = &TransferError{Code: "InvalidTransfer", Message: planErr.Error()}
		} else {
			res, watermark = s.engine.Run(iterCtx, plan, it.Seq, watermark, s.progress(iterCtx, id, it.Seq))
		}
		s.finish(context.WithoutCancel(iterCtx), id, res, watermark, ri)
	})
	return nil
}

// progress returns the engine's progress callback: it stores the running
// iteration's counters, throttled.
func (s *TransferService) progress(ctx context.Context, id string, seq int64) func(Iteration) {
	n, last := 0, s.clk.Now()
	return func(it Iteration) {
		s.mu.Lock()
		defer s.mu.Unlock()
		n++
		now := s.clk.Now()
		if n%progressEvery != 0 && now.Sub(last) < progressPeriod {
			return
		}
		last = now
		rec, err := s.repo.Get(ctx, id)
		if err != nil || rec.Status.LastIteration == nil || rec.Status.LastIteration.Seq != seq {
			return
		}
		it.State = IterationInProgress
		rec.Status.LastIteration = &it
		if err := s.repo.Update(ctx, rec); err != nil {
			s.log.WarnContext(ctx, "store transfer progress", "transfer_id", id, "error", err)
		}
	}
}

// finish stores a finished iteration and moves the transfer on: the next
// iteration after the interval, STOPPED when its stop condition is met or it
// was being stopped, FAILED when the iteration failed. A transfer being
// deleted is left to Delete.
func (s *TransferService) finish(ctx context.Context, id string, res Iteration, watermark time.Time, ri *runningIteration) {
	s.mu.Lock()
	defer func() {
		s.runMu.Lock()
		delete(s.running, id)
		s.runMu.Unlock()
		close(ri.done)
		s.mu.Unlock()
		s.notify()
	}()
	if res.State == IterationInProgress { // the engine returned without finishing
		res.State = IterationInterrupted
	}
	if res.End.IsZero() {
		res.End = s.clk.Now()
	}
	rec, err := s.repo.Get(ctx, id)
	if err != nil {
		if !errs.IsKind(err, errs.KindNotFound) {
			s.log.ErrorContext(ctx, "finish transfer iteration", "transfer_id", id, "error", err)
		}
		return
	}
	if rec.Status.State == TransferDeleting {
		return
	}
	if err := s.repo.PutIteration(ctx, id, res); err != nil {
		s.log.ErrorContext(ctx, "store transfer iteration", "transfer_id", id, "error", err)
	}
	s.advance(rec, res, watermark)
	if err := s.repo.Update(ctx, rec); err != nil && !errs.IsKind(err, errs.KindNotFound) {
		s.log.ErrorContext(ctx, "store transfer", "transfer_id", id, "error", err)
	}
}

// advance applies a finished iteration to the record's runner state.
func (s *TransferService) advance(rec *TransferRecord, res Iteration, watermark time.Time) {
	rec.Status.LastIteration = &res
	if res.State == IterationCompleted {
		rec.Watermark = watermark
		if res.Empty() {
			rec.EmptyStreak++
		} else {
			rec.EmptyStreak = 0
		}
	}
	switch rec.Status.State {
	case TransferStopping:
		rec.Status.State = TransferStopped
		return
	case TransferActive:
	case TransferStopped, TransferFailing, TransferFailed, TransferDeleting:
		return
	}
	plan, err := s.PlanOf(rec)
	if err != nil {
		rec.Status.State = TransferFailed
		rec.Status.Error = &TransferError{Code: "InvalidTransfer", Message: err.Error()}
		return
	}
	switch res.State {
	case IterationCompleted:
		if plan.Stop.Kind == StopAfterOneIteration || plan.Stop.Kind == StopAfterNEmptyIterations && rec.EmptyStreak >= plan.Stop.EmptyThreshold {
			rec.Status.State = TransferStopped
			return
		}
		rec.NextRun = res.End.Add(plan.Interval)
	case IterationFailed:
		rec.Status.State = TransferFailed
		rec.Status.Error = cmp.Or(cloneTransferError(res.Error), &TransferError{Code: "InternalError", Message: "the iteration failed"})
	case IterationInProgress, IterationInterrupted: // a shutdown: run again after a restart
		rec.NextRun = res.End
	}
}
