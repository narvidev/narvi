package contractstest

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/narvidev/narvi/contracts"
)

// manifest mirrors compat.Manifest's own shape (tools/contractscompat/
// compat/manifest.go) just enough for this test -- duplicated rather than
// imported so contracts/contractstest, a package other Steps may embed
// into a shipped binary one day (README.md's own "single source of wire
// truth"), never depends on a tools/ package.
type manifest struct {
	Surfaces []struct {
		Path string `json:"path"`
	} `json:"surfaces"`
}

// TestManifestMatchesEmbeddedSchemaSet is §6.3 design spec §3's guard 6:
// contracts/manifest.json's own surface set must equal contracts.FS's
// embedded *.schema.json set exactly -- a schema file added to disk but
// never registered in the manifest (or vice versa) would otherwise let
// tools/contractscompat silently skip it rather than fail loudly, since
// that tool trusts the manifest for direction/status but the actual
// content for everything else.
func TestManifestMatchesEmbeddedSchemaSet(t *testing.T) {
	data, err := os.ReadFile("../manifest.json")
	if err != nil {
		t.Fatalf("read manifest.json: %v", err)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse manifest.json: %v", err)
	}
	if len(m.Surfaces) == 0 {
		t.Fatal("manifest.json lists no surfaces")
	}

	manifestPaths := map[string]bool{}
	for _, s := range m.Surfaces {
		manifestPaths[s.Path] = true
	}

	embeddedPaths := map[string]bool{}
	err = fs.WalkDir(contracts.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".schema.json") {
			return nil
		}
		embeddedPaths[filepath.ToSlash(path)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk contracts.FS: %v", err)
	}
	if len(embeddedPaths) == 0 {
		t.Fatal("contracts.FS embeds no schema files")
	}

	for p := range manifestPaths {
		if !embeddedPaths[p] {
			t.Errorf("manifest.json lists %s but contracts.FS does not embed it", p)
		}
	}
	for p := range embeddedPaths {
		if !manifestPaths[p] {
			t.Errorf("contracts.FS embeds %s but manifest.json has no row for it", p)
		}
	}
}

// TestVersionMatchesPackageJSON pins contracts/VERSION and contracts/
// package.json's own "version" field to the same value (§6.3 design spec
// §4) -- two independent files a reviewer could update just one of by
// mistake, one read by contracts.Version (the Go/embedded side), the
// other by every npm tool that touches this scoped project (the TS
// side).
func TestVersionMatchesPackageJSON(t *testing.T) {
	versionData, err := os.ReadFile("../VERSION")
	if err != nil {
		t.Fatalf("read VERSION: %v", err)
	}
	version := strings.TrimSpace(string(versionData))
	if version == "" {
		t.Fatal("contracts/VERSION is empty")
	}
	if version != contracts.Version {
		t.Fatalf("contracts.Version (%q) does not match contracts/VERSION on disk (%q) -- did the embed drift?", contracts.Version, version)
	}

	pkgData, err := os.ReadFile("../package.json")
	if err != nil {
		t.Fatalf("read package.json: %v", err)
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(pkgData, &pkg); err != nil {
		t.Fatalf("parse package.json: %v", err)
	}
	if pkg.Version != version {
		t.Fatalf("contracts/package.json version (%q) does not match contracts/VERSION (%q)", pkg.Version, version)
	}
}
