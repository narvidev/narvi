// This file (modules.go) is controlplane's own module-composition logic:
// validating the combination of composed extension.Module values at
// boot (before any wiring runs), applying each module's own migrations,
// building and boot-logging the capability registry, and mounting each
// module's own routes/workers. See docs/design/boundaries-design.md,
// sections 1 and 3.2, for the full design; Build (serve.go) is the only
// caller.

package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/narvidev/narvi/extension"
	"github.com/narvidev/narvi/internal/app/capability"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/knowledge"
	"github.com/narvidev/narvi/internal/domain/license"
	"github.com/narvidev/narvi/internal/platform"
)

// moduleNamePattern is extension.Module.Name's own required shape --
// routes mount under /api/ext/<Name>/ and migrations track in
// "<Name>_schema_migrations", so an empty or shell-metacharacter-bearing
// Name would corrupt either.
var moduleNamePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// InvalidModuleError names one problem Build found in a composed
// module's own declared shape, checked BEFORE any wiring runs -- a
// malformed module fails boot loudly, once, rather than mounting a
// broken or half-wired route table.
type InvalidModuleError struct {
	// Name is the offending module's own Name, verbatim -- may itself be
	// the malformed value being reported.
	Name   string
	Reason string
}

func (e *InvalidModuleError) Error() string {
	return fmt.Sprintf("controlplane: invalid module %q: %s", e.Name, e.Reason)
}

// validateModules checks every composed module's own declared shape
// before any wiring (migrations, routes, workers) runs. Every problem
// found is joined via errors.Join rather than returning on the first --
// mirrors platform.Timeouts.Validate's own identical "report every
// violation, not just one" precedent -- so a boot refusal names every
// defect at once.
//
// Three checks, per extension.Module's own Name, Capabilities and
// KnowledgeRanker doc comments (docs/design/boundaries-design.md,
// section 3.2):
//
//   - Name must match moduleNamePattern and be unique among every
//     composed module -- two modules sharing a Name would collide on
//     both the same /api/ext/<Name>/ route prefix and the same
//     "<Name>_schema_migrations" migrations table.
//   - Every declared Capability must be a member of license.All: a
//     module cannot implement a capability this build does not even
//     define.
//   - CapabilityKnowledgeRetrieval and KnowledgeRanker are a
//     BICONDITIONAL -- completing the check the paragraph above used to
//     defer, from when this struct had no hook to check it against. A
//     module declaring the capability without supplying a ranker has
//     advertised ranking behavior it does not implement; one supplying a
//     ranker without declaring the capability would run private ranking
//     logic capabilitySwitchRanker (knowledgeranker.go) never gates,
//     since that wrapper is the only place this codebase ever reads
//     CapabilityKnowledgeRetrieval to decide which ranker runs. Both
//     halves refuse boot the same way the first two checks do.
func validateModules(modules []extension.Module) error {
	var errs []error
	seen := make(map[string]bool, len(modules))

	for _, m := range modules {
		switch {
		case !moduleNamePattern.MatchString(m.Name):
			errs = append(errs, &InvalidModuleError{Name: m.Name, Reason: "Name must match ^[a-z0-9-]+$"})
		case seen[m.Name]:
			errs = append(errs, &InvalidModuleError{Name: m.Name, Reason: "Name is already used by another composed module"})
		}
		seen[m.Name] = true

		declaresKnowledgeRetrieval := false
		for _, c := range m.Capabilities {
			if !isKnownCapability(c) {
				errs = append(errs, &InvalidModuleError{Name: m.Name, Reason: fmt.Sprintf("declares capability %q, which this build does not implement", c)})
			}
			if c == license.CapabilityKnowledgeRetrieval {
				declaresKnowledgeRetrieval = true
			}
		}

		switch {
		case declaresKnowledgeRetrieval && m.KnowledgeRanker == nil:
			errs = append(errs, &InvalidModuleError{Name: m.Name, Reason: fmt.Sprintf("declares capability %q but supplies no KnowledgeRanker", license.CapabilityKnowledgeRetrieval)})
		case !declaresKnowledgeRetrieval && m.KnowledgeRanker != nil:
			errs = append(errs, &InvalidModuleError{Name: m.Name, Reason: fmt.Sprintf("supplies a KnowledgeRanker but does not declare capability %q", license.CapabilityKnowledgeRetrieval)})
		}
	}

	return errors.Join(errs...)
}

