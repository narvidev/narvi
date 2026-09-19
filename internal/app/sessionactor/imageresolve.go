// This file (imageresolve.go) implements §8.5's ("image builds",
// §8.5-note/§10-P2) own real per-spawn image selection: dispatch.go's
// planFreshSpawn/planRestore both build their CreateSpec with
// Image=defaultBaseImage (unconditionally, exactly as before this Step),
// and handleEnsureDispatched then calls resolveAndSetImage (below) to
// CORRECT that field in place -- to a real, previously-built image_ref, if
// (and only if) one is already known-ready for this exact spawn's own
// fingerprint -- immediately before executeSpawn/executeRestore ever calls
// the real provider.
//
// # §19.1 ("warm boot: shared fingerprint + spawn-path simplification",
// §19.1) rewrite -- what changed and why
//
// Previously, computing a fingerprint here required a per-repo
// `ResolveBranchSHA` GitHub API call (up to
// len(repos)*platform.Timeouts.RepoSHAResolutionTimeout of sequential
// network latency per spawn), which in turn required a usable creator
// GitHub token and a fresh CheckCreatorGuard recheck before ever touching
// it. §19.1 redefines domain/imagebuild.Fingerprint to hash each repo's
// normalized clone URL instead of a resolved SHA (one shared image per
// repo SET, not per exact SHA combination) -- so the fingerprint is now
// computable directly from plan.spec.SessionConfig.Repos alone, with ZERO
// network calls, for EVERY spawn, not just a lucky warm-hit one. This is
// §19.2's own explicit promise: "removes up to len(repos) *
// RepoSHAResolutionTimeout ... of sequential GitHub latency from every
// spawn attempt ... removes the 'creator has no GitHub token -> cold
// boot' fallback class entirely."
//
// This function therefore no longer calls ResolveBranchSHA, CheckCreatorGuard,
// or decryptCreatorGitHubToken AT ALL -- there is no creator/token
// dependency left in this file (confirmed via grep before removal: no
// other function in this file used them for any other reason). Those
// three remain exactly as they were for pushpr.go/contractdrift.go/
// scmcredentials.go's own, unrelated call sites (githubtoken.go).
//
// # §19.1/§19.2 boundary (§19.1 vs §19.9) -- documented design decision
//
// §19.1's own prose describes the builder resolving each repo's
// default-branch tip SHA "at claim time" -- §19.9's phasing note assigns
// that exact claim-time SHA resolution, and the new platform-level GitHub
// credential it needs (the freshness pump/background builder has no
// session/creator context to borrow a token from, unlike this spawn-time
// call site), to §19.2, not this one. §19.1's own resolved design
// decision, still exactly as implemented in THIS file: a cache MISS here
// creates a best-effort, URL-only pending row (repo_urls, no
// built_repo_shas yet) and does NOTHING further -- this spawn-path call
// site never resolves a per-repo SHA, on this spawn or any other; this
// spawn still uses the base image regardless, exactly as before.
// app/imagebuild.Builder.attempt is what actually
// performs that claim-time SHA resolution now (using
// platform.Config.GitHubImageBuildToken, §19.2), turning a brand-new
// pending row into a buildable one asynchronously, off this spawn's own
// path. The warm-HIT path below (a fingerprint that already has a
// 'ready' row) is unaffected by this boundary and always worked, zero
// network calls either way.
//
// # Why this runs where it runs (not "before assembling CreateSpec")
//
// dispatch.go's own top "# Sequencing" comment establishes, and an
// adversarial review already proved empirically, that a real network call
// must NEVER hold a Postgres transaction open (planDispatch's own transact
// commits the interim Spawning claim; the real provider call always
// happens strictly AFTER that commit, in executeSpawn/executeRestore).
// Resolving a fingerprint no longer needs network access (see above), but
// it does still do a Postgres read/best-effort upsert -- so, to keep this
// function's own placement rule simple and uniform regardless of what any
// future revision of it might need, it stays in the exact same
// "outside any transaction" zone executeSpawn/executeRestore's own
// provider call already occupies -- immediately before it, not inside
// planDispatch. plan.spec.Image is an ordinary Go struct field at this
// point (not yet written to any row -- the interim Spawning claim
// committed by planDispatch never persists Image at all), so mutating it
// here, after planDispatch returns and before executeSpawn/executeRestore
// reads it, is entirely safe: ports.CreateSpec.Validate (already called
// once, inside planFreshSpawn/planRestore) only checks Gen ==
// SessionConfig.Gen, never Image, so no re-validation is needed either.
//
// # "Never block a spawn" (§10 Phase 2, the one hard invariant)
//
// Every failure branch below -- no repos, an image_builds lookup error --
// is a plain, logged, early return: plan.spec.Image is simply LEFT at
// defaultBaseImage (planFreshSpawn/planRestore's own already-committed
// choice), exactly as if this function had never been called. Nothing
// here can fail or delay executeSpawn/executeRestore's own subsequent
// call, and -- as of this Step -- nothing here can add latency either:
// every code path is a plain in-memory hash plus at most one Postgres
// round trip.

