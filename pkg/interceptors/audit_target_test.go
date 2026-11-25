package interceptors

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"google.golang.org/grpc"

	auditv2pb "github.com/nebius/gosdk/proto/nebius/audit/v2"
	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/audit"
	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/clock"
)

// A custom verb whose request names its resource in its own field (KMS
// Encrypt's key_id) gets the resource from the method's authz target.
func TestAuditResourceFromRuleTarget(t *testing.T) {
	t.Parallel()
	l, err := audit.NewLog(10)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := audit.NewRecorder(l, clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	const method = "/nebius.kms.v1.SymmetricCryptoService/Encrypt"
	rules := authz.NewRules()
	rules.Add(method, authz.Rule{
		Permission: authz.Perm("kms", "symmetricKeys", "encrypt"),
		Target: func(_ context.Context, req any) (string, error) {
			r, _ := req.(*kmspb.SymmetricEncryptRequest)
			return r.GetKeyId(), nil
		},
	})
	h := &fakeHierarchy{parents: map[string]string{"kmssymmetrickey-1": "project-x", "project-x": "tenant-t"}}
	icpt := Audit(AuditConfig{Recorder: rec, Hierarchy: h, Rules: rules})
	ctx, _ := reqContext(t, "tenantuseraccount-alice")
	req := &kmspb.SymmetricEncryptRequest{KeyId: "kmssymmetrickey-1", Plaintext: []byte("p")}
	if _, err := icpt(ctx, req, &grpc.UnaryServerInfo{FullMethod: method}, returning(&kmspb.SymmetricEncryptResponse{}, nil)); err != nil {
		t.Fatal(err)
	}
	var evs []*auditv2pb.AuditEvent
	for en := range l.Since(0) {
		ev, err := en.Event()
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
	}
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	res := evs[0].GetResource()
	if res.GetMetadata().GetId() != "kmssymmetrickey-1" || res.GetMetadata().GetType() != "kmssymmetrickey" {
		t.Errorf("resource = %v, want kmssymmetrickey-1", res.GetMetadata())
	}
	if hier := res.GetHierarchy(); len(hier) != 2 || hier[0].GetId() != "tenant-t" || hier[1].GetId() != "project-x" {
		t.Errorf("hierarchy = %v, want [tenant-t, project-x]", hier)
	}
	if got := evs[0].GetType(); got != "ai.nebius.kms.kmssymmetrickey.encrypt" {
		t.Errorf("type = %q", got)
	}
}
