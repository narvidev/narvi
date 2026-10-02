package ops

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// authzImportPath is the package that owns §31.4's per-repository
// entitlement predicate (authz.RepoAdmission, authz.AuthorizeRepo).
const authzImportPath = "github.com/narvidev/narvi/internal/domain/authz"

// The files and directories ScanRepoEntitlementReads treats specially,
// slash-separated and relative to the scanned root.
const (
	repoEntitlementStoreFile  = "internal/adapters/outbound/postgres/githubprsession_store.go"
	repoEntitlementPostgres   = "internal/adapters/outbound/postgres/"
	repoEntitlementSQLCGen    = "internal/adapters/outbound/postgres/sqlcgen/"
	repoEntitlementAuthz      = "internal/domain/authz/"
	repoEntitlementQueriesDir = "internal/adapters/outbound/postgres/queries"
)

// repoEntitlementRawReads are the two generated queries that read an
// administrator's revocation (§31.4). Rule (a): only the GitHubPRSessionStore
// methods wrapping them call them, so every reader goes through a method
// that returns the revocation with the rest of the decision.
var repoEntitlementRawReads = map[string]bool{
	"ReadRepoEntitlement":        true,
	"FirstRevokedRepoForSession": true,
}

// RepoEntitlementReader is one (file, enclosing function) allowed to call
// GitHubPRSessionStore.RepoEntitlement outside the postgres adapter, and
// why (rule (b)).
type RepoEntitlementReader struct {
	File     string
	Function string
	Reason   string
}

// RepoEntitlementReaders is rule (b)'s allowlist: every caller of
// GitHubPRSessionStore.RepoEntitlement outside the postgres adapter. A new
// path that reads a repository's eligibility directly -- rather than
// through one of the resolvers, which refuse on Revoked before Known --
// fails the guard until it is added here with its reason.
var RepoEntitlementReaders = []RepoEntitlementReader{
	{
		File:     "internal/adapters/inbound/httpapi/repoentitlementgate.go",
		Function: "resolveRepoEntitlement",
		Reason:   "the session-creation resolver every creation path calls: it passes Known and Revoked to authz.AuthorizeRepo, which refuses a revoked repository first",
	},
	{
		File:     "internal/adapters/inbound/httpapi/repoentitlementgate.go",
		Function: "resolveGitHubRepoEntitlement",
		Reason:   "the GitHub-originated branch of the same resolver: the verified payload admits, and a revocation of the pull request's base repository or of the clone URL's repository refuses",
	},
	{
		File:     "internal/adapters/inbound/httpapi/reposettings.go",
		Function: "confirmRepoKnown",
		Reason:   "scopes the repository-scoped admin routes on Known alone, so an administrator can still reach -- and restore -- a revoked repository; it admits no new work",
	},
}

// RepoEntitlementViolation is one place the source breaks §31.4's rule that
// no reader of a repository's eligibility skips an administrator's
// revocation. Rule names the check that caught it:
//
//   - "a": a generated revocation read (ReadRepoEntitlement,
//     FirstRevokedRepoForSession) is called outside
//     postgres.GitHubPRSessionStore's own file.
//   - "b": GitHubPRSessionStore.RepoEntitlement is called outside the
//     postgres adapter from a (file, function) RepoEntitlementReaders does
//     not name.
//   - "c": an authz.RepoAdmission literal outside internal/domain/authz does
//     not set Revoked explicitly (or authz is dot-imported, so one could be
//     written without its package).
//   - "d": a named query reads github_pr_sessions in an EXISTS subquery --
//     the shape of an eligibility read -- without naming
//     repo_entitlement_revocations.
//
// Rules (a) and (b) apply to non-test Go code: a test reads what it
// asserts. Rule (c) applies to every Go file.
type RepoEntitlementViolation struct {
	File   string // slash-separated, relative to the scanned root
	Line   int
	Rule   string
	Detail string
}

func (v RepoEntitlementViolation) String() string {
	return fmt.Sprintf("%s:%d: rule (%s): %s", v.File, v.Line, v.Rule, v.Detail)
}