// # Repo-access gate (audit fix, "warm-boot image access control", HIGH)
//
// §19.1's own rewrite above (deliberately) dropped every creator/token
// dependency this function used to have -- which also, as an unintended
// side effect, dropped the ONLY thing that had ever gated which repos a
// given user's sandbox could contain: previously, a warm hit required
// resolving each repo's SHA under the CREATOR's own GitHub token, which
// implicitly 404/403'd for a repo that creator could not read. Once the
// fingerprint became URL-only and the background builder (§19.2) started
// resolving/building under a platform-level credential with no notion of
// which user asked, an authenticated member with ZERO read access to a
// private repo could name it in a session's repo list, cause a platform-
// credentialed build of it, and warm-boot into a sandbox containing its
// complete clone -- see this Step's own PR description / the audit finding
// this batch closes for the full repro.
//
// resolveAndSetImage now gets ONE new early-return check, right at its
// top, before the image_builds lookup below even runs: repoAccessAllowedForSpawn
// (below). If it doesn't pass, this function returns immediately, exactly
// like every other failure branch already here -- plan.spec.Image simply
// stays at defaultBaseImage. Because both the miss/upsert branch and the
// ready/warm-hit branch sit strictly AFTER this gate, NEITHER is reachable
// without a positive verdict here: the two decision points the audit found
// separately vulnerable (minting a pending row for an unreadable repo, and
// warm-hitting a row someone else's session caused to be built) are closed
// by construction, with one check, not two.
//
// # Where this gate lives, and why not session-creation instead
//
// The gate lives here -- re-run on EVERY spawn/restore, not just once at
// session creation -- because resolveAndSetImage itself runs on every
// spawn AND restore of an already-existing sandbox (handleEnsureDispatched,
// dispatch.go), which can happen long after creation (a respawn, a resume
// after Stopped). A session-creation-only check would leave a user whose
// repo access was revoked AFTER creating a session still served the baked
// private clone on every later respawn -- the exact staleness class
// CheckCreatorGuard (githubtoken.go) already exists to close for every
// other creator-token consumer in this codebase. Session creation
// (httpapi.CreateSession) is also a synchronous, Postgres-only path by
// design (this file's own "why this runs where it runs" comment above, and
// that handler's own doc comment) -- shared directly by Slack/Linear/
// GitHub webhook ingress under their own ack-time budgets -- so a
// synchronous outbound GitHub call has no safe home there; this hook point
// is the one place in this actor a network-bound check can safely live.
//
// # Fail-closed vs. fail-open -- which way this gate fails, and why
//
// This is a security gate, not a convenience feature, so unlike
// decryptCreatorGitHubToken's OTHER callers (pushpr.go's PR-attribution
// best-effort, contractdrift.go's opportunistic drift check -- both of
// which simply skip a nice-to-have on any absence), every absence or
// failure here is a DENY, not a silent allow:
//   - no created_by user at all (an automation/bot-created session):
//     denied. A NULL creator has no token to check access with in the
//     first place -- this is unavoidable, not a design choice: there is
//     no per-user credential to run CheckRepoAccess under. Audit-fix
//     correction (correctness-availability, finding #6): an EARLIER
//     version of this comment characterized every NULL-creator session as
//     targeting "a platform-configured default repo, never attacker-
//     influenced" -- that is true for Slack/Linear ingress, but NOT for
//     GitHub PR-mention-triggered review sessions with an unresolved/
//     bot-attributed commenter, which internal/adapters/inbound/github/
//     coalesce.go's own doc comment states is "by far the common case
//     today": those sessions run against that PR's own actual repo, which
//     is arbitrary (and often private), not a fixed platform default. This
//     gate still denies that case -- correctly, since there genuinely is
//     no creator token to check with -- but the REAL cost is a large,
//     ongoing, latency-sensitive slice of automation traffic permanently
//     never benefiting from warm-boot, not merely a rare, inert edge case.
//     Accepted as-is for this batch (widening this gate to check access
//     under a platform-level bot credential instead, the way PostIssueComment/
//     GetPullRequest already do for THEIR OWN calls, is a real option for a
//     follow-up but is its own design decision -- e.g. whether "the bot
//     can read this PR's repo" is an acceptable proxy for "warm-boot is
//     safe here" -- deliberately not made unilaterally inside this fix-up
//     batch).
//   - a real creator now Disabled or role==viewer (CheckCreatorGuard), or
//     no linked GitHub identity/no stored token/a decrypt failure
//     (decryptCreatorGitHubToken): denied, exactly like every other
//     "nothing usable to check with" case.
//   - a real, enabled, non-viewer creator whose token genuinely cannot
//     read the repo (CheckRepoAccess returns false, nil): denied -- the
//     actual attack case this batch exists to close. This degrades to
//     exactly the pre-existing property: cold boot on the base image, and
//     that same user's own credentials still fail naturally at CloneAll
//     (fatal boot for the primary repo) if they try anyway.
//   - CheckRepoAccess itself fails to even answer the question (network
//     timeout, GitHub 5xx, any transport error): ALSO denied for this one
//     spawn -- but this is the one outcome ports.SourceControl.
//     CheckRepoAccess's own doc comment is explicit callers must not treat
//     as a definitive, cacheable "no" (see repoAccessAllowedForSpawn's own
//     handling below): it is logged as an indeterminate SCM-check failure,
//     distinct from an explicit denial (an operator must be able to tell
//     "this user was denied access to this repo" apart from "the SCM API
//     was down" -- they are different failures), and NEVER cached, so the
//     very next spawn attempt re-checks live rather than freezing a
//     transient outage into a stale deny for this cache entry's whole TTL.
//     This still satisfies §10 Phase 2's own "never block a session"
//     invariant: the spawn itself is never held up or failed by this --
//     it just falls back to the base image for this one attempt,
//     identically to a genuine image_builds lookup error a few lines
//     below.
//
// # Caching
//
// See repoaccesscache.go's own top comment for the full design: a genuine
// (non-error) verdict, positive or negative, is cached per (session
// creator, normalized repo URL) for platform.Timeouts.RepoAccessCacheTTL,
// shared across every Actor via the Registry (like a.stores/a.sourceControl
// already are) -- after the first check per (user, repo), this reduces to
// zero network calls for the life of the TTL, preserving §19.1/§19.2's own
// "zero network calls on the steady-state hot path" property for the
// common case of a user who DOES have access.

