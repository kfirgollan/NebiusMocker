package interceptors

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
)

const (
	endpointSvc = "nebius.ai.v1.EndpointService"
	authSecret  = "s3cr3t-token"
)

// fakeEndpoints serves Get from a map; other methods stay unimplemented.
type fakeEndpoints struct {
	aipb.UnimplementedEndpointServiceServer
	byID map[string]*aipb.Endpoint
}

func (f *fakeEndpoints) Get(_ context.Context, req *aipb.GetEndpointRequest) (*aipb.Endpoint, error) {
	ep, ok := f.byID[req.GetId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "no such endpoint")
	}
	return ep, nil
}

// recordingRegistrar remembers the services registered with it.
type recordingRegistrar struct {
	names []string
}

func (r *recordingRegistrar) RegisterService(desc *grpc.ServiceDesc, _ any) {
	r.names = append(r.names, desc.ServiceName)
}

func newStates(t *testing.T) (*ResourceStates, *recordingRegistrar) {
	t.Helper()
	next := &recordingRegistrar{}
	states := NewResourceStates()
	aipb.RegisterEndpointServiceServer(states.Registrar(next), &fakeEndpoints{byID: map[string]*aipb.Endpoint{
		"aiendpoint-1": {
			Metadata: &commonpb.ResourceMetadata{Id: "aiendpoint-1", Name: "ep"},
			Spec:     &aipb.EndpointSpec{AuthToken: authSecret},
		},
	}})
	return states, next
}

func TestResourceStatesRegistrarForwards(t *testing.T) {
	t.Parallel()
	_, next := newStates(t)
	if len(next.names) != 1 || next.names[0] != endpointSvc {
		t.Errorf("wrapped registrar got %v, want [%s]", next.names, endpointSvc)
	}
}

func TestResourceStateReturnsRedactedGet(t *testing.T) {
	t.Parallel()
	states, _ := newStates(t)
	st, ok := states.ResourceState(t.Context(), "/"+endpointSvc+"/Update", "aiendpoint-1")
	if !ok {
		t.Fatal("ResourceState ok = false, want true")
	}
	md := st.GetFields()["metadata"].GetStructValue().GetFields()
	if got := md["id"].GetStringValue(); got != "aiendpoint-1" {
		t.Errorf("metadata.id = %q, want aiendpoint-1", got)
	}
	b, err := protojson.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), authSecret) {
		t.Errorf("state contains the sensitive auth token: %s", b)
	}
}

func TestResourceStateNotReadable(t *testing.T) {
	t.Parallel()
	states, _ := newStates(t)
	tests := []struct {
		name   string
		method string
		id     string
	}{
		{name: "unknown service", method: "/nebius.iam.v1.GroupService/Update", id: "aiendpoint-1"},
		{name: "get fails", method: "/" + endpointSvc + "/Delete", id: "aiendpoint-gone"},
		{name: "empty id", method: "/" + endpointSvc + "/Update", id: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st, ok := states.ResourceState(t.Context(), tt.method, tt.id)
			if ok || st != nil {
				t.Errorf("ResourceState(%q, %q) = (%v, %v), want (nil, false)", tt.method, tt.id, st, ok)
			}
		})
	}
}
