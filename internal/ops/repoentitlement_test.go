package ops

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestRepoEntitlementGuard is the backstop behind §31.4's "Un-entitlement",
// run against this repository's real tree: a revocation is read only
// through GitHubPRSessionStore's own methods, RepoEntitlement is called
// only by the readers RepoEntitlementReaders names, every
// authz.RepoAdmission literal sets Revoked, and no named query reads
// github_pr_sessions' eligibility shape without the revocations table.
func TestRepoEntitlementGuard(t *testing.T) {
	violations, err := ScanRepoEntitlementReads(repoRoot(t))
	if err != nil {
		t.Fatalf("ScanRepoEntitlementReads: %v", err)
	}
	if len(violations) > 0 {
		lines := make([]string, len(violations))
		for i, v := range violations {
			lines[i] = v.String()
		}
		t.Errorf("%d repository entitlement violation(s) -- no reader of a repository's eligibility may skip an administrator's revocation (technical plan §31.4):\n\t%s",
			len(violations), strings.Join(lines, "\n\t"))
	}
}

// TestRepoEntitlementReaders_EachNamesARealFunctionWithAReason keeps rule
// (b)'s allowlist honest: every entry names a function that exists in the
// file it names and still calls RepoEntitlement, and says why.
func TestRepoEntitlementReaders_EachNamesARealFunctionWithAReason(t *testing.T) {
	root := repoRoot(t)
	for _, r := range RepoEntitlementReaders {
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(r.File)))
		if err != nil {
			t.Errorf("%s: %v", r.File, err)
			continue
		}
		if !strings.Contains(string(src), "func "+r.Function+"(") {
			t.Errorf("%s names %s, which it does not declare", r.File, r.Function)
		}
		if !strings.Contains(string(src), ".RepoEntitlement(") {
			t.Errorf("%s no longer calls RepoEntitlement: drop its entry", r.File)
		}
		if len(strings.Fields(r.Reason)) < 8 {
			t.Errorf("%s|%s: reason %q does not say why", r.File, r.Function, r.Reason)
		}
	}
}

