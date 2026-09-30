package reviewpost_test

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
)

func TestRenderVerdictComment(t *testing.T) {
	v := review.Verdict{
		RiskLevel:         review.RiskLevelMedium,
		Premise:           review.PremiseStateOK,
		BlastRadius:       []review.Tag{review.TagAuth, review.TagContracts},
		FilesChanged:      7,
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableNeedsHuman,
		Shippable:         review.ShippableNeedsHuman,
	}

	digest := reviewpost.Digest{
		Summary: "Adds a constant-time comparison helper and swaps every password/token check onto it.",
		ArchDecisions: []reviewpost.ArchDecision{
			{Decision: "Centralize comparisons in one helper.", RejectedAlternative: "Fix each call site independently.", ConventionConformance: "Matches CLAUDE.md's shared-helper convention."},
		},
		StackRisks:          "Touches every auth check path; a regression here is broad.",
		UnverifiedLimits:    "Did not verify constant-time behavior on the CI runner's own hardware.",
		DescriptionAdequacy: review.DescriptionAdequacyOK,
		AdequacyExplanation: "The PR body accurately describes the constant-time comparison helper this diff adds.",
	}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), nil, digest, "Timing-unsafe comparison in verify.go.", "narvi-bot", reviewpost.LabelMediumRisk)

	for _, want := range []string{
		string(review.RiskLevelMedium),
		string(review.PremiseStateOK),
		string(review.TestsCoverageStateAdequate),
		string(review.DocsDriftStateNone),
		strconv.Itoa(v.FilesChanged),
		string(review.TagAuth),
		string(review.TagContracts),
		string(review.ShippableNeedsHuman),
		"Timing-unsafe comparison in verify.go.",
		reviewpost.LabelMediumRisk,
		"server-side verdict tool",
		"### What this PR does",
		digest.Summary,
		"### Architecture choices",
		"Centralize comparisons in one helper.",
		"Fix each call site independently.",
		"Matches CLAUDE.md's shared-helper convention.",
		"### Risks to the stack",
		digest.StackRisks,
		digest.UnverifiedLimits,
		"Description adequacy",
		string(review.DescriptionAdequacyOK),
		digest.AdequacyExplanation,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderVerdictComment() missing %q in:\n%s", want, got)
		}
	}

	// The rendered comment must ALWAYS carry the rerun guidance, so a
	// review a human wants to re-run always has an actionable next step.
	if !strings.Contains(got, reviewpost.RerunGuidance("narvi-bot")) {
		t.Errorf("RenderVerdictComment() missing RerunGuidance in:\n%s", got)
	}
}

// TestRenderVerdictComment_EmptyBlastRadiusOmitsLine proves an empty
// BlastRadius (a legitimate value, review.Verdict's own doc comment) never
// renders a dangling "Blast radius:" line with nothing after it.
func TestRenderVerdictComment_EmptyBlastRadiusOmitsLine(t *testing.T) {
	v := review.Verdict{
		RiskLevel:     review.RiskLevelLow,
		Premise:       review.PremiseStateOK,
		TestsCoverage: review.TestsCoverageStateAdequate,
		DocsDrift:     review.DocsDriftStateNone,
		Shippable:     review.ShippableAuto,
	}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), nil, reviewpost.Digest{Summary: "No changes of note."}, "Nothing to flag.", "narvi-bot", reviewpost.LabelLowRisk)
	if strings.Contains(got, "Blast radius") {
		t.Errorf("RenderVerdictComment() rendered a Blast radius line for an empty BlastRadius:\n%s", got)
	}
}

// TestRenderVerdictComment_AnchoredFindingRendersStartEndLine proves an
// anchored finding (§22.1.1) renders using StartLine/EndLine, never the
// model's own self-reported Line.
func TestRenderVerdictComment_AnchoredFindingRendersStartEndLine(t *testing.T) {
	v := review.Verdict{
		RiskLevel: review.RiskLevelLow, Premise: review.PremiseStateOK,
		TestsCoverage: review.TestsCoverageStateAdequate, DocsDrift: review.DocsDriftStateNone,
		Shippable: review.ShippableAuto,
	}
	modelLine := 999
	finding := reviewpost.Finding{
		Severity: review.RiskLevelMedium, FilePath: "main.go", Description: "off-by-one",
		Line:      &modelLine,
		StartLine: 10, EndLine: 12,
	}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), []reviewpost.Finding{finding}, reviewpost.Digest{Summary: "No changes of note."}, "Summary.", "narvi-bot", reviewpost.LabelLowRisk)

	if !strings.Contains(got, "main.go:10-12") {
		t.Errorf("RenderVerdictComment() missing anchored range %q in:\n%s", "main.go:10-12", got)
	}
	if strings.Contains(got, "main.go:999") {
		t.Errorf("RenderVerdictComment() rendered the model's own unverified Line (999) instead of the anchored StartLine/EndLine:\n%s", got)
	}
}

// TestRenderVerdictComment_AnchoredSingleLineFindingOmitsRange proves a
// single-line anchor (StartLine == EndLine) renders as "file:N", never
// "file:N-N".
func TestRenderVerdictComment_AnchoredSingleLineFindingOmitsRange(t *testing.T) {
	v := review.Verdict{
		RiskLevel: review.RiskLevelLow, Premise: review.PremiseStateOK,
		TestsCoverage: review.TestsCoverageStateAdequate, DocsDrift: review.DocsDriftStateNone,
		Shippable: review.ShippableAuto,
	}
	finding := reviewpost.Finding{
		Severity: review.RiskLevelMedium, FilePath: "main.go", Description: "off-by-one",
		StartLine: 10, EndLine: 10,
	}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), []reviewpost.Finding{finding}, reviewpost.Digest{Summary: "No changes of note."}, "Summary.", "narvi-bot", reviewpost.LabelLowRisk)

	if !strings.Contains(got, "main.go:10`") {
		t.Errorf("RenderVerdictComment() missing single-line anchor %q in:\n%s", "main.go:10`", got)
	}
	if strings.Contains(got, "main.go:10-10") {
		t.Errorf("RenderVerdictComment() rendered a redundant range (10-10) for a single-line anchor:\n%s", got)
	}
}

