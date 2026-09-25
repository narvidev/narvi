package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// C16 (M31/M32 pin): package main had no test file at all before this
// rewrite -- nothing exercised run()'s own exit-status contract (0 for a
// clean/non-breaking diff, 1 for anything MAJOR/FAIL-CLOSED or for a
// genuine read/parse error) end to end, through the real CLI flags and
// real file I/O. These two tests do.

type fixtureDir struct {
	dir string
}

func writeSchemaDoc(t *testing.T, dir, id string, defs map[string]any) {
	t.Helper()
	doc := map[string]any{
		"$schema":     "https://json-schema.org/draft/2020-12/schema",
		"$id":         id,
		"title":       "T",
		"description": "test",
		"$defs":       defs,
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "t", "v1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "t", "v1", "x.schema.json"), data, 0o644); err != nil {
		t.Fatalf("write schema: %v", err)
	}
}

func writeContractsDir(t *testing.T, version string, changelog string, defs map[string]any) fixtureDir {
	t.Helper()
	dir := t.TempDir()
	writeSchemaDoc(t, dir, "https://narvi.dev/t/v1/x.schema.json", defs)
	manifest := map[string]any{
		"version": "1.0.0",
		"surfaces": []any{
			map[string]any{"path": "t/v1/x.schema.json", "direction": "by-suffix", "status": "current"},
		},
	}
	mdata, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), mdata, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte(version+"\n"), 0o644); err != nil {
		t.Fatalf("write VERSION: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte(changelog), 0o644); err != nil {
		t.Fatalf("write CHANGELOG.md: %v", err)
	}
	return fixtureDir{dir: dir}
}

func writeRoutesGolden(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "routes.golden")
	if err := os.WriteFile(p, []byte("GET /api/sessions\n"), 0o644); err != nil {
		t.Fatalf("write routes.golden: %v", err)
	}
	return p
}

func runCLI(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	outFile, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatalf("create stdout temp: %v", err)
	}
	defer func() { _ = outFile.Close() }()
	errFile, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("create stderr temp: %v", err)
	}
	defer func() { _ = errFile.Close() }()

	code := run(args, outFile, errFile)

	stdout, _ := os.ReadFile(outFile.Name())
	stderr, _ := os.ReadFile(errFile.Name())
	return code, string(stdout), string(stderr)
}

