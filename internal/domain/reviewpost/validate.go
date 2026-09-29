package reviewpost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
)

// VerdictInput is the shape of a review-verdict-posting-tool call's own
// typed fields (§8.2) BEFORE the server recomputes the
// authoritative Shippable -- one field per Verdict field EXCEPT Shippable
// itself (never accepted from a caller, per review.Verdict's own CONTRACT)
// plus Summary, the agent's own human-readable narrative body (review's
// own doc.go design call #4: no Finding/narrative shape exists in that
// package yet, so this is carried here, one level up, until a later Step
// gives it a richer, structured home).
type VerdictInput struct {
	RiskLevel         review.RiskLevel
	Premise           review.PremiseState
	BlastRadius       []review.Tag
	FilesChanged      int
	TestsCoverage     review.TestsCoverageState
	DocsDrift         review.DocsDriftState
	ProposedShippable review.ProposedShippable
	// Summary is the agent's own free-text narrative -- required
	// (ValidateVerdictInput rejects an empty/whitespace-only value): a
	// verdict with no human-readable explanation at all defeats the point
	// of posting one. Never parsed back out as structured data by anything
	// in this codebase -- see review/doc.go's own top-level "nothing here
	// even imports a markdown parser, on principle" stance, which this
	// field's own handling matches: it is rendered once (rendercomment.go)
	// and never read again.
	Summary string

	// Findings is §8.2's own additive extension (§8.2/§17/§22.1): zero
	// or more per-finding typed fields, alongside the verdict's own
	// aggregate fields above -- restdtos.PostReviewVerdictRequest.findings
	// is OPTIONAL, so an old caller posting no findings at all (every
	// caller before this Step) keeps posting exactly as before: nil/empty
	// is a fully legitimate value, never rejected by ValidateVerdictInput
	// below. See finding.go's own doc comment for why this type lives
	// here, in reviewpost, rather than as a new review.Verdict field.
	Findings []FindingInput

	// Digest is §26.1's own additive extension (§26.1): the merge-
	// readout's typed content -- restdtos.PostReviewVerdictRequest.digest
	// is REQUIRED (unlike Findings above), and Digest.Summary within it is
	// unconditionally enforced by ValidateVerdictInput below; Digest.
	// ArchDecisions/StackRisks/UnverifiedLimits are requested on every
	// review (the review-turn prompt, review/context.go, asks the agent
	// to fill them), and (§26.3) become REQUIRED as well whenever
	// this VerdictInput's own ReviewDepth is reviewtriage.DepthDeep -- see
	// digest.go's own doc comment for the full "why", and this field's
	// own struct type for why it lives here rather than as a new
	// review.Verdict field.
	Digest Digest

	// ReviewDepth (§26.3) is the posting turn's own resolved
	// light/deep routing decision (turns.review_depth) -- threaded
	// through by the caller (internal/adapters/inbound/httpapi/
	// reviewverdict.go), never accepted from the agent's own POST body
	// (mirrors Shippable's own "server-computed only" contract: the
	// AGENT never gets to claim which path it ran on). The zero value
	// (reviewtriage.ReviewDepth("")) means "no resolvable depth for this
	// turn" -- ValidateVerdictInput below treats that identically to
	// DepthLight (never requiring the full digest), since a turn with no
	// recorded depth was never routed deep by construction.
	ReviewDepth reviewtriage.ReviewDepth

	// CounterReview (§26.4) is the deep path's own structural-
	// enforcement signal: whether the primary reviewer's orchestration
	// actually spawned and adjudicated the `counter-reviewer` sub-task
	// (§7.1's engine-native fan-out) before posting this verdict.
	// SCHEMA-REQUIRED (one of review.CounterReviewDone/CounterReviewSkipped,
	// ValidateVerdictInput's own ErrInvalidCounterReview) ONLY when
	// ReviewDepth == reviewtriage.DepthDeep -- unvalidated, and never fed
	// to the counter-review floor as-is, on every other path (see
	// BuildVerdict's own doc comment for the light-path substitution).
	// Never review.CounterReviewUncorroborated: that is the server's own
	// finding, which BuildVerdict alone produces, so the deep-path check
	// rejects it from a payload like any other unrecognized value.
	CounterReview review.CounterReviewStatus

	// FactCheck (§26.6) is the diff-only fact-check sub-task's
	// own outcome -- whether the primary reviewer's orchestration spawned
	// it at all before posting this verdict. SCHEMA-REQUIRED
	// UNCONDITIONALLY (validate.go's own ErrInvalidFactCheck check, both
	// paths, not merely deep) -- the fact-check pass runs on every review
	// regardless of depth (§26.6: "runs on the light path too"). Never an
	// input to ANY Shippable floor -- see FactCheckStatus's own doc
	// comment (factcheck.go) for why FactCheck:skipped, unlike
	// CounterReview:skipped above, never raises Shippable.
	FactCheck FactCheckStatus
	// FactCheckKilled (§26.6) is the count of findings the
	// fact-check pass actually removed as provably wrong from the diff
	// alone -- 0 when FactCheck == FactCheckSkipped (a skipped pass, by
	// construction, kills nothing), and always >= 0 otherwise
	// (ValidateVerdictInput's own ErrNegativeFactCheckKilled /
	// ErrFactCheckKilledOnSkip checks).
	FactCheckKilled int

	// CounterReviewCorroborated (§26.4) is what makes §26.4's own
	// "structural enforcement" heading honestly so: whether the caller
	// (internal/adapters/inbound/httpapi/reviewverdict.go) independently
	// confirmed, against THIS turn's own already-persisted sandbox event
	// trace (reviewverdict.CounterReviewCorroborated, gen-scoped to
	// turns.dispatched_sandbox_gen), that a counter-reviewer sub-task
	// (review.CounterReviewerAgentName) both actually started and actually
	// completed -- never merely that CounterReview above claims "done".
	// Computed server-side ONLY, never accepted from the agent's own POST
	// body (mirrors ReviewDepth/Shippable's own "server-computed only"
	// contract above) -- there is no restdtos wire field this is ever read
	// from. The caller ONLY runs the corroboration query, and therefore
	// only ever sets this true, when it could possibly matter (deep path,
	// CounterReview == done) -- see that call site's own doc comment for
	// why every other verdict leaves this at its zero value, false, by
	// simply never querying at all. BuildVerdict's own second substitution
	// (below) is the ONE place this field is read.
	CounterReviewCorroborated bool
}