// TestRenderVerdictComment_UnanchoredFindingNeverRendersAGuessedLine is
// this Step's own central proof for the rendering side of §22.1.1: an
// unanchored finding (StartLine == 0) must NEVER render ANY line
// reference at all -- not the anchored range (there is none), and NOT
// the model's own self-reported Line either (that would be exactly the
// "plausible-looking wrong answer" §22.1.1 says is worse than nothing).
func TestRenderVerdictComment_UnanchoredFindingNeverRendersAGuessedLine(t *testing.T) {
	v := review.Verdict{
		RiskLevel: review.RiskLevelLow, Premise: review.PremiseStateOK,
		TestsCoverage: review.TestsCoverageStateAdequate, DocsDrift: review.DocsDriftStateNone,
		Shippable: review.ShippableAuto,
	}
	modelLine := 42
	finding := reviewpost.Finding{
		Severity: review.RiskLevelMedium, FilePath: "main.go", Description: "some finding",
		Line:      &modelLine, // the model's own self-report -- must be ignored
		StartLine: 0, EndLine: 0,
	}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), []reviewpost.Finding{finding}, reviewpost.Digest{Summary: "No changes of note."}, "Summary.", "narvi-bot", reviewpost.LabelLowRisk)

	if strings.Contains(got, "main.go:42") {
		t.Errorf("RenderVerdictComment() rendered the model's own unverified Line (42) for an UNANCHORED finding -- must render no line at all:\n%s", got)
	}
	if !strings.Contains(got, "`main.go`") {
		t.Errorf("RenderVerdictComment() should still render the bare file path for an unanchored finding:\n%s", got)
	}
}

// TestRenderVerdictComment_FindingDescriptionEscapesAngleBrackets proves a
// finding's own Description (model-authored free text -- finding.go's own
// doc comment -- that can legitimately contain generics/tags/comparisons
// like "List<int>" or "a < b") is HTML-escaped before it lands in the
// rendered comment body: an unescaped '<'/'>' would otherwise be read by
// GitHub's own markdown renderer as literal HTML rather than the model's
// own text. Exercised across all three finding-rendering branches
// (single-line anchor, range anchor, unanchored) since each has its own
// fmt.Fprintf interpolation site in RenderVerdictComment.
func TestRenderVerdictComment_FindingDescriptionEscapesAngleBrackets(t *testing.T) {
	v := baseVerdict()
	findings := []reviewpost.Finding{
		{Severity: review.RiskLevelMedium, FilePath: "a.go", Description: "Use List<int> instead of List<Object>", StartLine: 5, EndLine: 5},
		{Severity: review.RiskLevelMedium, FilePath: "b.go", Description: "off-by-one when a < b", StartLine: 10, EndLine: 12},
		{Severity: review.RiskLevelMedium, FilePath: "c.go", Description: "compares Foo<T> unanchored"},
	}
	digest := reviewpost.Digest{Summary: "No changes of note."}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), findings, digest, "Summary.", "narvi-bot", reviewpost.LabelLowRisk)

	if strings.Contains(got, "List<int>") || strings.Contains(got, "List<Object>") {
		t.Errorf("RenderVerdictComment() rendered an unescaped '<'/'>' from a single-line-anchored finding's Description:\n%s", got)
	}
	if !strings.Contains(got, "List&lt;int&gt; instead of List&lt;Object&gt;") {
		t.Errorf("RenderVerdictComment() missing the escaped single-line-anchored finding Description in:\n%s", got)
	}
	if strings.Contains(got, "a < b") {
		t.Errorf("RenderVerdictComment() rendered an unescaped '<' from a range-anchored finding's Description:\n%s", got)
	}
	if !strings.Contains(got, "off-by-one when a &lt; b") {
		t.Errorf("RenderVerdictComment() missing the escaped range-anchored finding Description in:\n%s", got)
	}
	if strings.Contains(got, "Foo<T>") {
		t.Errorf("RenderVerdictComment() rendered an unescaped '<'/'>' from an unanchored finding's Description:\n%s", got)
	}
	if !strings.Contains(got, "compares Foo&lt;T&gt; unanchored") {
		t.Errorf("RenderVerdictComment() missing the escaped unanchored finding Description in:\n%s", got)
	}
}

// baseVerdict is the minimal valid review.Verdict the digest-section tests
// below share, mutating only what each test cares about.
func baseVerdict() review.Verdict {
	return review.Verdict{
		RiskLevel: review.RiskLevelLow, Premise: review.PremiseStateOK,
		TestsCoverage: review.TestsCoverageStateAdequate, DocsDrift: review.DocsDriftStateNone,
		Shippable: review.ShippableAuto,
	}
}

// assessmentOf is the review.ShippableAssessment BuildVerdict would hand
// RenderVerdictComment for v's own fields on the light path (an honest
// description, no counter-review to count), so a render test's Shippable
// bullet comes from v the way it does in production.
func assessmentOf(v review.Verdict) review.ShippableAssessment {
	return review.ComputeShippable(v.RiskLevel, v.TestsCoverage, v.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)
}

