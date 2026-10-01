package sessionactor

import (
	"encoding/json"
	"go/ast"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// eventTypePassThroughs are the functions that store an event under a type
// they were handed rather than one they name: the two append helpers, and
// the sandbox ingress, which stores a frame under the frame's own type only
// after refusing serverEventTypes (handleSandboxEvent, appendTokenFrame).
var eventTypePassThroughs = map[string]bool{
	"internal/app/sessionactor/actor.go:appendRawEvent":            true,
	"internal/app/sessionactor/timerfired.go:appendEvent":          true,
	"internal/app/sessionactor/sandboxevent.go:handleSandboxEvent": true,
	"internal/app/sessionactor/tokenframe.go:appendTokenFrame":     true,
}

// TestServerWrittenEventTypesAreReserved keeps serverEventTypes whole, so a
// type the control plane starts writing cannot be left open to the
// sandbox socket. It type-checks the production packages and reads every
// event type they store:
//
//   - the type argument of the actor's appendEvent and appendRawEvent, and
//     the Type field of a sqlcgen.CreateEventParams literal, each a
//     constant or else handed through by one of eventTypePassThroughs;
//   - every INSERT or MERGE into events in a Go string constant (a literal
//     or a constant concatenation, read whole -- constStrings), and in a
//     sqlc query other than
//     CreateEvent -- whose type is its param, read above -- the type taken
//     from a VALUES list that names it as an SQL string literal. One that
//     does not (a parameter, an INSERT ... SELECT, a MERGE) fails: its type
//     cannot be checked.
//
// Every type read must be a sandbox-ws event type
// (contracts/sandbox-ws/v1/events.schema.json) or reserved; every reserved
// type must be written somewhere and must not be a contract type --
// reserving one would drop the agent's own frames of it. It does not see
// SQL assembled at run time, a COPY into events, or a trigger.
func TestServerWrittenEventTypesAreReserved(t *testing.T) {
	t.Parallel()

	contract := sandboxWSEventTypes(t)
	written := map[string][]string{}
	passThroughs := map[string]bool{}
	recordSQL := func(where, sql string) {
		typesInSQL, readable, inserts := eventInsertTypes(sql)
		if !inserts {
			return
		}
		if !readable {
			t.Errorf("%s: an insert into events whose type is not an SQL string literal in its VALUES: write the event through CreateEvent (appendEvent, appendRawEvent), or name the type there, so TestServerWrittenEventTypesAreReserved can check it is reserved", where)
			return
		}
		for _, typ := range typesInSQL {
			written[typ] = append(written[typ], where)
		}
	}
	for _, f := range productionFiles(t) {
		f.constStrings(func(expr ast.Expr, sql string) {
			recordSQL(f.fset.Position(expr.Pos()).String(), sql)
		})
		if f.info == nil {
			continue
		}
		for _, decl := range f.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			site := f.rel + ":" + fn.Name.Name
			record := func(typeExpr ast.Expr) {
				if value, ok := f.constString(typeExpr); ok {
					written[value] = append(written[value], f.fset.Position(typeExpr.Pos()).String())
					return
				}
				if !eventTypePassThroughs[site] {
					t.Errorf("%s: an event stored under a type that is not a constant: name the type, so TestServerWrittenEventTypesAreReserved can check it is reserved", f.fset.Position(typeExpr.Pos()))
					return
				}
				passThroughs[site] = true
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					sel, ok := n.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if m, ok := f.method(sel); ok && m.is(sessionactorPkgPath, "Actor") && (m.name == "appendEvent" || m.name == "appendRawEvent") && len(n.Args) > 2 {
						record(n.Args[2])
					}
				case *ast.CompositeLit:
					if !isNamed(f.info.TypeOf(n), f.module+"/"+sqlcgenPkgPath, "CreateEventParams") {
						return true
					}
					for _, elt := range n.Elts {
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Type" {
								record(kv.Value)
							}
						}
					}
				}
				return true
			})
		}
	}

	queries, err := filepath.Glob(filepath.Join(sandboxStatusModuleRoot(t), "internal", "adapters", "outbound", "postgres", "queries", "*.sql"))
	if err != nil || len(queries) == 0 {
		t.Fatalf("list the sqlc queries: %v (%d files)", err, len(queries))
	}
	sawCreateEvent := false
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
			sql := sqlBlockComment.ReplaceAllString(sqlLineComment.ReplaceAllString(block, ""), "")
			if name == "CreateEvent" {
				sawCreateEvent = insertsEvents.MatchString(sql)
				continue
			}
			recordSQL(path+": query "+name, sql)
		}
	}
	if !sawCreateEvent {
		t.Fatal("the CreateEvent query no longer inserts into events, or is gone: the scan is broken")
	}

	if len(written[SandboxStatusEventType]) == 0 {
		t.Fatalf("no write of %q found: the scan is broken", SandboxStatusEventType)
	}
	for _, typ := range unreservedServerTypes(written, contract) {
		t.Errorf("event type %q (written at %v) is no sandbox-ws event type, so the control plane alone writes it: add it to serverEventTypes (sandboxstatus.go), or a sandbox can store one the page takes for the server's", typ, written[typ])
	}
	for typ := range serverEventTypes {
		if contract[typ] {
			t.Errorf("serverEventTypes reserves %q, a sandbox-ws event type: the agent's own frames of it would be dropped", typ)
		}
		if len(written[typ]) == 0 {
			t.Errorf("serverEventTypes reserves %q, which no production code writes: the scan is broken, or the entry is stale", typ)
		}
	}
	for site := range eventTypePassThroughs {
		if !passThroughs[site] {
			t.Errorf("%s no longer stores an event under a type it was handed: the scan is broken, or eventTypePassThroughs is stale", site)
		}
	}
}

