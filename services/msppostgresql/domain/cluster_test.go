package domain_test

import (
	"maps"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/nebius/gosdk/proto/fieldmask/mask"
	commonpb "github.com/nebius/gosdk/proto/nebius/common/v1"
	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"
	pgconfigpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1/config"
	mspv1alpha1pb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1"
	resourcepb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1/resource"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

// wantKind checks that err is a domain error of kind and mentions fragments.
func wantKind(t *testing.T, what string, err error, kind errs.Kind, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error, want %v", what, kind)
	}
	if got := errs.KindOf(err); got != kind {
		t.Fatalf("%s: kind = %v (%v), want %v", what, got, err, kind)
	}
	for _, f := range fragments {
		if !strings.Contains(err.Error(), f) {
			t.Errorf("%s: error %q does not mention %q", what, err, f)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestCreateClusterDefaults(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.create(t, "main", nil)
	st, spec := c.GetStatus(), c.GetSpec()
	if st.GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING || st.GetState() != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Fatalf("status = %v, want RUNNING/FINISHED", st)
	}
	if st.GetPresetDetails().GetCpuCount() != 2 || st.GetPresetDetails().GetMemoryGibibytes() != 8 || st.GetPresetDetails().GetGpuCount() != 0 {
		t.Errorf("preset_details = %v, want 2 vCPUs, 8 GiB, no GPU", st.GetPresetDetails())
	}
	id := c.GetMetadata().GetId()
	if got := st.GetConnectionEndpoints().GetPrivateReadWrite(); got != id+".rw.postgresql.mocker.localhost:5432" {
		t.Errorf("private_read_write = %q", got)
	}
	if st.GetConnectionEndpoints().GetPublicReadWrite() != "" {
		t.Errorf("public endpoint %q without public_access", st.GetConnectionEndpoints().GetPublicReadWrite())
	}
	if spec.GetNetworkId() != defaultNetwork || !e.vpc.isBound(defaultNetwork, id) {
		t.Errorf("network_id = %q bound=%v, want the default network, bound", spec.GetNetworkId(), e.vpc.isBound(defaultNetwork, id))
	}
	// Optional-not-computed fields are echoed as sent: no defaults filled in.
	if spec.GetConfig().GetPoolerConfig() != nil || spec.GetBackup() != nil || spec.GetConfig().GetPostgresqlConfig_16() != nil || spec.GetConfig().GetPublicAccess() {
		t.Errorf("spec = %v: optional fields were filled in", spec)
	}
	// ... but the defaults are used internally.
	bc := e.backend.last()
	if bc.Pooler.Mode != "session" || bc.Pooler.MaxPoolSize != 20 || bc.Hosts != 1 || bc.DiskGiB != 20 || bc.CPUs != 2 || bc.MemoryBytes != 8<<30 {
		t.Errorf("backend cluster = %+v", bc)
	}
	if bc.Password != "s3cret-pass" || bc.UserName != "app" || bc.DBName != "appdb" || bc.Restore != nil {
		t.Errorf("backend bootstrap = %+v", bc)
	}
	bk := e.backupsOf(t, id)
	if len(bk) != 1 || bk[0].Backup.GetOnDemand() || !bk[0].Done || bk[0].RetentionDays != 7 || !bk[0].Backup.GetSourceClusterVisible() {
		t.Fatalf("backups = %+v, want one finished automatic backup kept 7 days", bk)
	}
}

func TestPasswordIsNeverStoredOrReturned(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.create(t, "main", nil)
	id := c.GetMetadata().GetId()
	if c.GetSpec().GetBootstrap().GetUserPassword() != "" {
		t.Fatal("password stored on create")
	}
	if c.GetSpec().GetBootstrap().GetUserName() != "app" || c.GetSpec().GetBootstrap().GetDbName() != "appdb" {
		t.Errorf("bootstrap = %v", c.GetSpec().GetBootstrap())
	}
	// A password change goes to the backend only.
	spec := &pgpb.ClusterSpec{Bootstrap: &pgpb.BootstrapSpec{UserPassword: "n3w-pass-word"}}
	done(t)(e.svc.UpdateCluster(t.Context(), domain.UpdateClusterCmd{Metadata: c.GetMetadata(), Spec: spec}))
	if got := e.backend.last().Password; got != "n3w-pass-word" {
		t.Errorf("backend password after update = %q", got)
	}
	after := e.get(t, id)
	if after.GetSpec().GetBootstrap().GetUserPassword() != "" {
		t.Error("password stored on update")
	}
	if after.GetMetadata().GetResourceVersion() != c.GetMetadata().GetResourceVersion()+1 {
		t.Errorf("resource_version = %d, want a bump for the password change", after.GetMetadata().GetResourceVersion())
	}
	// An update without a password keeps the current one: the backend gets "".
	done(t)(e.svc.UpdateCluster(t.Context(), domain.UpdateClusterCmd{
		Metadata: after.GetMetadata(), Spec: &pgpb.ClusterSpec{Config: &pgpb.ConfigSpec{PublicAccess: true}},
	}))
	if got := e.backend.last().Password; got != "" {
		t.Errorf("backend password of a plain update = %q, want empty", got)
	}
}

func TestCreateClusterRejections(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		mod   func(*pgpb.ClusterSpec)
		kind  errs.Kind
		field string
	}{
		{"version", func(s *pgpb.ClusterSpec) { s.Config.Version = "17" }, errs.KindBadRequest, "spec.config.version"},
		{"platform", func(s *pgpb.ClusterSpec) { s.Config.Template.Resources.Platform = "gpu-h100" }, errs.KindBadRequest, "resources.platform"},
		{"preset", func(s *pgpb.ClusterSpec) { s.Config.Template.Resources.Preset = "3vcpu-3gb" }, errs.KindBadRequest, "resources.preset"},
		{"disk type", func(s *pgpb.ClusterSpec) { s.Config.Template.Disk.Type = "hdd" }, errs.KindBadRequest, "disk.type"},
		{"disk too small", func(s *pgpb.ClusterSpec) { s.Config.Template.Disk.SizeGibibytes = 9 }, errs.KindBadRequest, "disk.size_gibibytes"},
		{"disk too big", func(s *pgpb.ClusterSpec) { s.Config.Template.Disk.SizeGibibytes = 4097 }, errs.KindBadRequest, "disk.size_gibibytes"},
		{"no hosts", func(s *pgpb.ClusterSpec) { s.Config.Template.Hosts.Count = 0 }, errs.KindBadRequest, "hosts.count"},
		{"too many hosts", func(s *pgpb.ClusterSpec) { s.Config.Template.Hosts.Count = 4 }, errs.KindBadRequest, "hosts.count"},
		{"password", func(s *pgpb.ClusterSpec) { s.Bootstrap.UserPassword = "" }, errs.KindBadRequest, "spec.bootstrap.user_password is required"},
		{"window", func(s *pgpb.ClusterSpec) { s.Backup = &pgpb.BackupSpec{BackupWindowStart: "25:00:00"} }, errs.KindBadRequest, "backup_window_start"},
		{"window format", func(s *pgpb.ClusterSpec) { s.Backup = &pgpb.BackupSpec{BackupWindowStart: "03:00"} }, errs.KindBadRequest, "HH:MM:SS"},
		{"retention low", func(s *pgpb.ClusterSpec) { s.Backup = &pgpb.BackupSpec{RetentionPolicy: "6d"} }, errs.KindBadRequest, "retention_policy"},
		{"retention high", func(s *pgpb.ClusterSpec) { s.Backup = &pgpb.BackupSpec{RetentionPolicy: "31d"} }, errs.KindBadRequest, "retention_policy"},
		{"retention unit", func(s *pgpb.ClusterSpec) { s.Backup = &pgpb.BackupSpec{RetentionPolicy: "10"} }, errs.KindBadRequest, "retention_policy"},
		{"replication regions", func(s *pgpb.ClusterSpec) { s.Backup = &pgpb.BackupSpec{ReplicationRegions: []string{"eu-west1"}} }, errs.KindBadRequest, "cross-region backup replication is not emulated"},
		{"replication interval", func(s *pgpb.ClusterSpec) { s.Backup = &pgpb.BackupSpec{ReplicationInterval: durationpb.New(time.Hour)} }, errs.KindBadRequest, "replication_interval"},
		{"search_path", func(s *pgpb.ClusterSpec) {
			s.Config.Config = &pgpb.ConfigSpec_PostgresqlConfig_16{PostgresqlConfig_16: &pgconfigpb.PostgresqlConfig16{SearchPath: ptr("a\nb")}}
		}, errs.KindBadRequest, "search_path"},
		{"unknown network", func(s *pgpb.ClusterSpec) { s.NetworkId = "vpcnetwork-e00missing" }, errs.KindNotFound, "vpcnetwork-e00missing"},
		{"foreign network", func(s *pgpb.ClusterSpec) { s.NetworkId = foreignNetwork }, errs.KindBadRequest, "spec.network_id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			spec := validSpec()
			tc.mod(spec)
			_, err := e.svc.CreateCluster(t.Context(), domain.CreateClusterCmd{
				Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "bad"}, Spec: spec,
			})
			wantKind(t, tc.name, err, tc.kind, tc.field)
			if p, _ := e.svc.ListClusters(t.Context(), projectID, 0, ""); len(p.Items) != 0 {
				t.Errorf("a rejected create left %d clusters", len(p.Items))
			}
		})
	}
}

func TestCreateClusterRequestRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	create := func(md *commonpb.ResourceMetadata) error {
		_, err := e.svc.CreateCluster(t.Context(), domain.CreateClusterCmd{Metadata: md, Spec: validSpec()})
		return err
	}
	wantKind(t, "id set", create(&commonpb.ResourceMetadata{Id: "x", ParentId: projectID, Name: "a"}), errs.KindBadRequest, "metadata.id")
	wantKind(t, "not a project", create(&commonpb.ResourceMetadata{ParentId: "tenant-e00x", Name: "a"}), errs.KindBadRequest, "metadata.parent_id")
	wantKind(t, "unknown project", create(&commonpb.ResourceMetadata{ParentId: "project-e00missing", Name: "a"}), errs.KindNotFound)
	e.create(t, "dup", nil)
	wantKind(t, "duplicate name", create(&commonpb.ResourceMetadata{ParentId: projectID, Name: "dup"}), errs.KindAlreadyExists)
	// The same name in another project is fine, and so is an explicit network.
	other := e.create(t, "dup2", func(s *pgpb.ClusterSpec) { s.NetworkId = otherNetwork })
	if other.GetSpec().GetNetworkId() != otherNetwork {
		t.Errorf("network_id = %q, want %q", other.GetSpec().GetNetworkId(), otherNetwork)
	}
	if !e.vpc.isBound(otherNetwork, other.GetMetadata().GetId()) {
		t.Errorf("the cluster is not bound to %s", otherNetwork)
	}
}

func TestCreateWithoutVPCIsRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *domain.Deps) { d.Peers = fakePeers{} })
	_, err := e.svc.CreateCluster(t.Context(), domain.CreateClusterCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "a"}, Spec: validSpec(),
	})
	wantKind(t, "no vpc", err, errs.KindBadRequest, "spec.network_id", "vpc")
}

func TestPostgresParameters(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	cfg := &pgconfigpb.PostgresqlConfig16{
		AutovacuumWorkMem: ptr(int64(-1)), StatementTimeout: ptr(int64(30000)), IdleInTransactionSessionTimeout: ptr(int64(0)),
		AutovacuumVacuumCostDelay: ptr(int64(2)), AutovacuumVacuumCostLimit: ptr(int64(-1)), AutovacuumNaptime: ptr(int64(60)),
		AutovacuumVacuumScaleFactor: ptr(0.2), AutovacuumAnalyzeScaleFactor: ptr(0.05), DefaultTransactionReadOnly: ptr(true),
		SearchPath: ptr(`"$user", public`), MaxConnections: ptr(int64(200)), SharedBuffers: ptr(int64(262144)),
	}
	c := e.create(t, "tuned", func(s *pgpb.ClusterSpec) {
		s.Config.Config = &pgpb.ConfigSpec_PostgresqlConfig_16{PostgresqlConfig_16: cfg}
		s.Config.PoolerConfig = &pgpb.ConnectionPoolerConfig{PoolingMode: pgpb.ConnectionPoolerConfig_TRANSACTION, MaxPoolSize: ptr(int64(100))}
		s.Config.PublicAccess = true
	})
	want := map[string]string{
		"autovacuum_work_mem": "-1", "statement_timeout": "30000ms", "idle_in_transaction_session_timeout": "0ms",
		"autovacuum_vacuum_cost_delay": "2ms", "autovacuum_vacuum_cost_limit": "-1", "autovacuum_naptime": "60s",
		"autovacuum_vacuum_scale_factor": "0.2", "autovacuum_analyze_scale_factor": "0.05", "default_transaction_read_only": "on",
		"search_path": `"$user", public`, "max_connections": "200", "shared_buffers": "262144kB",
	}
	bc := e.backend.last()
	if !maps.Equal(bc.Parameters, want) {
		t.Errorf("parameters = %v, want %v", bc.Parameters, want)
	}
	if bc.Pooler.Mode != "transaction" || bc.Pooler.MaxPoolSize != 100 || !bc.PublicAccess {
		t.Errorf("pooler/public = %+v", bc)
	}
	if !proto.Equal(c.GetSpec().GetConfig().GetPostgresqlConfig_16(), cfg) {
		t.Errorf("stored config = %v, want it echoed as sent", c.GetSpec().GetConfig().GetPostgresqlConfig_16())
	}
	if got := c.GetStatus().GetConnectionEndpoints(); !strings.HasSuffix(got.GetPublicReadWrite(), ".public-rw.postgresql.mocker.localhost:5432") ||
		!strings.Contains(got.GetPublicReadOnly(), ".public-ro.") {
		t.Errorf("public endpoints = %v", got)
	}
}

