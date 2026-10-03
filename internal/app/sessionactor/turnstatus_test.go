package sessionactor

import (
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// turnStatusParams is the params type of the one query that writes
// turns.status, UpdateTurnStatus, which turnWriter.UpdateStatus takes.
const turnStatusParams = "UpdateTurnStatusParams"

// turnStatusQueries are the sqlc queries that write turns.status: the one
// turnWriter wraps. CreateTurn inserts a turn, pending only
// (postgres.ErrTurnNotPending), and TestNoOtherSQLInsertsTurns
// (internal/adapters/outbound/postgres) keeps it the one insert.
var turnStatusQueries = map[string]bool{
	"UpdateTurnStatus": true,
}

// The files that may touch the write directly: the store, which runs the
// query, and the recorder, which wraps the store.
const (
	turnStatusStoreFile    = "internal/adapters/outbound/postgres/turn_store.go"
	turnStatusRecorderFile = "internal/app/sessionactor/turnstatus.go"
)

// turnStatusOwners may name the params in a signature.
var turnStatusOwners = map[string]bool{
	turnStatusStoreFile:    true,
	turnStatusRecorderFile: true,
}

// TestTurnStatusWritesGoThroughTheRecorder keeps the re-review debounce's
// wake-up whole at the source (technical plan §24.9): a turn's end wakes
// the debounce in its own transaction only when it is written through
// turnWrites, which notes it for transact. It type-checks every non-test
// package under internal, controlplane, cmd and extension (go/packages;
// generated sqlcgen code aside) and fails on:
//
//   - a call or method value of postgres.TurnStore's UpdateStatus outside
//     the recorder -- by the receiver's type, whatever the variable is
//     called, through WithTx, a constructor chain or an embedding;
//   - a call or method value of sqlcgen.Queries' UpdateTurnStatus outside
//     the store;
//   - an identifier naming UpdateTurnStatusParams, unless it types a
//     composite literal handed straight to a turnWriter method, or sits in
//     a signature in the store or the recorder;
//   - a string literal, or a constant string concatenation read whole
//     (constStrings), that updates turns -- with or without ONLY, the table
//     bare, "quoted" or public-qualified, aliased or not -- with a SET whose
//     targets name status.
//
// A file the build constraints leave out of the type-checked load (none
// writes turns today) gets the last two checks only. It does not see SQL
// assembled at run time, or a write through reflection.
// TestTurnStatusQueriesAreTheRecordedOnes covers the sqlc queries.
func TestTurnStatusWritesGoThroughTheRecorder(t *testing.T) {
	t.Parallel()

	recorded := 0
	for _, f := range productionFiles(t) {
		recorded += checkTurnStatusWrites(t, f)
	}
	// The nine writes the session actor makes (turnstatus.go lists them).
	// Fewer means the scan no longer sees them.
	if recorded < 9 {
		t.Fatalf("found %d turn status writes through turnWrites, want at least 9: the scan is broken", recorded)
	}
}

func checkTurnStatusWrites(t *testing.T, f productionFile) int {
	t.Helper()
	fset := f.fset
	approved := map[token.Pos]bool{}
	approve := func(n ast.Node) {
		if n == nil {
			return
		}
		ast.Inspect(n, func(m ast.Node) bool {
			if id, ok := m.(*ast.Ident); ok && id.Name == turnStatusParams {
				approved[id.Pos()] = true
			}
			return true
		})
	}

	recorded := 0
	ast.Inspect(f.file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			if turnStatusOwners[f.rel] {
				approve(n.Type)
			}
		case *ast.CallExpr:
			// Only the recorder's own methods take the params literal.
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok {
				if m, ok := f.method(sel); ok && m.is(sessionactorPkgPath, "turnWriter") {
					for _, arg := range n.Args {
						if lit, ok := arg.(*ast.CompositeLit); ok {
							approve(lit.Type)
							recorded++
						}
					}
				}
			}
		case *ast.SelectorExpr:
			m, ok := f.method(n)
			if !ok {
				return true
			}
			switch {
			case m.is(postgresPkgPath, "TurnStore") && m.name == "UpdateStatus" && f.rel != turnStatusRecorderFile:
				t.Errorf("%s: TurnStore.UpdateStatus outside the recorder: a turn it ends never wakes the held re-review (technical plan §24.9) -- call a.turnWrites(tx).UpdateStatus", fset.Position(n.Pos()))
			case m.is(sqlcgenPkgPath, "Queries") && turnStatusQueries[m.name] && f.rel != turnStatusStoreFile:
				t.Errorf("%s: the %s query outside the turn store: a turn it ends never wakes the held re-review (technical plan §24.9) -- write it through the store and turnWrites", fset.Position(n.Pos()), m.name)
			}
		}
		return true
	})

	ast.Inspect(f.file, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == turnStatusParams && !approved[id.Pos()] {
			t.Errorf("%s: a %s value outside turnWrites: a turn ended this way never wakes the held re-review (technical plan §24.9) -- hand the literal straight to a.turnWrites(tx)", fset.Position(id.Pos()), id.Name)
		}
		return true
	})
	for _, pos := range rawTurnStatusWrites(f) {
		t.Errorf("%s: raw SQL writing turns.status: a turn it ends never wakes the held re-review (technical plan §24.9) -- write it through the turn store and turnWrites", fset.Position(pos))
	}
	return recorded
}

