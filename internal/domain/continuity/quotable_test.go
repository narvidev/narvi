package continuity_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

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

// reviewShapedPrompt is a review turn's stored prompt at the size the
// GitHub adapter allows a pull request's diff: its verdict tool's tokens up
// front, then size bytes of diff carrying tokens of its own.
func reviewShapedPrompt(size int) string {
	var b strings.Builder
	b.WriteString("Review this pull request. Post the verdict to " + review.VerdictToolURLPlaceholder +
		" with " + review.VerdictToolBearerPlaceholder + ".\n```diff\n")
	line := "+ a changed line quoting " + upload.BearerPlaceholder + "\n"
	for b.Len() < size {
		b.WriteString(line)
	}
	b.WriteString("```\n")
	return b.String()
}

// TestTurnQuotable_HoldsEachTextToWhatAnExcerptQuotes: preparing a turn
// keeps of its request and report only the head an excerpt can quote,
// however long they are stored, and the recap's cut lines still name their
// whole lengths.
func TestTurnQuotable_HoldsEachTextToWhatAnExcerptQuotes(t *testing.T) {
	t.Parallel()
	prompt := reviewShapedPrompt(4 << 20)
	report := strings.Repeat("A finding, quoted: "+review.VerdictToolGenPlaceholder+".\n", 4<<20/40)
	prepared := completed(1, prompt, report).Quotable()
	if len(prepared.Request) >= 2*continuity.RequestExcerptBytes || len(prepared.Report.Text) >= 2*continuity.ReportExcerptBytes {
		t.Fatalf("prepared request %d bytes, report %d bytes; want each held near its excerpt's cap, %d and %d",
			len(prepared.Request), len(prepared.Report.Text), continuity.RequestExcerptBytes, continuity.ReportExcerptBytes)
	}
	again := prepared.Quotable()
	if again.Request != prepared.Request || again.Report.Text != prepared.Report.Text {
		t.Fatal("a prepared turn prepared again changed")
	}
	recap, ok := continuity.Build(continuity.Input{Turns: []continuity.Turn{prepared}}, unbounded)
	if !ok {
		t.Fatal("Build is not ok with an unbounded budget")
	}
	for _, want := range []string{" of " + strconv.Itoa(len(prompt)) + " bytes.]", " of " + strconv.Itoa(len(report)) + " bytes.]"} {
		if !strings.Contains(recap.Text, want) {
			t.Errorf("recap:\n%s\nwant a cut line naming the whole length: %q", recap.Text, want)
		}
	}
	if strings.Contains(recap.Text, "{{") {
		t.Errorf("recap carries a token:\n%s", recap.Text)
	}
}

// TestBuild_ManyMiBReviewPromptsBuildWithinABound: a recap over the most
// earlier turns it reads, each a review turn whose stored prompt carries a
// 4 MiB diff and whose report is as long, built the way its caller builds
// it -- each turn prepared once as it is read, a Build after each, and a
// last Build -- or built once from turns no one prepared, costs what the
// excerpt caps allow, not what the stored texts weigh.
func TestBuild_ManyMiBReviewPromptsBuildWithinABound(t *testing.T) {
	t.Parallel()
	prompt := reviewShapedPrompt(4 << 20)
	report := strings.Repeat("A finding, quoted: "+review.VerdictToolGenPlaceholder+".\n", 4<<20/40)
	const bound = 2 * time.Second

	start := time.Now()
	var read []continuity.Turn // newest first
	for i := continuity.MaxTurns; i >= 1; i-- {
		read = append(read, completed(i, prompt, report).Quotable())
		oldestFirst := make([]continuity.Turn, len(read))
		for j, turn := range read {
			oldestFirst[len(read)-1-j] = turn
		}
		if _, ok := continuity.Build(continuity.Input{Turns: oldestFirst, Earlier: continuity.MaxTurns}, 16<<10); !ok {
			t.Fatal("Build is not ok")
		}
	}
	if elapsed := time.Since(start); elapsed > bound {
		t.Fatalf("the caller's Builds over %d prepared 4 MiB turns took %s, want under %s", continuity.MaxTurns, elapsed, bound)
	}

	raw := make([]continuity.Turn, continuity.MaxTurns)
	for i := range raw {
		raw[i] = completed(i+1, prompt, report)
	}
	start = time.Now()
	recap, ok := continuity.Build(continuity.Input{Turns: raw}, 16<<10)
	if elapsed := time.Since(start); elapsed > bound {
		t.Fatalf("one Build over %d unprepared 4 MiB turns took %s, want under %s", continuity.MaxTurns, elapsed, bound)
	}
	if !ok || strings.Contains(recap.Text, "{{") {
		t.Fatalf("recap ok %v, carrying a token: %v", ok, strings.Contains(recap.Text, "{{"))
	}
}

// TestBuild_StripsATokenThatStraddlesTheExcerptCap: a token that starts
// inside a request excerpt's cap and ends past it is still stripped whole,
// so no part of it is quoted.
func TestBuild_StripsATokenThatStraddlesTheExcerptCap(t *testing.T) {
	t.Parallel()
	for _, tok := range sandboxAgentTokens {
		request := strings.Repeat("x", continuity.RequestExcerptBytes-len(tok)/2) + tok + " and the rest of the request."
		recap, ok := continuity.Build(continuity.Input{Turns: []continuity.Turn{completed(1, request, "Done.")}}, unbounded)
		if !ok {
			t.Fatal("Build is not ok with an unbounded budget")
		}
		if strings.Contains(recap.Text, "{{") || strings.Contains(recap.Text, tok[2:len(tok)/2]) {
			t.Errorf("recap quotes part of %s straddling the cap:\n%s", tok, recap.Text)
		}
	}
}

// TestBuild_StripsWhatItsCallerLeftInAPreparedTurn: the built recap is
// stripped as a whole, so a token a caller puts into a turn after
// preparing it still never reaches the recap.
func TestBuild_StripsWhatItsCallerLeftInAPreparedTurn(t *testing.T) {
	t.Parallel()
	prepared := completed(1, "Fix the parser.", "Fixed it.").Quotable()
	prepared.Request = "Fix the parser, then call " + review.VerdictToolURLPlaceholder + "."
	prepared.Report.Text = "Fixed it with " + upload.BearerPlaceholder + "."
	interrupted := continuity.Attempt{Report: plan.Final{Text: "Halfway."}, Reported: true}.Quotable()
	interrupted.Report.Text = "Halfway, with " + turn.EpistemicOutcomeToolBearerPlaceholder + "."
	recap, ok := continuity.Build(continuity.Input{Turns: []continuity.Turn{prepared}, Interrupted: &interrupted}, unbounded)
	if !ok {
		t.Fatal("Build is not ok with an unbounded budget")
	}
	for _, tok := range sandboxAgentTokens {
		if strings.Contains(recap.Text, tok) {
			t.Errorf("recap carries %s:\n%s", tok, recap.Text)
		}
	}
	if !strings.Contains(recap.Text, "> Fix the parser, then call .") {
		t.Errorf("recap:\n%s\nwant the request kept, its token stripped", recap.Text)
	}
}
