// This file (rolloutgate.go) implements §10's own ("feature-flagged
// cohort rollout of sessions, with documented rollback", §10 Phase 6,
// §32) primary, session-creation-time gate: checkRolloutGate, called from
// CreateSessionOnTx (create.go) after validateCreateSessionRequest and
// before the environment/session inserts, on the SAME transaction that is
// about to insert the session.
//
// This is HALF of §32's "fail-closed, twice" pair -- the dispatch-time
// re-check (internal/app/sessionactor's own tryPlanSpawn, beside
// refuseIfSubstrateUnsupported) is the other half, and is what makes
// rollback real: without it, de-enrolling a repo would leave an existing
// PR review session respawning sandboxes forever on re-review turns,
// since those ride the REUSE branch and the actor's own dispatch loop,
// never this creation funnel. Both sites share internal/domain/rollout's
// ONE pure decision (Decide) -- this file's only job is the I/O half:
// resolving each named repo to a trusted, host-verified identity, reading
// its repo_settings.sessions_enabled row (fail-closed), and turning the
// result into a *CreateSessionError every caller of CreateSessionOnTx
// already knows how to propagate.

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/rollout"
	"github.com/narvidev/narvi/internal/platform"
)

// rolloutGateMeterName is this package's own OTel meter name for the
// rollout-refusal counter, mirroring cloudIdentityMeterName's own
// "narvi/httpapi-<concern>" precedent exactly (cloudidentitymetrics.go).
const rolloutGateMeterName = "narvi/httpapi-rolloutgate"

// sessionRolloutRefusedTotalCounter is resolved LAZILY, on first use
// (sync.OnceValue) -- mirrors cloudIdentityMintTotalCounter's own doc
// comment exactly: CreateSessionOnTx is a free function with no
// per-process constructor object to anchor eager construction to, and
// resolving otel.Meter at package-init time would permanently bind this
// instrument to whatever MeterProvider happens to be globally registered
// at that moment (which main.go's own real OTel SDK setup may not have
// installed yet).
var sessionRolloutRefusedTotalCounter = sync.OnceValue(newSessionRolloutRefusedTotalCounter)

func newSessionRolloutRefusedTotalCounter() metric.Int64Counter {
	c, err := otel.Meter(rolloutGateMeterName).Int64Counter(
		"session_rollout_refused_total",
		// Phase 6 audit fix (Finding 4): this description used to claim
		// the scope was session-CREATION refusals only -- true of THIS
		// package's own checkRolloutGate call site, but not of the metric
		// as a whole: internal/app/sessionactor's own dispatch-time gates
		// (refuseIfRolloutUnenrolled/rolloutRefusalForDispatch,
		// dispatch.go) register and increment the SAME instrument name
		// under their own meter (a metrics backend aggregates by
		// instrument name across meters, so this is genuinely the same
		// counter from an operator's own point of view) -- see
		// opsmetrics.go's own rolloutRefused doc comment there for why.
		metric.WithDescription("Count of every session-creation attempt, spawn/restore/resume attempt, or turn dispatch refused by §10's cohort-rollout gate (§32) because a named repo was not enrolled (repo_settings.sessions_enabled) -- httpapi.checkRolloutGate's own session-creation-time refusals here, PLUS internal/app/sessionactor's own dispatch-time re-check refusals (registered under the SAME name there). Tagged by the \"spawn_source\" attribute -- see checkRolloutGate's own doc comment."),
		metric.WithUnit("{refusal}"),
	)
	if err != nil {
		// Structurally cannot fail for a fixed, well-formed instrument
		// name -- logged defensively anyway, mirroring
		// newCloudIdentityMintTotalCounter's own identical precedent.
		platform.Logger(context.Background()).Error("httpapi: construct session_rollout_refused_total counter failed", "error", err)
	}
	return c
}

// recordRolloutRefusal increments the refusal counter by one, tagged by
// spawnSource (session_spawn_source's own wire value -- web/slack/linear/
// github), mirroring recordCloudIdentityMint's own "kind" attribute
// precedent.
func recordRolloutRefusal(ctx context.Context, spawnSource string) {
	sessionRolloutRefusedTotalCounter().Add(ctx, 1, metric.WithAttributes(attribute.String("spawn_source", spawnSource)))
}