// isNamed reports whether typ is the named type pkgPath.name.
func isNamed(typ types.Type, pkgPath, name string) bool {
	named, ok := types.Unalias(typ).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == pkgPath && named.Obj().Name() == name
}

// sandboxWSEventTypes reads the event types the sandbox-ws contract
// defines: each event $def's constant `type`.
func sandboxWSEventTypes(t *testing.T) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(sandboxStatusModuleRoot(t), "contracts", "sandbox-ws", "v1", "events.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]struct {
			Properties struct {
				Type struct {
					Const string `json:"const"`
				} `json:"type"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(body, &schema); err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, def := range schema.Defs {
		if def.Properties.Type.Const != "" {
			out[def.Properties.Type.Const] = true
		}
	}
	if len(out) < 20 {
		t.Fatalf("read %d sandbox-ws event types, want at least 20: the schema moved, or the read is broken", len(out))
	}
	return out
}

// unreservedServerTypes returns, sorted, the written event types that are
// neither sandbox-ws event types nor reserved.
func unreservedServerTypes(written map[string][]string, contract map[string]bool) []string {
	var out []string
	for typ := range written {
		if !contract[typ] && !serverEventTypes[typ] {
			out = append(out, typ)
		}
	}
	sort.Strings(out)
	return out
}

// eventInsertTypes reads sql the way the reserved-type guard does: whether
// it inserts into events at all, and if so whether every type it inserts
// is readable (sqlInsertedEventTypes), and which.
func eventInsertTypes(sql string) (typeNames []string, readable, inserts bool) {
	if !insertsEvents.MatchString(sql) {
		return nil, false, false
	}
	typeNames, readable = sqlInsertedEventTypes(sql)
	return typeNames, readable, true
}

// TestReservedTypeGuardReadsASplitConstantOnce: an insert into events
// written as a constant split across operands is read whole -- its
// operands alone, an INSERT without its VALUES, are never read as an
// unreadable insert -- so a reserved type split this way is accepted, and
// an unreserved one is still caught.
func TestReservedTypeGuardReadsASplitConstantOnce(t *testing.T) {
	t.Parallel()

	contract := sandboxWSEventTypes(t)
	tests := []struct {
		name           string
		src            string
		wantWritten    []string
		wantUnreserved []string
	}{
		{
			name:        "two operands, a reserved type",
			src:         `const zzProbeInsert = "INSERT INTO events (session_id, type, message_id, payload) " + "VALUES ($1, 'image_decision', $2, $3)"`,
			wantWritten: []string{"image_decision"},
		},
		{
			name:        "three operands, a reserved type",
			src:         `const zzProbeInsert = "INSERT INTO events (session_id, type, message_id, payload) " + "VALUES ($1, " + "'image_decision', $2, $3)"`,
			wantWritten: []string{"image_decision"},
		},
		{
			name:        "parenthesised operands, a contract type",
			src:         `const zzProbeInsert = ("INSERT INTO events (session_id, type) " + "VALUES ($1, ") + "'warning')"`,
			wantWritten: []string{"warning"},
		},
		{
			name:           "two operands, an unreserved type: still caught",
			src:            `const zzProbeInsert = "INSERT INTO events (session_id, type, message_id, payload) " + "VALUES ($1, 'server_note', $2, $3)"`,
			wantWritten:    []string{"server_note"},
			wantUnreserved: []string{"server_note"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := typeCheckedFixture(t, "package fixture\n\n"+tc.src+"\n")
			written := map[string][]string{}
			var unreadable []string
			f.constStrings(func(expr ast.Expr, sql string) {
				typeNames, readable, inserts := eventInsertTypes(sql)
				switch {
				case !inserts:
				case !readable:
					unreadable = append(unreadable, sql)
				default:
					for _, typ := range typeNames {
						written[typ] = append(written[typ], f.fset.Position(expr.Pos()).String())
					}
				}
			})
			if len(unreadable) != 0 {
				t.Errorf("read as an insert whose type cannot be checked: %q", unreadable)
			}
			var gotWritten []string
			for typ := range written {
				gotWritten = append(gotWritten, typ)
			}
			sort.Strings(gotWritten)
			if strings.Join(gotWritten, ",") != strings.Join(tc.wantWritten, ",") {
				t.Errorf("written types = %q, want %q", gotWritten, tc.wantWritten)
			}
			if got := unreservedServerTypes(written, contract); strings.Join(got, ",") != strings.Join(tc.wantUnreserved, ",") {
				t.Errorf("unreserved types = %q, want %q", got, tc.wantUnreserved)
			}
		})
	}
}

// insertsEvents matches an INSERT or a MERGE into the events table, bare,
// "quoted" or public-qualified.
var insertsEvents = regexp.MustCompile(`(?is)\b(?:insert|merge)\s+into\s+(?:(?:"public"|public)\s*\.\s*)?(?:"events"|events\b)`)

// insertEventsValues matches an INSERT into events up to its VALUES,
// capturing the column list.
var insertEventsValues = regexp.MustCompile(`(?is)\binsert\s+into\s+(?:(?:"public"|public)\s*\.\s*)?(?:"events"|events\b)\s*(?:as\s+[a-z_][a-z0-9_]*\s*)?\(([^)]*)\)\s*values\b`)

// sqlStringLiteral is an SQL string literal, optionally cast to text.
var sqlStringLiteral = regexp.MustCompile(`(?is)^'((?:[^']|'')*)'(?:\s*::\s*(?:text|varchar))?$`)

