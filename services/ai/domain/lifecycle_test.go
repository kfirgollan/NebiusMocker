package domain_test

import (
	"context"
	"testing"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/pkg/operations"
)

type verbFn func(ctx context.Context, h *harness, id string) (*operations.Operation, error)

func doStart(ctx context.Context, h *harness, id string) (*operations.Operation, error) {
	return h.svc.Start(ctx, id, nil)
}

func doStop(ctx context.Context, h *harness, id string) (*operations.Operation, error) {
	return h.svc.Stop(ctx, id, nil)
}

func doRestart(ctx context.Context, h *harness, id string) (*operations.Operation, error) {
	return h.svc.Restart(ctx, id, nil)
}

func doDelete(ctx context.Context, h *harness, id string) (*operations.Operation, error) {
	return h.svc.Delete(ctx, id, nil)
}

// newRunning returns a harness with one RUNNING endpoint.
func newRunning(t *testing.T) (h *harness, id string) {
	t.Helper()
	h = newService(t, opts{})
	e := mustCreate(t, h, "ep", baseSpec())
	return h, e.GetMetadata().GetId()
}

func instanceID(t *testing.T, e *aipb.Endpoint) string {
	t.Helper()
	ins := e.GetStatus().GetInstances()
	if len(ins) != 1 {
		t.Fatalf("instances = %d, want 1", len(ins))
	}
	return ins[0].GetComputeInstanceId()
}

func TestLifecycleTransitions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// setup moves the endpoint from RUNNING to its starting state.
		setup     verbFn
		act       verbFn
		wantKind  errs.Kind // the zero kind means success
		wantState aipb.EndpointStatus_State
		wantGone  bool
		wantNewVM bool // a new instance ID compared with the RUNNING one before setup
	}{
		{name: "stop from running", act: doStop, wantState: aipb.EndpointStatus_STOPPED},
		{name: "stop twice", setup: doStop, act: doStop, wantKind: errs.KindBadState},
		{name: "start from stopped", setup: doStop, act: doStart, wantState: aipb.EndpointStatus_RUNNING, wantNewVM: true},
		{name: "restart from running", act: doRestart, wantState: aipb.EndpointStatus_RUNNING, wantNewVM: true},
		{name: "start from running", act: doStart, wantKind: errs.KindBadState},
		{name: "restart from stopped", setup: doStop, act: doRestart, wantKind: errs.KindBadState},
		{name: "delete from running", act: doDelete, wantGone: true},
		{name: "delete from stopped", setup: doStop, act: doDelete, wantGone: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, id := newRunning(t)
			firstVM := instanceID(t, mustGet(t, h, id))
			if tt.setup != nil {
				if _, err := tt.setup(t.Context(), h, id); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}
			before := mustGet(t, h, id)
			op, err := tt.act(t.Context(), h, id)
			if got := errs.KindOf(err); got != tt.wantKind {
				t.Fatalf("kind = %v, want %v (err: %v)", got, tt.wantKind, err)
			}
			if err != nil {
				// A rejected verb leaves the endpoint untouched.
				if got := mustGet(t, h, id).GetStatus().GetState(); got != before.GetStatus().GetState() {
					t.Errorf("state after rejected verb = %v, want %v", got, before.GetStatus().GetState())
				}
				return
			}
			if op.Err != nil {
				t.Fatalf("operation failed: %v", op.Err)
			}
			if tt.wantGone {
				if _, err := h.svc.Get(t.Context(), id); !errs.IsKind(err, errs.KindNotFound) {
					t.Fatalf("Get after delete err = %v, want NotFound", err)
				}
				return
			}
			e := mustGet(t, h, id)
			if got := e.GetStatus().GetState(); got != tt.wantState {
				t.Fatalf("state = %v, want %v", got, tt.wantState)
			}
			if tt.wantState == aipb.EndpointStatus_STOPPED && len(e.GetStatus().GetInstances()) != 0 {
				t.Errorf("stopped endpoint has %d instances, want 0", len(e.GetStatus().GetInstances()))
			}
			if tt.wantNewVM {
				if got := instanceID(t, e); got == firstVM {
					t.Errorf("instance id = %s, want a new one", got)
				}
			}
		})
	}
}