// isKnownCapability reports whether c is a member of license.All --
// mirrors internal/domain/license's own identical, unexported check
// (license.Parse's own ErrUnknownCapability path), applied here to the
// INSTALLED side of the registry's conjunction rather than the LICENSED
// side.
func isKnownCapability(c license.Capability) bool {
	for _, known := range license.All {
		if known == c {
			return true
		}
	}
	return false
}

// unionCapabilities returns the deduplicated union of every composed
// module's own declared Capabilities, in first-seen order -- the
// capability registry's own "installed" input (docs/design/
// boundaries-design.md, section 1.1). Empty for zero modules, which is exactly
// the public binary's own shape.
func unionCapabilities(modules []extension.Module) []license.Capability {
	seen := make(map[license.Capability]bool)
	var installed []license.Capability
	for _, m := range modules {
		for _, c := range m.Capabilities {
			if !seen[c] {
				seen[c] = true
				installed = append(installed, c)
			}
		}
	}
	return installed
}

// parseLicenseKey parses raw against the embedded production keyset,
// returning (nil, nil) for an absent key -- DELIBERATELY DISTINCT from a
// present-but-unparseable key, which returns (nil, err). Both cases leave
// internal/app/capability.Registry.Enabled identically false (a nil
// grant, either way), but logLicenseBoot needs to tell "no key
// configured" apart from "a key was configured but did not verify" to
// pick the right line from docs/design/boundaries-design.md, section
// 1.3's table.
func parseLicenseKey(raw string) (*license.Grant, error) {
	if raw == "" {
		return nil, nil
	}
	grant, err := license.Parse(raw, license.IssuerKeys())
	if err != nil {
		return nil, err
	}
	return &grant, nil
}

// buildCapabilityRegistry parses cfg's own configured licence key, builds
// the capability registry over installed (every composed module's own
// declared Capabilities), and writes this boot's own design-note
// section-1.3 log lines (silent for zero modules -- see logLicenseBoot's
// own doc comment).
// Called once, in Build, before the router is constructed.
func buildCapabilityRegistry(logger *slog.Logger, cfg *platform.Config, modules []extension.Module) *capability.Registry {
	installed := unionCapabilities(modules)
	grant, parseErr := parseLicenseKey(cfg.LicenseKey)
	reg := capability.New(installed, grant, parseErr, time.Now, cfg.Timeouts.LicenseNotBeforeSkew)
	logLicenseBoot(logger, modules, reg, grant, parseErr, time.Now(), cfg.Timeouts.LicenseNotBeforeSkew)
	return reg
}

// logLicenseBoot writes docs/design/boundaries-design.md, section 1.3's
// own boot log lines -- but ONLY when at least one module is composed: with zero
// modules, the public binary's own column in that table is "silent" for
// EVERY key state (technical plan §34.5: "a key can entitle; it can
// never create behavior" -- with nothing installed, there is nothing for
// any key to entitle, and nothing in that fact is worth an operator's
// attention on a deployment that will never compose a module).
//
// reg is consulted through extension.Capabilities -- the SAME narrow,
// one-method interface a composed module itself receives, never the
// concrete *capability.Registry -- specifically so this exact function
// (the one buildCapabilityRegistry, and therefore Build, always calls)
// is unit-testable against a counting fake with no real licence key or
// Postgres: see TestBuild_WithoutModules_NeverConsultsCapabilities, which
// asserts a counting fake's own Enabled is called zero times when modules
// is empty.
func logLicenseBoot(logger *slog.Logger, modules []extension.Module, reg extension.Capabilities, grant *license.Grant, parseErr error, now time.Time, nbfSkew time.Duration) {
	if len(modules) == 0 {
		return
	}

	if grant == nil {
		if parseErr == nil {
			logger.Info("narvi control-plane: no licence key configured -- Narvi Gatekeeper capabilities disabled")
		} else {
			logger.Warn("narvi control-plane: licence key did not verify -- Narvi Gatekeeper capabilities disabled", "error", parseErr)
		}
		return
	}

	if !grant.ValidAt(now, nbfSkew) {
		if now.Before(grant.NotBefore.Add(-nbfSkew)) {
			logger.Warn("narvi control-plane: licence key not yet valid -- check the host clock", "not_before", grant.NotBefore)
		} else {
			logger.Warn("narvi control-plane: licence key expired", "expired_at", grant.ExpiresAt)
		}
		return
	}

	for _, c := range license.All {
		if !grant.Has(c) {
			continue // "valid, c not granted": no line (design note section 1.3's table)
		}
		if reg.Enabled(c) {
			logger.Info("narvi control-plane: capability enabled", "capability", c, "subject", grant.Subject, "expires_at", grant.ExpiresAt)
		} else {
			logger.Info("narvi control-plane: capability licensed but not installed", "capability", c)
		}
	}
}

