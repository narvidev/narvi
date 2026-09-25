package mcpimportban_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/narvidev/narvi/tools/lint/narvichecks/mcpimportban"
)

// TestAnalyzer proves the analyzer fires on every banned import inside
// package mcp (postgres, sqlcgen, authz, database/sql, pgx/v5/pgxpool,
// and the internal/app/* prefix -- pinned by TWO members, actorauthz and
// modelcatalog, not one, so a mutant that narrows the prefix check to an
// exact match against a single name still fails -- round 2 review of PR
// #324, finding N18), fires the SAME way inside a SUBPACKAGE of mcp
// (internal/leak -- finding M4 in the adversarial review of PR #324: a
// prior version of this analyzer compared pass.Pkg.Path() for exact
// equality only, so a subpackage's own banned import went entirely
// unreported), stays silent for an allowed import in the SAME package
// (httpapi, platform), stays silent inside a _test.go file in the SAME
// package (a real integration-rig shape), stays silent for an UNRELATED
// package ("d") that imports postgres freely (the whole point of this
// analyzer's inverted scope versus capabilityimportban), and stays
// silent for mcpgateway -- a sibling of internal/adapters/inbound/mcp
// that merely shares its own "mcp" prefix as CHARACTERS, not as a path
// segment (finding N18: pins isTargetPackage's own "+/" boundary, which
// no fixture in this testdata exercised before).
func TestAnalyzer(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, mcpimportban.Analyzer,
		"github.com/narvidev/narvi/internal/adapters/inbound/mcp",
		"github.com/narvidev/narvi/internal/adapters/inbound/mcp/internal/leak",
		"github.com/narvidev/narvi/internal/adapters/inbound/mcpgateway",
		"github.com/narvidev/narvi/d",
	)
}
