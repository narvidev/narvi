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
	repoEntitlementAuthz      = "internal/domain/authz/"
	repoEntitlementQueriesDir = "internal/adapters/outbound/postgres/queries"
)

// repoEntitlementRawReads are the two generated queries that read an
// administrator's revocation (§31.4), each with the one
// GitHubPRSessionStore method allowed to name it (rule (a)): every reader
// goes through a method that returns the revocation with the rest of the
// decision, and no other method of the store -- a Known-only wrapper
// included -- reads the query itself.
var repoEntitlementRawReads = map[string]string{
	"ReadRepoEntitlement":        "RepoEntitlement",
	"FirstRevokedRepoForSession": "RevokedRepoForSession",
}

// RepoEntitlementReader is one (file, enclosing function) allowed to name
// GitHubPRSessionStore.RepoEntitlement, and why (rule (b)).
type RepoEntitlementReader struct {
	File     string
	Function string
	Reason   string
}

// RepoEntitlementReaders is rule (b)'s allowlist: every place in non-test
// code -- the postgres adapter included -- that names
// GitHubPRSessionStore.RepoEntitlement, as a call or a method value. A new
// path that reads a repository's eligibility directly, rather than
// through one of the resolvers that refuse on Revoked before Known, fails
// the guard until it is added here with its reason.
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

// RepoEntitlementQueryExemption is one named query allowed to read
// github_pr_sessions without naming repo_entitlement_revocations, and why
// (rule (d)).
type RepoEntitlementQueryExemption struct {
	Query  string
	Reason string
}

// RepoEntitlementQueryExemptions is rule (d)'s allowlist: every named query
// that reads github_pr_sessions and is not an eligibility read. A new query
// reading that table -- in any shape: EXISTS, IN, a JOIN, a comma join, a
// count -- either reads repo_entitlement_revocations in the same statement
// or is added here with its reason.
var RepoEntitlementQueryExemptions = []RepoEntitlementQueryExemption{
	{Query: "LockGitHubPRSessionForUpdate", Reason: "the GitHub ingress claim lock on one pull request's row; the mention was admitted, revocation included, before the claim transaction opened"},
	{Query: "GetGitHubPRSessionBySessionID", Reason: "reads the pull request a known review session belongs to; it admits nothing, and that session's turns are refused by the actor's revocation re-reads"},
	{Query: "GetGitHubPRSessionByRepoAndPRNumber", Reason: "reads one pull request's claim row for its review state; it decides nothing about whether a repository may start new work"},
	{Query: "ListSlackChannelsForRepoSince", Reason: "the daily digest's channel scoping: reports past review activity of a repository, never admits new work"},
	{Query: "ListLinearOrganizationsForRepoSince", Reason: "the daily digest's organization scoping: reports past review activity of a repository, never admits new work"},
	{Query: "ListDistinctReposWithRecentSessions", Reason: "the daily digest's repository enumeration: reports past review activity, never admits new work"},
	{Query: "GetSessionGuardFacts", Reason: "the session guard's read of the spend caps a session's repositories set (technical plan §40.1): it reads a review session's claim to find its base repository's cap, and only ever refuses a turn, never admits work a revocation would refuse -- the actor's revocation re-reads still apply"},
	{Query: "MoveReviewSessionToBaseRepository", Reason: "moves a legacy review session's spec onto the base repository its own claim names (technical plan §30.4), in the spawn transaction before the spawn's revocation re-read (refuseIfRepoRevoked), which then reads the moved spec and the claim; it admits nothing"},
}

// RepoEntitlementViolation is one place the source breaks §31.4's rule that
// a reader of a repository's eligibility does not skip an administrator's
// revocation, in one of the shapes this guard checks. Rule names the check
// that caught it:
//
//   - "a": a generated revocation read (ReadRepoEntitlement,
//     FirstRevokedRepoForSession) is named -- called or taken as a method
//     value -- anywhere but its one GitHubPRSessionStore method.
//   - "b": GitHubPRSessionStore.RepoEntitlement is named from a (file,
//     function) RepoEntitlementReaders does not list -- the postgres
//     adapter included.
//   - "c": an authz.RepoAdmission is built without naming Revoked: a
//     literal (an element literal with its type elided, a pointer's
//     included) that does not set it, a var declared of the type,
//     new(authz.RepoAdmission), or a type declared from it, which would
//     escape the literal check; or authz is dot-imported. Inside package
//     authz the type's bare name is checked the same way, outside its test
//     files.
//   - "d": a named query reads github_pr_sessions -- in any position of a
//     statement, schema-qualified or quoted, comments of either kind
//     removed -- without naming repo_entitlement_revocations, and
//     RepoEntitlementQueryExemptions does not list it; or an exemption names
//     a query that no longer reads the table, or one that names the
//     revocations table and so needs no exemption.
//
// Rules (a) and (b) apply to non-test Go code: a test reads what it
// asserts. Rule (c) applies to every Go file but authz's own tests.
//
// What the guard cannot see, by its nature as a name-based check on
// syntax: a RepoEntitlementFacts read only for Known by an allowlisted
// function, a struct field or embedding of authz.RepoAdmission left at its
// zero value, reflection, and a read of github_pr_sessions through a view
// or function a migration defines rather than a named query.
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
// eligibility and revocation are read in one statement, and
// authz.AuthorizeRepo refuses on Revoked first; this keeps a later reader
// from reading the first without the second in the shapes
// RepoEntitlementViolation lists. The Go rules match by name on the syntax
// tree, like ScanGitHubOutbound; the SQL rule reads each named query's own
// text.
func ScanRepoEntitlementReads(root string) ([]RepoEntitlementViolation, error) {
	return scanRepoEntitlementReads(root, RepoEntitlementQueryExemptions)
}

