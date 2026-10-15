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
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/reposource"
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
				if got, want := recordedHeadInBase(ctx, t, rig, prNumber), head.wantBranch != nil; got == nil || *got != want {
					t.Errorf("the claim's head_in_base = %v, want %v: recorded from this delivery's head.repo", got, want)
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

// recordedHeadInBase reads forkBaseFullName#prNumber's claim's record of
// whether its head is a branch of the base; nil when nothing recorded it.
func recordedHeadInBase(ctx context.Context, t *testing.T, rig testRig, prNumber int) *bool {
	t.Helper()
	var got *bool
	if err := rig.pool.QueryRow(ctx, `SELECT head_in_base FROM github_pr_sessions WHERE repo_full_name = $1 AND pr_number = $2`, forkBaseFullName, prNumber).Scan(&got); err != nil {
		t.Fatalf("read the claim's head_in_base: %v", err)
	}
	return got
}

// TestGitHubIntegration_AMentionOnAnExistingSessionRecordsWhetherTheHeadIsInTheBase:
// the claim's record is written by every mention, a mention joining the
// pull request's existing review session included. A claim the previous
// ingress left unrecorded -- its session's spec possibly a deleted fork's,
// byte for byte a same-repository spec -- is recorded by the pull request's
// next mention, from that delivery's head.repo: in the base for a branch of
// the base, not in the base once the head repository is gone.
func TestGitHubIntegration_AMentionOnAnExistingSessionRecordsWhetherTheHeadIsInTheBase(t *testing.T) {
	ctx := context.Background()
	ev := forkCaseEvents()[1]
	heads := forkCaseHeads()
	sameRepo, deleted, fork := heads[2], heads[1], heads[0]
	rig := newForkCaseRig(t, ev, sameRepo)
	const commenterID = 91790010
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)
	const prNumber = 1795
	forget := func() {
		t.Helper()
		if _, err := rig.pool.Exec(ctx, `UPDATE github_pr_sessions SET head_in_base = NULL WHERE repo_full_name = $1 AND pr_number = $2`, forkBaseFullName, prNumber); err != nil {
			t.Fatal(err)
		}
	}

	for i, step := range []struct {
		name string
		head forkCaseHead
		// forgetFirst: the claim reads as the previous ingress left it.
		forgetFirst bool
		want        bool
	}{
		{name: "the first mention, opening the session", head: sameRepo, want: true},
		{name: "a mention on the existing session, the claim unrecorded", head: sameRepo, forgetFirst: true, want: true},
		{name: "a mention on the existing session once its head repository is gone", head: deleted, forgetFirst: true, want: false},
		{name: "a mention on the existing session naming a fork's head", head: fork, want: false},
		{name: "a mention on the existing session naming a branch of the base again", head: sameRepo, want: true},
	} {
		if step.forgetFirst {
			forget()
		}
		if status := postWebhookEventType(t, rig, ev.body(step.head, prNumber, commenterID), fmt.Sprintf("delivery-reuse-record-%d", i), ev.eventType); status != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", step.name, status)
		}
		if got := recordedHeadInBase(ctx, t, rig, prNumber); got == nil || *got != step.want {
			t.Errorf("%s: the claim's head_in_base = %v, want %v", step.name, got, step.want)
		}
	}
	if n := countRows(ctx, t, rig, `SELECT count(*) FROM sessions WHERE spawn_source = 'github'`); n != 1 {
		t.Fatalf("sessions = %d, want the one every mention joined", n)
	}
	if n := countRows(ctx, t, rig, `SELECT count(*) FROM turns`); n != 5 {
		t.Errorf("turns = %d, want 5: each mention joined the session", n)
	}
}

// switchablePullRequestResolver answers an issue_comment mention's head
// lookup as the test last set it: a pull request, or an error. The handler
// calls it from the server's goroutine, so it is guarded.
type switchablePullRequestResolver struct {
	mu  sync.Mutex
	pr  githubapi.PullRequest
	err error
}

func (f *switchablePullRequestResolver) set(pr githubapi.PullRequest, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pr, f.err = pr, err
}

func (f *switchablePullRequestResolver) GetPullRequest(_ context.Context, _, _ string, _ int32, _ string) (githubapi.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pr, f.err
}

// errLookupOutage is what a failed issue_comment head lookup answers.
var errLookupOutage = errors.New("simulated GitHub API outage")

// newSwitchableLookupRig is newTestRig wired as production wires the head
// lookup and the label lane, its lookup failing until the test sets it.
func newSwitchableLookupRig(t *testing.T) (testRig, *switchablePullRequestResolver) {
	t.Helper()
	resolver := &switchablePullRequestResolver{err: errLookupOutage}
	rig := newTestRig(t, func(cfg *githubingress.Config) {
		cfg.ReReviewLabel = "run-review"
		cfg.Timeouts = platform.DefaultTimeouts()
		cfg.PullRequests = resolver
	})
	return rig, resolver
}

