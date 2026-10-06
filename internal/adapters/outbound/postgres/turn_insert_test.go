package postgres_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// turnStoreFile is the one non-test file allowed to name
// sqlcgen.CreateTurnParams in a signature: the TurnStore's own Create (test
// fixtures), CreateAndArmDispatch and LockedTurnCreator.CreateLockedTurn.
// The sqlc CreateTurn query takes a CreateTurnParams, so no other file can
// call it without naming the type.
const turnStoreFile = "internal/adapters/outbound/postgres/turn_store.go"

// insertIntoTurns matches SQL that inserts into the turns table, quoted or
// schema-qualified or not; sqlComment strips a SQL line comment first, so
// prose that merely names such a statement does not match.
var (
	insertIntoTurns = regexp.MustCompile(`(?i)\binsert\s+into\s+("?public"?\s*\.\s*)?"?turns"?(\s|\(|$)`)
	sqlComment      = regexp.MustCompile(`--[^\n]*`)
)

// insertsIntoTurns reports whether sql, its line comments stripped,
// inserts into turns.
func insertsIntoTurns(sql string) bool {
	return insertIntoTurns.MatchString(sqlComment.ReplaceAllString(sql, ""))
}

// armingCallees are the calls a production CreateTurnParams literal may be
// handed to: CreateAndArmDispatch, and CreateLockedTurn, which runs it in a
// transaction of its own.
var armingCallees = map[string]bool{"CreateAndArmDispatch": true, "CreateLockedTurn": true}

// TestEveryTurnInsertArmsTheDispatchTimer keeps technical plan §2's durable
// dispatch trigger whole at the source: every turn production code creates
// arms the session's dispatch timer in the same transaction, because it
// goes through TurnStore.CreateAndArmDispatch (directly, or through
// LockedTurnCreator.CreateLockedTurn). In every non-test Go file of the
// module (generated sqlcgen code aside), a sqlcgen.CreateTurnParams value
// may appear only as a composite literal handed straight to one of those
// two calls, and the type may be named otherwise only in turn_store.go's
// own signatures and in an interface method of one of those two names (the
// release composition review's seam). So a path that inserts a turn
// through the plain Create -- one autocommit insert, no timer -- or calls
// the sqlc CreateTurn query itself fails here, however the params value was
// built: a literal handed to anything else, or the type named in a
// variable, a conversion or a helper's signature. A Go string literal that
// inserts into turns fails too (raw SQL through Exec or QueryRow), and
// TestNoOtherSQLInsertsTurns covers the sqlc queries and the migrations.
// Neither sees SQL assembled at run time from pieces.
func TestEveryTurnInsertArmsTheDispatchTimer(t *testing.T) {
	t.Parallel()

	root := moduleRootForTest(t)
	fset := token.NewFileSet()
	arming := 0
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
			arming += checkTurnInserts(t, fset, rel, file)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	// The seven places a turn is inserted (technical plan §43.20):
	// createTurnLocked, session creation, plan approval, the workflow
	// engine's advance, the re-review debounce's handler, an owed review
	// request's re-run, the composition review. Fewer means the scan no
	// longer sees them.
	if arming < 7 {
		t.Fatalf("found %d CreateTurnParams literals handed to an arming call, want at least 7: the scan is broken", arming)
	}
}