// MaxDigestSummaryBytes/MaxDigestAdequacyExplanationBytes/
// MaxDigestStackRisksBytes/MaxDigestUnverifiedLimitsBytes/
// MaxDigestProposedBodyBytes/MaxDigestContestedPointsBytes/
// MaxArchDecisionFieldBytes (hardening, G3) cap the byte length of
// every model-authored free-text digest field ValidateVerdictInput
// enforces below -- mirroring internal/domain/upload's own MaxFilenameBytes/
// MaxContentTypeBytes precedent (upload/validate.go) for the identical
// reason that doc comment gives: before this Step, none of these fields had
// ANY length limit at all, which is both a token-budget denial-of-service
// against §26.7's own per-review cost budget (an unbounded digest field
// inflates every FUTURE prompt a knowledge/projection read path re-injects
// it into, once one exists -- reviewpost/sanitize.go's own "why the write
// path too" reasoning applies here verbatim) and an amplifier for the
// SAME placeholder-token/delimiter-forgery injection surface
// reviewpost.SanitizeDigest (sanitize.go) neutralizes the CONTENT of but
// not the SIZE of.
//
// No field in §26.1/§26.2/§26.4's own spec text (docs/TECHNICAL_PLAN.md)
// names a concrete byte limit for any of these, so each cap below is this
// package's own deliberate choice, sized from what each field's own doc
// comment (digest.go) says it should actually contain, with generous
// headroom over the realistic case rather than a tight fit:
//   - Digest.Summary is documented as "2-4 sentences" -- a few hundred
//     bytes in practice; 2000 gives roughly 5x headroom.
//   - Digest.AdequacyExplanation is documented as "a one-line explanation"
//     -- shorter still; 1000 bytes is already generous for one line.
//   - Digest.StackRisks/UnverifiedLimits/ContestedPoints are each a prose
//     paragraph (coupling/deployment/reversibility risks; honest "not
//     verified" limits; inter-agent disagreement) that may reasonably name
//     several distinct points -- 4000 bytes (roughly 600-700 words) comfortably
//     covers a real one without bounding it to a single sentence.
//   - Digest.ProposedBody is categorically different: a full PR-body
//     REWRITE proposal, not a summary sentence -- 20000 bytes is sized to
//     comfortably hold even a long, multi-paragraph PR description while
//     staying a small fraction of GitHub's own 65536-character PR-body
//     ceiling.
//   - Each ArchDecision field (Decision/RejectedAlternative/
//     ConventionConformance) is documented via one-sentence examples
//     ("introduced a new retry queue table rather than reusing the
//     existing outbox") -- 2000 bytes per field, matching Digest.Summary's
//     own cap, is generous for a single structural-decision sentence.
//
// Deliberately scoped to Digest's own fields ONLY (the write-path attack
// surface this Step's own G1/G3 hardening targets, reviewpost/sanitize.go's
// own top doc comment) -- VerdictInput.Summary (the pre-existing,
// never-persisted-to-review_verdicts narrative, see insert.go's own
// InsertReviewVerdictParams, which carries no plain "summary" column) and
// Finding.Description (finding.go) are both out of scope for this Step,
// neither named by G1's own "every model-authored free-text field on the
// digest" framing nor written to review_verdicts by the digest_* columns
// this hardening targets.
const (
	MaxDigestSummaryBytes             = 2000
	MaxDigestAdequacyExplanationBytes = 1000
	MaxDigestStackRisksBytes          = 4000
	MaxDigestUnverifiedLimitsBytes    = 4000
	MaxDigestProposedBodyBytes        = 20000
	MaxDigestContestedPointsBytes     = 4000
	MaxArchDecisionFieldBytes         = 2000
)

