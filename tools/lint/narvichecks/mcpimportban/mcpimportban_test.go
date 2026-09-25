package mcpimportban_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/narvidev/narvi/tools/lint/narvichecks/mcpimportban"
)

// TestAnalyzer proves the ALLOW-LIST (round 3 review of PR #324, findings
// R7/R8, replacing the prior deny-list) fires on every import outside it
// inside package mcp (bad.go): postgres, sqlcgen, authz, database/sql,
// database/sql/driver, os/exec, net/rpc, pgx's own MODULE ROOT (not only
// pgxpool -- finding R8), pgx/v5/stdlib, golang-migrate's own database/
// postgres driver (finding R7 -- reachable even while database/sql and
// pgx/v5 were each individually banned by name), a generic
// internal/adapters/outbound/* package unrelated to postgres by name
// (somestore), an unrelated contracts/gen/go/* sibling of the one
// contracts subpackage the allow-list actually names (clientws), and the
// internal/app/* prefix -- pinned by TWO members, actorauthz and
// modelcatalog, not one, so a mutant that narrows that check to an exact
// match against a single name still fails (round 2 review, finding N18).
// It fires the SAME way inside a SUBPACKAGE of mcp (internal/leak --
// finding M4: a prior version of this analyzer compared pass.Pkg.Path()
// for exact equality only, so a subpackage's own import went entirely
// unreported). It stays silent for every entry actually ON the allow-list
// in the SAME package (good.go: httpapi, platform; good2.go: the MCP SDK,
// jsonschema, chi, auth, contracts, contracts/gen/go/restdtos, and a
// plain stdlib import that is not one of the four exceptions), stays
// silent inside a _test.go file in the SAME package (a real
// integration-rig shape), stays silent for an UNRELATED package ("d")
// that imports postgres freely (the whole point of this analyzer's
// inverted scope versus capabilityimportban), and stays silent for
// mcpgateway -- a sibling of internal/adapters/inbound/mcp that merely
// shares its own "mcp" prefix as CHARACTERS, not as a path segment
// (finding N18: pins isTargetPackage's/matchesPathOrSubpackage's own
// "+/" boundary).
func TestAnalyzer(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, mcpimportban.Analyzer,
		"github.com/narvidev/narvi/internal/adapters/inbound/mcp",
		"github.com/narvidev/narvi/internal/adapters/inbound/mcp/internal/leak",
		"github.com/narvidev/narvi/internal/adapters/inbound/mcpgateway",
		"github.com/narvidev/narvi/d",
	)
}