// checkTurnInserts reports every mention of CreateTurnParams file does not
// own under the rule TestEveryTurnInsertArmsTheDispatchTimer states, and
// returns how many literals it found handed to an arming call.
func checkTurnInserts(t *testing.T, fset *token.FileSet, rel string, file *ast.File) int {
	t.Helper()
	approved := map[token.Pos]bool{}
	approveIdents := func(n ast.Node) {
		if n == nil {
			return
		}
		ast.Inspect(n, func(m ast.Node) bool {
			if id, ok := m.(*ast.Ident); ok && id.Name == "CreateTurnParams" {
				approved[id.Pos()] = true
			}
			return true
		})
	}

	arming := 0
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			if rel == turnStoreFile {
				approveIdents(n.Type)
			}
		case *ast.InterfaceType:
			for _, m := range n.Methods.List {
				for _, name := range m.Names {
					if armingCallees[name.Name] {
						approveIdents(m.Type)
					}
				}
			}
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if !armingCallees[sel.Sel.Name] {
				return true
			}
			for _, arg := range n.Args {
				if lit, ok := arg.(*ast.CompositeLit); ok && namesCreateTurnParams(lit.Type) {
					approveIdents(lit.Type)
					arming++
				}
			}
		}
		return true
	})

	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			if n.Name == "CreateTurnParams" && !approved[n.Pos()] {
				t.Errorf("%s: a CreateTurnParams value outside CreateAndArmDispatch/CreateLockedTurn: every turn production code creates must arm its session's dispatch timer in the same transaction (technical plan §2) -- hand the literal straight to TurnStore.CreateAndArmDispatch", fset.Position(n.Pos()))
			}
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				return true
			}
			if sql, err := strconv.Unquote(n.Value); err == nil && insertsIntoTurns(sql) {
				t.Errorf("%s: raw SQL inserting into turns: a turn it creates arms no dispatch timer (technical plan §2) -- create it through TurnStore.CreateAndArmDispatch", fset.Position(n.Pos()))
			}
		}
		return true
	})
	return arming
}

// TestNoOtherSQLInsertsTurns is the SQL half of
// TestEveryTurnInsertArmsTheDispatchTimer: in the sqlc queries, the one
// statement that inserts into turns is CreateTurn -- the query
// TurnStore.Create and CreateAndArmDispatch run, and nothing else calls
// with a CreateTurnParams value -- and no migration inserts into turns. A
// second sqlc query inserting turns would get params of its own, which the
// Go scan cannot see, and a turn a migration inserts would have no
// dispatch timer.
func TestNoOtherSQLInsertsTurns(t *testing.T) {
	t.Parallel()

	root := moduleRootForTest(t)
	queries, err := filepath.Glob(filepath.Join(root, "internal", "adapters", "outbound", "postgres", "queries", "*.sql"))
	if err != nil || len(queries) == 0 {
		t.Fatalf("list the sqlc queries: %v (%d files)", err, len(queries))
	}
	createTurn := 0
	for _, path := range queries {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// One block per "-- name:" header; the text before the first
		// header is the file's own prose.
		blocks := strings.Split(string(body), "-- name:")
		for i, block := range blocks {
			if !insertsIntoTurns(block) {
				continue
			}
			name := ""
			if i > 0 {
				name = strings.Fields(block)[0]
			}
			if name == "CreateTurn" {
				createTurn++
				continue
			}
			t.Errorf("%s: query %q inserts into turns: only CreateTurn may, reached through TurnStore.CreateAndArmDispatch, which arms the session's dispatch timer (technical plan §2)", path, name)
		}
	}
	if createTurn != 1 {
		t.Fatalf("found %d CreateTurn queries inserting into turns, want 1: the scan is broken", createTurn)
	}

	migrations, err := filepath.Glob(filepath.Join(root, "migrations", "*.sql"))
	if err != nil || len(migrations) == 0 {
		t.Fatalf("list the migrations: %v (%d files)", err, len(migrations))
	}
	for _, path := range migrations {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if insertsIntoTurns(string(body)) {
			t.Errorf("%s: a migration inserts into turns: those turns have no dispatch timer (technical plan §2)", path)
		}
	}
}

// namesCreateTurnParams reports whether a composite literal's type is
// CreateTurnParams, bare or package-qualified.
func namesCreateTurnParams(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name == "CreateTurnParams"
	case *ast.SelectorExpr:
		return e.Sel.Name == "CreateTurnParams"
	default:
		return false
	}
}

// moduleRootForTest walks up from the package directory to go.mod.
func moduleRootForTest(t *testing.T) string {
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