// TestRenderVerdictComment_NamesWhatKeepsTheClassAboveAuto pins the
// readout's blocker lines (§26.1): under the Shippable bullet, a lead-in
// marking them as the server's decision rather than the reviewer's
// assertion, then one line per input that keeps the class above auto --
// and nothing at all for an auto verdict. Each case asserts the whole
// block from the Shippable bullet to the narrative summary, byte for
// byte, so dropping the lines, dropping the marker, or reordering either
// fails here.
func TestRenderVerdictComment_NamesWhatKeepsTheClassAboveAuto(t *testing.T) {
	const leadIn = "  - Kept above auto by (decided by the server, not asserted by the reviewer):\n"

	tests := []struct {
		name          string
		risk          review.RiskLevel
		coverage      review.TestsCoverageState
		premise       review.PremiseState
		adequacy      review.DescriptionAdequacy
		counterReview review.CounterReviewStatus
		wantBlock     string
	}{
		{
			name: "a medium-risk verdict with every floor clean names its risk level",
			risk: review.RiskLevelMedium, coverage: review.TestsCoverageStateAdequate, premise: review.PremiseStateOK, adequacy: review.DescriptionAdequacyOK, counterReview: review.CounterReviewDone,
			wantBlock: "- **Shippable**: needs_human (server-computed)\n" + leadIn +
				"    - risk level `medium` (needs_human)\n\n",
		},
		{
			name: "a verdict floored by a coverage gap names the coverage gap",
			risk: review.RiskLevelLow, coverage: review.TestsCoverageStateInsufficient, premise: review.PremiseStateOK, adequacy: review.DescriptionAdequacyOK, counterReview: review.CounterReviewDone,
			wantBlock: "- **Shippable**: needs_human (server-computed)\n" + leadIn +
				"    - test coverage `insufficient` (needs_human)\n\n",
		},
		{
			name: "an auto verdict names none and renders the header it always did",
			risk: review.RiskLevelLow, coverage: review.TestsCoverageStateAdequate, premise: review.PremiseStateOK, adequacy: review.DescriptionAdequacyOK, counterReview: review.CounterReviewDone,
			wantBlock: "- **Shippable**: auto (server-computed)\n\n",
		},
		{
			name: "an uncorroborated counter-review is named as such",
			risk: review.RiskLevelLow, coverage: review.TestsCoverageStateAdequate, premise: review.PremiseStateOK, adequacy: review.DescriptionAdequacyOK, counterReview: review.CounterReviewUncorroborated,
			wantBlock: "- **Shippable**: needs_human (server-computed)\n" + leadIn +
				"    - counter-review `uncorroborated` -- reported done, but the server could not confirm it from this turn's trace when the verdict was posted (needs_human)\n\n",
		},
		{
			name: "a skipped counter-review is named as skipped",
			risk: review.RiskLevelLow, coverage: review.TestsCoverageStateAdequate, premise: review.PremiseStateOK, adequacy: review.DescriptionAdequacyOK, counterReview: review.CounterReviewSkipped,
			wantBlock: "- **Shippable**: needs_human (server-computed)\n" + leadIn +
				"    - counter-review `skipped` (needs_human)\n\n",
		},
		{
			name: "a misleading description is named",
			risk: review.RiskLevelLow, coverage: review.TestsCoverageStateAdequate, premise: review.PremiseStateOK, adequacy: review.DescriptionAdequacyMisleading, counterReview: review.CounterReviewDone,
			wantBlock: "- **Shippable**: needs_human (server-computed)\n" + leadIn +
				"    - description adequacy `misleading` (needs_human)\n\n",
		},
		{
			name: "every input above auto is named, each with the level it alone forces",
			risk: review.RiskLevelHigh, coverage: review.TestsCoverageStateInsufficient, premise: review.PremiseStateNotAPR, adequacy: review.DescriptionAdequacyMisleading, counterReview: review.CounterReviewSkipped,
			wantBlock: "- **Shippable**: block (server-computed)\n" + leadIn +
				"    - risk level `high` (needs_human)\n" +
				"    - test coverage `insufficient` (needs_human)\n" +
				"    - premise `not_a_pr` (block)\n" +
				"    - description adequacy `misleading` (needs_human)\n" +
				"    - counter-review `skipped` (needs_human)\n\n",
		},
		{
			name: "an unset input is named as unset",
			risk: review.RiskLevelLow, coverage: review.TestsCoverageState(""), premise: review.PremiseStateOK, adequacy: review.DescriptionAdequacyOK, counterReview: review.CounterReviewDone,
			wantBlock: "- **Shippable**: needs_human (server-computed)\n" + leadIn +
				"    - test coverage `unset` (needs_human)\n\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shippable := review.ComputeShippable(tc.risk, tc.coverage, tc.premise, tc.adequacy, tc.counterReview)
			v := review.Verdict{
				RiskLevel: tc.risk, Premise: tc.premise, TestsCoverage: tc.coverage,
				DocsDrift: review.DocsDriftStateNone, Shippable: shippable.Class(),
			}
			digest := reviewpost.Digest{Summary: "No changes of note.", DescriptionAdequacy: tc.adequacy}

			got := reviewpost.RenderVerdictComment(v, shippable, nil, digest, "Narrative.", "narvi-bot", reviewpost.LabelLowRisk)

			if want := tc.wantBlock + "Narrative.\n\n"; !strings.Contains(got, want) {
				t.Errorf("RenderVerdictComment() missing the Shippable block\n%q\nin:\n%s", want, got)
			}
			if gotLeadIns := strings.Count(got, leadIn); gotLeadIns > 1 {
				t.Errorf("RenderVerdictComment() rendered the blocker lead-in %d times, want at most once:\n%s", gotLeadIns, got)
			}
		})
	}
}