// TestSQLInsertedEventTypes pins what the reserved-type guard reads from
// SQL that inserts into events.
func TestSQLInsertedEventTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sql  string
		want []string
		ok   bool
	}{
		{"a literal type", `INSERT INTO events (session_id, type, message_id, payload) VALUES ($1, 'server_note', gen_random_uuid()::text, '{}')`, []string{"server_note"}, true},
		{"cast, quoted columns, qualified table", `INSERT INTO public.events ("session_id", "type", message_id, payload) VALUES ($1, 'a_note'::text, $2, '{}')`, []string{"a_note"}, true},
		{"several rows", `INSERT INTO events (type, session_id) VALUES ('one', $1), ('it''s', $1)`, []string{"one", "it's"}, true},
		{"a parameter for the type", `INSERT INTO events (session_id, type, message_id, payload) VALUES ($1, $2, $3, $4)`, nil, false},
		{"an insert ... select", `INSERT INTO events (session_id, type) SELECT id, 'x' FROM sessions`, nil, false},
		{"no column list", `INSERT INTO events VALUES ($1, 'x')`, nil, false},
		{"a merge", `MERGE INTO events e USING s ON true WHEN NOT MATCHED THEN INSERT (type) VALUES ('x')`, nil, false},
		{"no type column", `INSERT INTO events (session_id, payload) VALUES ($1, '{}')`, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !insertsEvents.MatchString(tc.sql) {
				t.Fatalf("insertsEvents does not match %q", tc.sql)
			}
			got, ok := sqlInsertedEventTypes(tc.sql)
			if ok != tc.ok || strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("sqlInsertedEventTypes(%q) = %q, %v; want %q, %v", tc.sql, got, ok, tc.want, tc.ok)
			}
		})
	}
	for _, sql := range []string{`SELECT type FROM events`, `INSERT INTO events_archive (type) VALUES ('x')`, `UPDATE events SET type = 'x'`} {
		if insertsEvents.MatchString(sql) {
			t.Errorf("insertsEvents matches %q, which inserts nothing into events", sql)
		}
	}
}