// claimedSessionSpec reads forkBaseFullName#prNumber's review session's
// spec, raw.
func claimedSessionSpec(ctx context.Context, t *testing.T, rig testRig, prNumber int) []byte {
	t.Helper()
	var raw []byte
	if err := rig.pool.QueryRow(ctx, `SELECT s.repos FROM sessions s JOIN github_pr_sessions g ON g.session_id = s.id
		WHERE g.repo_full_name = $1 AND g.pr_number = $2`, forkBaseFullName, prNumber).Scan(&raw); err != nil {
		t.Fatalf("read the review session's spec: %v", err)
	}
	return raw
}

// specBranch is a spec's primary repo's branch; nil for none.
func specBranch(t *testing.T, raw []byte) *string {
	t.Helper()
	var repos []struct {
		Branch *string `json:"branch"`
	}
	if err := json.Unmarshal(raw, &repos); err != nil || len(repos) == 0 {
		t.Fatalf("decode the spec %s: %v", raw, err)
	}
	return repos[0].Branch
}

// TestGitHubIntegration_AMentionThatReadsNoHeadRecordsNothing: an
// issue_comment mention whose pull request lookup fails has read no head,
// so it records nothing on the claim (technical plan §27.1) -- a pull
// request's head repository never changes, so what an earlier mention read
// stays, and a claim no mention has read stays NULL, which fails closed.
// The mention still joins the session and starts its turn, and its
// session's spec is left as it was.
func TestGitHubIntegration_AMentionThatReadsNoHeadRecordsNothing(t *testing.T) {
	inBase, notInBase := true, false
	for i, tc := range []struct {
		name string
		// prior is the claim's record before the mention that reads no
		// head; set after the first mention opened the session.
		prior *bool
	}{
		{name: "recorded in the base", prior: &inBase},
		{name: "recorded not in the base", prior: &notInBase},
		{name: "never recorded", prior: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rig, _ := newSwitchableLookupRig(t)
			commenterID := int64(91790020 + i)
			createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)
			prNumber := 1910 + i
			sameRepo := forkCaseHeads()[2]

			// The first mention, a review comment, carries the head.
			if status := postWebhookEventType(t, rig, reviewCommentOn(sameRepo, prNumber, commenterID), fmt.Sprintf("delivery-no-head-open-%d", i), "pull_request_review_comment"); status != http.StatusOK {
				t.Fatalf("first mention: status = %d, want 200", status)
			}
			if _, err := rig.pool.Exec(ctx, `UPDATE github_pr_sessions SET head_in_base = $3 WHERE repo_full_name = $1 AND pr_number = $2`, forkBaseFullName, prNumber, tc.prior); err != nil {
				t.Fatal(err)
			}
			specBefore := claimedSessionSpec(ctx, t, rig, prNumber)

			// The next one, an issue comment, whose lookup fails.
			body := issueCommentBodyWithCommenter(forkBaseFullName, forkBaseName, forkBaseCloneURL, prNumber, "no-head", commenterID, "fork-reviewer")
			if status := postWebhook(t, rig, body, fmt.Sprintf("delivery-no-head-join-%d", i)); status != http.StatusOK {
				t.Fatalf("mention reading no head: status = %d, want 200", status)
			}
			got := recordedHeadInBase(ctx, t, rig, prNumber)
			if (got == nil) != (tc.prior == nil) || (got != nil && *got != *tc.prior) {
				t.Errorf("the claim's head_in_base = %v after a mention that read no head, want %v kept", got, tc.prior)
			}
			if specAfter := claimedSessionSpec(ctx, t, rig, prNumber); string(specAfter) != string(specBefore) {
				t.Errorf("the spec = %s after a mention that read no head, want %s", specAfter, specBefore)
			}
			if n := countRows(ctx, t, rig, `SELECT count(*) FROM turns`); n != 2 {
				t.Errorf("turns = %d, want 2: the mention joined the session all the same", n)
			}
		})
	}

	t.Run("a new claim", func(t *testing.T) {
		ctx := context.Background()
		rig, _ := newSwitchableLookupRig(t)
		const commenterID = 91790029
		createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)
		const prNumber = 1919
		body := issueCommentBodyWithCommenter(forkBaseFullName, forkBaseName, forkBaseCloneURL, prNumber, "no-head", commenterID, "fork-reviewer")
		if status := postWebhook(t, rig, body, "delivery-no-head-new"); status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if got := recordedHeadInBase(ctx, t, rig, prNumber); got != nil {
			t.Errorf("the claim's head_in_base = %v, want NULL: a head the ingress could not read is unknown", *got)
		}
		if branch := specBranch(t, claimedSessionSpec(ctx, t, rig, prNumber)); branch != nil {
			t.Errorf("the spec's branch = %q, want none: the head was not read", *branch)
		}
	})
}

