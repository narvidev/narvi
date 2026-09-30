package controlplane

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// fakeVersionRow answers requireSupportedPostgres's one read with a version
// of the test's choosing, or with an error.
type fakeVersionRow struct {
	num int
	err error
}

func (r fakeVersionRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 1 {
		return fmt.Errorf("fakeVersionRow: Scan into %d destinations, want 1", len(dest))
	}
	p, ok := dest[0].(*int)
	if !ok {
		return fmt.Errorf("fakeVersionRow: Scan into %T, want *int", dest[0])
	}
	*p = r.num
	return nil
}

// fakeVersionQuerier records the statement it was asked to run, and whether
// the read was bounded.
type fakeVersionQuerier struct {
	row         fakeVersionRow
	gotSQL      string
	gotDeadline bool
}

func (q *fakeVersionQuerier) QueryRow(ctx context.Context, sql string, _ ...any) pgx.Row {
	q.gotSQL = sql
	_, q.gotDeadline = ctx.Deadline()
	return q.row
}

// TestRequireSupportedPostgres proves boot's refusal of a Postgres server
// older than platform.MinPostgresServerVersionNum, against a stubbed
// version answer: a server at the floor or above is accepted, an older one
// is refused with both its version and the floor named, and a read that
// fails refuses too. internal/ops pins the floor itself to the version the
// documents state.
func TestRequireSupportedPostgres(t *testing.T) {
	t.Parallel()

	readErr := errors.New("connection refused")
	for _, tc := range []struct {
		name     string
		row      fakeVersionRow
		wantErr  bool
		wantText []string
	}{
		{name: "17.6, the tested version", row: fakeVersionRow{num: 170006}},
		{name: "16.0, the floor itself", row: fakeVersionRow{num: 160000}},
		{name: "16.10", row: fakeVersionRow{num: 160010}},
		{name: "15.13, the last major below the floor", row: fakeVersionRow{num: 150013}, wantErr: true,
			wantText: []string{"refusing to start", "version 15.13", "needs 16.0 or later"}},
		{name: "14.18", row: fakeVersionRow{num: 140018}, wantErr: true,
			wantText: []string{"version 14.18", "needs 16.0 or later"}},
		{name: "9.6.24, numbered the old way", row: fakeVersionRow{num: 90624}, wantErr: true,
			wantText: []string{"version 9.6.24", "needs 16.0 or later"}},
		{name: "a read that fails", row: fakeVersionRow{err: readErr}, wantErr: true,
			wantText: []string{"read the Postgres server's version", "connection refused"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			q := &fakeVersionQuerier{row: tc.row}
			err := requireSupportedPostgres(context.Background(), q, time.Second)

			if q.gotSQL != serverVersionNumQuery {
				t.Errorf("ran %q, want serverVersionNumQuery", q.gotSQL)
			}
			if !q.gotDeadline {
				t.Error("the version read ran with no deadline, want it bounded by the timeout")
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("requireSupportedPostgres() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("requireSupportedPostgres() = nil, want a refusal")
			}
			for _, want := range tc.wantText {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("requireSupportedPostgres() = %q, want it to contain %q", err, want)
				}
			}
			if tc.row.err != nil && !errors.Is(err, tc.row.err) {
				t.Errorf("requireSupportedPostgres() = %v, want it to wrap the read's own error", err)
			}
		})
	}
}

// TestPostgresVersion proves the rendering of a server_version_num in the
// refusal: major.minor from Postgres 10 on, major.minor.patch before it.
func TestPostgresVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		num  int
		want string
	}{
		{170006, "17.6"},
		{160000, "16.0"},
		{150013, "15.13"},
		{100023, "10.23"},
		{90624, "9.6.24"},
		{90105, "9.1.5"},
	} {
		if got := postgresVersion(tc.num); got != tc.want {
			t.Errorf("postgresVersion(%d) = %q, want %q", tc.num, got, tc.want)
		}
	}
}

// golangMigrate is the import path of golang-migrate's root package, the one
// whose New and NewWithInstance run migrations.
const golangMigrate = "github.com/golang-migrate/migrate/v4"

// funcKey names a function declaration as a call to it is written: its name,
// or (Receiver).Name for a method.
func funcKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if id, ok := recv.(*ast.Ident); ok {
		return "(" + id.Name + ")." + fn.Name.Name
	}
	return "(?)." + fn.Name.Name
}

// callsMigrate reports whether body calls golang-migrate's New or
// NewWithInstance, the package imported under local.
func callsMigrate(body *ast.BlockStmt, local string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); ok && x.Name == local && (sel.Sel.Name == "New" || sel.Sel.Name == "NewWithInstance") {
			found = true
		}
		return !found
	})
	return found
}

