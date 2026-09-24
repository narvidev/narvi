package controlplane

import (
	"context"
	"fmt"
	"io"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/platform"
)

// runRoutesCommand is the "routes" subcommand's own implementation (§41.1
// review round 1, findings P1/P3): load config, open the pool, apply
// migrations, build the router with the SAME Build serve() uses, and
// print its route table to w -- one "METHOD /path" per line, sorted --
// exactly controlplane/testdata/routes.golden's own format (App.Routes()
// already returns it pre-sorted; see that method's own doc comment).
//
// Deliberately mirrors serve()'s own "load config, open the pool, apply
// migrations, then Build" sequence up to (but never past) the point where
// serve() calls verifyGitHubAppScopeAtBoot: this command never
// constructs a GitHub App client and never calls out to GitHub at all,
// and it never calls App.Run, so no listener ever opens. That is what
// makes `docker run <image> routes` -- against a real, migrated Postgres,
// with no other configuration a live boot would need -- a clean,
// GitHub-independent proof that the packaged image's router is IDENTICAL
// to routes.golden, byte for byte, wholly separate from the "does this
// image actually serve traffic" proof (which does need a reachable
// GitHub App scope check, real or stubbed).
func runRoutesCommand(ctx context.Context, w io.Writer) error {
	cfg, err := platform.Load()
	if err != nil {
		return err
	}

	pool, err := postgres.NewPoolWithMaxConns(ctx, cfg.DatabaseURL, cfg.DBPoolMaxConns)
	if err != nil {
		return fmt.Errorf("open postgres pool: %w", err)
	}
	defer pool.Close()

	if err := applyMigrations(cfg.DatabaseURL); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	app, err := Build(ctx, cfg, pool)
	if err != nil {
		return err
	}

	for _, route := range app.Routes() {
		if _, err := fmt.Fprintln(w, route); err != nil {
			return fmt.Errorf("write route table: %w", err)
		}
	}
	return nil
}