// resolveTrustedRepoFullName resolves rawURL to a trusted "owner/repo"
// identity -- reposource.CheckRepoHost FIRST, then ParseOwnerRepo, exactly
// the pairing every other ParseOwnerRepo call site in this codebase
// already uses (app/sessionactor's pushpr.go/contractdrift.go/
// imageresolve.go, app/outboxworker's sentinelautofix.go, app/imagebuild's
// builder.go). This pairing is load-bearing, not defensive decoration:
// ParseOwnerRepo is deliberately host-agnostic (its own doc comment: "it
// never inspects rawURL's host at all"), so https://evil.example/acme/
// widgets.git derives the SAME "acme/widgets" a genuine https://
// github.com/acme/widgets.git would -- without the host check running
// FIRST, a repo enrolled/known under github.com could be spoofed by ANY
// host that happens to reuse its owner/repo path. ok is false for either a
// rejected host or an unparseable path -- every caller folds either into
// its own fail-closed "not admitted" fact, never distinguishing the two
// beyond its own log line.
//
// Shared by BOTH of this package's repo-keyed session-creation gates --
// checkRolloutGate (§32.3, this file) and checkRepoEntitlementGate
// (§31.4, repoentitlementgate.go) -- exactly one implementation of this
// check in the package, not two independently-maintained copies (mirrors
// resolveKnownRepo's own identical "exactly one implementation" value,
// reposettings.go).
func resolveTrustedRepoFullName(rawURL string) (fullName string, ok bool) {
	if err := reposource.CheckRepoHost(rawURL, ports.SupportedSourceControlHosts()...); err != nil {
		return "", false
	}
	owner, repo, err := reposource.ParseOwnerRepo(rawURL)
	if err != nil {
		return "", false
	}
	return owner + "/" + repo, true
}