// TestScanRepoEntitlementReads_Rules proves each rule fires on the shape it
// exists for, and stays quiet on the shapes it allows, over a synthetic
// tree per case. The cases exemptCases names are checked against one rule
// (d) exemption, GetGitHubPRSessionBySessionID.
func TestScanRepoEntitlementReads_Rules(t *testing.T) {
	const imp = `import "github.com/narvidev/narvi/internal/domain/authz"` + "\n"
	const store = "internal/adapters/outbound/postgres/githubprsession_store.go"
	const query = "internal/adapters/outbound/postgres/queries/x.sql"
	exemptions := []RepoEntitlementQueryExemption{{Query: "GetGitHubPRSessionBySessionID", Reason: "a test exemption"}}
	exemptCases := map[string]bool{
		"d: an exempt read, beside a write and an unrelated query": true,
		"d: a stale exemption": true,
	}
	tests := []struct {
		name  string
		path  string
		src   string
		rules []string // the rules expected to fire, in order
	}{
		{"a: a generated revocation read called from a handler", "internal/adapters/inbound/httpapi/x.go",
			"package httpapi\nfunc f(q interface{ ReadRepoEntitlement(int) }) { q.ReadRepoEntitlement(1) }\n", []string{"a"}},
		{"a: the session-scoped read called from the actor", "internal/app/sessionactor/x.go",
			"package sessionactor\nfunc f(q interface{ FirstRevokedRepoForSession(int) }) { q.FirstRevokedRepoForSession(1) }\n", []string{"a"}},
		{"a: another postgres store file", "internal/adapters/outbound/postgres/other_store.go",
			"package postgres\nfunc f(q interface{ ReadRepoEntitlement(int) }) { q.ReadRepoEntitlement(1) }\n", []string{"a"}},
		{"a: a Known-only wrapper in the store's own file", store,
			"package postgres\ntype S struct{ q interface{ ReadRepoEntitlement(string) (struct{ RepoKnown bool }, error) } }\nfunc (s *S) RepoKnown(r string) (bool, error) { row, err := s.q.ReadRepoEntitlement(r); return row.RepoKnown, err }\n", []string{"a"}},
		{"a: a method value of the generated read", store,
			"package postgres\ntype S struct{ q interface{ ReadRepoEntitlement(string) } }\nfunc (s *S) other() { read := s.q.ReadRepoEntitlement; read(\"x\") }\n", []string{"a"}},
		{"a: the store's own two methods wrap them", store,
			"package postgres\ntype S struct{ q interface{ ReadRepoEntitlement(int); FirstRevokedRepoForSession(int) } }\nfunc (s *S) RepoEntitlement() { s.q.ReadRepoEntitlement(1) }\nfunc (s *S) RevokedRepoForSession() { s.q.FirstRevokedRepoForSession(1) }\n", nil},
		{"a: crossed over, each read in the other's method", store,
			"package postgres\ntype S struct{ q interface{ ReadRepoEntitlement(int); FirstRevokedRepoForSession(int) } }\nfunc (s *S) RevokedRepoForSession() { s.q.ReadRepoEntitlement(1) }\nfunc (s *S) RepoEntitlement() { s.q.FirstRevokedRepoForSession(1) }\n", []string{"a", "a"}},
		{"a: a test may", "internal/adapters/inbound/httpapi/x_test.go",
			"package httpapi\nfunc f(q interface{ ReadRepoEntitlement(int) }) { q.ReadRepoEntitlement(1) }\n", nil},
		{"b: a new admission path reads eligibility directly", "internal/adapters/inbound/httpapi/reviewanalytics.go",
			"package httpapi\nfunc GetReviewAnalytics(s interface{ RepoEntitlement(string) }) { s.RepoEntitlement(\"acme/widgets\") }\n", []string{"b"}},
		{"b: the right file, another function", "internal/adapters/inbound/httpapi/repoentitlementgate.go",
			"package httpapi\nfunc somethingElse(s interface{ RepoEntitlement(string) }) { s.RepoEntitlement(\"acme/widgets\") }\n", []string{"b"}},
		{"b: inside a closure of a function not allowed", "internal/app/foo/foo.go",
			"package foo\nfunc g(s interface{ RepoEntitlement(string) }) { _ = func() { s.RepoEntitlement(\"x\") } }\n", []string{"b"}},
		{"b: another postgres store wraps it for Known only", "internal/adapters/outbound/postgres/other_store.go",
			"package postgres\nfunc known(s interface{ RepoEntitlement(string) (struct{ Known bool }, error) }) bool { f, _ := s.RepoEntitlement(\"x\"); return f.Known }\n", []string{"b"}},
		{"b: a method value", "internal/app/foo/foo.go",
			"package foo\nfunc g(s interface{ RepoEntitlement(string) }) { read := s.RepoEntitlement; read(\"x\") }\n", []string{"b"}},
		{"b: passed as a function argument", "internal/app/foo/foo.go",
			"package foo\nfunc use(func(string)) {}\nfunc g(s interface{ RepoEntitlement(string) }) { use(s.RepoEntitlement) }\n", []string{"b"}},
		{"b: the resolver is allowed", "internal/adapters/inbound/httpapi/repoentitlementgate.go",
			"package httpapi\nfunc resolveRepoEntitlement(s interface{ RepoEntitlement(string) }) { s.RepoEntitlement(\"acme/widgets\") }\nfunc resolveGitHubRepoEntitlement(s interface{ RepoEntitlement(string) }) { _ = func() { s.RepoEntitlement(\"x\") } }\n", nil},
		{"b: the admin-route scoping is allowed", "internal/adapters/inbound/httpapi/reposettings.go",
			"package httpapi\nfunc confirmRepoKnown(s interface{ RepoEntitlement(string) }) { s.RepoEntitlement(\"acme/widgets\") }\n", nil},
		{"b: a package-qualified conversion is not a read", "internal/adapters/inbound/httpapi/x.go",
			"package httpapi\nimport \"github.com/narvidev/narvi/contracts/gen/go/restdtos\"\nvar _ = restdtos.RepoEntitlement(restdtos.RepoEntitlement{})\n", nil},
		{"b: a test may", "internal/adapters/inbound/httpapi/x_test.go",
			"package httpapi\nfunc f(s interface{ RepoEntitlement(string) }) { s.RepoEntitlement(\"x\") }\n", nil},
		{"c: a literal without Revoked", "internal/adapters/inbound/httpapi/x.go",
			"package httpapi\n" + imp + "var _ = authz.RepoAdmission{FullName: \"acme/widgets\", Known: true}\n", []string{"c"}},
		{"c: the zero literal", "internal/app/foo/foo.go",
			"package foo\n" + imp + "var _ = authz.RepoAdmission{}\n", []string{"c"}},
		{"c: a pointer to a literal without Revoked", "internal/app/foo/foo.go",
			"package foo\n" + imp + "var _ = &authz.RepoAdmission{Known: true}\n", []string{"c"}},
		{"c: an elided element literal", "internal/app/foo/foo.go",
			"package foo\n" + imp + "var _ = []authz.RepoAdmission{{FullName: \"a\", Known: true, Revoked: false}, {FullName: \"b\", Known: true}}\n", []string{"c"}},
		{"c: an elided element literal of a pointer slice", "internal/app/foo/foo.go",
			"package foo\n" + imp + "var _ = []*authz.RepoAdmission{{Known: true}, &authz.RepoAdmission{Known: true, Revoked: false}}\n", []string{"c"}},
		{"c: an elided map value", "internal/app/foo/foo.go",
			"package foo\n" + imp + "var _ = map[string]authz.RepoAdmission{\"a\": {Known: true}}\n", []string{"c"}},
		{"c: a var of the type, filled in field by field", "internal/app/foo/foo.go",
			"package foo\n" + imp + "func f() authz.RepoAdmission { var a authz.RepoAdmission; a.Known = true; return a }\n", []string{"c"}},
		{"c: new()", "internal/app/foo/foo.go",
			"package foo\n" + imp + "func f() *authz.RepoAdmission { a := new(authz.RepoAdmission); a.Known = true; return a }\n", []string{"c"}},
		{"c: a local alias", "internal/app/foo/foo.go",
			"package foo\n" + imp + "type adm = authz.RepoAdmission\nvar _ = adm{Known: true}\n", []string{"c"}},
		{"c: a defined type", "internal/app/foo/foo.go",
			"package foo\n" + imp + "type adm authz.RepoAdmission\n", []string{"c"}},
		{"c: under another import name", "internal/app/foo/foo.go",
			"package foo\nimport z \"github.com/narvidev/narvi/internal/domain/authz\"\nvar _ = z.RepoAdmission{Known: true}\n", []string{"c"}},
		{"c: a dot import", "internal/app/foo/foo.go",
			"package foo\nimport . \"github.com/narvidev/narvi/internal/domain/authz\"\nvar _ = RepoAdmission{Known: true, Revoked: false}\n", []string{"c"}},
		{"c: a helper inside package authz", "internal/domain/authz/helper.go",
			"package authz\ntype RepoAdmission struct{ Known, Revoked bool }\nfunc known() RepoAdmission { return RepoAdmission{Known: true} }\n", []string{"c"}},
		{"c: tests elsewhere are held to it too", "internal/app/foo/foo_test.go",
			"package foo\n" + imp + "var _ = authz.RepoAdmission{Known: true}\n", []string{"c"}},
		{"c: authz's own tests may", "internal/domain/authz/x_test.go",
			"package authz\nvar _ = RepoAdmission{Known: true}\n", nil},
		{"c: Revoked named explicitly", "internal/adapters/inbound/httpapi/x.go",
			"package httpapi\n" + imp + "var _ = authz.RepoAdmission{FullName: \"acme/widgets\", Known: true, Revoked: false}\n", nil},
		{"c: an unkeyed literal lists every field", "internal/app/foo/foo.go",
			"package foo\n" + imp + "var _ = authz.RepoAdmission{\"acme/widgets\", true, false}\n", nil},
		{"c: a parameter of the type is not built here", "internal/app/foo/foo.go",
			"package foo\n" + imp + "func f(a authz.RepoAdmission) bool { return a.Known }\n", nil},
		{"c: another package's RepoAdmission", "internal/app/foo/foo.go",
			"package foo\nimport \"github.com/narvidev/narvi/internal/domain/rollout\"\nvar _ = rollout.RepoAdmission{Enrolled: true}\n", nil},
		{"d: an EXISTS read of github_pr_sessions alone", query,
			"-- name: RepoKnownToDeployment :one\nSELECT EXISTS(\n    SELECT 1 FROM github_pr_sessions WHERE repo_full_name = $1\n) AS repo_known;\n", []string{"d"}},
		{"d: a count", query,
			"-- name: Seen :one\nSELECT count(*) > 0 FROM github_pr_sessions WHERE repo_full_name = $1;\n", []string{"d"}},
		{"d: a JOIN with no EXISTS", query,
			"-- name: Joined :many\nSELECT s.id FROM sessions s JOIN github_pr_sessions g ON g.session_id = s.id WHERE g.repo_full_name = $1;\n", []string{"d"}},
		{"d: an IN subquery", query,
			"-- name: InList :one\nSELECT $1::text IN (SELECT repo_full_name FROM github_pr_sessions) AS seen;\n", []string{"d"}},
		{"d: a comma join inside EXISTS", query,
			"-- name: Comma :one\nSELECT EXISTS (SELECT 1 FROM sessions s, github_pr_sessions g WHERE g.session_id = s.id AND g.repo_full_name = $1);\n", []string{"d"}},
		{"d: schema-qualified", query,
			"-- name: Qualified :one\nSELECT EXISTS (SELECT 1 FROM public.github_pr_sessions WHERE repo_full_name = $1);\n", []string{"d"}},
		{"d: quoted", query,
			"-- name: Quoted :one\nSELECT EXISTS (SELECT 1 FROM \"github_pr_sessions\" WHERE repo_full_name = $1);\n", []string{"d"}},
		{"d: the revocations table named only in a block comment", query,
			"-- name: Sneaky :one\nSELECT EXISTS (SELECT 1 FROM github_pr_sessions /* repo_entitlement_revocations */ WHERE repo_full_name = $1);\n", []string{"d"}},
		{"d: the revocations table named only in a line comment", query,
			"-- name: Sneaky :one\n-- repo_entitlement_revocations is read elsewhere\nSELECT EXISTS (SELECT 1 FROM github_pr_sessions WHERE repo_full_name = $1);\n", []string{"d"}},
		{"d: read together with the revocations table", query,
			"-- name: ReadRepoEntitlement :one\nSELECT EXISTS (SELECT 1 FROM github_pr_sessions g WHERE g.repo_full_name = $1) AS repo_known,\n  EXISTS (SELECT 1 FROM repo_entitlement_revocations r WHERE r.repo_full_name = $1) AS revoked;\n", nil},
		{"d: an exempt read, beside a write and an unrelated query", query,
			"-- name: GetGitHubPRSessionBySessionID :one\nSELECT * FROM github_pr_sessions WHERE session_id = $1;\n\n-- name: SetX :exec\nUPDATE github_pr_sessions SET session_id = $2 WHERE repo_full_name = $1;\n\n-- name: Ins :exec\nINSERT INTO github_pr_sessions (repo_full_name) VALUES ($1);\n\n-- name: Other :one\nSELECT 1 FROM sessions WHERE id = $1;\n", nil},
		{"d: a stale exemption", query,
			"-- name: Other :one\nSELECT 1 FROM sessions WHERE id = $1;\n", []string{"d"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, filepath.FromSlash(tc.path))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.src), 0o600); err != nil {
				t.Fatal(err)
			}
			var treeExemptions []RepoEntitlementQueryExemption
			if exemptCases[tc.name] {
				treeExemptions = exemptions
			}

			violations, err := scanRepoEntitlementReads(root, treeExemptions)
			if err != nil {
				t.Fatalf("scanRepoEntitlementReads: %v", err)
			}
			var got []string
			for _, v := range violations {
				got = append(got, v.Rule)
			}
			if !slices.Equal(got, tc.rules) {
				t.Errorf("rules fired = %v, want %v (violations: %v)", got, tc.rules, violations)
			}
		})
	}
}

// TestRepoEntitlementQueryExemptions_EachHasAReason keeps rule (d)'s
// allowlist honest: every entry says why its query is not an eligibility
// read (TestRepoEntitlementGuard already fails a stale one).
func TestRepoEntitlementQueryExemptions_EachHasAReason(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range RepoEntitlementQueryExemptions {
		if seen[e.Query] {
			t.Errorf("%s is listed twice", e.Query)
		}
		seen[e.Query] = true
		if len(strings.Fields(e.Reason)) < 8 {
			t.Errorf("%s: reason %q does not say why", e.Query, e.Reason)
		}
	}
}