// TestCLIExitZeroOnCleanDiff pins the non-breaking half of the exit-status
// contract: base compared against an identical head (same VERSION, same
// CHANGELOG, same schema content) must exit 0 and print "no findings".
func TestCLIExitZeroOnCleanDiff(t *testing.T) {
	changelog := "## [1.0.0]\n"
	fixture := writeContractsDir(t, "1.0.0", changelog, map[string]any{
		"Widget": map[string]any{"type": "string"},
	})
	routes := writeRoutesGolden(t, t.TempDir())

	code, stdout, stderr := runCLI(t, []string{
		"--base", fixture.dir, "--head", fixture.dir,
		"--routes-base", routes, "--routes-head", routes,
	})
	if code != 0 {
		t.Fatalf("want exit 0 for a clean diff, got %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !bytes.Contains([]byte(stdout), []byte("no findings")) {
		t.Fatalf("want \"no findings\" in stdout, got %q", stdout)
	}
}

// TestCLIExitOneOnBreakingDiff pins the breaking half: deleting a
// property with no compensating VERSION/CHANGELOG bump must exit 1 (both
// the row-1 MAJOR finding and the version-changelog MAJOR finding fire),
// and the output must say so.
func TestCLIExitOneOnBreakingDiff(t *testing.T) {
	base := writeContractsDir(t, "1.0.0", "## [1.0.0]\n", map[string]any{
		"Widget": map[string]any{
			"type":       "object",
			"properties": map[string]any{"a": map[string]any{"type": "string"}},
		},
	})
	head := writeContractsDir(t, "1.0.0", "## [1.0.0]\n", map[string]any{
		"Widget": map[string]any{
			"type":       "object",
			"properties": map[string]any{}, // "a" removed
		},
	})
	routes := writeRoutesGolden(t, t.TempDir())

	code, stdout, stderr := runCLI(t, []string{
		"--base", base.dir, "--head", head.dir,
		"--routes-base", routes, "--routes-head", routes,
	})
	if code != 1 {
		t.Fatalf("want exit 1 for a breaking diff, got %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !bytes.Contains([]byte(stdout), []byte("BREAKING")) {
		t.Fatalf("want \"BREAKING\" in stdout, got %q", stdout)
	}
}

// TestCLIGenesisModePrintsDirectionNotice pins F5 (round 4 review): a
// genesis-mode run (base has the real schema file on disk but no
// manifest.json at all) must print a notice naming the surface's
// direction as taken from HEAD's own manifest -- so a human reviewing a
// genesis-mode PR knows to check it by hand, per COMPATIBILITY.md's
// "Relaxations" section. This is a print-only change (F5's scope): the
// underlying genesis behavior (direction read from head) is unchanged.
func TestCLIGenesisModePrintsDirectionNotice(t *testing.T) {
	head := writeContractsDir(t, "1.0.0", "## [1.0.0]\n", map[string]any{
		"Widget": map[string]any{"type": "string"},
	})

	// Genesis base: the same schema file on disk (as the real merge-base
	// commit would have), but no manifest.json/VERSION/CHANGELOG.md --
	// loadInput's own genesis substitution kicks in for exactly this
	// shape.
	baseDir := t.TempDir()
	writeSchemaDoc(t, baseDir, "https://narvi.dev/t/v1/x.schema.json", map[string]any{
		"Widget": map[string]any{"type": "string"},
	})

	routes := writeRoutesGolden(t, t.TempDir())

	// The exit code here is governed by ordinary VERSION/CHANGELOG
	// discipline (genesis synthesizes base VERSION as "0.0.0", so head's
	// real "1.0.0" needs a matching CHANGELOG heading, unrelated to F5) --
	// this test only cares that the notice itself was printed.
	_, stdout, _ := runCLI(t, []string{
		"--base", baseDir, "--head", head.dir,
		"--routes-base", routes, "--routes-head", routes,
	})
	if !bytes.Contains([]byte(stdout), []byte("GENESIS MODE")) {
		t.Fatalf("want a GENESIS MODE notice in stdout, got %q", stdout)
	}
	if !bytes.Contains([]byte(stdout), []byte("t/v1/x.schema.json: by-suffix")) {
		t.Fatalf("want the notice to name t/v1/x.schema.json's direction (by-suffix, from HEAD's own manifest), got %q", stdout)
	}
}

// TestCLINonGenesisModeOmitsDirectionNotice: a normal (non-genesis) run,
// where base already has its own real manifest.json, must NOT print the
// genesis notice -- it is specific to the one-time governance-adoption
// case, not every run.
func TestCLINonGenesisModeOmitsDirectionNotice(t *testing.T) {
	changelog := "## [1.0.0]\n"
	fixture := writeContractsDir(t, "1.0.0", changelog, map[string]any{
		"Widget": map[string]any{"type": "string"},
	})
	routes := writeRoutesGolden(t, t.TempDir())

	code, stdout, stderr := runCLI(t, []string{
		"--base", fixture.dir, "--head", fixture.dir,
		"--routes-base", routes, "--routes-head", routes,
	})
	if code != 0 {
		t.Fatalf("want exit 0 for a clean diff, got %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if bytes.Contains([]byte(stdout), []byte("GENESIS MODE")) {
		t.Fatalf("a non-genesis run must not print the genesis notice, got %q", stdout)
	}
}

// TestCLINonAPIRouteChangeNeedsVersionBump is the end-to-end form of the
// defect this test was added for: a routes.golden change outside /api/
// (here, one OAuth route added), with VERSION and CHANGELOG untouched,
// used to exit 0 with "no findings" -- the checker read the /api/ rows
// only. It must exit 1, grading the route and demanding the bump.
func TestCLINonAPIRouteChangeNeedsVersionBump(t *testing.T) {
	fixture := writeContractsDir(t, "1.0.0", "## [1.0.0]\n", map[string]any{
		"Widget": map[string]any{"type": "string"},
	})
	routesDir := t.TempDir()
	baseRoutes := filepath.Join(routesDir, "base.golden")
	headRoutes := filepath.Join(routesDir, "head.golden")
	if err := os.WriteFile(baseRoutes, []byte("GET /api/sessions\nPOST /oauth/token\n"), 0o644); err != nil {
		t.Fatalf("write base routes: %v", err)
	}
	if err := os.WriteFile(headRoutes, []byte("GET /api/sessions\nPOST /oauth/revoke\nPOST /oauth/token\n"), 0o644); err != nil {
		t.Fatalf("write head routes: %v", err)
	}

	code, stdout, stderr := runCLI(t, []string{
		"--base", fixture.dir, "--head", fixture.dir,
		"--routes-base", baseRoutes, "--routes-head", headRoutes,
	})
	if code != 1 {
		t.Fatalf("want exit 1 for a non-/api/ route added with no VERSION bump, got %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, want := range []string{
		"[MINOR] rule 41 controlplane/testdata/routes.golden POST /oauth/revoke",
		"[MAJOR] rule version-changelog",
		"BREAKING",
	} {
		if !bytes.Contains([]byte(stdout), []byte(want)) {
			t.Errorf("want %q in stdout, got %q", want, stdout)
		}
	}
}

// TestCLIExitNonZeroOnReadError pins the "genuine tool failure" half: a
// BASE directory that does not exist at all must not be treated as the
// genesis case (that only covers a MISSING manifest.json specifically,
// with routes.golden and the directory itself still real) -- it must
// exit non-zero with a real error message.
func TestCLIExitNonZeroOnReadError(t *testing.T) {
	head := writeContractsDir(t, "1.0.0", "## [1.0.0]\n", map[string]any{
		"Widget": map[string]any{"type": "string"},
	})
	routes := writeRoutesGolden(t, t.TempDir())

	code, stdout, stderr := runCLI(t, []string{
		"--base", filepath.Join(t.TempDir(), "does-not-exist"), "--head", head.dir,
		"--routes-base", routes, "--routes-head", routes,
	})
	if code == 0 {
		t.Fatalf("want a non-zero exit for a base routes/schema read error, got 0; stdout=%q stderr=%q", stdout, stderr)
	}
}
