package sessionactor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// sandboxStatusParams are the params of the four queries that write a
// sandbox's status or generation, one per sandboxWriter method. CreateSandbox
// writes them too, through SandboxStore.Create, which takes no params and
// no production code calls (TestSandboxStatusWritesGoThroughTheRecorder
// checks that as well).
var sandboxStatusParams = map[string]bool{
	"UpdateSandboxStatusParams":          true,
	"UpdateSandboxStatusToSuspectParams": true,
	"RecoverSandboxFromSuspectParams":    true,
	"UpsertSandboxForSpawnParams":        true,
}

// sandboxStatusQueries are the sqlc queries that write sandboxes.status or
// sandboxes.gen: the four above, and CreateSandbox.
var sandboxStatusQueries = map[string]bool{
	"UpdateSandboxStatus":          true,
	"UpdateSandboxStatusToSuspect": true,
	"RecoverSandboxFromSuspect":    true,
	"UpsertSandboxForSpawn":        true,
	"CreateSandbox":                true,
}

// sandboxStatusOwners may name the params in a signature: the store that
// runs the queries, and the recorder that wraps it.
var sandboxStatusOwners = map[string]bool{
	"internal/adapters/outbound/postgres/sandbox_store.go": true,
	"internal/app/sessionactor/sandboxstatus.go":           true,
}

// TestSandboxStatusWritesGoThroughTheRecorder keeps the sandbox_status
// event whole at the source (technical plan §3.2, §6.2): an open page
// learns a sandbox's new status only from that event, which transact
// appends for a write made through sandboxWrites. In every non-test Go file
// of the module (generated sqlcgen code aside), the params of a query that
// writes a sandbox's status or generation may appear only as a composite
// literal handed straight to a sandboxWrites(...) method, or in the
// signatures of the store and the recorder; a production call of
// SandboxStore.Create or of the sqlc CreateSandbox fails too, as does a Go
// string literal that updates or inserts into sandboxes. So a write that
// would change the status without the event -- the store called directly,
// a helper taking the params, raw SQL -- fails here.
// TestSandboxStatusQueriesAreTheRecordedOnes covers the sqlc queries.
func TestSandboxStatusWritesGoThroughTheRecorder(t *testing.T) {
	t.Parallel()

	root := sandboxStatusModuleRoot(t)
	fset := token.NewFileSet()
	recorded := 0
	for _, top := range []string{"internal", "controlplane", "cmd", "extension"} {
		dir := filepath.Join(root, top)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "node_modules" || d.Name() == "sqlcgen" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			recorded += checkSandboxStatusWrites(t, fset, rel, file)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	// The fourteen writes the session actor makes (dispatch, sandbox
	// events, stop, timers). Fewer means the scan no longer sees them.
	if recorded < 14 {
		t.Fatalf("found %d sandbox status writes through sandboxWrites, want at least 14: the scan is broken", recorded)
	}
}

// rawSandboxesWrite matches SQL that updates or inserts into sandboxes.
var rawSandboxesWrite = regexp.MustCompile(`(?i)\b(update|insert\s+into)\s+("?public"?\s*\.\s*)?"?sandboxes"?(\s|\(|$)`)

func checkSandboxStatusWrites(t *testing.T, fset *token.FileSet, rel string, file *ast.File) int {
	t.Helper()
	approved := map[token.Pos]bool{}
	approve := func(n ast.Node) {
		if n == nil {
			return
		}
		ast.Inspect(n, func(m ast.Node) bool {
			if id, ok := m.(*ast.Ident); ok && sandboxStatusParams[id.Name] {
				approved[id.Pos()] = true
			}
			return true
		})
	}

	recorded := 0
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			if sandboxStatusOwners[rel] {
				approve(n.Type)
			}
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch {
			case isSandboxWritesCall(sel.X):
				for _, arg := range n.Args {
					if lit, ok := arg.(*ast.CompositeLit); ok {
						approve(lit.Type)
						recorded++
					}
				}
			case sel.Sel.Name == "CreateSandbox" && isSQLCQueries(sel.X) && rel != "internal/adapters/outbound/postgres/sandbox_store.go":
				t.Errorf("%s: the CreateSandbox query called outside the store: a sandbox it creates is never reported to an open page -- create it through sandboxWrites(tx).UpsertForSpawn", fset.Position(n.Pos()))
			case sel.Sel.Name == "Create" && endsInSandboxStore(sel.X):
				t.Errorf("%s: SandboxStore.Create called in production code: a sandbox it creates is never reported to an open page -- create it through sandboxWrites(tx).UpsertForSpawn", fset.Position(n.Pos()))
			}
		}
		return true
	})

	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			if sandboxStatusParams[n.Name] && !approved[n.Pos()] {
				t.Errorf("%s: a %s value outside sandboxWrites: a sandbox status written this way reaches no open page (technical plan §3.2) -- hand the literal straight to a.sandboxWrites(tx)", fset.Position(n.Pos()), n.Name)
			}
		case *ast.BasicLit:
			if n.Kind == token.STRING && rawSandboxesWrite.MatchString(n.Value) {
				t.Errorf("%s: raw SQL writing sandboxes: a status it changes reaches no open page (technical plan §3.2) -- write it through the sandbox store and sandboxWrites", fset.Position(n.Pos()))
			}
		}
		return true
	})
	return recorded
}

