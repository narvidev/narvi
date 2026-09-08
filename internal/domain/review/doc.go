// Package review implements the code-review domain's structured verdict
// (§8.2): a first-class, typed outcome of an automated code review,
// and the two independent, raise-only "floors" (coverage, premise) that
// compose into Shippable — the field the automated-approval engine (§21.2,
// a later Step) will gate on. Every function here is pure per §11: no I/O,
// no time.Now(), no randomness, zero external imports.
//
// # Why a structured type at all
//
// §21.1/§22.1 both depend on the verdict being data from the moment it is
// produced, never markdown to be re-parsed after the fact: persistence
// (§21.1) appends a Verdict row as-is, and rebuttal identity (§22.1) hashes
// a finding's own persisted content rather than tracking it by file:line.
// Nothing in this package (or any consumer of it) may parse a posted
// comment back into a Verdict — nothing here even imports a markdown
// parser, on principle, not merely by omission.
//
// # Server-computed Shippable — the central security property
//
// §21.2 states plainly: "the LLM's verdict only ever *proposes* Shippable;
// the server recomputes it ... independently." Verdict therefore carries
// TWO different fields that must never be confused with each other:
//
//   - ProposedShippable — the model's own self-report, of a DISTINCT Go
//     type (ProposedShippable, not Shippable). This is not merely a
//     naming convention: a value of this type cannot be assigned into a
//     Shippable-typed field (Verdict.Shippable, or any parameter of
//     ComputeShippable) without an explicit, visible type conversion.
//     Grepping for that conversion is how a reviewer of THIS package's own
//     future changes would notice if anyone ever tried to launder a
//     model's opinion into the authoritative field.
//   - Shippable — the authoritative field. The ONLY way to obtain a
//     legitimate value for it is ComputeShippable's return value.
//     ComputeShippable's signature does not accept a ProposedShippable
//     parameter at all — the model's guess is not merely "not trusted", it
//     is structurally incapable of influencing the computation, because
//     there is no parameter for it to influence. A caller (a later Step's
//     verdict-posting tool, §8.2) is expected to populate
//     Verdict.Shippable with exactly ComputeShippable's result and never
//     with a converted ProposedShippable — see verdict.go's own doc
//     comment.
//
// # Exactly eight exported functions
// This package exports exactly eight functions that compute anything.
// The original five this section named when it was first written, all
// feeding directly into Shippable: CoverageFloor (the coverage floor),
// PremiseFloor (the premise floor), AdequacyFloor (the
// description-adequacy floor, §26.2 — the THIRD raise-only floor,
// added to the original two §8.2 established), CounterReviewFloor (the
// counter-review floor, §26.4 — the FOURTH raise-only floor), and
// ComputeShippable (the one composition seam a later Step calls). Plus
// three more this package has since grown to also own, each its own pure
// decision/render function unrelated to any Shippable floor:
// ShouldRunAggregateReview (§15.3's own conditional aggregate-diff-review
// trigger, aggregatereview.go), ComputeReleaseManifestFindings (§15's own
// release-manifest red-at-merge/stale-approval finding computation,
// manifestcheck.go), and RenderTurnPrompt (the review turn's own prompt
// renderer, context.go). Every other identifier besides these eight
// functions and the types/constants a caller needs to construct a Verdict
// or drive one of these three later additions is unexported — there is no
// second path to any of these eight results, and no method set
// duplicating them. (A ninth exported function, StripPlaceholderTokens, was
// added on top of these eight by a later Step — see design call #8 below
// for why it does not reopen this section's own count or its "no second
// path" discipline: it computes none of the eight RESULTS this section
// enumerates. A tenth and eleventh, RenderCompositionReviewPrompt
// (compositionreview.go) and TransitionCompositionDecision
// (compositiondecision.go), were added by §15.3/§12.2 item 9's own Step —
// see design call #9 below for the identical reasoning: neither computes
// one of the eight RESULTS above either.)
//
// # Ranking is an explicit table, never iota order
// Shippable's total order (auto < needs_human < block, most to least
// permissive) is asserted the same way this codebase's state machines
// assert their transition tables (internal/domain/gitstate.transitions,
// internal/domain/turn's own transitions map): as an explicit
// map[Shippable]int in shippable.go, never inferred from the declaration
// order of the ShippableAuto/ShippableNeedsHuman/ShippableBlock consts
// themselves. An accidental reorder of those consts (e.g. an
// alphabetizing pass) changes nothing about the policy this package
// enforces, because nothing here ever compares the consts' underlying
// iota/declaration positions — only the explicit table. TestShippableRank
// in shippable_test.go pins this table down exhaustively.
//
// # Fail-conservative policy for every closed enum (uniform, not ad hoc)
// Every enum in this package (RiskLevel, PremiseState, TestsCoverageState,
// DocsDriftState) is a string type whose Go zero value ("") is NOT one of
// its named legal values — an unset or garbled field is therefore always
// detectable as neither "the best case" nor "the worst case" but a THIRD,
// unrecognized thing. The uniform rule this package applies everywhere an
// enum feeds a floor or baseline: an unrecognized value is treated
// EXACTLY as conservatively as that enum's own worst-known legitimate
// value — never more conservative (there is no principled way to invent an
// "even worse than the worst defined case" outcome) and never less
// (§21.2's whole automated-approval design rests on Shippable never
// silently reading as "auto" for a value nobody actually assessed — "an
// unrecognized/unknown state defaulting to the permissive end would be a
// real defect"). Concretely:
//
//   - RiskLevel: unrecognized ranks with RiskLevelHigh (baselineFromRisk,
//     shippable.go).
//   - TestsCoverageState: unrecognized ranks with
//     TestsCoverageStateInsufficient (CoverageFloor, coverage.go).
//   - PremiseState: unrecognized ranks with PremiseStateNotAPR
//     (PremiseFloor, premise.go).
//   - DescriptionAdequacy: unrecognized ranks with
//     DescriptionAdequacyMisleading (AdequacyFloor, adequacy.go, §26.2).
//   - CounterReviewStatus: unrecognized ranks with CounterReviewSkipped
//     (CounterReviewFloor, counterreview.go, §26.4) — see that
//     type's own doc comment for the one deliberate exception to this
//     package's usual "every enum feeds a floor on every verdict"
//     framing: this field has no meaning at all on the light path, so
//     reviewpost.BuildVerdict (never this package) is responsible for
//     substituting CounterReviewDone before calling ComputeShippable on a
//     light-path verdict.
//   - DocsDriftState: unrecognized ranks with DocsDriftStateFound
//     (documented on the type itself, docsdrift.go) — inert in THIS
//     package today; see the design call below.
//
// # Design calls made in this Step, flagged rather than papered over
//
//  1. RiskLevel has exactly three values — low/medium/high — not four.
//     The plan's own §8.2 names the RiskLevel type but never
//     enumerates its values. A fourth "critical" tier was considered and
//     rejected: nothing in docs/TECHNICAL_PLAN.md or docs/design/
//     mockups.html ever shows a verdict-level risk beyond "medium" (the
//     mockup's own risk-map table shows individual FINDING severities up
//     to "high", via the same three-tier low/medium/high vocabulary —
//     see mockups.html's "chip crit" finding severity). Three tiers,
//     matching the vocabulary the UI already commits to, was chosen over
//     inventing a fourth with no textual support anywhere in the spec.
//     A consequence: RiskLevel alone (with both floors clean) never
//     reaches ShippableBlock — Block is reachable only through the
//     premise floor's PremiseStateNotAPR (or an unrecognized enum value
//     anywhere, per the fail-conservative policy above). This reads
//     Block as "this is not reviewable code / not a legitimate PR",
//     categorically worse than "a human needs to look", which the
//     three-tier RiskLevel scale alone never claims to detect.
//
//  2. RiskLevel's baseline is not one of this Step's two named "floors".
//     §8.2 is explicit that there are exactly two independent
//     raise-only floors: coverage and premise. RiskLevel plainly has to
//     feed Shippable somehow — a risk-map verdict whose own overall risk
//     assessment had zero effect on auto-approval eligibility would
//     defeat the point of assessing risk at all — so this package treats
//     RiskLevel as the BASELINE the two floors can only ever raise, never
//     lower (see baselineFromRisk, shippable.go). This keeps "raise-only"
//     uniform across all three inputs to ComputeShippable, without
//     inventing a THIRD exported floor function the plan never asked for.
//
//  3. BlastRadius's fixed Tag vocabulary is this package's own invention,
//     grounded in the one concrete source the plan gives: §21.2's
//     sensitive-path criterion names "migrations, auth code, /contracts
//     by default" as the auto-approval eligibility engine's own
//     configurable examples. Tag includes those three plus five siblings
//     of comparably broad blast radius (secrets, infra, public API, data
//     layer, dependencies) — see tag.go. Nothing elsewhere in the plan or
//     mockups enumerates this vocabulary; extending it is expected as
//     later work (§8.2/§8.6) finds real gaps, but any addition belongs here,
//     as a deliberate, reviewed change to this one fixed list — never
//     inferred ad hoc by a consumer.
//
//  4. No Finding type ships in this Step, despite two later sections of
//     the SAME technical plan appearing to assume one already exists.
//     §21.1 ("the structured type means this is pure storage, never
//     re-parsing anything out of posted comment text") and §22.1 ("a
//     hash/text of the finding stored at the moment the verdict that
//     raised it was posted — §8.2's structured type already
//     carries this data; storing it is not new capture, just retention")
//     both describe §8.2's verdict type as already carrying
//     per-finding content. But §8.2
//     — the authoritative, dedicated description of this Step's scope —
//     enumerates exactly seven fields (RiskLevel, PremiseState,
//     BlastRadius, FilesChanged, TestsCoverageState, DocsDriftState,
//     Shippable) and names no Finding/rebuttal-identity shape at all.
//     This is a genuine tension inside the plan itself, not a disagreement
//     between this Step's brief and the plan (both independently read the
//     same seven fields) — named here rather than resolved by guessing at
//     a hash algorithm, a Finding struct shape, or rebuttal-identity
//     machinery that §22 (a much later Step) is explicitly tasked with
//     designing against real posting/reconciliation logic this package has
//     no visibility into yet. Building that shape now, disconnected from
//     the code that will actually construct and reconcile it, risks
//     committing to the wrong one. Verdict below carries exactly the seven
//     named fields (plus ProposedShippable, required by the server-
//     computed-Shippable property above) and nothing else; a Finding type
//     is left for whichever Step actually needs it.
//
//  5. DocsDriftState is defined with a documented fail-conservative zero
//     value (see docsdrift.go) but is not wired into ComputeShippable at
//     all in this Step — consistent with "exactly two floors" above. A
//     future doc-drift floor, should one ever be added, has a policy to
//     match (unrecognized ranks with DocsDriftStateFound) already on
//     record rather than invented ad hoc when that Step arrives.
//
//  6. (§26.2) AdequacyFloor is the THIRD raise-only floor,
//     composing into ComputeShippable the SAME way as the original two —
//     max(rank), never a special case. §26.2 names only "misleading" as
//     a floor trigger ("misleading floors Shippable at needs_human");
//     "drift" is a real, distinct DescriptionAdequacy value but is
//     deliberately NOT wired to raise anything on its own (AdequacyFloor's
//     own doc comment, adequacy.go) — a stale-but-not-actively-wrong
//     description does not, by §26.2's own words, warrant the same human
//     gate an actively misleading one does. §26.2 also states an explicit
//     asymmetry this package's OTHER two floors never had occasion to
//     state (nothing about coverage or premise ever plausibly touches
//     RiskLevel): AdequacyFloor "deliberately never inflate[s] RiskLevel"
//     — the server computes Shippable, but never fabricates risk the
//     model did not itself report. This is not a new rule invented for
//     this Step; it is the same "model's own guess is structurally
//     incapable of influencing anything but the one field this package
//     exists to author" posture ComputeShippable's own doc comment
//     already states for ProposedShippable, restated here because §26.2's
//     own text calls it out by name for this specific floor.
//
//  7. (§26.4) CounterReviewFloor is the FOURTH raise-only floor,
//     composing into ComputeShippable the SAME way as the original
//     three — max(rank), never a special case. Unlike coverage/premise/
//     adequacy, this floor's own input has no meaning on the light path
//     at all (§26.9: the light path never runs a counter-reviewer sub-
//     task, so there is nothing to have "skipped") — this package still
//     keeps CounterReviewFloor a pure function of ONLY CounterReviewStatus
//     (no depth parameter), matching every other floor's own signature
//     shape exactly, rather than growing the one function this package
//     exports for path-awareness it otherwise has zero use for (doc.go's
//     own "zero external imports" convention already forbids importing
//     reviewtriage here to even ask the question). The substitution this
//     requires — CounterReviewDone on every light-path verdict, so the
//     floor is structurally a no-op there — is reviewpost.BuildVerdict's
//     own responsibility (reviewpost already imports both review and
//     reviewtriage), documented on that function and pinned by
//     TestBuildVerdict_CounterReviewFloorInertOnLightPath
//     (internal/domain/reviewpost/validate_test.go). Also unlike
//     coverage/premise/adequacy, never touches RiskLevel, for the
//     identical reason AdequacyFloor does not (design call #6 above).
//
//  8. (hardening, adversarial review) A NINTH exported function,
//     StripPlaceholderTokens (sanitize.go), was added on top of the eight
//     above without renumbering that section's own count -- a deliberate
//     choice, not an oversight the "exactly eight" wording failed to catch.
//     The eight are specifically the Verdict-computation surface ("no
//     second path to any of these eight RESULTS, and no method set
//     duplicating them") -- StripPlaceholderTokens computes no Verdict
//     field, floor, label, or rendered prompt text at all; it is a narrow
//     security utility (destroy every literal secret-substitution
//     placeholder token in a string, fixed-point) this package already
//     used internally (formerly unexported stripPlaceholderTokens, called
//     only by sanitizeDiffField/sanitizeDescriptionField for THIS
//     package's own read/prompt path, RenderTurnPrompt) and now also
//     exports for internal/domain/reviewpost's own SanitizeDigest
//     (reviewpost/sanitize.go) to call directly, hardening the WRITE path
//     (internal/app/reviewverdict.Insert persisting a review verdict's own
//     model-authored digest fields) against the identical placeholder-
//     forgery class the Phase 5 audit's CRITICAL finding closed on the
//     read path. The alternative -- reviewpost hand-duplicating
//     placeholderTokens a fourth time, mirroring how review/upload already
//     duplicate each OTHER's tokens as raw literals -- was rejected
//     because reviewpost, unlike review/upload, has no "zero external
//     imports" self-restriction forcing that duplication (reviewpost/
//     doc.go already permits exactly one non-stdlib import, this package,
//     for the Verdict/RiskLevel/Shippable/Tag types it needs regardless);
//     reusing this package's own single, already-drift-tested,
//     already-canonical list (placeholderdrift_internal_test.go's own
//     whole-internal/domain source scan) is what makes reviewpost's own
//     write-path sanitizer pick up a future eleventh placeholder family
//     automatically, exactly like this package's read path already does,
//     with no second scan-test to maintain. See StripPlaceholderTokens'
//     own doc comment (sanitize.go) for the full reasoning.
//
//  9. (§15.3/§12.2 item 9) Two further exported functions, neither
//     reopening the "exactly eight" count above for the identical reason
//     design call #8 already establishes for StripPlaceholderTokens (both
//     compute none of the eight VERDICT-COMPUTATION results that count
//     enumerates):
//
//       - RenderCompositionReviewPrompt (compositionreview.go) is the
//         aggregate-diff composition review turn's own prompt renderer --
//         deliberately a SEPARATE function from RenderTurnPrompt, never a
//         parameterized variant of it, because §15.3 requires "a prompt
//         distinct from the standard risk-map verdict": this pass must
//         never compute or consume Shippable/PremiseState/the digest/
//         BlastRadius vocabulary at all (§15.4). Its own tool-instructions
//         block names a SEPARATE endpoint/JSON shape
//         (compositionFindingsToolInstructions) from verdictToolInstructions
//         — see that file's own top doc comment for the full "why a second
//         tool, not a second call to the existing verdict tool" reasoning.
//       - TransitionCompositionDecision (compositiondecision.go) is §12.2
//         item 9's own "Block release / Acknowledge & ship" transition —
//         an explicit, table-driven decision (CLAUDE.md/§11's own "every
//         state transition goes through the machine's transition table"
//         rule) sized to exactly what it needs: one non-terminal state,
//         two legal, both-terminal actions. CompositionDecision is never
//         computed by this package (unlike Shippable, this package's OTHER
//         classification) — it is a human's own decision, recorded
//         verbatim; this function only validates which decision may
//         legally follow which.
package review