// rawTurnStatusWrites returns where f's constant strings write turns.status
// (sqlWritesTurnStatus), each read whole (constStrings).
func rawTurnStatusWrites(f productionFile) []token.Pos {
	var out []token.Pos
	f.constStrings(func(expr ast.Expr, sql string) {
		if sqlWritesTurnStatus(sqlBlockComment.ReplaceAllString(sqlLineComment.ReplaceAllString(sql, ""), "")) {
			out = append(out, expr.Pos())
		}
	})
	return out
}

var (
	// turnsTable is the turns table as a statement may name it: bare,
	// "quoted", or qualified by the public schema, quoted or not.
	turnsTable = `(?:(?:"public"|public)\s*\.\s*)?(?:"turns"|turns\b)`
	// updateTurnsSet matches an UPDATE of turns up to its SET, an alias
	// between them allowed; the SET clause starts at the match's end.
	updateTurnsSet = regexp.MustCompile(`(?is)\bupdate\s+(?:only\s+)?` + turnsTable + `(?:\s*\*)?(?:\s+(?:as\s+)?[a-z_][a-z0-9_]*)?\s+set\b`)
)

// sqlWritesTurnStatus reports whether sql (comments already stripped)
// updates turns with a SET naming status among its targets, singly
// (status = ...) or in a column list ((status, completed_at) = ...), quoted
// or not.
func sqlWritesTurnStatus(sql string) bool {
	for _, loc := range updateTurnsSet.FindAllStringIndex(sql, -1) {
		for _, target := range setTargets(sql[loc[1]:]) {
			if target == "status" {
				return true
			}
		}
	}
	return false
}

// TestTurnStatusQueriesAreTheRecordedOnes is the SQL half of
// TestTurnStatusWritesGoThroughTheRecorder: the sqlc queries that write
// turns.status are exactly the one turnWriter wraps, UpdateTurnStatus. A
// query writes it when it updates turns -- with or without ONLY, the table
// bare, "quoted" or public-qualified, aliased or not -- with a SET whose
// targets name status. A new query of that kind gets params of its own, or
// none, which the Go scan cannot see: route it through turnWrites, then add
// it here. Comments are ignored; SQL assembled at run time is not seen.
func TestTurnStatusQueriesAreTheRecordedOnes(t *testing.T) {
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
			if !sqlWritesTurnStatus(sqlBlockComment.ReplaceAllString(sqlLineComment.ReplaceAllString(block, ""), "")) {
				continue
			}
			if !turnStatusQueries[name] {
				t.Errorf("%s: query %q writes a turn's status: a turn it ends never wakes the held re-review (technical plan §24.9), so route its store method through turnWriter (turnstatus.go) and list it here", path, name)
				continue
			}
			found[name] = true
		}
	}
	for name := range turnStatusQueries {
		if !found[name] {
			t.Errorf("query %q no longer writes a turn's status, or is gone: the scan is broken, or the list is stale", name)
		}
	}
}