// TestRenderVerdictComment_BlockerLinesAreTheOnlyAddition proves the
// blocker lines are purely additive: removing them from a rendered
// needs_human verdict leaves, line for line, the comment this renderer
// produced before blockers existed, captured here as a golden. Other code
// that reads the posted comment, if any ever did, sees every line it saw
// before, in the same order.
func TestRenderVerdictComment_BlockerLinesAreTheOnlyAddition(t *testing.T) {
	shippable := review.ComputeShippable(review.RiskLevelMedium, review.TestsCoverageStateInsufficient, review.PremiseStateOK, review.DescriptionAdequacyOK, review.CounterReviewUncorroborated)
	v := review.Verdict{
		RiskLevel: review.RiskLevelMedium, Premise: review.PremiseStateOK,
		BlastRadius:   []review.Tag{review.TagAuth},
		FilesChanged:  4,
		TestsCoverage: review.TestsCoverageStateInsufficient, DocsDrift: review.DocsDriftStateNone,
		Shippable: shippable.Class(),
	}
	digest := reviewpost.Digest{
		Summary:             "Adds a retry helper.",
		DescriptionAdequacy: review.DescriptionAdequacyOK,
		AdequacyExplanation: "Matches the diff.",
		StackRisks:          "Touches the token refresh path.",
	}

	got := reviewpost.RenderVerdictComment(v, shippable, nil, digest, "One retry path is untested.", "narvi-bot", reviewpost.LabelMediumRisk)

	const before = "### Code review verdict\n\n" +
		"- **Risk**: medium\n" +
		"- **Premise**: ok\n" +
		"- **Description adequacy**: ok -- Matches the diff.\n" +
		"- **Shippable**: needs_human (server-computed)\n\n" +
		"One retry path is untested.\n\n" +
		"### What this PR does\n\n" +
		"Adds a retry helper.\n\n" +
		"### Architecture choices\n\n" +
		"_No architecture decisions reported for this review._\n\n" +
		"### Risks to the stack\n\n" +
		"- **Blast radius**: auth\n" +
		"- **Stack risks**: Touches the token refresh path.\n\n" +
		"<details>\n<summary>Findings, coverage &amp; docs-drift (supporting evidence)</summary>\n\n" +
		"- **Test coverage**: insufficient\n" +
		"- **Docs drift**: none\n" +
		"- **Files changed**: 4\n" +
		"\n</details>\n\n" +
		"_Posted via Narvi's server-side verdict tool_ · labels synced: `review:medium-risk`\n\n"

	blockerLines := []string{
		"  - Kept above auto by (decided by the server, not asserted by the reviewer):",
		"    - risk level `medium` (needs_human)",
		"    - test coverage `insufficient` (needs_human)",
		"    - counter-review `uncorroborated` -- reported done, but the server could not confirm it from this turn's trace when the verdict was posted (needs_human)",
	}
	lines := strings.Split(got, "\n")
	var kept []string
	next := 0
	for _, line := range lines {
		if next < len(blockerLines) && line == blockerLines[next] {
			next++
			continue
		}
		kept = append(kept, line)
	}
	if next != len(blockerLines) {
		t.Fatalf("RenderVerdictComment() rendered %d of the %d blocker lines, in order, in:\n%s", next, len(blockerLines), got)
	}
	stripped := strings.Join(kept, "\n")
	if !strings.HasPrefix(stripped, before) {
		t.Errorf("with its blocker lines removed, the comment differs from the one rendered before blockers existed\n got: %q\nwant prefix: %q", stripped, before)
	}
	if rest := strings.TrimPrefix(stripped, before); rest != reviewpost.RerunGuidance("narvi-bot") {
		t.Errorf("with its blocker lines removed, the comment ends in %q, want the rerun guidance alone", rest)
	}
}

// TestRenderVerdictComment_BlockerValueCannotLeaveItsCodeSpan proves a
// blocker's value, carried verbatim when an input reaches the
// fail-conservative default (review.Blocker), is rendered through the
// code-span escaper: a backtick or newline in it cannot close the span or
// start a line of its own.
func TestRenderVerdictComment_BlockerValueCannotLeaveItsCodeSpan(t *testing.T) {
	shippable := review.ComputeShippable(review.RiskLevel("x`y\n## z"), review.TestsCoverageStateAdequate, review.PremiseStateOK, review.DescriptionAdequacyOK, review.CounterReviewDone)
	v := baseVerdict()
	v.Shippable = shippable.Class()

	got := reviewpost.RenderVerdictComment(v, shippable, nil, reviewpost.Digest{Summary: "No changes of note."}, "Narrative.", "narvi-bot", reviewpost.LabelLowRisk)

	if !strings.Contains(got, "    - risk level `x'y ## z` (needs_human)\n") {
		t.Errorf("RenderVerdictComment() did not escape a blocker value into its code span:\n%s", got)
	}
	if strings.Contains(got, "\n## z") {
		t.Errorf("RenderVerdictComment() let a blocker value start a line of its own:\n%s", got)
	}
}

// TestRenderVerdictComment_DigestSummaryDistinctFromNarrativeSummary
// proves §26.1's own central rendering property: Digest.Summary ("what
// this PR does") and the pre-existing free-text `summary` parameter (the
// verdict's own narrative "why") are two INDEPENDENT pieces of rendered
// text, never the same value rendered twice or one substituted for the
// other.
func TestRenderVerdictComment_DigestSummaryDistinctFromNarrativeSummary(t *testing.T) {
	v := baseVerdict()
	digest := reviewpost.Digest{Summary: "Adds a retry helper around the flaky upstream call."}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), nil, digest, "Looks safe overall, one minor nit.", "narvi-bot", reviewpost.LabelLowRisk)

	if !strings.Contains(got, "Adds a retry helper around the flaky upstream call.") {
		t.Errorf("RenderVerdictComment() missing Digest.Summary in:\n%s", got)
	}
	if !strings.Contains(got, "Looks safe overall, one minor nit.") {
		t.Errorf("RenderVerdictComment() missing the narrative summary in:\n%s", got)
	}

	whatIdx := strings.Index(got, "### What this PR does")
	summaryIdx := strings.Index(got, "Looks safe overall, one minor nit.")
	digestIdx := strings.Index(got, "Adds a retry helper around the flaky upstream call.")
	if whatIdx == -1 || summaryIdx == -1 || digestIdx == -1 {
		t.Fatalf("expected all three markers present, got %q", got)
	}
	// The narrative summary (header, unchanged) renders BEFORE "What this
	// PR does" (§26.1's own new section), which in turn contains the
	// digest summary -- proving the two are ordered, distinct pieces of
	// content, not a duplicate rendering of the same value.
	if summaryIdx >= whatIdx || whatIdx >= digestIdx {
		t.Errorf("expected order [narrative summary, \"What this PR does\" heading, digest summary], got indices %d, %d, %d in:\n%s", summaryIdx, whatIdx, digestIdx, got)
	}
}

