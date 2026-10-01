package sessionactor

import (
	"bufio"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"golang.org/x/tools/go/packages"
)

// sandboxStatusParams are the params of the four queries that write a
// sandbox's status or generation, one per sandboxWriter method.
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

// sandboxStatusStoreMethods are the postgres.SandboxStore methods that run
// those queries: the four sandboxWriter wraps, and Create, which no
// production code may call.
var sandboxStatusStoreMethods = map[string]bool{
	"UpdateStatus":          true,
	"UpdateStatusToSuspect": true,
	"RecoverFromSuspect":    true,
	"UpsertForSpawn":        true,
	"Create":                true,
}

// The files that may touch the writes directly: the store, which runs the
// queries, and the recorder, which wraps the store.
const (
	sandboxStoreFile    = "internal/adapters/outbound/postgres/sandbox_store.go"
	sandboxRecorderFile = "internal/app/sessionactor/sandboxstatus.go"
)

// sandboxStatusOwners may name the params in a signature.
var sandboxStatusOwners = map[string]bool{
	sandboxStoreFile:    true,
	sandboxRecorderFile: true,
}

// TestSandboxStatusWritesGoThroughTheRecorder keeps the sandbox_status
// event whole at the source (technical plan §3.2, §6.2): an open page
// learns a sandbox's new status only from that event, which transact
// appends for a write made through sandboxWrites. It type-checks every
// non-test package under internal, controlplane, cmd and extension
// (go/packages; generated sqlcgen code aside) and fails on:
//
//   - a call or method value of postgres.SandboxStore's UpdateStatus,
//     UpdateStatusToSuspect, RecoverFromSuspect or UpsertForSpawn outside
//     the recorder, or of its Create anywhere -- by the receiver's type,
//     whatever the variable is called, through WithTx, a constructor
//     chain or an embedding;
//   - a call or method value of a sqlcgen.Queries method that writes the
//     status or the generation (sandboxStatusQueries) outside the store,
//     sqlcgen.New(tx).CreateSandbox included;
//   - an identifier naming one of the four queries' params, unless it
//     types a composite literal handed straight to a sandboxWriter method,
//     or sits in a signature in the store or the recorder;
//   - a string literal, or a constant string concatenation, that updates
//     or inserts or merges into sandboxes: UPDATE [ONLY] or INSERT INTO or
//     MERGE INTO, the table bare, "quoted" or public-qualified.
//
// A file the build constraints leave out of the type-checked load (none
// writes sandboxes today) gets the last two checks only. It does not see
// SQL assembled at run time, or a write through reflection.
// TestSandboxStatusQueriesAreTheRecordedOnes covers the sqlc queries.
func TestSandboxStatusWritesGoThroughTheRecorder(t *testing.T) {
	t.Parallel()

	recorded := 0
	for _, f := range productionFiles(t) {
		recorded += checkSandboxStatusWrites(t, f)
	}
	// The fourteen writes the session actor makes (dispatch, sandbox
	// events, stop, timers). Fewer means the scan no longer sees them.
	if recorded < 14 {
		t.Fatalf("found %d sandbox status writes through sandboxWrites, want at least 14: the scan is broken", recorded)
	}
}

// rawSandboxesWrite matches SQL that updates, inserts or merges into
// sandboxes, the table bare, "quoted" or schema-qualified.
var rawSandboxesWrite = regexp.MustCompile(`(?i)\b(update(\s+only)?|insert\s+into|merge\s+into)\s+("?public"?\s*\.\s*)?"?sandboxes"?(\s|\(|\*|;|$)`)