// selectWebAssets returns the last non-nil WebAssets among modules, or
// fallback if none supplied one (docs/design/boundaries-design.md,
// section 3.2: "WebAssets, when non-nil, replaces webui.DistFS"). Composition is
// always zero or one module today, so "last wins" is equivalent to "the
// one composed module's own choice" in every real deployment.
func selectWebAssets(fallback fs.FS, modules []extension.Module) fs.FS {
	assets := fallback
	for _, m := range modules {
		if m.WebAssets != nil {
			assets = m.WebAssets
		}
	}
	return assets
}

// mountModules wires each module's own routes onto router under
// /api/ext/<Name>/, behind requireAuth -- so a module route is
// authenticated before its own Mount ever runs, mirroring every public
// route group's own auth.Middleware placement (extension.Module.Mount's
// own doc comment) -- and collects every worker every module
// contributes, ready for Run's own errgroup. validateModules has already
// run by the time Build calls this, so every m.Name here is well-formed
// and unique.
func mountModules(router chi.Router, modules []extension.Module, requireAuth func(http.Handler) http.Handler, rt extension.Runtime) []extension.Worker {
	var workers []extension.Worker
	for _, m := range modules {
		if m.Mount != nil {
			mount := m.Mount
			router.Route("/api/ext/"+m.Name, func(r chi.Router) {
				r.Use(requireAuth)
				mount(r, rt)
			})
		}
		if m.Workers != nil {
			workers = append(workers, m.Workers(rt)...)
		}
	}
	return workers
}

// applyModuleMigrations runs fsys's own migrations against dsn, tracked
// in tableName rather than this repository's own schema_migrations table
// -- so a module's own migration chain never shares a version counter
// with Narvi's (docs/design/boundaries-design.md, section 3.2). Mirrors
// applyMigrations (migrate.go) exactly, parameterized over the migration
// source and its own tracking table. Applied regardless of licence
// state: schema presence is not licensed behavior (the design note's own
// "applied regardless of licence state").
func applyModuleMigrations(dsn, tableName string, fsys fs.FS) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open migration db handle for module migrations table %q: %w", tableName, err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			slog.Error("close module migration db handle failed", "error", closeErr, "migrations_table", tableName)
		}
	}()

	dbDriver, err := migratepg.WithInstance(db, &migratepg.Config{MigrationsTable: tableName})
	if err != nil {
		return fmt.Errorf("migrate postgres driver for module migrations table %q: %w", tableName, err)
	}

	srcDriver, err := iofs.New(fsys, ".")
	if err != nil {
		return fmt.Errorf("migrate iofs source for module migrations table %q: %w", tableName, err)
	}

	m, err := migrate.NewWithInstance("iofs", srcDriver, "pgx", dbDriver)
	if err != nil {
		return fmt.Errorf("migrate.NewWithInstance for module migrations table %q: %w", tableName, err)
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up for module migrations table %q: %w", tableName, err)
	}

	return nil
}

