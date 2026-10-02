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
// tree per case.
func TestScanRepoEntitlementReads_Rules(t *testing.T) {
	const imp = `import "github.com/narvidev/narvi/internal/domain/authz"` + "\n"
	const query = "internal/adapters/outbound/postgres/queries/x.sql"
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
		{"a: the store's own file wraps them", "internal/adapters/outbound/postgres/githubprsession_store.go",
			"package postgres\nfunc f(q interface{ ReadRepoEntitlement(int); FirstRevokedRepoForSession(int) }) { q.ReadRepoEntitlement(1); q.FirstRevokedRepoForSession(1) }\n", nil},
		{"a: a test may", "internal/adapters/inbound/httpapi/x_test.go",
			"package httpapi\nfunc f(q interface{ ReadRepoEntitlement(int) }) { q.ReadRepoEntitlement(1) }\n", nil},
		{"b: a new admission path reads eligibility directly", "internal/adapters/inbound/httpapi/reviewanalytics.go",
			"package httpapi\nfunc GetReviewAnalytics(s interface{ RepoEntitlement(string) }) { s.RepoEntitlement(\"acme/widgets\") }\n", []string{"b"}},
		{"b: the right file, another function", "internal/adapters/inbound/httpapi/repoentitlementgate.go",
			"package httpapi\nfunc somethingElse(s interface{ RepoEntitlement(string) }) { s.RepoEntitlement(\"acme/widgets\") }\n", []string{"b"}},
		{"b: inside a closure of a function not allowed", "internal/app/foo/foo.go",
			"package foo\nfunc g(s interface{ RepoEntitlement(string) }) { _ = func() { s.RepoEntitlement(\"x\") } }\n", []string{"b"}},
		{"b: the resolver is allowed", "internal/adapters/inbound/httpapi/repoentitlementgate.go",
			"package httpapi\nfunc resolveRepoEntitlement(s interface{ RepoEntitlement(string) }) { s.RepoEntitlement(\"acme/widgets\") }\nfunc resolveGitHubRepoEntitlement(s interface{ RepoEntitlement(string) }) { _ = func() { s.RepoEntitlement(\"x\") } }\n", nil},
		{"b: the admin-route scoping is allowed", "internal/adapters/inbound/httpapi/reposettings.go",
			"package httpapi\nfunc confirmRepoKnown(s interface{ RepoEntitlement(string) }) { s.RepoEntitlement(\"acme/widgets\") }\n", nil},
		{"b: the postgres adapter itself", "internal/adapters/outbound/postgres/x.go",
			"package postgres\nfunc f(s interface{ RepoEntitlement(string) }) { s.RepoEntitlement(\"x\") }\n", nil},
		{"b: a package-qualified conversion is not a read", "internal/adapters/inbound/httpapi/x.go",
			"package httpapi\nimport \"github.com/narvidev/narvi/contracts/gen/go/restdtos\"\nvar _ = restdtos.RepoEntitlement(restdtos.RepoEntitlement{})\n", nil},
		{"b: a test may", "internal/adapters/inbound/httpapi/x_test.go",
			"package httpapi\nfunc f(s interface{ RepoEntitlement(string) }) { s.RepoEntitlement(\"x\") }\n", nil},
		{"c: a literal without Revoked", "internal/adapters/inbound/httpapi/x.go",
			"package httpapi\n" + imp + "var _ = authz.RepoAdmission{FullName: \"acme/widgets\", Known: true}\n", []string{"c"}},
		{"c: the zero literal", "internal/app/foo/foo.go",
			"package foo\n" + imp + "var _ = authz.RepoAdmission{}\n", []string{"c"}},
		{"c: an elided element literal", "internal/app/foo/foo.go",
			"package foo\n" + imp + "var _ = []authz.RepoAdmission{{FullName: \"a\", Known: true, Revoked: false}, {FullName: \"b\", Known: true}}\n", []string{"c"}},
		{"c: under another import name", "internal/app/foo/foo.go",
			"package foo\nimport z \"github.com/narvidev/narvi/internal/domain/authz\"\nvar _ = z.RepoAdmission{Known: true}\n", []string{"c"}},
		{"c: a dot import", "internal/app/foo/foo.go",
			"package foo\nimport . \"github.com/narvidev/narvi/internal/domain/authz\"\nvar _ = RepoAdmission{Known: true, Revoked: false}\n", []string{"c"}},
		{"c: tests are held to it too", "internal/app/foo/foo_test.go",
			"package foo\n" + imp + "var _ = authz.RepoAdmission{Known: true}\n", []string{"c"}},
		{"c: Revoked named explicitly", "internal/adapters/inbound/httpapi/x.go",
			"package httpapi\n" + imp + "var _ = authz.RepoAdmission{FullName: \"acme/widgets\", Known: true, Revoked: false}\n", nil},
		{"c: an unkeyed literal lists every field", "internal/app/foo/foo.go",
			"package foo\n" + imp + "var _ = authz.RepoAdmission{\"acme/widgets\", true, false}\n", nil},
		{"c: the domain package itself", "internal/domain/authz/x.go",
			"package authz\nvar _ = RepoAdmission{Known: true}\n", nil},
		{"c: another package's RepoAdmission", "internal/app/foo/foo.go",
			"package foo\nimport \"github.com/narvidev/narvi/internal/domain/rollout\"\nvar _ = rollout.RepoAdmission{Enrolled: true}\n", nil},
		{"d: an EXISTS read of github_pr_sessions alone", query,
			"-- name: RepoKnownToDeployment :one\nSELECT EXISTS(\n    SELECT 1 FROM github_pr_sessions WHERE repo_full_name = $1\n) AS repo_known;\n", []string{"d"}},
		{"d: aliased, beside an unrelated EXISTS", query,
			"-- name: Other :one\nSELECT 1;\n\n-- name: Facts :one\nSELECT EXISTS (SELECT 1 FROM sessions s WHERE s.id = $1) AS a,\n  EXISTS (SELECT 1 FROM github_pr_sessions g WHERE g.repo_full_name = $2) AS b;\n", []string{"d"}},
		{"d: read together with the revocations table", query,
			"-- name: ReadRepoEntitlement :one\nSELECT EXISTS (SELECT 1 FROM github_pr_sessions g WHERE g.repo_full_name = $1) AS repo_known,\n  EXISTS (SELECT 1 FROM repo_entitlement_revocations r WHERE r.repo_full_name = $1) AS revoked;\n", nil},
		{"d: the table named only in a comment does not count", query,
			"-- name: Sneaky :one\n-- repo_entitlement_revocations is read elsewhere\nSELECT EXISTS (SELECT 1 FROM github_pr_sessions WHERE repo_full_name = $1);\n", []string{"d"}},
		{"d: a FROM nested in another EXISTS's subquery is not that EXISTS's read", query,
			"-- name: Activity :one\nSELECT 1 FROM sessions s\nWHERE EXISTS (SELECT 1 FROM workflow_step_runs sr WHERE sr.id = s.id)\n  AND s.id IN (SELECT gps.session_id FROM github_pr_sessions gps);\n", nil},
		{"d: a plain read of github_pr_sessions is not an eligibility read", query,
			"-- name: GetGitHubPRSessionBySessionID :one\nSELECT * FROM github_pr_sessions WHERE session_id = $1;\n", nil},
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

			violations, err := ScanRepoEntitlementReads(root)
			if err != nil {
				t.Fatalf("ScanRepoEntitlementReads: %v", err)
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