func TestBackendFailureLeavesTheClusterInError(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.backend.ensureErr = errs.Internal("no capacity")
	op, err := e.svc.CreateCluster(t.Context(), domain.CreateClusterCmd{
		Metadata: &commonpb.ResourceMetadata{ParentId: projectID, Name: "sad"}, Spec: validSpec(),
	})
	if err != nil {
		t.Fatalf("CreateCluster: %v", err)
	}
	wantKind(t, "operation", op.Err, errs.KindInternal, "no capacity")
	c := e.get(t, op.ResourceID)
	if c.GetStatus().GetState() != mspv1alpha1pb.ClusterStatus_STATE_ERROR || c.GetStatus().GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_PROVISIONING {
		t.Errorf("status = %v, want PROVISIONING/ERROR", c.GetStatus())
	}
	if len(e.backupsOf(t, op.ResourceID)) != 0 {
		t.Error("a failed cluster got a backup")
	}
	// A retry through Update reconciles it.
	e.backend.ensureErr = nil
	done(t)(e.svc.UpdateCluster(t.Context(), domain.UpdateClusterCmd{
		Metadata: c.GetMetadata(), Spec: &pgpb.ClusterSpec{Config: &pgpb.ConfigSpec{PublicAccess: true}},
	}))
	if got := e.get(t, op.ResourceID).GetStatus(); got.GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING || got.GetState() != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Errorf("status after retry = %v", got)
	}
}

