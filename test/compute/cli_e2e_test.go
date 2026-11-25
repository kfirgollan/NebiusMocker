//go:build e2e_images

package compute

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	computepb "github.com/nebius/gosdk/proto/nebius/compute/v1"
	iampb "github.com/nebius/gosdk/proto/nebius/iam/v1"
	iamv2pb "github.com/nebius/gosdk/proto/nebius/iam/v2"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/services/compute/domain"
	"github.com/kfirgollan/NebiusMocker/test/internal/awscli"
	"github.com/kfirgollan/NebiusMocker/test/internal/imagescenario"
	"github.com/kfirgollan/NebiusMocker/test/internal/nebiuscli"
	"github.com/kfirgollan/NebiusMocker/test/internal/s3upload"
)

var unmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

// cliJSON runs a CLI command with --format json and decodes the output.
func cliJSON(t *testing.T, cli *nebiuscli.CLI, into proto.Message, args ...string) {
	t.Helper()
	res, err := cli.Run(t.Context(), append(args, "--format", "json")...)
	if err != nil {
		t.Fatalf("nebius %s: %v", strings.Join(args, " "), err)
	}
	if err := unmarshal.Unmarshal([]byte(res.Stdout), into); err != nil {
		t.Fatalf("nebius %s: decode %q: %v", strings.Join(args, " "), res.Stdout, err)
	}
}

func names(imgs []*computepb.Image) []string {
	out := make([]string, 0, len(imgs))
	for _, i := range imgs {
		out = append(out, i.GetMetadata().GetName())
	}
	slices.Sort(out)
	return out
}

// TestCLIPublicImages: list-public per region, get, get-latest-by-family.
func TestCLIPublicImages(t *testing.T) {
	env := startOnRustFS(t)
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: env.Inst.Addr, Token: env.Inst.Token, ParentID: env.Inst.ProjectID, TenantID: env.Inst.TenantID})

	for _, region := range config.DefaultRegions() {
		var resp computepb.ListImagesResponse
		cliJSON(t, cli, &resp, "compute", "image", "list-public", "--region", region.Name, "--page-size", "1000")
		want := names(imagescenario.ListAll(t, env.Env, "", region.Name, 0))
		if got := names(resp.GetItems()); !slices.Equal(got, want) || len(got) == 0 {
			t.Fatalf("list-public --region %s = %v, want %v", region.Name, got, want)
		}
		for _, img := range resp.GetItems() {
			if img.GetMetadata().GetParentId() != domain.PublicProjectID(region.RoutingCode) || img.GetStatus().GetState() != computepb.ImageStatus_READY {
				t.Fatalf("list-public --region %s: %v", region.Name, img)
			}
		}
	}

	var latest computepb.Image
	cliJSON(t, cli, &latest, "compute", "image", "get-latest-by-family",
		"--image-family", "ubuntu24.04-driverless", "--parent-id", domain.PublicProjectID("e00"))
	if latest.GetSpec().GetVersion() != "20251015" || latest.GetSpec().GetImageFamily() != "ubuntu24.04-driverless" {
		t.Fatalf("get-latest-by-family = %v", &latest)
	}
	// Without --parent-id the request defaults to the public-images project.
	var dflt computepb.Image
	cliJSON(t, cli, &dflt, "compute", "image", "get-latest-by-family", "--image-family", "ubuntu24.04-driverless")
	if !proto.Equal(&dflt, &latest) {
		t.Fatalf("get-latest-by-family without --parent-id = %v, want %v", &dflt, &latest)
	}
	var deprecated computepb.Image
	cliJSON(t, cli, &deprecated, "compute", "image", "get-latest-by-family",
		"--image-family", "ubuntu22.04-cuda12", "--parent-id", domain.PublicProjectID("e00"))
	if deprecated.GetStatus().GetImageFamilyDeprecation().GetDeprecatedAt() == nil {
		t.Fatalf("deprecated family without deprecation status: %v", &deprecated)
	}
	var got computepb.Image
	cliJSON(t, cli, &got, "compute", "image", "get", "--id", latest.GetMetadata().GetId())
	if !proto.Equal(&got, &latest) {
		t.Fatalf("get --id = %v, want %v", &got, &latest)
	}
	imagescenario.AssertBlob(t, env.Store, &got)
	var byName computepb.Image
	cliJSON(t, cli, &byName, "compute", "image", "get-by-name",
		"--parent-id", domain.PublicProjectID("u00"), "--name", "ubuntu24.04-cuda12-20251015")
	if byName.GetSpec().GetImageFamily() != "ubuntu24.04-cuda12" {
		t.Fatalf("get-by-name = %v", &byName)
	}
}

