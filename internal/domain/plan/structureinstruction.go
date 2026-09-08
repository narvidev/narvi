// This file (structureinstruction.go) implements the OTHER half of
// structured.go's own "structure comes from asking the model, not from
// parsing its prose" design: the fixed text that does the asking, and the
// one place it is threaded onto a plan-mode turn's own prompt.
//
// Mirrors internal/domain/turn.MaybeInjectEpistemicPreamble's own
// resolve/exclude/render/prepend shape (that package's own doc comment on
// why a single small pure function, not an inline `if planMode` at each
// call site, is what keeps a future call site from forgetting this) --
// with the OPPOSITE condition: the epistemic preamble is injected on an
// ordinary build turn and structurally EXCLUDED for plan mode (§20.3);
// this instruction exists FOR plan mode specifically and is a no-op for
// every other turn. The two can never both apply to the same turn, so
// their relative order at a shared call site does not matter.

package plan

// structureInstruction is prepended verbatim onto a plan-mode turn's own
// prompt -- trusted, first-party instructional text (like the epistemic
// preamble, and unlike a review turn's diff/stack blocks), so it is never
// delimiter-wrapped as untrusted content. The exact fence tag
// (StructureFenceOpen, structured.go) and required JSON keys are spelled
// out here so ExtractStructured's own strict decode has something
// realistic to succeed against; the trailing paragraph tells the model to
// omit the block entirely rather than guess, which is what lets a
// genuinely under-specified plan still render honestly as prose (never a
// fabricated one-step "plan" invented just to satisfy the shape).
const structureInstruction = "" +
	"This is a plan-mode turn. Propose your plan as you normally would, in prose, for a human to read " +
	"first -- that text is what gets shown and decided on. Then, as the LAST thing in your reply, " +
	"include exactly one fenced code block labeled `plan-steps` containing a single JSON object with " +
	"this exact shape, so this system can also render your plan as a structured document:\n\n" +
	StructureFenceOpen + "\n" +
	"{\"steps\": [{\"title\": \"short step title\", \"description\": \"what this step does and why\", " +
	"\"fileRefs\": [\"path/to/file.go\"]}], \"scopeEstimate\": \"e.g. 6 files, 2 migrations\"}\n" +
	"```\n\n" +
	"Requirements: at least one step; every step needs a non-empty title and description; fileRefs is a " +
	"(possibly empty) array of real repository paths the step touches; scopeEstimate is a short, " +
	"non-empty summary of the plan's overall size. Emit exactly ONE such block, never more than one, and " +
	"never in place of your normal prose explanation above it. If you cannot confidently break the plan " +
	"into concrete steps, omit the block entirely rather than guessing at one -- your prose plan is still " +
	"complete and useful on its own.\n\n"

// RenderStructureInstruction returns structureInstruction -- a function
// (rather than a bare exported const) purely to mirror
// turn.RenderEpistemicPreamble's own naming for a rendered prompt fragment.
func RenderStructureInstruction() string {
	return structureInstruction
}

// MaybeInjectStructureInstruction prepends RenderStructureInstruction's own
// fixed text onto prompt whenever planMode is true, unconditionally --
// unlike the epistemic preamble, there is no platform/session-level
// override for this: every plan-mode turn is asked for structure the same
// way, exactly as every plan-mode turn is unconditionally routed to
// OpenCode's own native "plan" agent (internal/adapters/outbound/opencode/
// types.go's own promptAsyncRequest doc comment) regardless of any config.
func MaybeInjectStructureInstruction(planMode bool, prompt string) string {
	if !planMode {
		return prompt
	}
	return RenderStructureInstruction() + prompt
}