// TestSQLWritesTurnStatus pins the statement shapes the SQL guard reads as a
// write of turns.status, and the ones it must not.
func TestSQLWritesTurnStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sql  string
		want bool
	}{
		{"a plain update", `UPDATE turns SET status = $2 WHERE id = $1`, true},
		{"beside other columns", `UPDATE turns SET completed_at = now(), status = 'failed' WHERE id = $1`, true},
		{"only", `UPDATE ONLY turns SET status = 'failed' WHERE id = $1`, true},
		{"schema-qualified", `UPDATE public.turns SET status = 'failed' WHERE id = $1`, true},
		{"quoted", `UPDATE "public"."turns" SET "status" = 'failed' WHERE id = $1`, true},
		{"aliased", `UPDATE turns AS t SET status = 'failed' WHERE t.id = $1`, true},
		{"aliased without AS", `UPDATE turns t SET status = 'failed' WHERE t.id = $1`, true},
		{"a column list", `UPDATE turns SET (status, completed_at) = ('failed', now()) WHERE id = $1`, true},
		{"only, quoted, aliased, a column list", `UPDATE ONLY "public"."turns" AS t SET (status, completed_at) = ('cancelled', now()) WHERE t.id = $1`, true},
		{"a column list naming status second", `UPDATE turns SET (completed_at, status) = (now(), 'failed') WHERE id = $1`, true},
		{"inside a CTE", `WITH t AS (UPDATE turns SET status = 'failed' WHERE id = $1 RETURNING *) SELECT * FROM t`, true},
		{"lower case", `update turns set status = 'failed' where id = $1`, true},
		{"another column only", `UPDATE turns SET stop_requested_at = COALESCE(stop_requested_at, now()) WHERE session_id = $1 AND status IN ('pending', 'dispatched', 'processing')`, false},
		{"status read in the WHERE only", `UPDATE turns SET epistemic_outcome = $2 WHERE id = $1 AND status = 'processing'`, false},
		{"status read on the right only", `UPDATE turns SET conversation_id = status::text WHERE id = $1`, false},
		{"another table's status", `UPDATE sessions SET status = 'failed' WHERE id = $1`, false},
		{"a table whose name starts with turns", `UPDATE turns_archive SET status = 'failed'`, false},
		{"a table whose name ends with turns", `UPDATE old_turns SET status = 'failed'`, false},
		{"another table's status beside a turns write", `WITH t AS (UPDATE turns SET cost_usd = 1 WHERE id = $1) UPDATE sessions SET status = 'active' WHERE id = $2`, false},
		{"an insert", `INSERT INTO turns (session_id, status) VALUES ($1, 'pending')`, false},
		{"a read", `SELECT status FROM turns WHERE id = $1`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sqlWritesTurnStatus(tc.sql); got != tc.want {
				t.Errorf("sqlWritesTurnStatus(%q) = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}

// TestRawTurnStatusWritesReadsASplitConstantOnce: a constant split across
// operands is read whole, so an operand that only looks like a write on its
// own is not one, and a real write split across operands is still caught,
// once.
func TestRawTurnStatusWritesReadsASplitConstantOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want int
	}{
		{"a write split in two", `const q = "UPDATE " + "turns SET status = 'failed' WHERE id = $1"`, 1},
		{"a write split in three", `const q = "UPDATE ONLY " + "turns " + "SET status = 'failed'"`, 1},
		{"another table, split where its name starts like turns", `const q = "UPDATE turns" + "_archive SET status = 'failed'"`, 0},
		{"a write in one literal", `const q = "UPDATE turns SET status = 'failed'"`, 1},
		{"a write behind a comment", "const q = \"-- ends the turn\\nUPDATE turns SET status = 'failed'\"", 1},
		{"a comment naming the write", `const q = "SELECT 1 -- UPDATE turns SET status = 'failed'"`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := len(rawTurnStatusWrites(typeCheckedFixture(t, "package fixture\n\n"+tc.src+"\n"))); got != tc.want {
				t.Errorf("rawTurnStatusWrites found %d writes in %s, want %d", got, tc.src, tc.want)
			}
		})
	}
}
