package controlplane

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
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

// TestMigrationsRunOnlyAfterTheVersionCheck proves, from this package's own
// source, that every function running the embedded migrations
// (applyMigrations) has refused an older server first
// (requireSupportedPostgres): serve and seed are the subcommands that
// migrate, and both check. routes, which neither migrates nor needs a
// reachable database, does not. A new path to the migrations that skipped
// the check fails here.
func TestMigrationsRunOnlyAfterTheVersionCheck(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fset := token.NewFileSet()
	var migrating []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			var checks, migrations []token.Pos
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				switch id.Name {
				case "requireSupportedPostgres":
					checks = append(checks, call.Pos())
				case "applyMigrations":
					migrations = append(migrations, call.Pos())
				}
				return true
			})
			if len(migrations) == 0 {
				continue
			}
			migrating = append(migrating, fn.Name.Name)
			for _, m := range migrations {
				if len(checks) == 0 || checks[0] > m {
					t.Errorf("%s: %s runs the migrations without first refusing an older Postgres server -- call requireSupportedPostgres before applyMigrations",
						fset.Position(m), fn.Name.Name)
				}
			}
		}
	}
	for _, want := range []string{"serve", "runSeedCommand"} {
		if !slices.Contains(migrating, want) {
			t.Errorf("%s no longer runs applyMigrations; functions that do: %v -- revisit which subcommands check the server's version", want, migrating)
		}
	}
}
