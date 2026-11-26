//nolint:staticcheck // SA1019: nebius.msp.postgresql.v1alpha1 returns the deprecated common.v1alpha1 operations; serving them is what these tests check
package contract

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/nebius/gosdk/proto/fieldmask/grpcheader"
	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	commonv1alpha1pb "github.com/nebius/gosdk/proto/nebius/common/v1alpha1"
	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"
	pgconfigpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1/config"
	mspv1alpha1pb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1"
	vpcpb "github.com/nebius/gosdk/proto/nebius/vpc/v1"
	vpcsvc "github.com/nebius/gosdk/services/nebius/vpc/v1"
)

func TestMSPPostgresClusterLifecycle(t *testing.T) {
	t.Parallel()
	e := startPG(t)
	ctx := t.Context()

	op, err := e.clusters.Create(ctx, e.createReq("main", func(s *pgpb.ClusterSpec) {
		s.Description = "orders"
		s.Config.PublicAccess = true
		s.Config.PoolerConfig = &pgpb.ConnectionPoolerConfig{PoolingMode: pgpb.ConnectionPoolerConfig_TRANSACTION}
	}))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if op.ResourceID() == "" || op.Description() == "" {
		t.Fatalf("operation = %v", op)
	}
	// The operation is observable through the cluster service.
	if got, err := e.clusters.GetOperation(ctx, &commonv1alpha1pb.GetOperationRequest{Id: op.ID()}); err != nil || got.ID() != op.ID() {
		t.Errorf("GetOperation = %v, %v", got, err)
	}
	if _, err := op.Wait(ctx); err != nil {
		t.Fatalf("operation: %v", err)
	}
	id := op.ResourceID()

	c := e.getCluster(t, id)
	if phase, state := pgPhase(c); phase != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING || state != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Fatalf("status = %v, want RUNNING/FINISHED", c.GetStatus())
	}
	if c.GetMetadata().GetName() != "main" || c.GetMetadata().GetParentId() != e.project || c.GetMetadata().GetResourceVersion() != 1 || c.GetMetadata().GetCreatedAt() == nil {
		t.Errorf("metadata = %v", c.GetMetadata())
	}
	checkNoPassword(t, c)
	if c.GetSpec().GetNetworkId() == "" || c.GetSpec().GetDescription() != "orders" || c.GetStatus().GetPresetDetails().GetCpuCount() != 2 || c.GetStatus().GetPresetDetails().GetMemoryGibibytes() != 8 {
		t.Errorf("spec/status = %v / %v", c.GetSpec(), c.GetStatus())
	}
	ep := c.GetStatus().GetConnectionEndpoints()
	for name, v := range map[string]string{
		"private_read_write": ep.GetPrivateReadWrite(), "private_read_only": ep.GetPrivateReadOnly(),
		"public_read_write": ep.GetPublicReadWrite(), "public_read_only": ep.GetPublicReadOnly(),
	} {
		if !strings.HasPrefix(v, id+".") || !strings.HasSuffix(v, ":5432") {
			t.Errorf("endpoint %s = %q", name, v)
		}
	}
	// The cluster is bound to its network.
	netSDK := vpcsvc.NewNetworkService(e.inst.SDK(t))
	if _, err := netSDK.Delete(ctx, &vpcpb.DeleteNetworkRequest{Id: c.GetSpec().GetNetworkId()}); err == nil {
		t.Error("deleted a network that a PostgreSQL cluster is bound to")
	}

	byName, err := e.clusters.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: e.project, Name: "main"})
	if err != nil || byName.GetMetadata().GetId() != id {
		t.Errorf("GetByName = %v, %v", byName, err)
	}
	checkNoPassword(t, byName)
	second := e.newCluster(t, "second", nil)
	l, err := e.clusters.List(ctx, &pgpb.ListClustersRequest{ParentId: e.project, PageSize: 1})
	if err != nil || len(l.GetClusters()) != 1 || l.GetNextPageToken() == "" || l.GetClusters()[0].GetMetadata().GetId() != id {
		t.Fatalf("List page 1 = %v, %v", l, err)
	}
	checkNoPassword(t, l.GetClusters()[0])
	l, err = e.clusters.List(ctx, &pgpb.ListClustersRequest{ParentId: e.project, PageSize: 1, PageToken: l.GetNextPageToken()})
	if err != nil || len(l.GetClusters()) != 1 || l.GetClusters()[0].GetMetadata().GetId() != second || l.GetNextPageToken() != "" {
		t.Errorf("List page 2 = %v, %v", l, err)
	}
	var all int
	for c, err := range e.clusters.Filter(ctx, &pgpb.ListClustersRequest{ParentId: e.project, PageSize: 1}) {
		if err != nil {
			t.Fatalf("Filter: %v", err)
		}
		all++
		checkNoPassword(t, c)
	}
	if all != 2 {
		t.Errorf("Filter saw %d clusters, want 2", all)
	}

	// Update: a bigger preset, hosts and disk, plus a label. The password is
	// never returned, also not when it was just changed.
	cur := e.getCluster(t, id)
	cur.Spec.Config.Template.Resources.Preset = "4vcpu-16gb"
	cur.Spec.Config.Template.Hosts.Count = 2
	cur.Spec.Config.Template.Disk.SizeGibibytes = 40
	cur.Spec.Bootstrap.UserPassword = "An0ther-pw-value"
	cur.Metadata.Labels = map[string]string{"team": "data"}
	alphaWait(t)(e.clusters.Update(ctx, &pgpb.UpdateClusterRequest{Metadata: cur.GetMetadata(), Spec: cur.GetSpec()}))
	up := e.getCluster(t, id)
	tpl := up.GetSpec().GetConfig().GetTemplate()
	if tpl.GetResources().GetPreset() != "4vcpu-16gb" || tpl.GetHosts().GetCount() != 2 || tpl.GetDisk().GetSizeGibibytes() != 40 ||
		up.GetStatus().GetPresetDetails().GetCpuCount() != 4 || up.GetMetadata().GetLabels()["team"] != "data" || up.GetMetadata().GetResourceVersion() != 2 {
		t.Errorf("after update: %v", up)
	}
	checkNoPassword(t, up)
	// A stale resource_version conflicts.
	_, err = e.clusters.Update(ctx, &pgpb.UpdateClusterRequest{Metadata: cur.GetMetadata(), Spec: cur.GetSpec()})
	wantFailure(t, "stale update", err, codes.Aborted)

	// Stop and Start.
	alphaWait(t)(e.clusters.Stop(ctx, &pgpb.StopClusterRequest{Id: id}))
	if phase, state := pgPhase(e.getCluster(t, id)); phase != mspv1alpha1pb.ClusterStatus_PHASE_STOPPING || state != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Errorf("after Stop: phase %v state %v, want STOPPING/FINISHED", phase, state)
	}
	alphaWait(t)(e.clusters.Start(ctx, &pgpb.StartClusterRequest{Id: id}))
	if phase, state := pgPhase(e.getCluster(t, id)); phase != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING || state != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Errorf("after Start: phase %v state %v, want RUNNING/FINISHED", phase, state)
	}

	alphaWait(t)(e.clusters.Delete(ctx, &pgpb.DeleteClusterRequest{Id: id}))
	_, err = e.clusters.Get(ctx, &pgpb.GetClusterRequest{Id: id})
	wantFailure(t, "get after delete", err, codes.NotFound)
	if _, err := netSDK.Get(ctx, &vpcpb.GetNetworkRequest{Id: c.GetSpec().GetNetworkId()}); err != nil {
		t.Errorf("network after delete: %v", err)
	}
	// The name is free again.
	e.newCluster(t, "main", nil)
}

