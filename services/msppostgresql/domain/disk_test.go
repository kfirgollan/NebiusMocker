package domain_test

import (
	"context"
	"errors"
	"testing"

	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"
	mspv1alpha1pb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1"
	resourcepb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1/resource"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/services/msppostgresql/domain"
)

// CheckDisk is the fake backend's disk: usage is whatever the test sets, and
// the cluster is full once it reaches the limit.
func (b *backend) CheckDisk(_ context.Context, id string, limit int64) (domain.DiskUsage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.diskLimits = append(b.diskLimits, limit)
	if b.diskErr != nil {
		return domain.DiskUsage{}, b.diskErr
	}
	used := b.diskUsed[id]
	return domain.DiskUsage{UsedBytes: used, LimitBytes: limit, Full: used >= limit}, nil
}

func (b *backend) setUsed(id string, used int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.diskUsed == nil {
		b.diskUsed = map[string]int64{}
	}
	b.diskUsed[id] = used
}

func (b *backend) lastLimit() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.diskLimits[len(b.diskLimits)-1]
}

func stateOfCluster(t *testing.T, e *env, id string) mspv1alpha1pb.ClusterStatus_State {
	t.Helper()
	return e.get(t, id).GetStatus().GetState()
}

func TestDiskLimitUsesTheConfiguredUnit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(d *domain.Deps) {
		d.Config.MSP = config.Default().MSP
		d.Config.MSP.PostgresDiskUnit = 8 << 20
	})
	e.create(t, "main", nil) // 20 GiB
	if got, want := e.backend.lastLimit(), int64(20*8<<20); got != want {
		t.Errorf("limit = %d, want %d (20 GiB x 8 MiB)", got, want)
	}
	e2 := newEnv(t)
	e2.create(t, "main", nil)
	if got := e2.backend.lastLimit(); got != 20<<30 {
		t.Errorf("default limit = %d, want 20 GiB", got)
	}
}

func TestEnsureSetsDegradedWhenTheDiskIsFull(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.create(t, "main", nil)
	id := c.GetMetadata().GetId()
	if stateOfCluster(t, e, id) != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Fatalf("a new cluster is not FINISHED: %v", e.get(t, id).GetStatus())
	}

	// A start (or any operation ending in EnsureCluster) re-checks the disk.
	e.backend.setUsed(id, 20<<30)
	done(t)(e.svc.StopCluster(t.Context(), id, nil))
	done(t)(e.svc.StartCluster(t.Context(), id, nil))
	if st := e.get(t, id).GetStatus(); st.GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_RUNNING || st.GetState() != mspv1alpha1pb.ClusterStatus_STATE_DEGRADED {
		t.Fatalf("status = %v, want RUNNING/DEGRADED", st)
	}

	// Growing the disk through an update gets the cluster out of it.
	done(t)(e.svc.UpdateCluster(t.Context(), domain.UpdateClusterCmd{
		Metadata: e.get(t, id).GetMetadata(),
		Spec:     &pgpb.ClusterSpec{Config: &pgpb.ConfigSpec{Template: &pgpb.TemplateSpec{Disk: diskSpec(30)}}},
	}))
	if st := e.get(t, id).GetStatus(); st.GetState() != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Errorf("status after growing the disk = %v, want FINISHED", st)
	}
}

func TestDegradedClusterIsStillRunning(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	c := e.create(t, "main", nil)
	id := c.GetMetadata().GetId()
	e.backend.setUsed(id, 20<<30)
	if err := e.svc.CheckDisks(t.Context()); err != nil {
		t.Fatal(err)
	}
	if stateOfCluster(t, e, id) != mspv1alpha1pb.ClusterStatus_STATE_DEGRADED {
		t.Fatalf("status = %v", e.get(t, id).GetStatus())
	}
	// Backups, updates and stops treat it as running.
	before := len(e.backend.backups)
	done(t)(e.svc.CreateBackup(t.Context(), id, nil))
	if len(e.backend.backups) != before+1 {
		t.Errorf("backups taken = %v", e.backend.backups)
	}
	done(t)(e.svc.StopCluster(t.Context(), id, nil))
}

func TestCheckDisksTracksUsage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	a := e.create(t, "a", nil).GetMetadata().GetId()
	b := e.create(t, "b", nil).GetMetadata().GetId()
	stopped := e.create(t, "stopped", nil).GetMetadata().GetId()
	done(t)(e.svc.StopCluster(t.Context(), stopped, nil))
	e.backend.setUsed(a, 25<<30)
	e.backend.setUsed(stopped, 25<<30)
	if err := e.svc.CheckDisks(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := stateOfCluster(t, e, a); got != mspv1alpha1pb.ClusterStatus_STATE_DEGRADED {
		t.Errorf("a state = %v, want DEGRADED", got)
	}
	if got := stateOfCluster(t, e, b); got != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Errorf("b state = %v, want FINISHED", got)
	}
	if st := e.get(t, stopped).GetStatus(); st.GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_STOPPING || st.GetState() != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Errorf("a stopped cluster was touched: %v", st)
	}

	// Data deleted: the cluster recovers.
	e.backend.setUsed(a, 1<<30)
	if err := e.svc.CheckDisks(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := stateOfCluster(t, e, a); got != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Errorf("a state after freeing space = %v, want FINISHED", got)
	}
}

func TestCheckDisksNeverClobbersAnOperation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.create(t, "main", nil).GetMetadata().GetId()
	// An update is in progress: the stored state is UPDATING/IN_PROGRESS.
	c := e.get(t, id)
	c.Status.Phase, c.Status.State = mspv1alpha1pb.ClusterStatus_PHASE_UPDATING, mspv1alpha1pb.ClusterStatus_STATE_IN_PROGRESS
	if err := e.clusters.Update(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	e.backend.setUsed(id, 25<<30)
	if err := e.svc.CheckDisks(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st := e.get(t, id).GetStatus(); st.GetPhase() != mspv1alpha1pb.ClusterStatus_PHASE_UPDATING || st.GetState() != mspv1alpha1pb.ClusterStatus_STATE_IN_PROGRESS {
		t.Errorf("status = %v, the check overwrote an operation", st)
	}
}

func TestDiskCheckErrorsAreNotFatal(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.backend.diskErr = errors.New("docker is down")
	c := e.create(t, "main", nil) // the create operation succeeds all the same
	id := c.GetMetadata().GetId()
	if stateOfCluster(t, e, id) != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Errorf("status = %v", e.get(t, id).GetStatus())
	}
	if err := e.svc.CheckDisks(t.Context()); err == nil {
		t.Error("CheckDisks reported no error for a failing backend")
	}
	if stateOfCluster(t, e, id) != mspv1alpha1pb.ClusterStatus_STATE_FINISHED {
		t.Error("a failed check changed the state")
	}
}

func diskSpec(gib int64) *resourcepb.DiskSpec {
	return &resourcepb.DiskSpec{Type: "network-ssd", SizeGibibytes: gib}
}
