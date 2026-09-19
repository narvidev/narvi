package reasoncoverage_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/narvidev/narvi/tools/lint/narvichecks/reasoncoverage"
)

// TestAnalyzer proves the analyzer fires on a Reason constant that All()
// omits -- package "a" is a synthetic reproduction of A1's own audit
// finding: ReasonMissing is declared, of type Reason, and All() never
// lists it -- and stays silent on package "b", where every declared
// Reason constant (including the ReasonNone-style deliberate exception)
// is either listed in All() or named in the analyzer's own
// allowedMissing. Both packages are named "imagedecision" -- this
// Analyzer scopes itself by package NAME, not by testdata's own fake
// import path, so this exercises the exact same scoping decision that
// applies to the real internal/domain/imagedecision package.
func TestAnalyzer(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, reasoncoverage.Analyzer, "a", "b")
}
