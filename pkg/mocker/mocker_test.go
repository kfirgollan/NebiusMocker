package mocker

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
)

type lifecycleModule struct {
	name     string
	startErr error
	events   *[]string
}

func (m *lifecycleModule) Name() string { return m.name }

func (m *lifecycleModule) Register(context.Context, grpc.ServiceRegistrar, Deps) error { return nil }

func (m *lifecycleModule) Start(context.Context) error {
	*m.events = append(*m.events, "start "+m.name)
	return m.startErr
}

func (m *lifecycleModule) Stop(context.Context) error {
	*m.events = append(*m.events, "stop "+m.name)
	return nil
}

func newTestServer(t *testing.T, modules ...Module) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	s, err := New(cfg, slog.New(slog.DiscardHandler), modules)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLifecycleOrder(t *testing.T) {
	t.Parallel()
	var events []string
	s := newTestServer(t, &lifecycleModule{name: "a", events: &events}, &lifecycleModule{name: "b", events: &events})
	if _, err := s.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if want := []string{"start a", "start b", "stop b", "stop a"}; !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestLifecycleStartFailureRollsBack(t *testing.T) {
	t.Parallel()
	var events []string
	boom := errors.New("boom")
	s := newTestServer(t, &lifecycleModule{name: "a", events: &events}, &lifecycleModule{name: "b", startErr: boom, events: &events})
	if _, err := s.Start(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("Start err = %v, want boom", err)
	}
	if want := []string{"start a", "start b", "stop a"}; !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}
