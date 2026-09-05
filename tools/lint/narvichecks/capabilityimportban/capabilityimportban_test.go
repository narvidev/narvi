package capabilityimportban_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/narvidev/narvi/tools/lint/narvichecks/capabilityimportban"
)

// TestAnalyzer proves the analyzer fires on an import of any of the three
// banned packages planted in a shadow package (internal/app/shadowscm),
// in internal/app/sessionactor, in internal/domain/review, and -- the
// fixture for the bridge an exit audit of this guarantee found -- in a
// file inside internal/adapters/inbound/httpapi itself (that fixture
// package's own scmcredentials.go): four real production packages this
// analyzer must keep off the capability registry entirely (docs/design/
// boundaries-design.md, section 1.4). It stays silent in every allowed
// PACKAGE (controlplane, extension, internal/app/capability -- which
// legitimately imports license itself) and in a _test.go file (package
// "d") -- but no longer by FILE inside any other package: this fixture
// package's own capabilities.go and requirecapability.go, standing in
// for the two real files that used to be individually exempted, are
// silent here only because -- like the real files after this fix --
// they import nothing banned at all any more.
//
// Every fixture package below lives under
// testdata/src/github.com/narvidev/narvi/... , mirroring this
// repository's own real import-path prefix exactly: Go's own internal-
// import rule (this whole design's own section-0 constraint) applies inside
// analysistest's synthetic GOPATH tree too, so a fixture importing
// .../internal/domain/license must itself be rooted at
// github.com/narvidev/narvi/... or the fixture fails to even COMPILE,
// before this analyzer ever runs.
func TestAnalyzer(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, capabilityimportban.Analyzer,
		"github.com/narvidev/narvi/internal/app/shadowscm",
		"github.com/narvidev/narvi/internal/app/sessionactor",
		"github.com/narvidev/narvi/internal/domain/review",
		"github.com/narvidev/narvi/controlplane",
		"github.com/narvidev/narvi/extension",
		"github.com/narvidev/narvi/internal/app/capability",
		"github.com/narvidev/narvi/internal/adapters/inbound/httpapi",
		"github.com/narvidev/narvi/d",
	)
}
