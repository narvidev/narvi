//go:build integration

package controlplane

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/platform"
)

// TestRun_BeginsTheOutboxWorkersShutdown proves the one shutdown state
// Build hands the outbox delivery worker is the one Run sets as its drain
// begins (technical plan §5.1): the Builder that Build wired reads not
// begun while Run serves, and begun once Run's context has ended and Run
// has returned. Were Build to hand the Builder a state of its own, Run
// would set another, and every delivery a deploy cuts short would be
// counted as a failed attempt again.
func TestRun_BeginsTheOutboxWorkersShutdown(t *testing.T) {
	setRequiredEnv(t)
	pool, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)
	cfg, err := platform.Load()
	if err != nil {
		t.Fatalf("platform.Load: %v", err)
	}

	app, err := Build(context.Background(), cfg, pool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if app.outboxBuilder.ShutdownBegun() {
		t.Fatal("the outbox worker reads shutdown begun before Run has even started")
	}

	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	var running errgroup.Group
	running.Go(func() error { return app.Run(runCtx, "127.0.0.1:0") })

	stopRun()
	if err := running.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("App.Run after its context was cancelled = %v, want a clean shutdown", err)
	}
	if !app.outboxBuilder.ShutdownBegun() {
		t.Fatal("Run's drain began, and the outbox worker Build wired still reads not begun: the two hold different shutdown states")
	}
}
