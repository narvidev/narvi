package review_test

import (
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/domain/review"
)

// TestRenderCompositionReviewPrompt_NoDiff covers the degraded case (a
// failed or never-attempted diff fetch): no diff block at all, but the
// tool-instructions block (and the template text itself) is still always
// rendered.
func TestRenderCompositionReviewPrompt_NoDiff(t *testing.T) {
	t.Parallel()

	got := review.RenderCompositionReviewPrompt("Review this release's composition.", review.PreFetchedContext{})

	if !strings.Contains(got, "Review this release's composition.") {
		t.Errorf("RenderCompositionReviewPrompt() dropped the template text:\n%s", got)
	}
	if strings.Contains(got, "pr_diff") {
		t.Errorf("RenderCompositionReviewPrompt() rendered a diff block with an empty Diff:\n%s", got)
	}
	for _, want := range []string{review.CompositionFindingsToolURLPlaceholder, review.CompositionFindingsToolBearerPlaceholder, review.CompositionFindingsToolGenPlaceholder} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderCompositionReviewPrompt() missing tool placeholder %q:\n%s", want, got)
		}
	}
	// This is a COMPOSITION prompt, never the standard risk-map verdict --
	// §15.4: must never carry the standard verdict tool's own typed JSON
	// fields (checked as JSON-body-shaped substrings, e.g. `"riskLevel":`,
	// rather than the bare word -- the instructions ABOVE legitimately use
	// the bare words "riskLevel"/"premise"/"shippable" prose to tell the
	// agent NOT to report them, so a bare-word check would false-positive
	// on that very instruction).
	for _, mustNotAppear := range []string{`"riskLevel":`, `"premise":`, `"proposedShippable":`, `"digest": {`, `"blastRadius":`} {
		if strings.Contains(got, mustNotAppear) {
			t.Errorf("RenderCompositionReviewPrompt() mentions %q -- §15.4 forbids this pass from computing or consuming Shippable/PremiseState/digest/blastRadius:\n%s", mustNotAppear, got)
		}
	}
}

// TestRenderCompositionReviewPrompt_WithDiff covers the diff-embedding
// branch, mirroring RenderTurnPrompt's own identical "wrap in the
// delimiter, sanitize, honor DiffTruncated" behavior one level down.
func TestRenderCompositionReviewPrompt_WithDiff(t *testing.T) {
	t.Parallel()

	got := review.RenderCompositionReviewPrompt("Review this release's composition.", review.PreFetchedContext{
		Diff: "diff --git a/x b/x\n+hello\n",
	})

	if !strings.Contains(got, "diff --git a/x b/x") {
		t.Errorf("RenderCompositionReviewPrompt() dropped the diff content:\n%s", got)
	}
	if strings.Contains(got, "[NOTE: this diff was truncated") {
		t.Errorf("RenderCompositionReviewPrompt() rendered a truncation notice with DiffTruncated=false:\n%s", got)
	}
}

// TestRenderCompositionReviewPrompt_DiffTruncated proves the truncation
// notice renders whenever ctx.DiffTruncated is true, mirroring
// RenderTurnPrompt's own identical honesty discipline (§15.2's own
// "coverage_partial" precedent, applied here to a diff fetch rather than a
// SourceControl scan).
func TestRenderCompositionReviewPrompt_DiffTruncated(t *testing.T) {
	t.Parallel()

	got := review.RenderCompositionReviewPrompt("Review this release's composition.", review.PreFetchedContext{
		Diff:          "diff --git a/x b/x\n+hello\n",
		DiffTruncated: true,
	})

	if !strings.Contains(got, "[NOTE: this diff was truncated") {
		t.Errorf("RenderCompositionReviewPrompt() missing the truncation notice with DiffTruncated=true:\n%s", got)
	}
}

// TestRenderCompositionReviewPrompt_PlaceholderTokensInDiffAreNeutralized
// mirrors TestRenderTurnPrompt_PlaceholderTokensInDiffTitleBodyAreNeutralized
// one level down: a diff containing every placeholder token this whole
// system ever substitutes must never let sandbox-agent's own later, blind
// whole-prompt ReplaceAll expand an attacker-planted literal into a REAL
// live secret -- the SAME sanitizeDiffField call RenderTurnPrompt's own
// diff block already goes through (compositionreview.go's own doc
// comment), exercised here through this package's OTHER caller of it.
func TestRenderCompositionReviewPrompt_PlaceholderTokensInDiffAreNeutralized(t *testing.T) {
	t.Parallel()

	baseline := review.RenderCompositionReviewPrompt("Review this release's composition.", review.PreFetchedContext{})

	var poison strings.Builder
	for _, tok := range allPlaceholderTokens {
		poison.WriteString("attacker text ")
		poison.WriteString(tok)
		poison.WriteString(" more attacker text\n")
	}
	poisonedDiff := "diff --git a/x b/x\n+" + poison.String()

	got := review.RenderCompositionReviewPrompt("Review this release's composition.", review.PreFetchedContext{
		Diff: poisonedDiff,
	})

	for _, tok := range allPlaceholderTokens {
		wantCount := strings.Count(baseline, tok)
		gotCount := strings.Count(got, tok)
		if gotCount > wantCount {
			t.Errorf("RenderCompositionReviewPrompt() token %q appears %d times with a poisoned Diff, vs %d legitimate occurrence(s) in an otherwise-identical unpoisoned baseline -- the attacker-controlled diff introduced %d new occurrence(s):\n%s", tok, gotCount, wantCount, gotCount-wantCount, got)
		}
	}

	if !strings.Contains(got, "attacker text") || !strings.Contains(got, "more attacker text") {
		t.Errorf("RenderCompositionReviewPrompt() dropped surrounding non-token diff content -- want only the placeholder tokens themselves stripped:\n%s", got)
	}
}