func checkSandboxStatusWrites(t *testing.T, f productionFile) int {
	t.Helper()
	fset := f.fset
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
	ast.Inspect(f.file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			if sandboxStatusOwners[f.rel] {
				approve(n.Type)
			}
		case *ast.CallExpr:
			// Only the recorder's own methods take the params literal.
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok {
				if m, ok := f.method(sel); ok && m.is(sessionactorPkgPath, "sandboxWriter") {
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
			case m.is(postgresPkgPath, "SandboxStore") && m.name == "Create":
				t.Errorf("%s: SandboxStore.Create in production code: a sandbox it creates is never reported to an open page -- create it through sandboxWrites(tx).UpsertForSpawn", fset.Position(n.Pos()))
			case m.is(postgresPkgPath, "SandboxStore") && sandboxStatusStoreMethods[m.name] && f.rel != sandboxRecorderFile:
				t.Errorf("%s: SandboxStore.%s outside the recorder: a status it writes reaches no open page (technical plan §3.2) -- call a.sandboxWrites(tx).%s", fset.Position(n.Pos()), m.name, m.name)
			case m.is(sqlcgenPkgPath, "Queries") && sandboxStatusQueries[m.name] && f.rel != sandboxStoreFile:
				t.Errorf("%s: the %s query outside the sandbox store: a status it writes reaches no open page (technical plan §3.2) -- write it through the store and sandboxWrites", fset.Position(n.Pos()), m.name)
			}
		}
		return true
	})

	ast.Inspect(f.file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Ident:
			if sandboxStatusParams[n.Name] && !approved[n.Pos()] {
				t.Errorf("%s: a %s value outside sandboxWrites: a sandbox status written this way reaches no open page (technical plan §3.2) -- hand the literal straight to a.sandboxWrites(tx)", fset.Position(n.Pos()), n.Name)
			}
		case *ast.BasicLit, *ast.BinaryExpr:
			if sql, ok := f.constString(n.(ast.Expr)); ok && rawSandboxesWrite.MatchString(sql) {
				t.Errorf("%s: raw SQL writing sandboxes: a status it changes reaches no open page (technical plan §3.2) -- write it through the sandbox store and sandboxWrites", fset.Position(n.Pos()))
			}
		}
		return true
	})
	return recorded
}

var (
	sqlLineComment  = regexp.MustCompile(`--[^\n]*`)
	sqlBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	// sandboxesTable is the sandboxes table as a statement may name it:
	// bare, "quoted", or qualified by the public schema, quoted or not.
	sandboxesTable = `(?:(?:"public"|public)\s*\.\s*)?(?:"sandboxes"|sandboxes\b)`
	// insertsSandboxes matches an INSERT or a MERGE into sandboxes: a new
	// row has a status and a generation, so either counts as writing them.
	insertsSandboxes = regexp.MustCompile(`(?is)\b(?:insert|merge)\s+into\s+(?:only\s+)?` + sandboxesTable)
	// updateSandboxesSet matches an UPDATE of sandboxes up to its SET, an
	// alias between them allowed; the SET clause starts at the match's end.
	updateSandboxesSet = regexp.MustCompile(`(?is)\bupdate\s+(?:only\s+)?` + sandboxesTable + `(?:\s*\*)?(?:\s+(?:as\s+)?[a-z_][a-z0-9_]*)?\s+set\b`)
)

