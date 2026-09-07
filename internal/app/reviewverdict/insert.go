package reviewverdict

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/egressmode"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
)

// Insert appends one review_verdicts row for (repoFullName, prNumber),
// forwarding verdict/headSHA verbatim -- §21.1: "pure storage, never
// re-parsing anything out of posted comment text". store is taken
// directly (already WithTx'd by the caller when a transaction is in
// play, e.g. httpapi.PostReviewVerdict's own existing tx) rather than
// this package opening its own -- mirrors reviewFindings.WithTx(tx)'s own
// identical calling convention at that SAME call site, so this insert
// commits atomically alongside the findings upserts and outbox write
// already happening there.
//
// headSHA == "" is refused outright (never inserted as an empty string)
// -- see migrations/000067_review_verdicts.up.sql's own doc comment for
// why a verdict with no known head SHA must not exist in this table at
// all, rather than existing with a value the auto-approval eligibility
// engine's own stale-verdict guard could never honestly evaluate.
//
// digest (§26.1, extended by §26.2) is forwarded
// verbatim onto the SAME row's own seven digest_* columns (migrations/
// 000077_review_verdicts_digest.up.sql,
// 000078_review_verdicts_description_adequacy.up.sql) -- digest.Summary/
// DescriptionAdequacy/AdequacyExplanation are expected non-empty/in-enum
// by the time this function is called in production
// (reviewpost.ValidateVerdictInput's own ErrEmptyDigestSummary/
// ErrInvalidDescriptionAdequacy/ErrEmptyAdequacyExplanation already
// rejected a malformed one before BuildVerdict ever ran), but this
// function does not itself re-validate that -- exactly like it does not
// re-validate verdict's own fields, trusting its one caller
// (httpapi.PostReviewVerdict) to have already done so.
//
// digest is NOT, however, forwarded byte-for-byte: reviewpost.
// SanitizeDigest (hardening the write path against the
// class PR #188 closed on the read path) runs FIRST, over a local copy --
// see that function's own doc comment (reviewpost/sanitize.go) for the
// full "why the write path too" reasoning. Every model-authored free-text
// digest field (Summary/StackRisks/UnverifiedLimits/AdequacyExplanation/
// ProposedBody/ContestedPoints/each ArchDecision's own three fields) has
// every literal secret-substitution placeholder token stripped and every
// '<'/'>' HTML-entity-escaped before any of the marshaling/persistence
// below ever sees it -- so the row this function writes can never carry a
// placeholder token, whatever future read path re-injects a stored digest
// into a later prompt (none does today). This does NOT double-escape the
// SAME verdict's already-posted GitHub comment: RenderVerdictComment
// (httpapi.PostReviewVerdict's own earlier call, reviewverdict.go) renders
// the ORIGINAL, unsanitized in-memory digest -- Digest is passed by VALUE
// into both this function and RenderVerdictComment, so this function's own
// local sanitized copy is never visible to that already-completed
// rendering. See reviewpost.SanitizeDigest's own doc comment for the full
// "no double-escaping, verified not assumed" argument.
// reviewPath (§26.3) is the posting turn's own turns.
// review_depth, forwarded verbatim -- empty ("", reviewtriage.ReviewDepth
// zero value) is a legitimate, common value (a verdict whose own turn
// never resolved a depth, or a caller that predates this Step), persisted
// as a genuine SQL NULL, never the literal string "" (nonEmptyStringPtr
// below).
//
// counterReview/factCheck/factCheckKilled (§26.4/§26.6) are
// forwarded verbatim from the SAME already-validated VerdictInput this
// verdict/digest were themselves built from -- counterReview's own empty
// value (light path, §26.9) persists as NULL via nonEmptyStringPtr
// exactly like reviewPath's own identical degradation; factCheck is
// UNCONDITIONALLY non-empty by the time this function is called in
// production (reviewpost.ValidateVerdictInput's own ErrInvalidFactCheck
// already rejected anything else before BuildVerdict ever ran), but this
// function does not itself re-validate that, exactly like it does not
// re-validate any other field. factCheckKilled persists as a genuine SQL
// NULL only when this function is never reached by a real caller at all
// (there is no "unset" VerdictInput.FactCheckKilled distinct from 0 --
// factCheckKilledPtr below always returns a non-nil pointer).
//
// repoSettings/platformShadow (§30.8) resolve this row's OWN epoch stamp
// (suppressed_in_shadow, migrations/
// 000105_review_verdicts_shadow_epoch.up.sql) via egressmode.Resolve --
// the SAME single authority postgres.OutboxStore.Create already uses for
// the outbox's own identical stamp -- computed here, at this table's one
// INSERT, rather than trusted from any caller-supplied value: a verdict
// is written once, synchronously, so (unlike the outbox's own async
// retries) there is no delivery-time recheck to pair this stamp with --
// see migrations/000105's own doc comment for why that asymmetry is
// safe. The exclusion this stamp feeds lives entirely in the read
// queries (GetLatestNonShadowReviewVerdict, ListLatestAutoApprovedInRepo,
// ListNonShadowReviewVerdictsInWindow) -- §30.8: "never call-site
// checks".
//
// archDecisionTags/archDecisionRoots (§31.6) are forwarded verbatim onto
// this row's own arch_decision_tags/arch_decision_roots columns
// (migrations/000113) -- the caller has already read them off THIS
// verdict's own turn (turns.review_depth_decision's own ArchDecisionTags/
// ArchDecisionRoots, computed once at turn-creation time via
// autoapproval.ClassifyChangedPaths/ClassifyChangedRoots over that
// turn's own ChangedPaths). This function does no classification of its
// own -- exactly like it forwards reviewPath/counterReview/factCheck
// verbatim rather than re-deriving any of them.
func Insert(ctx context.Context, store *postgres.ReviewVerdictStore, repoSettings *postgres.RepoSettingsStore, platformShadow bool, repoFullName string, prNumber int32, headSHA string, sessionID pgtype.UUID, verdict review.Verdict, digest reviewpost.Digest, reviewPath reviewtriage.ReviewDepth, counterReview review.CounterReviewStatus, factCheck reviewpost.FactCheckStatus, factCheckKilled int, archDecisionTags, archDecisionRoots []string) (reviewverdict.Record, error) {
	if headSHA == "" {
		return reviewverdict.Record{}, fmt.Errorf("reviewverdict: insert: refusing to persist a verdict with no known head sha for %s#%d", repoFullName, prNumber)
	}

	suppressedInShadow := egressmode.Resolve(ctx, egressmode.Deps{
		RepoSettings:   repoSettings,
		PlatformShadow: platformShadow,
	}, repoFullName).Suppressed()

	// hardening: sanitize a LOCAL copy of digest before anything
	// below marshals or persists it -- see this function's own doc comment
	// (above) and reviewpost.SanitizeDigest's own doc comment for the full
	// "why" and the "no double-escaping" argument. digest (the parameter)
	// is intentionally never reassigned in place beyond this one line --
	// everything below reads only this new, sanitized value.
	digest = reviewpost.SanitizeDigest(digest)

	blastRadiusJSON, err := marshalTags(verdict.BlastRadius)
	if err != nil {
		return reviewverdict.Record{}, fmt.Errorf("reviewverdict: insert: marshal blast radius: %w", err)
	}

	archDecisionsJSON, err := marshalArchDecisions(digest.ArchDecisions)
	if err != nil {
		return reviewverdict.Record{}, fmt.Errorf("reviewverdict: insert: marshal digest arch decisions: %w", err)
	}

	// Marshalled through marshalStrings, never json.Marshal directly: a
	// nil slice marshals to "null", which the gate's own containment
	// operator cannot match and which is indistinguishable from a decode
	// failure. marshalStrings' copy through make gives an honestly empty
	// "[]" instead, so a verdict with no tags is reachable by the
	// selector's recency fallback and by nothing else.
	archDecisionTagsJSON, err := marshalStrings(archDecisionTags)
	if err != nil {
		return reviewverdict.Record{}, fmt.Errorf("reviewverdict: marshal arch decision tags: %w", err)
	}
	archDecisionRootsJSON, err := marshalStrings(archDecisionRoots)
	if err != nil {
		return reviewverdict.Record{}, fmt.Errorf("reviewverdict: marshal arch decision roots: %w", err)
	}

	row, err := store.Insert(ctx, sqlcgen.InsertReviewVerdictParams{
		RepoFullName:              repoFullName,
		PrNumber:                  prNumber,
		HeadSha:                   headSHA,
		RiskLevel:                 string(verdict.RiskLevel),
		Premise:                   string(verdict.Premise),
		BlastRadius:               blastRadiusJSON,
		FilesChanged:              int32(verdict.FilesChanged),
		TestsCoverage:             string(verdict.TestsCoverage),
		DocsDrift:                 string(verdict.DocsDrift),
		ProposedShippable:         string(verdict.ProposedShippable),
		Shippable:                 string(verdict.Shippable),
		SessionID:                 sessionID,
		DigestSummary:             nonEmptyStringPtr(digest.Summary),
		DigestArchDecisions:       archDecisionsJSON,
		DigestStackRisks:          nonEmptyStringPtr(digest.StackRisks),
		DigestUnverifiedLimits:    nonEmptyStringPtr(digest.UnverifiedLimits),
		DigestDescriptionAdequacy: nonEmptyStringPtr(string(digest.DescriptionAdequacy)),
		DigestAdequacyExplanation: nonEmptyStringPtr(digest.AdequacyExplanation),
		DigestProposedBody:        nonEmptyStringPtr(digest.ProposedBody),
		ReviewPath:                nonEmptyStringPtr(string(reviewPath)),
		CounterReview:             nonEmptyStringPtr(string(counterReview)),
		FactCheck:                 nonEmptyStringPtr(string(factCheck)),
		FactCheckKilled:           factCheckKilledPtr(factCheckKilled),
		DigestContestedPoints:     nonEmptyStringPtr(digest.ContestedPoints),
		SuppressedInShadow:        suppressedInShadow,
		ArchDecisionTags:          archDecisionTagsJSON,
		ArchDecisionRoots:         archDecisionRootsJSON,
	})
	if err != nil {
		return reviewverdict.Record{}, err
	}
	return recordFromRow(row), nil
}

