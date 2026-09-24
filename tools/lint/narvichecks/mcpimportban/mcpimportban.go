// Package mcpimportban implements technical plan §43 §4.5 item 1: the
// structural half of "the MCP adapter cannot reach a store or Authorize
// except through an HTTP handler". An import ban, scoped to exactly one
// package -- internal/adapters/inbound/mcp -- banning it from importing
// the Postgres adapter, sqlcgen, the authz domain, internal/app/
// actorauthz, or any OTHER internal/app/* application service.
//
// # Why an import ban, not a code-review convention
//
// The MCP tool bridge's whole design (bridge.go's own doc comment) rests
// on every tool invoking the SAME http.HandlerFunc httpapi's own router
// already registers, in-process, rather than a second implementation
// sharing an extracted "core" function. That discipline is easy to state
// and easy to erode one convenience import at a time: a future tool with
// no HTTP twin yet is a standing temptation to reach for
// *postgres.SessionStore directly "just this once", or to render an
// authz.Authorize verdict inline instead of threading a new REST route
// through the bridge. An import ban makes that impossible to even
// COMPILE, mirroring capabilityimportban's identical reasoning (this
// package's own sibling) for why an import-path ban beats a call-name or
// review-only convention: there is no dodge available (a re-exported
// type alias, a local wrapper, a second field holding the same store)
// that does not itself require importing one of the banned paths.
//
// # Scope: ONE package, not "everywhere except an allow-list"
//
// Unlike capabilityimportban (which bans a few paths EVERYWHERE except a
// short allow-list of legitimate importers), this analyzer inverts the
// shape: it bans several paths from exactly ONE package, and says nothing
// at all about any other package in the repository -- controlplane (the
// composition root) and httpapi both import postgres/authz freely and
// legitimately, and this analyzer must never flag either.
//
// # _test.go files are exempt
//
// A test file inside internal/adapters/inbound/mcp that imports postgres
// directly (to build a real store for an integration-style rig, mirroring
// httpapi's own *_integration_test.go convention) is not a second
// production path -- demotionsweep.skipFile's and capabilityimportban.
// skipFile's own identical "a test constructing X is not a production
// decision point" reasoning, applied here.
package mcpimportban

import (
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

const doc = `report a banned import inside internal/adapters/inbound/mcp

Technical plan §43 §4.5 item 1: internal/adapters/inbound/mcp (the MCP tool
bridge) may import the official MCP SDK, httpapi, auth, platform, contracts
(+contracts/gen/go/restdtos), chi, and stdlib -- never internal/adapters/
outbound/postgres (or sqlcgen), internal/domain/authz, or any internal/app/*
application service (including internal/app/actorauthz). Every MCP tool must
reach the application exclusively by invoking an existing httpapi
http.HandlerFunc in-process (the bridge); a banned import here would let a
tool reach a store or render an authz verdict directly, reintroducing a
second, divergent authorization path. _test.go files are exempt (a test rig
constructing a real store is not a production decision point).`

// Analyzer reports any banned import inside internal/adapters/inbound/mcp.
var Analyzer = &analysis.Analyzer{
	Name: "mcpimportban",
	Doc:  doc,
	Run:  run,
}

// targetPackage is internal/adapters/inbound/mcp itself, AND every one of
// its subpackages (isTargetPackage below matches this exactly OR this
// plus "/" plus anything) -- every OTHER package in the repository is
// silently ignored, regardless of what it imports (see this package's
// own doc comment, "Scope" section). A prior version of isTargetPackage
// compared pass.Pkg.Path() for exact equality only, so a subpackage such
// as internal/adapters/inbound/mcp/internal/direct could import postgres
// (or any other banned path) freely, and package mcp could then import
// THAT subpackage -- never itself importing a banned path directly, so
// never reported -- and call a method on the value it returned, reaching
// a store or an authz verdict exactly as if the ban did not exist. The
// analyzer's own doc comment already claimed "there is no dodge
// available"; a subpackage was exactly that dodge.
const targetPackage = "github.com/narvidev/narvi/internal/adapters/inbound/mcp"

// bannedExactImports are banned by exact import path.
var bannedExactImports = []string{
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres",
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen",
	"github.com/narvidev/narvi/internal/domain/authz",
}

// bannedPrefix bans EVERY internal/app/* package, not an enumerated list
// of today's services -- a future application service is covered the
// same way the first one is, with no update to this analyzer needed
// (execimportban's and capabilityimportban's own identical "ban the
// class, not an enumerated list of today's members" precedent).
const bannedPrefix = "github.com/narvidev/narvi/internal/app/"

// isTargetPackage reports whether path is internal/adapters/inbound/mcp
// itself or one of its subpackages -- a PREFIX match on "/", never a
// bare strings.HasPrefix(path, targetPackage) (which would also match an
// unrelated sibling package that merely starts with the same characters,
// e.g. a hypothetical .../mcp2).
func isTargetPackage(path string) bool {
	return path == targetPackage || strings.HasPrefix(path, targetPackage+"/")
}

func run(pass *analysis.Pass) (any, error) {
	if !isTargetPackage(pass.Pkg.Path()) {
		return nil, nil
	}
	for _, file := range pass.Files {
		filename := pass.Fset.Position(file.Pos()).Filename
		if strings.HasSuffix(filepath.Base(filepath.ToSlash(filename)), "_test.go") {
			continue
		}
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil || !isBanned(path) {
				continue
			}
			pass.Reportf(imp.Pos(), "importing %q is banned inside internal/adapters/inbound/mcp (technical plan §43 §4.5): every MCP tool must reach the application exclusively by invoking an existing httpapi handler in-process (the bridge), never a store or the authz domain directly", path)
		}
	}
	return nil, nil
}

// isBanned reports whether path is one of bannedExactImports or matches
// bannedPrefix.
func isBanned(path string) bool {
	for _, banned := range bannedExactImports {
		if path == banned {
			return true
		}
	}
	return strings.HasPrefix(path, bannedPrefix)
}
