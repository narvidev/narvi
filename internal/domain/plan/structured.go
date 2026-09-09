// This file (structured.go) implements §12.2 item 3's own missing piece:
// "persistent versioned plan document (numbered steps with file refs,
// scope estimate, v1->v2 history)" names a SHAPE this codebase never had
// -- content.go's own top doc comment and planapprovalcontent.go's
// (internal/app/sessionactor) both say so explicitly: "There is no
// structured plan schema anywhere in this codebase ... rendered from a
// plan turn's freeform assistant text, never parsed into a structured
// shape." This file is that schema, plus the one function that recovers
// it.
//
// # Where the structure comes from
//
// The model IS asked for structured output, and the result IS validated --
// never the other way around (inferring structure from whatever shape a
// plan's freeform prose happens to have, e.g. sniffing for numbered-list
// markdown and guessing where one bullet ends and its file references
// begin). Concretely: structureinstruction.go's RenderStructureInstruction
// asks a plan-mode turn's own model to end its reply with exactly one
// ```plan-steps fenced block containing JSON matching this file's own
// schema; ExtractStructured below recognizes ONLY that exact block, decodes
// it with DisallowUnknownFields, and validates every field -- any deviation
// at all (no block, more than one block, invalid JSON, an unknown key, a
// missing/empty required field) is treated identically to "the model never
// attempted structure", never salvaged into a partial result.
//
// This deliberately reuses the SAME channel content.go's ExtractContent
// already reads (the turn's own streamed assistant text, recovered from the
// event log) rather than inventing a new sandbox-to-CP HTTP endpoint the
// way §8.2's server-side verdict-posting tool does (reviewverdict.go's own
// top doc comment) -- that endpoint exists because a review verdict's
// Shippable field must never be trusted from the model and needs
// server-side recomputation against data the model cannot see (the diff,
// CI state); a plan document has no such server-computed counterpart to
// recompute against; every byte of it is, and always was, the model's own
// prose. Adding a second authenticated mid-turn write path solely to
// shuttle the SAME turn's own output through a different pipe would be
// exactly the kind of "novel scope the existing wire contracts do not
// anticipate" reviewverdict.go's own doc comment already declined for a
// case with a materially stronger justification than this one has. Asking
// for a strict, machine-parseable block INSIDE the existing text stream,
// then decoding it with no tolerance for deviation, is "structured output,
// validated" in substance -- it just rides the channel that already
// exists.
//
// # What a plan with no structure renders as
//
// Structured is a pointer for exactly one reason: nil is the ONLY spelling
// of "no structure available", and it covers three distinct real causes
// identically on purpose -- (1) every plan that predates this file's own
// existence, whose prose was never asked to carry a ```plan-steps block at
// all; (2) a plan-mode turn that ran after this Step shipped but whose
// model never emitted the block, or emitted one that failed validation;
// and (3) ExtractContent's own pre-existing ContentFallbackText case (no
// token event could be recovered at all -- there is no prose to look
// inside of). A caller that gets nil renders the plan's own Content
// verbatim, in prose, EXACTLY as every plan has always rendered --
// producing a structured value is additive, never a replacement content
// stops being computed for. This is the same "a real value and a
// never-computed one must never render identically" discipline §21.1's
// own "not yet computed" sentinel encodes, applied in the other direction:
// there, a real zero had to be kept distinguishable from no-data-yet; here,
// the two failure causes above are DELIBERATELY the SAME rendering (prose)
// specifically because there is no honest partial state to show for either
// one -- a plan whose steps could not be recovered is not a plan with zero
// steps, it is a plan whose ONLY faithful representation left is the prose
// it always was.
//
// A genuinely empty steps array is folded into this same nil/prose case,
// not kept as a distinguishable "real zero" the way a metric's real 0%
// would be (§21.1's own worked example) -- a structured plan document
// proposes something by definition (§12.2 item 3: "numbered steps"), so an
// explicit empty list is far more likely to be a model that emitted the
// wrapper without content than a deliberate "this plan needs zero steps"
// statement, and either way a human deciding whether to approve gets
// nothing useful from an empty numbered list that the SAME plan's own
// prose would not already tell them better.

package plan

import (
	"encoding/json"
	"strings"
)

// StructureFenceOpen is the exact opening delimiter ExtractStructured looks
// for -- also the literal token RenderStructureInstruction
// (structureinstruction.go) asks the model to emit, so the two can never
// drift to different tags. structureFenceClose is deliberately just a bare
// markdown code-fence close (any fenced block ends this way); requiring a
// SPECIFIC close tag would only need a wider set of models to get the
// close annotation right too, buying no additional safety.
const (
	StructureFenceOpen  = "```plan-steps"
	structureFenceClose = "```"
)

// Step is one numbered step of a structured plan document -- §12.2 item
// 3's own "numbered steps with file refs, scope estimate" -- always fully
// populated (Title/Description non-empty) by the time ExtractStructured
// returns one: see that function's own doc comment for why a step that
// fails either check invalidates the WHOLE document rather than being
// dropped or defaulted.
type Step struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	// FileRefs is never nil in a value ExtractStructured returns (an
	// omitted or null "fileRefs" key normalizes to an empty, non-nil
	// slice) -- callers that render "N files" or iterate this slice never
	// need a nil check of their own.
	FileRefs []string `json:"fileRefs"`
}

