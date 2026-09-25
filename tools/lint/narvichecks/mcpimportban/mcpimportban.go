// Package mcpimportban implements technical plan §43 §4.5 item 1: the
// structural half of "the MCP adapter cannot reach a store or Authorize
// except through an HTTP handler". An ALLOW-LIST, scoped to exactly one
// package tree -- internal/adapters/inbound/mcp -- of every import path
// its production code may use: the official MCP SDK, the small set of
// third-party libraries it actually depends on, this repo's own httpapi/
// auth/platform/contracts packages, its own subpackages, and the standard
// library MINUS a short, explicit list of exceptions (database/sql,
// database/sql/driver, os/exec, net/rpc). Anything else is reported.
//
// # Why an allow-list, not a deny-list
//
// The MCP tool bridge's whole design (bridge.go's own doc comment) rests
// on every tool invoking the SAME http.HandlerFunc httpapi's own router
// already registers, in-process, rather than a second implementation
// sharing an extracted "core" function, or reaching a store, or a driver,
// or an application service directly. A round 2 review of this Step's own
// fix (findings N9/N18) closed the two most obvious dodges -- postgres/
// sqlcgen/database-sql/pgx -- with a DENY-list naming those paths
// specifically. A round 3 review (findings R7/R8) then found the deny-list
// itself was the defect: golang-migrate's own database/postgres
// subpackage (already a direct go.mod dependency, used legitimately by
// controlplane/migrate.go) wraps database/sql and lib/pq and runs
// arbitrary SQL through neither name the deny-list matched, and pgx/v5's
// own MODULE ROOT (pgx.Connect) and its stdlib subpackage were reachable
// too, because only pgxpool's own import path had a fixture pinning it.
// Every one of those is a different name for the identical class of
// defect: an unreviewed, unenumerated path to Postgres, or to a
// subprocess, or to a second RPC transport. A deny-list can only ever
// name the paths someone thought of; an allow-list needs no dodge to be
// discovered ahead of time, because anything not explicitly named is
// refused by construction -- exactly the "prefer exhaustive checks and
// allow-lists" principle this round's own review applied everywhere else
// in this Step (the batch-refusal look-ahead, the Origin comparison).
//
// # Scope: ONE package tree, not "everywhere except an allow-list"
//
// This analyzer inverts capabilityimportban's own shape the same way its
// predecessor did: it constrains imports FROM exactly one package tree,
// and says nothing at all about any other package in the repository --
// controlplane (the composition root) and httpapi both import postgres/
// authz/pgx freely and legitimately, and this analyzer must never flag
// either.
//
// # _test.go files are exempt
//
// A test file inside internal/adapters/inbound/mcp that imports postgres,
// pgx, or auth/httpapi directly (to build a real store or a real rig,
// mirroring httpapi's own *_integration_test.go convention) is not a
// second production path -- demotionsweep.skipFile's and
// capabilityimportban.skipFile's own identical "a test constructing X is
// not a production decision point" reasoning, applied here.
package mcpimportban

import (
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"
)

const doc = `report an import outside the allow-list for internal/adapters/inbound/mcp

Technical plan §43 §4.5 item 1: internal/adapters/inbound/mcp (the MCP tool
bridge) may import ONLY:
  - the official MCP SDK (github.com/modelcontextprotocol/go-sdk/...),
    github.com/santhosh-tekuri/jsonschema/v6, github.com/go-chi/chi/v5;
  - this repo's own internal/adapters/inbound/httpapi, internal/adapters/
    inbound/auth, internal/platform (and their own subpackages);
  - this repo's own contracts and contracts/gen/go/restdtos EXACTLY (never
    a sibling contracts/gen/go/* wire-protocol package, and never
    contracts/contractstest);
  - this repo's own internal/domain/mcpscope EXACTLY (technical plan
    §43.17: the scope vocabulary that decides which tools a request may
    see -- a pure rule with no I/O, never any other domain package, and
    never a subpackage of it);
  - its own subpackages;
  - the Go standard library, EXCEPT database/sql, database/sql/driver,
    os/exec, and net/rpc.

Every MCP tool must reach the application exclusively by invoking an
existing httpapi http.HandlerFunc in-process (the bridge); an import
outside this list would let a tool reach a store, render an authz verdict
directly, run raw SQL via a driver this list does not name (round 3 review
of PR #324, finding R7: golang-migrate's own database/postgres subpackage
is exactly such a path, reachable even while database/sql and pgx/v5 were
both individually banned), or spawn a subprocess / open a second RPC
transport -- reintroducing a second, divergent authorization path either
way. _test.go files are exempt (a test rig constructing a real store or
client is not a production decision point).`

// Analyzer reports any import outside the allow-list inside
// internal/adapters/inbound/mcp.
var Analyzer = &analysis.Analyzer{
	Name: "mcpimportban",
	Doc:  doc,
	Run:  run,
}

// targetPackage is internal/adapters/inbound/mcp itself, AND every one of
// its subpackages (isTargetPackage below matches this exactly OR this
// plus "/" plus anything) -- every OTHER package in the repository is
// silently ignored, regardless of what it imports (see this package's own
// doc comment, "Scope" section).
const targetPackage = "github.com/narvidev/narvi/internal/adapters/inbound/mcp"

