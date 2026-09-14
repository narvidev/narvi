package plan

import (
	"strings"
	"testing"
)

func TestMaybeInjectStructureInstruction(t *testing.T) {
	tests := []struct {
		name     string
		planMode bool
		prompt   string
		want     string
	}{
		{
			name:     "planMode false is a byte-for-byte no-op",
			planMode: false,
			prompt:   "please add dark mode",
			want:     "please add dark mode",
		},
		{
			name:     "planMode true prepends the fixed instruction",
			planMode: true,
			prompt:   "please add dark mode",
			want:     structureInstruction + "please add dark mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MaybeInjectStructureInstruction(tt.planMode, tt.prompt)
			if got != tt.want {
				t.Errorf("MaybeInjectStructureInstruction() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRenderStructureInstruction_MentionsTheExactFenceTagExtractStructuredLooksFor
// pins the ONE property that actually matters about this text (as opposed
// to its exact wording, which is free to evolve): it must ask for the SAME
// fence tag ExtractStructured (structured.go) recognizes, or the model
// would be asked for a shape this package can never successfully parse.
//
// This deliberately does NOT feed RenderStructureInstruction's own output
// into ExtractStructured: the instruction text is what this system sends
// the model as PROMPT input (and, being illustrative, already contains its
// own example fenced block as part of explaining the shape); ExtractStructured
// only ever runs against the model's own REPLY text, a completely separate
// string that never includes the prompt it was given. Concatenating the
// two would spuriously trip ExtractStructured's own "a second open fence is
// ambiguous" guard against the instruction's own embedded example -- a
// self-inflicted false negative, not a real defect.
func TestRenderStructureInstruction_MentionsTheExactFenceTagExtractStructuredLooksFor(t *testing.T) {
	got := RenderStructureInstruction()
	if !strings.Contains(got, StructureFenceOpen) {
		t.Errorf("RenderStructureInstruction() does not mention StructureFenceOpen (%q): %q", StructureFenceOpen, got)
	}
	if !strings.Contains(got, "scopeEstimate") || !strings.Contains(got, "fileRefs") {
		t.Errorf("RenderStructureInstruction() does not mention the required JSON keys: %q", got)
	}

	// A minimal, independent simulated REPLY following the instruction's
	// own documented shape must round-trip through ExtractStructured --
	// proves the schema described in prose above actually matches what the
	// strict decoder accepts.
	simulatedReply := "Here is my plan.\n\n" +
		StructureFenceOpen + "\n" +
		`{"steps":[{"title":"t","description":"d","fileRefs":[]}],"scopeEstimate":"1 file"}` +
		"\n```\n"
	if ExtractStructured(simulatedReply) == nil {
		t.Errorf("a minimal well-formed reply following this instruction's own documented shape failed to extract: %q", simulatedReply)
	}
}

// TestStructureInstruction_OwnExampleNeverParsesAsAPlan is the invariant
// that ties the prompt and the extractor together.
//
// The example the instruction shows is a fully schema-valid document, and
// the content ExtractStructured reads is the model's REPLY -- so a model
// that answers by echoing the format it was shown would produce a plan
// reading "short step title / what this step does and why", presented as
// its own on the screen where a human clicks Approve & build. Feeding the
// whole instruction back through the extractor is the cheapest statement of
// the rule, and it keeps failing if anyone later edits the example, adds a
// second one, or relaxes the extractor.
func TestStructureInstruction_OwnExampleNeverParsesAsAPlan(t *testing.T) {
	if got := ExtractStructured(RenderStructureInstruction()); got != nil {
		t.Errorf("the instruction's own example parses as a plan (steps=%d, scope=%q) -- a model echoing the requested format would render a fabricated plan for a human to approve",
			len(got.Steps), got.ScopeEstimate)
	}
}

// TestStructureInstruction_ExampleRejectionIsExact keeps isInstructionExample
// (structured.go) from quietly widening into a placeholder-shaped heuristic
// that would turn away real plans, and from quietly narrowing back into the
// defect fixed alongside this test: comparing against a field a model varies
// freely even while echoing the placeholder text (fileRefs) let the
// fabricated example plan through under a one-character fileRefs drift, an
// empty array, or an omitted key.
//
// isInstructionExample now keeps exactly four conjuncts: exactly one step,
// and that step's title/description plus the document's scopeEstimate all
// equal to the instruction's own placeholder text. Every subtest below
// mutation-verifies ONE of those four: it changes exactly that one thing
// away from the placeholder (or, for fileRefs, changes ONLY fileRefs, which
// is deliberately not a conjunct at all) and asserts the resulting effect --
// dropping any of the four kept conjuncts from the real implementation would
// flip at least one of these subtests from its asserted outcome, which is
// what "every conjunct it keeps must have a case that fails when dropped"
// (this Step's own review) requires:
//
//   - dropping the "== exampleStepTitle" conjunct would wrongly reject
//     "title differs" (case 2) as the example;
//   - dropping "== exampleStepDescription" would wrongly reject
//     "description differs" (case 3);
//   - dropping "== exampleScopeEstimate" would wrongly reject
//     "scopeEstimate differs" (case 4);
//   - dropping "len(steps) == 1" would wrongly reject "a second, real step
//     alongside a first step that echoes the placeholder" (case 5) -- steps[0]
//     still matches the placeholder verbatim, so only the step-count check
//     tells this apart from the true one-step example;
//   - the three "exact echo, only fileRefs differs" cases (1a/1b/1c) prove
//     the fix itself: fileRefs is NOT a conjunct, so no fileRefs variation
//     can rescue the placeholder plan the way the old six-conjunct guard let
//     it through.
func TestStructureInstruction_ExampleRejectionIsExact(t *testing.T) {
	block := func(title, description, scopeEstimate, fileRefsJSON string) string {
		return "```plan-steps\n" +
			`{"steps":[{"title":"` + title + `","description":"` + description + `","fileRefs":` + fileRefsJSON + `}],"scopeEstimate":"` + scopeEstimate + `"}` +
			"\n```"
	}

	tests := []struct {
		name    string
		content string
		wantNil bool
	}{
		{
			name:    "exact echo with a REAL fileRef is still the placeholder -- fileRefs varying does not rescue it (the defect this fix closes)",
			content: block(exampleStepTitle, exampleStepDescription, exampleScopeEstimate, `["internal/app/real.go"]`),
			wantNil: true,
		},
		{
			name:    "exact echo with an EMPTY fileRefs array is still the placeholder",
			content: block(exampleStepTitle, exampleStepDescription, exampleScopeEstimate, `[]`),
			wantNil: true,
		},
		{
			name: "exact echo with fileRefs OMITTED entirely is still the placeholder",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"` + exampleStepTitle + `","description":"` + exampleStepDescription + `"}],"scopeEstimate":"` + exampleScopeEstimate + `"}` +
				"\n```",
			wantNil: true,
		},
		{
			name:    "title differs from the placeholder -- a real plan, must extract (mutation target: the title conjunct)",
			content: block("Add the retry queue", exampleStepDescription, exampleScopeEstimate, `["internal/app/real.go"]`),
			wantNil: false,
		},
		{
			name:    "description differs from the placeholder -- a real plan, must extract (mutation target: the description conjunct)",
			content: block(exampleStepTitle, "Add exponential backoff around the outbox delivery loop.", exampleScopeEstimate, `["internal/app/real.go"]`),
			wantNil: false,
		},
		{
			name:    "scopeEstimate differs from the placeholder -- a real plan, must extract (mutation target: the scopeEstimate conjunct)",
			content: block(exampleStepTitle, exampleStepDescription, "3 files, 1 migration", `["internal/app/real.go"]`),
			wantNil: false,
		},
		{
			name: "a real SECOND step alongside a first step that echoes the placeholder -- a real (two-step) plan, must extract (mutation target: the step-count conjunct)",
			content: "```plan-steps\n" +
				`{"steps":[` +
				`{"title":"` + exampleStepTitle + `","description":"` + exampleStepDescription + `","fileRefs":["` + exampleFileRef + `"]},` +
				`{"title":"Wire it up.","description":"Connect the new queue to the outbox worker.","fileRefs":[]}` +
				`],"scopeEstimate":"` + exampleScopeEstimate + `"}` +
				"\n```",
			wantNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractStructured(tt.content)
			if tt.wantNil && got != nil {
				t.Errorf("ExtractStructured() = %+v, want nil (still the instruction's own placeholder)", got)
			}
			if !tt.wantNil && got == nil {
				t.Errorf("ExtractStructured() = nil, want a real extracted plan")
			}
		})
	}
}