// # Persisted decision provenance
//
// Every return path below -- including the "never block a spawn" ones
// this file's own comment above already describes -- reports exactly one
// internal/domain/imagedecision.Reason value to
// persistImageDecisionBestEffort (below): a real warm-image selection
// (imagedecision.ReasonSelected) or the specific closed-vocabulary reason
// it fell back to defaultBaseImage instead. This closes the gap a log
// line alone cannot: once the pod that emitted it recycles, a log is
// gone, and there was previously no way to tell a DELIBERATE cold boot
// (no cached image yet) apart from a BROKEN warm path (repo access
// denied, a lookup failure, a data-integrity anomaly) after the fact, or
// to count either across sessions.
//
// The persisted record reuses two surfaces that already exist, rather
// than adding one: sandboxes.image_decision_reason/
// image_decision_fingerprint (migrations/000139_sandboxes_image_decision.
// up.sql, mirroring agent_version/image_digest's own identical "per-gen
// fact on the sandboxes row" precedent, migrations/000120_sandboxes_boot_
// fingerprint.up.sql) for the CURRENT gen's own latest decision, and a new
// "image_decision" event type appended to the session's own existing
// append-only event log (a.appendEvent, exactly like timerfired.go's own
// "warning"/reason:"inactivity_extended" precedent) for the full history
// across every spawn/restore. The events copy needs no REST/DTO/route
// change of its own to become readable: §6.2's client-WS subscribe/
// fetch_history replay and §6.3's existing GET .../events REST route
// (httpapi.ListEvents) already serve every row in that table,
// unconditionally, to any authorized reader -- this is precisely what
// "reusing the existing collection and event log rather than adding a
// surface" means in practice.
//
// # Why this is mechanically hard to bypass, not merely a convention
//
// decideImage and repoAccessAllowedForSpawn (below) both return their
// imagedecision.Reason as a plain, UNNAMED positional return value --
// deliberately, not a named one. Go refuses to compile a bare `return`
// inside a function with unnamed return types (every exit must spell out
// every value explicitly), so a future early return added to either
// function cannot compile without supplying SOME Reason at that exact
// call site -- the compiler is the enforcement, not a review checklist.
// A persisted value outside this package's closed vocabulary is
// additionally rejected by Postgres itself
// (sandboxes.image_decision_reason is a real ENUM column, not a
// CHECK-constrained TEXT one) -- see imagedecision's own package doc
// comment for the full two-part argument.
//
// # What this does NOT answer: which dependency manifest changed
//
// imagebuild.Fingerprint's own three inputs (base, each repo's
// (name, normalized URL), runtimeVersion) are repo IDENTITY, never repo
// CONTENT -- neither it nor the image_builds row it keys (which persists
// exactly those same inputs plus built_repo_shas/built_at) has ever had
// any dependency-manifest awareness. This spawn-path call site resolves
// no per-repo SHA at all any more (see this file's own top comment), so
// it cannot identify the CURRENT commit, let alone diff its dependency
// manifests against whatever the image was built from. The only place in
// this codebase that ever computes a manifest-level answer is
// internal/sandboxagent/boot's own evaluateDependencySkip/
// ComputeDependencyManifestDigest -- boot-TIME, inside the ephemeral
// sandbox, comparing the checked-out tree against the image's baked
// dependency_manifest_digests (§19.6) -- and even that is REPO
// granularity only (one folded digest per repo across every recognized
// lockfile), never per-file. That verdict is not sent over the sandbox-ws
// wire today (boot.OnHookRerunTiming's own callback signature carries no
// such field) and so never reaches Postgres from anywhere. Extending that
// wire contract is a real, separate, larger change (contracts/sandbox-ws,
// sandbox-agent's own hook-rerun-timing emission, and this package's own
// wire-event handling) with its own blast radius -- not folded in here.
// What this Step's own decision record CAN honestly carry, on
// ReasonSelected only, is the warm-hit row's own built_repo_shas (repo
// COMMIT identity, persisted already, zero extra I/O) -- see
// persistImageDecisionBestEffort's own doc comment for why that is
// explicitly NOT the same claim as "which manifest changed".

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/imagebuild"
	"github.com/narvidev/narvi/internal/domain/imagedecision"
	"github.com/narvidev/narvi/internal/domain/reposource"
)