// isSandboxWritesCall reports whether expr is a call of a method named
// sandboxWrites, as in a.sandboxWrites(tx).
func isSandboxWritesCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "sandboxWrites"
}

// isSQLCQueries reports whether expr reads like a *sqlcgen.Queries, as the
// stores name theirs (s.q, q, queries) -- never a sandbox provider, whose
// CreateSandbox makes the cloud sandbox, not the row.
func isSQLCQueries(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name == "q" || e.Name == "queries"
	case *ast.SelectorExpr:
		return e.Sel.Name == "q" || e.Sel.Name == "queries"
	case *ast.CallExpr:
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "WithTx" {
			return isSQLCQueries(sel.X)
		}
	}
	return false
}

// endsInSandboxStore reports whether expr reads like a SandboxStore: a
// selector or identifier named sandbox or sandboxes, or a WithTx call on
// one.
func endsInSandboxStore(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name == "sandbox" || e.Name == "sandboxes"
	case *ast.SelectorExpr:
		return e.Sel.Name == "sandbox" || e.Sel.Name == "sandboxes"
	case *ast.CallExpr:
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "WithTx" {
			return endsInSandboxStore(sel.X)
		}
	}
	return false
}

// setClause captures what a SET assigns, up to its WHERE, RETURNING or end.
var (
	setClause       = regexp.MustCompile(`(?is)\bSET\b(.*?)(\bWHERE\b|\bRETURNING\b|;|$)`)
	assignsStatus   = regexp.MustCompile(`(?i)(^|[\s,])(status|gen)\s*=`)
	sqlLineComment  = regexp.MustCompile(`--[^\n]*`)
	writesSandboxes = regexp.MustCompile(`(?i)\b(update\s+sandboxes|insert\s+into\s+sandboxes)\b`)
)

// TestSandboxStatusQueriesAreTheRecordedOnes is the SQL half of
// TestSandboxStatusWritesGoThroughTheRecorder: the sqlc queries that write
// sandboxes.status or sandboxes.gen are exactly the ones sandboxWriter
// wraps, plus CreateSandbox. A new one would get params of its own, which
// the Go scan cannot see: route it through sandboxWrites, then add it here.
func TestSandboxStatusQueriesAreTheRecordedOnes(t *testing.T) {
	t.Parallel()

	root := sandboxStatusModuleRoot(t)
	queries, err := filepath.Glob(filepath.Join(root, "internal", "adapters", "outbound", "postgres", "queries", "*.sql"))
	if err != nil || len(queries) == 0 {
		t.Fatalf("list the sqlc queries: %v (%d files)", err, len(queries))
	}
	found := map[string]bool{}
	for _, path := range queries {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, block := range strings.Split(string(body), "-- name:") {
			if i == 0 {
				continue
			}
			name := strings.Fields(block)[0]
			sql := sqlLineComment.ReplaceAllString(block, "")
			if !writesSandboxes.MatchString(sql) {
				continue
			}
			writesStatus := regexp.MustCompile(`(?i)\binsert\s+into\s+sandboxes\b`).MatchString(sql)
			for _, m := range setClause.FindAllStringSubmatch(sql, -1) {
				if assignsStatus.MatchString(m[1]) {
					writesStatus = true
				}
			}
			if !writesStatus {
				continue
			}
			if !sandboxStatusQueries[name] {
				t.Errorf("%s: query %q writes a sandbox's status or generation: a page learns that only from the sandbox_status event, so route its store method through sandboxWriter (sandboxstatus.go) and list it here", path, name)
				continue
			}
			found[name] = true
		}
	}
	for name := range sandboxStatusQueries {
		if !found[name] {
			t.Errorf("query %q no longer writes a sandbox's status or generation, or is gone: the scan is broken, or the list is stale", name)
		}
	}
}

// sandboxStatusModuleRoot walks up from the package directory to go.mod.
func sandboxStatusModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the package directory")
		}
		dir = parent
	}
}
