package sessionactor_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/app/sessionactor"
)

// declaredTimerKinds parses this package's own non-test Go files and
// returns every package-level string constant named Timer* -- the named
// timer kinds (command.go) -- as name -> value. Parsed from source, not
// listed by hand, so a kind added later is found without anyone updating a
// list.
func declaredTimerKinds(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	kinds := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, ident := range vs.Names {
					if !strings.HasPrefix(ident.Name, "Timer") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquote %s: %v", ident.Name, err)
					}
					kinds[ident.Name] = value
				}
			}
		}
	}
	return kinds
}

// TestClassifyTimer_EveryDeclaredKindIsClassified is the exhaustiveness
// gate technical plan §43.20 asks for: every named timer kind this package
// declares must have an explicit classification, so a kind added without
// one fails here instead of silently reading as whatever the default says.
// (The default is safe -- an unknown kind counts as work, never settled --
// but the classification is a decision each kind's author must make.)
func TestClassifyTimer_EveryDeclaredKindIsClassified(t *testing.T) {
	t.Parallel()

	kinds := declaredTimerKinds(t)
	if len(kinds) < 6 {
		t.Fatalf("found %d Timer* constants (%v), want at least the six named timers: the scan is broken", len(kinds), kinds)
	}
	for name, value := range kinds {
		if _, ok := sessionactor.ClassifyTimer(value); !ok {
			t.Errorf("timer kind %s = %q has no classification in ClassifyTimer (timerwork.go): decide whether its firing can create a turn with no new input, and say why", name, value)
		}
	}
}

// TestClassifyTimer_Table pins each kind's classification and what it
// means for settled.
func TestClassifyTimer_Table(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		want       sessionactor.TimerWork
		createWork bool
	}{
		{sessionactor.TimerConnectingDeadline, sessionactor.TimerWorkSandboxOnly, false},
		{sessionactor.TimerLivenessCheck, sessionactor.TimerWorkSandboxOnly, false},
		{sessionactor.TimerInactivity, sessionactor.TimerWorkSandboxOnly, false},
		{sessionactor.TimerTerminalGrace, sessionactor.TimerWorkSandboxOnly, false},
		{sessionactor.TimerTurnDeadline, sessionactor.TimerWorkTurnInFlight, false},
		{sessionactor.TimerStop, sessionactor.TimerWorkTurnInFlight, false},
		{sessionactor.TimerReviewRetriggerDebounce, sessionactor.TimerWorkCreatesTurn, true},
	} {
		got, ok := sessionactor.ClassifyTimer(tc.name)
		if !ok || got != tc.want {
			t.Errorf("ClassifyTimer(%q) = %v, %v; want %v, true", tc.name, got, ok, tc.want)
		}
		if can := sessionactor.TimerCanCreateWork(tc.name); can != tc.createWork {
			t.Errorf("TimerCanCreateWork(%q) = %v, want %v", tc.name, can, tc.createWork)
		}
	}
	for _, unknown := range []string{"", "a_kind_from_a_newer_binary", "Review_Retrigger_Debounce"} {
		if _, ok := sessionactor.ClassifyTimer(unknown); ok {
			t.Errorf("ClassifyTimer(%q) ok, want unknown", unknown)
		}
		if !sessionactor.TimerCanCreateWork(unknown) {
			t.Errorf("TimerCanCreateWork(%q) = false: an unknown kind must count as work, never settled", unknown)
		}
	}
}

// moduleRoot walks up from the package directory to go.mod.
func moduleRoot(t *testing.T) string {
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

// timerNameArg resolves the expression a call site passes as a timer
// name: a Timer* constant (bare in this package, or selected from it
// elsewhere) or a string literal. ok is false for anything else -- a
// variable, a call -- which this scan cannot see through.
func timerNameArg(expr ast.Expr, kinds map[string]string) (value, display string, ok bool) {
	switch e := expr.(type) {
	case *ast.Ident:
		v, found := kinds[e.Name]
		return v, e.Name, found
	case *ast.SelectorExpr:
		v, found := kinds[e.Sel.Name]
		return v, e.Sel.Name, found
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", "", false
		}
		v, err := strconv.Unquote(e.Value)
		return v, e.Value, err == nil
	default:
		return "", "", false
	}
}

