//go:build integration

// Integration tests for the optional readers of §12.5's GitHub outbound
// axis: with GitHub outbound off (a nil *platform.GitHubOutboundConfig) the
// review readout, the session result, the verdict tool and the re-review
// button stay mounted and answer, each through its existing degraded path,
// and none of them calls the code host -- a tripwire code host fails the
// test on any call. One rig per test: the re-review route spawns an actor,
// and this package's pool is pinned small.
package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/reviewfreshness"
	"github.com/narvidev/narvi/internal/domain/review"
)

// tripwireReviewFetcher is a reviewcontext.Fetcher that fails the test on
// any call: with GitHub outbound off nothing may read GitHub as the bot.
type tripwireReviewFetcher struct {
	t     *testing.T
	calls atomic.Int32
}

func (f *tripwireReviewFetcher) GetPullRequest(context.Context, string, string, int32, string) (githubapi.PullRequest, error) {
	f.calls.Add(1)
	f.t.Error("GetPullRequest called with GitHub outbound off")
	return githubapi.PullRequest{}, nil
}

func (f *tripwireReviewFetcher) GetCompareDiff(context.Context, string, string, string, string, string) (string, bool, error) {
	f.calls.Add(1)
	f.t.Error("GetCompareDiff called with GitHub outbound off")
	return "", false, nil
}

func (f *tripwireReviewFetcher) ResolveBranchSHA(context.Context, ports.ResolveBranchSHASpec) (string, string, error) {
	f.calls.Add(1)
	f.t.Error("ResolveBranchSHA called with GitHub outbound off")
	return "", "", nil
}

// TestGetReviewReadout_GitHubOutboundOff_NoLiveRead: the readout answers
// 200 with its live part "not available" (no title, no visual-QA status)
// and never calls the code host -- neither for the pull request nor for
// the diff its findings are re-anchored against, which a recorded verdict
// with a head sha would otherwise re-fetch.
func TestGetReviewReadout_GitHubOutboundOff_NoLiveRead(t *testing.T) {
	fetcher := &tripwireReviewFetcher{t: t}
	rig := newTestRig(t, func(r *testRig) {
		r.diffFetcher = fetcher
		r.outbound = nil
	})
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	const repoFullName = "acme/outbound-off-readout"
	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, repoFullName, 11)
	verdictAt(ctx, t, rig, session.ID, repoFullName, 11, pgtype.UUID{}, "outbound-off-head", currentContext())

	// Decoded loosely: the seeded verdict carries no digest, which the
	// generated DTO's own validation would refuse -- irrelevant here.
	var got map[string]any
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/review", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got["latestVerdict"] == nil {
		t.Fatal("latestVerdict = null, want the recorded verdict -- without it the diff re-fetch path this test guards is never reached")
	}
	if got["prTitle"] != nil || got["visualQa"] != nil {
		t.Errorf("prTitle = %v, visualQa = %v, want both null (the live read is not available with GitHub outbound off)", got["prTitle"], got["visualQa"])
	}
	if n := fetcher.calls.Load(); n != 0 {
		t.Errorf("code host called %d times, want 0", n)
	}
}

