package ops

import (
	"os"
	"strings"
	"testing"
)

// TestMigrationVersionsAreStructurallySound runs the checks against this
// repo's REAL /migrations directory, not a fixture -- the fixture-driven
// table below proves the checker detects each problem, and this test is
// what makes it bind on the tree that actually ships.
//
// The failure it exists to catch cannot be caught on a branch. Two
// parallel branches each take "the next number" from the same main, each
// is individually green, and the duplicate exists only in the merge
// neither branch's CI ever built -- at which point golang-migrate's iofs
// driver refuses the whole directory and nothing migrates, including on a
// fresh deployment. Running here means the merged tree is what gets
// checked.
func TestMigrationVersionsAreStructurallySound(t *testing.T) {
	dir := MigrationsDir(repoRoot(t))
	files, err := LoadMigrationFiles(dir)
	if err != nil {
		t.Fatalf("load migrations from %s: %v", dir, err)
	}
	if len(files) == 0 {
		t.Fatalf("loaded 0 migration files from %s -- this test would pass vacuously, which is worse than failing", dir)
	}

	if problems := CheckMigrationVersions(files); len(problems) > 0 {
		t.Errorf("migrations/ is structurally unsound:\n  - %s", strings.Join(problems, "\n  - "))
	}
}

func TestParseMigrationName(t *testing.T) {
	tests := []struct {
		name        string
		file        string
		wantVersion int
		wantSlug    string
		wantDir     string
		wantErr     bool
	}{
		{"up", "000042_widgets.up.sql", 42, "widgets", "up", false},
		{"down", "000042_widgets.down.sql", 42, "widgets", "down", false},
		{"slug with underscores", "000007_a_b_c.up.sql", 7, "a_b_c", "up", false},
		{"no direction", "000042_widgets.sql", 0, "", "", true},
		{"no version separator", "000042.up.sql", 0, "", "", true},
		{"non-numeric version", "abcdef_widgets.up.sql", 0, "", "", true},
		{"five digits", "00042_widgets.up.sql", 0, "", "", true},
		{"seven digits", "0000042_widgets.up.sql", 0, "", "", true},
		{"empty slug", "000042_.up.sql", 0, "", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMigrationName(tc.file)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseMigrationName(%q) = %+v, want an error", tc.file, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMigrationName(%q) unexpected error: %v", tc.file, err)
			}
			if got.Version != tc.wantVersion || got.Slug != tc.wantSlug || got.Direction != tc.wantDir {
				t.Errorf("parseMigrationName(%q) = {%d %q %q}, want {%d %q %q}",
					tc.file, got.Version, got.Slug, got.Direction, tc.wantVersion, tc.wantSlug, tc.wantDir)
			}
		})
	}
}

