package reviewtriage

import "testing"

// TestDefaultSizeExclusions_Matches pins what the built-in size patterns
// leave out of the count, and -- as much the point -- what they keep.
func TestDefaultSizeExclusions_Matches(t *testing.T) {
	exclusion := newSizeExclusion(DefaultSizeExclusions())
	tests := []struct {
		path string
		want bool
	}{
		// Tests.
		{"internal/app/foo/bar_test.go", true},
		{"tests/test_login.py", true},
		{"pkg/login_test.py", true},
		{"spec/models/user_spec.rb", true},
		{"web/src/session/view.test.tsx", true},
		{"web/src/api/client.spec.ts", true},
		{"src/main/java/app/UserServiceTest.java", true},
		{"test/e2e/run.sh", true},
		{"internal/app/foo/testdata/golden.json", true},
		{"web/src/__tests__/app.tsx", true},
		{"web/src/__snapshots__/app.snap", true},
		// Documentation.
		{"README.md", true},
		{"docs/TECHNICAL_PLAN.md", true},
		{"docs/design/mockups.html", true},
		{"web/doc/overview.rst", true},
		// Generated.
		{"contracts/gen/proto/api.pb.go", true},
		{"api/service_pb2.py", true},
		{"internal/x/zz_generated.deepcopy.go", true},
		{"web/dist/app.min.js", true},
		{"go.sum", true},
		{"web/package-lock.json", true},
		{"Cargo.lock", true},
		// Source: counted.
		{"internal/app/foo/bar.go", false},
		{"cmd/control-plane/main.go", false},
		{"internal/contest/entry.go", false},
		{"scripts/test", false},
		{"internal/latest/value.go", false},
		{"internal/doctor/check.go", false},
		{"go.mod", false},
		{"web/package.json", false},
		{"migrations/000001_init.up.sql", false},
		{"Dockerfile", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := exclusion.matches(tt.path); got != tt.want {
				t.Errorf("matches(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestSourceChangedLines(t *testing.T) {
	patterns := []string{"*_test.go", "**/docs/**"}
	tests := []struct {
		name     string
		changed  int
		files    []FileLines
		patterns []string
		want     int
	}{
		{"nothing excluded", 50, []FileLines{{Path: "a.go", Added: 30, Deleted: 20}}, patterns, 50},
		{"a test file's lines are subtracted", 650, []FileLines{{Path: "a.go", Added: 50}, {Path: "a_test.go", Added: 500, Deleted: 100}}, patterns, 50},
		{"no patterns subtracts nothing", 650, []FileLines{{Path: "a_test.go", Added: 600}}, nil, 650},
		{"a line the parse missed stays counted", 700, []FileLines{{Path: "a_test.go", Added: 600}}, patterns, 100},
		{"never negative when the reported total lags the diff", 10, []FileLines{{Path: "a_test.go", Added: 600}}, patterns, 0},
		{"a rename is excluded only when both paths match", 80, []FileLines{{Path: "x_test.go", OldPath: "x.go", Added: 80}}, patterns, 80},
		{"a rename between two excluded paths is excluded", 80, []FileLines{{Path: "docs/b.md", OldPath: "docs/a.md", Added: 80}}, patterns, 0},
		{"an unnamed section is never excluded", 30, []FileLines{{Path: "", Added: 30}}, []string{"**"}, 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sourceChangedLines(tt.changed, tt.files, tt.patterns); got != tt.want {
				t.Errorf("sourceChangedLines() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDefaultSizeExclusions_ReturnsAFreshSlice(t *testing.T) {
	a := DefaultSizeExclusions()
	a[0] = "mutated"
	if DefaultSizeExclusions()[0] == "mutated" {
		t.Fatal("DefaultSizeExclusions() returned a shared slice a caller can mutate")
	}
}