func TestLifecycleDuringStates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		setup      verbFn // run before holding; nil for none
		act        verbFn
		wantDuring aipb.EndpointStatus_State
		wantCode   string
		wantAfter  aipb.EndpointStatus_State
		wantGone   bool
	}{
		{name: "stop", act: doStop, wantDuring: aipb.EndpointStatus_STOPPING, wantCode: "Stopping", wantAfter: aipb.EndpointStatus_STOPPED},
		{name: "start", setup: doStop, act: doStart, wantDuring: aipb.EndpointStatus_STARTING, wantCode: "Starting", wantAfter: aipb.EndpointStatus_RUNNING},
		{name: "restart", act: doRestart, wantDuring: aipb.EndpointStatus_STARTING, wantCode: "Starting", wantAfter: aipb.EndpointStatus_RUNNING},
		{name: "delete", act: doDelete, wantDuring: aipb.EndpointStatus_DELETING, wantCode: "Deleting", wantGone: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, id := newRunning(t)
			if tt.setup != nil {
				if _, err := tt.setup(t.Context(), h, id); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}
			h.ops.hold = true
			if _, err := tt.act(t.Context(), h, id); err != nil {
				t.Fatalf("act: %v", err)
			}
			e := mustGet(t, h, id)
			if got := e.GetStatus().GetState(); got != tt.wantDuring {
				t.Fatalf("state while running = %v, want %v", got, tt.wantDuring)
			}
			if got := e.GetStatus().GetStateDetails().GetCode(); got != tt.wantCode {
				t.Errorf("state_details.code = %q, want %q", got, tt.wantCode)
			}
			h.ops.release()
			if tt.wantGone {
				if _, err := h.svc.Get(t.Context(), id); !errs.IsKind(err, errs.KindNotFound) {
					t.Fatalf("Get after release err = %v, want NotFound", err)
				}
				return
			}
			if got := mustGet(t, h, id).GetStatus().GetState(); got != tt.wantAfter {
				t.Errorf("state after release = %v, want %v", got, tt.wantAfter)
			}
		})
	}
}

func TestCreateProvisioningState(t *testing.T) {
	t.Parallel()
	h := newService(t, opts{})
	h.ops.hold = true
	op, err := h.svc.Create(t.Context(), createCmd("ep", baseSpec()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	e := mustGet(t, h, op.ResourceID)
	if got := e.GetStatus().GetState(); got != aipb.EndpointStatus_PROVISIONING {
		t.Fatalf("state while creating = %v, want PROVISIONING", got)
	}
	if got := e.GetStatus().GetStateDetails().GetCode(); got != "Provisioning" {
		t.Errorf("state_details.code = %q, want Provisioning", got)
	}
	if n := len(e.GetStatus().GetInstances()); n != 0 {
		t.Errorf("provisioning endpoint has %d instances, want 0", n)
	}
	h.ops.release()
	if got := mustGet(t, h, op.ResourceID).GetStatus().GetState(); got != aipb.EndpointStatus_RUNNING {
		t.Errorf("state after create = %v, want RUNNING", got)
	}
}

func TestLifecycleUnknownEndpoint(t *testing.T) {
	t.Parallel()
	h := newService(t, opts{})
	for name, fn := range map[string]verbFn{"start": doStart, "stop": doStop, "restart": doRestart, "delete": doDelete} {
		if _, err := fn(t.Context(), h, "aiendpoint-404"); !errs.IsKind(err, errs.KindNotFound) {
			t.Errorf("%s unknown err = %v, want NotFound", name, err)
		}
	}
}
