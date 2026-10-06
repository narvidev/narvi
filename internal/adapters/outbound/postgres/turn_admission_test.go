package postgres_test

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

// This file keeps technical plan §40.1's session guard whole at the source:
// every turn production code creates is admitted by the guard
// (TestEveryTurnInsertIsAdmitted), an admission for an existing session is
// minted only by the guard's own decision (TestDecideOnlyFromTurnguard), and
// the admission of a session's first turn without a read only where that
// session is created (TestAdmitNewSessionOnlyInCreateSessionOnTx). With
// TestEveryTurnInsertArmsTheDispatchTimer, which keeps every turn insert on
// CreateAndArmDispatch, no new path that creates a turn can skip the guard.
// TestEveryTurnCostWriterTargetsAProcessingTurn pins the fact the guard's
// read rests on: only a dispatched turn carries cost.

// productionGoFiles calls fn for every non-test Go file of the module under
// internal, controlplane, cmd and extension, sqlc's generated code aside,
// with its path relative to the module root.
func productionGoFiles(t *testing.T, fn func(rel string, fset *token.FileSet, file *ast.File)) {
	t.Helper()
	root := moduleRootForTest(t)
	fset := token.NewFileSet()
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
			file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			fn(filepath.ToSlash(rel), fset, file)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
}

// admittingCalls are the calls an admission may come from in the function
// that creates a turn: the session guard's own (turnguard's Guard.Admit and
// Bound.Admit), the helpers that wrap it (the session actor's
// admitAutomaticTurn, the workflow engine's admitNextAttempt), and, for a
// session created in the same transaction, sessionguard.AdmitNewSession.
var admittingCalls = map[string]bool{
	"Admit":              true,
	"admitAutomaticTurn": true,
	"admitNextAttempt":   true,
	"AdmitNewSession":    true,
}

// TestEveryTurnInsertIsAdmitted: in every non-test Go file (outside
// turn_store.go, whose CreateLockedTurn hands its own admission on), every
// call of a function that takes a sessionguard.Admission --
// CreateAndArmDispatch, and each helper that passes one on to it -- is
// passed either the result of an admitting call (admittingCalls) made in
// the calling function, or a sessionguard.Admission parameter of the
// calling function, whose own callers this same rule holds. Every call of
// CreateLockedTurn passes a guard's Admitter. So a path that inserts a turn
// with an admission from nowhere -- the zero value, or one carried in
// through a variable from another decision -- fails here, and at run time
// too, since the store refuses both (ErrTurnNotAdmitted).
func TestEveryTurnInsertIsAdmitted(t *testing.T) {
	t.Parallel()

	type parsed struct {
		rel  string
		fset *token.FileSet
		file *ast.File
	}
	var files []parsed
	productionGoFiles(t, func(rel string, fset *token.FileSet, file *ast.File) {
		files = append(files, parsed{rel, fset, file})
	})

	// The functions that take an admission, and at which argument.
	takesAdmission := map[string]int{}
	for _, f := range files {
		for _, decl := range f.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Type.Params == nil {
				continue
			}
			i := 0
			for _, field := range fn.Type.Params.List {
				n := len(field.Names)
				if n == 0 {
					n = 1
				}
				if sel, ok := field.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Admission" {
					takesAdmission[fn.Name.Name] = i
				}
				i += n
			}
		}
	}
	if _, ok := takesAdmission["CreateAndArmDispatch"]; !ok {
		t.Fatal("CreateAndArmDispatch takes no sessionguard.Admission: the scan is broken")
	}

	sites := 0
	for _, f := range files {
		if f.rel == turnStoreFile {
			continue
		}
		for _, decl := range f.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			admitted := map[string]bool{}
			if fn.Type.Params != nil {
				for _, field := range fn.Type.Params.List {
					if sel, ok := field.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Admission" {
						for _, name := range field.Names {
							admitted[name.Name] = true
						}
					}
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok || len(assign.Rhs) != 1 {
					return true
				}
				if call, ok := assign.Rhs[0].(*ast.CallExpr); ok && admittingCalls[calleeName(call)] {
					if id, ok := assign.Lhs[0].(*ast.Ident); ok {
						admitted[id.Name] = true
					}
				}
				return true
			})
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := calleeName(call)
				if name == "CreateLockedTurn" {
					sites++
					if len(call.Args) != 3 {
						t.Errorf("%s: CreateLockedTurn with %d arguments", f.fset.Position(call.Pos()), len(call.Args))
						return true
					}
					if adm, ok := call.Args[2].(*ast.CallExpr); !ok || calleeName(adm) != "Admitter" {
						t.Errorf("%s: CreateLockedTurn is not passed a session guard's Admitter: its turn would skip the guard (technical plan §40.1)", f.fset.Position(call.Pos()))
					}
					return true
				}
				idx, ok := takesAdmission[name]
				if !ok {
					return true
				}
				if name == "CreateAndArmDispatch" {
					sites++
				}
				if idx >= len(call.Args) {
					t.Errorf("%s: %s is called without its admission", f.fset.Position(call.Pos()), name)
					return true
				}
				switch adm := call.Args[idx].(type) {
				case *ast.Ident:
					if !admitted[adm.Name] {
						t.Errorf("%s: %s is passed %q as its admission, which is neither the result of the session guard's admission in %s nor a sessionguard.Admission parameter of it: every turn is admitted by the guard first (technical plan §40.1)", f.fset.Position(call.Pos()), name, adm.Name, fn.Name.Name)
					}
				case *ast.CallExpr:
					if calleeName(adm) != "AdmitNewSession" {
						t.Errorf("%s: %s is passed the result of %s as its admission: only the session guard admits a turn (technical plan §40.1)", f.fset.Position(call.Pos()), name, calleeName(adm))
					}
				default:
					t.Errorf("%s: %s is passed an admission that is not the session guard's: only the guard admits a turn (technical plan §40.1)", f.fset.Position(call.Pos()), name)
				}
				return true
			})
		}
	}
	// The seven places a turn is created: createTurnLocked, session
	// creation, plan approval, the workflow engine's next attempt (its own
	// advance and a person's revision share it), the automatic re-review,
	// an owed review request's re-run, the composition review. Fewer means
	// the scan no longer sees them.
	if sites < 7 {
		t.Fatalf("found %d turn inserts, want at least 7: the scan is broken", sites)
	}
}

