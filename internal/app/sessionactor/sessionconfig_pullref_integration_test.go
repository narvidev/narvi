//go:build integration

package sessionactor

import (
	"context"
	"testing"
	"time"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
)

// TestAssembleSessionConfig_ReviewSessionCarriesItsPullRef drives a real
// fresh spawn of a session through EnsureDispatched, against Postgres, and
// reads the SESSION_CONFIG the provider was handed (technical plan §21.1,
// §30.4): a pull request's review session whose primary repo names the
// pull request's base repository carries refs/pull/<number>/head, derived
// from its github_pr_sessions claim at assembly; one created while its spec
// named the fork is moved onto the base repository in the spawn's own
// transaction, before its SESSION_CONFIG is assembled, and so carries the
// base and the ref too (reviewbaserepository.go); and a session with no
// pull request carries none, its spec as it was. The ref is never written
// into the session's own repos.
func TestAssembleSessionConfig_ReviewSessionCarriesItsPullRef(t *testing.T) {
	const repoFullName = "acme/widgets"
	const prNumber int32 = 11
	tests := []struct {
		name    string
		repoURL string
		claimed bool
		wantRef string // "" means no ref
		// wantURL is the url the spawn boots and stores; repoURL when "".
		wantURL string
	}{
		{name: "review session on its base repository", repoURL: "https://github.com/Acme/Widgets.git", claimed: true, wantRef: "refs/pull/11/head"},
		{name: "review session still naming the fork is moved onto its base", repoURL: "https://github.com/contributor/widgets.git", claimed: true,
			wantRef: "refs/pull/11/head", wantURL: "https://github.com/acme/widgets.git"},
		{name: "session with no pull request", repoURL: "https://github.com/acme/widgets.git"},
		{name: "session with no pull request naming another repository is left alone", repoURL: "https://github.com/contributor/widgets.git"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)

			reposJSON := `[{"name":"widgets","url":"` + tc.repoURL + `","branch":null}]`
			session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
				SpawnSource: sqlcgen.SessionSpawnSourceGithub,
				Repos:       []byte(reposJSON),
			})
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			if tc.claimed {
				prSessions := narvipg.NewGitHubPRSessionStore(pool)
				if err := prSessions.EnsureRow(ctx, repoFullName, prNumber); err != nil {
					t.Fatalf("ensure github pr session row: %v", err)
				}
				if err := prSessions.SetSessionID(ctx, repoFullName, prNumber, session.ID); err != nil {
					t.Fatalf("set github pr session id: %v", err)
				}
			}
			createPendingTurn(ctx, t, narvipg.NewTurnStore(pool), session.ID, "review this pull request")

			provider := &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-pull-ref-" + tc.name}}
			r := newDispatchTestRegistry(t, ctx, pool, provider, nil)
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, session.ID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}
			sendEnsureDispatched(ctx, t, a)
			waitUntil(t, 5*time.Second, func() bool { return provider.callCount() == 1 })

			repos := provider.lastSpec().SessionConfig.Repos
			if len(repos) != 1 {
				t.Fatalf("SessionConfig.Repos = %+v, want the session's one repo", repos)
			}
			got := ""
			if repos[0].Ref != nil {
				got = *repos[0].Ref
			}
			if got != tc.wantRef {
				t.Errorf("SessionConfig.Repos[0].Ref = %q, want %q", got, tc.wantRef)
			}
			wantURL := tc.repoURL
			if tc.wantURL != "" {
				wantURL = tc.wantURL
			}
			if repos[0].Url != wantURL {
				t.Errorf("SessionConfig.Repos[0].Url = %q, want %q", repos[0].Url, wantURL)
			}

			var stored string
			if err := pool.QueryRow(ctx, `SELECT repos::text FROM sessions WHERE id = $1`, session.ID).Scan(&stored); err != nil {
				t.Fatalf("read the session's repos: %v", err)
			}
			if stored != `[{"url": "`+wantURL+`", "name": "widgets", "branch": null}]` {
				t.Errorf("sessions.repos = %s after the spawn, want the url the spawn booted, %s, and no ref: the ref is never stored", stored, wantURL)
			}
		})
	}
}
