package dockerfake

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/dockerhost"
)

// ExecCall is one recorded Exec.
type ExecCall struct {
	ContainerID string
	Container   string // the container's name
	Spec        dockerhost.ExecSpec
}

// SetExecHandler sets what Exec returns; the default is exit code 0 and no
// output. The handler runs without the fake's lock held.
func (f *Fake) SetExecHandler(h func(id string, e dockerhost.ExecSpec) dockerhost.ExecResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execHandler = h
}

// Execs returns every Exec made so far, oldest first.
func (f *Fake) Execs() []ExecCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.execs)
}

type autoExit struct {
	code int
	logs string
}

// SetAutoExit makes containers whose name starts with prefix exit with code
// (and logs) as soon as they are started, like a one-shot helper container.
func (f *Fake) SetAutoExit(prefix string, code int, logs string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.autoExit == nil {
		f.autoExit = map[string]autoExit{}
	}
	f.autoExit[prefix] = autoExit{code: code, logs: logs}
}

// Created returns the spec of every container created so far, oldest first
// (removed ones included), for asserting what a consumer asked Docker for.
func (f *Fake) Created() []dockerhost.ContainerSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.specs)
}

// Exec implements the client method. Like the daemon, it refuses a container
// that is not running.
func (f *Fake) Exec(_ context.Context, id string, e dockerhost.ExecSpec) (dockerhost.ExecResult, error) {
	f.mu.Lock()
	if err := f.popFail("Exec"); err != nil {
		f.mu.Unlock()
		return dockerhost.ExecResult{}, err
	}
	c, err := f.find(id)
	if err != nil {
		f.mu.Unlock()
		return dockerhost.ExecResult{}, err
	}
	if !c.c.Running {
		f.mu.Unlock()
		return dockerhost.ExecResult{}, fmt.Errorf("%w: container %s is not running", dockerhost.ErrConflict, id)
	}
	f.execs = append(f.execs, ExecCall{ContainerID: c.c.ID, Container: c.c.Name, Spec: e})
	h, cid := f.execHandler, c.c.ID
	f.mu.Unlock()
	if h == nil {
		return dockerhost.ExecResult{}, nil
	}
	return h(cid, e), nil
}

// StopContainer implements the client method.
func (f *Fake) StopContainer(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("StopContainer"); err != nil {
		return err
	}
	c, err := f.find(id)
	if err != nil {
		return err
	}
	c.c.Running, c.c.Status = false, "exited"
	return nil
}

// ImageExists implements the client method.
func (f *Fake) ImageExists(_ context.Context, ref string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("ImageExists"); err != nil {
		return false, err
	}
	return f.images[ref], nil
}

// BuildImage implements the client method; the tag shows up in Images.
func (f *Fake) BuildImage(_ context.Context, tag, _ string, _ map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.popFail("BuildImage"); err != nil {
		return err
	}
	f.images[tag] = true
	return nil
}