// The errors ValidateVerdictInput returns -- one per rejected field, named
// distinctly so a caller (internal/adapters/inbound/httpapi/
// reviewverdict.go) can render a specific, actionable 400 body rather than
// a single generic "invalid payload" message, and so a table-driven test
// can assert exactly WHICH check fired for a given malformed/partial
// input.
var (
	ErrInvalidRiskLevel         = errors.New("reviewpost: riskLevel must be one of low/medium/high")
	ErrInvalidPremise           = errors.New("reviewpost: premise must be one of ok/questionable/not_a_pr")
	ErrInvalidTestsCoverage     = errors.New("reviewpost: testsCoverage must be one of adequate/insufficient/skipped")
	ErrInvalidDocsDrift         = errors.New("reviewpost: docsDrift must be one of none/found/skipped")
	ErrInvalidProposedShippable = errors.New("reviewpost: proposedShippable must be one of auto/needs_human/block")
	ErrInvalidBlastRadiusTag    = errors.New("reviewpost: blastRadius contains an unrecognized tag")
	ErrNegativeFilesChanged     = errors.New("reviewpost: filesChanged must not be negative")
	ErrEmptySummary             = errors.New("reviewpost: summary must not be empty")
	// ErrEmptyDigestSummary is §26.1's own addition (§26.1): "Digest.Summary
	// is required on every review from day one" -- mirrors ErrEmptySummary
	// above exactly (same empty/whitespace-only check, same "missing and
	// garbled are the identical failure" posture), for the digest's own
	// "what this PR does" field rather than the verdict's overall narrative.
	ErrEmptyDigestSummary = errors.New("reviewpost: digest.summary must not be empty")
	// ErrInvalidDescriptionAdequacy is §26.2's own addition:
	// digest.descriptionAdequacy must be one of review's own three closed
	// DescriptionAdequacy values -- mirrors ErrInvalidPremise/
	// ErrInvalidRiskLevel's own identical closed-enum check shape, applied
	// here rather than treated like Digest.Summary's weaker "just
	// non-blank" check, since DescriptionAdequacy is a validated enum that
	// directly feeds review.ComputeShippable's own third floor, not free
	// text.
	ErrInvalidDescriptionAdequacy = errors.New("reviewpost: digest.descriptionAdequacy must be one of ok/drift/misleading")
	// ErrEmptyAdequacyExplanation is §26.2's own addition:
	// digest.adequacyExplanation must not be empty/whitespace-only --
	// mirrors ErrEmptySummary/ErrEmptyDigestSummary's own identical
	// "missing and garbled are the identical failure" posture, for the
	// tri-state's own required one-line explanation.
	ErrEmptyAdequacyExplanation = errors.New("reviewpost: digest.adequacyExplanation must not be empty")
	// ErrEmptyDigestArchDecisions/ErrEmptyDigestStackRisks/
	// ErrEmptyDigestUnverifiedLimits are §26.3's own addition (§26.3,
	// via §26.1's own forward reference: "the full digest ... becomes
	// schema-required on the deep path once §26.3 defines it") -- the
	// three digest fields §26.1 requested but deliberately left
	// unenforced ("explicit future work, §26.3, once a 'deep
	// path' exists for it to attach to", reviewpost/digest.go's own doc
	// comment) are now REQUIRED, but ONLY when in.ReviewDepth ==
	// reviewtriage.DepthDeep -- see ValidateVerdictInput's own check
	// below. Each mirrors ErrEmptyDigestSummary's identical "missing and
	// garbled are the identical failure" empty/whitespace-only check.
	ErrEmptyDigestArchDecisions    = errors.New("reviewpost: digest.archDecisions must not be empty on a deep-path review")
	ErrEmptyDigestStackRisks       = errors.New("reviewpost: digest.stackRisks must not be empty on a deep-path review")
	ErrEmptyDigestUnverifiedLimits = errors.New("reviewpost: digest.unverifiedLimits must not be empty on a deep-path review")
	// ErrInvalidFactCheck is §26.6's own addition: factCheck must
	// be one of review's own two closed FactCheckStatus values --
	// SCHEMA-REQUIRED UNCONDITIONALLY (both paths, never gated behind
	// ReviewDepth, unlike the deep-path-only digest checks above) since
	// the fact-check pass itself runs on every review, light and deep
	// alike (§26.6: "runs on the light path too").
	ErrInvalidFactCheck = errors.New("reviewpost: factCheck must be one of done/skipped")
	// ErrNegativeFactCheckKilled is §26.6's own addition: mirrors
	// ErrNegativeFilesChanged's own identical "a count field must never be
	// negative" check.
	ErrNegativeFactCheckKilled = errors.New("reviewpost: factCheckKilled must not be negative")
	// ErrFactCheckKilledOnSkip is §26.6's own addition: a SKIPPED
	// fact-check pass, by construction, removed nothing -- "FactCheckKilled
	// int (the count removed, 0 when skipped)" (§26.6, verbatim). A
	// non-zero count paired with FactCheckSkipped is not a value this
	// package can honestly persist (it would read, to any future KPI over
	// review_verdicts.fact_check_killed, as findings the pass DID prune
	// despite never having run at all) -- rejected outright, the SAME
	// reject-don't-repair posture every other structurally-inconsistent
	// combination in this function already gets, rather than silently
	// clamped to 0 or silently trusted.
	ErrFactCheckKilledOnSkip = errors.New("reviewpost: factCheckKilled must be 0 when factCheck is skipped")
	// ErrInvalidCounterReview is §26.4's own addition:
	// counterReview must be one of review's own two closed
	// CounterReviewStatus values -- SCHEMA-REQUIRED ONLY on the deep path
	// (in.ReviewDepth == reviewtriage.DepthDeep), mirroring
	// ErrEmptyDigestArchDecisions/ErrEmptyDigestStackRisks/
	// ErrEmptyDigestUnverifiedLimits' own identical deep-path-only
	// treatment immediately above -- never checked at all on the light
	// path, where counter-review has no meaning (§26.9).
	ErrInvalidCounterReview = errors.New("reviewpost: counterReview must be one of done/skipped on a deep-path review")
	// ErrDigestSummaryTooLong/ErrDigestAdequacyExplanationTooLong/
	// ErrDigestStackRisksTooLong/ErrDigestUnverifiedLimitsTooLong/
	// ErrDigestProposedBodyTooLong/ErrDigestContestedPointsTooLong/
	// ErrDigestArchDecisionFieldTooLong (hardening, G3) -- the
	// MaxDigest*Bytes/MaxArchDecisionFieldBytes caps' own rejection errors
	// (see those consts' own doc comment, above the errors block, for the
	// full "why" and how each limit was chosen). Checked LAST of all,
	// AFTER the Findings loop -- this function's own "each added at the
	// end of the existing fixed order" discipline (top doc comment):
	// appending these at the very end, rather than interleaving each cap
	// beside its own field's existing non-blank/enum check, keeps every
	// EXISTING malformed payload (one that already fails an earlier
	// check) reporting the exact SAME first error it always did. Checked
	// UNCONDITIONALLY -- never gated on in.ReviewDepth == DepthDeep,
	// unlike ArchDecisions/StackRisks/UnverifiedLimits' own non-blank
	// checks above: an oversized field is exactly as much of a
	// token-budget/injection-surface hazard on the light path (where these
	// fields are legal but optional) as on the deep path (where three of
	// them are additionally required non-blank) -- "required" and
	// "bounded" are independent axes, and this Step's own G3 scope is
	// bounding, regardless of whether a given field happens to also be
	// required on this verdict's own path.
	ErrDigestSummaryTooLong             = errors.New("reviewpost: digest.summary exceeds the maximum length")
	ErrDigestAdequacyExplanationTooLong = errors.New("reviewpost: digest.adequacyExplanation exceeds the maximum length")
	ErrDigestStackRisksTooLong          = errors.New("reviewpost: digest.stackRisks exceeds the maximum length")
	ErrDigestUnverifiedLimitsTooLong    = errors.New("reviewpost: digest.unverifiedLimits exceeds the maximum length")
	ErrDigestProposedBodyTooLong        = errors.New("reviewpost: digest.proposedBody exceeds the maximum length")
	ErrDigestContestedPointsTooLong     = errors.New("reviewpost: digest.contestedPoints exceeds the maximum length")
	ErrDigestArchDecisionFieldTooLong   = errors.New("reviewpost: digest.archDecisions contains a field exceeding the maximum length")
)