// resolveAndSetImage implements this file's own design (see top comment).
// plan.spec.Image is mutated in place to a real image_ref ONLY when this
// exact spawn's own fingerprint already has a 'ready' image_builds row;
// every other outcome (including any error) leaves it untouched.
//
// On finding a real ready image for a NON-restore plan, this also upgrades
// plan.spec.SessionConfig.BootMode from Fresh to RepoImage
// (sessionconfig.SessionConfigBootModeRepoImage) -- see this package's own
// design-decision note in dispatch.go's doc comment / this Step's PR
// description for why: internal/domain/sandboxboot.EvaluateHook's own hook
// policy (§6.4) treats repo_image as "setup.sh already ran at build time
// and does not run again" -- reporting Fresh for a real prebuilt-image boot
// would make sandbox-agent redundantly re-run setup.sh at every boot
// regardless, defeating the entire point of image prebuilding. A restore
// plan's BootMode is deliberately left at SnapshotRestore regardless (a
// restore's own identity -- resuming from a snapshot, not a repo-image
// build -- does not change just because a matching built image also
// happens to exist for the same fingerprint).
func (a *Actor) resolveAndSetImage(ctx context.Context, plan *spawnPlan) {
	reason, fingerprint, builtRepoShas := a.decideImage(ctx, plan)
	a.persistImageDecisionBestEffort(ctx, reason, fingerprint, builtRepoShas)
}

