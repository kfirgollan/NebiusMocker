//go:build e2e_mk8s

// Package mk8s runs Managed Kubernetes with the k3s backend end to end, the
// way a user does: Terraform (the official provider) creates the network,
// the cluster and node groups; `nebius mk8s cluster get-credentials` writes
// the kubeconfig; kubectl talks to the real k3s API servers with Nebius IAM
// tokens. The user's tools run in a Linux container (userMachine) that
// trusts the mocker's CA, because the kubeconfig's exec plugin runs the CLI
// without --insecure.
package mk8s

import (
	"crypto/tls"
	"fmt"
	"os"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/nebius/gosdk"
	"github.com/nebius/gosdk/conn"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mk8s"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/testca"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfcache"
)

func TestMain(m *testing.M) { os.Exit(tfcache.Main(m)) }

// env is one mocker with the k3s backend plus the user machine.
type env struct {
	d    *docker
	inst *testkit.Instance
	ca   *testca.CA
	addr string // the edge, 127.0.0.1:<port>
	port int
	sdk  *gosdk.SDK
	user *userMachine
}

// maxAgents is the injected host capacity of the scenario: the agents of
// every cluster on the Docker host.
const maxAgents = 4

func startEnv(t *testing.T) *env {
	t.Helper()
	d := newDocker(t)
	names := append([]string{"*.api.nebius.cloud", "*.iam.api.nebius.cloud", "localhost"}, apiHosts...)
	ca := testca.New(t, names, nil)
	port := freePort(t)
	inst := testkit.StartT(t, []mocker.Module{iam.New(), vpc.New(), compute.New(), mk8s.New()},
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce), testkit.WithLogs(),
		testkit.WithConfig(func(c *config.Config) {
			c.Listen = fmt.Sprintf("127.0.0.1:%d", port)
			c.TLS = config.TLSConfig{CertFile: ca.CertFile, KeyFile: ca.KeyFile}
			c.MK8s.Backend = config.MK8sBackendK3s
			c.MK8sK3s.DockerHost = d.host
			// kubectl runs in a container: it reaches published ports as
			// host.docker.internal (the API server certificate covers it).
			c.MK8sK3s.EndpointHost = "host.docker.internal"
			c.MK8sK3s.MaxAgents = maxAgents
		}))
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	e := &env{d: d, inst: inst, ca: ca, addr: addr, port: port, sdk: tlsSDK(t, addr, ca, inst.Token)}
	e.user = startUserMachine(t, d, ca)
	return e
}

// tlsSDK is the official Go SDK over TLS, trusting the test CA.
func tlsSDK(t *testing.T, addr string, ca *testca.CA, token string) *gosdk.SDK {
	t.Helper()
	sdk, err := gosdk.New(t.Context(),
		gosdk.WithResolvers(conn.NewBasicResolver("*", addr)),
		gosdk.WithDialOptions(grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			RootCAs: ca.Pool, ServerName: "api.nebius.cloud", MinVersion: tls.VersionTLS12,
		}))),
		gosdk.WithCredentials(gosdk.IAMToken(token)),
	)
	if err != nil {
		t.Fatalf("gosdk.New: %v", err)
	}
	t.Cleanup(func() { _ = sdk.Close() })
	return sdk
}

// profile creates a CLI profile in the user machine; extra are more
// `nebius profile create` flags (credentials).
func (e *env) profile(t *testing.T, name, extra string) {
	t.Helper()
	e.user.sh(t, fmt.Sprintf("nebius profile create %s --endpoint api.nebius.cloud:%d --parent-id %s %s </dev/null >/dev/null",
		name, e.port, e.inst.ProjectID, extra))
}

// tokenProfile is a profile authenticated with a static IAM token.
func (e *env) tokenProfile(t *testing.T, name, token string) {
	t.Helper()
	e.user.sh(t, fmt.Sprintf("printf %%s %q > /root/%s.token", token, name))
	e.profile(t, name, "--token-file /root/"+name+".token")
}
