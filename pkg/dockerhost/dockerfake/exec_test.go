package dockerfake_test

import (
	"errors"
	"testing"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost/dockerfake"
)

func TestExecAndStop(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := dockerfake.New()
	id, _ := f.CreateContainer(ctx, dockerhost.ContainerSpec{Name: "pg"})

	if _, err := f.Exec(ctx, id, dockerhost.ExecSpec{Cmd: []string{"true"}}); !errors.Is(err, dockerhost.ErrConflict) {
		t.Fatalf("Exec on a created container err = %v, want ErrConflict", err)
	}
	if err := f.StartContainer(ctx, id); err != nil {
		t.Fatal(err)
	}
	res, err := f.Exec(ctx, "pg", dockerhost.ExecSpec{Cmd: []string{"true"}})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("default Exec = %+v, %v", res, err)
	}
	f.SetExecHandler(func(cid string, e dockerhost.ExecSpec) dockerhost.ExecResult {
		return dockerhost.ExecResult{ExitCode: 2, Stdout: cid + ":" + e.Cmd[0]}
	})
	res, _ = f.Exec(ctx, id, dockerhost.ExecSpec{Cmd: []string{"psql"}})
	if res.ExitCode != 2 || res.Stdout != id+":psql" {
		t.Errorf("handler Exec = %+v", res)
	}
	calls := f.Execs()
	if len(calls) != 2 || calls[1].Container != "pg" || calls[1].Spec.Cmd[0] != "psql" {
		t.Errorf("Execs = %+v", calls)
	}

	if err := f.StopContainer(ctx, id, time.Second); err != nil {
		t.Fatal(err)
	}
	if c, _ := f.InspectContainer(ctx, id); c.Running || c.Status != "exited" {
		t.Errorf("after stop: %+v", c)
	}
	if err := f.StopContainer(ctx, "none", time.Second); !errors.Is(err, dockerhost.ErrNotFound) {
		t.Errorf("StopContainer(none) = %v, want ErrNotFound", err)
	}
}

func TestAutoExitAndCreated(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := dockerfake.New()
	idJob, _ := f.CreateContainer(ctx, dockerhost.ContainerSpec{Name: "job-1", Entrypoint: []string{"sh", "-c", "x"}})
	idDB, _ := f.CreateContainer(ctx, dockerhost.ContainerSpec{Name: "db"})
	f.SetAutoExit("job-", 3, "boom\n")
	_ = f.StartContainer(ctx, idJob)
	_ = f.StartContainer(ctx, idDB)

	job, _ := f.InspectContainer(ctx, idJob)
	if job.Running || job.ExitCode != 3 || job.Status != "exited" {
		t.Errorf("job after start: %+v", job)
	}
	if logs, _ := f.Logs(ctx, idJob, 0); logs != "boom\n" {
		t.Errorf("job logs = %q", logs)
	}
	if db, _ := f.InspectContainer(ctx, idDB); !db.Running {
		t.Errorf("db after start: %+v", db)
	}
	_ = f.RemoveContainer(ctx, idJob)
	if specs := f.Created(); len(specs) != 2 || specs[0].Name != "job-1" || specs[0].Entrypoint[0] != "sh" {
		t.Errorf("Created = %+v, want both specs, removed ones included", specs)
	}
}

func TestBuildAndImageExists(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := dockerfake.New()
	if ok, _ := f.ImageExists(ctx, "x:1"); ok {
		t.Error("ImageExists before build = true")
	}
	if err := f.BuildImage(ctx, "x:1", "FROM scratch", nil); err != nil {
		t.Fatal(err)
	}
	if ok, _ := f.ImageExists(ctx, "x:1"); !ok {
		t.Error("ImageExists after build = false")
	}
	if got := f.Images(); len(got) != 1 || got[0] != "x:1" {
		t.Errorf("Images = %v", got)
	}
}
