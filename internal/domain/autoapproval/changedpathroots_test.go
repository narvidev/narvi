package autoapproval

import (
	"reflect"
	"testing"
)

// TestClassifyChangedRoots_EmptyInput proves a nil/empty input returns
// nil, never a zero-length-but-non-nil slice -- mirrors
// TestClassifyChangedPaths_EmptyInput's own identical assertion for this
// function's sibling.
func TestClassifyChangedRoots_EmptyInput(t *testing.T) {
	if got := ClassifyChangedRoots(nil); got != nil {
		t.Errorf("ClassifyChangedRoots(nil) = %v, want nil", got)
	}
	if got := ClassifyChangedRoots([]string{}); got != nil {
		t.Errorf("ClassifyChangedRoots([]string{}) = %v, want nil", got)
	}
}

func TestClassifyChangedRoots(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
		want  []string
	}{
		{
			name:  "single nested path",
			paths: []string{"internal/domain/review/context.go"},
			want:  []string{"internal"},
		},
		{
			name:  "repo-root file with no slash",
			paths: []string{"README.md"},
			want:  []string{"README.md"},
		},
		{
			name:  "multiple distinct roots",
			paths: []string{"internal/domain/review/context.go", "docs/TECHNICAL_PLAN.md", "migrations/000001_x.up.sql"},
			want:  []string{"docs", "internal", "migrations"},
		},
		{
			name:  "duplicate roots collapse to one",
			paths: []string{"internal/domain/a.go", "internal/app/b.go", "internal/adapters/c.go"},
			want:  []string{"internal"},
		},
		{
			name:  "sorted regardless of input order",
			paths: []string{"z/one.go", "a/two.go", "m/three.go"},
			want:  []string{"a", "m", "z"},
		},
		{
			name:  "empty path entries are skipped",
			paths: []string{"", "internal/x.go", ""},
			want:  []string{"internal"},
		},
		{
			name:  "leading slash produces an empty first segment, skipped",
			paths: []string{"/etc/passwd"},
			want:  nil,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyChangedRoots(tc.paths)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ClassifyChangedRoots(%v) = %v, want %v", tc.paths, got, tc.want)
			}
		})
	}
}

// TestClassifyChangedRoots_Deterministic proves two calls over the SAME
// set of paths, supplied in different orders, produce byte-for-byte
// identical output -- the property the gate's write side (a verdict's
// own INSERT-time stamp) and read side (the current PR's own fresh
// Query.Roots) both depend on to ever produce a comparable overlap.
func TestClassifyChangedRoots_Deterministic(t *testing.T) {
	a := ClassifyChangedRoots([]string{"internal/x.go", "docs/y.md", "migrations/z.sql"})
	b := ClassifyChangedRoots([]string{"migrations/z.sql", "internal/x.go", "docs/y.md"})
	if !reflect.DeepEqual(a, b) {
		t.Errorf("ClassifyChangedRoots order-dependent: %v != %v", a, b)
	}
}

// TestTopLevelChangedRoot pins the unexported helper's own exact
// segment-splitting behavior -- an internal test (same package) so a
// future edit to ClassifyChangedRoots cannot silently change what
// "root" means without this test noticing.
func TestTopLevelChangedRoot(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"internal/domain/review/context.go", "internal"},
		{"README.md", "README.md"},
		{"", ""},
		{"/etc/passwd", ""},
		{"a/b/c", "a"},
	}
	for _, tc := range tests {
		if got := topLevelChangedRoot(tc.path); got != tc.want {
			t.Errorf("topLevelChangedRoot(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
