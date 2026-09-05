// Package capabilityimportban implements the structural half of
// docs/design/boundaries-design.md, section 1.4, and technical plan §34.5's own
// "the registry is unreachable from any suppression path" guarantee: an
// import ban on the three packages that make a licence/capability
// decision reachable at all, outside a short, named allow-list.
//
// # Why an import ban, not a call-name ban
//
// The requirement is that a future contributor cannot silently put a
// capability check on a shadow-mode suppression path (§30). A ban on
// calling Registry.Enabled (or Registry.State) BY NAME -- the
// demotionsweep shape, which this package otherwise mirrors closely -- is
// dodgeable: a method value (`f := reg.Enabled; f(c)`), a local wrapper
// function, or a second field holding the same *capability.Registry all
// let a call site avoid the literal selector expression a call-name ban
// greps for. An IMPORT declaration cannot be dodged the same way: there
// is no way to reach a *capability.Registry, a license.Grant, or
// anything the extension façade re-exports without an import statement
// naming one of the three packages below, and that import is a syntactic
// fact checked before any of this analyzer's own logic runs -- exactly
// execimportban's own reasoning for "os/exec", applied to three packages
// instead of one, plus demotionsweep's own allow-list-by-directory/file
// discipline layered on top (necessary here because, unlike os/exec, this
// codebase has legitimate, deliberate importers: the composition root,
// the façade itself, and two named HTTP handler files).
//
// # What this buys
//
// internal/app/egressmode, every shadow* package, readonlymint,
// repodemotion, outboxworker, sessionactor, the outbound githubapi
// transport gate, and httpapi/scmcredentials.go can none of them import
// any of the three banned packages -- so none of their own suppression
// decisions (the transport gate's resolve, the decorator's isLive,
// OutboxStore.Create's stamp, the mint substitution, the push
// short-circuit) can take a licence state as an input, structurally,
// regardless of what any of their own code does. A future 12th
// suppression seam is covered the same way this analyzer already covers
// the first eleven: the ban is on the registry's own importers, never on
// an enumerated list of suppression call sites that would need updating
// every time a new one is added.
package capabilityimportban

import (
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

const doc = `report an import of the capability registry, the licence domain, or the
extension façade outside the composition root and two named HTTP gate files

A capability check is a decision point at a system boundary
(docs/design/boundaries-design.md, section 1.4); the shadow-mode suppression
guarantees (technical plan §30) are structural and must never depend on
one. Importing github.com/narvidev/narvi/internal/domain/license,
github.com/narvidev/narvi/internal/app/capability, or
github.com/narvidev/narvi/extension is banned everywhere except
controlplane (the composition root), extension itself (the façade's own
re-export), internal/app/capability and internal/domain/license
themselves, the two named httpapi capability-gate files, and _test.go
files. Wire the private implementation from the composition root instead
of reaching for the registry from wherever a decision is inconvenient to
thread through.`

// Analyzer reports any import of a banned capability-decision package
// from a file outside the allow-list.
var Analyzer = &analysis.Analyzer{
	Name: "capabilityimportban",
	Doc:  doc,
	Run:  run,
}

// bannedImportPaths are the three packages docs/design/
// boundaries-design.md, section 1.4, names as the only way to ever reach a licence
// or capability decision: the pure domain, the app-layer registry built
// on top of it, and the façade that re-exports both to a composed
// module.
var bannedImportPaths = []string{
	"github.com/narvidev/narvi/internal/domain/license",
	"github.com/narvidev/narvi/internal/app/capability",
	"github.com/narvidev/narvi/extension",
}

// allowedDirs are the packages allowed to import any banned path freely:
// the composition root (parses the key once, builds the registry, wires
// RequireCapability onto module routes); extension itself (its own
// re-export of license.Capability, and its ActorFromContext/Runtime
// wiring); and internal/app/capability and internal/domain/license
// themselves (capability's own registry.go legitimately imports license
// for Grant/Capability; a package does not "import" itself, but this
// keeps the matching rule uniform rather than special-casing "the banned
// package's own path needs no entry").
//
// These are Go IMPORT PATHS, matched exactly -- deliberately not
// filesystem-path substrings, which is what this list used to be. A
// substring rule over the absolute filename made the whole ban depend on
// where the repository happens to be checked out: a clone under any
// directory named "extension" or "controlplane" exempted every file in
// the repository, and the analyzer's own test then failed with "no
// diagnostic" for its planted violations. A package path is a property of
// the code; an absolute filename is a property of the machine, and a
// guarantee that is supposed to be structural cannot rest on one.
var allowedPackages = []string{
	"github.com/narvidev/narvi/controlplane",
	"github.com/narvidev/narvi/extension",
	"github.com/narvidev/narvi/internal/app/capability",
	"github.com/narvidev/narvi/internal/domain/license",
}

// allowedFiles are the two httpapi handler files docs/design/
// boundaries-design.md, section 1.2, names as the only place a capability decision
// is allowed at an HTTP route boundary: capabilities.go (GET
// /api/capabilities, the derived read model; pre-declared here so landing
// it never required touching this analyzer) and requirecapability.go
// (RequireCapability, this PR).
var allowedFiles = []struct{ pkg, base string }{
	{"github.com/narvidev/narvi/internal/adapters/inbound/httpapi", "capabilities.go"},
	{"github.com/narvidev/narvi/internal/adapters/inbound/httpapi", "requirecapability.go"},
}

func run(pass *analysis.Pass) (any, error) {
	if isSyntheticTestBinaryPackage(pass) {
		return nil, nil
	}
	for _, file := range pass.Files {
		filename := pass.Fset.Position(file.Pos()).Filename
		if skipFile(pass.Pkg.Path(), filename) {
			continue
		}
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil || !isBanned(path) {
				continue
			}
			pass.Reportf(imp.Pos(), "importing %q is banned outside the composition root and the two named capability-gate files (docs/design/boundaries-design.md, section 1.4): a capability check is a decision point at a system boundary, and the shadow-mode suppression guarantees (technical plan §30) are structural and must never depend on one -- wire the private implementation from the composition root instead", path)
		}
	}
	return nil, nil
}

