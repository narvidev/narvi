package reviewverdict

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/app/egressmode"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
	"github.com/narvidev/narvi/internal/platform"
)

// RecordConfirmed idempotently records that (repoFullName, prNumber,
// headSHA)'s own auto-approved verdict was actually MERGED -- the
// engine's own judgment stood (§21.2 stage 2's own "confirmed" outcome).
// Called from BOTH merge-completion paths: httpapi.MergePullRequest (a
// human's 1-click confirm while the repo's auto-merge toggle is off) and
// internal/app/automerge's own worker (once armed). Best-effort: a
// failure here never blocks or unwinds an ALREADY-SUCCEEDED merge --
// mirrors httpapi.MergePullRequest's own existing "the merge already
// happened, a logging failure must never claim otherwise" posture for
// its audit-log write, applied here to this SAME class of post-merge
// bookkeeping.
//
// This doc comment (and migration
// 000070_auto_approval_outcomes.up.sql's own identical claim) previously
// described httpapi.MergePullRequest as ALREADY calling this -- it did
// not; the ONLY real caller was the armed auto-merge worker. During the
// ENTIRE toggle-off calibration window -- the exact period §21.2 says
// this metric exists to inform an admin's own decision to arm auto-merge
// -- only 'overridden' rows were ever written, pinning every unarmed
// repo's own contradiction rate at 100% or "not yet computed". Both
// claims are now accurate: httpapi.MergePullRequest (decisioninbox.go)
// calls this immediately after a successful merge.
//
// MUST NEVER be called for a merge whose own RevalidateForMerge/
// RevalidateForAutoMerge (internal/app/decisioninbox/revalidate.go)
// reported viaAcceptance=true (finding F1, adversarial review) -- both
// real callers gate on that flag and call RecordAcceptedOverride below
// instead in that case. This function's own name and doc comment ARE the
// contract "confirmed" analytics readers rely on: recording it for a
// merge the engine itself refused, and only proceeded past because a
// human's acceptance waived that refusal, would silently misrepresent an
// override as the engine having been right -- exactly the harm §21.1b
// names and the contradiction-rate signal §21.2 is calibrated on.
func RecordConfirmed(ctx context.Context, deps Deps, repoFullName string, prNumber int32, headSHA string) {
	recordOutcome(ctx, deps, repoFullName, prNumber, headSHA, reviewverdict.OutcomeConfirmed)
}

// RecordAcceptedOverride idempotently records that (repoFullName,
// prNumber, headSHA) merged ONLY because an applicable
// review_verdict_acceptances row waived the engine's own refusal
// (finding F1, adversarial review) -- reviewverdict.
// OutcomeAcceptedOverride's own doc comment. Called from the SAME two
// merge-completion call sites as RecordConfirmed, in its place, whenever
// RevalidateForMerge/RevalidateForAutoMerge reported viaAcceptance=true
// for this merge. Best-effort, mirroring RecordConfirmed above.
func RecordAcceptedOverride(ctx context.Context, deps Deps, repoFullName string, prNumber int32, headSHA string) {
	recordOutcome(ctx, deps, repoFullName, prNumber, headSHA, reviewverdict.OutcomeAcceptedOverride)
}

// RecordOverridden idempotently records that (repoFullName, prNumber,
// headSHA)'s own auto-approved verdict was CONTESTED by a human before
// any merge happened (GitHub's own HasChangesRequested became true, or a
// review:needs-human label was applied) -- §21.2 stage 2's own
// "overridden" outcome. Called from internal/app/decisioninbox's own
// buildPROpenItem, which already computes every fact this needs (the
// latest verdict's own Shippable, HasChangesRequested, the needs-human
// label) at zero extra cost -- see that call site for the exact
// condition. Best-effort, mirroring RecordConfirmed above.
func RecordOverridden(ctx context.Context, deps Deps, repoFullName string, prNumber int32, headSHA string) {
	recordOutcome(ctx, deps, repoFullName, prNumber, headSHA, reviewverdict.OutcomeOverridden)
}

func recordOutcome(ctx context.Context, deps Deps, repoFullName string, prNumber int32, headSHA string, outcome reviewverdict.Outcome) {
	if headSHA == "" {
		return
	}
	// §30.7's calibration-read exclusion, stamped at write time for the
	// same reason §30.8 stamps everything else at write time: the mode
	// that held when the observation was MADE is the one that decides
	// whether it may calibrate, and re-reading later would let a
	// promotion retroactively admit verdicts nobody ever saw.
	//
	// The outcome is still recorded. The contradiction rate is the
	// instrument that justifies arming auto-merge for real, and moving it
	// with shadow-era observations is the falsification §30.7 rules out --
	// but an operator's ledger still needs to see that the observation
	// happened.
	suppressedInShadow := egressmode.Resolve(ctx, egressmode.Deps{
		RepoSettings:   deps.RepoSettings,
		PlatformShadow: deps.PlatformShadow,
	}, repoFullName).Suppressed()

	if err := deps.AutoApprovalOutcomes.Record(ctx, repoFullName, prNumber, headSHA, string(outcome), suppressedInShadow); err != nil {
		platform.Logger(ctx).Warn("reviewverdict: record auto-approval outcome failed", "error", err, "repo_full_name", repoFullName, "pr_number", prNumber, "outcome", string(outcome))
	}
}

// ContradictionRate computes repoFullName's own §21.2 stage 2
// calibration metric, bounded to deps.Timeouts.ReviewVerdictAnalyticsWindow
// -- see internal/domain/reviewverdict.ContradictionRate's own doc
// comment for the "not yet computed" sentinel (ok=false) this forwards
// verbatim.
func ContradictionRate(ctx context.Context, deps Deps, repoFullName string, now time.Time) (rate float64, contested, total int, ok bool, err error) {
	since := now.Add(-deps.Timeouts.ReviewVerdictAnalyticsWindow)
	totalCount, contestedCount, err := deps.AutoApprovalOutcomes.CountInWindow(ctx, repoFullName, pgtype.Timestamptz{Time: since, Valid: true})
	if err != nil {
		return 0, 0, 0, false, err
	}
	rate, ok = reviewverdict.ContradictionRate(int(totalCount), int(contestedCount))
	return rate, int(contestedCount), int(totalCount), ok, nil
}