// decideImage is resolveAndSetImage's own decision logic, split out so its
// outcome -- not just its side effect on plan.spec.Image -- has somewhere
// to go. plan.spec.Image is mutated in place to a real image_ref ONLY when
// this exact spawn's own fingerprint already has a 'ready' image_builds
// row; every other outcome (including any error) leaves it untouched,
// exactly as resolveAndSetImage always has.
//
// Returns the imagedecision.Reason this decision landed on, the
// fingerprint it was computed against ("" only for ReasonNoRepos, the one
// outcome with nothing to fingerprint at all), and -- on ReasonSelected
// only -- the warm-hit row's own raw built_repo_shas JSON (nil otherwise).
// The three unnamed return types are load-bearing: see this file's own top
// "# Why this is mechanically hard to bypass" comment.
func (a *Actor) decideImage(ctx context.Context, plan *spawnPlan) (imagedecision.Reason, string, []byte) {
	repos := plan.spec.SessionConfig.Repos
	if len(repos) == 0 {
		return imagedecision.ReasonNoRepos, "", nil // nothing to fingerprint; stays on defaultBaseImage
	}

	repoURLs := make(map[string]string, len(repos))
	for _, r := range repos {
		repoURLs[r.Name] = imagebuild.NormalizeRepoURL(r.Url)
	}

	// Computed BEFORE the repo-access gate below -- a harmless reordering
	// relative to this function's pre-persistence shape: Fingerprint is a
	// pure hash with no side effects and no I/O, so computing it slightly
	// earlier changes nothing observable except letting a repo-access
	// DENIAL's own persisted decision still carry a fingerprint an
	// operator can cross-reference against image_builds, rather than
	// leaving it blank for every denial.
	fingerprint := imagebuild.Fingerprint(defaultBaseImage, repoURLs, a.openCodeRuntimeVersion)

	// Audit fix ("warm-boot image access control", HIGH): gate BEFORE
	// either branch below can run -- see this file's own top "# Repo-access
	// gate" comment for the full design. Neither the miss/upsert branch
	// nor the ready/warm-hit branch is reachable without a positive
	// verdict here.
	if allowed, reason := a.repoAccessAllowedForSpawn(ctx, plan, repoURLs); !allowed {
		return reason, fingerprint, nil
	}

	row, err := a.stores.imageBuild.Get(ctx, fingerprint)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			a.logger.Error("sessionactor: resolve image: look up image_builds failed; falling back to base image",
				"fingerprint", fingerprint, "error", err)
			return imagedecision.ReasonImageBuildLookupFailed, fingerprint, nil
		}
		// No row yet for this fingerprint: best-effort create a pending
		// tracking row carrying the URL-keyed fingerprint inputs, so
		// internal/app/imagebuild's own background loop has a record of
		// this repo set (see this file's own top comment for the §19.1/§19.2
		// boundary this best-effort row sits on: no SHA resolution
		// ever happens on this spawn path -- app/imagebuild.Builder's
		// claim-time resolution, §19.2, is what later resolves and
		// builds this row asynchronously). This spawn still uses the base
		// image regardless of whether the upsert itself succeeds.
		return a.upsertPendingImageBuildBestEffort(ctx, fingerprint, repoURLs), fingerprint, nil
	}

	if row.Status == sqlcgen.ImageBuildStatusReady {
		if row.ImageRef == nil || *row.ImageRef == "" {
			// Data-integrity anomaly: a 'ready' row should always carry a
			// real image_ref. Kept distinct from ReasonImageBuildNotReady
			// below so this can never silently masquerade as an ordinary
			// still-building state.
			a.logger.Error("sessionactor: resolve image: image_builds row is ready but has no image_ref; falling back to base image",
				"fingerprint", fingerprint)
			return imagedecision.ReasonImageBuildReadyRowMissingRef, fingerprint, nil
		}
		plan.spec.Image = *row.ImageRef
		if !plan.restore {
			plan.spec.SessionConfig.BootMode = sessionconfig.SessionConfigBootModeRepoImage
		}
		return imagedecision.ReasonSelected, fingerprint, row.BuiltRepoShas
	}

	// status is pending/building/failed-but-not-yet-due: the background
	// builder's own poll query (internal/app/imagebuild's ListDueImageBuilds)
	// already matches a failed row whose next_retry_at has elapsed
	// directly, regardless of any spawn-side activity -- so nothing
	// further is written here for that case; this spawn simply falls back
	// to the base image, exactly as if no image_builds row existed at
	// all. A row already existing means the earlier "no row" branch's own
	// upsert (ON CONFLICT DO NOTHING) would be a pure no-op anyway, so it
	// is not attempted again here.
	return imagedecision.ReasonImageBuildNotReady, fingerprint, nil
}

// upsertPendingImageBuildBestEffort best-effort inserts a fresh 'pending'
// image_builds row for fingerprint, carrying the raw (base, repoURLs,
// runtimeVersion) inputs -- the fingerprint's own URL-keyed inputs
// (migrations/000039_image_builds_shared_fingerprint.up.sql's own doc
// comment explains why those raw inputs are persisted, not just the
// fingerprint hash). Any failure (marshal, DB error/timeout) is logged
// and reported back via its own distinct Reason -- this remains explicitly
// best-effort, never allowed to affect the calling spawn, but the CALLER
// now needs to know which of the three outcomes (marshal failed, upsert
// failed, or a real/no-op pending row) actually happened, to persist the
// right one.
func (a *Actor) upsertPendingImageBuildBestEffort(ctx context.Context, fingerprint string, repoURLs map[string]string) imagedecision.Reason {
	raw, err := json.Marshal(repoURLs)
	if err != nil {
		a.logger.Error("sessionactor: resolve image: marshal repo urls for image_builds upsert failed",
			"fingerprint", fingerprint, "error", err)
		return imagedecision.ReasonImageBuildTrackingMarshalFailed
	}

	if err := a.stores.imageBuild.UpsertPending(ctx, sqlcgen.UpsertPendingImageBuildParams{
		Fingerprint:    fingerprint,
		Base:           defaultBaseImage,
		RepoUrls:       raw,
		RuntimeVersion: a.openCodeRuntimeVersion,
	}); err != nil {
		a.logger.Error("sessionactor: resolve image: upsert pending image_builds row failed",
			"fingerprint", fingerprint, "error", err)
		return imagedecision.ReasonImageBuildTrackingUpsertFailed
	}
	return imagedecision.ReasonImageBuildPending
}

