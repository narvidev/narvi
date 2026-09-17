// Package autoapproval implements §21.2 stage 1's auto-approval
// eligibility engine (§21) -- the REAL replacement for §16's own
// interim stand-in, internal/domain/decisioninbox.ComputeAutoApprovalEligible
// (deleted by this Step; see that function's own former doc comment for
// the full "why this existed, and what replaces it" history this package
// makes good on). Pure per CLAUDE.md/§11: no I/O, no time.Now(), no
// randomness -- every input is already-fetched data, exactly like
// internal/domain/review's own ComputeShippable and
// internal/domain/sentinelfix's own EvaluateMergeGate.
//
// # The criteria, and where each one actually lives
//
// §21.2 states: "A PR becomes auto-approved when Shippable == auto ...
// AND every one of a deterministic, server-checked eligibility list
// holds: CI green at head; no floor raised ...; diff size under a
// configurable-per-repo threshold; no sensitive path touched ...; the
// verdict being relied on was produced against the PR's CURRENT head
// SHA." ComputeEligible below checks exactly these, in this order:
//
//  1. HasNeedsHumanLabel -- checked FIRST, as an unconditional override
//     (§21.2: "review: needs-human ... forces a specific PR out of
//     auto-approval regardless of what the criteria say"), never merely
//     one AND-condition among equals.
//
//  2. VerdictAssessed (§21.1's amendment) -- must be true. A PR with no
//     posted verdict at all has no risk level to reason about; checked
//     before this function ever reads in.Verdict for anything.
//
//  3. VerdictHeadSHA != CurrentHeadSHA (the stale-verdict guard) -- "a
//     verdict computed against an earlier commit is stale by definition
//     and must never itself satisfy eligibility, no matter how low-risk
//     it once looked" (§21.2). Checked early, deliberately BEFORE the
//     Shippable check below: a stale verdict's own Shippable value is
//     never even a fact worth reasoning about, since it was never
//     computed against the code actually under consideration.
//
//  4. The verdict's own recorded CONTEXT matches the PR's current one
//     (§21.1's amendment, stated because head equality alone is NOT
//     sufficient): VerdictBaseRef == "" means no context was ever
//     recorded (a pre-amendment row) and fails as UNKNOWN; an empty
//     VerdictBaseSHA or CurrentBaseSHA likewise fails as UNKNOWN, on its
//     own dedicated reason (Finding F2), checked BEFORE the equality
//     comparison below ever runs -- "" == "" must never read as a
//     trivially-matching pair, exactly the same hole VerdictHeadSHA's own
//     dedicated empty-string check (item 3 above) already closes for the
//     head sha; otherwise
//     VerdictBaseRef must equal CurrentBaseRef, VerdictAncestorChain must
//     equal CurrentAncestorChain (order-sensitive), and
//     VerdictPolicyVersion must equal CurrentPolicyVersion.
//
//     VerdictBaseSHA/CurrentBaseSHA and the ancestor chain's own per-link
//     SHA are the exceptions, mirroring one another (D3, second
//     adversarial-review round, for the base; round-11 finding A3 for the
//     chain): they must be EQUAL, UNLESS BaseAdvancedWithoutRewrite (base)
//     or AncestorChainAdvancedWithoutRewrite (chain) confirms the
//     difference is a pure fast-forward (an ordinary, unrelated commit,
//     never a rewrite) -- see either field's own doc comment
//     (eligibility.go) for why tolerating this is sound and why a REF
//     change (base or per-link) still refuses unconditionally regardless.
//
//     An ancestor-chain link whose SHA is empty, on EITHER side, is a
//     THIRD, distinct outcome (round-11 finding A1): review.
//     AncestorChainFromStack's own dedicated "could not be established"
//     marker for a PR whose own stack position proves a link exists but
//     whose live SHA resolution failed. Checked (via
//     ancestorChainHasUnknownLink) BEFORE the equality comparison, on its
//     own ReasonAncestorChainUnknown, distinct from BOTH "no context
//     recorded" (VerdictBaseRef == "") and "recorded and it no longer
//     matches" (ReasonAncestorChainChanged) -- exactly the same
//     three-way distinction VerdictBaseSHA/CurrentBaseSHA's own empty-
//     string check already draws for the immediate base, one link
//     further out.
//
//     Any OTHER mismatch refuses -- this is what catches a PR retargeted
//     onto a different base, or whose parent moved beneath it via an
//     actual rewrite, that CurrentHeadSHA alone cannot see.
//
//  5. EligibilityInput.CIConclusionDegraded must be false -- the live CI
//     read (check 6 below) must have actually been FULLY performed
//     before its own answer is trusted for anything: ports.OpenPR.
//     CIConclusionDegraded's own doc comment (githubapi.
//     fetchCIConclusionLive makes two independent GETs, and either can
//     fail without the other). CIGreen below is already false whenever
//     this is true -- this check changes no OUTCOME by itself -- but it
//     is checked FIRST, on its own dedicated reason
//     (ReasonCIConclusionDegraded), the same "is the fact even knowable"
//     precedence check 9 below already establishes for check 10, so a
//     half-read CI composite refuses distinguishably from a confirmed-red
//     one (CIGreen == false, this field == false) or a still-running one.
//
//  6. CIGreen -- must be true.
//
//  7. Verdict.Shippable == review.ShippableAuto.
//
//  8. EligibilityInput.ChangedFileCount <= cfg.MaxFilesChanged (diff
//     size) -- GitHub's own authoritative changed-file scalar, never
//     Verdict.FilesChanged, and (Phase 5 audit finding 2, fixed) never
//     a possibly page-truncated len() of the fetched path listing
//     either.
//
//  9. EligibilityInput.TouchedBlastRadiusKnown is true (Phase 5 audit
//     findings 1+2, fixed) -- the sensitive-path facts check 10 below
//     relies on must have actually been established from GitHub; a
//     failed or page-truncated changed-files fetch refuses here rather
//     than silently reading as "nothing sensitive touched".
//
//  10. No cfg.SensitiveTags member appears in EligibilityInput.
//     TouchedBlastRadius (no sensitive path touched) -- never
//     Verdict.BlastRadius.
//
// "No floor raised: neither the coverage floor nor the premise floor
// ... is above its baseline" is DELIBERATELY not a separate check of its
// own anywhere in the numbered list above (never conflate this with
// check 9, Phase 5 audit findings 1+2's own "is the fact even knowable"
// gate above, which exists for an entirely different reason: whether
// GitHub's changed-files data could be fetched at all, nothing to do
// with floors). internal/domain/review's own ComputeShippable composes
// RiskLevel's baseline with CoverageFloor/PremiseFloor via max(rank) --
// a RAISE-ONLY composition (review/shippable.go's own doc comment: "this
// function never returns a Shippable ranked BELOW baselineFromRisk(risk)
// alone, nor below CoverageFloor(coverage) alone, nor below
// PremiseFloor(premise) alone"). It follows MATHEMATICALLY that
// Shippable == ShippableAuto (rank 0, the most permissive rank in
// review's own explicit total order) is only ever reachable when EVERY
// one of baseline/coverage-floor/premise-floor independently evaluated
// to ShippableAuto too -- there is no way for a raised floor to
// contribute a HIGHER rank into a max() and still have the max() come
// out at the LOWEST rank. Re-deriving "no floor raised" as a second,
// independent check over the verdict's own raw RiskLevel/TestsCoverage/
// Premise fields would therefore either (a) always agree with check 7
// above, making it dead weight, or (b) disagree with it, which would
// mean domain/review's own raise-only property had a bug -- a bug this
// package has no business re-litigating a second time. Check 7 alone
// already IS "no floor raised", exactly as rigorously as a bespoke
// second check would be. This package's own test suite (eligibility_test.go)
// still exercises "a floor raised" as its own, independently named
// scenario -- via three DISTINCT Verdict fixtures (coverage floor
// raised, premise floor raised, risk baseline alone raised), each
// proving check 7 catches that specific case -- rather than by adding a
// redundant branch that could never independently fail.
//
// # IsDraft / HasChangesRequested are deliberately NOT inputs here
//
// Both of internal/app/decisioninbox's two call sites already filter a
// draft PR before ever reaching this package (aggregate.go's own
// buildPRItems: "if pr.Draft { continue }"), and both already apply
// HasChangesRequested as a SEPARATE, hard block layered on top of this
// package's own eligible/not-eligible verdict (§16.1: "ready_to_merge's
// own 'approval' is auto-approval BY THE DETERMINISTIC ELIGIBILITY
// ENGINE ... never a human GitHub review" -- HasChangesRequested is
// exactly that separate human-review fact, never folded into what
// "eligibility" itself means). §21.2's own criteria list names neither,
// so this package does not manufacture a seventh/eighth check for
// either -- see internal/app/decisioninbox/revalidate.go's own
// revalidateCore for where both are actually enforced.
package autoapproval
