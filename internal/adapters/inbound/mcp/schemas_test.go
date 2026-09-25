package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/tools/go/packages"

	"github.com/narvidev/narvi/contracts"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/platform"
)

// rawRestDef independently re-parses contracts.FS's own rest/v1/
// dtos.schema.json (NOT via schemas.go's own loadedRestDefs) so this
// test's own "verbatim" assertions check schemas.go's output against the
// contract file itself, never against schemas.go's own idea of it.
func rawRestDef(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := contracts.FS.ReadFile("rest/v1/dtos.schema.json")
	if err != nil {
		t.Fatalf("read embedded schema: %v", err)
	}
	var doc struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse embedded schema: %v", err)
	}
	raw, ok := doc.Defs[name]
	if !ok {
		t.Fatalf("no $def named %q", name)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal $def %q: %v", name, err)
	}
	return m
}

// TestToolInputSchemas_ComeFromContracts pins technical plan §43.10: each
// tool's InputSchema is the embedded $defs entry, VERBATIM -- no
// bundling, no rewriting, nothing added or removed.
func TestToolInputSchemas_ComeFromContracts(t *testing.T) {
	for _, spec := range toolSpecs(Twins{}) {
		t.Run(spec.Name, func(t *testing.T) {
			got, err := inputSchema(spec.InputDef)
			if err != nil {
				t.Fatalf("inputSchema(%q) error = %v", spec.InputDef, err)
			}
			want := rawRestDef(t, spec.InputDef)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("inputSchema(%q) = %#v,\nwant (verbatim from contracts) %#v", spec.InputDef, got, want)
			}
		})
	}
}

// TestToolOutputSchemas_ValidateRealBodies pins technical plan §43.10:
// the bundled OutputSchema for each of the three 180 tools is a
// self-contained 2020-12 document that VALIDATES a real encoding of the
// REST DTO it reuses (Session, ListSessionsResponse, ModelCatalog),
// including every transitively-$ref'd $def (Session -> AutomationReposElem;
// ModelCatalog -> ModelCatalogProvider -> ModelCatalogModel ->
// ModelCatalogCost).
func TestToolOutputSchemas_ValidateRealBodies(t *testing.T) {
	sessionBody := `{
		"id": "5b1c1e2e-6b1a-4b1a-9b1a-6b1a4b1a9b1a",
		"title": null,
		"status": "created",
		"failureReason": null,
		"archived": false,
		"spawnSource": "web",
		"createdBy": null,
		"createdAt": "2026-01-01T00:00:00Z",
		"updatedAt": "2026-01-01T00:00:00Z",
		"repos": [{"name": "repo", "url": "https://example.com/repo", "branch": null}],
		"sandboxStatus": null,
		"buildModelId": null,
		"buildEffort": null
	}`

	modelCatalogBody := `{
		"providers": [{
			"id": "openai",
			"models": [{
				"id": "gpt-5",
				"name": "GPT-5",
				"contextWindow": 128000,
				"toolCall": true,
				"reasoning": true,
				"variants": ["low", "high"],
				"cost": {"input": 1.5, "output": 3.0, "cacheRead": null, "cacheWrite": null}
			}]
		}]
	}`

	tests := []struct {
		outputDef string
		body      string
	}{
		{"Session", sessionBody},
		{"ListSessionsResponse", `{"sessions": [` + sessionBody + `]}`},
		{"ModelCatalog", modelCatalogBody},
	}

	for _, tt := range tests {
		t.Run(tt.outputDef, func(t *testing.T) {
			bundled, err := bundleOutputSchema(tt.outputDef)
			if err != nil {
				t.Fatalf("bundleOutputSchema(%q) error = %v", tt.outputDef, err)
			}

			bundledJSON, err := json.Marshal(bundled)
			if err != nil {
				t.Fatalf("marshal bundled schema: %v", err)
			}

			c := jsonschema.NewCompiler()
			c.AssertFormat()
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(bundledJSON))
			if err != nil {
				t.Fatalf("decode bundled schema: %v", err)
			}
			const resourceURL = "mem://bundled.json"
			if err := c.AddResource(resourceURL, doc); err != nil {
				t.Fatalf("add bundled schema resource: %v", err)
			}
			sch, err := c.Compile(resourceURL)
			if err != nil {
				t.Fatalf("compile bundled schema: %v", err)
			}

			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader([]byte(tt.body)))
			if err != nil {
				t.Fatalf("decode instance: %v", err)
			}
			if err := sch.Validate(inst); err != nil {
				t.Errorf("bundled %q schema did not validate a real body: %v\nschema: %s\nbody: %s", tt.outputDef, err, bundledJSON, tt.body)
			}
		})
	}
}