// isTargetPackage reports whether path is internal/adapters/inbound/mcp
// itself or one of its subpackages -- a PREFIX match on "/", never a bare
// strings.HasPrefix(path, targetPackage) (which would also match an
// unrelated sibling package that merely starts with the same characters,
// e.g. a hypothetical .../mcp2).
func isTargetPackage(path string) bool {
	return matchesPathOrSubpackage(path, targetPackage)
}

// bannedStdlibImports is the SMALL, explicit exception list this analyzer
// carves out of an otherwise fully-allowed standard library (isAllowed's
// own doc comment explains why the library at large is allowed wholesale
// rather than itemized): database/sql and database/sql/driver are a
// second, driver-level path straight to Postgres with no authz.Authorize
// call and no REST twin (round 2 finding N9); os/exec is a subprocess-
// spawning capability execimportban already confines to the sandbox/
// outbound trees, which this package is not one of; net/rpc is a second
// RPC transport this design has no legitimate use for here.
var bannedStdlibImports = []string{
	"database/sql",
	"database/sql/driver",
	"os/exec",
	"net/rpc",
}

// allowedPrefixes are import paths this package may use freely, matched
// as EITHER an exact match OR a prefix of one of the path's own
// subpackages ("prefix" + "/" + anything) -- the official MCP SDK, the
// two other third-party libraries this package's own tools/schemas
// genuinely depend on, and this repo's own httpapi/auth/platform trees
// (each a single flat package today, but a future subpackage of any of
// them is exactly as legitimate as the package itself).
var allowedPrefixes = []string{
	"github.com/modelcontextprotocol/go-sdk",
	"github.com/santhosh-tekuri/jsonschema/v6",
	"github.com/go-chi/chi/v5",
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi",
	"github.com/narvidev/narvi/internal/adapters/inbound/auth",
	"github.com/narvidev/narvi/internal/platform",
}

// allowedExact are import paths allowed ONLY as an EXACT match, never as
// a prefix for some other, unrelated sibling package -- contracts/gen/go
// also holds clientws, sandboxws, and sessionconfig (other surfaces' own
// wire-protocol DTOs, entirely unrelated to this one), and contracts
// itself also holds contractstest (a test-only package); allowing
// "contracts" as a PREFIX would sweep all of those in too, which this
// package has no legitimate reason to import.
//
// internal/domain/mcpscope (technical plan §43.17) is here, EXACT, for the
// same reason: it is the one domain rule this package needs (which tools
// a grant's scopes let a request see), it holds no I/O and can reach no
// store, and a PREFIX entry would admit any subpackage someone later adds
// under it. Every other internal/domain package -- authz above all --
// stays banned: the tools' authorization is always their REST twins' own.
var allowedExact = []string{
	"github.com/narvidev/narvi/contracts",
	"github.com/narvidev/narvi/contracts/gen/go/restdtos",
	"github.com/narvidev/narvi/internal/domain/mcpscope",
}

// matchesPathOrSubpackage reports whether path is exactly prefix, or one
// of prefix's own subpackages (prefix + "/" + anything) -- never a bare
// strings.HasPrefix(path, prefix), which would also match an unrelated
// sibling package that merely starts with the same characters (e.g.
// "github.com/narvidev/narvi/internal/adapters/inbound/mcpgateway" is NOT
// a subpackage of ".../mcp", despite sharing that string prefix -- round
// 3 review of PR #324, finding R7/R8's own "no dodge available" standard,
// applied to matching itself).
func matchesPathOrSubpackage(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// isStdlibImportPath reports whether path is (almost certainly) a
// standard-library import path, using the SAME shape rule the Go
// toolchain itself relies on to tell a standard import apart from a
// module-hosted one: a REAL module path's first slash-separated element
// is a domain, which -- per Go's own module-path requirements -- always
// contains a dot ("github.com", "golang.org", "google.golang.org", ...);
// no standard-library import path ever does ("net/http", "encoding/json",
// "os", "context"). This needs no maintained enumeration of "every
// standard library package" at all -- exactly the point: an allow-list
// that had to itemize the whole standard library one package at a time
// would be exactly as fragile as the deny-list this analyzer replaces.
func isStdlibImportPath(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// isAllowed reports whether path may be imported from
// internal/adapters/inbound/mcp's own production code: its own
// subpackages, always; the standard library, except
// bannedStdlibImports; or one of allowedPrefixes/allowedExact.
func isAllowed(path string) bool {
	if isTargetPackage(path) {
		return true
	}
	if isStdlibImportPath(path) {
		return !isBannedStdlibImport(path)
	}
	for _, prefix := range allowedPrefixes {
		if matchesPathOrSubpackage(path, prefix) {
			return true
		}
	}
	for _, exact := range allowedExact {
		if path == exact {
			return true
		}
	}
	return false
}

// isBannedStdlibImport reports whether path is one of the small,
// explicit standard-library exceptions this analyzer still refuses (see
// bannedStdlibImports' own doc comment).
func isBannedStdlibImport(path string) bool {
	for _, banned := range bannedStdlibImports {
		if path == banned {
			return true
		}
	}
	return false
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
			if err != nil || isAllowed(path) {
				continue
			}
			pass.Reportf(imp.Pos(), "importing %q is not on the allow-list for internal/adapters/inbound/mcp (technical plan §43 §4.5): every MCP tool must reach the application exclusively by invoking an existing httpapi handler in-process (the bridge), and this package's own production imports are restricted to an explicit allow-list -- see this analyzer's own doc comment for the full list", path)
		}
	}
	return nil, nil
}
