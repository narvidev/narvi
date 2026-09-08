// Package releasereview implements §15's own ("release PR review",
// §15) app-layer orchestration: the manifest check (§15.2), which "ALWAYS
// runs" once a release PR is detected. Impure fetch/persist here, pure
// decision in internal/domain/review (manifestcheck.go, aggregatereview.go)
// -- mirrors internal/app/sessionactor/handoffsentinel.go's own identical
// "impure fetch in app, pure decision in domain" split for a comparable
// review-adjacent, fully-mechanical sentinel.
//
// # Where this is called from (two phases, since blocking-finding fix #1)
//
// internal/adapters/inbound/github's own webhook handler, on the WINNER
// (brand-new session) path only, once internal/domain/intent.DetectRelease
// (§15.1) has determined the mention's own PR is a release PR -- see that
// package's own handler.go for the call site and the reasoning for why
// this Step scopes detection to session-creation time only (a re-trigger
// on an already-tracked release PR does not re-run this check in this
// Step -- a natural, documented follow-up, not a correctness gap: the
// manifest is a point-in-time compliance snapshot, and the FIRST review
// session opened on a release PR is the moment its own existence is
// first discovered by this system).
//
// That handler no longer calls Run (below) directly. Blocking-finding fix
// #1: Run's own real work (SourceControl.ListMergedBetween) can take up
// to platform.Timeouts.GitHubListMergedBetweenTimeout (2 minutes, ~80+
// sequential GitHub API calls for a large release cut) -- far longer than
// GitHub's own ~10s webhook delivery timeout, so it must never run inline
// inside the webhook handler's own request/response cycle. The handler
// instead calls Enqueue (enqueue.go), which does ONE cheap, fast INSERT
// into release_manifest_pending and returns immediately, before its own
// ack. Worker (worker.go) is the SEPARATE background loop (started
// alongside every other background loop in cmd/control-plane/main.go's
// own errgroup) that later claims that row and calls Run -- entirely
// decoupled from any webhook request's own context/lifetime. See
// migrations/000050_release_manifest_pending.up.sql's own doc comment for
// the full "why" behind this two-phase split.
//
// # Why the manifest check needs no idempotency claim of its own
//
// Unlike internal/app/sessionactor/handoffsentinel.go (which claims a
// dedicated handoff_sentinel_runs row before posting, because ITS OWN
// trigger -- "a PR was just created by a scoped session" -- can fire
// multiple times for the same PR across retried/resumed turns), this
// package's one real caller only ever runs on session CREATION, and
// github_pr_sessions' own per-PR atomic claim (§8.2) already
// guarantees at most one winner ever creates a session for a given PR --
// so at most one call to Enqueue (and, transitively, at most one call to
// Run) ever happens per release PR, structurally, with no separate claim
// table needed for THAT guarantee (release_manifest_pending exists to
// decouple WHEN/on whose context Run runs, never to deduplicate it).
package releasereview

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/reviewcontext"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/platform"
)

// MergedPRLister is the narrow slice of ports.SourceControl this package
// needs -- mirrors internal/adapters/inbound/github's own
// PullRequestResolver/internal/app/reviewcontext's own Fetcher precedent:
// a small, locally-defined interface so a unit test can inject a fake
// with no real HTTP round trip. *githubapi.Adapter satisfies this
// directly, with no adapter-side change beyond what this Step already
// adds to it.
type MergedPRLister interface {
	ListMergedBetween(ctx context.Context, spec ports.ListMergedBetweenSpec) (merged []ports.MergedPR, truncated bool, err error)
}

// OutboxEnqueuer is the narrow slice of *postgres.OutboxStore this
// package needs -- mirrors internal/app/sessionactor/handoffsentinel.go's
// own use of the concrete store's Create method shape, here narrowed to
// an interface purely for this package's own unit tests.
type OutboxEnqueuer interface {
	Create(ctx context.Context, arg sqlcgen.CreateOutboxEntryParams) (sqlcgen.Outbox, error)
}

// Deps bundles what Run needs.
type Deps struct {
	SourceControl MergedPRLister
	Outbox        OutboxEnqueuer
	// ReleaseManifestChecks (§12.2 item 9) persists this check's
	// own typed result so the release-review screen has a durable read
	// model -- nil-safe (persistReleaseManifestCheck's own doc comment),
	// mirroring this package's own established "every internal failure
	// degrades, never blocks" posture for a caller that doesn't wire one.
	ReleaseManifestChecks ReleaseManifestCheckInserter
	// CompositionTemplates/CompositionDiffFetcher/CompositionTurns/
	// CompositionDispatch (Step 125, §15.3) back dispatchCompositionReview
	// (compositiondispatch.go) -- ALL nil-safe, mirroring
	// ReleaseManifestChecks immediately above: any one of them left unset
	// simply declines to dispatch the composition pass for a caller that
	// doesn't wire this deliverable, never a panic or a degraded manifest
	// check.
	CompositionTemplates   CompositionTemplateFetcher
	CompositionDiffFetcher reviewcontext.Fetcher
	CompositionTurns       CompositionTurnInserter
	CompositionDispatch    CompositionDispatcher
	Timeouts               platform.Timeouts
}