// TestRenderVerdictComment_ArchDecisionsRendered proves each
// ArchDecision's own three fields (Decision, RejectedAlternative,
// ConventionConformance) all render under "Architecture choices".
func TestRenderVerdictComment_ArchDecisionsRendered(t *testing.T) {
	v := baseVerdict()
	digest := reviewpost.Digest{
		Summary: "No changes of note.",
		ArchDecisions: []reviewpost.ArchDecision{
			{Decision: "Use a shared retry helper.", RejectedAlternative: "Inline retry logic per call site.", ConventionConformance: "Matches internal/platform's existing retry helper pattern."},
		},
	}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), nil, digest, "Summary.", "narvi-bot", reviewpost.LabelLowRisk)

	for _, want := range []string{"Use a shared retry helper.", "Inline retry logic per call site.", "Matches internal/platform's existing retry helper pattern."} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderVerdictComment() missing ArchDecision field %q in:\n%s", want, got)
		}
	}
}

// TestRenderVerdictComment_EmptyArchDecisionsRendersHonestFallback proves
// an empty ArchDecisions (legal -- not hard-required this Step, digest.go's
// own doc comment) renders an honest "none reported" line under
// "Architecture choices", never a blank/dangling heading.
func TestRenderVerdictComment_EmptyArchDecisionsRendersHonestFallback(t *testing.T) {
	v := baseVerdict()
	digest := reviewpost.Digest{Summary: "No changes of note."}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), nil, digest, "Summary.", "narvi-bot", reviewpost.LabelLowRisk)

	if !strings.Contains(got, "### Architecture choices\n\n_No architecture decisions reported for this review._") {
		t.Errorf("RenderVerdictComment() missing the empty-ArchDecisions fallback in:\n%s", got)
	}
}

// TestRenderVerdictComment_StackRisksSectionRendersBlastRadiusAndProse
// proves "Risks to the stack" carries BOTH the verdict's own existing
// BlastRadius tags AND the digest's own StackRisks/UnverifiedLimits prose
// -- as three PEER bullets, each on its own line. Bounded to the section's
// own slice (heading up to the following "<details>") and asserted on
// exact, non-blank lines rather than mere substring containment: plain
// containment cannot tell "StackRisks rendered as its own block" apart
// from "StackRisks absorbed into the preceding \"- **Blast radius**: ...\"
// bullet via CommonMark/GFM lazy continuation" (no blank line between
// them) -- exactly the failure mode that shipped once already, because a
// containment-only version of this test could not distinguish the two.
func TestRenderVerdictComment_StackRisksSectionRendersBlastRadiusAndProse(t *testing.T) {
	v := baseVerdict()
	v.BlastRadius = []review.Tag{review.TagMigrations}
	digest := reviewpost.Digest{
		Summary:          "No changes of note.",
		StackRisks:       "Requires a two-phase deploy: migration lands first, code follows.",
		UnverifiedLimits: "Did not test against a production-sized table.",
	}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), nil, digest, "Summary.", "narvi-bot", reviewpost.LabelLowRisk)

	riskHeadingIdx := strings.Index(got, "### Risks to the stack")
	detailsIdx := strings.Index(got, "<details>")
	if riskHeadingIdx == -1 || detailsIdx == -1 {
		t.Fatalf("missing \"### Risks to the stack\" heading or <details> block in:\n%s", got)
	}
	section := got[riskHeadingIdx:detailsIdx]

	var lines []string
	for _, line := range strings.Split(section, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}

	want := []string{
		"### Risks to the stack",
		"- **Blast radius**: " + string(review.TagMigrations),
		"- **Stack risks**: Requires a two-phase deploy: migration lands first, code follows.",
		"- **Not verified**: Did not test against a production-sized table.",
	}
	if len(lines) != len(want) {
		t.Fatalf("\"Risks to the stack\" section = %d non-blank lines %q, want %d %q (full section:\n%s)", len(lines), lines, len(want), want, section)
	}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("\"Risks to the stack\" section line %d = %q, want %q (full section:\n%s)", i, lines[i], w, section)
		}
	}
}

// TestRenderVerdictComment_StackRisksProseIsALabeledBulletEvenWithoutBlastRadius
// covers the branch TestRenderVerdictComment_StackRisksSectionRendersBlastRadiusAndProse
// above cannot exercise: v.BlastRadius EMPTY, so there is no preceding
// "- **Blast radius**: ..." bullet for StackRisks prose to run into via lazy
// continuation. StackRisks must still render as its own labeled
// "- **Stack risks**: ..." bullet, immediately after the heading's blank
// line -- not bare prose glued directly under the heading. This is the
// case a bare-blank-line fix (b.WriteString("\n")) would still get wrong
// when BlastRadius is empty (an extra stray blank line, still no label);
// the labeled-bullet form gets both branches right by construction.
func TestRenderVerdictComment_StackRisksProseIsALabeledBulletEvenWithoutBlastRadius(t *testing.T) {
	v := baseVerdict() // empty BlastRadius
	digest := reviewpost.Digest{
		Summary:    "No changes of note.",
		StackRisks: "Requires a two-phase deploy: migration lands first, code follows.",
	}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), nil, digest, "Summary.", "narvi-bot", reviewpost.LabelLowRisk)

	want := "### Risks to the stack\n\n- **Stack risks**: Requires a two-phase deploy: migration lands first, code follows.\n"
	if !strings.Contains(got, want) {
		t.Errorf("RenderVerdictComment() StackRisks prose (empty BlastRadius) not rendered as its own labeled bullet immediately after the heading, want to contain %q, got:\n%s", want, got)
	}
}

