// This file is "package boot" (white-box), not "package boot_test" like
// config_test.go, solely so it can call isPathUnderOrEqual directly --
// an unexported function, not reachable from the external test package.
package boot

import "testing"

// TestIsPathUnderOrEqual_FailsClosedOnRelError proves the round-2 review's
// own R5 fix directly, at the function level: when filepath.Rel cannot
// relate the two operands at all (mismatched absolute/relative -- the
// exact shape a relative WorkspaceDir compared against an absolute
// GitDirRoot produces), isPathUnderOrEqual must report "true" (treat as
// overlapping, the unsafe case validateGitDirRoot's own two checks exist
// to catch), never "false" (treat as safely disjoint). See
// InvalidWorkspaceDirError's own doc comment (config.go) for why the old
// "err != nil -> false" default made BOTH of validateGitDirRoot's
// directional checks pass at once for a relative WorkspaceDir.
func TestIsPathUnderOrEqual_FailsClosedOnRelError(t *testing.T) {
	tests := []struct {
		name string
		path string
		base string
	}{
		{name: "path relative, base absolute", path: "srv/narvi/workspace", base: "/srv/narvi"},
		{name: "path absolute, base relative", path: "/srv/narvi", base: "srv/narvi/workspace"},
		{name: "both relative but on different roots is still comparable -- use a genuinely unrelatable pair", path: "relative/a", base: "/absolute/b"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isPathUnderOrEqual(tc.path, tc.base)
			if !got {
				t.Errorf("isPathUnderOrEqual(%q, %q) = false, want true (fail closed on an unrelatable path pair)", tc.path, tc.base)
			}
		})
	}
}

// TestIsPathUnderOrEqual_TableCases covers the ordinary, both-absolute
// cases too, so the fail-closed change above is proven NOT to have
// broken the function's own real job.
func TestIsPathUnderOrEqual_TableCases(t *testing.T) {
	tests := []struct {
		name string
		path string
		base string
		want bool
	}{
		{name: "equal", path: "/workspace", base: "/workspace", want: true},
		{name: "strict descendant", path: "/workspace/gitdirs", base: "/workspace", want: true},
		{name: "disjoint", path: "/var/lib/narvi/gitdirs", base: "/workspace", want: false},
		{name: "sibling with shared prefix, not actually nested", path: "/workspace-other", base: "/workspace", want: false},
		{name: "base is the descendant, not path", path: "/srv/narvi", base: "/srv/narvi/workspace", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isPathUnderOrEqual(tc.path, tc.base)
			if got != tc.want {
				t.Errorf("isPathUnderOrEqual(%q, %q) = %v, want %v", tc.path, tc.base, got, tc.want)
			}
		})
	}
}