// Input is what Run needs to know about the just-detected release PR.
type Input struct {
	// SessionID is the review session just created for this release PR --
	// the outbox row this function enqueues is scoped to it, mirroring
	// every other review-adjacent Notifier's own session-scoped outbox
	// row (VerdictPayload/HandoffPayload's own identical convention).
	SessionID pgtype.UUID
	Owner     string
	Repo      string
	PRNumber  int32
	// BaseRef/HeadRef are this release PR's own base/head branches --
	// exactly ports.ListMergedBetweenSpec's own BaseRef/HeadRef.
	BaseRef string
	HeadRef string
	// Token authenticates every outbound call ListMergedBetween itself
	// makes -- the bot's own statically-configured credential (this
	// check is a system-generated audit, never attributed to any
	// individual commenter, mirroring VerdictNotifier/HandoffNotifier's
	// own identical bot-token choice).
	Token string
	// CorrelationID mirrors internal/app/sessionactor/outboxenqueue.go's
	// own "read from ctx if present, else NULL" convention -- the caller
	// passes whatever internal/platform.CorrelationIDFromContext(ctx)
	// already resolved, rather than this package re-deriving it.
	CorrelationID *string
}

// Run implements this package's own top doc comment: fetch the release
// PR's own constituent-PR manifest, compute the mechanical findings
// (§15.2) and the aggregate-review trigger (§15.3), render one comment,
// and enqueue it via the outbox -- never a raw, synchronous GitHub call
// (§15.2: "posted through the same server-side... path... never a raw
// comment" -- this package enqueues an outbox row exactly like the
// verdict-posting tool and the handoff sentinel both do, delivered later
// by internal/adapters/outbound/githubapi.ReleaseManifestNotifier, never
// a direct PostIssueComment call from inside this function).
//
// Best-effort throughout, mirroring every other sentinel-shaped app-layer
// function in this codebase (checkHandoffContractDrift, createPRBestEffort):
// a failure at ANY step here is logged and this function simply returns,
// never propagating an error the caller would have to decide whether to
// fail an already-successful session creation over.
func Run(ctx context.Context, logger *slog.Logger, deps Deps, in Input) {
	listCtx, cancel := context.WithTimeout(ctx, deps.Timeouts.GitHubListMergedBetweenTimeout)
	merged, truncated, err := deps.SourceControl.ListMergedBetween(listCtx, ports.ListMergedBetweenSpec{
		Owner:   in.Owner,
		Repo:    in.Repo,
		BaseRef: in.BaseRef,
		HeadRef: in.HeadRef,
		Token:   in.Token,
	})
	cancel()
	if err != nil {
		logger.Warn("releasereview: list merged between failed, skipping manifest check",
			"error", err, "owner", in.Owner, "repo", in.Repo, "pr_number", in.PRNumber)
		return
	}

	domainMerged := make([]review.MergedPR, len(merged))
	for i, m := range merged {
		domainMerged[i] = toDomainMergedPR(m)
	}

	findings := review.ComputeReleaseManifestFindings(domainMerged)
	aggregateReview := review.ShouldRunAggregateReview(domainMerged)
	triggerReasons := review.AggregateReviewTriggerReasons(domainMerged)
	// Blocking-finding fix #5: truncated (ListMergedBetween's own second
	// return -- see MergedPRLister's own doc comment) is threaded through
	// so the rendered comment never claims a completeness guarantee this
	// port call did not actually give it -- see RenderManifestComment's
	// own doc comment.
	body := reviewpost.RenderManifestComment(findings, len(domainMerged), aggregateReview, truncated)

	logger.Info("releasereview: manifest check computed",
		"owner", in.Owner, "repo", in.Repo, "pr_number", in.PRNumber,
		"constituent_pr_count", len(domainMerged), "finding_count", len(findings),
		"aggregate_review_triggered", aggregateReview, "coverage_truncated", truncated)

	// §12.2 item 9's own release-review screen: persist the SAME typed
	// data above, alongside (never instead of) the outbox-delivered
	// comment below -- see persist.go's own doc comment for why this
	// exists and why it is best-effort.
	persistReleaseManifestCheck(ctx, logger, deps.ReleaseManifestChecks, in, domainMerged, findings, aggregateReview, triggerReasons, truncated)

	// Step 125 (§15.3): dispatch the actual composition review pass, once
	// its own trigger decision (aggregateReview, above) fires -- see
	// dispatchCompositionReview's own doc comment (compositiondispatch.go)
	// for the full "why a second turn on this same session" design.
	// Called AFTER persistReleaseManifestCheck above so the
	// release_manifest_checks row this dispatches against already exists
	// by the time the composition-findings-posting tool's own later call
	// looks it up by session id.
	if aggregateReview {
		dispatchCompositionReview(ctx, logger, deps, in)
	}

	payload, err := json.Marshal(githubapi.ReleaseManifestPayload{
		Owner:    in.Owner,
		Repo:     in.Repo,
		PRNumber: int(in.PRNumber),
		Body:     body,
	})
	if err != nil {
		logger.Error("releasereview: marshal outbox payload failed", "error", err)
		return
	}

	if _, err := deps.Outbox.Create(ctx, sqlcgen.CreateOutboxEntryParams{
		SessionID:     in.SessionID,
		Kind:          string(ports.NotificationKindReleaseManifest),
		Payload:       payload,
		CorrelationID: in.CorrelationID,
	}); err != nil {
		logger.Error("releasereview: enqueue outbox entry failed", "error", err)
	}
}