func checkNoPassword(t *testing.T, c *pgpb.Cluster) {
	t.Helper()
	if pw := c.GetSpec().GetBootstrap().GetUserPassword(); pw != "" {
		t.Errorf("cluster %s returned bootstrap.user_password %q", c.GetMetadata().GetId(), pw)
	}
}

func TestMSPPostgresConfigIsEchoedAsSent(t *testing.T) {
	t.Parallel()
	e := startPG(t)
	cfg := &pgconfigpb.PostgresqlConfig16{
		StatementTimeout: proto.Int64(30000), MaxConnections: proto.Int64(150), SharedBuffers: proto.Int64(262144),
		AutovacuumVacuumScaleFactor: proto.Float64(0.1), DefaultTransactionReadOnly: proto.Bool(true), SearchPath: proto.String("public"),
	}
	maxPool := int64(50)
	id := e.newCluster(t, "tuned", func(s *pgpb.ClusterSpec) {
		s.Config.Config = &pgpb.ConfigSpec_PostgresqlConfig_16{PostgresqlConfig_16: cfg}
		s.Config.PoolerConfig = &pgpb.ConnectionPoolerConfig{PoolingMode: pgpb.ConnectionPoolerConfig_SESSION, MaxPoolSize: &maxPool}
		s.Backup = &pgpb.BackupSpec{BackupWindowStart: "04:30:00", RetentionPolicy: "14d"}
	})
	c := e.getCluster(t, id)
	if !proto.Equal(c.GetSpec().GetConfig().GetPostgresqlConfig_16(), cfg) {
		t.Errorf("postgresql_config_16 = %v, want %v", c.GetSpec().GetConfig().GetPostgresqlConfig_16(), cfg)
	}
	if p := c.GetSpec().GetConfig().GetPoolerConfig(); p.GetPoolingMode() != pgpb.ConnectionPoolerConfig_SESSION || p.GetMaxPoolSize() != 50 {
		t.Errorf("pooler_config = %v", p)
	}
	if b := c.GetSpec().GetBackup(); b.GetBackupWindowStart() != "04:30:00" || b.GetRetentionPolicy() != "14d" {
		t.Errorf("backup = %v", b)
	}

	// A minimal cluster round-trips without defaults filled in: Terraform
	// would plan a diff otherwise.
	plain := e.getCluster(t, e.newCluster(t, "plain", nil))
	want := pgSpec()
	want.NetworkId = plain.GetSpec().GetNetworkId()
	want.Bootstrap.UserPassword = ""
	if !proto.Equal(plain.GetSpec(), want) || want.GetNetworkId() == "" {
		t.Errorf("spec = %v, want exactly what was sent plus network_id %v", plain.GetSpec(), want)
	}
}