// sqlInsertedEventTypes returns the event types every INSERT into events
// in sql names, and whether each names its type as an SQL string literal
// in a VALUES list -- false for a MERGE, an INSERT ... SELECT, a missing
// column list, or a type that is anything but a literal.
func sqlInsertedEventTypes(sql string) ([]string, bool) {
	inserts := len(insertsEvents.FindAllStringIndex(sql, -1))
	matches := insertEventsValues.FindAllStringSubmatchIndex(sql, -1)
	if len(matches) != inserts {
		return nil, false
	}
	var out []string
	for _, m := range matches {
		typeAt := -1
		for i, col := range splitTopLevel(sql[m[2]:m[3]]) {
			if strings.EqualFold(strings.Trim(strings.TrimSpace(col), `"`), "type") {
				typeAt = i
			}
		}
		if typeAt < 0 {
			return nil, false
		}
		tuples, ok := valuesTuples(sql[m[1]:])
		if !ok {
			return nil, false
		}
		for _, tuple := range tuples {
			values := splitTopLevel(tuple)
			if typeAt >= len(values) {
				return nil, false
			}
			lit := sqlStringLiteral.FindStringSubmatch(strings.TrimSpace(values[typeAt]))
			if lit == nil {
				return nil, false
			}
			out = append(out, strings.ReplaceAll(lit[1], "''", "'"))
		}
	}
	return out, true
}

// valuesTuples returns the inside of each parenthesised tuple a VALUES
// list starts with: s begins just after VALUES.
func valuesTuples(s string) ([]string, bool) {
	var tuples []string
	i := 0
	for {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
		if i >= len(s) || s[i] != '(' {
			return tuples, len(tuples) > 0
		}
		depth, inString, start := 0, false, i+1
		for ; i < len(s); i++ {
			c := s[i]
			if inString {
				inString = c != '\''
				continue
			}
			switch c {
			case '\'':
				inString = true
			case '(':
				depth++
			case ')':
				depth--
			}
			if depth == 0 {
				break
			}
		}
		if i >= len(s) {
			return nil, false
		}
		tuples = append(tuples, s[start:i])
		i++
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
		if i >= len(s) || s[i] != ',' {
			return tuples, true
		}
		i++
	}
}

// splitTopLevel splits s at its commas outside parentheses and quotes.
func splitTopLevel(s string) []string {
	var parts []string
	depth, start := 0, 0
	inString, inIdent := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inString:
			inString = c != '\''
		case inIdent:
			inIdent = c != '"'
		case c == '\'':
			inString = true
		case c == '"':
			inIdent = true
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}
