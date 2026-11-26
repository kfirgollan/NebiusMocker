package contract

import (
	"context"
	"fmt"
	"testing"

	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	dnspb "github.com/nebius/gosdk/proto/nebius/dns/v1"
	kmspb "github.com/nebius/gosdk/proto/nebius/kms/v1"
	mysteryboxpb "github.com/nebius/gosdk/proto/nebius/mysterybox/v1"
	quotaspb "github.com/nebius/gosdk/proto/nebius/quotas/v1"
	registrypb "github.com/nebius/gosdk/proto/nebius/registry/v1"
	storagepb "github.com/nebius/gosdk/proto/nebius/storage/v1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	dnssvc "github.com/nebius/gosdk/services/nebius/dns/v1"
	kmssvc "github.com/nebius/gosdk/services/nebius/kms/v1"
	mysteryboxsvc "github.com/nebius/gosdk/services/nebius/mysterybox/v1"
	quotassvc "github.com/nebius/gosdk/services/nebius/quotas/v1"
	registrysvc "github.com/nebius/gosdk/services/nebius/registry/v1"
	storagesvc "github.com/nebius/gosdk/services/nebius/storage/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/authz"
	"github.com/kfirgollan/NebiusMocker/pkg/mocker"
	"github.com/kfirgollan/NebiusMocker/pkg/quota"
	"github.com/kfirgollan/NebiusMocker/pkg/testkit"
	"github.com/kfirgollan/NebiusMocker/services/dns"
	"github.com/kfirgollan/NebiusMocker/services/iam"
	"github.com/kfirgollan/NebiusMocker/services/kms"
	"github.com/kfirgollan/NebiusMocker/services/mysterybox"
	"github.com/kfirgollan/NebiusMocker/services/quotas"
	"github.com/kfirgollan/NebiusMocker/services/registry"
	"github.com/kfirgollan/NebiusMocker/services/storage"
	"github.com/kfirgollan/NebiusMocker/services/vpc"
)