// isSyntheticTestBinaryPackage reports whether pass is analyzing the
// synthetic "test binary main" package `go/packages` (and therefore this
// multichecker, run over patterns that include test files) generates
// alongside every package that has tests: `Name() == "main"`, `Path()`
// suffixed ".test". That synthetic package's own generated source
// (materialized under the build cache, never a real file in this
// repository) imports the tested package by its PLAIN import path purely
// to link the test binary -- for exactly the three packages this
// analyzer bans, that self-link is a false positive this analyzer would
// otherwise report on every single run: importing
// github.com/narvidev/narvi/internal/domain/license (or .../capability,
// or .../extension) to link THAT PACKAGE's OWN test binary is not a new
// consumer reaching for the registry, it is the Go toolchain testing the
// registry itself. A real `package main` binary (cmd/control-plane,
// cmd/sandbox-agent) has Name() == "main" too, but its own Path() is its
// ordinary import path, never ".test"-suffixed, so this check does not
// exempt those.
func isSyntheticTestBinaryPackage(pass *analysis.Pass) bool {
	return pass.Pkg.Name() == "main" && strings.HasSuffix(pass.Pkg.Path(), ".test")
}

// isBanned reports whether path is one of bannedImportPaths.
func isBanned(path string) bool {
	for _, banned := range bannedImportPaths {
		if path == banned {
			return true
		}
	}
	return false
}

// skipFile exempts _test.go files -- a test constructing a registry is
// not a production decision point, demotionsweep.skipFile's own identical
// reasoning -- and every allowed package and file.
//
// The decision is made on pkgPath (the Go import path) and on the file's
// BASE name, never on its absolute path. Both are properties of the code
// rather than of the checkout, so the ban holds identically in CI, in a
// contributor's home directory, and in a temporary clone whose path
// happens to contain one of the allowed names.
func skipFile(pkgPath, filename string) bool {
	base := filepath.Base(filepath.ToSlash(filename))
	if strings.HasSuffix(base, "_test.go") {
		return true
	}
	for _, pkg := range allowedPackages {
		if pkgPath == pkg {
			return true
		}
	}
	for _, f := range allowedFiles {
		if pkgPath == f.pkg && base == f.base {
			return true
		}
	}
	return false
}
