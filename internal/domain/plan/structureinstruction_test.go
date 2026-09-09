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

// TestStructureInstruction_ExampleRejectionIsExact keeps the guard above
// from quietly widening into a placeholder-shaped heuristic that would turn
// away real plans: a document that differs from the example in ONE field is
// a plan, and must extract.
func TestStructureInstruction_ExampleRejectionIsExact(t *testing.T) {
	content := "```plan-steps\n" +
		`{"steps":[{"title":"short step title","description":"what this step does and why","fileRefs":["internal/app/real.go"]}],"scopeEstimate":"e.g. 6 files, 2 migrations"}` +
		"\n```"
	got := ExtractStructured(content)
	if got == nil {
		t.Fatal("a document differing from the example only in fileRefs was rejected -- the guard must be exact, not a placeholder heuristic")
	}
	if len(got.Steps) != 1 || got.Steps[0].FileRefs[0] != "internal/app/real.go" {
		t.Errorf("extracted = %+v, want the real fileRef preserved", got.Steps)
	}
}