func TestMSPPostgresUpdateClearsWhatTheRequestOmits(t *testing.T) {
	t.Parallel()
	e := startPG(t)
	ctx := t.Context()
	id := e.newCluster(t, "main", func(s *pgpb.ClusterSpec) {
		s.Config.PoolerConfig = &pgpb.ConnectionPoolerConfig{PoolingMode: pgpb.ConnectionPoolerConfig_TRANSACTION}
		s.Config.PublicAccess = true
	})
	// The SDK sends an X-ResetMask naming the fields the request leaves
	// unset: omitting pooler_config clears it, everything else is kept.
	cur := e.getCluster(t, id)
	spec := proto.CloneOf(cur.GetSpec())
	spec.Config.PoolerConfig = nil
	alphaWait(t)(e.clusters.Update(ctx, &pgpb.UpdateClusterRequest{Metadata: cur.GetMetadata(), Spec: spec}))
	got := e.getCluster(t, id)
	if got.GetSpec().GetConfig().GetPoolerConfig() != nil || !got.GetSpec().GetConfig().GetPublicAccess() || got.GetSpec().GetConfig().GetTemplate() == nil {
		t.Errorf("config = %v, want pooler_config cleared and the rest kept", got.GetSpec().GetConfig())
	}

	// An explicit X-ResetMask is honored too: clear public_access.
	maskCtx, err := grpcheader.AddResetMaskToOutgoingContext(ctx, mask.ParseMust("spec.config.public_access"))
	if err != nil {
		t.Fatalf("reset mask: %v", err)
	}
	spec = proto.CloneOf(got.GetSpec())
	spec.Config.PublicAccess = false
	alphaWait(t)(e.clusters.Update(maskCtx, &pgpb.UpdateClusterRequest{Metadata: got.GetMetadata(), Spec: spec}))
	if after := e.getCluster(t, id); after.GetSpec().GetConfig().GetPublicAccess() || after.GetStatus().GetConnectionEndpoints().GetPublicReadWrite() != "" {
		t.Errorf("after: %v, want no public access and no public endpoint", after)
	}
}

