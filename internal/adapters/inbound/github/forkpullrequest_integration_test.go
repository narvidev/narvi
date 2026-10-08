//go:build integration

// This file proves technical plan §21.1 and §30.4 at the GitHub ingress: a
// pull request's review session names the pull request's base repository
// -- its name and clone URL -- for every event type that opens one, a
// pull request from a fork included, and carries the head branch only when
// that branch is in the base repository. The sandbox reads the head from
// the base's refs/pull/<number>/head (SESSION_CONFIG's ref), so the fork is
// never cloned, and no installation on the fork owner's account is ever
// asked for. Every reader that resolves a repository from the spec then
// resolves the base: here, the rollout gate at creation and the outbox's
// egress mode.
package github_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

const (
	forkBaseFullName = "acme/widgets"
	forkBaseName     = "widgets"
	forkBaseCloneURL = "https://github.com/acme/widgets.git"
)

// forkCaseHead is the pull request's head as an event carries it.
type forkCaseHead struct {
	name string
	ref  string
	// repoFullName is head.repo.full_name; deleted means head.repo is null.
	repoFullName string
	deleted      bool
	// wantBranch is the branch the session's spec carries; nil for none.
	wantBranch *string
}

func forkCaseHeads() []forkCaseHead {
	featureX := "feature-x"
	return []forkCaseHead{
		// The head branch's name is deliberately the base's own default
		// branch: a spec carrying it would name the base's main.
		{name: "from a fork", ref: "main", repoFullName: "contributor/widgets"},
		{name: "from a deleted fork", ref: "main", deleted: true},
		{name: "from a branch of the base repository", ref: "feature-x", repoFullName: forkBaseFullName, wantBranch: &featureX},
	}
}

// headRepoObject is head.repo as a delivery or the REST API carries it.
func headRepoObject(head forkCaseHead) any {
	if head.deleted {
		return nil
	}
	return map[string]any{
		"name":      forkBaseName,
		"full_name": head.repoFullName,
		"clone_url": "https://github.com/" + head.repoFullName + ".git",
	}
}

// reviewCommentOn is a "pull_request_review_comment" mention of the bot on
// forkBaseFullName#prNumber whose head is head.
func reviewCommentOn(head forkCaseHead, prNumber int, commenterID int64) []byte {
	body, err := json.Marshal(map[string]any{
		"action": "created",
		"comment": map[string]any{
			"id":   int64(prNumber)*1000 + 7,
			"body": fmt.Sprintf("@%s please review", testBotHandleIntegration),
			"user": map[string]any{"id": commenterID, "login": "fork-reviewer"},
		},
		"pull_request": map[string]any{
			"number": prNumber,
			"head":   map[string]any{"ref": head.ref, "sha": "sha-head-" + head.ref, "repo": headRepoObject(head)},
		},
		"repository": map[string]any{"full_name": forkBaseFullName, "name": forkBaseName, "clone_url": forkBaseCloneURL},
	})
	if err != nil {
		panic(err)
	}
	return body
}

// labeledOn is a "pull_request"/"labeled" re-trigger of forkBaseFullName#
// prNumber whose head is head.
func labeledOn(head forkCaseHead, prNumber int, senderID int64) []byte {
	body, err := json.Marshal(map[string]any{
		"action": "labeled",
		"label":  map[string]any{"name": "run-review"},
		"sender": map[string]any{"id": senderID, "login": "fork-reviewer"},
		"pull_request": map[string]any{
			"number": prNumber,
			"head":   map[string]any{"ref": head.ref, "sha": "sha-head-" + head.ref, "repo": headRepoObject(head)},
			"base":   map[string]any{"ref": "main"},
		},
		"repository": map[string]any{"full_name": forkBaseFullName, "name": forkBaseName, "clone_url": forkBaseCloneURL, "default_branch": "main"},
	})
	if err != nil {
		panic(err)
	}
	return body
}

// forkCaseEvent is one event type that opens a review session.
type forkCaseEvent struct {
	name      string
	eventType string
	body      func(head forkCaseHead, prNumber int, commenterID int64) []byte
	// resolver is what an issue_comment mention's head lookup answers.
	resolver func(head forkCaseHead) *fakePullRequestResolver
}

func forkCaseEvents() []forkCaseEvent {
	return []forkCaseEvent{
		{
			name: "issue_comment", eventType: "issue_comment",
			body: func(_ forkCaseHead, prNumber int, commenterID int64) []byte {
				return issueCommentBodyWithCommenter(forkBaseFullName, forkBaseName, forkBaseCloneURL, prNumber, "fork-case", commenterID, "fork-reviewer")
			},
			resolver: func(head forkCaseHead) *fakePullRequestResolver {
				pr := githubapi.PullRequest{HeadRef: head.ref, HeadSHA: "sha-head-" + head.ref}
				if !head.deleted {
					pr.HeadRepoFullName = head.repoFullName
				}
				return &fakePullRequestResolver{pr: pr}
			},
		},
		{name: "pull_request_review_comment", eventType: "pull_request_review_comment", body: reviewCommentOn},
		{name: "pull_request labeled", eventType: "pull_request", body: labeledOn},
	}
}

// newForkCaseRig is newTestRig wired as production wires the head lookup
// and the label lane, with ev's lookup answering for head.
func newForkCaseRig(t *testing.T, ev forkCaseEvent, head forkCaseHead) testRig {
	t.Helper()
	return newTestRig(t, func(cfg *githubingress.Config) {
		cfg.ReReviewLabel = "run-review"
		cfg.Timeouts = platform.DefaultTimeouts()
		if ev.resolver != nil {
			cfg.PullRequests = ev.resolver(head)
		}
	})
}