func TestUpdateClusterResources(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.create(t, "main", nil)
	id := c.GetMetadata().GetId()
	calls := e.backend.ensureCalls()
	done(t)(e.svc.UpdateCluster(t.Context(), domain.UpdateClusterCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, Labels: map[string]string{"env": "dev"}},
		Spec: &pgpb.ClusterSpec{Config: &pgpb.ConfigSpec{Template: &pgpb.TemplateSpec{
			Resources: &resourcepb.ResourcesSpec{Platform: "cpu-e2", Preset: "4vcpu-16gb"},
			Hosts:     &resourcepb.HostSpec{Count: 3},
			Disk:      &resourcepb.DiskSpec{Type: "network-ssd", SizeGibibytes: 50},
		}}},
	}))
	got := e.get(t, id)
	tpl := got.GetSpec().GetConfig().GetTemplate()
	if tpl.GetResources().GetPreset() != "4vcpu-16gb" || tpl.GetHosts().GetCount() != 3 || tpl.GetDisk().GetSizeGibibytes() != 50 {
		t.Errorf("template = %v", tpl)
	}
	if got.GetStatus().GetPresetDetails().GetCpuCount() != 4 || got.GetStatus().GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING {
		t.Errorf("status = %v, want RUNNING with 4 vCPUs", got.GetStatus())
	}
	if got.GetMetadata().GetLabels()["env"] != "dev" || got.GetMetadata().GetName() != "main" {
		t.Errorf("metadata = %v, want the label added and the name kept", got.GetMetadata())
	}
	bc := e.backend.last()
	if e.backend.ensureCalls() != calls+1 || bc.Hosts != 3 || bc.CPUs != 4 || bc.DiskGiB != 50 {
		t.Errorf("backend calls = %d, last = %+v", e.backend.ensureCalls(), bc)
	}
	if got.GetMetadata().GetResourceVersion() != 2 || !got.GetMetadata().GetUpdatedAt().AsTime().Equal(t0) {
		t.Errorf("metadata = %v", got.GetMetadata())
	}

	// Labels only: no backend call, the phase does not move.
	e.clk.Advance(time.Minute)
	done(t)(e.svc.UpdateCluster(t.Context(), domain.UpdateClusterCmd{
		Metadata: &commonpb.ResourceMetadata{Id: id, Labels: map[string]string{"env": "prod"}},
	}))
	if e.backend.ensureCalls() != calls+1 {
		t.Error("a label change reconciled the backend")
	}
	if l := e.get(t, id); l.GetMetadata().GetLabels()["env"] != "prod" || l.GetMetadata().GetResourceVersion() != 3 || l.GetStatus().GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING {
		t.Errorf("after label update: %v", l)
	}
	// A no-op update changes nothing.
	same := e.get(t, id)
	done(t)(e.svc.UpdateCluster(t.Context(), domain.UpdateClusterCmd{Metadata: same.GetMetadata()}))
	if !proto.Equal(e.get(t, id), same) {
		t.Error("a no-op update changed the cluster")
	}
}

func TestUpdateClusterRejections(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		spec  *pgpb.ClusterSpec
		kind  errs.Kind
		field string
	}{
		{"description", &pgpb.ClusterSpec{Description: "changed"}, errs.KindBadRequest, "spec.description"},
		{"network", &pgpb.ClusterSpec{NetworkId: otherNetwork}, errs.KindBadRequest, "spec.network_id"},
		{"version", &pgpb.ClusterSpec{Config: &pgpb.ConfigSpec{Version: "17"}}, errs.KindBadRequest, "spec.config.version"},
		{"user name", &pgpb.ClusterSpec{Bootstrap: &pgpb.BootstrapSpec{UserName: "other"}}, errs.KindBadRequest, "spec.bootstrap.user_name"},
		{"db name", &pgpb.ClusterSpec{Bootstrap: &pgpb.BootstrapSpec{DbName: "other"}}, errs.KindBadRequest, "spec.bootstrap.db_name"},
		{"backup window", &pgpb.ClusterSpec{Backup: &pgpb.BackupSpec{BackupWindowStart: "03:00:00"}}, errs.KindBadRequest, "backup_window_start"},
		{"retention", &pgpb.ClusterSpec{Backup: &pgpb.BackupSpec{RetentionPolicy: "10d"}}, errs.KindBadRequest, "retention_policy"},
		{"disk shrinks", &pgpb.ClusterSpec{Config: &pgpb.ConfigSpec{Template: &pgpb.TemplateSpec{
			Disk: &resourcepb.DiskSpec{Type: "network-ssd", SizeGibibytes: 10},
		}}}, errs.KindBadRequest, "cannot shrink"},
		{"too many hosts", &pgpb.ClusterSpec{Config: &pgpb.ConfigSpec{Template: &pgpb.TemplateSpec{
			Hosts: &resourcepb.HostSpec{Count: 9},
		}}}, errs.KindBadRequest, "hosts.count"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			c := e.create(t, "main", func(s *pgpb.ClusterSpec) { s.Description = "orig" })
			before := e.get(t, c.GetMetadata().GetId())
			_, err := e.svc.UpdateCluster(t.Context(), domain.UpdateClusterCmd{Metadata: c.GetMetadata(), Spec: tc.spec})
			wantKind(t, tc.name, err, tc.kind, tc.field)
			if tc.kind == errs.KindBadRequest && tc.field == "spec.description" {
				if !strings.Contains(err.Error(), "immutable") {
					t.Errorf("error %q does not say immutable", err)
				}
			}
			if !proto.Equal(e.get(t, c.GetMetadata().GetId()), before) {
				t.Error("a rejected update changed the cluster")
			}
		})
	}
}

