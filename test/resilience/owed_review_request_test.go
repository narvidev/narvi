//go:build integration

package resilience_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// This file is the restart half of technical plan §24.9's owed human
// review requests: the request a moved review attempt leaves owed is two
// rows -- the request and its timer -- committed by the dispatch that ends
// the attempt, so a replica that dies before it re-runs the request loses
// nothing: whichever replica's pump claims the timer re-runs it.

// movedPR is the pull request as the code host answers it to every reader
// a replica has -- the context check (ReviewLiveReader) and the re-run's
// fetch (ReviewDiffFetcher): it has moved to head, with diff.
type movedPR struct {
	head, diff string
}

func (p movedPR) GetPullRequest(context.Context, string, string, int32, string) (githubapi.PullRequest, error) {
	return githubapi.PullRequest{HeadSHA: p.head, BaseRef: "main"}, nil
}

func (p movedPR) ResolveBranchSHA(_ context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	return "base-tip", spec.Branch, nil
}

func (p movedPR) GetCompareDiff(context.Context, string, string, string, string, string) (string, bool, error) {
	return p.diff, false, nil
}

func (p movedPR) GetOpenPR(_ context.Context, owner, repo string, number int, _ string) (ports.OpenPR, bool, error) {
	return ports.OpenPR{Owner: owner, Repo: repo, Number: number, HeadSHA: p.head, BaseRef: "main"}, true, nil
}

func (p movedPR) IsAncestor(context.Context, ports.IsAncestorSpec) (bool, error) { return true, nil }

// allowingAuthorizer authorizes every request and counts them.
type allowingAuthorizer struct {
	mu    sync.Mutex
	asked int
}

func (a *allowingAuthorizer) AuthorizeReviewRequest(context.Context, ports.ReviewRequest) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked++
	return true, nil
}

func (a *allowingAuthorizer) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asked
}

// owedReplica is one "pod" wired as the control plane wires the owed
// request's path: the pull request read through pr, a person's
// authorization asked of auth.
func owedReplica(ctx context.Context, t *testing.T, h *Harness, pr movedPR, auth ports.ReviewRequestAuthorizer) *sessionactor.Registry {
	t.Helper()
	r, err := sessionactor.NewRegistry(ctx, h.Pool, h.Timeouts, h.Hub, &promptRecorder{}, &recordingProvider{}, "http://localhost:8080", nil, nil, "", nil, false,
		sessionactor.RegistryOptions{
			ReviewDiffFetcher: pr, ReviewLiveReader: pr, GitHubBotHandle: "narvi-bot",
			GitHubOutbound:          platform.MustNewGitHubOutboundConfig("test-token"),
			ReviewRequestAuthorizer: auth,
		})
	if err != nil {
		t.Fatalf("sessionactor.NewRegistry: %v", err)
	}
	return r
}

