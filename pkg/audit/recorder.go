package audit

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"google.golang.org/genproto/googleapis/rpc/code"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/clock"
)

// Versions stamped on every event.
const (
	SpecVersion  = "1.0" // CloudEvents
	EventVersion = "1.0"
)

const (
	maxPendingOps = 10_000  // running operations awaiting their DONE/ERROR event
	maxEarly      = 1_024   // finished operations not (yet) claimed by an event
	maxNames      = 100_000 // remembered resource names
)

// Sink stores events. *Log implements it.
type Sink interface {
	Append(ev *auditv2pb.AuditEvent, typ auditv2pb.EventType) (*Entry, error)
}

// StateReader reads the current state of a resource for resource.state: the
// view its own service's Get returns, already redacted. method is the full
// gRPC method of the call that changed the resource
// ("/nebius.iam.v1.GroupService/Update"); ok is false when the resource
// cannot be read (it is gone, or the service has no Get by ID).
type StateReader interface {
	ResourceState(ctx context.Context, method, id string) (state *structpb.Struct, ok bool)
}

// RecorderOption configures a Recorder.
type RecorderOption func(*Recorder)

// WithStateReader makes the Recorder fill resource.state.current of the
// DONE event of an asynchronous operation, read when it finishes.
func WithStateReader(sr StateReader) RecorderOption {
	return func(r *Recorder) { r.state = sr }
}

// Recorder stamps events (ID, time, versions), fills in resource names it
// has seen before, appends them to a Sink, and emits the follow-up event when
// an asynchronous operation finishes. It is safe for concurrent use and never
// blocks for long: every method is a few map operations and one append under
// one mutex.
type Recorder struct {
	sink  Sink
	clk   clock.Clock
	log   *slog.Logger
	state StateReader // nil: resource.state.current of async operations stays empty

	mu      sync.Mutex
	pending map[string]*auditv2pb.AuditEvent // operation ID → STARTED event
	early   map[string]opResult              // operation ID → result, finished before Record
	order   []string                         // insertion order of early, for eviction
	names   map[string]string                // resource ID → last known name
}

type opResult struct {
	code    code.Code
	message string
}