// TestBundleOutputSchema_TransitiveClosure pins the bundler's own
// transitive-closure guarantee explicitly: bundling "ModelCatalog" pulls
// in ModelCatalogProvider, ModelCatalogModel, AND ModelCatalogCost (a
// three-hop chain), not merely its own direct $ref.
func TestBundleOutputSchema_TransitiveClosure(t *testing.T) {
	bundled, err := bundleOutputSchema("ModelCatalog")
	if err != nil {
		t.Fatalf("bundleOutputSchema error = %v", err)
	}
	defs, ok := bundled["$defs"].(map[string]any)
	if !ok {
		t.Fatalf("bundled[$defs] = %#v, want a map", bundled["$defs"])
	}
	for _, want := range []string{"ModelCatalog", "ModelCatalogProvider", "ModelCatalogModel", "ModelCatalogCost"} {
		if _, ok := defs[want]; !ok {
			t.Errorf("bundled $defs missing %q; got keys %v", want, defKeys(defs))
		}
	}
	if len(defs) != 4 {
		t.Errorf("bundled $defs has %d entries (%v), want exactly 4", len(defs), defKeys(defs))
	}
}

func defKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestListModelsOutputSchema_ValidatesRealCatalogBody closes round 2
// review findings N6/N10: TestToolOutputSchemas_ValidateRealBodies above
// validates only a HAND-WRITTEN ModelCatalog body, in which every model
// already carries a non-empty "variants" array -- it cannot notice
// httpapi.GetModelCatalog()'s own nonNilStrings fix (round 1 finding M5)
// ever regressing, since a body that never exercises the empty-variants
// case validates identically whether or not that fix is even present.
// This test instead drives the REAL httpapi.GetModelCatalog() handler --
// the exact function narvi_list_models' own twin invokes -- directly
// (authorize's own check is pure role logic, no store, so no Postgres is
// needed here), and validates the REAL body it writes against the SAME
// bundled schema narvi_list_models advertises as its OutputSchema. The
// real catalog (internal/app/modelcatalog's own compiled-in snapshot)
// has multiple models with zero variants (google's gemini-2.0-flash and
// others), so a regression back to `Variants: m.Variants` (letting a nil
// slice encode as JSON null) fails this test where the hand-written-body
// test above cannot.
func TestListModelsOutputSchema_ValidatesRealCatalogBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	req = req.WithContext(platform.WithUser(req.Context(), testUser))
	rec := httptest.NewRecorder()
	httpapi.GetModelCatalog()(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("httpapi.GetModelCatalog(): status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	body := rec.Body.Bytes()

	if strings.Contains(string(body), `"variants":null`) {
		t.Errorf("the real model catalog body contains \"variants\":null -- round 1 finding M5's fix (nonNilStrings, httpapi/modelcatalog.go) has regressed:\n%s", body)
	}

	bundled, err := bundleOutputSchema("ModelCatalog")
	if err != nil {
		t.Fatalf("bundleOutputSchema(%q) error = %v", "ModelCatalog", err)
	}
	bundledJSON, err := json.Marshal(bundled)
	if err != nil {
		t.Fatalf("marshal bundled schema: %v", err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(bundledJSON))
	if err != nil {
		t.Fatalf("decode bundled schema: %v", err)
	}
	const resourceURL = "mem://bundled-real-catalog.json"
	if err := c.AddResource(resourceURL, doc); err != nil {
		t.Fatalf("add bundled schema resource: %v", err)
	}
	sch, err := c.Compile(resourceURL)
	if err != nil {
		t.Fatalf("compile bundled schema: %v", err)
	}

	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("decode real catalog body: %v", err)
	}
	if err := sch.Validate(inst); err != nil {
		t.Errorf("the real model catalog body does not validate against narvi_list_models' own advertised outputSchema: %v\nbody: %s", err, body)
	}
}

// TestCompileInputSchemas_RealDefsSucceed and
// TestCompileInputSchemas_UnknownDefFails pin compileInputSchemas' own two
// documented outcomes (schemas.go): every one of toolInputDefs()'s real
// names compiles into a non-nil *jsonschema.Schema, and a name with no
// matching $def in rest/v1/dtos.schema.json returns an error rather than
// panicking or silently omitting an entry -- the exact error NewHandler
// now surfaces as a BOOT failure (round 2 review of PR #324, findings
// N1/N3/N4: a schema defect must fail boot, never a live request).
func TestCompileInputSchemas_RealDefsSucceed(t *testing.T) {
	defs := toolInputDefs()
	schemas, err := compileInputSchemas(defs)
	if err != nil {
		t.Fatalf("compileInputSchemas(%v) error = %v", defs, err)
	}
	if len(schemas) != len(defs) {
		t.Fatalf("len(schemas) = %d, want %d", len(schemas), len(defs))
	}
	for _, name := range defs {
		if schemas[name] == nil {
			t.Errorf("schemas[%q] = nil, want a compiled *jsonschema.Schema", name)
		}
	}
}

func TestCompileInputSchemas_UnknownDefFails(t *testing.T) {
	_, err := compileInputSchemas([]string{"ThisDefDoesNotExist"})
	if err == nil {
		t.Fatal("compileInputSchemas with an unknown $def name: error = nil, want an error")
	}
}

// jsonschemaImportPath is the JSON Schema compiler package's own IMPORT
// PATH -- TestNoSecondJSONSchemaCompiler below resolves every identifier
// against THIS, via go/types, never against the literal identifier
// "jsonschema" a file happens to spell an import with (round 4 review of
// PR #324, findings S3/S5: an aliased import made an identifier-text
// check blind).
const jsonschemaImportPath = "github.com/santhosh-tekuri/jsonschema/v6"

// eagerCompileFile and eagerCompileFunc name compileInputSchemas' own
// home: the one top-level function in this package's production code
// allowed to reference jsonschema.NewCompiler, or the Compile/MustCompile
// method of a jsonschema.Compiler (see TestNoSecondJSONSchemaCompiler).
const (
	eagerCompileFile = "schemas.go"
	eagerCompileFunc = "compileInputSchemas"
)

// shippedBuildContexts are the build-tag sets this repo compiles
// production code under (Makefile): the default context (`go build`,
// `make test`, `make lint`) and web_assets (`make dist`, which builds the
// release binary with `go build -tags web_assets`, and `make
// lint-web-assets`). packages.Load type-checks only the files its own
// build context selects -- a file under `//go:build web_assets` is absent
// from a default-context load while still shipping in the release binary
// (round 5 review of PR #324, finding T3) -- so
// TestNoSecondJSONSchemaCompiler loads this package once per entry here.
var shippedBuildContexts = []struct {
	name       string
	buildFlags []string
}{
	{name: "default"},
	{name: "web_assets", buildFlags: []string{"-tags=web_assets"}},
}

// jsonschemaRefSite is one identifier TestNoSecondJSONSchemaCompiler
// found resolving to jsonschema.NewCompiler or to a jsonschema.Compiler
// Compile/MustCompile method.
type jsonschemaRefSite struct {
	file  string // base name
	line  int
	fn    string // enclosing top-level func declaration, "" at package level
	eager bool   // inside compileInputSchemas in schemas.go
}

func (s jsonschemaRefSite) String() string {
	fn := s.fn
	if fn == "" {
		fn = "<package level>"
	}
	return fmt.Sprintf("%s:%d in %s", s.file, s.line, fn)
}

// TestNoSecondJSONSchemaCompiler is a cheap, STATIC early warning for the
// most common shapes a reintroduced lazy schema compile takes. The defect
// it watches for is round 2 review of PR #324, findings N1/N3/N4: input
// schemas compiled lazily, per request, on a shared *jsonschema.Compiler
// that has no locking -- the first concurrent tools/call after boot then
// dies with "fatal error: concurrent map writes", which no recover
// catches.
//
// This test is NOT the guarantee. The guarantee is behavioural:
// TestToolHandler_ValidatesOnlyAgainstTheInjectedSchemaMap
// (toolhandler_test.go) proves toolHandler decides whether arguments are
// valid from the eagerly compiled map it is handed and from nothing else,
// so a lazy path that toolHandler validates against fails it whatever
// its shape (round 5 review, finding T1: three different shapes passed a
// static check alone).
//
// What this test checks, for every non-_test.go file of this package in
// every shippedBuildContexts entry, resolving identifiers with go/types
// (pkg.TypesInfo.Uses) rather than by spelling:
//
//  1. Every REFERENCE to jsonschema.NewCompiler -- a call, or the function
//     taken as a value -- is counted. There must be exactly one, and it
//     must sit lexically inside compileInputSchemas in schemas.go.
//  2. Every REFERENCE to jsonschema.Compiler's Compile or MustCompile
//     method -- a call, a method value (`f := c.Compile`), a method
//     expression, or a method promoted through an embedded field -- must
//     sit lexically inside compileInputSchemas in schemas.go.
//  3. A dot or blank import of the jsonschema package is refused.
//  4. A non-_test.go file that no shippedBuildContexts entry compiles
//     (pkg.IgnoredFiles in every context) fails the test: nothing here
//     would ever inspect it. A GOOS/GOARCH-specific file counts too, on
//     a host it does not match -- deliberately loud, since none exists in
//     this package today.
//
// What it does NOT catch, by construction (the behavioural test does):
//
//   - a Compile called through a package-local interface -- that
//     reference resolves to the interface's own method, not to
//     jsonschema.Compiler's;
//   - a lazy path that goes through compileInputSchemas itself (called
//     per request, or handed a shared compiler), because every reference
//     then sits inside the allowed function;
//   - reflection or any other dynamic dispatch.
func TestNoSecondJSONSchemaCompiler(t *testing.T) {
	// Keyed by the reference's own file:line:column, so a file compiled
	// in more than one build context is counted once.
	newCompilerRefs := map[string]jsonschemaRefSite{}
	badCompileRefs := map[string]jsonschemaRefSite{}
	compiled := map[string]bool{}      // absolute path -> compiled in some shipped context
	ignoredBy := map[string][]string{} // absolute path -> contexts that ignored it

	for _, bc := range shippedBuildContexts {
		pkg := loadProductionPackage(t, bc.name, bc.buildFlags)
		for _, f := range pkg.GoFiles {
			compiled[f] = true
		}
		for _, f := range pkg.IgnoredFiles {
			if strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, "_test.go") {
				ignoredBy[f] = append(ignoredBy[f], bc.name)
			}
		}

		for _, file := range pkg.Syntax {
			base := filepath.Base(pkg.Fset.Position(file.Pos()).Filename)

			// A dot import lets NewCompiler appear as a bare identifier
			// and a blank import serves this package no purpose beyond
			// obscuring a real one alongside it; neither is allowed.
			for _, imp := range file.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil || path != jsonschemaImportPath {
					continue
				}
				if imp.Name != nil && (imp.Name.Name == "." || imp.Name.Name == "_") {
					t.Errorf("[%s] %s: %s is imported as %q -- a dot or blank import of the JSON Schema compiler package is never allowed in this package's production code", bc.name, base, jsonschemaImportPath, imp.Name.Name)
				}
			}

			ast.Inspect(file, func(n ast.Node) bool {
				ident, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				fn, ok := pkg.TypesInfo.Uses[ident].(*types.Func)
				if !ok {
					return true
				}
				isNewCompiler := isJSONSchemaNewCompiler(fn)
				isCompile := isJSONSchemaCompileMethod(fn)
				if !isNewCompiler && !isCompile {
					return true
				}
				pos := pkg.Fset.Position(ident.Pos())
				decl := enclosingFuncDecl(file, ident.Pos())
				site := jsonschemaRefSite{file: base, line: pos.Line, eager: base == eagerCompileFile && decl != nil && decl.Recv == nil && decl.Name.Name == eagerCompileFunc}
				if decl != nil {
					site.fn = decl.Name.Name
				}
				switch {
				case isNewCompiler:
					newCompilerRefs[pos.String()] = site
				case !site.eager:
					badCompileRefs[pos.String()] = site
				}
				return true
			})
		}
	}

	contextNames := make([]string, len(shippedBuildContexts))
	for i, bc := range shippedBuildContexts {
		contextNames[i] = bc.name
	}
	var uncovered []string
	for f, contexts := range ignoredBy {
		if !compiled[f] {
			uncovered = append(uncovered, fmt.Sprintf("%s (ignored by: %s)", filepath.Base(f), strings.Join(contexts, ", ")))
		}
	}
	sort.Strings(uncovered)
	if len(uncovered) > 0 {
		t.Errorf("production (non-_test.go) file(s) that no shipped build context (%s) compiles: %v -- this test never inspects them; add the build context that selects them to shippedBuildContexts, or make them _test.go files", strings.Join(contextNames, ", "), uncovered)
	}

	newSites := sortedRefSites(newCompilerRefs)
	switch {
	case len(newSites) != 1:
		t.Errorf("found %d reference(s) to %s.NewCompiler (%v), want exactly 1, inside %s's own %s -- every tool's input schema is compiled EAGERLY there, at NewHandler/boot time (round 2 N1/N3/N4); a second compiler is the shape a reintroduced lazy compile takes", len(newSites), jsonschemaImportPath, newSites, eagerCompileFile, eagerCompileFunc)
	case !newSites[0].eager:
		t.Errorf("the one reference to %s.NewCompiler is at %v, want it inside %s's own %s -- a compiler built anywhere else (a helper, a package-level var) can be shared with a lazy, per-request compile path (round 5 review, finding T1)", jsonschemaImportPath, newSites[0], eagerCompileFile, eagerCompileFunc)
	}
	if badSites := sortedRefSites(badCompileRefs); len(badSites) > 0 {
		t.Errorf("found reference(s) to a %s.Compiler Compile/MustCompile method outside %s's own %s: %v -- every compile must happen EAGERLY, at boot, in that one function (round 2 N1/N3/N4)", jsonschemaImportPath, eagerCompileFile, eagerCompileFunc, badSites)
	}
}