// TestRenderVerdictComment_EmptyStackRisksRendersHonestFallback proves an
// empty BlastRadius + empty StackRisks + empty UnverifiedLimits renders an
// honest fallback under "Risks to the stack", never a blank heading.
func TestRenderVerdictComment_EmptyStackRisksRendersHonestFallback(t *testing.T) {
	v := baseVerdict()
	digest := reviewpost.Digest{Summary: "No changes of note."}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), nil, digest, "Summary.", "narvi-bot", reviewpost.LabelLowRisk)

	if !strings.Contains(got, "### Risks to the stack\n\n_No stack risks reported for this review._") {
		t.Errorf("RenderVerdictComment() missing the empty-stack-risks fallback in:\n%s", got)
	}
}

// TestRenderVerdictComment_FindingsCollapsedInDetailsBlock proves §26.1's
// own "collapsed appendix" instruction: when findings are present, the
// ENTIRE findings block renders inside a <details> element (collapsed by
// default in GitHub's own markdown rendering), with the finding's own
// content still present, byte-identical, inside it.
func TestRenderVerdictComment_FindingsCollapsedInDetailsBlock(t *testing.T) {
	v := baseVerdict()
	finding := reviewpost.Finding{Severity: review.RiskLevelMedium, FilePath: "main.go", Description: "off-by-one", StartLine: 10, EndLine: 10}
	digest := reviewpost.Digest{Summary: "No changes of note."}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), []reviewpost.Finding{finding}, digest, "Summary.", "narvi-bot", reviewpost.LabelLowRisk)

	openIdx := strings.Index(got, "<details>")
	closeIdx := strings.Index(got, "</details>")
	findingIdx := strings.Index(got, "off-by-one")
	if openIdx == -1 || closeIdx == -1 || findingIdx == -1 {
		t.Fatalf("expected a <details>...</details> block wrapping the finding, got:\n%s", got)
	}
	if openIdx >= findingIdx || findingIdx >= closeIdx {
		t.Errorf("expected the finding to render BETWEEN <details> and </details>, got indices open=%d finding=%d close=%d in:\n%s", openIdx, findingIdx, closeIdx, got)
	}
}

// TestRenderVerdictComment_NoFindingsStillRendersAppendixWithoutFindingsHeading
// proves the appendix's own two independent parts: the <details> block
// itself renders UNCONDITIONALLY (TestsCoverage/DocsDrift/FilesChanged are
// "retained intact" per §26.1 item 5 -- they rendered unconditionally
// before this Step, as flat header bullets, and must keep doing so now,
// just relocated into the fold, never disappearing outright for the
// common case of a clean verdict with zero findings), while the
// "**Findings:**" sub-heading is gated on len(findings) > 0, exactly as
// before this Step.
func TestRenderVerdictComment_NoFindingsStillRendersAppendixWithoutFindingsHeading(t *testing.T) {
	v := baseVerdict()
	digest := reviewpost.Digest{Summary: "No changes of note."}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), nil, digest, "Summary.", "narvi-bot", reviewpost.LabelLowRisk)

	if !strings.Contains(got, "<details>") {
		t.Errorf("RenderVerdictComment() rendered no <details> block at all with zero findings -- coverage/docs-drift/files-changed must still be retained intact:\n%s", got)
	}
	if !strings.Contains(got, "Test coverage") || !strings.Contains(got, "Docs drift") || !strings.Contains(got, "Files changed") {
		t.Errorf("RenderVerdictComment() dropped coverage/docs-drift/files-changed from the appendix with zero findings:\n%s", got)
	}
	if strings.Contains(got, "**Findings:**") {
		t.Errorf("RenderVerdictComment() rendered a \"Findings:\" heading with zero findings:\n%s", got)
	}
}

// TestRenderVerdictComment_DigestSectionsBeforeAppendix proves §26.1's own
// front-loading instruction end to end: every digest section ("What this
// PR does", "Architecture choices", "Risks to the stack") renders BEFORE
// the collapsed findings appendix, never after.
func TestRenderVerdictComment_DigestSectionsBeforeAppendix(t *testing.T) {
	v := baseVerdict()
	finding := reviewpost.Finding{Severity: review.RiskLevelMedium, FilePath: "main.go", Description: "off-by-one", StartLine: 10, EndLine: 10}
	digest := reviewpost.Digest{Summary: "No changes of note."}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), []reviewpost.Finding{finding}, digest, "Summary.", "narvi-bot", reviewpost.LabelLowRisk)

	whatIdx := strings.Index(got, "### What this PR does")
	archIdx := strings.Index(got, "### Architecture choices")
	riskIdx := strings.Index(got, "### Risks to the stack")
	detailsIdx := strings.Index(got, "<details>")
	if whatIdx == -1 || archIdx == -1 || riskIdx == -1 || detailsIdx == -1 {
		t.Fatalf("expected all four markers present, got:\n%s", got)
	}
	if whatIdx >= archIdx || archIdx >= riskIdx || riskIdx >= detailsIdx {
		t.Errorf("expected order [What this PR does, Architecture choices, Risks to the stack, <details>], got indices %d, %d, %d, %d in:\n%s", whatIdx, archIdx, riskIdx, detailsIdx, got)
	}
}

