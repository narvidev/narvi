package compat

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoContractsDir/repoRoutesFile locate the REAL /contracts directory and
// controlplane/testdata/routes.golden from this package's own directory
// (tools/contractscompat/compat), for guard 8's smoke test: "real tool on
// contracts/ vs itself = zero findings; vs a copy with a response
// property deleted = MAJOR."
const (
	repoContractsDir = "../../../contracts"
	repoRoutesFile   = "../../../controlplane/testdata/routes.golden"
)

func loadRealInput(t *testing.T) Input {
	t.Helper()
	files, err := loadRealSchemaFiles(t, repoContractsDir)
	if err != nil {
		t.Fatalf("load real schema files: %v", err)
	}
	manifestRaw, err := os.ReadFile(filepath.Join(repoContractsDir, "manifest.json"))
	if err != nil {
		t.Fatalf("read real manifest.json: %v", err)
	}
	versionRaw, err := os.ReadFile(filepath.Join(repoContractsDir, "VERSION"))
	if err != nil {
		t.Fatalf("read real VERSION: %v", err)
	}
	changelogRaw, err := os.ReadFile(filepath.Join(repoContractsDir, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read real CHANGELOG.md: %v", err)
	}
	routesRaw, err := os.ReadFile(repoRoutesFile)
	if err != nil {
		t.Fatalf("read real routes.golden: %v", err)
	}

	return Input{
		BaseManifestRaw:  manifestRaw,
		HeadManifestRaw:  manifestRaw,
		BaseVersion:      strings.TrimSpace(string(versionRaw)),
		HeadVersion:      strings.TrimSpace(string(versionRaw)),
		BaseChangelogRaw: changelogRaw,
		HeadChangelogRaw: changelogRaw,
		BaseRoutes:       routesRaw,
		HeadRoutes:       routesRaw,
		BaseSchemaFiles:  files,
		HeadSchemaFiles:  cloneFiles(files),
	}
}

func loadRealSchemaFiles(t *testing.T, dir string) (map[string][]byte, error) {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".schema.json") {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = data
		return nil
	})
	return out, err
}

func cloneFiles(in map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(in))
	for k, v := range in {
		cp := make([]byte, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// TestSmokeRealContractsAgainstItself is half of guard 8: the real tool,
// run against the real /contracts directory on both sides, must find
// nothing to say.
func TestSmokeRealContractsAgainstItself(t *testing.T) {
	in := loadRealInput(t)
	report, err := Compare(in)
	if err != nil {
		t.Fatalf("Compare: unexpected error: %v", err)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("comparing contracts/ against itself found %d findings, want 0: %+v", len(report.Findings), report.Findings)
	}
}

// TestSmokeRealContractsWithPropertyDeleted is the other half of guard 8:
// deleting a required response property (Session.id, a required property
// of a platform-produced entity) from a copy of the real rest/v1/dtos.
// schema.json must be classified MAJOR (row 1) and must fail
// report.HasBreaking().
func TestSmokeRealContractsWithPropertyDeleted(t *testing.T) {
	in := loadRealInput(t)

	const restPath = "rest/v1/dtos.schema.json"
	raw, ok := in.HeadSchemaFiles[restPath]
	if !ok {
		t.Fatalf("real contracts tree has no %s", restPath)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", restPath, err)
	}
	defs, _ := doc["$defs"].(map[string]any)
	session, ok := defs["Session"].(map[string]any)
	if !ok {
		t.Fatal("real rest/v1/dtos.schema.json has no $defs.Session")
	}
	props, ok := session["properties"].(map[string]any)
	if !ok {
		t.Fatal("real $defs.Session has no properties")
	}
	if _, ok := props["id"]; !ok {
		t.Fatal("real $defs.Session.properties has no \"id\" -- fixture assumption changed, update this test")
	}
	delete(props, "id")

	mutated, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal mutated schema: %v", err)
	}
	// A VERSION bump matching the change so the version/changelog rule
	// (not under test here) doesn't itself add unrelated findings.
	in.HeadSchemaFiles[restPath] = mutated
	in.HeadVersion = "2.0.0"
	in.HeadChangelogRaw = []byte("## [2.0.0]\n### " + restPath + "\n- Removed: Session.id (test fixture)\n\n" + string(in.BaseChangelogRaw))

	report, err := Compare(in)
	if err != nil {
		t.Fatalf("Compare: unexpected error: %v", err)
	}
	if !report.HasBreaking() {
		t.Fatalf("deleting Session.id must be classified breaking, got findings: %+v", report.Findings)
	}
	if !containsFinding(report.Findings, "1", SeverityMajor) {
		t.Fatalf("want rule 1 MAJOR among findings, got: %+v", report.Findings)
	}
}
