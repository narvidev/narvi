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
