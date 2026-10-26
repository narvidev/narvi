package continuity_test

import (
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/domain/continuity"
	"github.com/narvidev/narvi/internal/domain/plan"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/domain/upload"
)

// sandboxAgentTokens are the placeholder tokens the sandbox agent replaces
// with live values in a prompt's whole text before the agent runtime reads
// it, one entry per replacement of each of its render passes
// (cmd/sandbox-agent: renderVerdictToolPromptText, renderUploadToolPromptText,
// renderEpistemicOutcomeToolPromptText, renderReviewCostBudgetToolPromptText,
// renderCompositionFindingsToolPromptText).
var sandboxAgentTokens = []string{
	review.VerdictToolURLPlaceholder,
	review.VerdictToolBearerPlaceholder,
	review.VerdictToolGenPlaceholder,
	review.VerdictToolDispatchMessageIDPlaceholder,
	upload.BaseURLPlaceholder,
	upload.BearerPlaceholder,
	upload.GenPlaceholder,
	turn.EpistemicOutcomeToolURLPlaceholder,
	turn.EpistemicOutcomeToolBearerPlaceholder,
	turn.EpistemicOutcomeToolGenPlaceholder,
	review.ReviewCostBudgetToolURLPlaceholder,
	review.CompositionFindingsToolURLPlaceholder,
	review.CompositionFindingsToolBearerPlaceholder,
	review.CompositionFindingsToolGenPlaceholder,
}

// TestBuild_StripsEveryPlaceholderTokenTheSandboxAgentSubstitutes: the
// sandbox agent's render passes replace their tokens over a prompt's whole
// text, quoted recap lines included, so a token quoted from an earlier
// request, report or interrupted attempt would come out as a live value.
// Build strips every one from every excerpt, as every other text quoted
// into a prompt is stripped, splices included, and keeps the rest of the
// text.
func TestBuild_StripsEveryPlaceholderTokenTheSandboxAgentSubstitutes(t *testing.T) {
	t.Parallel()
	carrying := func(what string) string {
		var b strings.Builder
		for _, tok := range sandboxAgentTokens {
			b.WriteString(what + " quotes " + tok + " here.\n")
		}
		// A splice: removing the inner token must not leave an outer one.
		b.WriteString("{{UPLOAD_TOOL_" + review.VerdictToolGenPlaceholder + "BEARER}} " + what + " ends.")
		return b.String()
	}
	tests := []struct {
		name string
		in   continuity.Input
	}{
		{"an earlier request", continuity.Input{Turns: []continuity.Turn{completed(1, carrying("the request"), "Done.")}}},
		{"an earlier report", continuity.Input{Turns: []continuity.Turn{completed(1, "Fix the parser.", carrying("the report"))}}},
		{"the interrupted attempt", continuity.Input{Interrupted: &continuity.Attempt{Report: plan.Final{Text: carrying("the attempt")}, Reported: true}}},
		{"an earlier review request, composed with its tool's tokens", continuity.Input{Turns: []continuity.Turn{completed(1,
			"Review this pull request. Post the verdict to "+review.VerdictToolURLPlaceholder+" with "+review.VerdictToolBearerPlaceholder+
				" for message "+review.VerdictToolDispatchMessageIDPlaceholder+".", "Reviewed.")}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recap, ok := continuity.Build(tc.in, unbounded)
			if !ok {
				t.Fatal("Build is not ok with an unbounded budget")
			}
			for _, tok := range sandboxAgentTokens {
				if strings.Contains(recap.Text, tok) {
					t.Errorf("recap carries %s:\n%s", tok, recap.Text)
				}
			}
			if strings.Contains(recap.Text, "{{UPLOAD_TOOL_BEARER}}") || strings.Contains(recap.Text, "{{") {
				t.Errorf("recap carries a token's remains:\n%s", recap.Text)
			}
			if !strings.Contains(recap.Text, " here.") && !strings.Contains(recap.Text, "Review this pull request.") {
				t.Errorf("recap lost the text around the tokens:\n%s", recap.Text)
			}
			assertQuotedOrTemplate(t, recap.Text)
		})
	}
}

// TestPersonsRequest: the request a recap quotes is what was asked, read
// out of a stored prompt the control plane opened with its own fixed
// instructions -- the structured-plan instruction on a plan-mode turn, the
// epistemic preamble on a build turn -- and any other prompt as it is.
func TestPersonsRequest(t *testing.T) {
	t.Parallel()
	const asked = "Design a retry for the CSV importer: at most 3 attempts, never retry a 4xx."
	tests := []struct {
		name, prompt, want string
	}{
		{"a plan-mode turn", plan.MaybeInjectStructureInstruction(true, asked), asked},
		{"a build turn with the epistemic check on", turn.MaybeInjectEpistemicPreamble(true, nil, false, asked), asked},
		{"a turn with neither", asked, asked},
		{"an empty request after the instruction", plan.RenderStructureInstruction(), ""},
		{"the instruction quoted inside a request is kept", "Explain this: " + plan.RenderStructureInstruction(), "Explain this: " + plan.RenderStructureInstruction()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := continuity.PersonsRequest(tc.prompt); got != tc.want {
				t.Errorf("PersonsRequest = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuild_QuotesThePersonsRequestNotTheInjectedInstructions: a plan-mode
// turn's and an epistemic build turn's stored prompts open with the
// control plane's own instructions, longer than a request excerpt; the
// recap quotes the person's request whole, and none of the instructions.
func TestBuild_QuotesThePersonsRequestNotTheInjectedInstructions(t *testing.T) {
	t.Parallel()
	const asked = "Design a retry for the CSV importer: at most 3 attempts, never retry a 4xx."
	for _, tc := range []struct {
		name, prompt, injected string
	}{
		{"a plan-mode turn", plan.MaybeInjectStructureInstruction(true, asked), plan.RenderStructureInstruction()},
		{"a build turn with the epistemic check on", turn.MaybeInjectEpistemicPreamble(true, nil, false, asked), turn.RenderEpistemicPreamble()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if len(tc.injected) <= continuity.RequestExcerptBytes {
				t.Fatalf("the injected instructions are %d bytes, want more than a request excerpt's %d: the case this pins", len(tc.injected), continuity.RequestExcerptBytes)
			}
			recap, ok := continuity.Build(continuity.Input{Turns: []continuity.Turn{completed(1, tc.prompt, "Proposed a plan.")}}, unbounded)
			if !ok {
				t.Fatal("Build is not ok with an unbounded budget")
			}
			want := "\n" + continuity.LineRequest + "\n" + continuity.QuotePrefix + asked + "\n" + continuity.LineReport + "\n"
			if !strings.Contains(recap.Text, want) {
				t.Fatalf("recap:\n%s\nwant the person's request quoted whole: %q", recap.Text, want)
			}
			first, _, _ := strings.Cut(tc.injected, "\n")
			if strings.Contains(recap.Text, first) || recap.Trimmed {
				t.Fatalf("recap quotes the injected instructions or was cut:\n%s", recap.Text)
			}
		})
	}
}