func TestUpdateClusterResetMask(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.create(t, "main", func(s *pgpb.ClusterSpec) {
		s.Config.PoolerConfig = &pgpb.ConnectionPoolerConfig{PoolingMode: pgpb.ConnectionPoolerConfig_TRANSACTION}
		s.Config.PublicAccess = true
	})
	id := c.GetMetadata().GetId()
	// Without a mask an empty value does not clear anything (patch).
	done(t)(e.svc.UpdateCluster(t.Context(), domain.UpdateClusterCmd{Metadata: c.GetMetadata(), Spec: &pgpb.ClusterSpec{}}))
	if e.get(t, id).GetSpec().GetConfig().GetPoolerConfig() == nil {
		t.Fatal("pooler_config cleared without a reset mask")
	}
	// The reset mask clears exactly what it names.
	done(t)(e.svc.UpdateCluster(t.Context(), domain.UpdateClusterCmd{
		Metadata: c.GetMetadata(), Spec: &pgpb.ClusterSpec{Config: &pgpb.ConfigSpec{}}, ResetMask: mask.ParseMust("spec.config.pooler_config"),
	}))
	got := e.get(t, id)
	if got.GetSpec().GetConfig().GetPoolerConfig() != nil || !got.GetSpec().GetConfig().GetPublicAccess() {
		t.Errorf("config = %v, want the pooler cleared and public_access kept", got.GetSpec().GetConfig())
	}
	if bc := e.backend.last(); bc.Pooler.Mode != "session" {
		t.Errorf("pooler after reset = %+v, want the internal default", bc.Pooler)
	}
	// Resetting an immutable field is an immutable change.
	_, err := e.svc.UpdateCluster(t.Context(), domain.UpdateClusterCmd{
		Metadata: got.GetMetadata(), Spec: &pgpb.ClusterSpec{Bootstrap: &pgpb.BootstrapSpec{}}, ResetMask: mask.ParseMust("spec.bootstrap.user_name"),
	})
	wantKind(t, "reset user_name", err, errs.KindBadRequest, "user_name")
}

func TestUpdateClusterConcurrencyAndState(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.create(t, "main", nil)
	stale := proto.CloneOf(c.GetMetadata())
	stale.ResourceVersion = 99
	_, err := e.svc.UpdateCluster(t.Context(), domain.UpdateClusterCmd{Metadata: stale, Spec: &pgpb.ClusterSpec{}})
	wantKind(t, "stale version", err, errs.KindConflict)
	_, err = e.svc.UpdateCluster(t.Context(), domain.UpdateClusterCmd{Metadata: &commonpb.ResourceMetadata{Id: c.GetMetadata().GetId(), ParentId: otherProjectID}})
	wantKind(t, "parent change", err, errs.KindBadRequest, "metadata.parent_id")
	_, err = e.svc.UpdateCluster(t.Context(), domain.UpdateClusterCmd{Metadata: &commonpb.ResourceMetadata{Id: "postgresql-e00missing"}})
	wantKind(t, "missing", err, errs.KindNotFound)
}

