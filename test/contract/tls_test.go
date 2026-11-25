package contract

import (
	"crypto/tls"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/nebius/gosdk"
	"github.com/nebius/gosdk/conn"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	registrypb "github.com/nebius/gosdk/proto/nebius/registry/v1"
	registrysvc "github.com/nebius/gosdk/services/nebius/registry/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/registry"
	"github.com/kfirgollan/NebiusMocker/test/internal/testca"
)

// TestEdgeTLS: with NEBIUS_MOCKER_TLS_CERT_FILE/_KEY_FILE the edge serves gRPC
// over TLS, so clients that cannot be told "insecure" (the nebius CLI's docker
// credential helper) can connect by trusting the CA; plaintext is refused.
func TestEdgeTLS(t *testing.T) {
	t.Parallel()
	ca := testca.New(t, []string{"localhost"}, []net.IP{net.IPv4(127, 0, 0, 1)})
	inst := testkit.StartT(t, []mocker.Module{iam.New(), registry.New()}, testkit.WithConfig(func(c *config.Config) {
		c.TLS = config.TLSConfig{CertFile: ca.CertFile, KeyFile: ca.KeyFile}
	}))
	dial := func(creds credentials.TransportCredentials) registrysvc.RegistryService {
		sdk, err := gosdk.New(t.Context(),
			gosdk.WithResolvers(conn.NewBasicResolver("*", inst.Addr)),
			gosdk.WithDialOptions(grpc.WithTransportCredentials(creds)),
			gosdk.WithCredentials(gosdk.IAMToken(inst.Token)),
		)
		if err != nil {
			t.Fatalf("gosdk.New: %v", err)
		}
		t.Cleanup(func() { _ = sdk.Close() })
		return registrysvc.NewRegistryService(sdk)
	}

	svc := dial(credentials.NewTLS(&tls.Config{RootCAs: ca.Pool, MinVersion: tls.VersionTLS12}))
	op, err := svc.Create(t.Context(), &registrypb.CreateRegistryRequest{
		Metadata: &commonpb.ResourceMetadata{ParentId: inst.ProjectID, Name: "over-tls"}, Spec: &registrypb.RegistrySpec{},
	})
	if err != nil {
		t.Fatalf("Create over TLS: %v", err)
	}
	if _, err := op.Wait(t.Context()); err != nil {
		t.Fatalf("Create wait: %v", err)
	}
	if _, err := svc.Get(t.Context(), &registrypb.GetRegistryRequest{Id: op.ResourceID()}); err != nil {
		t.Fatalf("Get over TLS: %v", err)
	}
	if _, err := dial(insecure.NewCredentials()).Get(t.Context(), &registrypb.GetRegistryRequest{Id: op.ResourceID()}); err == nil {
		t.Error("plaintext Get against the TLS edge succeeded, want a transport error")
	}
}
