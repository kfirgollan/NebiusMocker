package domain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// maxObjectKeyLen is the S3 limit on object key length in bytes.
const maxObjectKeyLen = 1024

// TransferEngine runs transfer iterations (see transfer_types.go).
type TransferEngine struct {
	endpoints Endpoints
	managed   ManagedObjects
	clk       clock.Clock
	log       *slog.Logger
}

// NewTransferEngine returns an engine that opens sides through endpoints.
func NewTransferEngine(endpoints Endpoints, managed ManagedObjects, clk clock.Clock, log *slog.Logger) *TransferEngine {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &TransferEngine{endpoints: endpoints, managed: managed, clk: clk, log: log}
}

// Run executes iteration seq of plan. progress, when non-nil, receives an
// in-progress snapshot after every object. Run returns the finished iteration
// (IterationCompleted, IterationFailed with Error set, or IterationInterrupted
// when ctx is cancelled) and the watermark for the next iteration.
//
// Upstream: docs.nebius.com/object-storage/transfer/overview.
func (e *TransferEngine) Run(ctx context.Context, plan Plan, seq int64, watermark time.Time, progress func(Iteration)) (Iteration, time.Time) {
	r := &transferRun{
		e:         e,
		plan:      plan,
		watermark: watermark,
		progress:  progress,
		reqs:      newTokenBucket(e.clk, plan.RequestsPerSecond),
		bytes:     newTokenBucket(e.clk, plan.BandwidthBytesPerSecond),
		srcKeys:   make(map[string]struct{}),
		it:        Iteration{Seq: seq, State: IterationInProgress, Start: e.clk.Now()},
	}
	e.log.DebugContext(ctx, "transfer iteration started", "seq", seq,
		"source", plan.Source.Identity(), "destination", plan.Destination.Identity())
	err := r.run(ctx)
	r.finish(ctx, err)
	e.log.InfoContext(ctx, "transfer iteration finished", "seq", seq, "state", int(r.it.State),
		"discovered", r.it.Discovered, "transferred", r.it.Transferred, "skipped", r.it.Skipped,
		"deleted", r.it.Deleted, "bytes", r.it.TransferredBytes)
	if r.it.State != IterationCompleted {
		return r.it, watermark
	}
	return r.it, r.nextWatermark()
}

// transferRun is the state of one iteration.
type transferRun struct {
	e         *TransferEngine
	plan      Plan
	watermark time.Time
	progress  func(Iteration)
	reqs      *tokenBucket
	bytes     *tokenBucket
	src, dst  Endpoint
	// srcKeys holds every listed source key (watermark-skipped ones too).
	srcKeys map[string]struct{}
	maxLM   time.Time
	it      Iteration
}

// sideError attributes an error to the side it came from.
type sideError struct {
	origin ErrorOrigin
	err    error
}

func (s *sideError) Error() string { return s.err.Error() }
func (s *sideError) Unwrap() error { return s.err }

func atSource(err error) error      { return &sideError{origin: OriginSource, err: err} }
func atDestination(err error) error { return &sideError{origin: OriginDestination, err: err} }
func atManaged(err error) error     { return &sideError{origin: OriginUnspecified, err: err} }

func (r *transferRun) run(ctx context.Context) error {
	var err error
	if r.src, err = r.e.endpoints.Open(ctx, r.plan.Source); err != nil {
		return atSource(err)
	}
	if r.dst, err = r.e.endpoints.Open(ctx, r.plan.Destination); err != nil {
		return atDestination(err)
	}
	if err := r.copyAll(ctx); err != nil {
		return err
	}
	if r.plan.DeleteInDestination {
		return r.deleteOrphans(ctx)
	}
	return nil
}

// finish sets the final state, error, end time and throughput.
func (r *transferRun) finish(ctx context.Context, err error) {
	r.it.End = r.e.clk.Now()
	secs := max(1, int64(r.it.End.Sub(r.it.Start)/time.Second))
	r.it.ThroughputBytes = r.it.TransferredBytes / secs
	switch {
	case err == nil:
		r.it.State = IterationCompleted
	case ctx.Err() != nil:
		r.it.State = IterationInterrupted
	default:
		r.it.State = IterationFailed
		r.it.Error = transferErrorOf(err)
		r.e.log.WarnContext(ctx, "transfer iteration failed", "seq", r.it.Seq, "error", err)
	}
}

func transferErrorOf(err error) *TransferError {
	te := &TransferError{Code: "InternalError", Message: err.Error()}
	if se, ok := errors.AsType[*sideError](err); ok {
		te.Origin = se.origin
		te.Message = se.err.Error()
	}
	if ee, ok := errors.AsType[*EndpointError](err); ok {
		te.Code, te.Message = ee.Code, ee.Message
	}
	return te
}

func (r *transferRun) nextWatermark() time.Time {
	if r.maxLM.After(r.watermark) {
		return r.maxLM
	}
	return r.watermark
}

func (r *transferRun) report() {
	if r.progress != nil {
		r.progress(r.it)
	}
}

