package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// osUser runs commands as postgres (uid 70), which owns the data directory
// and may connect over the local socket (trust) as any role.
const osUser = "postgres"

// logTail is how many log lines a failure message carries.
const logTail = 25

// run executes cmd in container id as osUser and returns its trimmed stdout.
// A non-zero exit is an error carrying stderr.
func (b *Backend) run(ctx context.Context, id string, cmd ...string) (string, error) {
	res, err := b.docker.Exec(ctx, id, dockerhost.ExecSpec{Cmd: cmd, User: osUser})
	if err != nil {
		return "", fmt.Errorf("exec %s: %w", cmd[0], err)
	}
	if res.ExitCode != 0 {
		return strings.TrimSpace(res.Stdout), fmt.Errorf("%s exited %d: %s", cmd[0], res.ExitCode, strings.TrimSpace(res.Stderr+res.Stdout))
	}
	return strings.TrimSpace(res.Stdout), nil
}

// sh runs script with args ($1…) in container id.
func (b *Backend) sh(ctx context.Context, id, script string, args ...string) (string, error) {
	return b.run(ctx, id, append([]string{"sh", "-c", script, "nmock"}, args...)...)
}

// psql runs sql in the "postgres" database of the container over the local
// socket as role user and returns the unaligned, tuple-only output.
func (b *Backend) psql(ctx context.Context, id, user, sql string) (string, error) {
	return b.run(ctx, id, "psql", "-X", "-q", "-tA", "-v", "ON_ERROR_STOP=1", "-U", user, "-d", "postgres", "-c", sql)
}

// readiness is what waitPostgres waits for.
type readiness int

const (
	// accepting: connections work (a replica is in recovery by design).
	accepting readiness = iota
	// promoted: the end of recovery as well, for a restored primary.
	promoted
)

// waitPostgres polls until the host's PostgreSQL accepts connections over TCP
// (not just the init-time socket-only server of the image's entrypoint) and a
// query as user works. With promoted it also waits for the end of recovery
// (a restored primary promotes once it reached its target).
func (b *Backend) waitPostgres(ctx context.Context, id, name, user string, want readiness) error {
	deadline := b.clk.Now().Add(b.cfg.StartTimeout)
	var last error
	for {
		ctr, err := b.docker.InspectContainer(ctx, id)
		if err != nil {
			return internal("inspect "+name, err)
		}
		if !ctr.Running {
			return b.startFailure(ctx, id, name, fmt.Sprintf("exited (code %d)", ctr.ExitCode))
		}
		last = b.probe(ctx, id, user, want)
		if last == nil {
			return nil
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if !b.clk.Now().Before(deadline) {
			return b.startFailure(ctx, id, name, fmt.Sprintf("not ready within %s (NEBIUS_MOCKER_MSP_START_TIMEOUT): %v", b.cfg.StartTimeout, last))
		}
		if err := b.sleep(ctx, b.poll); err != nil {
			return err
		}
	}
}

func (b *Backend) probe(ctx context.Context, id, user string, want readiness) error {
	if _, err := b.run(ctx, id, "pg_isready", "-q", "-h", "127.0.0.1", "-p", "5432"); err != nil {
		return err
	}
	out, err := b.psql(ctx, id, user, "select pg_is_in_recovery()")
	if err != nil {
		return err
	}
	if want == promoted && out != "f" {
		return errors.New("still in recovery")
	}
	return nil
}

// waitPooler polls until the pooler accepts connections.
func (b *Backend) waitPooler(ctx context.Context, id, name string) error {
	deadline := b.clk.Now().Add(b.cfg.StartTimeout)
	for {
		ctr, err := b.docker.InspectContainer(ctx, id)
		if err != nil {
			return internal("inspect "+name, err)
		}
		if !ctr.Running {
			return b.startFailure(ctx, id, name, fmt.Sprintf("exited (code %d)", ctr.ExitCode))
		}
		_, last := b.run(ctx, id, "pg_isready", "-q", "-h", "127.0.0.1", "-p", "5432")
		if last == nil {
			return nil
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if !b.clk.Now().Before(deadline) {
			return b.startFailure(ctx, id, name, fmt.Sprintf("not ready within %s (NEBIUS_MOCKER_MSP_START_TIMEOUT): %v", b.cfg.StartTimeout, last))
		}
		if err := b.sleep(ctx, b.poll); err != nil {
			return err
		}
	}
}

// startFailure is the error of a container that did not come up, with the
// last lines of its log.
func (b *Backend) startFailure(ctx context.Context, id, name, why string) error {
	logs, err := b.docker.Logs(context.WithoutCancel(ctx), id, logTail)
	if err != nil {
		logs = fmt.Sprintf("(no logs: %v)", err)
	}
	return errs.Internal(fmt.Sprintf("container %s %s; last log lines:\n%s", name, why, strings.TrimSpace(logs)))
}

// runHelper runs a one-shot container from image (entrypoint: sh -c script,
// as root) with binds and files, waits for it to exit and removes it. A
// non-zero exit is an error carrying the container's log.
func (b *Backend) runHelper(ctx context.Context, image, script string, binds []string, files []dockerhost.File) error {
	labels := b.baseLabels()
	labels[LabelRole] = roleHelper
	name := "nmock-pg-helper-" + b.mac(fmt.Sprint(b.clk.Now().UnixNano(), script))[:12]
	id, err := b.docker.CreateContainer(ctx, dockerhost.ContainerSpec{
		Name: name, Image: image, Labels: labels, Binds: binds, Files: files,
		Entrypoint: []string{"sh", "-c", script, "nmock-helper"}, User: "root",
	})
	if err != nil {
		return internal("create helper container", err)
	}
	defer func() { _ = b.docker.RemoveContainer(context.WithoutCancel(ctx), id) }() // best-effort: a leftover is reaped by label
	if err := b.docker.StartContainer(ctx, id); err != nil {
		return internal("start helper container", err)
	}
	return b.awaitExit(ctx, id, name)
}

// awaitExit polls until the container stopped and checks its exit code.
func (b *Backend) awaitExit(ctx context.Context, id, name string) error {
	deadline := b.clk.Now().Add(b.cfg.StartTimeout)
	for {
		ctr, err := b.docker.InspectContainer(ctx, id)
		if err != nil {
			return internal("inspect "+name, err)
		}
		if !ctr.Running && ctr.Status != "created" {
			if ctr.ExitCode == 0 {
				return nil
			}
			return b.startFailure(ctx, id, name, fmt.Sprintf("failed (exit code %d)", ctr.ExitCode))
		}
		if !b.clk.Now().Before(deadline) {
			return errs.Internal(fmt.Sprintf("container %s did not finish within %s", name, b.cfg.StartTimeout))
		}
		if err := b.sleep(ctx, b.poll); err != nil {
			return err
		}
	}
}

// pollTimeout bounds waits that are best-effort.
const pollTimeout = 30 * time.Second