// TestGetSessionResult_GitHubOutboundOff_FreshnessUnconfirmed: an assessed
// verdict's freshness reads unconfirmed for want of a code host -- the
// SAME answer as a deployment with no code host at all -- and the code
// host that IS wired is never read.
func TestGetSessionResult_GitHubOutboundOff_FreshnessUnconfirmed(t *testing.T) {
	ctx := context.Background()
	host := newResultCodeHost()
	rig := newTestRig(t, func(r *testRig) {
		r.resultSourceControl = host
		r.resultOutbound = nil
	})
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)

	const owner, repo, full = "acme", "outbound-off-result", "acme/outbound-off-result"
	host.open(owner, repo, 1, "h1", "main")
	host.branch(owner, repo, "main", "b1")
	sess := reviewSession(ctx, t, rig, user.ID, full, 1)
	attempt := reviewAttempt(ctx, t, rig, sess.ID, endedCompleted...)
	verdictAt(ctx, t, rig, sess.ID, full, 1, attempt.ID, "h1", currentContext())

	got, _ := getResult(t, rig, sess.ID, cookie)
	if got.ReviewedPullRequest == nil || got.ReviewedPullRequest.Review.State != restdtos.SessionOutcomeReviewStateAssessed {
		t.Fatalf("reviewedPullRequest = %+v, want an assessed verdict", got.ReviewedPullRequest)
	}
	f := got.ReviewedPullRequest.Review.Freshness
	if f.State != restdtos.SessionOutcomeReviewFreshnessStateUnconfirmed || reasonOf(f) != reviewfreshness.ReasonNoCodeHost {
		t.Errorf("freshness = %q (%q), want unconfirmed (%q)", f.State, reasonOf(f), reviewfreshness.ReasonNoCodeHost)
	}
	if n := host.readsOf(owner, repo, 1); n != 0 || len(host.tokens) != 0 {
		t.Errorf("code host read %d times with tokens %v, want no read at all", n, host.tokens)
	}
	// The SAME answer as no code host at all, the suggested delay included:
	// nothing was read live, so there is nothing to poll for sooner.
	if want := int(rig.resultTimeouts.SessionResultDelaySettled / time.Second); got.SuggestedDelaySeconds != want {
		t.Errorf("suggestedDelaySeconds = %d, want %d (SessionResultDelaySettled: nothing is read live with GitHub outbound off)", got.SuggestedDelaySeconds, want)
	}
}

