//go:build e2e_filesystems

// Package filesystems_test proves that a compute shared filesystem is
// mountable, following the user guide in docs/services/compute.md
// ("Mounting a shared filesystem") step by step:
//
//  1. Terraform (official nebius/nebius provider) creates the filesystem, a
//     service account with editor on the project, an access key and an
//     Object Storage filesystem bucket that exposes the filesystem;
//  2. the access key secret is read with the Go SDK (GetSecret);
//  3. the official rclone/rclone image mounts the bucket with FUSE through
//     the mocker's S3 front, exactly as the guide configures it.
//
// The filesystem's files live in RustFS (started in Docker by
// test/rustfsfixture). Nothing is written to RustFS directly: every write goes
// through the mount.
package filesystems_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	computesvc "github.com/nebius/gosdk/services/nebius/compute/v1"
	iamv2svc "github.com/nebius/gosdk/services/nebius/iam/v2"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/objstore"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/compute"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfcache"
	"github.com/kfirgollan/NebiusMocker/test/internal/tfharness"
	"github.com/kfirgollan/NebiusMocker/test/rustfsfixture"
)

// rcloneImage is the pinned official rclone image (multi-arch).
const rcloneImage = "rclone/rclone:1.71.2"

// rcloneLabel marks the containers this suite starts (make e2e-filesystems
// removes leftovers of a killed run).
const rcloneLabel = "nebius-mocker.test=rclone"

const (
	mib = int64(1) << 20
	gib = int64(1) << 30
)

func TestMain(m *testing.M) {
	code := tfcache.Main(m)
	rustfsfixture.Cleanup()
	os.Exit(code)
}

// infra is the Terraform configuration of the guide: the filesystem, the
// credentials and one filesystem bucket per name in buckets.
func infra(tenant, project, suffix string, buckets ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `
resource "nebius_compute_v1_filesystem" "shared" {
  parent_id        = %[1]q
  name             = "shared-%[2]s"
  type             = "NETWORK_SSD"
  size_bytes       = %[3]d
  block_size_bytes = 4096
}

resource "nebius_iam_v1_service_account" "mount" {
  parent_id = %[1]q
  name      = "fs-mount-%[2]s"
}

data "nebius_iam_v1_group" "editors" {
  parent_id = %[4]q
  name      = "editors"
}

resource "nebius_iam_v1_group_membership" "mount" {
  parent_id = data.nebius_iam_v1_group.editors.id
  member_id = nebius_iam_v1_service_account.mount.id
}

resource "nebius_iam_v2_access_key" "mount" {
  parent_id            = %[1]q
  description          = "rclone mount"
  secret_delivery_mode = "EXPLICIT"
  account = {
    service_account = { id = nebius_iam_v1_service_account.mount.id }
  }
}
`, project, suffix, gib, tenant)
	for _, name := range buckets {
		fmt.Fprintf(&b, `
resource "nebius_storage_v1_bucket" %[1]q {
  parent_id = %[2]q
  name      = %[1]q
  filesystem_bucket = {
    filesystem_id = nebius_compute_v1_filesystem.shared.id
  }
}
`, name, project)
	}
	return b.String()
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// rclone runs script in a fresh official rclone container whose
// /config/rclone/rclone.conf is the guide's configuration, with FUSE enabled.
// It returns the combined output and the script's error.
func rclone(t *testing.T, confDir, script string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--label", rcloneLabel,
		"--device", "/dev/fuse", "--cap-add", "SYS_ADMIN", "--security-opt", "apparmor:unconfined",
		"-v", confDir+":/config/rclone:ro", "--entrypoint", "sh", rcloneImage, "-ec", script)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// mounted wraps body between the guide's mount command and an unmount, so
// every write is uploaded before the container exits.
func mounted(bucket, body string) string {
	return fmt.Sprintf(`mkdir -p /mnt/fs
rclone mount nebius:%s /mnt/fs --daemon --daemon-wait 30s
%s
umount /mnt/fs
`, bucket, body)
}

