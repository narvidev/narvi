package reviewtriage

import "testing"

// TestDefaultSizeExclusions_Matches pins what the built-in size patterns
// leave out of the count, and -- as much the point -- the hand-written
// production code, contracts and dependency records they must keep
// counting, whatever directory or name they sit under.
func TestDefaultSizeExclusions_Matches(t *testing.T) {
	exclusion := newSizeExclusion(DefaultSizeExclusions())
	tests := []struct {
		path string
		want bool
	}{
		// Tests, by this repository's own language conventions.
		{"internal/app/foo/bar_test.go", true},
		{"web/src/session/view.test.tsx", true},
		{"web/src/api/__tests__/http.test.ts", true},
		// Documentation, by extension.
		{"README.md", true},
		{"docs/TECHNICAL_PLAN.md", true},
		{"web/doc/overview.rst", true},
		{"guide/index.adoc", true},
		// Generated, by names that say so.
		{"contracts/gen/proto/api.pb.go", true},
		{"api/service_pb2.py", true},
		{"api/service_pb2_grpc.py", true},
		{"internal/x/zz_generated.deepcopy.go", true},
		{"internal/x/types_generated.go", true},
		{"internal/api/server.gen.go", true},
		// Production code under a directory named doc, docs or test: counted.
		{"pkg/doc/render.go", false},
		{"internal/docs/handler.go", false},
		{"docs/site/server.go", false},
		{"src/main/java/com/acme/docs/DocsController.java", false},
		{"internal/test/harness.go", false},
		{"internal/app/foo/testdata/golden.json", false},
		// Main-source classes whose names end in Test: counted.
		{"src/main/java/com/acme/experiments/ABTest.java", false},
		{"src/main/java/com/acme/Test.java", false},
		{"src/main/kotlin/com/acme/ABTest.kt", false},
		{"src/main/java/com/acme/LoadTests.java", false},
		// Contracts and specs: counted.
		{"api/openapi.spec.yaml", false},
		{"web/src/api/client.spec.ts", false},
		// Other languages' test names: counted until an operator adds them.
		{"tests/test_login.py", false},
		{"spec/models/user_spec.rb", false},
		// Lockfiles: counted (no sensitive rule covers dependencies).
		{"go.sum", false},
		{"web/package-lock.json", false},
		{"yarn.lock", false},
		{"pnpm-lock.yaml", false},
		{"Cargo.lock", false},
		// Shipped or compiled content: counted.
		{"web/dist/app.min.js", false},
		{"web/src/__snapshots__/app.test.tsx.snap", false},
		{"web/src/pages/about.mdx", false},
		// Plain source.
		{"internal/app/foo/bar.go", false},
		{"cmd/control-plane/main.go", false},
		{"scripts/test", false},
		{"go.mod", false},
		{"migrations/000001_init.up.sql", false},
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

// TestDiffSize pins that the size comes from the diff alone: every line it
// shows (total), and those outside excluded files (source).
func TestDiffSize(t *testing.T) {
	patterns := []string{"*_test.go", "*.md"}
	tests := []struct {
		name       string
		files      []FileLines
		patterns   []string
		wantTotal  int
		wantSource int
	}{
		{"nothing excluded", []FileLines{{Path: "a.go", Added: 30, Deleted: 20}}, patterns, 50, 50},
		{"a test file's lines are left out", []FileLines{{Path: "a.go", Added: 50}, {Path: "a_test.go", Added: 500, Deleted: 100}}, patterns, 650, 50},
		{"no patterns leaves out nothing", []FileLines{{Path: "a_test.go", Added: 600}}, nil, 600, 600},
		{"every line excluded", []FileLines{{Path: "a_test.go", Added: 600}, {Path: "README.md", Deleted: 100}}, patterns, 700, 0},
		{"a rename is excluded only when both paths match", []FileLines{{Path: "x_test.go", OldPath: "x.go", Added: 80}}, patterns, 80, 80},
		{"a rename between two excluded paths is excluded", []FileLines{{Path: "b.md", OldPath: "a.md", Added: 80}}, patterns, 80, 0},
		{"an unnamed section is never excluded", []FileLines{{Path: "", Added: 30}}, []string{"**"}, 30, 30},
		{"no diff at all", nil, patterns, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			total, source := diffSize(tt.files, tt.patterns)
			if total != tt.wantTotal || source != tt.wantSource {
				t.Errorf("diffSize() = (%d, %d), want (%d, %d)", total, source, tt.wantTotal, tt.wantSource)
			}
		})
	}
}

// TestUnsupportedSizeExclusionConstruct pins every construct the
// codeowners dialect reads as literal text, so a pattern using one would
// silently never match what its author meant.
func TestUnsupportedSizeExclusionConstruct(t *testing.T) {
	tests := []struct {
		pattern       string
		wantConstruct string
	}{
		{"!docs/keep.md", `negation (a leading "!")`},
		{"#comment", `comment (a leading "#")`},
		{"[Tt]est/**", `character class ("[...]")`},
		{"app/[slug]/page.test.tsx", `character class ("[...]")`},
		{"stray]/*.md", `character class ("[...]")`},
		{"*.{md", `brace list ("{a,b}")`},
		{"rst}", `brace list ("{a,b}")`},
		{`docs\*.md`, `escape (a backslash)`},
		{"*_test.go", ""},
		{"**/fixtures/**", ""},
		{"src/*/gen?.go", ""},
		{"docs/#1.md", ""},
		{"my dir/*.md", ""},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			construct, ok := UnsupportedSizeExclusionConstruct(tt.pattern)
			if construct != tt.wantConstruct || ok != (tt.wantConstruct != "") {
				t.Errorf("UnsupportedSizeExclusionConstruct(%q) = (%q, %v), want (%q, %v)", tt.pattern, construct, ok, tt.wantConstruct, tt.wantConstruct != "")
			}
		})
	}
}

// TestDefaultSizeExclusions_AreSupported pins that no default uses a
// construct Load would refuse.
func TestDefaultSizeExclusions_AreSupported(t *testing.T) {
	for _, p := range DefaultSizeExclusions() {
		if construct, bad := UnsupportedSizeExclusionConstruct(p); bad {
			t.Errorf("default %q uses an unsupported %s", p, construct)
		}
	}
}

func TestDefaultSizeExclusions_ReturnsAFreshSlice(t *testing.T) {
	a := DefaultSizeExclusions()
	a[0] = "mutated"
	if DefaultSizeExclusions()[0] == "mutated" {
		t.Fatal("DefaultSizeExclusions() returned a shared slice a caller can mutate")
	}
}
