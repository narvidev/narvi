package releasereview

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/reviewcontext"
	"github.com/narvidev/narvi/internal/domain/review"
)

// This file (compositiondispatch.go) closes a named gap:
// §15.3's own aggregate-diff composition review pass was, before this
// Step, never dispatched anywhere in this codebase -- only its own
// TRIGGER decision (review.ShouldRunAggregateReview) was computed and
// rendered (run.go's own pre-Step doc comment, and httpapi/
// releasemanifestreadout.go's identical note, both now updated).
//
// dispatchCompositionReview is Run's own new final step, called only when
// the trigger fired: it inserts ONE further turn on the SAME review
// session Run itself is running for (in.SessionID) -- §15.5's own
// "extends... review-session reuse" instruction, never a second session,
// a new sandbox, or a new subsystem. The release PR's own diff against
// its immediate base IS the aggregate diff baseRef..headRef §15.3 asks
// this pass to review (a release PR's own diff is, by construction,
// exactly that range), so this re-fetches it via the SAME reviewcontext.
// Fetch assembly point every other review-turn-creation path already
// calls, rather than inventing a second diff-fetch mechanism.
//
// Mirrors internal/app/sessionactor/reviewretrigger.go's own
// insertAutoRetriggerTurn precedent for WHY this inserts a turn directly
// via a store's own Create method rather than going through httpapi's
// createTurnLocked: that function is unexported, so no package outside
// internal/adapters/inbound/httpapi can call it at all, regardless of any
// import-cycle question. This inserts the turn directly via
// CompositionTurnInserter.Create -- the SAME store-level primitive
// createTurnLocked itself calls -- mirroring §8.2's manual path at the
// storage layer rather than calling through it, exactly like
// insertAutoRetriggerTurn already does for the automatic-re-review lane.
// The SAME "workflowengine wiring is deliberately not duplicated"
// omission reviewretrigger.go's own doc comment names applies identically
// here: a composition-review turn is untracked by workflowengine,
// degrading safely exactly like every other turn workflow engine was
// never taught to track.
//
// Entirely best-effort, mirroring Run's own established "every internal
// failure is logged and this function simply returns" posture: a
// dispatch failure here must never affect the manifest check's own
// already-enqueued outbox comment, and must never surface as a fabricated
// "no composition findings" -- release_manifest_checks.
// composition_reviewed_at simply stays NULL, which httpapi/
// releasemanifestreadout.go already renders as an honest "not yet
// available" state (this file's own top doc comment).
func dispatchCompositionReview(ctx context.Context, logger *slog.Logger, deps Deps, in Input, checkID pgtype.UUID) {
	if deps.CompositionTemplates == nil || deps.CompositionDiffFetcher == nil || deps.CompositionTurns == nil || deps.CompositionDispatch == nil {
		logger.Warn("releasereview: aggregate review triggered but composition-dispatch dependencies are not configured, the composition pass will not run for this release",
			"owner", in.Owner, "repo", in.Repo, "pr_number", in.PRNumber)
		return
	}

	template, err := deps.CompositionTemplates.GetTemplate(ctx, compositionReviewPromptTemplateName)
	if err != nil {
		logger.Warn("releasereview: fetch composition review prompt template failed, declining to dispatch the composition pass",
			"error", err, "owner", in.Owner, "repo", in.Repo, "pr_number", in.PRNumber)
		return
	}

	reviewCtx := reviewcontext.Fetch(ctx, logger, deps.CompositionDiffFetcher, deps.Timeouts, in.Owner, in.Repo, in.PRNumber, in.Token, nil)
	if reviewCtx.HeadSHA == "" {
		// §24's own fail-closed direction, reused here: without a live,
		// provably-fresh head sha to anchor this turn's own
		// turns.review_head_sha to, dispatching it would leave no honest
		// commit to record it against -- decline this cycle rather than
		// guess-and-dispatch (sessionactor/reviewretrigger.go's own
		// identical reasoning).
		logger.Warn("releasereview: could not resolve a live head sha for this release PR, declining to dispatch the composition pass",
			"owner", in.Owner, "repo", in.Repo, "pr_number", in.PRNumber)
		return
	}
	if reviewCtx.Diff == "" {
		// Confirmed-major fix: reviewcontext.Fetch degrades HeadSHA and
		// Diff INDEPENDENTLY (that function's own doc comment) -- a
		// GetPullRequest success followed by a GetCompareDiff FAILURE
		// leaves HeadSHA populated (the check immediately above passes)
		// while Diff stays "". Dispatching anyway used to send
		// RenderCompositionReviewPrompt a prompt with NO diff block at
		// all (that function's own "ctx.Diff empty... renders no diff
		// block at all" branch) -- the reviewing agent would then have
		// nothing to review but the tool instructions, and would very
		// plausibly still call the composition-findings tool with an
		// empty findings array, which release_manifest_checks.
		// composition_reviewed_at/composition_findings would then
		// persist as an INDISTINGUISHABLE-from-genuine "reviewed, found
		// nothing" result. A pass that reviewed nothing must never be
		// recorded as a pass that found nothing -- decline this cycle
		// exactly like the missing-head-sha branch immediately above,
		// leaving composition_reviewed_at NULL (this Step's own
		// "not yet available" sentinel, never a fabricated clean result).
		logger.Warn("releasereview: could not fetch this release PR's own diff, declining to dispatch the composition pass rather than review nothing and record it as a clean result",
			"owner", in.Owner, "repo", in.Repo, "pr_number", in.PRNumber, "head_sha", reviewCtx.HeadSHA)
		return
	}

	prompt := review.RenderCompositionReviewPrompt(template, reviewCtx)

	created, err := deps.CompositionTurns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID:     in.SessionID,
		Status:        sqlcgen.TurnStatusPending,
		Prompt:        &prompt,
		ReviewHeadSha: &reviewCtx.HeadSHA,
		CorrelationID: in.CorrelationID,
	})
	if err != nil {
		logger.Error("releasereview: insert composition review turn failed",
			"error", err, "owner", in.Owner, "repo", in.Repo, "pr_number", in.PRNumber)
		return
	}

	logger.Info("releasereview: composition review turn dispatched",
		"owner", in.Owner, "repo", in.Repo, "pr_number", in.PRNumber, "turn_id", created.ID.String())

	// Confirmed-major fix (auditability): anchor this composition review to
	// the EXACT commit/diff-completeness it actually ran against, on the
	// SAME release_manifest_checks row its own findings will later land on
	// -- see UpdateCompositionAnchor's own doc comment (postgres store) for
	// the full "why at dispatch time, never at findings-post time"
	// reasoning. checkID.Valid is false whenever persistReleaseManifestCheck
	// itself never produced a row (store unset, or the insert failed) --
	// there is nothing to anchor in that case, and deps.CompositionAnchor
	// nil is this dependency's own established "caller never wired this
	// deliverable" degrade (mirrors every other Composition* dep on this
	// same Deps struct). Best-effort, exactly like every other step in this
	// function: a failure (or a declined write) here never unwinds the
	// turn dispatch that already happened above.
	if deps.CompositionAnchor != nil && checkID.Valid {
		if _, err := deps.CompositionAnchor.UpdateCompositionAnchor(ctx, checkID, reviewCtx.HeadSHA, reviewCtx.DiffTruncated); err != nil {
			logger.Warn("releasereview: record composition review anchor (head sha/diff-truncated) failed",
				"error", err, "owner", in.Owner, "repo", in.Repo, "pr_number", in.PRNumber)
		}
	}

	if err := deps.CompositionDispatch.EnsureDispatched(ctx, in.SessionID); err != nil {
		logger.Warn("releasereview: ensure-dispatched after composition review turn insert failed",
			"error", err, "owner", in.Owner, "repo", in.Repo, "pr_number", in.PRNumber)
	}
}