// Structured is a plan version's own successfully recovered structured
// document. See this file's own top doc comment for why its ABSENCE (a nil
// *Structured, never this type's own zero value) is the only representation
// of "no structure" -- there is no zero-step, empty-ScopeEstimate
// Structured value ExtractStructured can ever produce.
type Structured struct {
	// Steps always has at least one element in a value ExtractStructured
	// returns.
	Steps []Step `json:"steps"`
	// ScopeEstimate is a short, non-empty, model-authored free-text
	// summary of the plan's overall size (the mockup's own "6 files · 2
	// migrations" line, docs/design/mockups.html) -- deliberately a single
	// string, not a further-structured {filesChanged, migrations, ...}
	// shape: §12.2 item 3 asks for "a scope estimate", not a taxonomy of
	// scope-estimate kinds, and inventing fields the model would have to
	// populate consistently buys structure this Step was not asked to
	// design.
	ScopeEstimate string `json:"scopeEstimate"`
}

// wireStep/wireStructured are ExtractStructured's own strict JSON decode
// targets -- deliberately distinct Go types from Step/Structured above
// (never decoded directly into the public types) so a caller can never
// observe a partially-decoded, not-yet-validated value: the only path from
// wire shape to public shape is through this function's own validation,
// below.
type wireStep struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	FileRefs    []string `json:"fileRefs"`
}

type wireStructured struct {
	Steps         []wireStep `json:"steps"`
	ScopeEstimate string     `json:"scopeEstimate"`
}

// ExtractStructured looks for exactly one StructureFenceOpen ... ``` fenced
// block inside content (a plan version's own already-recovered prose --
// ExtractContent's own output, never raw event payloads: this function
// takes plain text and does no I/O of its own, per §11) and, if one is
// found, decodes and validates it into a *Structured. Returns nil --
// never a partial value -- when ANY of the following holds; every one
// collapses to the SAME nil result, deliberately (this file's own top doc
// comment):
//
//   - no open fence at all;
//   - a SECOND open fence exists anywhere after the first (ambiguous --
//     which block is authoritative is not this function's call to make,
//     so neither is used);
//   - an open fence with no matching close fence (a truncated/cut-off
//     stream -- guessing at an unterminated block's intended content is
//     exactly the "silently degrades" failure mode this function exists
//     to avoid, mirroring ExtractContent's own "never fails, never
//     guesses" contract one level up);
//   - the fenced text is not valid JSON, decodes with a field this schema
//     does not define (json.Decoder.DisallowUnknownFields -- a model
//     inventing its own extra keys is treated as "did not follow the
//     schema", never "followed it loosely"), or carries trailing content
//     after the one JSON value;
//   - zero steps, or any step whose title or description is empty after
//     trimming whitespace;
//   - an empty (after trimming) scopeEstimate.
//
// Never fails the caller with an error: an extraction attempt that finds
// nothing usable is exactly as valid an outcome as a plan that never had a
// plan-mode turn at all, and every caller already has a well-defined
// fallback (render Content in prose) for it.
func ExtractStructured(content string) *Structured {
	openIdx := strings.Index(content, StructureFenceOpen)
	if openIdx == -1 {
		return nil
	}

	afterOpen := content[openIdx+len(StructureFenceOpen):]
	if strings.Contains(afterOpen, StructureFenceOpen) {
		// A second open fence exists somewhere after the first -- refuse
		// both rather than guess which one the model meant as authoritative.
		return nil
	}

	closeIdx := strings.Index(afterOpen, structureFenceClose)
	if closeIdx == -1 {
		return nil
	}

	raw := strings.TrimSpace(afterOpen[:closeIdx])

	var wire wireStructured
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return nil
	}
	if dec.More() {
		// Trailing content after the one JSON value the fence is supposed
		// to contain -- reject rather than silently ignore it.
		return nil
	}

	if len(wire.Steps) == 0 {
		return nil
	}
	scopeEstimate := strings.TrimSpace(wire.ScopeEstimate)
	if scopeEstimate == "" || containsNUL(scopeEstimate) {
		return nil
	}

	steps := make([]Step, len(wire.Steps))
	for i, s := range wire.Steps {
		title := strings.TrimSpace(s.Title)
		description := strings.TrimSpace(s.Description)
		if title == "" || description == "" {
			return nil
		}
		if containsNUL(title) || containsNUL(description) {
			return nil
		}
		fileRefs := s.FileRefs
		if fileRefs == nil {
			fileRefs = []string{}
		}
		for _, ref := range fileRefs {
			if containsNUL(ref) {
				return nil
			}
		}
		steps[i] = Step{Title: title, Description: description, FileRefs: fileRefs}
	}

	return &Structured{Steps: steps, ScopeEstimate: scopeEstimate}
}

// containsNUL reports whether s carries a U+0000, which is valid in a Go
// string and valid in JSON (as the escape \u0000) but which Postgres cannot
// store in a jsonb column: it raises 22P05, "unsupported Unicode escape
// sequence".
//
// That refusal is why this is a rejection and not a cosmetic check. The
// structured value is persisted to plan_documents.structured_steps inside
// the SAME transaction that approves the plan, so one NUL anywhere in a
// model-emitted title, description, fileRef or scope estimate rolls that
// transaction back -- and because the content is re-derived deterministically
// from the immutable event log on every attempt, the plan is not merely
// failing once, it is unapprovable forever, on the web button, the Slack
// button and the Linear reply alike. The prose in that same content reaches
// a TEXT column and is harmless there, which is exactly why this was not a
// problem before the structured column existed.
//
// U+0000 is the only character with this effect: U+0001 through U+001F
// marshal to the same escape shape and jsonb accepts them, so this checks
// for the one byte that actually bricks the write rather than sweeping a
// whole control-character range it has no reason to refuse.
//
// Folding to nil rather than stripping the byte keeps the one rule this
// extractor has: every deviation renders as the prose it always was. Editing
// model-authored text on its way through would be a silent rewrite of the
// document a human is about to approve.
func containsNUL(s string) bool { return strings.ContainsRune(s, 0) }