// NewRecorder returns a recorder appending to sink.
func NewRecorder(sink Sink, clk clock.Clock, log *slog.Logger, opts ...RecorderOption) (*Recorder, error) {
	if sink == nil || clk == nil || log == nil {
		return nil, errors.New("audit: recorder needs a sink, a clock and a logger")
	}
	r := &Recorder{
		sink: sink, clk: clk, log: log,
		pending: map[string]*auditv2pb.AuditEvent{},
		early:   map[string]opResult{},
		names:   map[string]string{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// RememberName records the display name of a resource, for events about it
// that do not carry the name (Delete requests carry only the ID).
func (r *Recorder) RememberName(id, name string) {
	if id == "" || name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rememberLocked(id, name)
}

// Record stores ev as a CONTROL_PLANE event. When runningOpID is set, ev describes an operation that
// is still running (status STARTED): a second event with status DONE or
// ERROR is recorded once OperationFinished reports it.
func (r *Recorder) Record(ctx context.Context, ev *auditv2pb.AuditEvent, runningOpID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fillNamesLocked(ev)
	r.appendLocked(ctx, ev, auditv2pb.EventType_CONTROL_PLANE)
	if runningOpID == "" {
		return
	}
	if res, ok := r.early[runningOpID]; ok {
		// The operation finished while its RPC was returning: the caller
		// built ev before that, so there is no current state to add.
		r.dropEarlyLocked(runningOpID)
		r.appendLocked(ctx, finished(ev, res, nil), auditv2pb.EventType_CONTROL_PLANE)
		return
	}
	if len(r.pending) >= maxPendingOps {
		r.log.WarnContext(ctx, "audit: too many running operations; completion event dropped", "op_id", runningOpID)
		return
	}
	r.pending[runningOpID] = proto.CloneOf(ev)
}

// RecordDataPlane stores ev as a DATA_PLANE event (Object Storage object
// access). It is stamped and named like a control-plane event.
func (r *Recorder) RecordDataPlane(ctx context.Context, ev *auditv2pb.AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fillNamesLocked(ev)
	r.appendLocked(ctx, ev, auditv2pb.EventType_DATA_PLANE)
}

// OperationFinished records the completion event of a running operation
// whose STARTED event was recorded. c is the operation's final status code.
// For a successful operation, resource.state.current is the resource as its
// service's Get returns it now (see WithStateReader).
func (r *Recorder) OperationFinished(ctx context.Context, opID string, c code.Code, message string) {
	res := opResult{code: c, message: message}
	r.mu.Lock()
	started, ok := r.pending[opID]
	if !ok {
		// Not recorded yet (the operation finished inside its RPC) or not
		// audited at all. Keep the result briefly in case Record follows.
		r.rememberEarlyLocked(opID, res)
		r.mu.Unlock()
		return
	}
	delete(r.pending, opID)
	r.mu.Unlock()

	// Read the state outside the lock: it calls into a service.
	var current *structpb.Struct
	if id := started.GetResource().GetMetadata().GetId(); r.state != nil && c == code.Code_OK && id != "" {
		current, _ = r.state.ResourceState(ctx, "/"+started.GetSource(), id)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appendLocked(ctx, finished(started, res, current), auditv2pb.EventType_CONTROL_PLANE)
}

// Forget drops what is known about an operation that finished inside its
// RPC and was recorded with its final status.
func (r *Recorder) Forget(opID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropEarlyLocked(opID)
}

func (r *Recorder) appendLocked(ctx context.Context, ev *auditv2pb.AuditEvent, typ auditv2pb.EventType) {
	ev.Id = newEventID()
	ev.Time = timestamppb.New(r.clk.Now())
	ev.SpecVersion = SpecVersion
	ev.EventVersion = EventVersion
	if _, err := r.sink.Append(ev, typ); err != nil {
		r.log.ErrorContext(ctx, "audit: append event", "type", ev.GetType(), "error", err)
	}
}

func (r *Recorder) fillNamesLocked(ev *auditv2pb.AuditEvent) {
	res := ev.GetResource()
	md := res.GetMetadata()
	switch {
	case md.GetName() != "":
		r.rememberLocked(md.GetId(), md.GetName())
	case md != nil:
		md.Name = r.names[md.GetId()]
	}
	for _, h := range res.GetHierarchy() {
		if h.GetName() == "" {
			h.Name = r.names[h.GetId()]
		}
	}
}

func (r *Recorder) rememberLocked(id, name string) {
	if id == "" {
		return
	}
	if _, known := r.names[id]; !known && len(r.names) >= maxNames {
		clear(r.names) // crude but bounded; names are a convenience
	}
	r.names[id] = name
}

func (r *Recorder) rememberEarlyLocked(opID string, res opResult) {
	if _, known := r.early[opID]; known {
		r.early[opID] = res // already in order
		return
	}
	if len(r.order) >= maxEarly {
		delete(r.early, r.order[0])
		r.order = r.order[1:]
	}
	r.early[opID] = res
	r.order = append(r.order, opID)
}

// dropEarlyLocked removes opID from early and from the eviction order, so
// claimed results do not count against maxEarly.
func (r *Recorder) dropEarlyLocked(opID string) {
	if _, ok := r.early[opID]; !ok {
		return
	}
	delete(r.early, opID)
	if i := slices.Index(r.order, opID); i >= 0 {
		r.order = slices.Delete(r.order, i, i+1)
	}
}

// finished derives the DONE/ERROR event of an operation from its STARTED
// event; current, when set, becomes resource.state.current.
func finished(started *auditv2pb.AuditEvent, res opResult, current *structpb.Struct) *auditv2pb.AuditEvent {
	ev := proto.CloneOf(started)
	ev.Status = auditv2pb.Status_DONE
	if res.code != code.Code_OK {
		ev.Status = auditv2pb.Status_ERROR
	}
	if current != nil && ev.GetResource() != nil {
		st := ev.GetResource().GetState()
		if st == nil {
			st = &auditv2pb.ResourceState{}
			ev.Resource.State = st
		}
		st.Current = current
	}
	if ev.GetResponse() == nil {
		ev.Response = &auditv2pb.Response{}
	}
	ev.Response.StatusCode = res.code
	ev.Response.ErrorMessage = res.message
	return ev
}

// newEventID returns a random RFC 4122 version 4 UUID.
func newEventID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