// ValidateVerdictInput rejects a malformed or partial verdict-posting-tool
// call -- every enum field is checked against review's own EXPORTED
// consts (never a bare string comparison against a literal, so a future
// addition to any of those enums only ever needs a switch case added
// here, never a parallel vocabulary invented independently). Checked in a
// fixed order (RiskLevel, Premise, TestsCoverage, DocsDrift,
// ProposedShippable, BlastRadius, FilesChanged, Summary, Digest.Summary,
// Digest.DescriptionAdequacy, Digest.AdequacyExplanation, FactCheck/
// FactCheckKilled (§26.6, unconditional), Digest.ArchDecisions/
// StackRisks/UnverifiedLimits/CounterReview (§26.4, ONLY when
// in.ReviewDepth == reviewtriage.DepthDeep), Findings (§8.2), and --
// hardening, G3, LAST of all -- the seven digest length caps
// (Digest.Summary/AdequacyExplanation/StackRisks/UnverifiedLimits/
// ProposedBody/ContestedPoints/each ArchDecision field, unconditional on
// path)) so a caller presenting more than one bad field always gets the
// SAME, deterministic first error rather than one that depends on map
// iteration order or similar. Digest.Summary is checked next (
// §26.1's own new required field), Digest.DescriptionAdequacy/Digest.
// AdequacyExplanation follow (§26.2's own new required fields),
// FactCheck/FactCheckKilled follow those (§26.6's own new
// UNCONDITIONAL fields -- checked before the deep-path-only block, never
// inside it, since they apply regardless of depth), the deep-path-only
// checks (three digest fields plus CounterReview) follow those, Findings
// follows that, and the length caps are checked LAST of all -- each
// addition appended at the end of the existing fixed order rather than
// interleaved earlier, so no Step ever changes which error an EXISTING
// malformed payload (one that already fails an earlier-checked field) was
// already reporting before it shipped.
//
// Every one of review's four "closed enum" types has a Go zero value
// ("") that is deliberately not a legal member (review/doc.go's own
// "fail-conservative policy for every closed enum" section) -- an absent
// JSON field decodes to that same zero value, so a MISSING field and a
// GARBLED one are rejected by the identical check here, never
// distinguished (there is no principled difference between "the caller
// forgot this field" and "the caller sent a value this package doesn't
// recognize" from a validation standpoint -- both mean the payload cannot
// be trusted to construct an authoritative Verdict).
func ValidateVerdictInput(in VerdictInput) error {
	switch in.RiskLevel {
	case review.RiskLevelLow, review.RiskLevelMedium, review.RiskLevelHigh:
	default:
		return ErrInvalidRiskLevel
	}

	switch in.Premise {
	case review.PremiseStateOK, review.PremiseStateQuestionable, review.PremiseStateNotAPR:
	default:
		return ErrInvalidPremise
	}

	switch in.TestsCoverage {
	case review.TestsCoverageStateAdequate, review.TestsCoverageStateInsufficient, review.TestsCoverageStateSkipped:
	default:
		return ErrInvalidTestsCoverage
	}

	switch in.DocsDrift {
	case review.DocsDriftStateNone, review.DocsDriftStateFound, review.DocsDriftStateSkipped:
	default:
		return ErrInvalidDocsDrift
	}

	switch in.ProposedShippable {
	case review.ProposedShippableAuto, review.ProposedShippableNeedsHuman, review.ProposedShippableBlock:
	default:
		return ErrInvalidProposedShippable
	}

	for _, tag := range in.BlastRadius {
		switch tag {
		case review.TagAuth, review.TagMigrations, review.TagContracts, review.TagSecrets,
			review.TagInfra, review.TagPublicAPI, review.TagDataLayer, review.TagDependencies:
		default:
			return ErrInvalidBlastRadiusTag
		}
	}

	if in.FilesChanged < 0 {
		return ErrNegativeFilesChanged
	}

	if strings.TrimSpace(in.Summary) == "" {
		return ErrEmptySummary
	}

	// Digest.Summary (§26.1): "required on every review from
	// day one" -- the ONE digest field required on EVERY review,
	// light and deep alike. ArchDecisions/StackRisks/UnverifiedLimits are
	// NOT checked here -- they are requested via the prompt (review/
	// context.go) on every review, and validation-enforced separately,
	// below, but ONLY on the deep path (§26.3) -- see that
	// check's own doc comment further down for the full "why".
	if strings.TrimSpace(in.Digest.Summary) == "" {
		return ErrEmptyDigestSummary
	}

	// Digest.DescriptionAdequacy (§26.2): a closed-enum check,
	// mirroring RiskLevel/Premise/TestsCoverage/DocsDrift/ProposedShippable
	// above rather than Digest.Summary's own weaker "just non-blank"
	// check -- this field directly feeds review.ComputeShippable's own
	// third raise-only floor (review.AdequacyFloor), so an unvalidated
	// value here would let a garbled/missing assessment silently reach
	// that computation instead of being rejected up front. REQUIRED on
	// EVERY review, light and deep alike -- unlike ArchDecisions/
	// StackRisks/UnverifiedLimits below, this was never deferred behind
	// the light/deep distinction §26.3 draws.
	switch in.Digest.DescriptionAdequacy {
	case review.DescriptionAdequacyOK, review.DescriptionAdequacyDrift, review.DescriptionAdequacyMisleading:
	default:
		return ErrInvalidDescriptionAdequacy
	}

	// Digest.AdequacyExplanation (§26.2): "plus a one-line
	// explanation" -- REQUIRED non-blank, mirroring Summary/Digest.
	// Summary's own identical "missing and garbled are the identical
	// failure" treatment for a free-text narrative field.
	if strings.TrimSpace(in.Digest.AdequacyExplanation) == "" {
		return ErrEmptyAdequacyExplanation
	}

	// FactCheck/FactCheckKilled (§26.6): a closed-enum check,
	// mirroring Digest.DescriptionAdequacy's own treatment immediately
	// above -- SCHEMA-REQUIRED UNCONDITIONALLY, both paths, since the
	// fact-check pass itself runs on every review regardless of depth
	// (§26.6: "runs on the light path too" -- the light path's own single
	// review turn spawns exactly one fact-check sub-task"). Checked here,
	// BEFORE the deep-path-only block below, so an unconditional check
	// never has its own first-error-wins position made to depend on
	// ReviewDepth.
	switch in.FactCheck {
	case FactCheckDone, FactCheckSkipped:
	default:
		return ErrInvalidFactCheck
	}
	if in.FactCheckKilled < 0 {
		return ErrNegativeFactCheckKilled
	}
	if in.FactCheck == FactCheckSkipped && in.FactCheckKilled != 0 {
		return ErrFactCheckKilledOnSkip
	}

	// Digest.ArchDecisions/StackRisks/UnverifiedLimits (§26.3,
	// via §26.1's own forward reference): schema-required ONLY on the
	// deep path -- checked LAST, after every field the light path already
	// requires, so a light-path or pre-existing (ReviewDepth == "") caller
	// keeps failing on exactly the SAME first error it always did (this
	// function's own "fixed order" discipline, top doc comment). The
	// posting endpoint's own reject-don't-repair posture (§26.1) means
	// the agent re-submits with a real digest rather than this package
	// ever repairing/fabricating one. review.RenderTurnPrompt's own
	// verdictToolInstructions(deep) (context.go, D2's own fix) is what
	// tells a deep-path agent these three fields are REQUIRED rather than
	// merely requested -- this check is that promise's enforcement half.
	//
	// hasNonBlankArchDecision (adversarial-review fix, D2's own "hollow
	// check" aggravator): ArchDecisions is checked for at least one
	// entry carrying real content, NOT merely len() > 0. A bare
	// len(in.Digest.ArchDecisions) == 0 check would pass a payload
	// carrying exactly one ArchDecision{} with all three fields blank --
	// technically a non-empty slice, but exactly as uninformative as
	// submitting none at all, and indistinguishable from a caller padding
	// the array purely to slip past this check.
	if in.ReviewDepth == reviewtriage.DepthDeep {
		if !hasNonBlankArchDecision(in.Digest.ArchDecisions) {
			return ErrEmptyDigestArchDecisions
		}
		if strings.TrimSpace(in.Digest.StackRisks) == "" {
			return ErrEmptyDigestStackRisks
		}
		if strings.TrimSpace(in.Digest.UnverifiedLimits) == "" {
			return ErrEmptyDigestUnverifiedLimits
		}
		// CounterReview (§26.4): schema-required ONLY on the deep
		// path, appended LAST within this deep-path-only block (this
		// function's own "each added at the end of the existing fixed
		// order" discipline, top doc comment) -- rejected if absent or
		// garbled, following the exact reject-don't-repair pattern the
		// three checks immediately above already establish for this SAME
		// deep-path-only block. Never checked at all on the light path
		// (in.ReviewDepth != reviewtriage.DepthDeep skips this whole
		// block) -- counter-review has no meaning there (§26.9), and
		// BuildVerdict's own light-path substitution (validate.go's
		// BuildVerdict doc comment) is what keeps an unvalidated
		// in.CounterReview from ever reaching CounterReviewFloor on that
		// path.
		switch in.CounterReview {
		case review.CounterReviewDone, review.CounterReviewSkipped:
		default:
			return ErrInvalidCounterReview
		}
	}

	// Findings (§8.2, additive): each one validated by
	// ValidateFindingInput, in order -- first bad finding wins, mirroring
	// this function's own fixed-order, first-error-wins discipline above.
	// nil/empty Findings is not iterated at all, so an old caller posting
	// none is never rejected here.
	for _, f := range in.Findings {
		if err := ValidateFindingInput(f); err != nil {
			return err
		}
	}

	// Digest field length caps (hardening, G3) -- checked LAST of
	// all, after every other check above including the Findings loop; see
	// the Max*Bytes consts' own doc comment and each Err*TooLong error's
	// own doc comment (both above) for the full "why here, why
	// unconditional" reasoning. len() is a raw BYTE count (not a rune
	// count) -- "hard byte cap", matching internal/domain/upload's own
	// identical MaxFilenameBytes/MaxContentTypeBytes treatment
	// (upload/validate.go).
	if len(in.Digest.Summary) > MaxDigestSummaryBytes {
		return ErrDigestSummaryTooLong
	}
	if len(in.Digest.AdequacyExplanation) > MaxDigestAdequacyExplanationBytes {
		return ErrDigestAdequacyExplanationTooLong
	}
	if len(in.Digest.StackRisks) > MaxDigestStackRisksBytes {
		return ErrDigestStackRisksTooLong
	}
	if len(in.Digest.UnverifiedLimits) > MaxDigestUnverifiedLimitsBytes {
		return ErrDigestUnverifiedLimitsTooLong
	}
	if len(in.Digest.ProposedBody) > MaxDigestProposedBodyBytes {
		return ErrDigestProposedBodyTooLong
	}
	if len(in.Digest.ContestedPoints) > MaxDigestContestedPointsBytes {
		return ErrDigestContestedPointsTooLong
	}
	for _, ad := range in.Digest.ArchDecisions {
		if len(ad.Decision) > MaxArchDecisionFieldBytes ||
			len(ad.RejectedAlternative) > MaxArchDecisionFieldBytes ||
			len(ad.ConventionConformance) > MaxArchDecisionFieldBytes {
			return ErrDigestArchDecisionFieldTooLong
		}
	}

	return nil
}