// TestGitHubIntegration_AMentionThatReadsTheHeadInTheBaseGivesTheSpecItsBranch:
// a same-repository pull request's first mention, an issue comment whose
// lookup fails, opens a session naming the base with no branch, and
// records nothing. A later mention that reads the head in the base -- an
// issue comment whose lookup succeeds, a review comment, a label --
// records it and gives the spec the head's branch, so the review-head rule
// reads the session as the base's own, as if its first mention had read
// the head. A mention that then reads no head changes neither.
func TestGitHubIntegration_AMentionThatReadsTheHeadInTheBaseGivesTheSpecItsBranch(t *testing.T) {
	sameRepo := forkCaseHeads()[2]
	for i, tc := range []struct {
		name      string
		eventType string
		body      func(prNumber int, commenterID int64) []byte
	}{
		{name: "an issue comment whose lookup succeeds", eventType: "issue_comment", body: func(prNumber int, commenterID int64) []byte {
			return issueCommentBodyWithCommenter(forkBaseFullName, forkBaseName, forkBaseCloneURL, prNumber, "lookup-ok", commenterID, "fork-reviewer")
		}},
		{name: "a review comment", eventType: "pull_request_review_comment", body: func(prNumber int, commenterID int64) []byte {
			return reviewCommentOn(sameRepo, prNumber, commenterID)
		}},
		{name: "a label", eventType: "pull_request", body: func(prNumber int, commenterID int64) []byte {
			return labeledOn(sameRepo, prNumber, commenterID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rig, resolver := newSwitchableLookupRig(t)
			commenterID := int64(91790030 + i)
			createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)
			prNumber := 1920 + i
			issueComment := func(label string) []byte {
				return issueCommentBodyWithCommenter(forkBaseFullName, forkBaseName, forkBaseCloneURL, prNumber, label, commenterID, "fork-reviewer")
			}

			if status := postWebhook(t, rig, issueComment("lookup-failed"), fmt.Sprintf("delivery-repair-open-%d", i)); status != http.StatusOK {
				t.Fatalf("first mention: status = %d, want 200", status)
			}
			if branch := specBranch(t, claimedSessionSpec(ctx, t, rig, prNumber)); branch != nil {
				t.Fatalf("the spec's branch = %q after a failed lookup, want none", *branch)
			}
			if got := recordedHeadInBase(ctx, t, rig, prNumber); got != nil {
				t.Fatalf("the claim's head_in_base = %v after a failed lookup, want NULL", *got)
			}

			resolver.set(githubapi.PullRequest{HeadRef: sameRepo.ref, HeadSHA: "sha-head-" + sameRepo.ref, HeadRepoFullName: forkBaseFullName}, nil)
			if status := postWebhookEventType(t, rig, tc.body(prNumber, commenterID), fmt.Sprintf("delivery-repair-read-%d", i), tc.eventType); status != http.StatusOK {
				t.Fatalf("mention reading the head: status = %d, want 200", status)
			}
			assertRepaired := func(after string) {
				t.Helper()
				spec := claimedSessionSpec(ctx, t, rig, prNumber)
				if branch := specBranch(t, spec); branch == nil || *branch != sameRepo.ref {
					t.Errorf("%s: the spec = %s, want its branch %q", after, spec, sameRepo.ref)
				}
				var repos []struct {
					URL string `json:"url"`
				}
				if err := json.Unmarshal(spec, &repos); err != nil || len(repos) != 1 || repos[0].URL != forkBaseCloneURL {
					t.Errorf("%s: the spec = %s (%v), want one repo, the base's %q", after, spec, err, forkBaseCloneURL)
				}
				record := recordedHeadInBase(ctx, t, rig, prNumber)
				if record == nil || !*record {
					t.Errorf("%s: the claim's head_in_base = %v, want true", after, record)
				}
				if !reposource.ReviewHeadInBaseRepository(forkBaseFullName, record, spec) {
					t.Errorf("%s: the review-head rule reads the session as not the base's own (record %v, spec %s)", after, record, spec)
				}
			}
			assertRepaired("after the mention that read the head")

			resolver.set(githubapi.PullRequest{}, errLookupOutage)
			if status := postWebhook(t, rig, issueComment("lookup-failed-again"), fmt.Sprintf("delivery-repair-unread-%d", i)); status != http.StatusOK {
				t.Fatalf("mention reading no head: status = %d, want 200", status)
			}
			assertRepaired("after a later mention that read no head")

			if n := countRows(ctx, t, rig, `SELECT count(*) FROM sessions WHERE spawn_source = 'github'`); n != 1 {
				t.Errorf("sessions = %d, want the one every mention joined", n)
			}
			if n := countRows(ctx, t, rig, `SELECT count(*) FROM turns`); n != 3 {
				t.Errorf("turns = %d, want 3", n)
			}
		})
	}
}
