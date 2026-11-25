package contract

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nebius/gosdk/operations"
	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"
	aisvc "github.com/nebius/gosdk/services/nebius/ai/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

const (
	aiOutsiderToken = "ai-outsider-token"
	aiOutsiderID    = "serviceaccount-e00mock00000000aiout"
)

func TestAIEndpointAuthz(t *testing.T) {
	t.Parallel()
	env := startAI(t, nil,
		testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithTokens(map[string]string{aiOutsiderToken: aiOutsiderID}),
	)
	parent := env.inst.ProjectID
	admin := env.svc
	editor := aisvc.NewEndpointService(env.inst.SDKAs(t, config.DefaultEditorToken))
	viewer := aisvc.NewEndpointService(env.inst.SDKAs(t, config.DefaultViewerToken))
	outsider := aisvc.NewEndpointService(env.inst.SDKAs(t, aiOutsiderToken))

	id := aiCreate(t, admin, parent, "target", nil)

	t.Run("admin does everything", func(t *testing.T) {
		t.Parallel()
		own := aiCreate(t, admin, parent, "admin-own", nil)
		aiLifecycle(t, admin, own)
	})
	t.Run("editor manages endpoints", func(t *testing.T) {
		t.Parallel()
		own := aiCreate(t, editor, parent, "editor-own", nil)
		if _, err := editor.Get(t.Context(), &aipb.GetEndpointRequest{Id: own}); err != nil {
			t.Errorf("editor Get: %v", err)
		}
		if _, err := editor.List(t.Context(), &aipb.ListEndpointsRequest{ParentId: parent}); err != nil {
			t.Errorf("editor List: %v", err)
		}
		aiLifecycle(t, editor, own)
	})
	t.Run("viewer reads only", func(t *testing.T) {
		t.Parallel()
		if _, err := viewer.Get(t.Context(), &aipb.GetEndpointRequest{Id: id}); err != nil {
			t.Errorf("viewer Get: %v", err)
		}
		if _, err := viewer.List(t.Context(), &aipb.ListEndpointsRequest{ParentId: parent}); err != nil {
			t.Errorf("viewer List: %v", err)
		}
		_, err := viewer.Create(t.Context(), aiCreateReq(parent, "by-viewer", validSpec()))
		assertCode(t, err, codes.PermissionDenied)
		_, err = viewer.Delete(t.Context(), &aipb.DeleteEndpointRequest{Id: id})
		assertCode(t, err, codes.PermissionDenied)
		_, err = viewer.Start(t.Context(), &aipb.StartEndpointRequest{Id: id})
		assertCode(t, err, codes.PermissionDenied)
		_, err = viewer.Stop(t.Context(), &aipb.StopEndpointRequest{Id: id})
		assertCode(t, err, codes.PermissionDenied)
		_, err = viewer.Restart(t.Context(), &aipb.RestartEndpointRequest{Id: id})
		assertCode(t, err, codes.PermissionDenied)
	})
	t.Run("view SECRET needs getSensitiveData", func(t *testing.T) {
		t.Parallel()
		secret := &aipb.GetEndpointRequest{Id: id, View: aipb.GetEndpointRequest_SECRET}
		if _, err := admin.Get(t.Context(), secret); err != nil {
			t.Errorf("admin Get SECRET: %v", err)
		}
		for name, c := range map[string]aisvc.EndpointService{"editor": editor, "viewer": viewer} {
			_, err := c.Get(t.Context(), secret)
			if code := status.Code(err); code != codes.PermissionDenied {
				t.Errorf("%s Get SECRET code = %v, want PermissionDenied", name, code)
			}
			if _, err := c.Get(t.Context(), &aipb.GetEndpointRequest{Id: id}); err != nil {
				t.Errorf("%s plain Get: %v", name, err)
			}
		}
	})
	t.Run("outsider is denied", func(t *testing.T) {
		t.Parallel()
		// A denied read is NOT_FOUND, so existence is not leaked.
		_, err := outsider.Get(t.Context(), &aipb.GetEndpointRequest{Id: id})
		assertCode(t, err, codes.NotFound)
		_, err = outsider.List(t.Context(), &aipb.ListEndpointsRequest{ParentId: parent})
		assertCode(t, err, codes.PermissionDenied)
	})
}

// aiLifecycle runs stop, start, restart and delete on id as svc's caller.
func aiLifecycle(t *testing.T, svc aisvc.EndpointService, id string) {
	t.Helper()
	ctx := t.Context()
	steps := []struct {
		name string
		do   func() (operations.Operation, error)
	}{
		{"Stop", func() (operations.Operation, error) { return svc.Stop(ctx, &aipb.StopEndpointRequest{Id: id}) }},
		{"Start", func() (operations.Operation, error) { return svc.Start(ctx, &aipb.StartEndpointRequest{Id: id}) }},
		{"Restart", func() (operations.Operation, error) { return svc.Restart(ctx, &aipb.RestartEndpointRequest{Id: id}) }},
		{"Delete", func() (operations.Operation, error) { return svc.Delete(ctx, &aipb.DeleteEndpointRequest{Id: id}) }},
	}
	for _, s := range steps {
		op, err := s.do()
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if err := waitOp(ctx, op); err != nil {
			t.Fatalf("%s wait: %v", s.name, err)
		}
	}
}

func waitOp(ctx context.Context, op operations.Operation) error {
	_, err := op.Wait(ctx)
	return err
}