func TestCheckMigrationVersions(t *testing.T) {
	mf := func(names ...string) []MigrationFile {
		t.Helper()
		out := make([]MigrationFile, 0, len(names))
		for _, n := range names {
			parsed, err := parseMigrationName(n)
			if err != nil {
				t.Fatalf("fixture %q does not parse: %v", n, err)
			}
			out = append(out, parsed)
		}
		return out
	}

	tests := []struct {
		name  string
		files []MigrationFile
		// wantSubstr, when non-empty, must appear in at least one problem
		// -- asserting on the message and not just the count, so a check
		// that fires for the wrong reason cannot pass this table.
		wantSubstr string
		wantOK     bool
	}{
		{
			name:   "a well-formed pair",
			files:  mf("000001_a.up.sql", "000001_a.down.sql"),
			wantOK: true,
		},
		{
			name:   "a gap between versions is allowed",
			files:  mf("000001_a.up.sql", "000001_a.down.sql", "000009_b.up.sql", "000009_b.down.sql"),
			wantOK: true,
		},
		{
			name: "the parallel-branch collision: two different slugs at one version",
			files: mf(
				"000126_plan_documents_structured.up.sql", "000126_plan_documents_structured.down.sql",
				"000126_release_manifest_checks_composition.up.sql", "000126_release_manifest_checks_composition.down.sql",
			),
			wantSubstr: "version 000126 has 2 .up.sql files",
		},
		{
			name:       "an up with no down",
			files:      mf("000001_a.up.sql"),
			wantSubstr: "no .down.sql",
		},
		{
			name:       "a down with no up",
			files:      mf("000001_a.down.sql"),
			wantSubstr: "no .up.sql",
		},
		{
			name:       "a pair whose halves disagree on the slug",
			files:      mf("000001_a.up.sql", "000001_b.down.sql"),
			wantSubstr: `pairs slug "a" (up) with "b" (down)`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			problems := CheckMigrationVersions(tc.files)
			if tc.wantOK {
				if len(problems) != 0 {
					t.Fatalf("CheckMigrationVersions = %v, want no problems", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatalf("CheckMigrationVersions found no problems, want one containing %q", tc.wantSubstr)
			}
			joined := strings.Join(problems, "\n")
			if !strings.Contains(joined, tc.wantSubstr) {
				t.Errorf("CheckMigrationVersions problems do not mention %q:\n%s", tc.wantSubstr, joined)
			}
		})
	}
}

// TestCheckMigrationGaps is CheckMigrationGaps' own pure, unconditional
// table test -- always runs, everywhere (unlike TestNoMigrationGaps
// below, which only ever runs against the real tree on main) -- U5 audit
// fix.
func TestCheckMigrationGaps(t *testing.T) {
	mf := func(names ...string) []MigrationFile {
		t.Helper()
		out := make([]MigrationFile, 0, len(names))
		for _, n := range names {
			parsed, err := parseMigrationName(n)
			if err != nil {
				t.Fatalf("fixture %q does not parse: %v", n, err)
			}
			out = append(out, parsed)
		}
		return out
	}

	tests := []struct {
		name       string
		files      []MigrationFile
		wantSubstr string
		wantOK     bool
	}{
		{
			name:   "empty",
			files:  nil,
			wantOK: true,
		},
		{
			name:   "one version, trivially no gap",
			files:  mf("000001_a.up.sql", "000001_a.down.sql"),
			wantOK: true,
		},
		{
			name:   "contiguous versions",
			files:  mf("000001_a.up.sql", "000002_b.up.sql", "000003_c.up.sql"),
			wantOK: true,
		},
		{
			name:       "a single missing version in the middle",
			files:      mf("000134_a.up.sql", "000136_b.up.sql", "000137_c.up.sql"),
			wantSubstr: "missing version 000135",
		},
		{
			name: "multiple missing versions each reported",
			files: mf(
				"000001_a.up.sql",
				"000005_b.up.sql",
			),
			wantSubstr: "missing version 000002",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			problems := CheckMigrationGaps(tc.files)
			if tc.wantOK {
				if len(problems) != 0 {
					t.Fatalf("CheckMigrationGaps = %v, want no problems", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatalf("CheckMigrationGaps found no problems, want one containing %q", tc.wantSubstr)
			}
			joined := strings.Join(problems, "\n")
			if !strings.Contains(joined, tc.wantSubstr) {
				t.Errorf("CheckMigrationGaps problems do not mention %q:\n%s", tc.wantSubstr, joined)
			}
		})
	}
}

// TestCheckMigrationGaps_ReportsEveryMissingVersion pins the "multiple
// missing versions each reported" case's own full count -- a check that
// silently stopped after the first gap would still pass a substring-only
// assertion.
func TestCheckMigrationGaps_ReportsEveryMissingVersion(t *testing.T) {
	files := []MigrationFile{
		{Version: 1, Slug: "a", Direction: "up", Name: "000001_a.up.sql"},
		{Version: 5, Slug: "b", Direction: "up", Name: "000005_b.up.sql"},
	}
	problems := CheckMigrationGaps(files)
	if len(problems) != 3 {
		t.Fatalf("CheckMigrationGaps returned %d problems, want 3 (versions 2, 3, 4 each missing):\n%s", len(problems), strings.Join(problems, "\n"))
	}
	for _, want := range []string{"000002", "000003", "000004"} {
		found := false
		for _, p := range problems {
			if strings.Contains(p, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no problem mentions missing version %s:\n%s", want, strings.Join(problems, "\n"))
		}
	}
}

// TestNoMigrationGaps is U5's own CI-enforcing structural guard: a gap in
// this repo's REAL /migrations directory must fail the build -- but ONLY
// once merged to main, never on a branch (see CheckMigrationGaps' own doc
// comment for the full "why": a feature branch legitimately has a pending
// gap until a sibling that owns the lower number merges first, and this
// check must never fail a branch's own CI for that reason -- this exact
// branch has one, 000135, as of this fix).
//
// Gated on GITHUB_EVENT_NAME == "push" && GITHUB_REF_NAME == "main" --
// GitHub Actions' own default environment variables, populated for every
// step without any workflow-file change: this repository's own
// .github/workflows/ci.yml triggers on `push: branches: [main]` (a
// post-merge run) and `pull_request:` (every PR, including this branch's
// own) -- checking event_name/ref_name this way distinguishes the two
// without a new job, a new `if:`, or any CI configuration this package
// would otherwise have no way to see for itself. Locally (no CI env vars
// set at all) this also skips, exactly like the "on a branch" case --
// running this against a developer's own local tree, mid-work, would be
// the identical false alarm.
func TestNoMigrationGaps(t *testing.T) {
	if os.Getenv("GITHUB_EVENT_NAME") != "push" || os.Getenv("GITHUB_REF_NAME") != "main" {
		t.Skip("only meaningful once merged to main -- see this test's own doc comment: a feature branch legitimately has a pending gap until a sibling that owns the lower number merges first, and this check must never fail a branch's own CI (or a developer's own local run) for that reason")
	}

	dir := MigrationsDir(repoRoot(t))
	files, err := LoadMigrationFiles(dir)
	if err != nil {
		t.Fatalf("load migrations from %s: %v", dir, err)
	}
	if len(files) == 0 {
		t.Fatalf("loaded 0 migration files from %s -- this test would pass vacuously, which is worse than failing", dir)
	}

	if problems := CheckMigrationGaps(files); len(problems) > 0 {
		t.Errorf("migrations/ has a gap on main, where every merged migration must already be present:\n  - %s", strings.Join(problems, "\n  - "))
	}
}
