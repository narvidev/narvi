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
//     Items 2 to 4 are the freshness prefix, and they live in ONE exported
//     function, CheckFreshness (freshness.go), which ComputeEligible calls
//     right after item 1. It is exported because a session's result (row
//     182, technical plan §43.20) asks the same question -- is this
//     verdict still about the code as it stands -- and must get the merge
//     path's answer, never a second comparison's.
//
//  5. EligibilityInput.CIConclusionDegraded must be false -- the live CI
//     read (check 7 below) must have actually been FULLY performed
//     before its own answer is trusted for anything: ports.OpenPR.
//     CIConclusionDegraded's own doc comment (githubapi.
//     fetchCIConclusionLive makes two independent GETs, and either can
//     fail without the other). CIGreen below is already false whenever
//     this is true -- this check changes no OUTCOME by itself -- but it
//     is checked FIRST, on its own dedicated reason
//     (ReasonCIConclusionDegraded), the same "is the fact even knowable"
//     precedence check 10 below already establishes for check 11, so a
//     half-read CI composite refuses distinguishably from a confirmed-red
//     one (CIGreen == false, this field == false) or a still-running one.
//
//  6. EligibilityInput.RequiredChecks -- the base branch's required
//     checks (§21.2's "CI green means the required checks" amendment)
//     must have been read (ReasonRequiredChecksUnknown otherwise, never a
//     fall-back to the CI read alone), and the head must satisfy each one:
//     reported, from the App the base names when it names one (its check
//     run, or a commit status its bot account posted, that App identified
//     by id), and passed -- a check run and a commit status of the same
//     name both passed. A required check that is missing, could not be
//     confirmed, still running or failed refuses with a Reason naming it,
//     and its App (requiredchecks.go). narvi/review is taken
//     out of the required set: it is the review this eligibility already
//     reads. Checked before check 7, so a required check that has not
//     reported is named rather than read as "CI is not green".
//
//  7. CIGreen -- must be true, whatever check 6 said: the required set is
//     added to the CI read, never substituted for it, so a failing check
//     the base does not require still blocks here.
//
//  8. Verdict.Shippable == review.ShippableAuto.
//
//  9. EligibilityInput.ChangedFileCount <= cfg.MaxFilesChanged (diff
//     size) -- GitHub's own authoritative changed-file scalar, never
//     Verdict.FilesChanged, and (Phase 5 audit finding 2, fixed) never
//     a possibly page-truncated len() of the fetched path listing
//     either.
//
//  10. EligibilityInput.TouchedBlastRadiusKnown is true (Phase 5 audit
//     findings 1+2, fixed) -- the sensitive-path facts check 11 below
//     relies on must have actually been established from GitHub; a
//     failed or page-truncated changed-files fetch refuses here rather
//     than silently reading as "nothing sensitive touched".
//
//  11. No cfg.SensitiveTags member appears in EligibilityInput.
//     TouchedBlastRadius (no sensitive path touched) -- never
//     Verdict.BlastRadius.
//
// "No floor raised: neither the coverage floor nor the premise floor
// ... is above its baseline" is DELIBERATELY not a separate check of its
// own anywhere in the numbered list above (never conflate this with
// check 10, Phase 5 audit findings 1+2's own "is the fact even knowable"
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
// Premise fields would therefore either (a) always agree with check 8
// above, making it dead weight, or (b) disagree with it, which would
// mean domain/review's own raise-only property had a bug -- a bug this
// package has no business re-litigating a second time. Check 8 alone
// already IS "no floor raised", exactly as rigorously as a bespoke
// second check would be. This package's own test suite (eligibility_test.go)
// still exercises "a floor raised" as its own, independently named
// scenario -- via three DISTINCT Verdict fixtures (coverage floor
// raised, premise floor raised, risk baseline alone raised), each
// proving check 8 catches that specific case -- rather than by adding a
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