// TestOwedReviewRequest_ARestartBetweenTheMoveAndTheReRunLosesNothing is
// the exit's third sentence's second half (technical plan §24.9): replica
// A's dispatch finds a person's queued review request moved, and commits
// its end with the request owed; A shuts down before anything re-runs it.
// Replica B, which never saw the move, claims the owed_review_request timer
// with its pump and re-runs the request for the head the pull request has
// now.
func TestOwedReviewRequest_ARestartBetweenTheMoveAndTheReRunLosesNothing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	pr := movedPR{head: "sha-pushed-while-queued", diff: "+ a line the push added"}

	requester, err := narvipg.NewUserStore(h.Pool).Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: fmt.Sprintf("owed-restart-%d@example.com", time.Now().UnixNano()), DisplayName: "Requester", Role: sqlcgen.UserRoleMaintainer,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := h.Sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceGithub,
		Repos:       []byte(`[{"name":"widgets","url":"https://github.com/acme/owed-restart.git","branch":null}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	prSessions := narvipg.NewGitHubPRSessionStore(h.Pool)
	if err := prSessions.EnsureRow(ctx, "acme/owed-restart", 5); err != nil {
		t.Fatal(err)
	}
	if err := prSessions.SetSessionID(ctx, "acme/owed-restart", 5, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Sandboxes.Create(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready', gen = 1 WHERE session_id = $1`, session.ID); err != nil {
		t.Fatal(err)
	}
	// The review the request waited behind: created before it, ended after
	// it was created.
	earlier, err := h.Turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusPending, IsReviewAttempt: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(ctx, `UPDATE turns SET status = 'completed', created_at = now() - interval '30 seconds', completed_at = now() - interval '2 seconds' WHERE id = $1`, earlier.ID); err != nil {
		t.Fatal(err)
	}
	recorded, err := json.Marshal(reviewverdict.Context{BaseRef: "main", BaseSHA: "base-recorded", PolicyVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	head, text, trigger := "sha-recorded-at-the-click", "Manual re-review requested via the web review button.", turn.RequestTriggerButton
	prompt := text + " Head: " + head
	attempt, err := h.Turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: session.ID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt, ReviewHeadSha: &head, ReviewVerdictContext: recorded,
		IsReviewAttempt: true, RequestTrigger: &trigger, RequestedBy: requester.ID, RequestText: &text,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(ctx, `UPDATE turns SET created_at = now() - interval '10 seconds' WHERE id = $1`, attempt.ID); err != nil {
		t.Fatal(err)
	}

	// Replica A: the dispatch that finds the request moved.
	authA := &allowingAuthorizer{}
	a := owedReplica(ctx, t, h, pr, authA)
	actor, err := a.GetOrSpawn(ctx, session.ID)
	if err != nil {
		_ = a.Shutdown()
		t.Fatalf("A GetOrSpawn: %v", err)
	}
	if err := actor.Send(ctx, sessionactor.EnsureDispatched{}); err != nil {
		_ = a.Shutdown()
		t.Fatal(err)
	}
	owedCount := func() int {
		var n int
		if err := h.Pool.QueryRow(ctx, `SELECT count(*) FROM owed_review_requests WHERE session_id = $1 AND moved_turn_id = $2`, session.ID, attempt.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	waitUntil(t, 5*time.Second, func() bool { return owedCount() == 1 })
	// ...and A goes away before its pump ever delivers the owed timer. Its
	// actors stop on a cancelled context, which Shutdown reports.
	_ = a.Shutdown()
	ended, err := h.Turns.Get(ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ended.EndReason == nil || *ended.EndReason != turn.EndReasonContextMoved {
		t.Fatalf("the moved request's end reason = %v, want context_moved", ended.EndReason)
	}
	if _, err := h.Timers.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: session.ID, Name: sessionactor.TimerOwedReviewRequest}); err != nil {
		t.Fatalf("the owed timer after A went away: %v", err)
	}
	if n := authA.count(); n != 0 {
		t.Fatalf("A asked the authorizer %d times: nothing was re-run before it went away", n)
	}

	// Replica B, which never saw the move, claims the timer.
	authB := &allowingAuthorizer{}
	b := owedReplica(ctx, t, h, pr, authB)
	t.Cleanup(func() { _ = b.Shutdown() })
	var rerun sqlcgen.Turn
	waitUntil(t, 10*time.Second, func() bool {
		if err := b.PumpOnce(ctx); err != nil {
			t.Fatalf("B PumpOnce: %v", err)
		}
		turns, err := h.Turns.ListForSession(ctx, session.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range turns {
			if tr.ReviewHeadSha != nil && *tr.ReviewHeadSha == pr.head {
				rerun = tr
				return true
			}
		}
		return false
	})
	if rerun.RequestTrigger == nil || *rerun.RequestTrigger != trigger || rerun.RequestedBy != requester.ID || rerun.ContextMoves == nil || *rerun.ContextMoves != 1 ||
		rerun.Prompt == nil || !strings.Contains(*rerun.Prompt, pr.diff) {
		t.Fatalf("B's re-run = %+v, want the button's request by its requester, one move, built for %s", rerun, pr.head)
	}
	if n := authB.count(); n != 1 {
		t.Fatalf("B asked the authorizer %d times, want 1", n)
	}
	waitUntil(t, 5*time.Second, func() bool { return owedCount() == 0 })
	if _, err := h.Timers.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: session.ID, Name: sessionactor.TimerOwedReviewRequest}); err == nil {
		t.Fatal("the owed timer outlived the request B re-ran")
	}
}