// loadProductionPackage type-checks this package's own production files
// (no _test.go files: a test's own throwaway compiler is not a production
// decision point, exactly tools/lint/narvichecks/mcpimportban's own
// exemption) in the build context buildFlags selects.
func loadProductionPackage(t *testing.T, contextName string, buildFlags []string) *packages.Package {
	t.Helper()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax,
		BuildFlags: buildFlags,
		Tests:      false,
	}
	pkgs, err := packages.Load(cfg, ".")
	if err != nil {
		t.Fatalf("[%s] packages.Load(\".\"): %v", contextName, err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		t.Fatalf("[%s] packages.Load(\".\") reported %d error(s) loading this package (see stderr above)", contextName, n)
	}
	if len(pkgs) != 1 {
		t.Fatalf("[%s] packages.Load(\".\") returned %d packages, want exactly 1", contextName, len(pkgs))
	}
	if len(pkgs[0].Syntax) == 0 {
		t.Fatalf("[%s] packages.Load(\".\") found no source files -- this test must run from its own package directory (go test's own documented working-directory contract)", contextName)
	}
	return pkgs[0]
}

// isJSONSchemaNewCompiler reports whether fn is the package-level
// jsonschema.NewCompiler function.
func isJSONSchemaNewCompiler(fn *types.Func) bool {
	return fn.Signature().Recv() == nil && fn.Pkg() != nil &&
		fn.Pkg().Path() == jsonschemaImportPath && fn.Name() == "NewCompiler"
}

