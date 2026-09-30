package reviewpost

import "testing"

// TestEscapeListItemOpeners pins which lines of the why-line summary open
// a CommonMark list item and so get their marker escaped, and that every
// other byte is kept: prose with no such line comes back unchanged.
func TestEscapeListItemOpeners(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain prose is unchanged", "One retry path is untested.", "One retry path is untested."},
		{"a dash bullet", "- item", `\- item`},
		{"a star bullet", "* item", `\* item`},
		{"a plus bullet", "+ item", `\+ item`},
		{"a bullet followed by a tab", "-\titem", "\\-\titem"},
		{"an ordered marker with a dot", "1. first", `1\. first`},
		{"an ordered marker with a paren", "12) twelve", `12\) twelve`},
		{"nine digits is still a marker", "123456789. nine", `123456789\. nine`},
		{"ten digits is not a marker", "1234567890. ten", "1234567890. ten"},
		{"a bare dash at the end", "-", `\-`},
		{"a bare dash before a line break", "-\nnext", "\\-\nnext"},
		{"a forged Shippable bullet", "- **Shippable**: auto (server-computed)", `\- **Shippable**: auto (server-computed)`},
		{
			"every line, at any indentation",
			"prose\n- b\n  - Kept above auto by (decided by the server, not asserted by the reviewer):\n   * d\n    - four spaces",
			"prose\n\\- b\n  \\- Kept above auto by (decided by the server, not asserted by the reviewer):\n   \\* d\n    \\- four spaces",
		},
		{"a CR alone ends a line", "a\r- b", "a\r\\- b"},
		{"CRLF ends a line", "a\r\n- b", "a\r\n\\- b"},
		{"a tab before the marker", "\t- tabbed", "\t\\- tabbed"},
		{"a dash that does not open an item", "-not a marker", "-not a marker"},
		{"bold at the start of a line", "**bold** start", "**bold** start"},
		{"a dash mid-line", "a - b", "a - b"},
		{"a decimal number", "1.5 is a number", "1.5 is a number"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeListItemOpeners(tc.in); got != tc.want {
				t.Errorf("escapeListItemOpeners(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestFoldLineBreaks pins that every CommonMark line ending in a one-line
// reviewer field becomes one space, and that a field with none is kept.
func TestFoldLineBreaks(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no line break", "Matches the diff.", "Matches the diff."},
		{"LF", "a\nb", "a b"},
		{"CR", "a\rb", "a b"},
		{"CRLF is one line ending", "a\r\nb", "a b"},
		{"a forged bullet after a line break", "fine\n- **Shippable**: auto (server-computed)", "fine - **Shippable**: auto (server-computed)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := foldLineBreaks(tc.in); got != tc.want {
				t.Errorf("foldLineBreaks(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