// checkRolloutGate is §32's own primary gate. mode is platform.Config.
// RolloutMode, verbatim -- when it is anything other than rollout.
// ModeCohort (including the unset/default rollout.ModeOpen), this
// function returns nil WITHOUT iterating repos, resolving any URL, or
// touching repoSettings at all: §32's own "byte-for-byte no-op" property
// for every existing deployment and CI depends on this short-circuit
// running before any I/O, not just on Decide's own pure short-circuit
// (internal/domain/rollout.Decide has the identical guard, but this
// function must not even READ repo_settings to get there).
//
// In rollout.ModeCohort, every repo in repos is resolved and looked up,
// on tx (repoSettings.WithTx(tx).Get) -- fail-closed per §32: an absent
// row (pgx.ErrNoRows), any OTHER read error, or an unresolvable/
// unsupported-host URL (resolveTrustedRepoFullName's own ok=false) all
// fold into RepoAdmission.Enrolled == false identically. §32's own
// reasoning for why this is nearly free: this read runs inside the SAME
// transaction that was about to insert the session two statements later,
// on the same Postgres -- there is no real state where repo_settings is
// unreadable but that insert would have succeeded.
//
// On refusal, this logs a structured Warn (repo, spawn source, mode --
// point 8) and increments session_rollout_refused_total, but writes NO
// audit_log row: this codebase's own audit_log table records completed
// STATE CHANGES only, never a refusal of any kind (reposettings.go's own
// logUnknownRepoRefusal doc comment states this convention explicitly;
// mirrored here, not reinvented).
//
// Fail-closed vs. terminal (adversarial-review fix, §32): failing closed
// on a degraded read and marking a refusal as a PERMANENT policy decision
// are two different properties, and this function used to conflate them
// -- every refusal, including one caused by nothing more than a
// context-canceled or timed-out repo_settings query, came back with
// CreateSessionError.RolloutRefusal set. The four ingress paths
// (REST/create.go aside, which does not branch on it) use that field
// structurally to decide terminal vs. retry (CreateSessionError's own doc
// comment) -- so a momentary database blip did not merely refuse THIS
// attempt, it made Linear ack terminally, Slack post a permanent denial,
// GitHub stay silent while keeping its claim, and the sentinel-autofix
// outbox skip terminally, permanently dropping legitimate work for a repo
// that may be genuinely enrolled, with no retry ever attempted.
//
// The fix: this function still refuses the attempt EITHER way (fail-
// closed stays fail-closed -- a repo whose enrollment could not be
// verified is never silently admitted), but RolloutRefusal is now true
// ONLY when the refusal is a genuine, DEMONSTRATED policy outcome (mode
// is cohort and the repo's own repo_settings row -- or lack of one -- was
// actually read; an unresolvable/unsupported-host URL is equally
// terminal, since re-parsing the identical URL can never produce a
// different answer). readErrored tracks, alongside admissions and in the
// SAME order, which admissions were forced to Enrolled == false by a
// genuine I/O error (an absent row, or a URL that could never resolve,
// are real, reproducible facts -- never counted here) rather than a
// demonstrated fact -- so whichever admission rollout.Decide's own
// "first not-enrolled repo" stops at (decision.RepoFullName) can be
// checked for whether THAT SPECIFIC refusal was read-error-caused, not
// merely whether ANY repo in the request happened to hit one.
func checkRolloutGate(ctx context.Context, tx pgx.Tx, repoSettings *postgres.RepoSettingsStore, mode platform.RolloutMode, req restdtos.CreateSessionRequest) *CreateSessionError {
	if mode != rollout.ModeCohort {
		return nil
	}

	logger := platform.Logger(ctx)

	admissions := make([]rollout.RepoAdmission, 0, len(req.Repos))
	readErrored := make([]bool, 0, len(req.Repos))
	for _, repo := range req.Repos {
		fullName, resolved := resolveTrustedRepoFullName(repo.Url)
		if !resolved {
			logger.Warn("httpapi: rollout gate: repo url could not be resolved to a trusted, host-verified owner/repo identity; treating as not enrolled",
				"url", repo.Url, "spawn_source", string(req.SpawnSource), "rollout_mode", string(mode))
			admissions = append(admissions, rollout.RepoAdmission{FullName: repo.Url, Enrolled: false})
			readErrored = append(readErrored, false)
			continue
		}

		row, err := repoSettings.WithTx(tx).Get(ctx, fullName)
		switch {
		case err == nil:
			admissions = append(admissions, rollout.RepoAdmission{FullName: fullName, Enrolled: row.SessionsEnabled})
			readErrored = append(readErrored, false)
		case errors.Is(err, pgx.ErrNoRows):
			admissions = append(admissions, rollout.RepoAdmission{FullName: fullName, Enrolled: false})
			readErrored = append(readErrored, false)
		default:
			// A genuine read error -- including a context-canceled or
			// timed-out query, which surfaces here identically, never as
			// some other classification -- on the transaction about to
			// insert this very session -- fail-closed (§32 finding
			// C3's own precedent: widening policy on a degraded read is
			// backwards), never treated as "no row, so unenrolled"
			// without comment: logged distinctly so an operator can tell
			// a real Postgres problem apart from an ordinary
			// not-yet-enrolled repo. readErrored records this SPECIFICALLY
			// so the refusal built below can tell a degraded read apart
			// from a demonstrated policy fact -- see this function's own
			// doc comment.
			logger.Warn("httpapi: rollout gate: read repo_settings failed; failing closed (treating as not enrolled)",
				"repo", fullName, "error", err, "spawn_source", string(req.SpawnSource), "rollout_mode", string(mode))
			admissions = append(admissions, rollout.RepoAdmission{FullName: fullName, Enrolled: false})
			readErrored = append(readErrored, true)
		}
	}

	decision := rollout.Decide(mode, admissions)
	if decision.Admitted {
		return nil
	}

	// rollout.Decide's own doc comment: "refuses on the FIRST [repo] that
	// is not [enrolled]" -- mirrored here, over the SAME admissions slice
	// in the SAME order, so refusalIsTransient always describes exactly
	// the repo Decide's own decision.RepoFullName names, never a
	// different one a later, unrelated read error happened to touch.
	refusalIsTransient := false
	for i, a := range admissions {
		if !a.Enrolled {
			refusalIsTransient = readErrored[i]
			break
		}
	}

	logger.Warn("httpapi: rollout gate: session creation refused, repo not enrolled",
		"repo", decision.RepoFullName, "spawn_source", string(req.SpawnSource), "rollout_mode", string(mode), "transient", refusalIsTransient)

	if refusalIsTransient {
		// Fail-closed, but NOT a policy refusal: repo_settings could not be
		// read for this repo (a context cancellation, a timeout, or any
		// other degraded-read condition), so THIS attempt is refused, but
		// RolloutRefusal stays false -- every one of the four ingress
		// channels' own existing transient-failure retry path applies
		// here exactly as it would for any other database hiccup, so the
		// work is picked up again once Postgres recovers, rather than
		// being dropped forever. Deliberately no session_rollout_refused_
		// total increment here: that counter is §32's own "a repo was
		// refused by policy" signal (its own metric doc comment) -- a
		// degraded read is an infrastructure problem, not a rollout
		// decision, and conflating the two would make this metric lie to
		// an operator about how many repos are actually being kept out by
		// the cohort gate.
		return &CreateSessionError{
			Status:  http.StatusServiceUnavailable,
			Message: "repository enrollment could not be verified: " + decision.RepoFullName,
		}
	}

	recordRolloutRefusal(ctx, string(req.SpawnSource))

	return &CreateSessionError{
		Status:         http.StatusForbidden,
		Message:        "repository not enrolled: " + decision.RepoFullName,
		RolloutRefusal: true,
	}
}