// isJSONSchemaCompileMethod reports whether fn is jsonschema.Compiler's
// own Compile or MustCompile method (pointer or value receiver).
func isJSONSchemaCompileMethod(fn *types.Func) bool {
	recv := fn.Signature().Recv()
	if recv == nil || (fn.Name() != "Compile" && fn.Name() != "MustCompile") {
		return false
	}
	recvType := recv.Type()
	if ptr, ok := recvType.(*types.Pointer); ok {
		recvType = ptr.Elem()
	}
	named, ok := recvType.(*types.Named)
	return ok && named.Obj().Pkg() != nil &&
		named.Obj().Pkg().Path() == jsonschemaImportPath && named.Obj().Name() == "Compiler"
}

// enclosingFuncDecl returns the top-level func declaration lexically
// containing pos, or nil when pos is at package level (e.g. inside a
// package-level var's own func literal -- exactly the shape a
// `sync.OnceValues(func() {...})` lazy cache takes).
func enclosingFuncDecl(file *ast.File, pos token.Pos) *ast.FuncDecl {
	for _, decl := range file.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Pos() <= pos && pos <= fd.End() {
			return fd
		}
	}
	return nil
}

// sortedRefSites returns m's values ordered by file, then line.
func sortedRefSites(m map[string]jsonschemaRefSite) []jsonschemaRefSite {
	sites := make([]jsonschemaRefSite, 0, len(m))
	for _, s := range m {
		sites = append(sites, s)
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].file != sites[j].file {
			return sites[i].file < sites[j].file
		}
		return sites[i].line < sites[j].line
	})
	return sites
}
