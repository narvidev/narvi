package reviewtriage

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
	"github.com/narvidev/narvi/internal/platform"
)

// ComputeDecision is §26.3's own "never-throw" entry point (§26.3: "ANY
// triage error fails open to light -- a review must never be blocked by
// its own router"), mirroring internal/app/intentclassifier.Service.
// ClassifyAndRecord's own identical "the caller never sees an error, only
// a safe, always-valid result" contract (§18.1). NO error return at all,
// by construction: every internal failure this function's own body can
// hit (a repo_settings read failure in LoadConfig, a review_verdicts read
// failure below) is caught, logged, and neutralized to its safe value
// (the built-in config, no prior high verdict, no floor).
//
// That fail-open rule is about this function's OWN reads. An input the
// review context could not read (prCtx.InputRead, set by the context's
// producer) is not a router error: it reaches reviewtriage.Decide as a
// typed fact and routes deep under ReasonInputUnreadable (§26.3).
//
// prCtx is the SAME review.PreFetchedContext every review-trigger path
// already builds via internal/app/reviewcontext.Fetch (§26.3: "already
// fetched with the inline diff at review-session creation") -- this
// function performs two further reads of its own (the latest posted
// verdict, for the "prior high verdict" signal, and the floor's own
// review_path read below) plus LoadConfig's own repo_settings read; no
// new network call, no diff re-fetch.
//
// Returns the FRESH decision, cfg, and priorReviewDepth -- the depth §24's
// re-review floor composes with (reviewtriage.Floor(decision.Depth,
// priorReviewDepth)), which every lane applies unless the fresh decision
// is an always_light override (Floor's own doc comment). priorReviewDepth
// is the review_path of the latest verdict whose producing turn was NOT
// routed for a reviewtriage.NonFloorReasons reason (depth.go): a depth
// chosen only because an input could not be read is never a floor, and
// the verdict before it is read instead. Empty ("") when no such verdict
// exists, or its own turn never resolved a depth -- both degrade to
// "nothing to floor against", exactly like a brand-new review session.
//
// deps.SizeExclusions (the deployment's size patterns) are set on the
// returned cfg, so the caller's decision record reflects the config the
// decision was actually made under.
func ComputeDecision(ctx context.Context, deps Deps, repoFullName string, prNumber int32, prCtx review.PreFetchedContext) (decision reviewtriage.Decision, cfg reviewtriage.Config, priorReviewDepth reviewtriage.ReviewDepth) {
	logger := platform.Logger(ctx)

	cfg, err := LoadConfig(ctx, deps, repoFullName)
	if err != nil {
		// LoadConfig's own doc comment already logs the read failure --
		// no second log line here, just the safe fallback.
		cfg = reviewtriage.DefaultConfig()
	}
	cfg.SizeExclusions = deps.SizeExclusions

	// A nil deps.ReviewVerdicts (this package's own tests, or any other
	// minimal wiring that doesn't care about this Step) degrades
	// identically to "no verdict on record" -- never a panic, mirroring
	// LoadConfig's own identical nil-store convention (config.go).
	priorVerdictRiskHigh := false
	if deps.ReviewVerdicts == nil {
		decision, cfg = decideWithSignals(prCtx, cfg, priorVerdictRiskHigh)
		return decision, cfg, ""
	}
	if latest, verdictErr := deps.ReviewVerdicts.GetLatest(ctx, repoFullName, prNumber); verdictErr != nil {
		if !errors.Is(verdictErr, pgx.ErrNoRows) {
			logger.Warn("reviewtriage: compute decision: read latest review verdict failed, treating prior-high-verdict signal as absent", "error", verdictErr, "repo_full_name", repoFullName, "pr_number", prNumber)
		}
		// pgx.ErrNoRows: no verdict has ever been posted for this PR --
		// priorVerdictRiskHigh correctly stays false, not an error at all.
	} else {
		priorVerdictRiskHigh = latest.RiskLevel == string(review.RiskLevelHigh)
	}

	priorReviewDepth = readFloorDepth(ctx, deps, repoFullName, prNumber)

	decision, cfg = decideWithSignals(prCtx, cfg, priorVerdictRiskHigh)
	return decision, cfg, priorReviewDepth
}

// readFloorDepth reads the depth §24's re-review floor composes with --
// ComputeDecision's own doc comment. A read failure is logged and reads
// as no floor (fail open to the fresh decision, §26.3).
func readFloorDepth(ctx context.Context, deps Deps, repoFullName string, prNumber int32) reviewtriage.ReviewDepth {
	nonFloor := reviewtriage.NonFloorReasons()
	excluded := make([]string, len(nonFloor))
	for i, r := range nonFloor {
		excluded[i] = string(r)
	}
	path, err := deps.ReviewVerdicts.GetLatestFloorReviewPath(ctx, repoFullName, prNumber, excluded)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			platform.Logger(ctx).Warn("reviewtriage: compute decision: read the re-review floor's prior depth failed, applying no floor", "error", err, "repo_full_name", repoFullName, "pr_number", prNumber)
		}
		return ""
	}
	if path == nil {
		return ""
	}
	return reviewtriage.ReviewDepth(*path)
}

// decideWithSignals assembles the final reviewtriage.Signals from prCtx
// and the already-resolved priorVerdictRiskHigh, and calls the pure
// domain Decide -- the one tail both of ComputeDecision's own paths
// (real deps.ReviewVerdicts read, or a nil-store short-circuit) share.
// prCtx.InputRead is passed through as is: whether the input was readable
// is the context producer's fact, never re-derived here from empty
// fields.
func decideWithSignals(prCtx review.PreFetchedContext, cfg reviewtriage.Config, priorVerdictRiskHigh bool) (reviewtriage.Decision, reviewtriage.Config) {
	sig := reviewtriage.Signals{
		Additions:              prCtx.Additions,
		Deletions:              prCtx.Deletions,
		ChangedPaths:           prCtx.ChangedPaths,
		FileLines:              reviewtriage.ExtractFileLines(prCtx.Diff),
		InputRead:              prCtx.InputRead,
		NeedsHumanLabelPresent: hasNeedsHumanLabel(prCtx.Labels),
		PriorVerdictRiskHigh:   priorVerdictRiskHigh,
	}
	return reviewtriage.Decide(sig, cfg), cfg
}

// hasNeedsHumanLabel reports whether labels contains reviewpost.
// LabelNeedsHuman (§8.2's own maintainer escape hatch) -- see
// internal/domain/reviewtriage/doc.go's own "v1 rules -- five, not
// three" section for why this is one of Decide's triggers.
func hasNeedsHumanLabel(labels []string) bool {
	for _, l := range labels {
		if l == reviewpost.LabelNeedsHuman {
			return true
		}
	}
	return false
}
