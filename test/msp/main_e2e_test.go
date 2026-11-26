//go:build e2e_msp

// Package msp runs Managed PostgreSQL and Managed MLflow with the docker
// backends end to end, the way a user does: Terraform (the official provider)
// creates the clusters, and the user's tools (psql, the nebius CLI, the MLflow
// Python client, the AWS CLI) reach the real PostgreSQL, PgBouncer and MLflow
// containers through the published endpoints with the mocker's CA. The tools
// run in Linux containers (userMachine), like on a user's machine that does
// not share the mocker's loopback.
package msp

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
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/mspmlflow"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
	"github.com/kfirgollan/NebiusMocker/test/internal/testca"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfcache"
	"github.com/kfirgollan/NebiusMocker/test/rustfsfixture"
)

func TestMain(m *testing.M) {
	code := m.Run()
	tfcache.Cleanup()
	rustfsfixture.Cleanup()
	os.Exit(code)
}

// env is one mocker with the docker backends and RustFS as its object store.
type env struct {
	d    *docker
	inst *testkit.Instance
	ca   *testca.CA // the edge's CA
	addr string     // the edge, 127.0.0.1:<port>
	port int
	s3   string // the S3 front, 127.0.0.1:<port>
	sdk  *gosdk.SDK
	// mspCA is the file of the mocker's managed-service CA (clusters'
	// server certificates), the one users put in ~/.postgresql/root.crt.
	mspCA string
}

func startEnv(t *testing.T) *env {
	t.Helper()
	d := newDocker(t)
	rustfs := rustfsfixture.Start(t)
	names := append([]string{"*.api.nebius.cloud", "*.iam.api.nebius.cloud", "*.msp.api.nebius.cloud", "localhost"}, apiHosts...)
	ca := testca.New(t, names, nil)
	port, s3Port := freePort(t), freePort(t)
	mspDir := t.TempDir()
	inst := testkit.StartT(t,
		[]mocker.Module{iam.New(), vpc.New(), storage.New(), msppostgresql.New(), mspmlflow.New()},
		testkit.WithObjStore(rustfs.Client(t)),
		testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce), testkit.WithLogs(),
		testkit.WithConfig(func(c *config.Config) {
			c.Listen = fmt.Sprintf("127.0.0.1:%d", port)
			c.TLS = config.TLSConfig{CertFile: ca.CertFile, KeyFile: ca.KeyFile}
			c.Storage.S3Listen = fmt.Sprintf("127.0.0.1:%d", s3Port)
			c.MSP.PostgreSQLBackend = config.MSPBackendDocker
			c.MSP.MLflowBackend = config.MSPBackendDocker
			c.MSP.DockerHost = d.host
			// The user's tools run in containers: they reach the published
			// ports (and the S3 front) as host.docker.internal, which the
			// clusters' certificates cover.
			c.MSP.EndpointHost = "host.docker.internal"
			c.MSP.TLSDir = mspDir
		}))
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	return &env{
		d: d, inst: inst, ca: ca, addr: addr, port: port, s3: fmt.Sprintf("127.0.0.1:%d", s3Port),
		sdk: tlsSDK(t, addr, ca, inst.Token), mspCA: mspDir + "/ca.pem",
	}
}

// cliProfile creates a CLI profile in the user machine, authenticated with
// the admin IAM token, the way the documentation sets the CLI up.
func (e *env) cliProfile(t *testing.T, u *userMachine) {
	t.Helper()
	u.sh(t, fmt.Sprintf("printf %%s %q > /root/admin.token", e.inst.Token))
	u.sh(t, fmt.Sprintf("nebius profile create admin --endpoint api.nebius.cloud:%d --parent-id %s --token-file /root/admin.token </dev/null >/dev/null",
		e.port, e.inst.ProjectID))
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