// accessKeyForAWSCLI follows the Object Storage quick start
// (docs.nebius.com/object-storage/quickstart) with the nebius CLI: create a
// service account, add it to the editors group, create an access key and read
// its secret. It returns the AWS-style key ID and secret.
func accessKeyForAWSCLI(t *testing.T, cli *nebiuscli.CLI, tenantID string) (keyID, secret string) {
	t.Helper()
	var sa iampb.ServiceAccount
	cliJSON(t, cli, &sa, "iam", "service-account", "create", "--name", s3upload.Unique("object-storage-sa-"))
	var editors iampb.Group
	cliJSON(t, cli, &editors, "iam", "group", "get-by-name", "--name", "editors", "--parent-id", tenantID)
	if _, err := cli.Run(t.Context(), "iam", "group-membership", "create",
		"--parent-id", editors.GetMetadata().GetId(), "--member-id", sa.GetMetadata().GetId()); err != nil {
		t.Fatalf("group-membership create: %v", err)
	}
	var key iamv2pb.AccessKey
	cliJSON(t, cli, &key, "iam", "v2", "access-key", "create",
		"--account-service-account-id", sa.GetMetadata().GetId(), "--description", "AWS CLI")
	var got iamv2pb.AccessKey
	cliJSON(t, cli, &got, "iam", "v2", "access-key", "get", "--id", key.GetMetadata().GetId())
	if got.GetStatus().GetSecret() == "" || got.GetStatus().GetAwsAccessKeyId() == "" {
		t.Fatalf("access-key get returned no secret: %v", &got)
	}
	return got.GetStatus().GetAwsAccessKeyId(), got.GetStatus().GetSecret()
}