// TestSandboxStatusQueriesAreTheRecordedOnes is the SQL half of
// TestSandboxStatusWritesGoThroughTheRecorder: the sqlc queries that write
// sandboxes.status or sandboxes.gen are exactly the ones sandboxWriter
// wraps, plus CreateSandbox. A query writes them when it inserts or merges
// into sandboxes, or updates sandboxes -- with or without ONLY, the table
// bare, "quoted" or public-qualified, aliased or not -- with a SET whose
// targets name status or gen, singly (status = ...) or in a column list
// ((status, pre_suspect_status) = ...), quoted or not. A new query of that
// kind gets params of its own, or none, which the Go scan cannot see:
// route it through sandboxWrites, then add it here. Comments are ignored;
// SQL assembled at run time is not seen.
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
			if !sqlWritesSandboxStatus(sqlBlockComment.ReplaceAllString(sqlLineComment.ReplaceAllString(block, ""), "")) {
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

// TestSQLWritesSandboxStatus pins the statement shapes the SQL guard reads
// as a status or generation write, and the ones it must not.
func TestSQLWritesSandboxStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sql  string
		want bool
	}{
		{"a plain update", `UPDATE sandboxes SET status = 'failed' WHERE session_id = $1`, true},
		{"the generation", `UPDATE sandboxes SET last_seen_at = now(), gen = gen + 1 WHERE session_id = $1`, true},
		{"only", `UPDATE ONLY sandboxes SET status = 'stopped' WHERE session_id = $1`, true},
		{"schema-qualified", `UPDATE public.sandboxes SET status = 'failed' WHERE session_id = $1`, true},
		{"quoted", `UPDATE "public"."sandboxes" SET "status" = 'failed' WHERE session_id = $1`, true},
		{"aliased", `UPDATE sandboxes AS s SET status = 'failed' WHERE s.session_id = $1`, true},
		{"a column list", `UPDATE sandboxes SET (status, pre_suspect_status) = ('stopped', NULL) WHERE session_id = $1`, true},
		{"a column list naming gen second", `UPDATE sandboxes SET (last_seen_at, gen) = (now(), 2) WHERE session_id = $1`, true},
		{"inside a CTE", `WITH s AS (UPDATE sandboxes SET status = 'stopped' WHERE session_id = $1 RETURNING *) SELECT * FROM s`, true},
		{"an insert", `INSERT INTO sandboxes (session_id) VALUES ($1)`, true},
		{"a qualified insert", `INSERT INTO public.sandboxes (session_id) VALUES ($1)`, true},
		{"a merge", `MERGE INTO sandboxes s USING x ON s.session_id = x.id WHEN MATCHED THEN UPDATE SET last_seen_at = now()`, true},
		{"another column only", `UPDATE sandboxes SET last_seen_at = now() WHERE session_id = $1`, false},
		{"status read on the right only", `UPDATE sandboxes SET pre_suspect_status = status WHERE session_id = $1`, false},
		{"status read in a function on the right", `UPDATE sandboxes SET last_seen_at = COALESCE(sqlc.narg('x'), last_seen_at), pre_suspect_status = NULLIF(status, 'suspect') WHERE status <> 'failed'`, false},
		{"another table's status", `UPDATE sessions SET status = 'failed' WHERE id = $1`, false},
		{"a table whose name starts with sandboxes", `UPDATE sandboxes_archive SET status = 'failed'`, false},
		{"another table's status beside a sandboxes liveness bump", `WITH s AS (UPDATE sandboxes SET last_seen_at = now() WHERE session_id = $1) UPDATE sessions SET status = 'active' WHERE id = $1`, false},
		{"a read", `SELECT status FROM sandboxes WHERE session_id = $1`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sqlWritesSandboxStatus(tc.sql); got != tc.want {
				t.Errorf("sqlWritesSandboxStatus(%q) = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}

// sqlWritesSandboxStatus reports whether sql (comments already stripped)
// inserts or merges into sandboxes, or updates it with a SET naming status
// or gen among its targets.
func sqlWritesSandboxStatus(sql string) bool {
	if insertsSandboxes.MatchString(sql) {
		return true
	}
	for _, loc := range updateSandboxesSet.FindAllStringIndex(sql, -1) {
		for _, target := range setTargets(sql[loc[1]:]) {
			if target == "status" || target == "gen" {
				return true
			}
		}
	}
	return false
}

// setTargets returns the columns a SET clause assigns, lower-cased and
// unquoted: the text before the first top-level "=" of each top-level,
// comma-separated assignment, a parenthesised column list split into its
// names. The clause ends at a top-level WHERE, RETURNING or FROM, a ";",
// or the ")" closing the statement it sits in.
func setTargets(clause string) []string {
	var assignments []string
	depth, start := 0, 0
	inString, inIdent := false, false
	end := len(clause)
scan:
	for i := 0; i < len(clause); i++ {
		c := clause[i]
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
			if depth == 0 {
				end = i
				break scan
			}
			depth--
		case c == ';' && depth == 0:
			end = i
			break scan
		case c == ',' && depth == 0:
			assignments = append(assignments, clause[start:i])
			start = i + 1
		case depth == 0 && isWordStart(clause, i) && (hasKeyword(clause[i:], "where") || hasKeyword(clause[i:], "returning") || hasKeyword(clause[i:], "from")):
			end = i
			break scan
		}
	}
	if start < end {
		assignments = append(assignments, clause[start:end])
	}

	var targets []string
	for _, a := range assignments {
		eq := strings.IndexByte(a, '=')
		if eq < 0 {
			continue
		}
		lhs := strings.TrimSpace(a[:eq])
		lhs = strings.TrimSuffix(strings.TrimPrefix(lhs, "("), ")")
		for _, col := range strings.Split(lhs, ",") {
			targets = append(targets, strings.ToLower(strings.Trim(strings.TrimSpace(col), `"`)))
		}
	}
	return targets
}

func isWordStart(s string, i int) bool {
	return i == 0 || !isWordByte(s[i-1])
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// hasKeyword reports whether s starts with keyword as a whole word.
func hasKeyword(s, keyword string) bool {
	return len(s) >= len(keyword) && strings.EqualFold(s[:len(keyword)], keyword) && (len(s) == len(keyword) || !isWordByte(s[len(keyword)]))
}

// sandboxStatusModuleRoot walks up from the package directory to go.mod.
func sandboxStatusModuleRoot(t *testing.T) string {
	t.Helper()
	root, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above the package directory")
		}
		dir = parent
	}
}

// The packages whose types the scans recognise, under the module path.
var (
	sessionactorPkgPath = "internal/app/sessionactor"
	postgresPkgPath     = "internal/adapters/outbound/postgres"
	sqlcgenPkgPath      = "internal/adapters/outbound/postgres/sqlcgen"
)

// productionFile is one non-test Go file of the module's production trees,
// with its package's type information -- nil info for a file the build
// constraints leave out of the load.
type productionFile struct {
	rel    string
	file   *ast.File
	fset   *token.FileSet
	info   *types.Info
	module string
}

// methodRef is the method a selector names: its receiver's package (under
// the module) and type name, and the method's own name.
type methodRef struct {
	pkg, recv, name string
}

func (m methodRef) is(pkg, recv string) bool { return m.pkg == pkg && m.recv == recv }

// method resolves sel to the method it calls or takes as a value, through
// the type checker: the variable's name plays no part.
func (f productionFile) method(sel *ast.SelectorExpr) (methodRef, bool) {
	if f.info == nil {
		return methodRef{}, false
	}
	s, ok := f.info.Selections[sel]
	if !ok || (s.Kind() != types.MethodVal && s.Kind() != types.MethodExpr) {
		return methodRef{}, false
	}
	fn, ok := s.Obj().(*types.Func)
	if !ok {
		return methodRef{}, false
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return methodRef{}, false
	}
	recv := types.Unalias(sig.Recv().Type())
	if p, ok := recv.(*types.Pointer); ok {
		recv = types.Unalias(p.Elem())
	}
	named, ok := recv.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return methodRef{}, false
	}
	pkg := strings.TrimPrefix(named.Obj().Pkg().Path(), f.module+"/")
	return methodRef{pkg: pkg, recv: named.Obj().Name(), name: fn.Name()}, true
}

// constString is expr's value when it is a constant string: a literal, or,
// with type information, any constant expression such as a concatenation.
func (f productionFile) constString(expr ast.Expr) (string, bool) {
	if f.info != nil {
		if tv, ok := f.info.Types[expr]; ok && tv.Value != nil && tv.Value.Kind() == constant.String {
			return constant.StringVal(tv.Value), true
		}
		return "", false
	}
	if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		return lit.Value, true
	}
	return "", false
}

