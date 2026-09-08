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
