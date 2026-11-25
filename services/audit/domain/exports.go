package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/audit"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
	"github.com/kfirgollan/NebiusMocker/pkg/reqctx"
)

const (
	defaultExportPageSize = 100
	maxExportPageSize     = 1000

	// defaultObjectPrefix is used when the destination sets no object_prefix.
	defaultObjectPrefix = "auditlogs"
	// maxEventsPerObject bounds the events in one exported object.
	maxEventsPerObject = 10_000
	exportContentType  = "application/x-ndjson"
)

const (
	stateRunning  = auditv2pb.AuditEventExportState_AUDIT_EVENT_EXPORT_STATE_RUNNING
	stateDone     = auditv2pb.AuditEventExportState_AUDIT_EVENT_EXPORT_STATE_DONE
	stateFailed   = auditv2pb.AuditEventExportState_AUDIT_EVENT_EXPORT_STATE_FAILED
	stateCanceled = auditv2pb.AuditEventExportState_AUDIT_EVENT_EXPORT_STATE_CANCELED
)

// errSettled aborts a Modify whose export already left RUNNING.
var errSettled = errors.New("audit: export already finished")

// ExportDeps are the dependencies of an ExportService. All but Log are required.
type ExportDeps struct {
	Exports   ExportRepository
	Events    EventSource
	Hierarchy Hierarchy
	Buckets   Buckets
	Writer    ObjectWriter
	Ops       Operations
	IDs       IDGenerator
	Clock     clock.Clock
	// Regions are the emulated region names; the first holds every event.
	Regions []string
	// Delay is how long an export stays RUNNING before it writes its
	// objects. Zero exports inside Start.
	Delay time.Duration
	Log   *slog.Logger
}

// ExportService implements AuditEventExportService. An export moves from
// RUNNING to exactly one of DONE, FAILED or CANCELED.
type ExportService struct {
	d       ExportDeps
	regions regionSet

	mu   sync.Mutex    // guards the runner fields below
	stop chan struct{} // closed by StopRunner; nil while the runner is not running
	wg   sync.WaitGroup
}

