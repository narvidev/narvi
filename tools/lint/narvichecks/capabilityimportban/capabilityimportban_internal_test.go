package capabilityimportban

import "testing"

// TestSkipFile_DecidesOnPackagePathNotCheckoutLocation pins the property
// the allow-list exists to have: whether a file may import the registry is
// a fact about the code, never about where the repository sits on disk.
//
// This test exists because the allow-list was once a substring match over
// the absolute filename, with single-segment entries "/extension/" and
// "/controlplane/". Any clone under a directory of either name -- and any
// sub-package named either -- exempted the file, so the whole ban went
// silently off and the analyzer's own suite failed with "no diagnostic"
// for its planted violations. The rows below are those two reproductions,
// kept as tests so the substring form cannot come back unnoticed.
func TestSkipFile_DecidesOnPackagePathNotCheckoutLocation(t *testing.T) {
	t.Parallel()

	const shadow = "github.com/narvidev/narvi/internal/app/shadowscm"

	tests := []struct {
		name     string
		pkgPath  string
		filename string
		want     bool
	}{
		{
			name:     "suppression package in a checkout under a directory named extension",
			pkgPath:  shadow,
			filename: "/tmp/extension/narvi/internal/app/shadowscm/synthetic.go",
			want:     false,
		},
		{
			name:     "suppression package in a checkout under a directory named controlplane",
			pkgPath:  shadow,
			filename: "/home/ci/controlplane/narvi/internal/app/shadowscm/synthetic.go",
			want:     false,
		},
		{
			name:     "sub-package literally named extension inside a suppression package",
			pkgPath:  shadow + "/extension",
			filename: "/repo/internal/app/shadowscm/extension/gate.go",
			want:     false,
		},
		{
			name:     "the real composition root, wherever it is checked out",
			pkgPath:  "github.com/narvidev/narvi/controlplane",
			filename: "/somewhere/odd/controlplane/serve.go",
			want:     true,
		},
		{
			name:     "the real facade",
			pkgPath:  "github.com/narvidev/narvi/extension",
			filename: "/somewhere/odd/extension/module.go",
			want:     true,
		},
		{
			name:     "tests are exempt wherever they live",
			pkgPath:  shadow,
			filename: "/repo/internal/app/shadowscm/synthetic_test.go",
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := skipFile(tt.pkgPath, tt.filename); got != tt.want {
				t.Errorf("skipFile(%q, %q) = %v, want %v", tt.pkgPath, tt.filename, got, tt.want)
			}
		})
	}
}

// TestSkipFile_NoFileLevelExemption pins the fix for a bridge an exit
// audit of this guarantee found: this analyzer's allow-list is
// package-level only, with no per-file
// entry inside an otherwise-banned package. There used to be one --
// internal/adapters/inbound/httpapi/capabilities.go and
// requirecapability.go were each individually exempted inside httpapi, a
// package this analyzer otherwise bans like any other. That was
// dodgeable: Go imports are per file, but identifiers are per package, so
// a package-level helper either exempted file declared was callable,
// with no import of its own, from any OTHER file in httpapi --
// scmcredentials.go, a real §30 suppression path, in the audit's own
// reproduction. Every row below must come back false: no file in httpapi
// is exempt any more, including the two that used to be, and a file's
// own base name never matters to skipFile at all now -- only its
// package.
func TestSkipFile_NoFileLevelExemption(t *testing.T) {
	t.Parallel()

	const httpapi = "github.com/narvidev/narvi/internal/adapters/inbound/httpapi"

	tests := []struct {
		name     string
		filename string
	}{
		{
			name:     "the former capabilities.go exemption",
			filename: "/repo/internal/adapters/inbound/httpapi/capabilities.go",
		},
		{
			name:     "the former requirecapability.go exemption",
			filename: "/repo/internal/adapters/inbound/httpapi/requirecapability.go",
		},
		{
			name:     "scmcredentials.go, the file the audit actually bridged through",
			filename: "/repo/internal/adapters/inbound/httpapi/scmcredentials.go",
		},
		{
			name:     "a file merely NAMED like a former exemption, in a different package, was already covered -- this pins the converse: the real package, any file",
			filename: "/repo/internal/adapters/inbound/httpapi/helpers.go",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := skipFile(httpapi, tt.filename); got {
				t.Errorf("skipFile(%q, %q) = true, want false -- the allow-list is package-level only now", httpapi, tt.filename)
			}
		})
	}
}