// persistImageDecisionBestEffort is decideImage's own caller-side
// persistence step (see this file's own top "# Persisted decision
// provenance" comment for the full design): writes reason/fingerprint
// onto this session's CURRENT sandboxes row (the "collection" half --
// cheap to read without scanning the event log, reset to NULL on every
// respawn, queries/sandboxes.sql's own UpsertSandboxForSpawn) and appends
// an "image_decision" event carrying the identical reason plus
// fingerprint/built_repo_shas (the "event log" half -- full history
// across every spawn/restore, already reachable via §6.2/§6.3's existing
// replay/REST surface, no new route needed).
//
// built_repo_shas, when present, is the SAME image_builds.built_repo_shas
// a warm hit just selected -- repo-COMMIT granularity, carried here so
// consecutive decisions for this session can be diffed to see which
// repo's built commit moved between one warm boot and the next. This is
// NOT "which dependency manifest changed" -- see this file's own top
// comment for why that question has no answer available at this call
// site, and why extending the wire contract to carry the boot-time
// answer is deliberately not folded into this fix.
//
// Both writes happen inside ONE a.transact call: "transact is the ONLY
// way this package writes session/turn/sandbox state" (actor.go's own doc
// comment) applies to sandboxes exactly as much as to sessions/turns, and
// bundling both writes atomically means an invalid Reason can never reach
// the events log either -- sandboxes.image_decision_reason's own Postgres
// ENUM type rejects it outright, the whole transact rolls back, and
// NOTHING is persisted, rather than the events copy alone silently
// surviving with a value the enum-typed column would have refused.
//
// Best-effort, exactly like every other write in this file (§10 Phase 2,
// "never block a spawn"): a transact failure here -- including
// ErrStaleEpoch, a real possibility since this call runs strictly after
// planDispatch's own transact has already committed and returned -- is
// logged and never propagated; the spawn this decision was computed for
// is already underway by the time this runs and must never be delayed or
// failed by a provenance write.
func (a *Actor) persistImageDecisionBestEffort(ctx context.Context, reason imagedecision.Reason, fingerprint string, builtRepoShas []byte) {
	payload := map[string]any{"reason": string(reason)}
	if fingerprint != "" {
		payload["fingerprint"] = fingerprint
	}
	if len(builtRepoShas) > 0 {
		payload["built_repo_shas"] = json.RawMessage(builtRepoShas)
	}

	reasonValue := sqlcgen.ImageDecisionReason(reason)
	var fingerprintParam *string
	if fingerprint != "" {
		fingerprintParam = &fingerprint
	}

	err := a.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := a.stores.sandbox.WithTx(tx).UpdateImageDecision(ctx, sqlcgen.UpdateSandboxImageDecisionParams{
			SessionID:                a.sessionID,
			ImageDecisionReason:      &reasonValue,
			ImageDecisionFingerprint: fingerprintParam,
		}); err != nil {
			return fmt.Errorf("sessionactor: update sandbox image decision: %w", err)
		}
		return a.appendEvent(ctx, tx, "image_decision", payload)
	})
	if err != nil {
		a.logger.Warn("sessionactor: resolve image: persist image decision failed (best-effort, spawn unaffected)",
			"reason", string(reason), "fingerprint", fingerprint, "error", err)
	}
}

// githubRepoHost/repoURLHostAllowed used to live here -- audit-remediation
// batch B3 moved the const to ports.GitHubSourceControlHost and the check
// itself to reposource.CheckRepoHost (a THIRD independent host-checking
// implementation, added for imagebuild.Builder.resolveRepoSHAs, would
// have been its own version of the exact finding this const/check
// originally closed -- see ports.GitHubSourceControlHost's own doc
// comment for the full rationale). This gate now calls
// reposource.CheckRepoHost(repoURL, ports.SupportedSourceControlHosts()...)
// directly, below, in the exact same "before this gate ever derives an
// owner/repo or spends a cache lookup/network call" position --
// audit-remediation batch B3 round 2 (finding #7) further routes both this
// call and imagebuild.Builder.resolveRepoSHAs' own identical call through
// ports.SupportedSourceControlHosts() rather than each naming
// ports.GitHubSourceControlHost directly, so the two allowlists can no
// longer drift apart independently -- see that function's own doc comment.