// nonEmptyStringPtr returns nil for an empty string, or a pointer to s
// otherwise -- so an unset/blank digest field is stored as a real SQL
// NULL (migrations/000077's own "not yet computed" convention), never the
// empty string "", which this table's own reader (digestFromRow,
// convert.go) would otherwise be unable to distinguish from a genuinely
// empty-but-present value.
func nonEmptyStringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// factCheckKilledPtr converts n into sqlcgen.InsertReviewVerdictParams.
// FactCheckKilled's own *int32 column shape -- ALWAYS a non-nil pointer
// (unlike nonEmptyStringPtr above): there is no "unset" FactCheckKilled
// value distinct from 0 (reviewpost.VerdictInput.FactCheckKilled is a
// plain int, always present on every VerdictInput, validated non-negative
// by ValidateVerdictInput before this function is ever called in
// production) -- a real, non-NULL 0 in review_verdicts.fact_check_killed
// therefore always means "a real INSERT recorded zero kills" (fact_check
// == 'skipped', or a 'done' pass that happened to remove nothing), never
// "no value was ever recorded" -- that latter case is what a genuine SQL
// NULL (a pre-existing row, this column simply not existing yet) means
// instead.
func factCheckKilledPtr(n int) *int32 {
	v := int32(n)
	return &v
}
