//go:build integration

// This file pins runRoutesCommand's own output format to
// controlplane/testdata/routes.golden BYTE FOR BYTE -- §41.1 review round
// 1, finding P1/P3's own proof obligation: the image's "routes" subcommand
// must produce a route table IDENTICAL to the golden, not merely one
// where every golden line happens to resolve on a live HTTP probe (the
// Makefile-level probe this Step's own review replaced -- see that
// target's own doc comment). Deliberately a byte-exact comparison, not
// TestBuild_RouteTableMatchesGolden's own missing/extra-set comparison
// immediately above in build_integration_test.go: a route ADDED to the
// router without a matching golden update fails THIS test (extra line in
// runRoutesCommand's own output, absent from the golden), exactly as a
// route removed without updating the golden already fails
// TestBuild_RouteTableMatchesGolden -- together the two tests cover both
// directions doubly, at two different layers (Build's own live router
// object vs. this command's own stdout-shaped rendering), which is the
// whole point: this test is what a `docker run <image> routes | cmp -
// routes.golden` in CI is actually trusting.
package controlplane

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestRunRoutesCommand_MatchesGolden is this file's own top doc comment's
// proof obligation.
func TestRunRoutesCommand_MatchesGolden(t *testing.T) {
	setRequiredEnv(t)

	_, connStr := newTestPool(t)
	t.Setenv("NARVI_DATABASE_URL", connStr)

	var buf bytes.Buffer
	if err := runRoutesCommand(t.Context(), &buf); err != nil {
		t.Fatalf("runRoutesCommand: %v", err)
	}

	golden, err := os.ReadFile(filepath.Join("testdata", "routes.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}

	if !bytes.Equal(buf.Bytes(), golden) {
		t.Errorf("runRoutesCommand's own output does not match controlplane/testdata/routes.golden BYTE FOR BYTE.\ngot:\n%s\nwant:\n%s", buf.String(), string(golden))
	}
}