// scanRepoEntitlementReads is ScanRepoEntitlementReads with rule (d)'s
// exemptions given, so a synthetic tree can be checked against its own.
func scanRepoEntitlementReads(root string, exemptions []RepoEntitlementQueryExemption) ([]RepoEntitlementViolation, error) {
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

	sqlViolations, err := scanRepoEntitlementQueries(root, exemptions)
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

	inAuthz := strings.HasPrefix(rel, repoEntitlementAuthz) && !strings.Contains(strings.TrimPrefix(rel, repoEntitlementAuthz), "/")
	checkAdmissions := !inAuthz || !isTest

	// Every name a package is imported under in this file: a selector on
	// one of these is a package-qualified identifier, never a method.
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
			if checkAdmissions {
				add(imp.Pos(), "c", "internal/domain/authz is dot-imported, so an authz.RepoAdmission could be built without its package")
			}
		case "_":
		default:
			authzNames[name] = true
		}
	}

	// isRepoAdmission reports whether t names authz.RepoAdmission -- its
	// bare name inside package authz itself.
	isRepoAdmission := func(t ast.Expr) bool {
		switch t := t.(type) {
		case *ast.SelectorExpr:
			pkg, ok := t.X.(*ast.Ident)
			return ok && authzNames[pkg.Name] && t.Sel.Name == "RepoAdmission"
		case *ast.Ident:
			return inAuthz && t.Name == "RepoAdmission"
		}
		return false
	}
	// isRepoAdmissionElem is isRepoAdmission for a container's element
	// type, a pointer to the type included.
	isRepoAdmissionElem := func(t ast.Expr) bool {
		if star, ok := t.(*ast.StarExpr); ok {
			t = star.X
		}
		return isRepoAdmission(t)
	}
	// checkLiteral applies rule (c) to one RepoAdmission literal.
	checkLiteral := func(lit *ast.CompositeLit) {
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

	for _, decl := range file.Decls {
		enclosing := ""
		if fn, ok := decl.(*ast.FuncDecl); ok {
			enclosing = fn.Name.Name
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := n.X.(*ast.Ident); ok && importNames[pkg.Name] {
					return true
				}
				if isTest {
					return true
				}
				name := n.Sel.Name
				if method, raw := repoEntitlementRawReads[name]; raw && (rel != repoEntitlementStoreFile || enclosing != method) {
					add(n.Sel.Pos(), "a", "."+name+" named outside "+repoEntitlementStoreFile+"'s "+method+" -- read a revocation only through GitHubPRSessionStore.RepoEntitlement or RevokedRepoForSession")
				}
				if name == "RepoEntitlement" && !allowed[rel+"|"+enclosing] {
					fnName := enclosing
					if fnName == "" {
						fnName = "<package level>"
					}
					add(n.Sel.Pos(), "b", ".RepoEntitlement named in "+fnName+", which internal/ops.RepoEntitlementReaders does not list -- admit through httpapi's ResolveRepoEntitlement, or add this reader with its reason")
				}
			case *ast.CompositeLit:
				if !checkAdmissions {
					return true
				}
				if isRepoAdmission(n.Type) {
					checkLiteral(n)
					return true
				}
				// A slice, array or map of authz.RepoAdmission (or of
				// pointers to it) whose element literals elide their type:
				// an elided *T element is a bare {...} in the AST too.
				var elem ast.Expr
				switch t := n.Type.(type) {
				case *ast.ArrayType:
					elem = t.Elt
				case *ast.MapType:
					elem = t.Value
				}
				if elem == nil || !isRepoAdmissionElem(elem) {
					return true
				}
				for _, elt := range n.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						elt = kv.Value
					}
					if lit, ok := elt.(*ast.CompositeLit); ok && lit.Type == nil {
						checkLiteral(lit)
					}
				}
			case *ast.CallExpr:
				if !checkAdmissions {
					return true
				}
				if fn, ok := n.Fun.(*ast.Ident); ok && fn.Name == "new" && len(n.Args) == 1 && isRepoAdmission(n.Args[0]) {
					add(n.Pos(), "c", "new(authz.RepoAdmission) sets no Revoked -- build it as a literal that names Revoked (§31.4)")
				}
			case *ast.ValueSpec:
				if checkAdmissions && n.Type != nil && isRepoAdmission(n.Type) {
					add(n.Pos(), "c", "a var of type authz.RepoAdmission starts with no Revoked -- build it as a literal that names Revoked (§31.4)")
				}
			case *ast.TypeSpec:
				if checkAdmissions && isRepoAdmissionElem(n.Type) {
					add(n.Pos(), "c", "a type declared from authz.RepoAdmission escapes the literal check -- use authz.RepoAdmission itself (§31.4)")
				}
			}
			return true
		})
	}
	return out
}