// repoAccessAllowedForSpawn implements this file's own "# Repo-access
// gate" design (see this file's own top comment for the complete
// rationale): true only when plan's own session creator can, RIGHT NOW,
// read every repo in repoURLs. Every other outcome -- no creator, a
// disabled/viewer creator, no usable token, an unsupported repo-URL host,
// a definitive per-repo denial, or even an indeterminate SCM-check
// failure -- returns false, per that same top comment's fail-closed
// reasoning.
//
// The second return value is the exact imagedecision.Reason this verdict
// landed on -- meaningful only when the first is false; callers must
// ignore it (imagedecision.ReasonNone, never persisted) when allowed is
// true. See this file's own top "# Why this is mechanically hard to
// bypass" comment for why both are unnamed positional returns rather than
// a struct or named returns.
//
// repoURLs is resolveAndSetImage's own already-normalized map (repo name
// -> imagebuild.NormalizeRepoURL(r.Url)) -- reused here rather than
// re-deriving it, and also what this function's own cache keys off of, so
// a differently-spelled-but-equivalent URL shares one cache entry (and one
// CheckRepoAccess call) with whatever spelling was checked first, exactly
// like Fingerprint already treats them as the same repo.
func (a *Actor) repoAccessAllowedForSpawn(ctx context.Context, plan *spawnPlan, repoURLs map[string]string) (bool, imagedecision.Reason) {
	// createdBy.Valid is checked HERE, before ever calling CheckCreatorGuard
	// -- CheckCreatorGuard's own Allowed:true for an invalid createdBy means
	// "nothing for THIS guard to check", not "access verified" (its own doc
	// comment). Conflating the two would silently reopen this gate for
	// every automation/bot-created session (sessions.created_by IS NULL) --
	// see this file's own top comment for why that is an acceptable speed
	// regression, not a compensating control to lean on.
	if !plan.createdBy.Valid {
		a.logger.Warn("sessionactor: resolve image: repo-access gate: session has no created_by user; automation-created sessions never warm-boot (denying, not a bug -- see imageresolve.go's own top comment)")
		return false, imagedecision.ReasonRepoAccessNoCreator
	}

	// CheckCreatorGuard ALWAYS runs here, unconditionally, on EVERY spawn
	// regardless of what repoAccessCache below already knows -- this is
	// the one recheck (audit-remediation finding #7) that must never be
	// skipped just because every repo happens to be cache-hit:
	// repoAccessCache only ever remembers a REPO-read verdict, never the
	// creator's own current enabled/role status, so a creator disabled or
	// demoted to viewer AFTER their repo access was cached must still be
	// caught here, on this very next spawn (§13.3 viewer-guard parity --
	// this file's own top comment). Skipping this call on a cache hit
	// would silently reopen exactly the staleness hole this whole gate
	// exists to close.
	if v := CheckCreatorGuard(ctx, a.stores.user, plan.createdBy); !v.Allowed {
		switch {
		case v.Err != nil:
			a.logger.Error("sessionactor: resolve image: repo-access gate: get session creator failed; denying (fail-closed)",
				"user_id", plan.createdBy.String(), "error", v.Err)
			return false, imagedecision.ReasonRepoAccessCreatorLookupFailed
		case v.Disabled:
			a.logger.Warn("sessionactor: resolve image: repo-access gate: session creator is now disabled; denying warm-boot (§13.3 viewer guard parity)",
				"user_id", plan.createdBy.String())
			return false, imagedecision.ReasonRepoAccessCreatorDisabled
		case v.Viewer:
			a.logger.Warn("sessionactor: resolve image: repo-access gate: session creator is now a viewer; denying warm-boot (§13.3 viewer guard parity)",
				"user_id", plan.createdBy.String())
			return false, imagedecision.ReasonRepoAccessCreatorViewer
		}
		// Defensive, should be unreachable: CreatorGuardVerdict's own doc
		// comment states exactly one of Allowed/Err/Disabled/Viewer is
		// meaningful per verdict -- if none of the three cases above
		// matched, something upstream returned a shape this switch does
		// not recognize. Denied fail-closed regardless (never a silent
		// fall-through as if this whole check had passed) -- mirrors
		// EvaluateHook's own "unrecognized value is a programming error,
		// not a data error" convention.
		a.logger.Error("sessionactor: resolve image: repo-access gate: creator guard verdict has none of Err/Disabled/Viewer set; denying defensively (should be unreachable)",
			"user_id", plan.createdBy.String())
		return false, imagedecision.ReasonRepoAccessCreatorGuardUnknown
	}

	// Audit-remediation fix (correctness-availability, finding #7): unlike
	// CheckCreatorGuard above, the creator's own GitHub token is decrypted
	// LAZILY, on first actual need, NOT unconditionally here -- getToken
	// (below) resolves it (one identity-store Postgres read plus one AES
	// decrypt) at most once per call to this function, and only if at
	// least one repo below turns out to be a genuine cache MISS. On a
	// spawn/restore whose every repo already has a cached verdict, this
	// means neither the identity lookup nor the decrypt ever happens at
	// all -- the decrypted token would never have been used anyway (every
	// repo already answered from repoAccessCache) -- restoring the "zero
	// network calls, minimal Postgres reads" property of the all-cache-hit
	// hot path §19.1/§19.2 built this gate to sit in front of, without
	// weakening CheckCreatorGuard's own always-fresh recheck above.
	var (
		token        string
		tokenOK      bool
		tokenChecked bool
	)
	getToken := func() (string, bool) {
		if !tokenChecked {
			token, tokenOK = a.decryptCreatorGitHubToken(ctx, plan.createdBy)
			tokenChecked = true
		}
		return token, tokenOK
	}

	userID := plan.createdBy.String()
	now := time.Now()

	for name, repoURL := range repoURLs {
		// Audit hardening (security-adversarial, finding #2): checked
		// BEFORE this repo's owner/repo is even derived, or a cache lookup
		// spent on it -- see ports.GitHubSourceControlHost's own doc
		// comment.
		if err := reposource.CheckRepoHost(repoURL, ports.SupportedSourceControlHosts()...); err != nil {
			a.logger.Warn("sessionactor: resolve image: repo-access gate: repo url does not name a supported source-control host; denying warm-boot",
				"repo", name, "url", repoURL, "error", err)
			return false, imagedecision.ReasonRepoAccessUnsupportedHost
		}

		owner, repoName, err := reposource.ParseOwnerRepo(repoURL)
		if err != nil {
			a.logger.Warn("sessionactor: resolve image: repo-access gate: parse owner/repo from clone url failed; denying warm-boot",
				"repo", name, "error", err)
			return false, imagedecision.ReasonRepoAccessUnparseableURL
		}
		repoLabel := owner + "/" + repoName

		if a.repoAccessCache != nil {
			if allowed, cached := a.repoAccessCache.get(userID, repoURL, now); cached {
				if !allowed {
					a.logger.Warn("sessionactor: resolve image: repo-access gate: cached verdict is deny; denying warm-boot",
						"user_id", userID, "repo", repoLabel)
					return false, imagedecision.ReasonRepoAccessCachedDeny
				}
				continue
			}

			// Cache MISS, about to (maybe) make a real network call --
			// audit-remediation addition (correctness-availability, finding
			// #5): if CheckRepoAccess has failed indeterminately
			// repeatedly enough, recently enough, skip the network call
			// entirely and deny THIS repo (and therefore this spawn)
			// straight away -- still fail-closed, identical outcome to an
			// individual indeterminate failure below, just without paying
			// for the round trip again during a sustained outage. See
			// repoAccessCache's own breakerOpen/"# Circuit breaker" doc
			// comment for the full design.
			if a.repoAccessCache.breakerOpen(now, a.timeouts.RepoAccessCheckBreakerWindow) {
				a.logger.Warn("sessionactor: resolve image: repo-access gate: CheckRepoAccess circuit breaker open (repeated indeterminate SCM failures); denying warm-boot without a network call this spawn",
					"user_id", userID, "repo", repoLabel)
				return false, imagedecision.ReasonRepoAccessCircuitBreakerOpen
			}
		}

		tok, ok := getToken()
		if !ok {
			// Already logged (Warn or Error, as appropriate) by
			// decryptCreatorGitHubToken itself.
			return false, imagedecision.ReasonRepoAccessNoToken
		}

		if a.sourceControl == nil {
			// Defensive: mirrors resolveAndSetImage's/checkContractDrift's
			// own nil-provider guard exactly -- some tests, and any future
			// caller genuinely without one, must not panic here.
			a.logger.Warn("sessionactor: resolve image: repo-access gate: no SourceControl configured; denying warm-boot")
			return false, imagedecision.ReasonRepoAccessNoSourceControl
		}

		checkCtx, cancel := context.WithTimeout(ctx, a.timeouts.RepoAccessCheckTimeout)
		allowed, err := a.sourceControl.CheckRepoAccess(checkCtx, ports.CheckRepoAccessSpec{
			Owner: owner, Repo: repoName, Token: tok,
		})
		cancel()
		if err != nil {
			// Indeterminate: the SCM check itself failed (network/timeout/
			// 5xx, or a rate-limited 403 -- see githubapi.
			// isRateLimitedResponse), never a definitive "no" (ports.
			// SourceControl.CheckRepoAccess's own doc comment). Logged
			// distinctly from an explicit denial below -- an operator must
			// be able to tell "this user was denied access to this repo"
			// apart from "the SCM API was down" -- and deliberately NEVER
			// cached (this file's own top comment): the next spawn attempt
			// re-checks live rather than freezing this transient outage
			// into a stale deny for RepoAccessCacheTTL.
			if a.repoAccessCache != nil {
				a.repoAccessCache.recordCheckFailure(now)
			}
			a.logger.Error("sessionactor: resolve image: repo-access gate: CheckRepoAccess call failed; SCM check indeterminate, denying only THIS spawn's warm-boot (not cached, not a permanent deny)",
				"user_id", userID, "repo", repoLabel, "error", err)
			return false, imagedecision.ReasonRepoAccessCheckIndeterminate
		}

		if a.repoAccessCache != nil {
			a.repoAccessCache.recordCheckSuccess()
			a.repoAccessCache.set(userID, repoURL, allowed, now, a.timeouts.RepoAccessCacheTTL)
		}

		if !allowed {
			a.logger.Warn("sessionactor: resolve image: repo-access gate: session creator cannot read this repo; denying warm-boot (audit fix: warm-boot image access control)",
				"user_id", userID, "repo", repoLabel)
			return false, imagedecision.ReasonRepoAccessDenied
		}
	}

	return true, imagedecision.ReasonNone
}