// TestQuotaEnforcedPerArea proves each wired area end to end through gosdk:
// with the project allowance lowered to the live usage, the area's create is
// refused with RESOURCE_EXHAUSTED + QuotaFailure naming the quota, and the
// usage the allowance reports does not move; one more unit admits it and the
// usage follows.
func TestQuotaEnforcedPerArea(t *testing.T) {
	t.Parallel()
	inst := testkit.StartT(t, []mocker.Module{
		iam.New(), vpc.New(), quotas.New(), dns.New(dns.WithListen("127.0.0.1:0")),
		storage.New(storage.WithS3Listen("127.0.0.1:0")), registry.New(), mysterybox.New(), kms.New(),
	}, testkit.WithAuthz(authz.ModeEnforce))
	sdk := inst.SDK(t)
	e := quotaEnv{inst: inst, quotas: quotassvc.NewQuotaAllowanceService(sdk), vpc: vpcClientsFor(t, inst, inst.Token)}
	p := inst.ProjectID
	ctx := t.Context()
	defNet, err := e.vpc.networks.GetByName(ctx, &vpcpb.GetNetworkByNameRequest{ParentId: p, Name: "default-network"})
	if err != nil {
		t.Fatalf("default network: %v", err)
	}
	netID := defNet.GetMetadata().GetId()
	pubPool := seedPoolID(t, e.vpc, seedPublicPool)
	buckets, registries := storagesvc.NewBucketService(sdk), registrysvc.NewRegistryService(sdk)
	secrets, zones := mysteryboxsvc.NewSecretService(sdk), dnssvc.NewZoneService(sdk)
	sym, asym := kmssvc.NewSymmetricKeyService(sdk), kmssvc.NewAsymmetricKeyService(sdk)
	meta := func(name string) *commonpb.ResourceMetadata {
		return &commonpb.ResourceMetadata{ParentId: p, Name: name}
	}

	areas := []struct {
		quota  string
		create func(ctx context.Context, name string) error
	}{
		{quota.VPCSubnetCount, func(ctx context.Context, n string) error {
			return waitOK(ctx)(e.vpc.subnets.Create(ctx, &vpcpb.CreateSubnetRequest{Metadata: meta(n), Spec: &vpcpb.SubnetSpec{NetworkId: netID}}))
		}},
		{quota.VPCSecurityGroupCount, func(ctx context.Context, n string) error {
			return waitOK(ctx)(e.vpc.groups.Create(ctx, &vpcpb.CreateSecurityGroupRequest{Metadata: meta(n), Spec: &vpcpb.SecurityGroupSpec{NetworkId: netID}}))
		}},
		{quota.VPCPublicIPv4Count, func(ctx context.Context, n string) error {
			return waitOK(ctx)(e.vpc.allocations.Create(ctx, &vpcpb.CreateAllocationRequest{Metadata: meta(n), Spec: pubAllocSpec("", pubPool, "")}))
		}},
		{quota.DNSZoneCount, func(ctx context.Context, n string) error {
			return waitOK(ctx)(zones.Create(ctx, &dnspb.CreateZoneRequest{Metadata: meta(n), Spec: &dnspb.ZoneSpec{
				DomainName: n + ".example.", Scope: &dnspb.ZoneSpec_Vpc{Vpc: &dnspb.VpcZoneScope{PrimaryNetworkId: netID}},
			}}))
		}},
		{quota.StorageBucketCount, func(ctx context.Context, n string) error {
			return waitOK(ctx)(buckets.Create(ctx, &storagepb.CreateBucketRequest{Metadata: meta(n)}))
		}},
		{quota.RegistryCount, func(ctx context.Context, n string) error {
			return waitOK(ctx)(registries.Create(ctx, &registrypb.CreateRegistryRequest{Metadata: meta(n), Spec: &registrypb.RegistrySpec{}}))
		}},
		{quota.MysteryBoxSecretCount, func(ctx context.Context, n string) error {
			return waitOK(ctx)(secrets.Create(ctx, &mysteryboxpb.CreateSecretRequest{Metadata: meta(n), Spec: &mysteryboxpb.SecretSpec{}}))
		}},
		{quota.KMSSymmetricKeyCount, func(ctx context.Context, n string) error {
			return waitOK(ctx)(sym.Create(ctx, &kmspb.CreateSymmetricKeyRequest{
				Metadata: meta(n),
				Spec:     &kmspb.SymmetricKeySpec{Algorithm: kmspb.SymmetricAlgorithm_AES_256},
			}))
		}},
		{quota.KMSAsymmetricKeyCount, func(ctx context.Context, n string) error {
			return waitOK(ctx)(asym.Create(ctx, &kmspb.CreateAsymmetricKeyRequest{
				Metadata: meta(n),
				Spec:     &kmspb.AsymmetricKeySpec{Algorithm: kmspb.AsymmetricAlgorithm_ECDSA_NIST_P256_SHA_256},
			}))
		}},
	}
	for i, a := range areas {
		// Not parallel: the areas share one project, and a public allocation
		// would otherwise race the subnet for vpc.* usage.
		used := e.byName(t, p, a.quota).GetStatus().GetUsage()
		e.setLimit(t, p, a.quota, used)
		name := fmt.Sprintf("quota-area-%d", i)
		qf := quotaFailure(t, a.create(ctx, name))
		if v := qf.Violations(); len(v) == 0 || v[0].GetQuota() != a.quota {
			t.Fatalf("%s: violations = %v", a.quota, v)
		}
		if got := e.byName(t, p, a.quota).GetStatus().GetUsage(); got != used {
			t.Fatalf("%s: usage after a refused create = %d, want %d", a.quota, got, used)
		}
		e.setLimit(t, p, a.quota, used+1)
		if err := a.create(ctx, name); err != nil {
			t.Fatalf("%s: create after raising the allowance: %v", a.quota, err)
		}
		got := e.byName(t, p, a.quota)
		if got.GetStatus().GetUsage() != used+1 || got.GetStatus().GetUsageState() != quotaspb.QuotaAllowanceStatus_USAGE_STATE_USED {
			t.Fatalf("%s: after create = %v, want usage %d", a.quota, got.GetStatus(), used+1)
		}
	}
}