// TestPostReviewVerdict_GitHubOutboundOff_FindingsUnanchored: the verdict
// tool still records and enqueues the verdict, but with no bot credential
// there is no diff to anchor findings against -- every finding renders
// with no line, exactly as when the diff fetch fails -- and no fetch is
// attempted.
func TestPostReviewVerdict_GitHubOutboundOff_FindingsUnanchored(t *testing.T) {
	ctx := context.Background()
	fetcher := &tripwireReviewFetcher{t: t}
	rig := newTestRig(t, func(r *testRig) {
		r.diffFetcher = fetcher
		r.outbound = nil
	})

	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-outbound-off", 62)
	headSHA := "outbound-off-head-sha"
	createdTurn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusProcessing, ReviewHeadSha: &headSHA})
	if err != nil {
		t.Fatalf("seed processing turn with review head sha: %v", err)
	}
	turnMessageID := testDispatchMessageID
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: createdTurn.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedMessageID: &turnMessageID}); err != nil {
		t.Fatalf("stamp dispatched_message_id on seeded turn: %v", err)
	}

	// The SAME finding the matchable-anchoring test anchors to main.go:12
	// when outbound is on.
	body := verdictRequestWithFinding("main.go", "for i := 0; i < len(items); i++ looks risky")
	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, body)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var raw []byte
	if err := rig.pool.QueryRow(ctx, `SELECT payload FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindGitHubVerdict)).Scan(&raw); err != nil {
		t.Fatalf("query outbox payload: %v", err)
	}
	var payload githubapi.VerdictPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal outbox payload: %v", err)
	}
	if strings.Contains(payload.Body, "main.go:") {
		t.Errorf("posted comment body rendered a line reference with GitHub outbound off; got:\n%s", payload.Body)
	}
	if !strings.Contains(payload.Body, "`main.go`") {
		t.Errorf("posted comment body should still render the bare file path; got:\n%s", payload.Body)
	}
	if n := fetcher.calls.Load(); n != 0 {
		t.Errorf("code host called %d times, want 0", n)
	}
}

// TestRetriggerReview_GitHubOutboundOff_NoPrefetch: the re-review button
// still queues the review turn, never calls the code host, and takes the
// SAME degraded path a failed live read takes: no head sha, but a prompt
// that still carries the verdict tool's instructions, so the turn can post
// what it finds (TestPostReviewVerdict_GitHubOutboundOff_NoHeadSHA_
// FindingsShowInReadout is the other half).
func TestRetriggerReview_GitHubOutboundOff_NoPrefetch(t *testing.T) {
	fetcher := &tripwireReviewFetcher{t: t}
	rig := newTestRig(t, func(r *testRig) {
		r.diffFetcher = fetcher
		r.outbound = nil
	})
	ctx := context.Background()
	owner, _ := rig.createAuthenticatedUser(ctx, t)
	_, token := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMaintainer)
	session := rig.createOwnedGitHubReviewSession(ctx, t, owner.ID, "acme/outbound-off-retrigger", 43)

	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/review/retrigger", nil, nil, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var prompt string
	var reviewHeadSHA *string
	if err := rig.pool.QueryRow(ctx, `SELECT prompt, review_head_sha FROM turns WHERE session_id = $1`, session.ID).Scan(&prompt, &reviewHeadSHA); err != nil {
		t.Fatalf("query turn: %v", err)
	}
	if !strings.HasPrefix(prompt, "Manual re-review requested via the web review button.") {
		t.Errorf("turn prompt = %q, want the fixed re-review prompt first", prompt)
	}
	for _, placeholder := range []string{review.VerdictToolURLPlaceholder, review.VerdictToolBearerPlaceholder, review.VerdictToolDispatchMessageIDPlaceholder} {
		if !strings.Contains(prompt, placeholder) {
			t.Errorf("turn prompt lacks %s -- without the verdict tool's instructions the turn cannot post a verdict", placeholder)
		}
	}
	if reviewHeadSHA != nil {
		t.Errorf("turns.review_head_sha = %q, want NULL (no live head read)", *reviewHeadSHA)
	}
	if n := fetcher.calls.Load(); n != 0 {
		t.Errorf("code host called %d times, want 0", n)
	}
}

// TestPostReviewVerdict_GitHubOutboundOff_NoHeadSHA_FindingsShowInReadout
// proves the outbound-off re-review turn is worth running: a verdict posted
// against a turn with no head sha -- what the button queues with GitHub
// outbound off -- records its findings, and the review readout shows them.
// Nothing is stored as a verdict (no head sha) and nothing reaches GitHub.
func TestPostReviewVerdict_GitHubOutboundOff_NoHeadSHA_FindingsShowInReadout(t *testing.T) {
	ctx := context.Background()
	fetcher := &tripwireReviewFetcher{t: t}
	rig := newTestRig(t, func(r *testRig) {
		r.diffFetcher = fetcher
		r.outbound = nil
	})

	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/outbound-off-findings", 63)
	seedDispatchedTurn(ctx, t, rig, session.ID)

	const description = "the loop reads past the end of items when the list is empty"
	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, verdictRequestWithFinding("main.go", description))
	if status != http.StatusCreated {
		t.Fatalf("POST verdict status = %d, want %d", status, http.StatusCreated)
	}

	_, token := rig.createAuthenticatedUser(ctx, t)
	var got map[string]any
	if status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/review", nil, &got, token); status != http.StatusOK {
		t.Fatalf("GET review status = %d, want %d", status, http.StatusOK)
	}
	findings, _ := got["findings"].([]any)
	found := false
	for _, f := range findings {
		if m, ok := f.(map[string]any); ok && m["description"] == description {
			found = true
		}
	}
	if !found {
		t.Errorf("readout findings = %v, want the finding the verdict posted", findings)
	}
	if got["latestVerdict"] != nil {
		t.Errorf("latestVerdict = %v, want null (no head sha, so no verdict row)", got["latestVerdict"])
	}
	if n := fetcher.calls.Load(); n != 0 {
		t.Errorf("code host called %d times, want 0", n)
	}
}