// ScanRepoEntitlementReads walks every .go file under root (skipping
// testdata, vendor, node_modules and .git) and every named query in root's
// postgres queries directory, and returns every RepoEntitlementViolation,
// sorted. It is the build-time backstop behind §31.4's "Un-entitlement":
// eligibility and revocation are read in one statement, and authz.
// AuthorizeRepo refuses on Revoked first -- this keeps a later reader from
// reading the first without the second. The Go rules match by name on the
// syntax tree, like ScanGitHubOutbound; the SQL rule reads each named
// query's own text, comments removed.
func ScanRepoEntitlementReads(root string) ([]RepoEntitlementViolation, error) {
	allowed := map[string]bool{}
	for _, r := range RepoEntitlementReaders {
		allowed[r.File+"|"+r.Function] = true
	}

	var violations []RepoEntitlementViolation
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", "vendor", "node_modules", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return fmt.Errorf("ops: parse %s: %w", path, parseErr)
		}
		violations = append(violations, scanRepoEntitlementFile(fset, file, rel, strings.HasSuffix(rel, "_test.go"), allowed)...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sqlViolations, err := scanRepoEntitlementQueries(root)
	if err != nil {
		return nil, err
	}
	violations = append(violations, sqlViolations...)

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].File != violations[j].File {
			return violations[i].File < violations[j].File
		}
		if violations[i].Line != violations[j].Line {
			return violations[i].Line < violations[j].Line
		}
		return violations[i].Rule < violations[j].Rule
	})
	return violations, nil
}

// scanRepoEntitlementFile applies rules (a) to (c) to one parsed file.
func scanRepoEntitlementFile(fset *token.FileSet, file *ast.File, rel string, isTest bool, allowed map[string]bool) []RepoEntitlementViolation {
	var out []RepoEntitlementViolation
	add := func(pos token.Pos, rule, detail string) {
		out = append(out, RepoEntitlementViolation{File: rel, Line: fset.Position(pos).Line, Rule: rule, Detail: detail})
	}

	// Every name a package is imported under in this file: a selector on
	// one of these is a package-qualified identifier, never a method call.
	importNames := map[string]bool{}
	authzNames := map[string]bool{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		importNames[name] = true
		if path != authzImportPath {
			continue
		}
		switch name {
		case ".":
			if !strings.HasPrefix(rel, repoEntitlementAuthz) {
				add(imp.Pos(), "c", "internal/domain/authz is dot-imported, so an authz.RepoAdmission literal could be written without its package")
			}
		case "_":
		default:
			authzNames[name] = true
		}
	}

	inAuthz := strings.HasPrefix(rel, repoEntitlementAuthz)
	inPostgres := strings.HasPrefix(rel, repoEntitlementPostgres)

	// isRepoAdmission reports whether t names authz.RepoAdmission.
	isRepoAdmission := func(t ast.Expr) bool {
		sel, ok := t.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && authzNames[pkg.Name] && sel.Sel.Name == "RepoAdmission"
	}
	// checkAdmission applies rule (c) to one RepoAdmission literal.
	checkAdmission := func(lit *ast.CompositeLit) {
		if len(lit.Elts) == 0 {
			add(lit.Pos(), "c", "authz.RepoAdmission{} sets no Revoked -- name it explicitly, read with Known in one statement (§31.4)")
			return
		}
		keyed := false
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			keyed = true
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Revoked" {
				return
			}
		}
		if keyed {
			add(lit.Pos(), "c", "authz.RepoAdmission literal does not set Revoked -- name it explicitly, read with Known in one statement (§31.4)")
		}
		// An unkeyed literal must list every field, Revoked included: the
		// compiler enforces that.
	}

	// The enclosing function of every node, for rule (b).
	var enclosing string
	for _, decl := range file.Decls {
		fn, isFunc := decl.(*ast.FuncDecl)
		enclosing = ""
		if isFunc {
			enclosing = fn.Name.Name
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && importNames[pkg.Name] {
					return true
				}
				if isTest {
					return true
				}
				name := sel.Sel.Name
				if repoEntitlementRawReads[name] && rel != repoEntitlementStoreFile && !strings.HasPrefix(rel, repoEntitlementSQLCGen) {
					add(sel.Sel.Pos(), "a", "."+name+"( called outside "+repoEntitlementStoreFile+" -- read a revocation only through GitHubPRSessionStore.RepoEntitlement or RevokedRepoForSession")
				}
				if name == "RepoEntitlement" && !inPostgres && !allowed[rel+"|"+enclosing] {
					fnName := enclosing
					if fnName == "" {
						fnName = "<package level>"
					}
					add(sel.Sel.Pos(), "b", ".RepoEntitlement( called from "+fnName+", which internal/ops.RepoEntitlementReaders does not name -- admit through httpapi's ResolveRepoEntitlement, or add this reader with its reason")
				}
			case *ast.CompositeLit:
				if inAuthz {
					return true
				}
				if isRepoAdmission(n.Type) {
					checkAdmission(n)
					return true
				}
				// A slice, array or map of authz.RepoAdmission whose
				// element literals elide their type.
				var elem ast.Expr
				switch t := n.Type.(type) {
				case *ast.ArrayType:
					elem = t.Elt
				case *ast.MapType:
					elem = t.Value
				}
				if elem == nil || !isRepoAdmission(elem) {
					return true
				}
				for _, elt := range n.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						elt = kv.Value
					}
					if lit, ok := elt.(*ast.CompositeLit); ok && lit.Type == nil {
						checkAdmission(lit)
					}
				}
			}
			return true
		})
	}
	return out
}