func TestMountFilesystem(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found on PATH")
	}
	srv := rustfsfixture.Start(t)
	store := srv.Client(t)
	mod := storage.New(storage.WithS3Listen("127.0.0.1:0"))
	inst := testkit.StartT(t, []mocker.Module{iam.New(), compute.New(), mod},
		testkit.WithObjStore(store), testkit.WithAuthnRequired(), testkit.WithAuthz(authz.ModeEnforce),
		testkit.WithOperationDelay(100*time.Millisecond))
	sdk := inst.SDK(t)
	suffix := randomSuffix(t)
	first, second := "fsb-a-"+suffix, "fsb-b-"+suffix

	// 1. Terraform: filesystem + credentials + a filesystem bucket, no drift.
	tf := tfharness.New(t, inst.Addr, inst.Token)
	tf.Write(t, infra(inst.TenantID, inst.ProjectID, suffix, first))
	tf.Init(t)
	tf.ExpectPlan(t, map[string]string{
		"nebius_compute_v1_filesystem.shared": "create", "nebius_iam_v1_service_account.mount": "create",
		"nebius_iam_v1_group_membership.mount": "create", "nebius_iam_v2_access_key.mount": "create",
		"nebius_storage_v1_bucket." + first: "create",
	})
	tf.Apply(t)
	tf.ExpectNoChanges(t)
	state := tf.State(t)
	fsID, _ := state["nebius_compute_v1_filesystem.shared"]["id"].(string)
	keyID, _ := state["nebius_iam_v2_access_key.mount"]["id"].(string)
	fs, err := computesvc.NewFilesystemService(sdk).Get(t.Context(), &computepb.GetFilesystemRequest{Id: fsID})
	if err != nil || fs.GetStatus().GetState() != computepb.FilesystemStatus_READY || fs.GetStatus().GetSizeBytes() != gib {
		t.Fatalf("filesystem after apply = %v, %v", fs, err)
	}
	bkt, err := storagesvc.NewBucketService(sdk).GetByName(t.Context(), &storagepb.GetBucketByNameRequest{ParentId: inst.ProjectID, Name: first})
	if err != nil || bkt.GetStatus().GetBucketType() != storagepb.BucketStatus_FILESYSTEM {
		t.Fatalf("filesystem bucket = %v, %v", bkt, err)
	}

	// 2. The access key secret (the guide uses `nebius iam v2 access-key get-secret`).
	sec, err := iamv2svc.NewAccessKeyService(sdk).GetSecret(t.Context(), &iamv2pb.GetAccessKeySecretRequest{Id: keyID})
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}

	// 3. The guide's rclone.conf. Containers on Docker Desktop reach the
	// mocker on the host as host.docker.internal.
	_, port, _ := strings.Cut(mod.S3Addr(), ":")
	confDir := t.TempDir()
	conf := fmt.Sprintf(`[nebius]
type = s3
provider = Other
access_key_id = %s
secret_access_key = %s
endpoint = http://host.docker.internal:%s
region = eu-north1
force_path_style = true
`, sec.GetAwsAccessKeyId(), sec.GetSecret(), port)
	if err := os.WriteFile(filepath.Join(confDir, "rclone.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}

	// 4. Write files through a mount.
	out, err := rclone(t, confDir, mounted(first, `mkdir -p /mnt/fs/data/nested
echo "hello from mount one" > /mnt/fs/data/hello.txt
head -c 3145728 /dev/urandom > /mnt/fs/data/nested/blob.bin
sha256sum /mnt/fs/data/nested/blob.bin | cut -d' ' -f1 > /tmp/sum
cat /tmp/sum`))
	if err != nil {
		t.Fatalf("write through mount: %v\n%s", err, out)
	}
	sum := lastLine(out)

	// 5. A second mount (a new container) reads them back.
	out, err = rclone(t, confDir, mounted(first, `cat /mnt/fs/data/hello.txt
sha256sum /mnt/fs/data/nested/blob.bin | cut -d' ' -f1`))
	if err != nil {
		t.Fatalf("read through second mount: %v\n%s", err, out)
	}
	if !strings.Contains(out, "hello from mount one") || lastLine(out) != sum {
		t.Fatalf("second mount read:\n%s\nwant hello text and sha %s", out, sum)
	}

	// 6. The filesystem size is the capacity: a write past 1 GiB fails, the
	// files written before stay, and space freed by a delete is reusable.
	out, err = rclone(t, confDir, mounted(first, `if head -c 1100000000 /dev/zero > /mnt/fs/big.bin; then echo WRITE-OK; else echo WRITE-FAILED; fi
rm -f /mnt/fs/big.bin
echo after > /mnt/fs/data/after.txt
ls /mnt/fs/data`))
	if err != nil {
		t.Fatalf("over-capacity script: %v\n%s", err, out)
	}
	t.Logf("over-capacity mount output:\n%s", out)
	if !strings.Contains(out, "WRITE-FAILED") || !strings.Contains(out, "after.txt") || !strings.Contains(out, "hello.txt") {
		t.Fatalf("over-capacity write must fail and later writes succeed:\n%s", out)
	}

	// 7. Deleting the bucket keeps the files: a new filesystem bucket on the
	// same filesystem shows them.
	tf.Write(t, infra(inst.TenantID, inst.ProjectID, suffix, second))
	tf.ExpectPlan(t, map[string]string{
		"nebius_compute_v1_filesystem.shared": "no-op", "nebius_iam_v1_service_account.mount": "no-op",
		"nebius_iam_v1_group_membership.mount": "no-op", "nebius_iam_v2_access_key.mount": "no-op",
		"nebius_storage_v1_bucket." + first: "delete", "nebius_storage_v1_bucket." + second: "create",
	})
	tf.Apply(t)
	tf.ExpectNoChanges(t)
	out, err = rclone(t, confDir, mounted(second, `cat /mnt/fs/data/hello.txt
cat /mnt/fs/data/after.txt`))
	if err != nil || !strings.Contains(out, "hello from mount one") || !strings.Contains(out, "after") {
		t.Fatalf("new bucket on the same filesystem: %v\n%s", err, out)
	}

	// 8. The filesystem cannot be deleted while a bucket exposes it.
	_, err = computesvc.NewFilesystemService(sdk).Delete(t.Context(), &computepb.DeleteFilesystemRequest{Id: fsID})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete of an exposed filesystem: %v, want FailedPrecondition", err)
	}

	// 9. Destroy: the bucket, then the filesystem and its files are gone.
	tf.Destroy(t)
	if _, err := computesvc.NewFilesystemService(sdk).Get(t.Context(), &computepb.GetFilesystemRequest{Id: fsID}); status.Code(err) != codes.NotFound {
		t.Fatalf("filesystem after destroy: %v, want NotFound", err)
	}
	if _, err := store.BucketEmpty(t.Context(), "nebius-mocker-fs-"+fsID); !errors.Is(err, objstore.ErrBucketNotFound) {
		t.Fatalf("files of the destroyed filesystem: %v, want the store bucket gone", err)
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