// TestCLICustomImage follows the docs' import flow end to end
// (docs.nebius.com/compute/storage/import-image):
//  1. create an Object Storage bucket: `nebius storage bucket create --name`;
//  2. upload the image file with the AWS CLI configured with a Nebius access
//     key: `aws s3 cp <file> s3://<bucket>/<object>`;
//  3. `nebius compute image create --source-storage-bucket-name … --source-storage-object-name …`;
//
// then list, get, update, list operations and delete through the CLI.
func TestCLICustomImage(t *testing.T) {
	env := startOnRustFS(t)
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: env.Inst.Addr, Token: env.Inst.Token, ParentID: env.Inst.ProjectID, TenantID: env.Inst.TenantID})
	payload := domain.SyntheticQCOW2(30<<30, 256<<10, "cli-upload")
	file := filepath.Join(t.TempDir(), "ubuntu-custom.qcow2")
	if err := os.WriteFile(file, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	bucket := env.Bucket("cli-bucket")

	if _, err := cli.Run(t.Context(), "storage", "bucket", "create", "--name", bucket); err != nil {
		t.Fatalf("storage bucket create: %v", err)
	}
	keyID, secret := accessKeyForAWSCLI(t, cli, env.Inst.TenantID)
	aws := awscli.New(t)
	aws.Configure(t, keyID, secret, s3upload.Region, "http://"+env.s3Addr)
	if _, err := aws.Run(t.Context(), "s3", "cp", file, "s3://"+bucket+"/images/ubuntu-custom.qcow2"); err != nil {
		t.Fatalf("upload: %v", err)
	}

	var created computepb.Image
	cliJSON(t, cli, &created, "compute", "image", "create",
		"--name", "cli-image",
		"--source-storage-bucket-name", bucket,
		"--source-storage-object-name", "images/ubuntu-custom.qcow2",
		"--image-family", "my-ubuntu", "--version", "1",
		"--cpu-architecture", "arm64",
		"--labels", "team=ml")
	id := created.GetMetadata().GetId()
	img, err := env.Images.Get(t.Context(), &computepb.GetImageRequest{Id: id})
	if err != nil {
		t.Fatalf("created image %q not in the mock: %v (cli output %v)", id, err, &created)
	}
	if img.GetStatus().GetState() != computepb.ImageStatus_READY || img.GetStatus().GetMinDiskSizeBytes() != 30<<30 ||
		img.GetSpec().GetCpuArchitecture() != computepb.ImageSpec_ARM64 || img.GetMetadata().GetParentId() != env.Inst.ProjectID ||
		img.GetMetadata().GetLabels()["team"] != "ml" {
		t.Fatalf("CLI-created image = %v", img)
	}
	if got := imagescenario.AssertBlob(t, env.Store, img); len(got) != len(payload) {
		t.Fatalf("payload in RustFS has %d bytes, uploaded %d", len(got), len(payload))
	}

	var list computepb.ListImagesResponse
	cliJSON(t, cli, &list, "compute", "image", "list")
	if got := names(list.GetItems()); !slices.Equal(got, []string{"cli-image"}) {
		t.Fatalf("list = %v", got)
	}
	var byName computepb.Image
	cliJSON(t, cli, &byName, "compute", "image", "get-by-name", "--name", "cli-image")
	if byName.GetMetadata().GetId() != id {
		t.Fatalf("get-by-name = %v", &byName)
	}
	var latest computepb.Image
	cliJSON(t, cli, &latest, "compute", "image", "get-latest-by-family", "--image-family", "my-ubuntu", "--parent-id", env.Inst.ProjectID)
	if latest.GetMetadata().GetId() != id {
		t.Fatalf("get-latest-by-family (custom) = %v", &latest)
	}

	if _, err := cli.Run(t.Context(), "compute", "image", "update", "--id", id, "--description", "golden", "--labels-add", "tier=gold"); err != nil {
		t.Fatalf("update: %v", err)
	}
	img, err = env.Images.Get(t.Context(), &computepb.GetImageRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	if img.GetSpec().GetDescription() != "golden" || img.GetMetadata().GetLabels()["tier"] != "gold" || img.GetMetadata().GetLabels()["team"] != "ml" ||
		img.GetSpec().GetImageFamily() != "my-ubuntu" || img.GetMetadata().GetResourceVersion() != 2 {
		t.Fatalf("after CLI update: %v", img)
	}

	var ops commonpb.ListOperationsResponse
	cliJSON(t, cli, &ops, "compute", "image", "list-operations-by-parent", "--parent-id", env.Inst.ProjectID)
	if len(ops.GetOperations()) != 2 || ops.GetOperations()[0].GetDescription() != "Update image" {
		t.Fatalf("list-operations-by-parent = %v", &ops)
	}

	if _, err := cli.Run(t.Context(), "compute", "image", "delete", "--id", id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := env.Images.Get(t.Context(), &computepb.GetImageRequest{Id: id}); err == nil {
		t.Fatal("image still exists after CLI delete")
	}
	cliJSON(t, cli, &list, "compute", "image", "list")
	if len(list.GetItems()) != 0 {
		t.Fatalf("list after delete = %v", names(list.GetItems()))
	}
}

// TestCLIImageFromDiskAndSnapshot: `nebius compute disk create`, `nebius
// compute disk-snapshot create`, then `nebius compute image create
// --source-disk-id` / `--source-disk-snapshot-id`.
func TestCLIImageFromDiskAndSnapshot(t *testing.T) {
	env := startOnRustFS(t)
	cli := nebiuscli.New(t, nebiuscli.Profile{Addr: env.Inst.Addr, Token: env.Inst.Token, ParentID: env.Inst.ProjectID, TenantID: env.Inst.TenantID})

	var disk computepb.Disk
	cliJSON(t, cli, &disk, "compute", "disk", "create", "--name", "cli-disk", "--size-gibibytes", "25", "--type", "network_ssd")
	var snap computepb.DiskSnapshot
	cliJSON(t, cli, &snap, "compute", "disk-snapshot", "create", "--name", "cli-snap", "--source-disk-id", disk.GetMetadata().GetId())

	for _, tc := range []struct{ name, flag, id string }{
		{"from-disk", "--source-disk-id", disk.GetMetadata().GetId()},
		{"from-snapshot", "--source-disk-snapshot-id", snap.GetMetadata().GetId()},
	} {
		var created computepb.Image
		cliJSON(t, cli, &created, "compute", "image", "create", "--name", tc.name, tc.flag, tc.id)
		img, err := env.Images.Get(t.Context(), &computepb.GetImageRequest{Id: created.GetMetadata().GetId()})
		if err != nil {
			t.Fatalf("%s: image not in the mock: %v", tc.name, err)
		}
		if img.GetStatus().GetState() != computepb.ImageStatus_READY || img.GetStatus().GetMinDiskSizeBytes() != 25<<30 {
			t.Fatalf("%s: image = %v", tc.name, img)
		}
		imagescenario.AssertBlob(t, env.Store, img)
	}
}
