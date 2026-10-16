//go:build integration

package postgres_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
)

// The claim's head_in_base record (technical plan §27.1) is written by the
// GitHub ingress on every mention that reads the pull request's head, and
// read with the claim on every sandbox secrets and provider-credential
// delivery and dispatch gate; a mention that reads the head in the base
// gives a session opened without its branch the branch. Each by key:
const (
	// headInBaseReadMaxBuffers bounds GetGitHubPRSessionBySessionID, the
	// claim read the record rides on: a descent of
	// github_pr_sessions_session_id_idx and the claim's heap page.
	headInBaseReadMaxBuffers = 8
	// headInBaseWriteMaxBuffers bounds RecordGitHubPRSessionHeadInBase, the
	// row's first write in its transaction: a descent of
	// github_pr_sessions_pkey and the row's heap page. On the densely
	// loaded table below the page has no room for the new version, so the
	// update also reads the free space map, puts the version on another
	// page and gives it an entry in each of the table's two indexes (the
	// key and github_pr_sessions_session_id_idx): 15 measured under both
	// plans. head_in_base itself is not indexed.
	headInBaseWriteMaxBuffers = 18
	// headBranchRepairMaxBuffers bounds SetReviewSessionHeadBranch: the
	// session's row by sessions_pkey, its claim and the count of its
	// claims each by github_pr_sessions_session_id_idx, and the new
	// version of the session's row, which on the densely loaded table
	// below lands on another page, with the free space map read and an
	// entry in each of sessions' indexes: 21 measured under both plans.
	headBranchRepairMaxBuffers = 28
)

// TestGitHubPRSessionHeadInBase_PlansReadByKey measures, by buffers read,
// the claim read, the record's write and the spec's head-branch repair
// under a custom plan and under the generic plan pgx's statement cache lets
// Postgres settle on, on 3,000 review sessions and their claims, half of
// them recorded: each reads its rows by key, never a scan of a table.
func TestGitHubPRSessionHeadInBase_PlansReadByKey(t *testing.T) {
	ctx := context.Background()
	pool, _ := holdPlanDatabase(ctx, t, headInBaseMigration)
	exec := func(what, sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("store %s: %v", what, err)
		}
	}
	for _, table := range []string{"sessions", "github_pr_sessions"} {
		exec("autovacuum off "+table, `ALTER TABLE `+table+` SET (autovacuum_enabled = false)`)
	}
	exec("sessions", `
		INSERT INTO sessions (spawn_source, repos, spawn_depth)
		SELECT 'github', jsonb_build_array(jsonb_build_object('name', 'widgets', 'url', 'https://github.com/acme/plan-' || g || '.git', 'branch', NULL)), 0
		FROM generate_series(1, 3000) g`)
	exec("claims", `
		INSERT INTO github_pr_sessions (repo_full_name, pr_number, session_id, head_in_base)
		SELECT 'acme/plan-' || row_number() OVER (ORDER BY id), 7, id,
		       CASE WHEN row_number() OVER (ORDER BY id) % 2 = 0 THEN true END
		FROM sessions`)
	var session pgtype.UUID
	var repo, url string
	// The probe is a claim recorded in the base, never written since, so
	// the repair below finds its session and writes it, and the record's
	// write finds its row where the insert left it.
	if err := pool.QueryRow(ctx, `SELECT g.session_id, g.repo_full_name, s.repos->0->>'url' FROM github_pr_sessions g JOIN sessions s ON s.id = g.session_id
		WHERE g.head_in_base ORDER BY g.session_id LIMIT 1 OFFSET 750`).Scan(&session, &repo, &url); err != nil {
		t.Fatal(err)
	}
	exec("analyze", `ANALYZE`)

	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	sessions := narvipg.NewSessionStore(pool)
	quote := func(id pgtype.UUID) string { return "'" + id.String() + "'::uuid" }
	claimOnly := []string{"github_pr_sessions"}
	statements := []struct {
		holdPlanStatement
		max float64
		// relations is every table the statement may read.
		relations []string
	}{
		{holdPlanStatement{
			name: "GetGitHubPRSessionBySessionID",
			run: func(ctx context.Context, tx pgx.Tx, _ holdPlanProbe) error {
				_, err := prSessions.WithTx(tx).GetBySessionID(ctx, session)
				return err
			},
			args: func(holdPlanProbe) string { return quote(session) },
		}, headInBaseReadMaxBuffers, claimOnly},
		{holdPlanStatement{
			name: "RecordGitHubPRSessionHeadInBase",
			run: func(ctx context.Context, tx pgx.Tx, _ holdPlanProbe) error {
				return prSessions.WithTx(tx).RecordHeadInBase(ctx, repo, 7, false)
			},
			args: func(holdPlanProbe) string { return "false, '" + repo + "', 7" },
		}, headInBaseWriteMaxBuffers, claimOnly},
		{holdPlanStatement{
			name: "SetReviewSessionHeadBranch",
			// Each measured run starts from the last commit, where the
			// probe's spec has no branch; the eight runs before it share
			// one transaction, so only the first of them writes.
			run: func(ctx context.Context, tx pgx.Tx, _ holdPlanProbe) error {
				_, err := sessions.WithTx(tx).SetReviewSessionHeadBranch(ctx, session, repo, 7, url, "feature-x")
				return err
			},
			args: func(holdPlanProbe) string {
				return "'feature-x', " + quote(session) + ", '" + repo + "', 7, '" + url + "'"
			},
		}, headBranchRepairMaxBuffers, []string{"sessions", "github_pr_sessions"}},
	}

	probe := holdPlanProbe{name: "review session", sessionID: session}
	for _, s := range statements {
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			m := measureHoldPlan(ctx, t, pool, s.holdPlanStatement, probe, mode)
			t.Logf("%s (%s, args %s): %s", s.name, mode, s.args(probe), m)
			if m.buffers > s.max {
				t.Errorf("%s under %s reads %.0f buffers, over %.0f: %v", s.name, mode, m.buffers, s.max, m.scans)
			}
			if len(m.scans) == 0 {
				t.Errorf("%s under %s: no scan in its plan", s.name, mode)
			}
			for _, scan := range m.scans {
				if scan.Node == "Seq Scan" || strings.HasPrefix(scan.Node, "Bitmap") {
					t.Errorf("%s under %s scans %s with %s, want an index probe by key: %v", s.name, mode, scan.Relation, scan.Node, m.scans)
				}
				if scan.Relation != "" && !slices.Contains(s.relations, scan.Relation) {
					t.Errorf("%s under %s reads %s, want only %v: %v", s.name, mode, scan.Relation, s.relations, m.scans)
				}
			}
		}
	}
}
