// This file (seed.go) implements the "seed" subcommand (
// "config/data seeding", §10-P6, §13.4): `control-plane seed -manifest
// <path> [-dry-run]`. Thin by design -- flag parsing, config load, DB
// pool, the Postgres version check + migrations (the SAME
// requireSupportedPostgres and applyMigrations helpers serve() itself
// calls, reused unchanged), then a single call into internal/app/seed.Run,
// which owns every actual decision. See internal/app/seed/doc.go for the
// full "why this lives here, not its own cmd/ binary" writeup (deps.go's
// own doc comment on Deps has the short version).

package controlplane

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/seed"
	"github.com/narvidev/narvi/internal/platform"
)

// runSeedCommand parses args (os.Args[2:] from main), loads config the
// SAME way serve() does, opens a pool, applies migrations (so `seed` also
// works as the very first command run against a fresh database, with no
// requirement that `serve` has ever run first), loads and validates the
// manifest, then runs the reconciliation and prints its report to stdout.
// Returns a non-nil error (causing main() to os.Exit(1)) on any setup
// failure OR if the report itself contains any per-item error -- the
// report is printed BEFORE that error is returned either way, so an
// operator (or CI) always sees what happened, not just an exit code.
func runSeedCommand(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	manifestPath := fs.String("manifest", "", "path to the seed manifest YAML file (required)")
	dryRun := fs.Bool("dry-run", false, "compute and print the plan without writing anything to the database")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *manifestPath == "" {
		return errors.New("control-plane seed: -manifest is required (usage: control-plane seed -manifest <path> [-dry-run])")
	}

	cfg, err := platform.Load()
	if err != nil {
		return err
	}
	logger := platform.NewLogger(os.Stdout, cfg.LogLevel)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeouts.SeedRunTimeout)
	defer cancel()

	manifest, err := seed.LoadManifest(*manifestPath)
	if err != nil {
		return err
	}

	pool, err := postgres.NewPoolWithMaxConns(ctx, cfg.DatabaseURL, cfg.DBPoolMaxConns)
	if err != nil {
		return fmt.Errorf("open postgres pool: %w", err)
	}
	defer pool.Close()

	// The same refusal serve() makes of a server older than
	// platform.MinPostgresServerVersionNum, before any migration runs
	// against it (§5.1).
	if err := requireSupportedPostgres(ctx, pool, cfg.Timeouts.PostgresVersionCheckTimeout); err != nil {
		return err
	}

	// Mirrors serve()'s own boot-time migration call exactly (same
	// helper, same "safe to call on every boot regardless of replica
	// count" reasoning) -- an operator can run `control-plane seed`
	// against a brand-new database with no prior `control-plane serve`
	// invocation.
	if err := applyMigrations(cfg.DatabaseURL); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	deps := seed.NewDeps(pool, cfg.TokenEncryptionKey, cfg.InitialAdminEmails)

	report, runErr := seed.Run(ctx, deps, manifest, *dryRun)
	if report != nil {
		fmt.Print(report.String())
	}
	if runErr != nil {
		return fmt.Errorf("control-plane seed: %w", runErr)
	}
	if report.HasErrors() {
		return errors.New("control-plane seed: completed with one or more item errors, see report above")
	}
	return nil
}