// sqlQueryName matches one sqlc named-query header.
var sqlQueryName = regexp.MustCompile(`(?m)^--\s*name:\s*(\w+)`)

// sqlPRSessionsTable matches one reference to the github_pr_sessions
// table: bare, quoted, or schema-qualified.
var sqlPRSessionsTable = regexp.MustCompile(`(?i)(?:"?\w+"?\s*\.\s*)?"?github_pr_sessions"?\b`)

// sqlWriteTarget matches the end of the text that makes a table reference
// the target of a write rather than a read.
var sqlWriteTarget = regexp.MustCompile(`(?i)\b(INSERT\s+INTO|UPDATE|DELETE\s+FROM)\s*$`)

// sqlBlockComment matches one /* */ comment.
var sqlBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)

// scanRepoEntitlementQueries applies rule (d) to every named query under
// root's postgres queries directory.
func scanRepoEntitlementQueries(root string, exemptions []RepoEntitlementQueryExemption) ([]RepoEntitlementViolation, error) {
	dir := filepath.Join(root, filepath.FromSlash(repoEntitlementQueriesDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops: read %s: %w", dir, err)
	}
	exempt := map[string]bool{}
	for _, e := range exemptions {
		exempt[e.Query] = true
	}
	readers := map[string]bool{}
	readsRevocations := map[string]bool{}

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
			if !readsPRSessions(body) {
				continue
			}
			readers[name] = true
			if strings.Contains(strings.ToLower(body), "repo_entitlement_revocations") {
				readsRevocations[name] = true
				continue
			}
			if exempt[name] {
				continue
			}
			line := 1 + strings.Count(src[:h[0]], "\n")
			out = append(out, RepoEntitlementViolation{
				File:   rel,
				Line:   line,
				Rule:   "d",
				Detail: "query " + name + " reads github_pr_sessions without naming repo_entitlement_revocations -- read eligibility and revocation in one statement (§31.4), or list the query in internal/ops.RepoEntitlementQueryExemptions with its reason",
			})
		}
	}
	for _, e := range exemptions {
		switch {
		case !readers[e.Query]:
			out = append(out, RepoEntitlementViolation{
				File:   repoEntitlementQueriesDir,
				Rule:   "d",
				Detail: "internal/ops.RepoEntitlementQueryExemptions lists " + e.Query + ", which is no named query reading github_pr_sessions: drop the stale entry",
			})
		case readsRevocations[e.Query]:
			out = append(out, RepoEntitlementViolation{
				File:   repoEntitlementQueriesDir,
				Rule:   "d",
				Detail: "internal/ops.RepoEntitlementQueryExemptions lists " + e.Query + ", which already reads repo_entitlement_revocations and needs no exemption: drop the stale entry",
			})
		}
	}
	return out, nil
}

// stripSQLComments drops every /* */ block comment and every "--" line
// comment from sql.
func stripSQLComments(sql string) string {
	sql = sqlBlockComment.ReplaceAllString(sql, " ")
	lines := strings.Split(sql, "\n")
	for i, line := range lines {
		if idx := strings.Index(line, "--"); idx >= 0 {
			lines[i] = line[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

// readsPRSessions reports whether body reads github_pr_sessions: any
// reference to the table that is not the target of an INSERT, UPDATE or
// DELETE.
func readsPRSessions(body string) bool {
	for _, loc := range sqlPRSessionsTable.FindAllStringIndex(body, -1) {
		if loc[0] > 0 {
			prev := body[loc[0]-1]
			if prev == '_' || (prev >= 'a' && prev <= 'z') || (prev >= 'A' && prev <= 'Z') || (prev >= '0' && prev <= '9') {
				continue // part of a longer identifier
			}
		}
		if sqlWriteTarget.MatchString(body[:loc[0]]) {
			continue
		}
		return true
	}
	return false
}