// toDomainMergedPR converts one ports.MergedPR (SourceControl.
// ListMergedBetween's own wire/port-facing shape) into
// internal/domain/review.MergedPR -- the boundary conversion
// internal/domain/review/manifestcheck.go's own top doc comment
// describes, mirroring internal/adapters/inbound/httpapi/reviewverdict.go's
// identical restdtos->reviewpost.VerdictInput conversion precedent.
// HighRiskFlagged is derived here (never inside domain/review itself,
// which cannot import reviewpost -- see that package's own doc comment)
// by checking m.Labels against reviewpost.LabelHighRisk, the SAME
// review:*-risk label vocabulary §8.2's verdict-posting tool already
// syncs onto every reviewed PR.
func toDomainMergedPR(m ports.MergedPR) review.MergedPR {
	var revertedAfterSeconds *int64
	if m.RevertedAt != nil && !m.MergedAt.IsZero() {
		seconds := int64(m.RevertedAt.Sub(m.MergedAt).Seconds())
		revertedAfterSeconds = &seconds
	}

	highRisk := false
	for _, l := range m.Labels {
		if l == reviewpost.LabelHighRisk {
			highRisk = true
			break
		}
	}

	return review.MergedPR{
		Number:                      m.Number,
		Title:                       m.Title,
		HasApprovingReview:          m.HasApprovingReview,
		MergedViaAdminOverride:      m.MergedViaAdminOverride,
		CIConclusionAtMergeSHA:      toDomainCIConclusion(m.CIConclusionAtMergeSHA),
		WasReverted:                 m.WasReverted,
		RevertReviewState:           toDomainRevertReviewState(m.RevertReviewState),
		RevertedAfterMergeSeconds:   revertedAfterSeconds,
		HadManualConflictResolution: m.HadManualConflictResolution,
		ChangedPathPrefixes:         m.ChangedPathPrefixes,
		HighRiskFlagged:             highRisk,
	}
}

// toDomainCIConclusion converts ports.CIConclusion into
// review.CIConclusion -- a direct, exhaustive mapping; any unrecognized
// ports.CIConclusion value (there should never be one -- githubapi's own
// ListMergedBetween implementation only ever produces one of the three
// named constants) maps to review.CIConclusionUnknown, matching that
// type's own "never asserted without positive confirmation" discipline
// (internal/domain/review/manifestcheck.go).
func toDomainCIConclusion(c ports.CIConclusion) review.CIConclusion {
	switch c {
	case ports.CIConclusionSuccess:
		return review.CIConclusionSuccess
	case ports.CIConclusionFailure:
		return review.CIConclusionFailure
	default:
		return review.CIConclusionUnknown
	}
}

// toDomainRevertReviewState converts ports.RevertReviewState into
// review.RevertReviewState -- a direct, exhaustive mapping, mirroring
// toDomainCIConclusion's own identical shape immediately above (any
// unrecognized ports.RevertReviewState value -- there should never be
// one -- maps to review.RevertReviewStateUnknown, never silently to
// RevertReviewStateNotReviewed, matching that type's own "never asserted
// without positive confirmation" discipline, blocking-finding fix #4).
func toDomainRevertReviewState(s ports.RevertReviewState) review.RevertReviewState {
	switch s {
	case ports.RevertReviewStateReviewed:
		return review.RevertReviewStateReviewed
	case ports.RevertReviewStateNotReviewed:
		return review.RevertReviewStateNotReviewed
	default:
		return review.RevertReviewStateUnknown
	}
}