// compositionReviewPromptTemplateName is §15.3's own versioned prompt
// template's name in prompt_templates (migrations/
// 000127_release_manifest_checks_composition.up.sql's own seed row) --
// the SAME DB-backed storage/versioning mechanism §18.6/§12.2 item 5
// already built for the intent classifier's own templates (§15.3's own
// explicit "same mechanism as §8.3/§12.2 item 5" instruction).
const compositionReviewPromptTemplateName = "release_composition_review"

// CompositionTemplateFetcher is the narrow slice of
// *postgres.PromptTemplateStore this package needs for the composition
// review's own versioned prompt template -- mirrors internal/app/
// intentclassifier's own identically-shaped TemplateFetcher interface,
// declared separately here (rather than imported) since intentclassifier
// is an unrelated app package this one has no reason to depend on for a
// single-method interface shape. *postgres.PromptTemplateStore satisfies
// this directly (GetTemplate already exists on it for intentclassifier's
// own use).
type CompositionTemplateFetcher interface {
	GetTemplate(ctx context.Context, name string) (string, error)
}

// CompositionTurnInserter is the narrow slice of *postgres.TurnStore this
// package needs -- mirrors this package's own OutboxEnqueuer/
// ReleaseManifestCheckInserter precedent: a small, locally-defined
// interface so a unit test can inject a fake with no real DB round trip.
type CompositionTurnInserter interface {
	Create(ctx context.Context, arg sqlcgen.CreateTurnParams) (sqlcgen.Turn, error)
}

// CompositionAnchorUpdater is the narrow slice of
// *postgres.ReleaseManifestCheckStore this package needs to record
// composition_head_sha/composition_diff_truncated (migrations/
// 000128_release_manifest_checks_composition_anchor.up.sql) -- mirrors
// CompositionTurnInserter's own identical "small, locally-defined
// interface so a unit test can inject a fake" precedent immediately
// below. *postgres.ReleaseManifestCheckStore satisfies this directly
// (UpdateCompositionAnchor already exists on it, alongside
// UpdateCompositionFindings/UpdateCompositionDecision).
type CompositionAnchorUpdater interface {
	UpdateCompositionAnchor(ctx context.Context, id pgtype.UUID, headSHA string, diffTruncated bool) (sqlcgen.ReleaseManifestCheck, error)
}

// CompositionDispatcher is this package's own narrow "please dispatch
// this session's newly-inserted pending turn now" dependency -- mirrors
// httpapi.createTurnLocked's own established "GetOrSpawn, then
// Send(EnsureDispatched{})" fire-and-forget sequencing (internal/adapters/
// inbound/httpapi/turn.go), abstracted behind one method so this package
// never needs to import internal/app/sessionactor's own Command type
// system directly -- production wiring (controlplane/serve.go) supplies a
// thin adapter around sessionactor.Registry.GetOrSpawn +
// (*sessionactor.Actor).Send(sessionactor.EnsureDispatched{}); a unit test
// injects a no-op fake with no real Actor/mailbox involved at all.
type CompositionDispatcher interface {
	EnsureDispatched(ctx context.Context, sessionID pgtype.UUID) error
}