// TestDecideOnlyFromTurnguard: sessionguard.Decide, the one mint of an
// admission for an existing session, is called only by
// internal/app/turnguard, which reads the session's facts under its row
// lock first. A second caller could decide on facts read anywhere -- on the
// pool, before the lock, or not at all.
func TestDecideOnlyFromTurnguard(t *testing.T) {
	t.Parallel()

	found := 0
	productionGoFiles(t, func(rel string, fset *token.FileSet, file *ast.File) {
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Decide" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "sessionguard" {
				return true
			}
			if strings.HasPrefix(rel, "internal/app/turnguard/") {
				found++
				return true
			}
			t.Errorf("%s: sessionguard.Decide outside internal/app/turnguard: only the guard decides, on facts it read under the session's row lock (technical plan §40.1)", fset.Position(sel.Pos()))
			return true
		})
	})
	if found == 0 {
		t.Fatal("found no sessionguard.Decide call in internal/app/turnguard: the scan is broken")
	}
}

// TestAdmitNewSessionOnlyInCreateSessionOnTx: sessionguard.AdmitNewSession
// admits a turn without reading anything, which is sound only for the first
// turn of a session created in the same transaction. It is named only in
// httpapi's CreateSessionOnTx; anywhere else it would let a turn of an
// existing session walk past a spent cap.
func TestAdmitNewSessionOnlyInCreateSessionOnTx(t *testing.T) {
	t.Parallel()

	const allowedFile = "internal/adapters/inbound/httpapi/create.go"
	const allowedFunc = "CreateSessionOnTx"
	found := 0
	productionGoFiles(t, func(rel string, fset *token.FileSet, file *ast.File) {
		if strings.HasPrefix(rel, "internal/domain/sessionguard/") {
			return
		}
		for _, decl := range file.Decls {
			var name string
			if fn, ok := decl.(*ast.FuncDecl); ok {
				name = fn.Name.Name
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "AdmitNewSession" {
					return true
				}
				if rel == allowedFile && name == allowedFunc {
					found++
					return true
				}
				t.Errorf("%s: sessionguard.AdmitNewSession outside %s's %s: it admits without reading the session's spend, which only a session created in the same transaction may skip (technical plan §40.1)", fset.Position(sel.Pos()), allowedFile, allowedFunc)
				return true
			})
		}
	})
	if found != 1 {
		t.Fatalf("found %d AdmitNewSession calls in %s, want 1: the scan is broken", found, allowedFunc)
	}
}

