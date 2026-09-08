package plan

import (
	"reflect"
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
