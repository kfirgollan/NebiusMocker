package interceptors

import (
	"context"
	"errors"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/kfirgollan/NebiusMocker/pkg/audit"
)

// ResourceStates reads resources for the audit event's resource.state: it
// calls the Get RPC of the service that changed the resource, in process and
// without the interceptor chain, so the state is exactly the view a client's
// Get returns (metadata, spec, status), redacted like every audit payload.
//
// It learns the Get handlers from the registrations that pass through
// Registrar, so no module has to cooperate. It is safe for concurrent use.
type ResourceStates struct {
	mu   sync.RWMutex
	gets map[string]getHandler // gRPC service name → its Get
}

type getHandler struct {
	impl    any
	handler grpc.MethodHandler
}

var _ audit.StateReader = (*ResourceStates)(nil)

// NewResourceStates returns an empty reader.
func NewResourceStates() *ResourceStates {
	return &ResourceStates{gets: map[string]getHandler{}}
}

// Registrar wraps next: services registered through it are served by next
// as usual, and their Get methods become readable by ResourceState.
func (s *ResourceStates) Registrar(next grpc.ServiceRegistrar) grpc.ServiceRegistrar {
	return statesRegistrar{next: next, states: s}
}

type statesRegistrar struct {
	next   grpc.ServiceRegistrar
	states *ResourceStates
}

func (r statesRegistrar) RegisterService(desc *grpc.ServiceDesc, impl any) {
	r.next.RegisterService(desc, impl)
	for _, m := range desc.Methods {
		if m.MethodName == "Get" {
			r.states.mu.Lock()
			r.states.gets[desc.ServiceName] = getHandler{impl: impl, handler: m.Handler}
			r.states.mu.Unlock()
		}
	}
}

// errNoIDField is returned by the request decoder when a service's Get
// request has no string "id" field (it is not a Get by ID).
var errNoIDField = errors.New("get request has no id field")

// ResourceState implements audit.StateReader: it returns the redacted Get
// view of resource id from the service of method, or false when that service
// has no Get by ID or the Get fails (e.g. the resource was deleted).
func (s *ResourceStates) ResourceState(ctx context.Context, method, id string) (*structpb.Struct, bool) {
	svc, _, _ := strings.Cut(strings.TrimPrefix(method, "/"), "/")
	s.mu.RLock()
	g, ok := s.gets[svc]
	s.mu.RUnlock()
	if !ok || id == "" {
		return nil, false
	}
	dec := func(v any) error {
		m, ok := v.(proto.Message)
		if !ok {
			return errNoIDField
		}
		r := m.ProtoReflect()
		fd := r.Descriptor().Fields().ByName("id")
		if fd == nil || fd.Kind() != protoreflect.StringKind || fd.IsList() {
			return errNoIDField
		}
		r.Set(fd, protoreflect.ValueOfString(id))
		return nil
	}
	resp, err := g.handler(g.impl, ctx, dec, nil)
	if err != nil {
		return nil, false
	}
	m, ok := resp.(proto.Message)
	if !ok {
		return nil, false
	}
	st := audit.ToStruct(audit.Redact(m))
	return st, st != nil
}
