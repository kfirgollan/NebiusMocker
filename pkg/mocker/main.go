package mocker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
)

const shutdownTimeout = 10 * time.Second

// Main runs a mocker process with the given modules until SIGINT/SIGTERM and
// returns the process exit code. It is the whole body of every cmd/ binary.
func Main(modules ...Module) int {
	cfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "nebius-mocker: config:", err)
		return 2
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := New(cfg, log, modules)
	if err != nil {
		log.Error("assemble server", "error", err)
		return 1
	}
	if _, err := srv.Start(ctx); err != nil {
		log.Error("start server", "error", err)
		return 1
	}
	<-ctx.Done()
	log.Info("shutting down", "cause", context.Cause(ctx))
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Stop(shutdownCtx); err != nil {
		log.Error("shutdown", "error", err)
		return 1
	}
	return 0
}