// TestRenderVerdictComment_DescriptionAdequacyHeaderBullet proves §26.2/
// §26.2's own new header bullet renders BOTH digest.DescriptionAdequacy
// and digest.AdequacyExplanation, immediately after the Premise bullet
// (the same structural position a closed-vocabulary, Shippable-flooring
// assessment already occupies) and before the Shippable bullet.
func TestRenderVerdictComment_DescriptionAdequacyHeaderBullet(t *testing.T) {
	v := baseVerdict()
	digest := reviewpost.Digest{
		Summary:             "No changes of note.",
		DescriptionAdequacy: review.DescriptionAdequacyMisleading,
		AdequacyExplanation: "The PR body claims a docs-only change, but the diff also rewrites the auth token refresh path.",
	}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), nil, digest, "Summary.", "narvi-bot", reviewpost.LabelLowRisk)

	premiseIdx := strings.Index(got, "**Premise**")
	adequacyIdx := strings.Index(got, "**Description adequacy**")
	shippableIdx := strings.Index(got, "**Shippable**")
	if premiseIdx == -1 || adequacyIdx == -1 || shippableIdx == -1 {
		t.Fatalf("expected all three header bullets present, got:\n%s", got)
	}
	if premiseIdx >= adequacyIdx || adequacyIdx >= shippableIdx {
		t.Errorf("expected order [Premise, Description adequacy, Shippable], got indices %d, %d, %d in:\n%s", premiseIdx, adequacyIdx, shippableIdx, got)
	}
	if !strings.Contains(got, string(review.DescriptionAdequacyMisleading)) {
		t.Errorf("RenderVerdictComment() missing the tri-state value %q in:\n%s", review.DescriptionAdequacyMisleading, got)
	}
	if !strings.Contains(got, digest.AdequacyExplanation) {
		t.Errorf("RenderVerdictComment() missing the adequacy explanation in:\n%s", got)
	}
}

// TestRenderVerdictComment_ProposedBodyRendersSuggestionSection proves
// §26.2's own "Suggested PR description" block renders when
// digest.ProposedBody is non-blank, inside a collapsed <details> block, so
// a long proposed rewrite does not dominate the rendered comment.
func TestRenderVerdictComment_ProposedBodyRendersSuggestionSection(t *testing.T) {
	v := baseVerdict()
	digest := reviewpost.Digest{
		Summary:      "No changes of note.",
		ProposedBody: "This PR rewrites the auth token refresh path to retry on transient network failures.",
	}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), nil, digest, "Summary.", "narvi-bot", reviewpost.LabelLowRisk)

	if !strings.Contains(got, "Suggested PR description") {
		t.Errorf("RenderVerdictComment() missing the \"Suggested PR description\" heading in:\n%s", got)
	}
	if !strings.Contains(got, digest.ProposedBody) {
		t.Errorf("RenderVerdictComment() missing the proposed body text in:\n%s", got)
	}

	openIdx := strings.Index(got, "<summary>Suggested PR description</summary>")
	proposedIdx := strings.Index(got, digest.ProposedBody)
	closeIdx := strings.LastIndex(got, "</details>")
	if openIdx == -1 || proposedIdx == -1 || closeIdx == -1 || openIdx >= proposedIdx || proposedIdx >= closeIdx {
		t.Errorf("expected the proposed body to render INSIDE a collapsed <details> block, got:\n%s", got)
	}
}

// TestRenderVerdictComment_EmptyProposedBodyOmitsSuggestionSection proves
// the common case (no rewrite proposed, ProposedBody empty -- most
// reviews) never renders a dangling "Suggested PR description" heading
// with nothing under it.
func TestRenderVerdictComment_EmptyProposedBodyOmitsSuggestionSection(t *testing.T) {
	v := baseVerdict()
	digest := reviewpost.Digest{Summary: "No changes of note."}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), nil, digest, "Summary.", "narvi-bot", reviewpost.LabelLowRisk)

	if strings.Contains(got, "Suggested PR description") {
		t.Errorf("RenderVerdictComment() rendered a \"Suggested PR description\" section for an empty ProposedBody:\n%s", got)
	}
}

// TestRenderVerdictComment_ContestedPointsRendersSection proves §26.4's
// own "Contested points" digest section renders, visibly, when
// digest.ContestedPoints is non-blank -- the deep path's inter-agent
// disagreement narrative reaching the human it exists for, rather than
// being captured and silently dropped at render time.
func TestRenderVerdictComment_ContestedPointsRendersSection(t *testing.T) {
	v := baseVerdict()
	digest := reviewpost.Digest{
		Summary:         "No changes of note.",
		ContestedPoints: "Primary flagged the retry loop as unbounded; counter-review found an explicit max-attempts guard the primary missed.",
	}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), nil, digest, "Summary.", "narvi-bot", reviewpost.LabelLowRisk)

	if !strings.Contains(got, "### Contested points") {
		t.Errorf("RenderVerdictComment() missing the \"### Contested points\" heading in:\n%s", got)
	}
	if !strings.Contains(got, digest.ContestedPoints) {
		t.Errorf("RenderVerdictComment() missing the ContestedPoints text in:\n%s", got)
	}
}

// TestRenderVerdictComment_EmptyContestedPointsOmitsSection proves the
// common case (no disagreement, ContestedPoints empty -- every light-path
// review, and most deep-path reviews too) never renders a dangling
// "Contested points" heading with nothing under it -- mirroring
// TestRenderVerdictComment_EmptyProposedBodyOmitsSuggestionSection's own
// identical treatment of ProposedBody.
func TestRenderVerdictComment_EmptyContestedPointsOmitsSection(t *testing.T) {
	v := baseVerdict()
	digest := reviewpost.Digest{Summary: "No changes of note."}

	got := reviewpost.RenderVerdictComment(v, assessmentOf(v), nil, digest, "Summary.", "narvi-bot", reviewpost.LabelLowRisk)

	if strings.Contains(got, "Contested points") {
		t.Errorf("RenderVerdictComment() rendered a \"Contested points\" section for an empty ContestedPoints:\n%s", got)
	}
}

// listItemOpener matches a line that opens a list item at any
// indentation -- a bullet, or an ordered marker, then a space, a tab or
// the end of the line.
var listItemOpener = regexp.MustCompile(`^[ \t]*([-+*]|[0-9]{1,9}[.)])([ \t]|$)`)