// copyAll lists the source and brings every object to the destination. One
// request token is charged per started list page.
func (r *transferRun) copyAll(ctx context.Context) error {
	if err := r.reqs.Take(ctx, 1); err != nil {
		return err
	}
	var n int64
	for obj, err := range r.src.List(ctx, r.plan.Source.Prefix) {
		if err != nil {
			return atSource(err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := r.copyOne(ctx, obj); err != nil {
			return err
		}
		r.report()
		n++
		if n%TransferListPage == 0 {
			if err := r.reqs.Take(ctx, 1); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *transferRun) copyOne(ctx context.Context, obj ObjectMeta) error {
	r.it.Discovered++
	r.srcKeys[obj.Key] = struct{}{}
	if obj.LastModified.After(r.maxLM) {
		r.maxLM = obj.LastModified
	}
	dstKey := r.plan.DestinationKey(obj.Key)
	if dstKey == "" || len(dstKey) > maxObjectKeyLen {
		return atDestination(&EndpointError{
			Code:    "InvalidObjectName",
			Message: fmt.Sprintf("source object %q maps to invalid destination key %q", obj.Key, dstKey),
		})
	}
	if !r.watermark.IsZero() && obj.LastModified.Before(r.watermark) {
		r.it.Skipped++
		return nil
	}
	dst, err := r.dst.Head(ctx, dstKey)
	switch {
	case errs.IsKind(err, errs.KindNotFound):
		return r.copyData(ctx, obj.Key, dstKey)
	case err != nil:
		return atDestination(err)
	}
	allowed, err := r.mayOverwrite(ctx, obj, dst)
	if err != nil {
		return err
	}
	if !allowed {
		r.it.Skipped++
		return nil
	}
	if dst.ETag == obj.ETag && dst.Size == obj.Size {
		return r.syncMetadata(ctx, obj.Key, dst)
	}
	return r.copyData(ctx, obj.Key, dstKey)
}

// mayOverwrite applies the overwrite strategy to an existing destination object.
func (r *transferRun) mayOverwrite(ctx context.Context, src, dst ObjectMeta) (bool, error) {
	switch r.plan.Overwrite {
	case OverwriteAlways:
		return true, nil
	case OverwriteIfNewer:
		if !src.LastModified.After(dst.LastModified) {
			return false, nil
		}
		return r.touchable(ctx, dst)
	case OverwriteNever:
		return false, nil
	default:
		return false, nil
	}
}

// touchable reports whether the transfer may modify the destination object.
func (r *transferRun) touchable(ctx context.Context, dst ObjectMeta) (bool, error) {
	if r.plan.TouchUnmanaged {
		return true, nil
	}
	ok, err := r.e.managed.Managed(ctx, r.plan.Destination.Identity(), dst.Key, dst.ETag)
	if err != nil {
		return false, atManaged(err)
	}
	return ok, nil
}

// syncMetadata handles identical data: metadata is replaced in place when it
// differs ("CopyObject with MetadataDirective=REPLACE, no data re-transfer").
func (r *transferRun) syncMetadata(ctx context.Context, srcKey string, dst ObjectMeta) error {
	if err := r.reqs.Take(ctx, 1); err != nil {
		return err
	}
	src, err := r.src.Head(ctx, srcKey)
	if errs.IsKind(err, errs.KindNotFound) {
		// Deleted since it was listed: nothing to bring over.
		r.it.Skipped++
		return nil
	}
	if err != nil {
		return atSource(err)
	}
	if sameAttributes(src, dst) {
		r.it.Skipped++
		return nil
	}
	src.Key = dst.Key
	etag, err := r.dst.ReplaceMetadata(ctx, dst.Key, src)
	if err != nil {
		return atDestination(err)
	}
	r.it.Transferred++
	return r.markManaged(ctx, dst.Key, etag)
}

func sameAttributes(a, b ObjectMeta) bool {
	return a.ContentType == b.ContentType &&
		a.CacheControl == b.CacheControl &&
		a.ContentDisposition == b.ContentDisposition &&
		a.ContentEncoding == b.ContentEncoding &&
		a.ContentLanguage == b.ContentLanguage &&
		maps.Equal(a.Metadata, b.Metadata)
}

// copyData streams the source object to the destination through the
// bandwidth limiter.
func (r *transferRun) copyData(ctx context.Context, srcKey, dstKey string) error {
	if err := r.reqs.Take(ctx, 1); err != nil {
		return err
	}
	body, meta, err := r.src.Get(ctx, srcKey)
	if errs.IsKind(err, errs.KindNotFound) {
		r.it.Skipped++
		return nil
	}
	if err != nil {
		return atSource(err)
	}
	defer func() { _ = body.Close() }()
	meta.Key = dstKey
	etag, err := r.dst.Put(ctx, dstKey, newLimitedReader(ctx, body, r.bytes), meta)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		return atDestination(err)
	}
	r.it.Transferred++
	r.it.TransferredBytes += meta.Size
	return r.markManaged(ctx, dstKey, etag)
}

func (r *transferRun) markManaged(ctx context.Context, key, etag string) error {
	if err := r.e.managed.MarkManaged(ctx, r.plan.Destination.Identity(), key, etag); err != nil {
		return atManaged(err)
	}
	return nil
}

// deleteOrphans removes destination objects under the destination prefix
// whose source counterpart was not listed (enable_deletes_in_destination).
func (r *transferRun) deleteOrphans(ctx context.Context) error {
	prefix := r.plan.Destination.Prefix
	for obj, err := range r.dst.List(ctx, prefix) {
		if err != nil {
			return atDestination(err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasPrefix(obj.Key, prefix) {
			continue
		}
		if _, ok := r.srcKeys[r.plan.Source.Prefix+obj.Key[len(prefix):]]; ok {
			continue
		}
		deleted, err := r.deleteOne(ctx, obj)
		if err != nil {
			return err
		}
		if deleted {
			r.report()
		}
	}
	return nil
}

func (r *transferRun) deleteOne(ctx context.Context, obj ObjectMeta) (bool, error) {
	ok, err := r.touchable(ctx, obj)
	if err != nil || !ok {
		return false, err
	}
	err = r.dst.Delete(ctx, obj.Key)
	if err != nil && !errs.IsKind(err, errs.KindNotFound) {
		return false, atDestination(err)
	}
	if err := r.e.managed.Forget(ctx, r.plan.Destination.Identity(), obj.Key); err != nil {
		return false, atManaged(err)
	}
	r.it.Deleted++
	return true, nil
}