func TestStopStartAndStates(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.create(t, "main", nil)
	id := c.GetMetadata().GetId()
	wantKind(t, "start running", errOf(e.svc.StartCluster(t.Context(), id, nil)), errs.KindBadState, "main")

	done(t)(e.svc.StopCluster(t.Context(), id, nil))
	st := e.get(t, id).GetStatus()
	if st.GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_STOPPING || st.GetState() != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Fatalf("status after stop = %v, want STOPPING/FINISHED", st)
	}
	if len(e.backend.stopped) != 1 || e.backend.stopped[0] != id {
		t.Errorf("backend stops = %v", e.backend.stopped)
	}
	wantKind(t, "stop stopped", errOf(e.svc.StopCluster(t.Context(), id, nil)), errs.KindBadState, "main")
	wantKind(t, "update stopped", errOf(e.svc.UpdateCluster(t.Context(), domain.UpdateClusterCmd{
		Metadata: c.GetMetadata(), Spec: &pgpb.ClusterSpec{Config: &pgpb.ConfigSpec{PublicAccess: true}},
	})), errs.KindBadState, "main", "stopped")
	wantKind(t, "backup stopped", errOf(e.svc.CreateBackup(t.Context(), id, nil)), errs.KindBadState, "main", "stopped")

	done(t)(e.svc.StartCluster(t.Context(), id, nil))
	st = e.get(t, id).GetStatus()
	if st.GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING || st.GetState() != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Fatalf("status after start = %v, want RUNNING/FINISHED", st)
	}
	if bc := e.backend.last(); bc.Password != "" {
		t.Errorf("start passed password %q", bc.Password)
	}
	wantKind(t, "stop missing", errOf(e.svc.StopCluster(t.Context(), "postgresql-e00missing", nil)), errs.KindNotFound)
}

func TestDeleteClusterKeepsBackups(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.create(t, "main", nil)
	id := c.GetMetadata().GetId()
	done(t)(e.svc.CreateBackup(t.Context(), id, nil))
	done(t)(e.svc.DeleteCluster(t.Context(), id, nil))

	if _, err := e.svc.GetCluster(t.Context(), id); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("GetCluster after delete: %v, want NotFound", err)
	}
	if e.vpc.isBound(defaultNetwork, id) {
		t.Error("the network binding was not released")
	}
	if len(e.backend.deleted) != 1 {
		t.Errorf("backend deletes = %v", e.backend.deleted)
	}
	bk := e.backupsOf(t, id)
	if len(bk) != 2 {
		t.Fatalf("backups = %d, want both kept", len(bk))
	}
	for _, b := range bk {
		if b.Backup.GetSourceClusterVisible() {
			t.Errorf("backup %s still shows its cluster as visible", b.Backup.GetId())
		}
	}
	// The cluster as it was is still available.
	snap, err := e.svc.GetClusterForBackup(t.Context(), id, bk[0].Backup.GetId())
	if err != nil || snap.GetMetadata().GetName() != "main" || snap.GetSpec().GetBootstrap().GetUserPassword() != "" {
		t.Errorf("GetClusterForBackup = %v, %v", snap, err)
	}
	// The name can be reused.
	e.create(t, "main", nil)
	wantKind(t, "delete twice", errOf(e.svc.DeleteCluster(t.Context(), id, nil)), errs.KindNotFound)
}

func TestDeleteFailureKeepsTheCluster(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.create(t, "main", nil)
	id := c.GetMetadata().GetId()
	e.backend.deleteErr = errs.Internal("docker is down")
	op, err := e.svc.DeleteCluster(t.Context(), id, nil)
	if err != nil {
		t.Fatalf("DeleteCluster: %v", err)
	}
	wantKind(t, "operation", op.Err, errs.KindInternal, "docker is down")
	st := e.get(t, id).GetStatus()
	if st.GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_DELETING || st.GetState() != mspv1alpha1pb.ClusterStatus_STATE_ERROR {
		t.Errorf("status = %v, want DELETING/ERROR", st)
	}
	if !e.vpc.isBound(defaultNetwork, id) {
		t.Error("the network was released although the cluster is still there")
	}
	e.backend.deleteErr = nil
	done(t)(e.svc.DeleteCluster(t.Context(), id, nil))
	if _, err := e.svc.GetCluster(t.Context(), id); !errs.IsKind(err, errs.KindNotFound) {
		t.Errorf("GetCluster after the retry: %v", err)
	}
}

func errOf(_ any, err error) error { return err }