func TestMSPPostgresCreateRejections(t *testing.T) {
	t.Parallel()
	e := startPG(t)
	ctx := t.Context()
	tests := []struct {
		name  string
		mod   func(*pgpb.ClusterSpec)
		code  codes.Code
		field string
	}{
		{"version", func(s *pgpb.ClusterSpec) { s.Config.Version = "9.6" }, codes.InvalidArgument, "spec.config.version"},
		{"platform", func(s *pgpb.ClusterSpec) { s.Config.Template.Resources.Platform = "gpu-h100" }, codes.InvalidArgument, "resources.platform"},
		{"preset", func(s *pgpb.ClusterSpec) { s.Config.Template.Resources.Preset = "3vcpu-5gb" }, codes.InvalidArgument, "resources.preset"},
		{"disk type", func(s *pgpb.ClusterSpec) { s.Config.Template.Disk.Type = "floppy" }, codes.InvalidArgument, "disk.type"},
		{"disk size", func(s *pgpb.ClusterSpec) { s.Config.Template.Disk.SizeGibibytes = 1 }, codes.InvalidArgument, "disk.size_gibibytes"},
		{"hosts", func(s *pgpb.ClusterSpec) { s.Config.Template.Hosts.Count = 100 }, codes.InvalidArgument, "hosts.count"},
		{"password", func(s *pgpb.ClusterSpec) { s.Bootstrap.UserPassword = "" }, codes.InvalidArgument, "spec.bootstrap.user_password is required"},
		{"user name", func(s *pgpb.ClusterSpec) { s.Bootstrap.UserName = "no spaces" }, codes.InvalidArgument, "user_name"},
		{"db name", func(s *pgpb.ClusterSpec) { s.Bootstrap.DbName = "" }, codes.InvalidArgument, "db_name"},
		{"pool size", func(s *pgpb.ClusterSpec) {
			s.Config.PoolerConfig = &pgpb.ConnectionPoolerConfig{MaxPoolSize: proto.Int64(2)}
		}, codes.InvalidArgument, "max_pool_size"},
		{"retention", func(s *pgpb.ClusterSpec) { s.Backup = &pgpb.BackupSpec{RetentionPolicy: "3d"} }, codes.InvalidArgument, "retention_policy"},
		{"window", func(s *pgpb.ClusterSpec) { s.Backup = &pgpb.BackupSpec{BackupWindowStart: "midnight"} }, codes.InvalidArgument, "backup_window_start"},
		{"replication regions", func(s *pgpb.ClusterSpec) { s.Backup = &pgpb.BackupSpec{ReplicationRegions: []string{"eu-west1"}} }, codes.InvalidArgument, "cross-region backup replication is not emulated"},
		{"replication interval", func(s *pgpb.ClusterSpec) {
			s.Backup = &pgpb.BackupSpec{ReplicationInterval: durationpb.New(10 * 60e9)}
		}, codes.InvalidArgument, "replication"},
		{"no template", func(s *pgpb.ClusterSpec) { s.Config.Template = nil }, codes.InvalidArgument, "template"},
		{"unknown network", func(s *pgpb.ClusterSpec) { s.NetworkId = "vpcnetwork-e00doesnotexist" }, codes.NotFound, "vpcnetwork-e00doesnotexist"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := e.clusters.Create(ctx, e.createReq("bad-"+strings.ReplaceAll(tc.name, " ", "-"), tc.mod))
			wantFailure(t, tc.name, err, tc.code, tc.field)
		})
	}
	// A project that is not one.
	req := e.createReq("x", nil)
	req.Metadata.ParentId = "project-e00nonexistent"
	_, err := e.clusters.Create(ctx, req)
	wantFailure(t, "unknown project", err, codes.NotFound)

	e.newCluster(t, "dup", nil)
	_, err = e.clusters.Create(ctx, e.createReq("dup", nil))
	wantFailure(t, "duplicate name", err, codes.AlreadyExists)
	if l, err := e.clusters.List(ctx, &pgpb.ListClustersRequest{ParentId: e.project}); err != nil || len(l.GetClusters()) != 1 {
		t.Errorf("clusters after the rejections = %v, %v, want only dup", l, err)
	}
}