// TestMigrationsRunOnlyAfterTheVersionCheck proves, from the source, that
// nothing in this package reaches a migration without first refusing an
// older Postgres server (requireSupportedPostgres, technical plan §5.1).
//
// The migration entry points are the functions that call golang-migrate
// themselves (applyMigrations and applyModuleMigrations today), and no
// non-test file outside this package imports golang-migrate, so none can
// migrate out of this test's sight. A function that calls an entry point,
// or a function that reaches one, must call requireSupportedPostgres
// earlier in its own body; one that does not is an unchecked path itself,
// and every caller of it must check first in turn. Build is the one function
// allowed to reach a migration unchecked (applyModuleMigrations): it is
// exported and takes an open pool, and its contract is that its caller has
// checked first -- serve and runRoutesCommand do, and are held to it here
// like any other caller. serve, runSeedCommand and runRoutesCommand, the
// three subcommands, must each be among the callers that check. "Earlier"
// is source order within one function body, and a function reached only as
// a value, not called, fails: the check cannot follow it.
func TestMigrationsRunOnlyAfterTheVersionCheck(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	var entryPoints []string
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		local := ""
		for _, imp := range file.Imports {
			if strings.Trim(imp.Path.Value, `"`) != golangMigrate {
				continue
			}
			local = "migrate"
			if imp.Name != nil {
				local = imp.Name.Name
			}
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			funcs[funcKey(fn)] = fn
			if local != "" && callsMigrate(fn.Body, local) {
				entryPoints = append(entryPoints, funcKey(fn))
			}
		}
	}
	for _, want := range []string{"applyMigrations", "applyModuleMigrations"} {
		if !slices.Contains(entryPoints, want) {
			t.Errorf("%s no longer calls golang-migrate; entry points found: %v -- revisit this test", want, entryPoints)
		}
	}

	// unchecked maps every function that reaches a migration without the
	// check to the function through which it does.
	unchecked := map[string]string{}
	for _, ep := range entryPoints {
		unchecked[ep] = "golang-migrate"
	}
	var checked map[string]bool
	for changed := true; changed; {
		changed = false
		checked = map[string]bool{}
		for key, fn := range funcs {
			if _, done := unchecked[key]; done {
				continue
			}
			firstCheck := token.NoPos
			type reach struct {
				pos    token.Pos
				callee string
			}
			var reaches []reach
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				if id.Name == "requireSupportedPostgres" && (firstCheck == token.NoPos || call.Pos() < firstCheck) {
					firstCheck = call.Pos()
				}
				if _, ok := unchecked[id.Name]; ok {
					reaches = append(reaches, reach{pos: call.Pos(), callee: id.Name})
				}
				return true
			})
			if len(reaches) == 0 {
				continue
			}
			checked[key] = true
			for _, r := range reaches {
				if firstCheck == token.NoPos || firstCheck > r.pos {
					unchecked[key] = r.callee
					delete(checked, key)
					changed = true
					break
				}
			}
		}
	}

	for key, via := range unchecked {
		if slices.Contains(entryPoints, key) || key == "Build" {
			continue
		}
		t.Errorf("%s: %s reaches the migrations through %s without first refusing an older Postgres server -- call requireSupportedPostgres before it",
			fset.Position(funcs[key].Pos()), key, via)
	}
	if _, ok := unchecked["Build"]; !ok {
		t.Error("Build no longer reaches a migration: drop it from this test's one exception, and from its doc comment")
	}
	for _, want := range []string{"serve", "runSeedCommand", "runRoutesCommand"} {
		if !checked[want] {
			t.Errorf("%s is not among the callers that check the server's version before reaching a migration (checked: %v) -- revisit which subcommands check", want, checked)
		}
	}

	// A migrating function referenced other than by a call -- passed as a
	// value -- escapes the order this test checks. A selector's name
	// (x.Build) is some other package's or type's, not this package's
	// function.
	for key, fn := range funcs {
		skip := map[*ast.Ident]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if id, ok := n.Fun.(*ast.Ident); ok {
					skip[id] = true
				}
			case *ast.SelectorExpr:
				skip[n.Sel] = true
			}
			return true
		})
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok || skip[id] {
				return true
			}
			if _, migrating := unchecked[id.Name]; migrating {
				t.Errorf("%s: %s refers to %s other than by calling it; this test cannot follow it to the version check",
					fset.Position(id.Pos()), key, id.Name)
			}
			return true
		})
	}

	// Nothing outside this package can run a migration: no other non-test
	// file imports golang-migrate.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	root, self := "..", filepath.Join("..", filepath.Base(wd))
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.Dir(path) == self {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		for _, imp := range file.Imports {
			if strings.Trim(imp.Path.Value, `"`) == golangMigrate {
				t.Errorf("%s imports %s outside controlplane: a migration it runs would escape the version check this test holds", path, golangMigrate)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}
}
