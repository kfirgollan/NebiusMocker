//nolint:staticcheck // SA1019: nebius.mk8s.v1alpha1 is deprecated upstream; serving it is this file's purpose
package api_test

import (
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoreflect"

	mk8spb "github.com/nebius/gosdk/proto/nebius/mk8s/v1"
	mk8sv1alpha1pb "github.com/nebius/gosdk/proto/nebius/mk8s/v1alpha1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/services/mk8s/api"
)

func TestEveryAlphaRPCHasARule(t *testing.T) {
	t.Parallel()
	r := authz.NewRules()
	api.RegisterAlphaAuthzRules(r)
	for _, d := range []grpc.ServiceDesc{mk8sv1alpha1pb.ClusterService_ServiceDesc, mk8sv1alpha1pb.NodeGroupService_ServiceDesc} {
		for _, m := range d.Methods {
			full := "/" + d.ServiceName + "/" + m.MethodName
			if _, ok := r.Lookup(full); !ok {
				t.Errorf("no authz rule for %s", full)
			}
		}
	}
}

// TestAlphaMessagesMapOntoV1 pins what makes the binary round-trip safe: every
// field of the v1alpha1 resources exists in v1 with the same number, name and
// type (AttachedFilesystemSpec 2 is device_name in alpha, mount_tag in v1: the
// same string), and every enum value agrees.
func TestAlphaMessagesMapOntoV1(t *testing.T) {
	t.Parallel()
	seen := map[protoreflect.FullName]bool{}
	for _, p := range []struct {
		alpha, v1 protoreflect.MessageDescriptor
	}{
		{(&mk8sv1alpha1pb.ClusterSpec{}).ProtoReflect().Descriptor(), (&mk8spb.ClusterSpec{}).ProtoReflect().Descriptor()},
		{(&mk8sv1alpha1pb.NodeGroupSpec{}).ProtoReflect().Descriptor(), (&mk8spb.NodeGroupSpec{}).ProtoReflect().Descriptor()},
		{(&mk8sv1alpha1pb.Cluster{}).ProtoReflect().Descriptor(), (&mk8spb.Cluster{}).ProtoReflect().Descriptor()},
		{(&mk8sv1alpha1pb.NodeGroup{}).ProtoReflect().Descriptor(), (&mk8spb.NodeGroup{}).ProtoReflect().Descriptor()},
	} {
		checkMapsOnto(t, p.alpha, p.v1, seen)
	}
}

func checkMapsOnto(t *testing.T, alpha, v1 protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool) {
	t.Helper()
	if seen[alpha.FullName()] {
		return
	}
	seen[alpha.FullName()] = true
	for i := range alpha.Fields().Len() {
		f := alpha.Fields().Get(i)
		g := v1.Fields().ByNumber(f.Number())
		if g == nil {
			t.Errorf("%s.%s (%d) has no v1 field", alpha.Name(), f.Name(), f.Number())
			continue
		}
		sameName := f.Name() == g.Name() || (alpha.Name() == "AttachedFilesystemSpec" && f.Number() == 2)
		if !sameName || f.Kind() != g.Kind() || f.Cardinality() != g.Cardinality() || f.IsMap() != g.IsMap() ||
			(f.ContainingOneof() == nil) != (g.ContainingOneof() == nil) || f.HasOptionalKeyword() != g.HasOptionalKeyword() {
			t.Errorf("%s.%s (%d) differs from v1 %s", alpha.Name(), f.Name(), f.Number(), g.Name())
		}
		switch {
		case f.Kind() == protoreflect.EnumKind && g.Kind() == protoreflect.EnumKind:
			for j := range f.Enum().Values().Len() {
				ev := f.Enum().Values().Get(j)
				if w := g.Enum().Values().ByNumber(ev.Number()); w == nil || w.Name() != ev.Name() {
					t.Errorf("enum %s value %s=%d differs from v1", f.Enum().Name(), ev.Name(), ev.Number())
				}
			}
		case f.Kind() == protoreflect.MessageKind && g.Kind() == protoreflect.MessageKind && !f.IsMap() &&
			f.Message().ParentFile().Package() != "google.protobuf":
			checkMapsOnto(t, f.Message(), g.Message(), seen)
		}
	}
}