// productionTrees are the module's directories holding production code.
var productionTrees = []string{"internal", "controlplane", "cmd", "extension"}

// loadedProduction is the type-checked load every scan in this package
// shares: one go/packages load per test binary.
var loadedProduction = sync.OnceValues(func() ([]productionFile, error) {
	root, err := moduleRoot()
	if err != nil {
		return nil, err
	}
	module, err := modulePath(root)
	if err != nil {
		return nil, err
	}
	var patterns []string
	for _, top := range productionTrees {
		if _, err := os.Stat(filepath.Join(root, top)); err == nil {
			patterns = append(patterns, "./"+top+"/...")
		}
	}
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  root,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("load the production packages: %w", err)
	}
	var loadErrs []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			loadErrs = append(loadErrs, e.Error())
		}
	})
	if len(loadErrs) > 0 {
		return nil, fmt.Errorf("the production packages do not type-check: %s", strings.Join(loadErrs, "; "))
	}

	rel := func(path string) (string, error) {
		r, err := filepath.Rel(root, path)
		return filepath.ToSlash(r), err
	}
	var files []productionFile
	seen := map[string]bool{}
	ignored := map[string]bool{}
	for _, p := range pkgs {
		for _, path := range p.IgnoredFiles {
			r, err := rel(path)
			if err != nil {
				return nil, err
			}
			ignored[r] = true
		}
		if strings.HasSuffix(p.PkgPath, "/sqlcgen") {
			for _, path := range p.GoFiles {
				r, err := rel(path)
				if err != nil {
					return nil, err
				}
				seen[r] = true
			}
			continue
		}
		for _, file := range p.Syntax {
			r, err := rel(p.Fset.Position(file.Pos()).Filename)
			if err != nil {
				return nil, err
			}
			seen[r] = true
			files = append(files, productionFile{rel: r, file: file, fset: p.Fset, info: p.TypesInfo, module: module})
		}
	}

	// Every non-test Go file in the trees is either in the typed load or
	// left out of it by its build constraints; one in neither means the
	// load missed a package. A left-out file is parsed alone, untyped.
	fset := token.NewFileSet()
	for _, top := range productionTrees {
		dir := filepath.Join(root, top)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" || d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			r, err := rel(path)
			if err != nil {
				return err
			}
			switch {
			case seen[r]:
			case ignored[r]:
				file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
				if err != nil {
					return err
				}
				files = append(files, productionFile{rel: r, file: file, fset: fset, module: module})
			default:
				return fmt.Errorf("%s is in no loaded package: the type-checked load missed it", r)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, nil
})

func productionFiles(t *testing.T) []productionFile {
	t.Helper()
	files, err := loadedProduction()
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// modulePath reads the module path from root's go.mod.
func modulePath(root string) (string, error) {
	f, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "module "); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", errors.New("go.mod names no module")
}