// TestSpendCapAppliesRegardlessOfWhoAsked: the cap's inversion of §24.6 --
// it binds every turn on the session, a person's included -- is stated
// where the decision is made and where every person's turn passes it, so a
// reader who would exempt a person's prompt, as §24.6 exempts a person's
// re-trigger from the automatic re-review budget, meets the reason first.
func TestSpendCapAppliesRegardlessOfWhoAsked(t *testing.T) {
	t.Parallel()

	root := moduleRootForTest(t)
	for _, tc := range []struct {
		file string
		want string
	}{
		{"internal/domain/sessionguard/doc.go", "This inverts §24.6's rule"},
		{"internal/app/turnguard/guard.go", "§40.1's inversion of §24.6"},
		{"internal/adapters/inbound/httpapi/turn.go", "§40.1's inversion of §24.6"},
		{"internal/adapters/inbound/httpapi/decideplan.go", "§40.1's inversion of §24.6"},
	} {
		body, err := os.ReadFile(filepath.Join(root, tc.file))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), tc.want) {
			t.Errorf("%s does not state %q: the spend cap applies to every turn regardless of who asked, the inversion of §24.6 (technical plan §40.1)", tc.file, tc.want)
		}
	}
}

// setCostUSD matches an assignment of turns.cost_usd in an UPDATE.
var setCostUSD = regexp.MustCompile(`(?is)\bupdate\s+turns\b.*\bset\b.*\bcost_usd\s*=`)

// TestEveryTurnCostWriterTargetsAProcessingTurn pins what the session
// guard's read of a session's spend rests on (GetSessionGuardFacts): only a
// dispatched turn carries cost, so summing over dispatched_at IS NOT NULL
// loses nothing. The one statement that writes turns.cost_usd is
// RecordTurnStepCost, and it targets the session's processing turn, which
// reached processing through dispatched, which stamped dispatched_at. A
// second writer, or one that targets any other turn, fails here.
func TestEveryTurnCostWriterTargetsAProcessingTurn(t *testing.T) {
	t.Parallel()

	root := moduleRootForTest(t)
	queries, err := filepath.Glob(filepath.Join(root, "internal", "adapters", "outbound", "postgres", "queries", "*.sql"))
	if err != nil || len(queries) == 0 {
		t.Fatalf("list the sqlc queries: %v (%d files)", err, len(queries))
	}
	writers := 0
	for _, path := range queries {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, block := range strings.Split(string(body), "-- name:") {
			if i == 0 {
				continue
			}
			sql := sqlComment.ReplaceAllString(block, "")
			if !setCostUSD.MatchString(sql) {
				continue
			}
			name := strings.Fields(block)[0]
			writers++
			if name != "RecordTurnStepCost" {
				t.Errorf("%s: query %q writes turns.cost_usd: only RecordTurnStepCost may, onto the processing turn, so the session guard's sum over dispatched turns stays whole (technical plan §40.1)", path, name)
				continue
			}
			if !strings.Contains(sql, "t.status = 'processing'") {
				t.Errorf("%s: RecordTurnStepCost no longer targets the processing turn: a cost on a turn never dispatched would escape the session guard's sum (technical plan §40.1)", path)
			}
		}
	}
	if writers != 1 {
		t.Fatalf("found %d writers of turns.cost_usd, want 1: the scan is broken", writers)
	}
}

// calleeName is the name a call expression calls: the selector of a
// method or package-qualified call, or the identifier of a plain one.
func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fun.Sel.Name
	case *ast.Ident:
		return fun.Name
	default:
		return ""
	}
}
