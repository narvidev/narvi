// Package reviewpost implements the pure computation behind §8.2's
// server-side verdict-posting tool (§8.2, §5.2, §21.2): given a review
// session's own typed tool-call fields (VerdictInput), it validates them,
// builds the authoritative internal/domain/review.Verdict (Shippable
// populated ONLY via review.ComputeShippable, per that package's own
// CONTRACT), decides which review:*-risk label should be synced onto the
// pull request, decides which GitHub formal-review event (COMMENT vs
// REQUEST_CHANGES) the posting tool submits, and renders both the posted
// comment body and the server-rendered re-run guidance line every posted
// verdict carries. Every function here is pure per §11: no I/O, no
// time.Now(), no randomness -- the one non-stdlib import is internal/
// domain/review itself, for the shared Verdict/RiskLevel/Shippable/Tag
// types, exactly the same "one pure domain package depends on a sibling
// pure domain package" precedent internal/domain/session already
// establishes for internal/domain/turn.
//
// # Why a SEPARATE package from internal/domain/review, not more files in it
//
// internal/domain/review's own doc.go is explicit and deliberate: "This
// package exports exactly eight functions that compute anything:
// CoverageFloor, PremiseFloor, AdequacyFloor, CounterReviewFloor,
// ComputeShippable, ShouldRunAggregateReview,
// ComputeReleaseManifestFindings, and RenderTurnPrompt ... every other
// identifier besides these eight functions and the types/constants a
// caller needs to construct a Verdict [or drive one of the three later
// additions] is unexported." Adding
// ComputeFormalReviewEvent/RiskLabel/ComputeLabelSync/RenderVerdictComment/
// RerunGuidance/ValidateVerdictInput as new exported functions directly in
// that package would silently break an invariant its own maintainers
// documented on purpose (and which a future reviewer greps for by that
// exact count) -- so this Step's own new pure logic lives in a sibling
// package instead, importing review's types rather than extending its
// export surface. review/tag.go's own doc comment anticipates exactly
// this split: "Validating a caller-supplied tag list against this
// vocabulary... is the job of whichever Step accepts that external input
// (§8.2), not this package" -- ValidateVerdictInput (validate.go)
// is that validation, deliberately NOT added to review itself.
//
// # File layout
//   - validate.go: VerdictInput (the tool call's own typed fields before
//     Shippable is computed), ValidateVerdictInput (rejects a malformed or
//     partial payload), BuildVerdict (the one sanctioned way to turn a
//     validated VerdictInput into a review.Verdict, populating Shippable
//     via review.ComputeShippable exactly per that package's CONTRACT,
//     and returning that call's review.ShippableAssessment beside it so
//     the readout can name what keeps the class above auto, §26.1).
//   - formalreview.go: FormalReviewEvent, ComputeFormalReviewEvent -- the
//     formal-review gate's own event decision (§8.2's "submitting
//     an actual GitHub PR review rather than a comment"), and the
//     blockOnHighRisk policy flag's own effect on it (§21.2: "reuses the
//     SAME formal-review submission path and carries no independent
//     permission of its own" -- blockOnHighRisk changes only WHICH event
//     this same call submits, never a new capability).
//   - label.go: the review:*-risk label vocabulary, RiskLabel,
//     ComputeLabelSync -- deliberately NEVER touches the human-owned
//     review:needs-human escape hatch (§21.2's "existing review: low risk
//     label inverts... a maintainer... lever"), see LabelNeedsHuman's own
//     doc comment.
//   - rerunguidance.go: RerunGuidance -- the exact re-run phrasing every
//     posted verdict renders server-side (§5.2), built to be recognized by
//     GitHub ingress's own already-deterministic @mention detector
//     (internal/adapters/inbound/github's compileMentionPattern) regardless
//     of the intent classifier's own health -- see that function's own doc
//     comment for the full reasoning, and internal/adapters/inbound/github's
//     own rerunguidance_test.go for the cross-package proof this string is
//     actually matched by that real regex.
//   - rendercomment.go: RenderVerdictComment -- the full posted markdown
//     body, folding in the verdict's own typed fields, the agent-supplied
//     narrative Summary (never re-parsed back out of it afterward -- it is
//     accepted, rendered, and never read again as structured data), the
//     synced label, RerunGuidance, (§26.1) the digest sections that
//     now front the appendix findings/coverage/docs-drift content, and
//     (§26.1) the blockers that keep the Shippable class above auto,
//     rendered under the class from the same assessment.
//   - digest.go: Digest, ArchDecision -- §26.1's own merge-readout
//     content (§26.1): "what this PR does", architecture choices, and
//     risks to the stack, carried on VerdictInput alongside its
//     pre-existing fields, rendered (never re-parsed) by
//     RenderVerdictComment above. Extended by (§26.2) with
//     Digest.DescriptionAdequacy/AdequacyExplanation/ProposedBody --
//     description-adequacy tri-state, its required explanation, and the
//     agent's own optional PR-body rewrite proposal.
//   - autofixbody.go: RenderAutofixBody -- §26.2's own (§26.2) graduated-
//     remediation content: the ACTUAL new PR body text a Narvi-authored
//     PR's description gets rewritten to (proposed body + the original
//     preserved in a collapsed block), distinct from the read-only
//     suggestion rendercomment.go renders inside the posted verdict
//     comment for every PR.
package reviewpost
