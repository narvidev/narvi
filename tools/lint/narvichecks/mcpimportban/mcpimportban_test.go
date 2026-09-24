package mcpimportban_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/narvidev/narvi/tools/lint/narvichecks/mcpimportban"
)

// TestAnalyzer proves the analyzer fires on every banned import inside
// package mcp (postgres, sqlcgen, authz, and the internal/app/* prefix,
// via actorauthz), stays silent for an allowed import in the SAME
// package (httpapi, platform), stays silent inside a _test.go file in
// the SAME package (a real integration-rig shape), and -- the whole
// point of this analyzer's inverted scope versus capabilityimportban --
// stays silent for an UNRELATED package ("d") that imports postgres
// freely, proving the ban never leaks beyond internal/adapters/inbound/
// mcp itself.
func TestAnalyzer(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, mcpimportban.Analyzer,
		"github.com/narvidev/narvi/internal/adapters/inbound/mcp",
		"github.com/narvidev/narvi/d",
	)
}