// capabilitySwitchRanker is the ONLY place in this codebase
// license.CapabilityKnowledgeRetrieval is ever read to decide which
// ports.KnowledgeRanker actually runs (docs/design/boundaries-design.md,
// section 2.2) -- it lives here, in controlplane, because this package
// is one of the few tools/lint/narvichecks/capabilityimportban lets
// import internal/app/capability at all (that analyzer's own doc
// comment).
//
// reg is consulted PER CALL, in both Name and Score, never cached at
// construction: a licence that expires mid-process reverts to public at
// the very next call, and one activated mid-process takes effect the
// same way -- neither direction needs a restart. That is this switch's
// entire job, deciding WHICH ranker runs, and nothing more: it has no
// opinion of its own on ordering (every KnowledgeRanker's Score can only
// ever return scores, never candidates -- see the port's own doc
// comment), and a failure or timeout in private is returned to the
// caller UNCHANGED, never silently retried against public. Falling back
// to the gate's own order on a ranker failure, never to empty, is
// reviewcontext.FetchPriorArchDecisions' own job (a later Step), not
// this switch's -- conflating the two would mean two different places in
// this codebase each partially implementing degradation.
type capabilitySwitchRanker struct {
	reg     *capability.Registry
	private ports.KnowledgeRanker
	public  ports.KnowledgeRanker
	timeout time.Duration
}

// Name returns whichever ranker Score would currently delegate to's own
// Name, re-decided on every call exactly like Score itself -- so a
// caller recording which ranker actually ran never needs its own copy of
// this switch's own licence check.
func (r capabilitySwitchRanker) Name() string {
	if r.reg.Enabled(license.CapabilityKnowledgeRetrieval) {
		return r.private.Name()
	}
	return r.public.Name()
}

// Score delegates to private when CapabilityKnowledgeRetrieval is
// currently enabled, to public otherwise -- re-decided on every call. The
// private call alone is bounded by timeout, layered onto ctx via
// context.WithTimeout: a composed module's own ranker is third-party
// code this switch cannot trust to respect ctx's own deadline
// unprompted, unlike public (first-party, synchronous, zero I/O, needs no
// bound of its own). Returns whatever the chosen ranker itself returns,
// value or error, unmodified.
func (r capabilitySwitchRanker) Score(ctx context.Context, q knowledge.Query, cands []knowledge.Candidate) ([]float64, error) {
	if !r.reg.Enabled(license.CapabilityKnowledgeRetrieval) {
		return r.public.Score(ctx, q, cands)
	}
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	// A composed module's ranker gets a deep copy, never the caller's own
	// slices. The port's signature stops it adding or dropping a
	// candidate, but a []Candidate is a slice header: an implementation
	// receiving one can rewrite the elements the caller still holds, and
	// substituted prose would then reach the review prompt having bypassed
	// the sanitization applied when the decision was written. Same
	// reasoning as the timeout above -- this is the boundary where a
	// module's own code starts, so it is where the caller stops trusting
	// it. The public branch above is first-party and gets no copy.
	sq, scands := knowledge.CloneForRanking(q, cands)
	return r.private.Score(cctx, sq, scands)
}

// selectKnowledgeRanker returns the ports.KnowledgeRanker Build wires in:
// knowledge.RecencyRanker{} when no composed module supplies one, or a
// capabilitySwitchRanker over the LAST module's own KnowledgeRanker
// otherwise -- mirrors selectWebAssets' own identical "last supplier
// wins" reasoning (composition is always zero or one module today).
//
// The no-module (and no-KnowledgeRanker-supplying-module) branch never
// touches reg at all -- it returns knowledge.RecencyRanker{} directly,
// without even reading the field -- preserving
// TestBuild_WithoutModules_NeverConsultsCapabilities' own guarantee: this
// function is as much a part of "nothing Build does ever consults the
// registry with zero modules composed" as buildCapabilityRegistry/
// logLicenseBoot already are.
func selectKnowledgeRanker(reg *capability.Registry, modules []extension.Module, timeout time.Duration) ports.KnowledgeRanker {
	var private ports.KnowledgeRanker
	for _, m := range modules {
		if m.KnowledgeRanker != nil {
			private = m.KnowledgeRanker
		}
	}
	if private == nil {
		return knowledge.RecencyRanker{}
	}
	return capabilitySwitchRanker{reg: reg, private: private, public: knowledge.RecencyRanker{}, timeout: timeout}
}