func TestMSPPostgresUpdateAndStateErrors(t *testing.T) {
	t.Parallel()
	e := startPG(t)
	ctx := t.Context()
	id := e.newCluster(t, "main", nil)
	cur := e.getCluster(t, id)

	mod := func(f func(*pgpb.ClusterSpec)) error {
		spec := proto.CloneOf(cur.GetSpec())
		f(spec)
		_, err := e.clusters.Update(ctx, &pgpb.UpdateClusterRequest{Metadata: cur.GetMetadata(), Spec: spec})
		return err
	}
	immutables := map[string]func(*pgpb.ClusterSpec){
		"spec.description":                func(s *pgpb.ClusterSpec) { s.Description = "new" },
		"spec.network_id":                 func(s *pgpb.ClusterSpec) { s.NetworkId = "vpcnetwork-e00other" },
		"spec.config.version":             func(s *pgpb.ClusterSpec) { s.Config.Version = "17" },
		"spec.bootstrap.user_name":        func(s *pgpb.ClusterSpec) { s.Bootstrap.UserName = "other" },
		"spec.bootstrap.db_name":          func(s *pgpb.ClusterSpec) { s.Bootstrap.DbName = "other" },
		"spec.backup.backup_window_start": func(s *pgpb.ClusterSpec) { s.Backup = &pgpb.BackupSpec{BackupWindowStart: "05:00:00"} },
		"spec.backup.retention_policy":    func(s *pgpb.ClusterSpec) { s.Backup = &pgpb.BackupSpec{RetentionPolicy: "9d"} },
	}
	for field, f := range immutables {
		wantFailure(t, "update "+field, mod(f), codes.InvalidArgument, field, "immutable")
	}
	wantFailure(t, "shrink", mod(func(s *pgpb.ClusterSpec) { s.Config.Template.Disk.SizeGibibytes = 10 }), codes.InvalidArgument, "cannot shrink")
	wantFailure(t, "hosts", mod(func(s *pgpb.ClusterSpec) { s.Config.Template.Hosts.Count = 99 }), codes.InvalidArgument, "hosts.count")
	_, err := e.clusters.Update(ctx, &pgpb.UpdateClusterRequest{Metadata: &commonpb.ResourceMetadata{Id: "postgresql-e00missing", Name: "x"}, Spec: cur.GetSpec()})
	wantFailure(t, "update missing", err, codes.NotFound)
	if got := e.getCluster(t, id); !proto.Equal(got, cur) {
		t.Error("a rejected update changed the cluster")
	}

	// State errors.
	_, err = e.clusters.Start(ctx, &pgpb.StartClusterRequest{Id: id})
	wantFailure(t, "start running", err, codes.FailedPrecondition, "main")
	alphaWait(t)(e.clusters.Stop(ctx, &pgpb.StopClusterRequest{Id: id}))
	_, err = e.clusters.Stop(ctx, &pgpb.StopClusterRequest{Id: id})
	wantFailure(t, "stop stopped", err, codes.FailedPrecondition, "main")
	_, err = e.clusters.Update(ctx, &pgpb.UpdateClusterRequest{Metadata: cur.GetMetadata(), Spec: cur.GetSpec()})
	wantFailure(t, "update stopped", err, codes.FailedPrecondition, "main", "stopped")
	_, err = e.backups.Create(ctx, &pgpb.CreateBackupRequest{ClusterId: id})
	wantFailure(t, "backup stopped", err, codes.FailedPrecondition, "main", "stopped")

	for name, call := range map[string]func() error{
		"get": func() error {
			_, err := e.clusters.Get(ctx, &pgpb.GetClusterRequest{Id: "postgresql-e00missing"})
			return err
		},
		"stop": func() error {
			_, err := e.clusters.Stop(ctx, &pgpb.StopClusterRequest{Id: "postgresql-e00missing"})
			return err
		},
		"start": func() error {
			_, err := e.clusters.Start(ctx, &pgpb.StartClusterRequest{Id: "postgresql-e00missing"})
			return err
		},
		"delete": func() error {
			_, err := e.clusters.Delete(ctx, &pgpb.DeleteClusterRequest{Id: "postgresql-e00missing"})
			return err
		},
		"by name": func() error {
			_, err := e.clusters.GetByName(ctx, &commonpb.GetByNameRequest{ParentId: e.project, Name: "nope"})
			return err
		},
	} {
		wantFailure(t, name+" of a missing cluster", call(), codes.NotFound)
	}
	_, err = e.clusters.Get(ctx, &pgpb.GetClusterRequest{})
	wantFailure(t, "get without id", err, codes.InvalidArgument)
	_, err = e.clusters.List(ctx, &pgpb.ListClustersRequest{ParentId: e.project, PageSize: 2000})
	wantFailure(t, "page size", err, codes.InvalidArgument, "page_size")
}