// reviewSessionSpec reads the one github session's id and spec.
func reviewSessionSpec(ctx context.Context, t *testing.T, rig testRig) (string, any) {
	t.Helper()
	var id string
	var raw []byte
	if err := rig.pool.QueryRow(ctx, `SELECT id::text, repos FROM sessions WHERE spawn_source = 'github'`).Scan(&id, &raw); err != nil {
		t.Fatalf("read the review session: %v", err)
	}
	var spec any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("decode the spec %s: %v", raw, err)
	}
	return id, spec
}

// TestGitHubIntegration_ForkPullRequest_ReviewSessionNamesTheBaseRepository:
// for every event type that opens a review session, and every head -- a
// fork's, a deleted fork's, a branch of the base repository -- the session
// clones the base repository, claims the pull request on it, and carries
// the head branch only for a branch of the base. A fork opened from its own
// main never names the base's main.
func TestGitHubIntegration_ForkPullRequest_ReviewSessionNamesTheBaseRepository(t *testing.T) {
	for i, ev := range forkCaseEvents() {
		for j, head := range forkCaseHeads() {
			t.Run(ev.name+" "+head.name, func(t *testing.T) {
				ctx := context.Background()
				rig := newForkCaseRig(t, ev, head)
				commenterID := int64(91700000 + i*10 + j)
				createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)
				prNumber := 1700 + i*10 + j

				status := postWebhookEventType(t, rig, ev.body(head, prNumber, commenterID), fmt.Sprintf("delivery-fork-case-%d-%d", i, j), ev.eventType)
				if status != http.StatusOK {
					t.Fatalf("status = %d, want 200", status)
				}

				id, spec := reviewSessionSpec(ctx, t, rig)
				want := []any{map[string]any{"name": forkBaseName, "url": forkBaseCloneURL, "branch": nil}}
				if head.wantBranch != nil {
					want[0].(map[string]any)["branch"] = *head.wantBranch
				}
				if !reflect.DeepEqual(spec, want) {
					t.Errorf("the review session's spec = %v, want %v", spec, want)
				}
				var claimed string
				if err := rig.pool.QueryRow(ctx, `SELECT repo_full_name FROM github_pr_sessions WHERE session_id = $1 AND pr_number = $2`, id, prNumber).Scan(&claimed); err != nil || claimed != forkBaseFullName {
					t.Errorf("claim = %q (%v), want the pull request on %s", claimed, err, forkBaseFullName)
				}
			})
		}
	}
}

// TestGitHubIntegration_RolloutGate_ForkPullRequestAdmittedByItsBase: in
// cohort mode a session is created only when every repository its spec
// names is enrolled. The pull request's base repository is enrolled; the
// contributor's fork never is. A fork's pull request is admitted on its
// base, as a pull request from one of the base's own branches is.
func TestGitHubIntegration_RolloutGate_ForkPullRequestAdmittedByItsBase(t *testing.T) {
	ctx := context.Background()
	rig, repoSettings, _ := newRolloutTestRig(t, platform.RolloutModeCohort)
	if _, err := repoSettings.UpsertSessionsEnabled(ctx, forkBaseFullName, true); err != nil {
		t.Fatalf("enroll the base repository: %v", err)
	}
	const commenterID = 91790001
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)

	fork := forkCaseHeads()[0]
	if status := postWebhookEventType(t, rig, reviewCommentOn(fork, 1790, commenterID), "delivery-fork-rollout-1", "pull_request_review_comment"); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if n := countRows(ctx, t, rig, `SELECT count(*) FROM sessions WHERE spawn_source = 'github'`); n != 1 {
		t.Fatalf("sessions = %d, want the fork's pull request admitted on its enrolled base", n)
	}
}

// TestOutboxEgressMode_ForkPullRequestResolvesItsBaseRepository: a
// notification about a review session -- its verdict comment, its check --
// is suppressed when any repository the session names resolves shadow, and
// a repository with no settings row resolves shadow. The base repository
// is live, and the fork has no settings: the review session of a fork's
// pull request, opened through the webhook, resolves live, by its base.
func TestOutboxEgressMode_ForkPullRequestResolvesItsBaseRepository(t *testing.T) {
	ctx := context.Background()
	ev := forkCaseEvents()[1]
	fork := forkCaseHeads()[0]
	rig := newForkCaseRig(t, ev, fork)
	if _, err := narvipg.NewRepoSettingsStore(rig.pool).UpsertLiveEgressEnabled(ctx, forkBaseFullName, true); err != nil {
		t.Fatalf("promote the base repository live: %v", err)
	}
	const commenterID = 91790002
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)
	if status := postWebhookEventType(t, rig, ev.body(fork, 1791, commenterID), "delivery-fork-egress-1", ev.eventType); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	var sessionID pgtype.UUID
	if err := rig.pool.QueryRow(ctx, `SELECT id FROM sessions WHERE spawn_source = 'github'`).Scan(&sessionID); err != nil {
		t.Fatalf("read the review session: %v", err)
	}

	shadow, confirmed := narvipg.NewOutboxStore(rig.pool, false).ResolveEffectiveModeConfirmed(ctx, sessionID)
	if shadow || !confirmed {
		t.Errorf("egress mode shadow = %v, confirmed = %v; want live, read: the session names its live base, not the fork", shadow, confirmed)
	}
}