// hasNonBlankArchDecision reports whether decisions contains at least one
// ArchDecision with a real (non-blank, after trimming) value in ANY of its
// three fields -- ValidateVerdictInput's own deep-path ArchDecisions check
// calls this instead of a bare len() > 0 test (see that call site's own
// doc comment for the "hollow check" this closes). An entry whose
// Decision/RejectedAlternative/ConventionConformance are ALL blank
// contributes nothing a human merge-readout reader could use, so a slice
// containing only such entries is treated exactly like an empty slice --
// nil/empty decisions trivially returns false, the loop below simply never
// running.
func hasNonBlankArchDecision(decisions []ArchDecision) bool {
	for _, d := range decisions {
		if strings.TrimSpace(d.Decision) != "" || strings.TrimSpace(d.RejectedAlternative) != "" || strings.TrimSpace(d.ConventionConformance) != "" {
			return true
		}
	}
	return false
}

// BuildVerdict is the ONE sanctioned way this package turns an
// ALREADY-VALIDATED VerdictInput (ValidateVerdictInput must be called
// first -- BuildVerdict does not re-validate) into an authoritative
// review.Verdict: Shippable is populated with EXACTLY the Class of
// review.ComputeShippable's own result, never in.ProposedShippable
// converted, matching review.Verdict's own CONTRACT to the letter (the
// caller's ProposedShippable is still carried onto the result, verbatim,
// as pure audit/transparency data -- it simply never influences
// Shippable's own computation, since ComputeShippable's signature does not
// accept it at all).
//
// That result is returned beside the Verdict (§26.1): the
// review.ShippableAssessment whose Class the Verdict's Shippable is, and
// whose Blockers name every input that keeps it above auto.
// RenderVerdictComment renders the class and its blockers from this one
// value, so the posted readout's class and the reasons it gives cannot
// come from two computations. The Verdict itself carries no blockers
// (review/doc.go's design call #10).
//
// in.Digest.DescriptionAdequacy (§26.2) is threaded through as
// ComputeShippable's own fourth argument, the THIRD raise-only floor --
// composed via the SAME max(rank) as coverage/premise, never a special
// case. RiskLevel below is set from in.RiskLevel VERBATIM, completely
// independent of this call: §26.2's own explicit "deliberately never
// inflating RiskLevel" asymmetry is structurally guaranteed here by
// construction, not by any runtime check -- RiskLevel is assigned once,
// above, from the caller's own self-reported field, and nothing about
// in.Digest.DescriptionAdequacy (or any other floor input) can reach that
// assignment.
//
// counterReviewForFloor (§26.4) is this function's own resolution
// of ComputeShippable's fifth argument, the FOURTH raise-only floor --
// NEVER in.CounterReview forwarded verbatim. On a deep-path verdict
// (in.ReviewDepth == reviewtriage.DepthDeep), ValidateVerdictInput has
// already rejected anything but review.CounterReviewDone/
// CounterReviewSkipped by the time this function runs (its own
// ErrInvalidCounterReview check, below), so in.CounterReview is forwarded
// as-is. On every OTHER verdict (light path, or ReviewDepth's own zero
// value, "no resolvable depth" -- VerdictInput.ReviewDepth's own doc
// comment) this field carries NO validated meaning at all: the light path
// never runs a counter-reviewer sub-task (§26.9), so in.CounterReview is
// whatever zero-value/unset field an agent that was never asked to
// populate it happens to submit -- review.CounterReviewFloor's own
// fail-conservative policy would otherwise read that blank value via its
// own default branch and floor EVERY light-path verdict to needs_human,
// silently defeating light-path auto-approval entirely.
//
// B11 fix: the substitution is skipped -- in.CounterReview is forwarded
// as-is instead -- when in.CounterReview is EXPLICITLY
// review.CounterReviewSkipped, even on a verdict this function cannot
// confirm is genuinely deep-path. An agent reporting "skipped" is an
// honest, explicit self-report that an adversarial counter-review did not
// happen, for WHATEVER reason (§26.7's own "each field's already-decided
// Shippable consequence applies unchanged and un-special-cased" wording,
// review.CounterReviewStatus's own doc comment) -- silently laundering
// that explicit signal into CounterReviewDone here, solely because
// in.ReviewDepth did not read back as DepthDeep (which could be the
// verdict's own genuine light-path status, but could just as easily be a
// misrouted/blank ReviewDepth on what was actually meant to be an
// adversarially-reviewed PR), would erase the SAME "did not get an
// adversarial counter-review" signal §26.4 says must float the floor to
// needs_human. This carve-out can only ever make a verdict's own
// Shippable MORE conservative than the unconditional substitution did,
// never less -- it does not touch the blank/unset case the substitution
// exists for in the first place (blank != CounterReviewSkipped, so that
// case still substitutes exactly as before). This function is the one
// place VerdictInput's own depth is in scope (reviewpost already imports
// both review and reviewtriage; review itself cannot, doc.go's own "zero
// external imports" convention) and therefore the one place this
// substitution belongs -- see review.CounterReviewStatus's own doc comment
// for why CounterReviewFloor itself stays a plain, depth-unaware pure
// function rather than growing a parameter for this. Pinned by
// TestBuildVerdict_CounterReviewFloorInertOnLightPath and
// TestBuildVerdict_ExplicitCounterReviewSkippedNeverOverwrittenOnLightPath
// (validate_test.go).
//
// # Second substitution: post-hoc corroboration (§26.4)
//
// Immediately after the light-path substitution above, a SECOND
// substitution closes §26.4's own named residual: a schema-required
// `CounterReview: done` self-report is presence-verified by
// ValidateVerdictInput's own closed-enum check, but truth-verified by
// nothing -- a primary reviewer that never actually dispatched the
// counter-reviewer sub-task can still self-report "done". When the caller
// (httpapi) has independently confirmed against this turn's own
// persisted sandbox event trace (reviewverdict.CounterReviewCorroborated,
// gen-scoped to the turn's own dispatched_sandbox_gen) that the claim
// does NOT hold up, this substitution sets counterReviewForFloor to
// review.CounterReviewUncorroborated -- a value of its own, floored by
// CounterReviewFloor to ShippableNeedsHuman exactly as an honest
// "skipped" self-report is, and kept apart from skipped so the blocker it
// produces names what happened: the reviewer said done and the server
// could not confirm it (§26.1). This can only ever make Shippable MORE
// conservative than the self-report alone would, mirroring the first
// substitution's own "never less permissive" direction and the B11
// carve-out's identical posture.
//
// The gate is REQUIRED to be in.ReviewDepth == reviewtriage.DepthDeep
// EXPLICITLY -- not merely in.CounterReview == review.CounterReviewDone --
// and this is the one place getting the gate wrong would be a real bug,
// not a style nit. Reason: by the time this line runs, the FIRST
// substitution has already forced counterReviewForFloor to
// review.CounterReviewDone on every light-path verdict (the branch
// immediately above) -- but in.CounterReview itself, the RAW field this
// second substitution's condition reads, is untouched by that first
// substitution, and VerdictInput.CounterReview's own doc comment already
// warns that on the light path this raw field "carries NO validated
// meaning at all": ValidateVerdictInput never even looks at it outside
// the deep-path-only block, so a light-path agent's payload can echo
// "done" (or anything else) into that field with zero consequence today.
// A gate reading only "in.CounterReview == review.CounterReviewDone"
// would ALSO match that light-path echo, and -- since httpapi's own
// corroboration query only ever runs on the deep path (VerdictInput.
// CounterReviewCorroborated's own doc comment: "ONLY when it could
// possibly matter") -- in.CounterReviewCorroborated is ALWAYS false, its
// own zero value, on every light-path verdict, with no exception. Gating
// on the raw CounterReview value alone would therefore silently floor
// EVERY light-path verdict whose payload happens to carry
// CounterReview:"done" straight to ShippableNeedsHuman via this second
// substitution -- reintroducing, through a different door, the EXACT
// "silently defeating light-path auto-approval entirely" failure mode the
// FIRST substitution's own doc comment exists to prevent. Requiring
// in.ReviewDepth == reviewtriage.DepthDeep explicitly closes that door:
// only a genuinely deep-path verdict, where in.CounterReview was actually
// validated and in.CounterReviewCorroborated was actually computed from a
// real query, can ever reach this branch.
//
// # The accepted race: not a bug, do not "fix" it
//
// httpapi's own corroboration query call site (reviewverdict.go) is a
// sandbox-bearer-authenticated HTTP POST -- a channel entirely separate
// from the WS event stream that carries sub_task_finish. The deep-path
// review prompt (review/context.go's own orchestration instructions)
// tells the agent to wait for the counter-reviewer sub-task's own result
// before composing this verdict, so CAUSALLY the sub-task has already
// resolved by the time this POST is made -- but there is no server-side
// guarantee the corresponding sub_task_finish WS event has actually been
// committed to Postgres yet: two independent network round-trips from the
// sandbox, no ordering guarantee between them. A false negative here (the
// counter-review genuinely completed, but its own finish event's own
// commit has not landed by the time this POST is processed) fails toward
// ShippableNeedsHuman -- MORE conservative, never less, the identical
// fail-conservative bias review.CounterReviewSkipped's own doc comment
// already commits to ("every cause floors identically... whatever the
// reason"). This is accepted, not a defect: no retries, no polling, no
// new timeout constant belongs here to chase it away.
func BuildVerdict(in VerdictInput) (review.Verdict, review.ShippableAssessment) {
	counterReviewForFloor := in.CounterReview
	if in.ReviewDepth != reviewtriage.DepthDeep && in.CounterReview != review.CounterReviewSkipped {
		counterReviewForFloor = review.CounterReviewDone
	}
	// Second substitution (§26.4) -- see this function's own doc
	// comment above ("Second substitution: post-hoc corroboration") for
	// the full "why", especially why this gate is in.ReviewDepth ==
	// reviewtriage.DepthDeep EXPLICITLY and not merely in.CounterReview ==
	// review.CounterReviewDone.
	if in.ReviewDepth == reviewtriage.DepthDeep && in.CounterReview == review.CounterReviewDone && !in.CounterReviewCorroborated {
		counterReviewForFloor = review.CounterReviewUncorroborated
	}
	shippable := review.ComputeShippable(in.RiskLevel, in.TestsCoverage, in.Premise, in.Digest.DescriptionAdequacy, counterReviewForFloor)
	return review.Verdict{
		RiskLevel:         in.RiskLevel,
		Premise:           in.Premise,
		BlastRadius:       in.BlastRadius,
		FilesChanged:      in.FilesChanged,
		TestsCoverage:     in.TestsCoverage,
		DocsDrift:         in.DocsDrift,
		ProposedShippable: in.ProposedShippable,
		Shippable:         shippable.Class(),
	}, shippable
}

