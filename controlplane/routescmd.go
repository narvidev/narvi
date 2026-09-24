package controlplane

import (
	"context"
	"fmt"
	"io"

	"github.com/narvidev/narvi/extension"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/platform"
)

// runRoutesCommand is the "routes" subcommand's own implementation (§41.1
// review round 1, findings P1/P3; round 2, findings Q1/Q3/Q6/Q12): load
// config, open the pool, and build the router with the SAME Build -- and
// the SAME modules -- serve() uses, then print its route table to w --
// one "METHOD /path" per line, sorted -- exactly
// controlplane/testdata/routes.golden's own format (App.Routes() already
// returns it pre-sorted; see that method's own doc comment).
//
// This command is READ-ONLY: unlike round 1's version, it does NOT apply
// migrations (§41.1 review round 2, finding Q1/Q3). Build does not need a
// migrated schema to construct the router -- its only DB read at
// construction time, CountSuppressedRepos, merely logs a WARN on failure
// -- and the pool returned by NewPoolWithMaxConns is lazy (it never
// pings), so no reachable database is required either. Forward-migrating
// a database that a "list the routes" command was only asked to inspect
// is exactly the hazard round 2 found: it can advance a production
// schema ahead of a rollout, race serve()'s own migration lock, or fail
// outright under a read-only DB role where a listing should still
// succeed. If the target schema is missing tables Build wants to read
// (e.g. a fresh, unmigrated database), the affected reads log a WARN to
// stderr and are otherwise silently skipped -- that is acceptable for a
// read-only introspection command.
//
// modules is the same extension.Module list Main receives and threads to
// serve() (§41.1 review round 2, finding Q6/Q12): passing none here, as
// this command did before round 2, would make a composed private
// binary's `routes` output silently omit every module's mounted routes
// and skip validateModules, contradicting this command's own "SAME Build
// serve() uses" claim.
func runRoutesCommand(ctx context.Context, w io.Writer, modules ...extension.Module) error {
	cfg, err := platform.Load()
	if err != nil {
		return err
	}

	pool, err := postgres.NewPoolWithMaxConns(ctx, cfg.DatabaseURL, cfg.DBPoolMaxConns)
	if err != nil {
		return fmt.Errorf("open postgres pool: %w", err)
	}
	defer pool.Close()

	app, err := Build(ctx, cfg, pool, modules...)
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