// sqlQueryName matches one sqlc named-query header.
var sqlQueryName = regexp.MustCompile(`(?m)^--\s*name:\s*(\w+)`)

// sqlEligibilityExists matches an EXISTS subquery's opening parenthesis.
var sqlEligibilityExists = regexp.MustCompile(`(?i)\bEXISTS\s*\(`)

// sqlReadsPRSessions matches a top-level FROM or JOIN of github_pr_sessions.
var sqlReadsPRSessions = regexp.MustCompile(`(?i)\b(FROM|JOIN)\s+github_pr_sessions\b`)

// sqlInnerParens matches one innermost parenthesized group.
var sqlInnerParens = regexp.MustCompile(`\([^()]*\)`)

// scanRepoEntitlementQueries applies rule (d) to every named query under
// root's postgres queries directory. A query reads a repository's
// eligibility when one of its EXISTS subqueries selects FROM (or JOINs)
// github_pr_sessions at the subquery's own top level -- a FROM nested
// deeper belongs to another subquery. Such a query must also name
// repo_entitlement_revocations.
func scanRepoEntitlementQueries(root string) ([]RepoEntitlementViolation, error) {
	dir := filepath.Join(root, filepath.FromSlash(repoEntitlementQueriesDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops: read %s: %w", dir, err)
	}
	var out []RepoEntitlementViolation
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("ops: read %s: %w", e.Name(), err)
		}
		rel := repoEntitlementQueriesDir + "/" + e.Name()
		src := string(raw)
		headers := sqlQueryName.FindAllStringSubmatchIndex(src, -1)
		for i, h := range headers {
			end := len(src)
			if i+1 < len(headers) {
				end = headers[i+1][0]
			}
			name := src[h[2]:h[3]]
			body := stripSQLComments(src[h[1]:end])
			if !readsEligibilityInExists(body) || strings.Contains(body, "repo_entitlement_revocations") {
				continue
			}
			line := 1 + strings.Count(src[:h[0]], "\n")
			out = append(out, RepoEntitlementViolation{
				File:   rel,
				Line:   line,
				Rule:   "d",
				Detail: "query " + name + " reads github_pr_sessions in an EXISTS subquery without naming repo_entitlement_revocations -- read eligibility and revocation in one statement (§31.4)",
			})
		}
	}
	return out, nil
}

// stripSQLComments drops every "--" line comment from sql.
func stripSQLComments(sql string) string {
	lines := strings.Split(sql, "\n")
	for i, line := range lines {
		if idx := strings.Index(line, "--"); idx >= 0 {
			lines[i] = line[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

// readsEligibilityInExists reports whether body has an EXISTS subquery
// whose own top level reads github_pr_sessions.
func readsEligibilityInExists(body string) bool {
	for _, loc := range sqlEligibilityExists.FindAllStringIndex(body, -1) {
		open := loc[1] - 1
		depth := 0
		closeAt := -1
		for i := open; i < len(body); i++ {
			switch body[i] {
			case '(':
				depth++
			case ')':
				depth--
			}
			if depth == 0 {
				closeAt = i
				break
			}
		}
		if closeAt < 0 {
			continue
		}
		inner := body[open+1 : closeAt]
		for sqlInnerParens.MatchString(inner) {
			inner = sqlInnerParens.ReplaceAllString(inner, " ")
		}
		if sqlReadsPRSessions.MatchString(inner) {
			return true
		}
	}
	return false
}
