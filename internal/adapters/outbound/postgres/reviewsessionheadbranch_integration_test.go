//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestSessionStore_SetReviewSessionHeadBranch_WritesOnlyAMissingBranchOfAHeadInTheBase
// pins the guards of the statement the GitHub ingress runs when a mention
// reads a pull request's head in its base repository (technical plan
// §27.1): it gives the review session's spec the head's branch only when
// the claim records the head in the base, the claim is the session's only
// one, and the primary repo is still the url the ingress read, with no
// branch. It never replaces a branch, and running it twice writes once.
func TestSessionStore_SetReviewSessionHeadBranch_WritesOnlyAMissingBranchOfAHeadInTheBase(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	prSessions := narvipg.NewGitHubPRSessionStore(pool)
	inBase, notInBase := true, false

	for i, tc := range []struct {
		name string
		// spec is the session's repos, %[1]s its url.
		spec   string
		record *bool
		// secondClaim: the session also claims another pull request.
		secondClaim bool
		// readURL replaces the url the ingress read, when not "".
		readURL string
		// wrongPR: the claim key passed is another pull request's.
		wrongPR bool
		want    bool
	}{
		{name: "no branch, recorded in the base", spec: `[{"name":"widgets","url":%[1]q,"branch":null}]`, record: &inBase, want: true},
		{name: "no branch key at all, recorded in the base", spec: `[{"name":"widgets","url":%[1]q}]`, record: &inBase, want: true},
		{name: "an empty branch, recorded in the base", spec: `[{"name":"widgets","url":%[1]q,"branch":""}]`, record: &inBase, want: true},
		{name: "no branch, unrecorded", spec: `[{"name":"widgets","url":%[1]q,"branch":null}]`},
		{name: "no branch, recorded not in the base", spec: `[{"name":"widgets","url":%[1]q,"branch":null}]`, record: &notInBase},
		{name: "a branch already, recorded in the base", spec: `[{"name":"widgets","url":%[1]q,"branch":"main"}]`, record: &inBase},
		{name: "the url changed since the ingress read it", spec: `[{"name":"widgets","url":%[1]q,"branch":null}]`, record: &inBase, readURL: "https://github.com/acme/other.git"},
		{name: "a session claiming a second pull request", spec: `[{"name":"widgets","url":%[1]q,"branch":null}]`, record: &inBase, secondClaim: true},
		{name: "another pull request's claim key", spec: `[{"name":"widgets","url":%[1]q,"branch":null}]`, record: &inBase, wrongPR: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoFullName := fmt.Sprintf("acme/head-branch-%d-%d", i, time.Now().UnixNano())
			url := "https://github.com/" + repoFullName + ".git"
			session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{
				SpawnSource: sqlcgen.SessionSpawnSourceGithub,
				Repos:       []byte(fmt.Sprintf(tc.spec, url)),
			})
			if err != nil {
				t.Fatal(err)
			}
			claim := func(pr int32) {
				t.Helper()
				if err := prSessions.EnsureRow(ctx, repoFullName, pr); err != nil {
					t.Fatal(err)
				}
				if err := prSessions.SetSessionID(ctx, repoFullName, pr, session.ID); err != nil {
					t.Fatal(err)
				}
			}
			claim(7)
			if tc.secondClaim {
				claim(8)
			}
			if tc.record != nil {
				if err := prSessions.RecordHeadInBase(ctx, repoFullName, 7, *tc.record); err != nil {
					t.Fatal(err)
				}
			}
			before, err := sessions.Get(ctx, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			readURL, pr := url, int32(7)
			if tc.readURL != "" {
				readURL = tc.readURL
			}
			if tc.wrongPR {
				pr = 9
			}

			set, err := sessions.SetReviewSessionHeadBranch(ctx, session.ID, repoFullName, pr, readURL, "feature-x")
			if err != nil {
				t.Fatalf("SetReviewSessionHeadBranch: %v", err)
			}
			if set != tc.want {
				t.Fatalf("set = %v, want %v", set, tc.want)
			}
			after, err := sessions.Get(ctx, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.want {
				if string(after.Repos) != string(before.Repos) {
					t.Errorf("repos = %s, want %s unchanged", after.Repos, before.Repos)
				}
				return
			}
			var repos []map[string]any
			if err := json.Unmarshal(after.Repos, &repos); err != nil || len(repos) != 1 {
				t.Fatalf("decode %s: %v", after.Repos, err)
			}
			if repos[0]["branch"] != "feature-x" || repos[0]["url"] != url || repos[0]["name"] != "widgets" {
				t.Errorf("repos = %s, want the url and name kept and branch feature-x", after.Repos)
			}
			again, err := sessions.SetReviewSessionHeadBranch(ctx, session.ID, repoFullName, 7, url, "other-branch")
			if err != nil || again {
				t.Errorf("a second run: set %v, %v; want nothing written over the branch", again, err)
			}
		})
	}
}
