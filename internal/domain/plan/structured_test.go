package plan

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestExtractStructured(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    *Structured
	}{
		{
			name:    "no fence at all -- the ordinary case for every plan predating this schema",
			content: "1. Add a table.\n2. Wire it up.",
			want:    nil,
		},
		{
			// dec.More() reported false for these, because it answers "is
			// there another element in the CURRENT array/object" rather than
			// "is there anything after the value". Both were accepted.
			name: "a trailing closing brace is rejected",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"T","description":"D","fileRefs":["a.go"]}],"scopeEstimate":"1 file"}}` +
				"\n```",
			want: nil,
		},
		{
			name: "a trailing closing bracket is rejected",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"T","description":"D","fileRefs":["a.go"]}],"scopeEstimate":"1 file"}]` +
				"\n```",
			want: nil,
		},
		{
			name: "an empty fileRef is rejected -- an empty path renders an empty chip pointing at nothing",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"T","description":"D","fileRefs":[""]}],"scopeEstimate":"1 file"}` +
				"\n```",
			want: nil,
		},
		{
			name: "a whitespace-only fileRef is rejected too",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"T","description":"D","fileRefs":["   "]}],"scopeEstimate":"1 file"}` +
				"\n```",
			want: nil,
		},
		{
			// A NUL is valid JSON and a valid Go string, but Postgres jsonb
			// raises 22P05 on it -- and the structured value is persisted in
			// the SAME transaction that approves the plan, so letting one
			// through makes that plan unapprovable forever, on every channel.
			name: "a NUL escape in a title folds to prose rather than bricking approval",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"Add\u0000table","description":"New migration.","fileRefs":["a.sql"]}],"scopeEstimate":"1 file"}` +
				"\n```",
			want: nil,
		},
		{
			name: "a NUL escape in a description folds to prose",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"Add table","description":"New\u0000migration.","fileRefs":["a.sql"]}],"scopeEstimate":"1 file"}` +
				"\n```",
			want: nil,
		},
		{
			name: "a NUL escape in a fileRef folds to prose",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"Add table","description":"New migration.","fileRefs":["a\u0000.sql"]}],"scopeEstimate":"1 file"}` +
				"\n```",
			want: nil,
		},
		{
			name: "a NUL escape in the scope estimate folds to prose",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"Add table","description":"New migration.","fileRefs":["a.sql"]}],"scopeEstimate":"1\u0000file"}` +
				"\n```",
			want: nil,
		},
		{
			// The control that keeps the guard honest: U+0001..U+001F share the
			// escape SHAPE but jsonb stores them fine, so rejecting them too
			// would refuse a plan Postgres would have accepted.
			name: "a non-NUL control escape is NOT rejected -- jsonb accepts those",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"Add\u0001table","description":"New migration.","fileRefs":["a.sql"]}],"scopeEstimate":"1 file"}` +
				"\n```",
			want: &Structured{
				Steps:         []Step{{Title: "Add\u0001table", Description: "New migration.", FileRefs: []string{"a.sql"}}},
				ScopeEstimate: "1 file",
			},
		},
		{
			name: "well-formed block embedded in surrounding prose, before and after",
			content: "Here is my plan.\n\n1. Add a table.\n2. Wire it up.\n\n" +
				"```plan-steps\n" +
				`{"steps":[{"title":"Add table","description":"New migration.","fileRefs":["migrations/000200.up.sql"]}],"scopeEstimate":"1 file"}` +
				"\n```\n\nLet me know if this looks right.",
			want: &Structured{
				Steps:         []Step{{Title: "Add table", Description: "New migration.", FileRefs: []string{"migrations/000200.up.sql"}}},
				ScopeEstimate: "1 file",
			},
		},
		{
			name: "block at the very end of the content, no trailing prose",
			content: "Plan:\n1. Add a table.\n\n```plan-steps\n" +
				`{"steps":[{"title":"Add table","description":"New migration.","fileRefs":[]}],"scopeEstimate":"1 file"}` +
				"\n```",
			want: &Structured{
				Steps:         []Step{{Title: "Add table", Description: "New migration.", FileRefs: []string{}}},
				ScopeEstimate: "1 file",
			},
		},
		{
			name: "multiple steps preserve order",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"First","description":"Do first.","fileRefs":[]},{"title":"Second","description":"Do second.","fileRefs":["a.go","b.go"]}],"scopeEstimate":"2 files"}` +
				"\n```",
			want: &Structured{
				Steps: []Step{
					{Title: "First", Description: "Do first.", FileRefs: []string{}},
					{Title: "Second", Description: "Do second.", FileRefs: []string{"a.go", "b.go"}},
				},
				ScopeEstimate: "2 files",
			},
		},
		{
			name: "fileRefs omitted entirely normalizes to an empty, non-nil slice",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"First","description":"Do first."}],"scopeEstimate":"1 file"}` +
				"\n```",
			want: &Structured{
				Steps:         []Step{{Title: "First", Description: "Do first.", FileRefs: []string{}}},
				ScopeEstimate: "1 file",
			},
		},
		{
			name: "surrounding whitespace on title/description/scopeEstimate is trimmed",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"  First  ","description":"  Do first.  ","fileRefs":[]}],"scopeEstimate":"  1 file  "}` +
				"\n```",
			want: &Structured{
				Steps:         []Step{{Title: "First", Description: "Do first.", FileRefs: []string{}}},
				ScopeEstimate: "1 file",
			},
		},
		{
			name: "open fence with no matching close fence -- truncated stream, never guess",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"First","description":"Do first.","fileRefs":[]}],"scopeEstimate":"1 file"}`,
			want: nil,
		},
		{
			name: "a second open fence anywhere after the first is ambiguous -- refuse both",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"A","description":"a","fileRefs":[]}],"scopeEstimate":"1 file"}` +
				"\n```\n\nOn second thought:\n\n```plan-steps\n" +
				`{"steps":[{"title":"B","description":"b","fileRefs":[]}],"scopeEstimate":"1 file"}` +
				"\n```",
			want: nil,
		},
		{
			name:    "malformed JSON inside the fence",
			content: "```plan-steps\n{not json at all\n```",
			want:    nil,
		},
		{
			name: "an unknown top-level field is rejected -- DisallowUnknownFields, never followed loosely",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"A","description":"a","fileRefs":[]}],"scopeEstimate":"1 file","confidence":"high"}` +
				"\n```",
			want: nil,
		},
		{
			name: "trailing content after the JSON value inside the fence is rejected",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"A","description":"a","fileRefs":[]}],"scopeEstimate":"1 file"} also this` +
				"\n```",
			want: nil,
		},
		{
			name:    "zero steps is folded into the SAME failure case as no block at all -- never a distinguishable real zero",
			content: "```plan-steps\n" + `{"steps":[],"scopeEstimate":"1 file"}` + "\n```",
			want:    nil,
		},
		{
			name: "a step with an empty title invalidates the WHOLE document, not just that step",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"A","description":"a","fileRefs":[]},{"title":"","description":"b","fileRefs":[]}],"scopeEstimate":"2 files"}` +
				"\n```",
			want: nil,
		},
		{
			name: "a step with a whitespace-only description invalidates the whole document",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"A","description":"   ","fileRefs":[]}],"scopeEstimate":"1 file"}` +
				"\n```",
			want: nil,
		},
		{
			name: "an empty scopeEstimate invalidates the whole document",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"A","description":"a","fileRefs":[]}],"scopeEstimate":""}` +
				"\n```",
			want: nil,
		},
		{
			name:    "empty content",
			content: "",
			want:    nil,
		},
		{
			// JSON does not require backticks to be escaped inside a string,
			// so a model describing something like wrapping output in code
			// fences can legitimately put a "```" sequence INSIDE a title or
			// description, strictly before the block's own real close. A
			// naive strings.Index(afterOpen, "```") stops at THAT occurrence
			// (mid-line, inside the still-open JSON string) rather than the
			// real close two lines later -- truncating raw mid-string, which
			// is invalid JSON, so this self-corrects to nil today. The fix
			// (closingFenceIndex requiring the close to start a line) makes
			// it extract correctly instead of merely failing safe.
			name: "an embedded ``` sequence inside a field's own value does not end the block early",
			content: "```plan-steps\n" +
				"{\"steps\":[{\"title\":\"Wrap it in ```code``` blocks\",\"description\":\"D\",\"fileRefs\":[]}],\"scopeEstimate\":\"1 file\"}" +
				"\n```",
			want: &Structured{
				Steps:         []Step{{Title: "Wrap it in ```code``` blocks", Description: "D", FileRefs: []string{}}},
				ScopeEstimate: "1 file",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractStructured(tt.content)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ExtractStructured() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestStripStructureBlock(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "content with no block is returned unchanged",
			content: "1. Add a table.\n2. Wire it up.",
			want:    "1. Add a table.\n2. Wire it up.",
		},
		{
			name: "a trailing block is removed, and so is the blank seam it left",
			content: "Here is my plan.\n\n1. Add a table.\n\n```plan-steps\n" +
				`{"steps":[{"title":"Add table","description":"New migration.","fileRefs":[]}],"scopeEstimate":"1 file"}` +
				"\n```\n",
			want: "Here is my plan.\n\n1. Add a table.",
		},
		{
			name: "prose after the block survives",
			content: "Plan.\n\n```plan-steps\n" +
				`{"steps":[{"title":"T","description":"D","fileRefs":[]}],"scopeEstimate":"1 file"}` +
				"\n```\n\nLet me know.",
			want: "Plan.\n\nLet me know.",
		},
		{
			// The same ambiguity ExtractStructured refuses: with two open
			// fences neither can tell which block was meant, so the human
			// sees exactly what the model wrote rather than a guess.
			name:    "two open fences: nothing is removed",
			content: "A\n```plan-steps\n{}\n```\nB\n```plan-steps\n{}\n```\n",
			want:    "A\n```plan-steps\n{}\n```\nB\n```plan-steps\n{}\n```\n",
		},
		{
			name:    "an unterminated fence removes nothing",
			content: "Plan.\n\n```plan-steps\n{\"steps\":[]",
			want:    "Plan.\n\n```plan-steps\n{\"steps\":[]",
		},
		{
			// The core reproduction for finding 1: a naive
			// strings.Index(afterOpen, "```") stops at the mid-line "```"
			// inside the title's own value, splicing the back half of the
			// fenced JSON ("code```blocks\",\"description\":...}\n```\n\nDone.")
			// straight into the prose a human reads. The fix requires the
			// close to start a line, so the whole block -- ALL of it, past
			// every embedded backtick sequence -- is removed, and nothing
			// but the real prose on either side survives.
			name: "an embedded ``` sequence inside the fenced JSON does not end the block early -- the whole block is removed, none of it leaks into the prose",
			content: "Plan.\n\n```plan-steps\n" +
				"{\"steps\":[{\"title\":\"Wrap it in ```code``` blocks\",\"description\":\"D\",\"fileRefs\":[]}],\"scopeEstimate\":\"1 file\"}" +
				"\n```\n\nDone.",
			want: "Plan.\n\nDone.",
		},
		{
			// Finding 2: a reply that is ONLY the block (the model skipped
			// the "propose your plan in prose first" half of the
			// instruction) must not strip down to the empty string -- an
			// empty result is never more honest than the raw block it came
			// from, so the whole content comes back unchanged.
			name: "a reply that is ONLY the block is returned UNCHANGED, never stripped to empty",
			content: "```plan-steps\n" +
				`{"steps":[{"title":"T","description":"D","fileRefs":[]}],"scopeEstimate":"1 file"}` +
				"\n```",
			want: "```plan-steps\n" +
				`{"steps":[{"title":"T","description":"D","fileRefs":[]}],"scopeEstimate":"1 file"}` +
				"\n```",
		},
		{
			// Same case, with only whitespace padding the block on both
			// sides -- still nothing for a human to read once trimmed, so
			// still falls back to the unchanged original (whitespace and
			// all), not an empty string.
			name: "a reply that is the block plus only surrounding whitespace is also returned unchanged",
			content: "   \n```plan-steps\n" +
				`{"steps":[{"title":"T","description":"D","fileRefs":[]}],"scopeEstimate":"1 file"}` +
				"\n```\n   ",
			want: "   \n```plan-steps\n" +
				`{"steps":[{"title":"T","description":"D","fileRefs":[]}],"scopeEstimate":"1 file"}` +
				"\n```\n   ",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripStructureBlock(tc.content); got != tc.want {
				t.Errorf("StripStructureBlock() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStripStructureBlock_NeverStripsWhatExtractStructuredWouldRead is the
// pair invariant: whenever a block IS extractable, stripping must actually
// remove it -- otherwise a human reads the JSON on exactly the plans that
// worked, which is the leak this function exists to close.
func TestStripStructureBlock_NeverStripsWhatExtractStructuredWouldRead(t *testing.T) {
	content := "Prose first.\n\n```plan-steps\n" +
		`{"steps":[{"title":"T","description":"D","fileRefs":["a.go"]}],"scopeEstimate":"1 file"}` +
		"\n```\n"
	if ExtractStructured(content) == nil {
		t.Fatal("fixture bug: this content must extract, or the invariant below is vacuous")
	}
	if got := StripStructureBlock(content); strings.Contains(got, "plan-steps") || strings.Contains(got, "scopeEstimate") {
		t.Errorf("StripStructureBlock() = %q, want the machine block gone", got)
	}
}

// TestExtractStructured_StepCountBound pins both sides of MaxSteps: at the
// bound a plan still extracts, past it the document folds to prose -- which
// is what keeps the web's own per-string 8000-character cap meaningful,
// since the prose path renders one string rather than three per step.
func TestExtractStructured_StepCountBound(t *testing.T) {
	build := func(n int) string {
		var b strings.Builder
		b.WriteString("```plan-steps\n{\"steps\":[")
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"title":"T","description":"D","fileRefs":["a.go"]}`)
		}
		b.WriteString(`],"scopeEstimate":"s"}` + "\n```")
		return b.String()
	}

	if got := ExtractStructured(build(MaxSteps)); got == nil || len(got.Steps) != MaxSteps {
		t.Errorf("exactly MaxSteps (%d) steps must extract, got %v", MaxSteps, got)
	}
	if got := ExtractStructured(build(MaxSteps + 1)); got != nil {
		t.Errorf("MaxSteps+1 (%d) steps extracted %d steps, want nil -- past the bound a plan renders as prose, where the render cap does hold", MaxSteps+1, len(got.Steps))
	}
}

// TestExtractStructured_FileRefsCountBound pins both sides of
// MaxFileRefsPerStep -- MaxSteps' own missing companion (finding 5): a
// document at the step-count ceiling with an unbounded fileRefs array per
// step would still render an unbounded number of file chips for that ONE
// step, so the step-count cap alone bounds nothing about a single step's
// own render size.
func TestExtractStructured_FileRefsCountBound(t *testing.T) {
	build := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"title":"T","description":"D","fileRefs":[`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`"a.go"`)
		}
		b.WriteString("]}")
		return "```plan-steps\n" + `{"steps":[` + b.String() + `],"scopeEstimate":"s"}` + "\n```"
	}

	if got := ExtractStructured(build(MaxFileRefsPerStep)); got == nil || len(got.Steps[0].FileRefs) != MaxFileRefsPerStep {
		t.Errorf("exactly MaxFileRefsPerStep (%d) fileRefs must extract, got %v", MaxFileRefsPerStep, got)
	}
	if got := ExtractStructured(build(MaxFileRefsPerStep + 1)); got != nil {
		t.Errorf("MaxFileRefsPerStep+1 (%d) fileRefs extracted %d, want nil -- past the bound the step folds to prose", MaxFileRefsPerStep+1, len(got.Steps[0].FileRefs))
	}
}

// TestExtractStructured_FieldCharBound pins both sides of MaxFieldChars for
// every model-authored string field a structured document carries: a
// step's title and description, one of its fileRefs entries, and the
// document's own scopeEstimate. Without this, MaxSteps and
// MaxFileRefsPerStep together still bound only COUNTS (how many steps, how
// many fileRefs) -- a single field of unbounded length would still make
// the aggregate render unbounded, which is exactly the property this
// Step's own MaxSteps doc comment claims and, before this bound existed,
// did not actually have.
func TestExtractStructured_FieldCharBound(t *testing.T) {
	tests := []struct {
		name  string
		build func(n int) string
	}{
		{
			name: "title",
			build: func(n int) string {
				return "```plan-steps\n" + fmt.Sprintf(`{"steps":[{"title":%q,"description":"D","fileRefs":[]}],"scopeEstimate":"s"}`, strings.Repeat("a", n)) + "\n```"
			},
		},
		{
			name: "description",
			build: func(n int) string {
				return "```plan-steps\n" + fmt.Sprintf(`{"steps":[{"title":"T","description":%q,"fileRefs":[]}],"scopeEstimate":"s"}`, strings.Repeat("a", n)) + "\n```"
			},
		},
		{
			name: "a fileRefs entry",
			build: func(n int) string {
				return "```plan-steps\n" + fmt.Sprintf(`{"steps":[{"title":"T","description":"D","fileRefs":[%q]}],"scopeEstimate":"s"}`, strings.Repeat("a", n)) + "\n```"
			},
		},
		{
			name: "scopeEstimate",
			build: func(n int) string {
				return "```plan-steps\n" + fmt.Sprintf(`{"steps":[{"title":"T","description":"D","fileRefs":[]}],"scopeEstimate":%q}`, strings.Repeat("a", n)) + "\n```"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractStructured(tt.build(MaxFieldChars)); got == nil {
				t.Errorf("a %s of exactly MaxFieldChars (%d) must extract, got nil", tt.name, MaxFieldChars)
			}
			if got := ExtractStructured(tt.build(MaxFieldChars + 1)); got != nil {
				t.Errorf("a %s of MaxFieldChars+1 (%d) extracted, want nil -- past the bound the document folds to prose", tt.name, MaxFieldChars+1)
			}
		})
	}
}