// BuildFindings is BuildVerdict's own per-finding sibling (§8.2,
// additive): turns an ALREADY-VALIDATED VerdictInput.Findings (every
// element already passed ValidateFindingInput, via ValidateVerdictInput's
// own loop above) into the []Finding a caller upserts into review_findings
// -- IdentityHash computed via BuildFinding for each, never client-supplied.
// nil/empty in.Findings returns nil, never a zero-length-but-non-nil
// slice, so a caller's own "did this verdict report any findings at all"
// check can use a plain len()/nil check either way.
//
// Confirmed-finding fix: ComputeFindingIdentity deliberately excludes Line
// (its own doc comment, finding.go) so a finding re-reported at a shifted
// line number still matches its own prior row -- but that SAME property
// means two genuinely DIFFERENT findings in the same file, at different
// lines, that happen to share identical (post-normalization) kind/path/
// description text would otherwise collide onto the identical
// IdentityHash and silently collapse onto the SAME review_findings row
// (UpsertReviewFinding's own ON CONFLICT clause only refreshes
// last_seen_at on a re-post of an already-known identity) -- losing one of
// the two findings entirely, and later making a maintainer's rebuttal of
// "the" finding silently suppress the OTHER, never-looked-at one too, on
// every future re-review (reconcile.go's RenderAlreadyAnsweredFacts).
//
// This function is the one place that can see every finding in the SAME
// verdict submission at once, so it is the one place that can catch this:
// when two or more elements of in.Findings hash to the SAME base identity
// (ComputeFindingIdentity's own kind/path/description triple), each one
// beyond the first is disambiguated by additionally hashing in ITS OWN
// Line (or, absent that, its own position within the colliding group) --
// see disambiguateFindingIdentity below. A base identity that occurs
// exactly once in this batch (the overwhelming common case) is left
// completely untouched, so the ordinary single-finding-per-identity,
// shifted-line-still-matches behavior this package's own tests already
// pin (TestComputeFindingIdentity_SurvivesLineShift) is unaffected.
//
// This deliberately fails toward "an under-match" (two findings that
// really are the SAME issue, reported with a shifted line NUMBER in a
// batch that also happens to contain another same-worded finding, might
// occasionally be treated as distinct across passes) rather than "an
// over-match" (two DIFFERENT findings silently collapsing into one) --
// the identical fail-conservative direction ComputeFindingIdentity's own
// doc comment already commits to for its own residual ambiguity.
func BuildFindings(in VerdictInput) []Finding {
	if len(in.Findings) == 0 {
		return nil
	}

	baseIdentities := make([]string, len(in.Findings))
	counts := make(map[string]int, len(in.Findings))
	for i, f := range in.Findings {
		h := ComputeFindingIdentity(f.SentinelKind, f.FilePath, f.Description)
		baseIdentities[i] = h
		counts[h]++
	}

	seenInGroup := make(map[string]int, len(in.Findings))
	out := make([]Finding, len(in.Findings))
	for i, f := range in.Findings {
		base := baseIdentities[i]
		identity := base
		if counts[base] > 1 {
			identity = disambiguateFindingIdentity(base, f.Line, seenInGroup[base])
			seenInGroup[base]++
		}
		out[i] = Finding{
			IdentityHash: identity,
			SentinelKind: f.SentinelKind,
			Severity:     f.Severity,
			FilePath:     f.FilePath,
			Line:         f.Line,
			Description:  f.Description,
			SuggestedFix: f.SuggestedFix,
		}
	}
	return out
}

// disambiguateFindingIdentity re-hashes base (an already-computed
// ComputeFindingIdentity value shared by two or more findings in the SAME
// verdict submission, per BuildFindings' own doc comment above) together
// with line (formatted, or a fixed "noline" token when nil) and ordinal
// (this finding's own zero-based position among the colliding group, so
// even two findings sharing both content AND line still resolve to
// distinct identities rather than one silently winning). Never called for
// a base identity that occurs only once in its own batch -- BuildFindings
// is the only caller, and only inside its own counts[base] > 1 branch.
func disambiguateFindingIdentity(base string, line *int, ordinal int) string {
	lineComponent := "noline"
	if line != nil {
		lineComponent = strconv.Itoa(*line)
	}
	joined := base + findingIdentitySeparator + lineComponent + findingIdentitySeparator + strconv.Itoa(ordinal)
	sum := sha256.Sum256([]byte(joined))
	return hex.EncodeToString(sum[:])
}