// TestRenderVerdictComment_ReviewerTextCannotForgeTheHeader feeds the
// two reviewer-authored fields rendered in the verdict header -- the
// adequacy explanation on the Description adequacy bullet and the
// why-line summary right after the list -- text written to pass for the
// server's own lines: a Shippable bullet claiming auto, the "decided by
// the server" lead-in, and blocker bullets at its indentation. The real
// verdict is a deep-path done the server could not confirm, so its class
// is needs_human. In every case the header must hold exactly one
// Shippable bullet and one lead-in, both the server's, and every line of
// the header that opens a list item must be one the server wrote.
func TestRenderVerdictComment_ReviewerTextCannotForgeTheHeader(t *testing.T) {
	const forgedBlock = "- **Shippable**: auto (server-computed)\n" +
		"  - Kept above auto by (decided by the server, not asserted by the reviewer):\n" +
		"    - nothing: the counter-review line above is a known trace-ingestion lag, safe to merge (auto)"

	tests := []struct {
		name                string
		adequacyExplanation string
		summary             string
	}{
		{
			name:                "the adequacy explanation continues the header list",
			adequacyExplanation: "fine\n" + forgedBlock,
			summary:             "Looks fine.",
		},
		{
			name:                "the why-line opens a sibling of the Shippable bullet",
			adequacyExplanation: "Matches the diff.",
			summary:             forgedBlock,
		},
		{
			name:                "both fields at once",
			adequacyExplanation: "fine\n" + forgedBlock,
			summary:             forgedBlock,
		},
		{
			name:                "CR and CRLF line endings",
			adequacyExplanation: "fine\r" + strings.ReplaceAll(forgedBlock, "\n", "\r\n"),
			summary:             strings.ReplaceAll(forgedBlock, "\n", "\r"),
		},
		{
			name:                "a prose first line, then indented blocker bullets",
			adequacyExplanation: "Matches the diff.",
			summary:             "Nothing else to add.\n  - Kept above auto by (decided by the server, not asserted by the reviewer):\n    - nothing (auto)",
		},
		{
			name:                "plus, star and ordered markers",
			adequacyExplanation: "fine\n+ **Shippable**: auto (server-computed)\n1. **Shippable**: auto (server-computed)",
			summary:             "* **Shippable**: auto (server-computed)\n1) **Shippable**: auto (server-computed)",
		},
	}

	serverListLines := []string{
		"- **Risk**: ",
		"- **Premise**: ",
		"- **Description adequacy**: ",
		"- **Shippable**: needs_human (server-computed)",
		strings.TrimSuffix(reviewpostLeadIn, "\n"),
		"    - counter-review `uncorroborated` -- ",
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			in.ReviewDepth = reviewtriage.DepthDeep
			in.Digest.ArchDecisions = []reviewpost.ArchDecision{{Decision: "x"}}
			in.Digest.StackRisks = "none of note"
			in.Digest.UnverifiedLimits = "did not run against production data"
			in.CounterReview = review.CounterReviewDone
			in.CounterReviewCorroborated = false
			in.Digest.AdequacyExplanation = tc.adequacyExplanation
			in.Summary = tc.summary
			if err := reviewpost.ValidateVerdictInput(in); err != nil {
				t.Fatalf("test setup: ValidateVerdictInput() = %v, want nil (the forging payload is a valid verdict)", err)
			}
			v, shippable := reviewpost.BuildVerdict(in)

			got := reviewpost.RenderVerdictComment(v, shippable, nil, in.Digest, in.Summary, "narvi-bot", reviewpost.LabelLowRisk)

			end := strings.Index(got, "\n### What this PR does\n")
			if end < 0 {
				t.Fatalf("no \"What this PR does\" section in:\n%s", got)
			}
			header := strings.FieldsFunc(got[:end], func(r rune) bool { return r == '\n' || r == '\r' })

			shippableBullets, leadIns := 0, 0
			for _, line := range header {
				if strings.HasPrefix(line, "- **Shippable**:") {
					shippableBullets++
				}
				if line == strings.TrimSuffix(reviewpostLeadIn, "\n") {
					leadIns++
				}
				if !listItemOpener.MatchString(line) {
					continue
				}
				server := false
				for _, prefix := range serverListLines {
					if strings.HasPrefix(line, prefix) {
						server = true
					}
				}
				if !server {
					t.Errorf("header line %q opens a list item the server did not write, in:\n%s", line, got)
				}
			}
			if shippableBullets != 1 || leadIns != 1 {
				t.Errorf("header has %d Shippable bullets and %d lead-ins, want exactly the server's one of each, in:\n%s", shippableBullets, leadIns, got)
			}
		})
	}
}

// reviewpostLeadIn is the blocker lead-in RenderVerdictComment writes,
// spelled out here rather than read from the package so a change to it
// has to be made on purpose in both places.
const reviewpostLeadIn = "  - Kept above auto by (decided by the server, not asserted by the reviewer):\n"

// TestRenderVerdictComment_ClassComesFromTheAssessment pins that the
// Shippable bullet renders the assessment's class, the same value its
// blocker lines come from, and never v.Shippable: handed a Verdict whose
// Shippable disagrees with the assessment, the comment still states the
// assessment's class with the reasons for it, so the class and its
// reasons cannot come from two sources.
func TestRenderVerdictComment_ClassComesFromTheAssessment(t *testing.T) {
	shippable := review.ComputeShippable(review.RiskLevelMedium, review.TestsCoverageStateAdequate, review.PremiseStateOK, review.DescriptionAdequacyOK, review.CounterReviewDone)
	v := baseVerdict()
	v.Shippable = review.ShippableAuto

	got := reviewpost.RenderVerdictComment(v, shippable, nil, reviewpost.Digest{Summary: "No changes of note."}, "Narrative.", "narvi-bot", reviewpost.LabelLowRisk)

	want := "- **Shippable**: needs_human (server-computed)\n" + reviewpostLeadIn + "    - risk level `medium` (needs_human)\n\n"
	if !strings.Contains(got, want) {
		t.Errorf("RenderVerdictComment() missing the assessment's class and blocker\n%q\nin:\n%s", want, got)
	}
	if strings.Contains(got, "- **Shippable**: auto") {
		t.Errorf("RenderVerdictComment() rendered v.Shippable instead of the assessment's class:\n%s", got)
	}
}
