package contract

import (
	"slices"
	"testing"

	"google.golang.org/grpc/codes"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
)

const (
	platformOutsiderToken = "platform-outsider-token"
	platformOutsiderID    = "serviceaccount-e00mock0000000platout"
)

func TestPlatformService(t *testing.T) {
	t.Parallel()
	env := startImages(t)
	svc := computesvc.NewPlatformService(env.Inst.SDK(t))
	ctx := t.Context()
	project := env.Inst.ProjectID

	list, err := svc.List(ctx, &computepb.ListPlatformsRequest{ParentId: project})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*computepb.Platform{}
	for _, p := range list.GetItems() {
		byName[p.GetMetadata().GetName()] = p
	}
	h100, cpu := byName["gpu-h100-sxm"], byName["cpu-e2"]
	if h100 == nil || cpu == nil || byName["gpu-b200-sxm"] != nil {
		t.Fatalf("eu-north1 platforms = %v", slices.Collect(func(yield func(string) bool) {
			for n := range byName {
				if !yield(n) {
					return
				}
			}
		}))
	}
	presets := func(p *computepb.Platform) (out []string) {
		for _, pr := range p.GetSpec().GetPresets() {
			out = append(out, pr.GetName())
		}
		return out
	}
	if got := presets(h100); !slices.Equal(got, []string{"1gpu-16vcpu-200gb", "8gpu-128vcpu-1600gb"}) {
		t.Errorf("h100 presets = %v", got)
	}
	if got := presets(cpu); !slices.Equal(got, []string{"2vcpu-8gb", "4vcpu-16gb", "8vcpu-32gb", "16vcpu-64gb", "32vcpu-128gb", "48vcpu-192gb", "64vcpu-256gb", "80vcpu-320gb"}) {
		t.Errorf("cpu-e2 presets = %v", got)
	}
	if h100.GetMetadata().GetParentId() != project || h100.GetStatus().GetAllowedForPreemptibles() != true || cpu.GetStatus().GetAllowedForPreemptibles() {
		t.Errorf("metadata/status: %v %v", h100, cpu)
	}

	t.Run("paging", func(t *testing.T) {
		t.Parallel()
		var got []string
		token := ""
		for range 20 {
			page, err := svc.List(ctx, &computepb.ListPlatformsRequest{ParentId: project, PageSize: 2, PageToken: token})
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range page.GetItems() {
				got = append(got, p.GetMetadata().GetName())
			}
			if token = page.GetNextPageToken(); token == "" {
				break
			}
		}
		if len(got) != len(list.GetItems()) {
			t.Errorf("paged %d platforms, want %d", len(got), len(list.GetItems()))
		}
	})
	t.Run("GetByName", func(t *testing.T) {
		t.Parallel()
		p, err := svc.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: project, Name: "gpu-h100-sxm"})
		if err != nil || p.GetMetadata().GetId() != h100.GetMetadata().GetId() {
			t.Fatalf("GetByName = %v, %v", p, err)
		}
		_, err = svc.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: project, Name: "gpu-b200-sxm"})
		assertCode(t, err, codes.NotFound)
		_, err = svc.GetByName(ctx, &commonpb.GetByNameRequest{Name: "gpu-h100-sxm"})
		assertCode(t, err, codes.InvalidArgument)
		_, err = svc.List(ctx, &computepb.ListPlatformsRequest{})
		assertCode(t, err, codes.InvalidArgument)
	})
}

func TestPlatformAuthz(t *testing.T) {
	t.Parallel()
	env := startImages(t,
		testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithTokens(map[string]string{platformOutsiderToken: platformOutsiderID}),
	)
	project := env.Inst.ProjectID
	for _, token := range []string{config.DefaultDevToken, config.DefaultEditorToken, config.DefaultViewerToken} {
		svc := computesvc.NewPlatformService(env.Inst.SDKAs(t, token))
		if _, err := svc.List(t.Context(), &computepb.ListPlatformsRequest{ParentId: project}); err != nil {
			t.Errorf("%s List: %v", token, err)
		}
		if _, err := svc.GetByName(t.Context(), &commonpb.GetByNameRequest{ParentId: project, Name: "cpu-e2"}); err != nil {
			t.Errorf("%s GetByName: %v", token, err)
		}
	}
	out := computesvc.NewPlatformService(env.Inst.SDKAs(t, platformOutsiderToken))
	_, err := out.List(t.Context(), &computepb.ListPlatformsRequest{ParentId: project})
	assertCode(t, err, codes.PermissionDenied)
	_, err = out.GetByName(t.Context(), &commonpb.GetByNameRequest{ParentId: project, Name: "cpu-e2"})
	// A denied by-name read is NOT_FOUND, so existence is not leaked (as for other by-name reads).
	assertCode(t, err, codes.NotFound)
}