// NewExportService wires an ExportService.
func NewExportService(d ExportDeps) (*ExportService, error) {
	if d.Exports == nil || d.Events == nil || d.Hierarchy == nil || d.Buckets == nil || d.Writer == nil ||
		d.Ops == nil || d.IDs == nil || d.Clock == nil {
		return nil, errors.New("audit: export service dependencies must not be nil")
	}
	if d.Delay < 0 {
		return nil, fmt.Errorf("audit: export delay must not be negative, got %s", d.Delay)
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	rs, err := newRegionSet(d.Regions)
	if err != nil {
		return nil, err
	}
	return &ExportService{d: d, regions: rs}, nil
}

// StartExportCmd is the input of Start.
type StartExportCmd struct {
	Metadata *commonpb.ResourceMetadata
	Spec     *auditv2pb.AuditEventExportSpec
	// Region is the region the export runs in; it only exports that region's events.
	Region string
	// Request is echoed in Operation.request.
	Request proto.Message
}

// Start validates the request, creates a RUNNING export and returns its
// operation. With a zero delay the export has finished (DONE or FAILED) when
// Start returns; otherwise the runner finishes it after the delay.
func (s *ExportService) Start(ctx context.Context, cmd StartExportCmd) (*operations.Operation, error) {
	exp, err := s.newExport(ctx, cmd)
	if err != nil {
		return nil, err
	}
	id := exp.Resource.GetMetadata().GetId()
	op, err := s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Start audit event export",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     cmd.Request,
		Prepare:     func(ctx context.Context) error { return s.d.Exports.Create(ctx, exp) },
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.modify(ctx, id, func(e *auditv2pb.AuditEventExport) error {
		e.Status.ExportOperationId = op.ID
		return nil
	}); err != nil {
		return nil, fmt.Errorf("record export operation: %w", err)
	}
	if s.d.Delay == 0 {
		if err := s.run(ctx, id); err != nil {
			return nil, err
		}
		return op, nil
	}
	return op, s.schedule(ctx, id)
}

// Cancel moves a RUNNING export to CANCELED. The transition happens in the
// operation's Prepare step, not in Work, on purpose: Cancel takes effect
// immediately and a pending run, which re-reads the export, then sees CANCELED
// and writes nothing.
func (s *ExportService) Cancel(ctx context.Context, id string, request proto.Message) (*operations.Operation, error) {
	return s.d.Ops.Submit(ctx, operations.Spec{
		ResourceID:  id,
		Description: "Cancel audit event export",
		CreatedBy:   reqctx.PrincipalFrom(ctx).ID,
		Request:     request,
		Prepare: func(ctx context.Context) error {
			now := timestamppb.New(s.d.Clock.Now())
			_, err := s.modify(ctx, id, func(e *auditv2pb.AuditEventExport) error {
				if st := e.GetStatus().GetState(); st != stateRunning {
					return errs.BadState(id, "export is "+stateName(st))
				}
				e.Status.State = stateCanceled
				e.Status.FinishedAt = now
				e.Status.Message = "canceled by user"
				return nil
			})
			return err
		},
	})
}

// Get returns an export by ID.
func (s *ExportService) Get(ctx context.Context, id string) (*auditv2pb.AuditEventExport, error) {
	e, err := s.d.Exports.Get(ctx, id)
	return e.Resource, err
}

// List returns one page of the exports a parent started in region, in
// creation order.
func (s *ExportService) List(ctx context.Context, parentID, region string, pageSize int64, pageToken string) (ExportPage, error) {
	if err := s.regions.check("region", region); err != nil {
		return ExportPage{}, err
	}
	if pageSize < 0 || pageSize > maxExportPageSize {
		return ExportPage{}, errs.BadRequest(errs.Violation{
			Field: "page_size", Message: fmt.Sprintf("must be between 0 and %d", maxExportPageSize),
		})
	}
	size := int(pageSize)
	if size == 0 {
		size = defaultExportPageSize
	}
	return s.d.Exports.List(ctx, parentID, region, size, pageToken)
}

// newExport validates cmd and builds the RUNNING export it asks for.
func (s *ExportService) newExport(ctx context.Context, cmd StartExportCmd) (Export, error) {
	if err := s.regions.check("region", cmd.Region); err != nil {
		return Export{}, err
	}
	md := cmd.Metadata
	if md.GetId() != "" {
		return Export{}, errs.BadRequest(errs.Violation{Field: "metadata.id", Message: "must not be set on create"})
	}
	parentID := md.GetParentId()
	if parentID == "" {
		return Export{}, errs.BadRequest(errs.Violation{Field: "metadata.parent_id", Message: "required"})
	}
	if err := validateExportSpec(cmd.Spec); err != nil {
		return Export{}, err
	}
	if _, err := s.d.Hierarchy.Parent(ctx, parentID); err != nil {
		return Export{}, fmt.Errorf("export parent %s: %w", parentID, err)
	}
	dest := cmd.Spec.GetNebiusObjectStorage()
	if err := s.d.Buckets.Validate(ctx, dest.GetBucketById().GetId(), parentID); err != nil {
		return Export{}, err
	}
	id, err := s.d.IDs.New(KindExport)
	if err != nil {
		return Export{}, fmt.Errorf("new export id: %w", err)
	}
	spec := proto.CloneOf(cmd.Spec)
	if spec.GetNebiusObjectStorage().GetObjectPrefix() == "" {
		spec.GetNebiusObjectStorage().ObjectPrefix = defaultObjectPrefix
	}
	now := timestamppb.New(s.d.Clock.Now())
	res := &auditv2pb.AuditEventExport{
		Metadata: &commonpb.ResourceMetadata{
			Id:              id,
			ParentId:        parentID,
			Name:            md.GetName(),
			Labels:          maps.Clone(md.GetLabels()),
			ResourceVersion: 1,
			CreatedAt:       now,
			UpdatedAt:       now,
		},
		Spec:   spec,
		Status: &auditv2pb.AuditEventExportStatus{State: stateRunning, StartedAt: now},
	}
	return Export{Region: cmd.Region, Resource: res}, nil
}

func validateExportSpec(spec *auditv2pb.AuditEventExportSpec) error {
	params := spec.GetParams()
	switch {
	case params == nil:
		return errs.BadRequest(errs.Violation{Field: "spec.params", Message: "required"})
	case params.GetFrom() == nil:
		return errs.BadRequest(errs.Violation{Field: "spec.params.from", Message: "required"})
	case params.GetTo() == nil:
		return errs.BadRequest(errs.Violation{Field: "spec.params.to", Message: "required"})
	case !params.GetFrom().AsTime().Before(params.GetTo().AsTime()):
		return errs.BadRequest(errs.Violation{Field: "spec.params.to", Message: "to must be after from"})
	case params.GetEventType() == auditv2pb.EventType_EVENT_TYPE_UNSPECIFIED:
		return errs.BadRequest(errs.Violation{Field: "spec.params.event_type", Message: "required"})
	case params.GetEventType() != auditv2pb.EventType_CONTROL_PLANE && params.GetEventType() != auditv2pb.EventType_DATA_PLANE:
		return errs.BadRequest(errs.Violation{
			Field: "spec.params.event_type", Message: fmt.Sprintf("unknown event type %d", params.GetEventType()),
		})
	}
	if _, err := audit.ParseFilter(params.GetFilter()); err != nil {
		return err
	}
	dest := spec.GetNebiusObjectStorage()
	if dest == nil {
		return errs.BadRequest(errs.Violation{Field: "spec.nebius_object_storage", Message: "required"})
	}
	if dest.GetBucketById().GetId() == "" {
		return errs.BadRequest(errs.Violation{Field: "spec.nebius_object_storage.bucket_by_id.id", Message: "required"})
	}
	return nil
}

// modify applies fn to the stored export and bumps its resource version and
// update time in the same transaction.
func (s *ExportService) modify(ctx context.Context, id string, fn func(*auditv2pb.AuditEventExport) error) (*auditv2pb.AuditEventExport, error) {
	now := timestamppb.New(s.d.Clock.Now())
	return s.d.Exports.Modify(ctx, id, func(e *auditv2pb.AuditEventExport) error {
		if err := fn(e); err != nil {
			return err
		}
		e.Metadata.ResourceVersion++
		e.Metadata.UpdatedAt = now
		return nil
	})
}

// run writes the export's events to object storage and settles its state.
// An export that is no longer RUNNING (canceled) is left alone. It returns
// an error only when the final state could not be stored.
func (s *ExportService) run(ctx context.Context, id string) error {
	exp, err := s.d.Exports.Get(ctx, id)
	if err != nil {
		return err
	}
	if exp.Resource.GetStatus().GetState() != stateRunning {
		return nil
	}
	werr := s.write(ctx, exp)
	if werr != nil {
		s.d.Log.WarnContext(ctx, "audit export failed", "export_id", id, "error", werr)
	}
	// Settle even when ctx was canceled by Stop: a half-run export must not stay RUNNING.
	return s.settle(context.WithoutCancel(ctx), id, werr)
}

// settle moves a RUNNING export to DONE, or to FAILED when runErr is set.
// A concurrent Cancel wins: it leaves the export CANCELED.
func (s *ExportService) settle(ctx context.Context, id string, runErr error) error {
	now := timestamppb.New(s.d.Clock.Now())
	_, err := s.modify(ctx, id, func(e *auditv2pb.AuditEventExport) error {
		if e.GetStatus().GetState() != stateRunning {
			return errSettled
		}
		e.Status.State = stateDone
		if runErr != nil {
			e.Status.State = stateFailed
			e.Status.Message = runErr.Error()
		}
		e.Status.FinishedAt = now
		return nil
	})
	if err != nil && !errors.Is(err, errSettled) {
		return fmt.Errorf("settle export %s: %w", id, err)
	}
	return nil
}

// write exports the matching events as JSON Lines objects.
func (s *ExportService) write(ctx context.Context, exp Export) error {
	res := exp.Resource
	params := res.GetSpec().GetParams()
	dest := res.GetSpec().GetNebiusObjectStorage()
	filter, err := audit.ParseFilter(params.GetFilter())
	if err != nil {
		return err
	}
	bucket, err := s.d.Buckets.BackendName(ctx, dest.GetBucketById().GetId())
	if err != nil {
		return fmt.Errorf("resolve bucket: %w", err)
	}
	// The storage module owns the backend bucket: a missing one fails the export.
	if _, err := s.d.Writer.BucketEmpty(ctx, bucket); err != nil {
		return fmt.Errorf("destination bucket %s: %w", bucket, err)
	}
	if !s.regions.holdsEvents(exp.Region) {
		return nil // every recorded event belongs to the default region
	}
	prefix := strings.Trim(dest.GetObjectPrefix(), "/")
	if prefix == "" {
		prefix = defaultObjectPrefix
	}
	w := &objectWriter{
		store: s.d.Writer, bucket: bucket,
		keyPrefix: prefix + "/" + res.GetStatus().GetExportOperationId() + "/",
	}
	sc := scope{
		parentID:  res.GetMetadata().GetParentId(),
		eventType: params.GetEventType(),
		from:      params.GetFrom().AsTime(), to: params.GetTo().AsTime(), filter: filter,
	}
	for e := range s.d.Events.Since(0) {
		if !sc.match(e) {
			continue
		}
		ev, err := e.Event()
		if err != nil {
			return err
		}
		if err := w.add(ctx, ev); err != nil {
			return err
		}
	}
	return w.flush(ctx)
}

// objectWriter batches events into numbered JSON Lines objects.
type objectWriter struct {
	store     ObjectWriter
	bucket    string
	keyPrefix string

	buf   bytes.Buffer
	count int // events in buf
	index int // objects written
}

var jsonLines = protojson.MarshalOptions{UseProtoNames: true}

func (w *objectWriter) add(ctx context.Context, ev *auditv2pb.AuditEvent) error {
	line, err := jsonLines.Marshal(ev)
	if err != nil {
		return fmt.Errorf("encode event %s: %w", ev.GetId(), err)
	}
	w.buf.Write(line)
	w.buf.WriteByte('\n')
	w.count++
	if w.count == maxEventsPerObject {
		return w.flush(ctx)
	}
	return nil
}

// flush writes the buffered events as the next object; it is a no-op when
// nothing is buffered.
func (w *objectWriter) flush(ctx context.Context) error {
	if w.count == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.index++
	key := fmt.Sprintf("%saudit-events-%06d.jsonl", w.keyPrefix, w.index)
	_, err := w.store.Put(ctx, w.bucket, key, bytes.NewReader(w.buf.Bytes()), int64(w.buf.Len()),
		objstore.PutOptions{ContentType: exportContentType})
	if err != nil {
		return fmt.Errorf("put %s/%s: %w", w.bucket, key, err)
	}
	w.buf.Reset()
	w.count = 0
	return nil
}

func stateName(st auditv2pb.AuditEventExportState) string {
	return strings.TrimPrefix(st.String(), "AUDIT_EVENT_EXPORT_STATE_")
}

// StartRunner starts the runner that finishes exports after the configured
// delay. It must be paired with StopRunner. Exports scheduled while the
// runner is not running are run immediately instead.
func (s *ExportService) StartRunner() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		return
	}
	s.stop = make(chan struct{})
}

// StopRunner releases exports that are still waiting for their delay and
// waits for runs in progress to settle. Released exports stay RUNNING, as
// after a real control-plane restart.
func (s *ExportService) StopRunner() {
	s.mu.Lock()
	if s.stop == nil {
		s.mu.Unlock()
		return
	}
	close(s.stop)
	s.stop = nil
	s.mu.Unlock()
	s.wg.Wait()
}

// schedule runs export id after the delay on the runner, or right away when
// the runner is not running (nothing would ever run it). The run keeps the
// values of ctx (such as the principal) but not its cancellation: the
// request that started the export ends long before the run does.
func (s *ExportService) schedule(ctx context.Context, id string) error {
	s.mu.Lock()
	stop := s.stop
	if stop == nil {
		s.mu.Unlock()
		return s.run(ctx, id)
	}
	due := s.d.Clock.After(s.d.Delay) // registered before returning so a manual clock cannot miss it
	runCtx := context.WithoutCancel(ctx)
	s.wg.Go(func() {
		select {
		case <-due:
			if err := s.run(runCtx, id); err != nil {
				s.d.Log.ErrorContext(runCtx, "audit export run", "export_id", id, "error", err)
			}
		case <-stop:
		}
	})
	s.mu.Unlock()
	return nil
}