// TestTimerCountsAsScheduledWork_Table pins the status's reading of an
// armed timer (technical plan §43.20, review round 4's P3): the re-review
// debounce counts only while its fire can insert a turn (the repository
// opted in, the budget unspent -- reviewRetriggerCanFire); every other kind
// is TimerCanCreateWork's, whatever that flag says, an unknown kind
// included.
func TestTimerCountsAsScheduledWork_Table(t *testing.T) {
	t.Parallel()

	for _, canFire := range []bool{false, true} {
		if got := sessionactor.TimerCountsAsScheduledWork(sessionactor.TimerReviewRetriggerDebounce, canFire); got != canFire {
			t.Errorf("TimerCountsAsScheduledWork(debounce, canFire %v) = %v, want %v", canFire, got, canFire)
		}
		for _, name := range []string{sessionactor.TimerConnectingDeadline, sessionactor.TimerLivenessCheck, sessionactor.TimerInactivity, sessionactor.TimerTerminalGrace, sessionactor.TimerTurnDeadline, sessionactor.TimerStop, "a_kind_from_a_newer_binary"} {
			if got, want := sessionactor.TimerCountsAsScheduledWork(name, canFire), sessionactor.TimerCanCreateWork(name); got != want {
				t.Errorf("TimerCountsAsScheduledWork(%q, canFire %v) = %v, want TimerCanCreateWork's %v", name, canFire, got, want)
			}
		}
	}
}

// TestClassifyTimer_EveryArmedNameIsClassified closes the other way a kind
// could slip in, for the two arming forms the code uses: in every non-test
// Go file under internal/, controlplane/ and cmd/ (sqlcgen excluded), the
// name argument of every armTimer call, and the keyed Name of every
// UpsertSessionTimerParams composite literal (the synchronize webhook's), must
// be a classified Timer* constant. It cannot see a params value filled by
// field assignment, the params type used through an alias, a positional
// literal (go vet's composites check flags that one), or SQL that writes
// session_timers -- a raw Exec, a new sqlc query, a migration or backfill;
// technical plan §43.20 states the runtime backstop for those.
func TestClassifyTimer_EveryArmedNameIsClassified(t *testing.T) {
	t.Parallel()

	kinds := declaredTimerKinds(t)
	root := moduleRoot(t)
	fset := token.NewFileSet()
	sites := 0
	for _, top := range []string{"internal", "controlplane", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.Contains(path, string(filepath.Separator)+"sqlcgen"+string(filepath.Separator)) {
				return nil
			}
			file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return perr
			}
			check := func(pos token.Pos, expr ast.Expr, what string) {
				sites++
				value, display, ok := timerNameArg(expr, kinds)
				if !ok {
					t.Errorf("%s: %s names a timer with an expression this scan cannot resolve to a Timer* constant", fset.Position(pos), what)
					return
				}
				if _, classified := sessionactor.ClassifyTimer(value); !classified {
					t.Errorf("%s: %s arms timer %s = %q, which ClassifyTimer (timerwork.go) does not classify", fset.Position(pos), what, display, value)
				}
			}
			ast.Inspect(file, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.FuncDecl:
					// armTimer itself forwards its name parameter to the
					// upsert; every call to it is checked instead.
					if n.Name.Name == "armTimer" {
						return false
					}
				case *ast.CallExpr:
					var fn string
					switch f := n.Fun.(type) {
					case *ast.SelectorExpr:
						fn = f.Sel.Name
					case *ast.Ident:
						fn = f.Name
					}
					if fn == "armTimer" && len(n.Args) >= 3 {
						check(n.Pos(), n.Args[2], "armTimer")
					}
				case *ast.CompositeLit:
					var typ string
					switch ty := n.Type.(type) {
					case *ast.SelectorExpr:
						typ = ty.Sel.Name
					case *ast.Ident:
						typ = ty.Name
					}
					if typ != "UpsertSessionTimerParams" {
						return true
					}
					for _, elt := range n.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Name" {
							check(kv.Pos(), kv.Value, "UpsertSessionTimerParams")
						}
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	if sites < 10 {
		t.Fatalf("found only %d timer-arming call sites: the scan is broken", sites)
	}
}
