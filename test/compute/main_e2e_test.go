//go:build e2e_images

// Package compute runs NebiusMocker's Compute images end to end against a
// real RustFS object store (Docker), the way users work with them:
//   - image files go into an Object Storage bucket through the mocker's S3
//     front, with a Nebius access key and the AWS CLI or the AWS SDK for Go v2;
//   - images are imported and managed with the official nebius CLI and the
//     official Go SDK.
//
// The object store is only read, to verify that payloads really landed.
//
// Run with: make e2e-images
package compute

import (
	"os"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
	"github.com/kfirgollan/NebiusMocker/test/internal/s3upload"
	"github.com/kfirgollan/NebiusMocker/test/rustfsfixture"
)

// One RustFS container for the whole test binary, removed when it exits.
func TestMain(m *testing.M) { os.Exit(rustfsfixture.Main(m)) }

// env is a mocker with IAM, Object Storage (S3 front on a random port) and
// Compute, its blobs in RustFS, enforcing authorization.
type env struct {
	*imagescenario.Env
	s3Addr string
}

func startOnRustFS(t *testing.T, opts ...testkit.Option) *env {
	t.Helper()
	srv := rustfsfixture.Start(t)
	st := storage.New(storage.WithS3Listen("127.0.0.1:0"))
	e := imagescenario.Start(t, srv.Client(t), []mocker.Module{st},
		append([]testkit.Option{testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce), testkit.WithOperationDelay(100 * time.Millisecond)}, opts...)...)
	e.BucketPrefix = s3upload.Unique("img") + "-"
	e.Upload = s3upload.New(t, e.Inst, st.S3Addr()).Upload
	return &env{Env: e, s3Addr: st.S3Addr()}
}
