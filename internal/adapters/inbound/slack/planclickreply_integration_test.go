//go:build integration

// Integration tests for the reply to a click on a plan's Approve button
// (technical plan §8.1, §13.2): a click that leaves the plan awaiting
// approval -- a turn of the session still open, a refused actor, a role
// that could not be read, a failed authorization check, a failed
// DecidePlan -- is answered to the clicking user alone, through
// chat.postEphemeral, and sends no chat.update, which would strip the
// approval message of its plan and its buttons for everyone in the
// channel; the next click on the same message, once the plan can be
// decided, decides it and shows the outcome on the message. A reply that
// itself fails leaves the message as it was. Each runs on the real handler
// and Postgres, reusing interactive_integration_test.go's rig, as
// TestSlackInteractive_ApproveCutPlan_ReasonEphemeral_MessageKeepsItsBlocks
// runs the cut.
package slack_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/slack"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/platform"
)

// The texts a click on Approve is answered with, as the clicker reads them.
const (
	wantPlanOpenTurnText      = "A revision is already in progress for this plan — try again once it completes."
	wantPlanForbiddenText     = "You don't have permission to approve or reject this plan."
	wantPlanDecisionErrorText = "Something went wrong recording this decision. Please try again."
	wantPlanApprovedText      = "✅ Approved — implementation started."
)

// The approval message every click in this file is made on.
const (
	planClickChannel   = "C0PLANCLICK"
	planClickMessageTS = "1700000230.000100"
)

// clickApprove posts a signed Approve click on plan's approval message, by
// slackUserID, to handler, and returns how long the handler took to answer
// -- every Slack call the click makes is made before it answers.
func clickApprove(t *testing.T, handler http.HandlerFunc, session sqlcgen.Session, plan sqlcgen.Plan, slackUserID string) time.Duration {
	t.Helper()
	value := slackapi.EncodePlanActionValue(plan.ID.String(), session.ID.String())
	payload := blockActionsPayloadJSONWithUser(slackapi.ActionApprovePlan, value, planClickChannel, planClickMessageTS, "trigger-plan-click", slackUserID)
	rec := httptest.NewRecorder()
	start := time.Now()
	handler(rec, signedInteractivityRequest(t, payload))
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: every click is acked (body=%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	return elapsed
}

// takeSlackCalls drains, without waiting, every chat.update and
// chat.postEphemeral the rig's fake Slack server recorded so far.
func takeSlackCalls(requests chan recordedSlackRequest) (updates, ephemerals []recordedSlackRequest) {
	for {
		select {
		case got := <-requests:
			switch got.path {
			case "/chat.update":
				updates = append(updates, got)
			case "/chat.postEphemeral":
				ephemerals = append(ephemerals, got)
			}
		default:
			return updates, ephemerals
		}
	}
}

// assertAnsweredPrivately fails unless the click sent no chat.update -- the
// approval message keeps its plan and its buttons -- and exactly one
// chat.postEphemeral carrying text, to slackUserID alone, on the message.
func assertAnsweredPrivately(t *testing.T, updates, ephemerals []recordedSlackRequest, slackUserID, text string) {
	t.Helper()
	if len(updates) != 0 {
		t.Errorf("chat.update sent %d time(s) (%v), want none: the approval message must keep its plan and its buttons", len(updates), updates)
	}
	if len(ephemerals) != 1 {
		t.Fatalf("chat.postEphemeral sent %d time(s) (%v), want exactly one, to the clicker", len(ephemerals), ephemerals)
	}
	got := ephemerals[0].body
	if got["text"] != text || got["user"] != slackUserID || got["channel"] != planClickChannel || got["thread_ts"] != planClickMessageTS {
		t.Errorf("chat.postEphemeral = %v, want %q to %s alone in %s, on the message %s", got, text, slackUserID, planClickChannel, planClickMessageTS)
	}
}

// assertOutcomeOnTheMessage fails unless the click sent exactly one
// chat.update showing text on the approval message itself, and nothing
// ephemeral.
func assertOutcomeOnTheMessage(t *testing.T, updates, ephemerals []recordedSlackRequest, text string) {
	t.Helper()
	if len(ephemerals) != 0 {
		t.Errorf("chat.postEphemeral sent %d time(s) (%v), want none for an outcome", len(ephemerals), ephemerals)
	}
	if len(updates) != 1 {
		t.Fatalf("chat.update sent %d time(s) (%v), want exactly one, showing the outcome", len(updates), updates)
	}
	got := updates[0].body
	if got["text"] != text || got["channel"] != planClickChannel || got["ts"] != planClickMessageTS {
		t.Errorf("chat.update = %v, want %q on the message %s in %s", got, text, planClickMessageTS, planClickChannel)
	}
}

// assertPlanStatus fails unless plan is now want.
func assertPlanStatus(ctx context.Context, t *testing.T, pool *pgxpool.Pool, plan sqlcgen.Plan, want sqlcgen.PlanStatus) {
	t.Helper()
	var got sqlcgen.PlanStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, plan.ID).Scan(&got); err != nil {
		t.Fatalf("query plan row: %v", err)
	}
	if got != want {
		t.Errorf("plan status = %q, want %q", got, want)
	}
}

// closedPoolForTest opens a SEPARATE pool on the same database and closes
// it at once: every call through a store built on it fails
// deterministically (pgxpool.ErrClosedPool), with no timing dependency --
// the technique authz_backend_error_integration_test.go uses.
func closedPoolForTest(ctx context.Context, t *testing.T, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	broken, err := narvipg.NewPool(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("new broken pool: %v", err)
	}
	broken.Close()
	return broken
}

// seedAwaitingPlanCreatedBy is seedSessionTurnAndAwaitingPlan for a
// session createdBy started, so a member who started it is entitled to
// approve its plan.
func seedAwaitingPlanCreatedBy(ctx context.Context, t *testing.T, rig *interactiveTestRig, createdBy pgtype.UUID) (sqlcgen.Session, sqlcgen.Plan) {
	t.Helper()
	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: createdBy})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	turn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("seed producing turn: %v", err)
	}
	plan, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: turn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("seed awaiting_approval plan: %v", err)
	}
	return session, plan
}

// TestSlackInteractive_ApproveWhileATurnIsOpen_BusyTextToClickerAlone_ApprovesOnceItEnds:
// a click on Approve while a turn of the session -- the plan's own
// revision -- is pending, dispatched or processing is refused
// (httpapi.ErrPlanOpenTurnInFlight) and answered with the busy text to the
// clicker alone; no chat.update is sent, and the plan stays awaiting
// approval. Once that turn has ended without recording a newer version, a
// click on the same message approves the plan and shows the outcome on the
// message, as before.
func TestSlackInteractive_ApproveWhileATurnIsOpen_BusyTextToClickerAlone_ApprovesOnceItEnds(t *testing.T) {
	for _, status := range []sqlcgen.TurnStatus{sqlcgen.TurnStatusPending, sqlcgen.TurnStatusDispatched, sqlcgen.TurnStatusProcessing} {
		t.Run(string(status), func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			rig := newInteractiveTestRig(t, pool)

			session, plan := seedSessionTurnAndAwaitingPlan(ctx, t, rig)
			revision, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: status, PlanMode: true})
			if err != nil {
				t.Fatalf("seed the plan's open revision: %v", err)
			}

			clickApprove(t, rig.handler, session, plan, interactivityDefaultUserID)
			updates, ephemerals := takeSlackCalls(rig.requests)
			assertAnsweredPrivately(t, updates, ephemerals, interactivityDefaultUserID, wantPlanOpenTurnText)
			assertPlanStatus(ctx, t, pool, plan, sqlcgen.PlanStatusAwaitingApproval)
			if turns, err := rig.turns.ListForSession(ctx, session.ID); err != nil || len(turns) != 2 {
				t.Errorf("turns = %d (err %v), want the 2 seeded: no implementation turn", len(turns), err)
			}

			// The revision fails: it ends and records no newer version, so
			// nothing supersedes the plan.
			if _, err := pool.Exec(ctx, `UPDATE turns SET status = 'failed', completed_at = now() WHERE id = $1`, revision.ID); err != nil {
				t.Fatalf("end the revision: %v", err)
			}

			clickApprove(t, rig.handler, session, plan, interactivityDefaultUserID)
			updates, ephemerals = takeSlackCalls(rig.requests)
			assertOutcomeOnTheMessage(t, updates, ephemerals, wantPlanApprovedText)
			assertPlanStatus(ctx, t, pool, plan, sqlcgen.PlanStatusApproved)
		})
	}
}

// TestSlackInteractive_RefusedOrFailedPlanClick_ReachesClickerAlone_PermittedClickDecides:
// a viewer's click on Approve, a disabled account's, one whose
// authorization check fails (the session store on a closed pool) and one
// whose DecidePlan fails (its transaction's pool closed) each send no
// chat.update and reach the clicker alone -- the permission text for the
// two refusals, the generic text and its "try again" for the two failures
// -- and leave the plan awaiting approval. A permitted member's click on
// the same message then decides the plan: the buttons a refused click
// used to strip were the channel's, not the clicker's.
func TestSlackInteractive_RefusedOrFailedPlanClick_ReachesClickerAlone_PermittedClickDecides(t *testing.T) {
	tests := []struct {
		name string
		// refuse returns the handler and the clicking Slack user of the
		// click that must decide nothing, from the rig's own deps.
		refuse   func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, deps slack.InteractiveDeps) (http.HandlerFunc, string)
		wantText string
	}{
		{
			name: "a viewer's click",
			refuse: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, deps slack.InteractiveDeps) (http.HandlerFunc, string) {
				linkSlackIdentityForTest(ctx, t, pool, "U0VIEWERCLICK", sqlcgen.UserRoleViewer)
				return slack.NewInteractivityHandler(deps), "U0VIEWERCLICK"
			},
			wantText: wantPlanForbiddenText,
		},
		{
			name: "a disabled account's click",
			refuse: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, deps slack.InteractiveDeps) (http.HandlerFunc, string) {
				user := linkSlackIdentityForTest(ctx, t, pool, "U0DISABLEDCLICK", sqlcgen.UserRoleMaintainer)
				if _, err := pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1`, user.ID); err != nil {
					t.Fatalf("disable the account: %v", err)
				}
				return slack.NewInteractivityHandler(deps), "U0DISABLEDCLICK"
			},
			wantText: wantPlanForbiddenText,
		},
		{
			name: "a failed authorization check",
			refuse: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, deps slack.InteractiveDeps) (http.HandlerFunc, string) {
				deps.Sessions = narvipg.NewSessionStore(closedPoolForTest(ctx, t, pool))
				return slack.NewInteractivityHandler(deps), interactivityDefaultUserID
			},
			wantText: wantPlanDecisionErrorText,
		},
		{
			name: "a failed DecidePlan",
			refuse: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, deps slack.InteractiveDeps) (http.HandlerFunc, string) {
				deps.Pool = closedPoolForTest(ctx, t, pool)
				return slack.NewInteractivityHandler(deps), interactivityDefaultUserID
			},
			wantText: wantPlanDecisionErrorText,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			var deps slack.InteractiveDeps
			rig := newInteractiveTestRigWithDeps(t, pool, platform.DefaultTimeouts(), func(d *slack.InteractiveDeps) { deps = *d })

			session, plan := seedSessionTurnAndAwaitingPlan(ctx, t, rig)
			refusing, clicker := tc.refuse(ctx, t, pool, deps)

			clickApprove(t, refusing, session, plan, clicker)
			updates, ephemerals := takeSlackCalls(rig.requests)
			assertAnsweredPrivately(t, updates, ephemerals, clicker, tc.wantText)
			assertPlanStatus(ctx, t, pool, plan, sqlcgen.PlanStatusAwaitingApproval)
			if turns, err := rig.turns.ListForSession(ctx, session.ID); err != nil || len(turns) != 1 {
				t.Errorf("turns = %d (err %v), want the 1 seeded: no implementation turn", len(turns), err)
			}

			clickApprove(t, rig.handler, session, plan, interactivityDefaultUserID)
			updates, ephemerals = takeSlackCalls(rig.requests)
			assertOutcomeOnTheMessage(t, updates, ephemerals, wantPlanApprovedText)
			assertPlanStatus(ctx, t, pool, plan, sqlcgen.PlanStatusApproved)
		})
	}
}

// TestSlackInteractive_ApprovePlan_RoleUnreadable_TryAgainPrivately_ViewerStillRefused:
// an entitled member -- the session's own creator -- whose role cannot be
// read (the user store on a closed pool, so the authorization verdict is
// actorauthz.LinkedActorError) is told, privately, to try again, and
// never that they lack permission; a viewer, whose role is read and
// refused (LinkedActorDenied), is still told they lack permission. Neither
// sends a chat.update.
func TestSlackInteractive_ApprovePlan_RoleUnreadable_TryAgainPrivately_ViewerStillRefused(t *testing.T) {
	tests := []struct {
		name       string
		role       sqlcgen.UserRole
		breakUsers bool
		wantText   string
	}{
		{name: "an entitled member whose role cannot be read", role: sqlcgen.UserRoleMember, breakUsers: true, wantText: wantPlanDecisionErrorText},
		{name: "a viewer whose role is read", role: sqlcgen.UserRoleViewer, wantText: wantPlanForbiddenText},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			var mutate func(*slack.InteractiveDeps)
			if tc.breakUsers {
				brokenUsers := narvipg.NewUserStore(closedPoolForTest(ctx, t, pool))
				mutate = func(d *slack.InteractiveDeps) { d.IdentityLink.Users = brokenUsers }
			}
			rig := newInteractiveTestRigWithDeps(t, pool, platform.DefaultTimeouts(), mutate)

			const clicker = "U0ROLECLICK"
			actor := linkSlackIdentityForTest(ctx, t, pool, clicker, tc.role)
			session, plan := seedAwaitingPlanCreatedBy(ctx, t, rig, actor.ID)

			clickApprove(t, rig.handler, session, plan, clicker)
			updates, ephemerals := takeSlackCalls(rig.requests)
			assertAnsweredPrivately(t, updates, ephemerals, clicker, tc.wantText)
			assertPlanStatus(ctx, t, pool, plan, sqlcgen.PlanStatusAwaitingApproval)
		})
	}
}

// TestInteractivityHandler_ViewSubmission_RoleUnreadable_GenericErrorNotPermission
// is the Request-changes modal's side of the same verdict: a submitter
// whose role cannot be read (the user store on a closed pool) gets the
// generic error under the feedback field, the modal staying open, never
// the permission text, and no turn is created.
func TestInteractivityHandler_ViewSubmission_RoleUnreadable_GenericErrorNotPermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	brokenUsers := narvipg.NewUserStore(closedPoolForTest(ctx, t, pool))
	rig := newInteractiveTestRigWithDeps(t, pool, platform.DefaultTimeouts(), func(d *slack.InteractiveDeps) { d.IdentityLink.Users = brokenUsers })

	session, plan := seedSessionTurnAndAwaitingPlan(ctx, t, rig)
	modal := openRequestChangesModal(t, rig, session, plan)

	got := modalErrors(t, submitRequestChanges(t, rig, modal, "split the migration in two"))
	if len(got) != 1 || got[modal.blockID] != wantRequestChangesErrorText {
		t.Fatalf("errors = %v, want only %q under the feedback block %q", got, wantRequestChangesErrorText, modal.blockID)
	}
	if turns, err := rig.turns.ListForSession(ctx, session.ID); err != nil || len(turns) != 1 {
		t.Errorf("turns = %d (err %v), want the 1 seeded: no revision created", len(turns), err)
	}
}

// TestSlackInteractive_RefusedClick_FailingReply_MessageLeftAsItWas: a
// viewer's click whose private reply itself fails -- Slack refusing it, or
// not answering within SlackInteractivityReplyTimeout -- is logged and
// reaches nobody: the reply is attempted once, nothing falls back to a
// chat.update, the handler still answers within
// SlackInteractivityAckTimeout, and the plan stays awaiting approval. The
// buttons are still there, so a permitted member's click on the same
// message, with Slack answering again, decides the plan.
func TestSlackInteractive_RefusedClick_FailingReply_MessageLeftAsItWas(t *testing.T) {
	tests := []struct {
		name   string
		answer func(w http.ResponseWriter, r *http.Request)
	}{
		{name: "Slack refuses the reply", answer: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
		}},
		{name: "Slack does not answer within the reply budget", answer: func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			timeouts := platform.DefaultTimeouts()
			var deps slack.InteractiveDeps
			rig := newInteractiveTestRigWithDeps(t, pool, timeouts, func(d *slack.InteractiveDeps) { deps = *d })

			var mu sync.Mutex
			var paths []string
			failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				// Read the body through, so the server notices the client
				// giving up and ends r's context.
				_, _ = io.Copy(io.Discard, r.Body)
				tc.answer(w, r)
			}))
			t.Cleanup(failing.Close)
			deps.SlackClient = slackapi.New(failing.Client(), failing.URL, "test-bot-token")

			const viewer = "U0VIEWERFAILREPLY"
			linkSlackIdentityForTest(ctx, t, pool, viewer, sqlcgen.UserRoleViewer)
			session, plan := seedSessionTurnAndAwaitingPlan(ctx, t, rig)

			elapsed := clickApprove(t, slack.NewInteractivityHandler(deps), session, plan, viewer)
			if elapsed >= timeouts.SlackInteractivityAckTimeout {
				t.Errorf("handler answered after %s, want within SlackInteractivityAckTimeout (%s)", elapsed, timeouts.SlackInteractivityAckTimeout)
			}
			mu.Lock()
			got := slices.Clone(paths)
			mu.Unlock()
			if want := []string{"/chat.postEphemeral"}; !slices.Equal(got, want) {
				t.Errorf("Slack calls = %v, want %v: the private reply attempted once, and no chat.update after it fails", got, want)
			}
			assertPlanStatus(ctx, t, pool, plan, sqlcgen.PlanStatusAwaitingApproval)

			clickApprove(t, rig.handler, session, plan, interactivityDefaultUserID)
			updates, ephemerals := takeSlackCalls(rig.requests)
			assertOutcomeOnTheMessage(t, updates, ephemerals, wantPlanApprovedText)
			assertPlanStatus(ctx, t, pool, plan, sqlcgen.PlanStatusApproved)
		})
	}
}

// fakeSlackForPlanClick is a fake Slack Web API for one click: it answers
// users.info with email for any user when email is set (with no email
// otherwise), records every other call on the returned channel, and holds
// a request to a path in hang -- its body read, so the server notices the
// client giving up -- until the client gives up.
func fakeSlackForPlanClick(t *testing.T, email string, hang ...string) (*httptest.Server, chan recordedSlackRequest) {
	t.Helper()
	requests := make(chan recordedSlackRequest, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users.info" {
			w.Header().Set("Content-Type", "application/json")
			if email == "" {
				_, _ = w.Write([]byte(`{"ok":true}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "user": map[string]any{"profile": map[string]any{"email": email}}})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.Copy(io.Discard, r.Body)
		requests <- recordedSlackRequest{path: r.URL.Path, body: body}
		if slices.Contains(hang, r.URL.Path) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)
	return server, requests
}

// holdLockForTest runs statement in a transaction of its own and keeps it
// open until the test ends, standing in for a migration or a competing
// writer holding what the click needs.
func holdLockForTest(ctx context.Context, t *testing.T, pool *pgxpool.Pool, statement string, args ...any) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin competing lock tx: %v", err)
	}
	if _, err := tx.Exec(ctx, statement, args...); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("take the competing lock: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
}

// TestSlackInteractive_IdentityNotResolved_TryAgainPrivately_UnlinkedStillToldToLink:
// a click whose identity resolution does not complete -- a linked member
// whose identities read waits past the decision's share behind a table
// lock, the same member with the identity store on a closed pool, or an
// unlinked clicker whose Resolve fails (the user store on a closed pool)
// -- is a backend failure, not an unlinked actor: the generic text and its
// "try again" reach the clicker alone, never the not-linked text, no
// chat.update is sent, and the handler answers within
// SlackInteractivityAckTimeout. An unlinked clicker whose resolution
// completes and finds no link is still told to link.
func TestSlackInteractive_IdentityNotResolved_TryAgainPrivately_UnlinkedStillToldToLink(t *testing.T) {
	const (
		unlinkedClicker   = "U0UNLINKEDCLICK"
		wantNotLinkedText = "Your Slack account isn't linked to a Narvi account yet, so this decision wasn't recorded. Once you're linked (see the message just above), click Approve/Reject again."
	)
	tests := []struct {
		name    string
		clicker string
		// breakIt breaks what the row breaks, on the handler's deps or in
		// the database, once the rig and the plan are seeded.
		breakIt  func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, deps *slack.InteractiveDeps)
		wantText string
	}{
		{
			name:    "a linked member whose identity read waits past the decision's share",
			clicker: interactivityDefaultUserID,
			breakIt: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, _ *slack.InteractiveDeps) {
				holdLockForTest(ctx, t, pool, `LOCK TABLE identities IN ACCESS EXCLUSIVE MODE`)
			},
			wantText: wantPlanDecisionErrorText,
		},
		{
			name:    "a linked member whose identity store is on a closed pool",
			clicker: interactivityDefaultUserID,
			breakIt: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, deps *slack.InteractiveDeps) {
				broken := closedPoolForTest(ctx, t, pool)
				deps.IdentityLink.Identities = narvipg.NewIdentityStore(broken)
				deps.IdentityLink.Pool = broken
			},
			wantText: wantPlanDecisionErrorText,
		},
		{
			name:    "an unlinked clicker whose resolution fails",
			clicker: unlinkedClicker,
			breakIt: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, deps *slack.InteractiveDeps) {
				deps.IdentityLink.Users = narvipg.NewUserStore(closedPoolForTest(ctx, t, pool))
			},
			wantText: wantPlanDecisionErrorText,
		},
		{
			name:     "an unlinked clicker whose resolution completes",
			clicker:  unlinkedClicker,
			wantText: wantNotLinkedText,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			// A window short enough to keep the lock row quick, with the
			// identity fetch (800ms) still inside the decision's share.
			timeouts := platform.DefaultTimeouts()
			timeouts.SlackInteractivityAckTimeout = 1500 * time.Millisecond
			timeouts.SlackInteractivityReplyTimeout = 500 * time.Millisecond
			var deps slack.InteractiveDeps
			rig := newInteractiveTestRigWithDeps(t, pool, timeouts, func(d *slack.InteractiveDeps) { deps = *d })
			// The clicker's profile email matches no account, so a
			// resolution that completes for the unlinked clicker finds no
			// link and mints a link prompt.
			server, requests := fakeSlackForPlanClick(t, "nobody-plan-click@example.com")
			deps.SlackClient = slackapi.New(server.Client(), server.URL, "test-bot-token")

			session, plan := seedSessionTurnAndAwaitingPlan(ctx, t, rig)
			if tc.breakIt != nil {
				tc.breakIt(ctx, t, pool, &deps)
			}

			elapsed := clickApprove(t, slack.NewInteractivityHandler(deps), session, plan, tc.clicker)
			if elapsed >= timeouts.SlackInteractivityAckTimeout {
				t.Errorf("handler answered after %s, want within SlackInteractivityAckTimeout (%s)", elapsed, timeouts.SlackInteractivityAckTimeout)
			}

			updates, ephemerals := takeSlackCalls(requests)
			if len(updates) != 0 {
				t.Errorf("chat.update sent %d time(s) (%v), want none: the approval message must keep its plan and its buttons", len(updates), updates)
			}
			// The identity-link notice a completed resolution posts is an
			// ephemeral too; the reply to the click is the one carrying
			// either of the two texts.
			var replies []recordedSlackRequest
			for _, e := range ephemerals {
				if text := e.body["text"]; text == wantPlanDecisionErrorText || text == wantNotLinkedText {
					replies = append(replies, e)
				}
			}
			if len(replies) != 1 {
				t.Fatalf("replies to the click = %v, want exactly one, %q", replies, tc.wantText)
			}
			got := replies[0].body
			if got["text"] != tc.wantText || got["user"] != tc.clicker || got["channel"] != planClickChannel || got["thread_ts"] != planClickMessageTS {
				t.Errorf("chat.postEphemeral = %v, want %q to %s alone in %s, on the message %s", got, tc.wantText, tc.clicker, planClickChannel, planClickMessageTS)
			}
			assertPlanStatus(ctx, t, pool, plan, sqlcgen.PlanStatusAwaitingApproval)
		})
	}
}

// TestInteractivityHandler_ViewSubmission_IdentityNotResolved_GenericErrorNotPermission
// is the Request-changes modal's side of the same rule: a submitter whose
// identity read fails (the identity store on a closed pool) gets the
// generic error under the feedback field, the modal staying open, never
// the permission text, and no turn is created.
func TestInteractivityHandler_ViewSubmission_IdentityNotResolved_GenericErrorNotPermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	broken := closedPoolForTest(ctx, t, pool)
	rig := newInteractiveTestRigWithDeps(t, pool, platform.DefaultTimeouts(), func(d *slack.InteractiveDeps) {
		d.IdentityLink.Identities = narvipg.NewIdentityStore(broken)
		d.IdentityLink.Pool = broken
	})

	session, plan := seedSessionTurnAndAwaitingPlan(ctx, t, rig)
	modal := openRequestChangesModal(t, rig, session, plan)

	got := modalErrors(t, submitRequestChanges(t, rig, modal, "split the migration in two"))
	if len(got) != 1 || got[modal.blockID] != wantRequestChangesErrorText {
		t.Fatalf("errors = %v, want only %q under the feedback block %q", got, wantRequestChangesErrorText, modal.blockID)
	}
	if turns, err := rig.turns.ListForSession(ctx, session.ID); err != nil || len(turns) != 1 {
		t.Errorf("turns = %d (err %v), want the 1 seeded: no revision created", len(turns), err)
	}
}

// TestSlackInteractive_ApproveOnADecidedPlan_OutcomeOnTheMessage: an
// Approve click on a plan already decided or superseded is answered with
// its outcome on the message, through chat.update, and nothing private --
// whether or not a turn of the session is open. DecidePlanOnTx refuses an
// Approve while a turn is open before it reads the plan, and right after
// an approval the plan's own implementation is that turn, so the click
// reads the plan again rather than tell the clicker a revision is running.
// A plan still awaiting approval with an open turn still gets the private
// busy text
// (TestSlackInteractive_ApproveWhileATurnIsOpen_BusyTextToClickerAlone_ApprovesOnceItEnds).
func TestSlackInteractive_ApproveOnADecidedPlan_OutcomeOnTheMessage(t *testing.T) {
	const (
		wantAlreadyApprovedText = "✅ Already approved (via a different channel)."
		wantAlreadyRejectedText = "❌ Already rejected (via a different channel)."
		wantSupersededText      = "This plan was superseded by a newer revision."
	)
	type planSpec struct {
		status sqlcgen.PlanStatus
		// implStatus, when set, seeds the plan's implementation turn.
		implStatus sqlcgen.TurnStatus
	}
	tests := []struct {
		name string
		// plans are seeded in order, v1 first; the click is on v1.
		plans    []planSpec
		wantText string
	}{
		{name: "approved, its implementation processing", plans: []planSpec{{sqlcgen.PlanStatusApproved, sqlcgen.TurnStatusProcessing}}, wantText: wantAlreadyApprovedText},
		{name: "approved, its implementation pending", plans: []planSpec{{sqlcgen.PlanStatusApproved, sqlcgen.TurnStatusPending}}, wantText: wantAlreadyApprovedText},
		{name: "approved, its implementation completed", plans: []planSpec{{sqlcgen.PlanStatusApproved, sqlcgen.TurnStatusCompleted}}, wantText: wantAlreadyApprovedText},
		{name: "rejected", plans: []planSpec{{status: sqlcgen.PlanStatusRejected}}, wantText: wantAlreadyRejectedText},
		{name: "superseded by an approved version whose implementation is processing", plans: []planSpec{{status: sqlcgen.PlanStatusSuperseded}, {sqlcgen.PlanStatusApproved, sqlcgen.TurnStatusProcessing}}, wantText: wantSupersededText},
		{name: "superseded by a version awaiting approval", plans: []planSpec{{status: sqlcgen.PlanStatusSuperseded}, {status: sqlcgen.PlanStatusAwaitingApproval}}, wantText: wantSupersededText},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			rig := newInteractiveTestRig(t, pool)

			session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			var clicked sqlcgen.Plan
			for i, spec := range tc.plans {
				planTurn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
				if err != nil {
					t.Fatalf("seed plan-mode turn v%d: %v", i+1, err)
				}
				plan, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: planTurn.ID, Version: int32(i + 1), Status: spec.status})
				if err != nil {
					t.Fatalf("seed plan v%d: %v", i+1, err)
				}
				if spec.implStatus != "" {
					if _, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: spec.implStatus, PlanMode: false}); err != nil {
						t.Fatalf("seed v%d's implementation turn: %v", i+1, err)
					}
				}
				if i == 0 {
					clicked = plan
				}
			}
			turnsBefore, err := rig.turns.ListForSession(ctx, session.ID)
			if err != nil {
				t.Fatalf("list turns: %v", err)
			}

			clickApprove(t, rig.handler, session, clicked, interactivityDefaultUserID)
			updates, ephemerals := takeSlackCalls(rig.requests)
			assertOutcomeOnTheMessage(t, updates, ephemerals, tc.wantText)
			assertPlanStatus(ctx, t, pool, clicked, tc.plans[0].status)
			if turns, err := rig.turns.ListForSession(ctx, session.ID); err != nil || len(turns) != len(turnsBefore) {
				t.Errorf("turns = %d (err %v), want the %d seeded: nothing dispatched", len(turns), err, len(turnsBefore))
			}
		})
	}
}

// TestSlackInteractive_HangingReply_HandlerAnswersWithinItsBudget: a reply
// that Slack never answers is cut off at SlackInteractivityReplyTimeout,
// counted from the moment the reply starts, on a context detached from the
// decision's. A won decision whose chat.update hangs answers within about
// that budget -- not the decision's remaining share, and not never; a
// decision that ran out of its share, whose private reply then hangs,
// answers within the whole window, never the decision's share twice over.
func TestSlackInteractive_HangingReply_HandlerAnswersWithinItsBudget(t *testing.T) {
	tests := []struct {
		name string
		hang string
		// lockSession holds the session row past the decision's share.
		lockSession bool
		wantStatus  sqlcgen.PlanStatus
		// maxElapsed is the latest the handler may answer, for timeouts.
		maxElapsed func(platform.Timeouts) time.Duration
	}{
		{
			name:       "a won decision whose chat.update hangs",
			hang:       "/chat.update",
			wantStatus: sqlcgen.PlanStatusApproved,
			maxElapsed: func(to platform.Timeouts) time.Duration {
				return to.SlackInteractivityReplyTimeout + 500*time.Millisecond
			},
		},
		{
			name:        "a decision past its share whose private reply hangs",
			hang:        "/chat.postEphemeral",
			lockSession: true,
			wantStatus:  sqlcgen.PlanStatusAwaitingApproval,
			maxElapsed: func(to platform.Timeouts) time.Duration {
				return to.SlackInteractivityAckTimeout + 400*time.Millisecond
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			timeouts := platform.DefaultTimeouts()
			var deps slack.InteractiveDeps
			rig := newInteractiveTestRigWithDeps(t, pool, timeouts, func(d *slack.InteractiveDeps) { deps = *d })
			server, requests := fakeSlackForPlanClick(t, "", tc.hang)
			deps.SlackClient = slackapi.New(server.Client(), server.URL, "test-bot-token")

			session, plan := seedSessionTurnAndAwaitingPlan(ctx, t, rig)
			if tc.lockSession {
				holdLockForTest(ctx, t, pool, `SELECT 1 FROM sessions WHERE id = $1 FOR UPDATE`, session.ID)
			}

			elapsed := clickApprove(t, slack.NewInteractivityHandler(deps), session, plan, interactivityDefaultUserID)
			if limit := tc.maxElapsed(timeouts); elapsed >= limit {
				t.Errorf("handler answered after %s, want under %s: the hanging reply must be cut off at SlackInteractivityReplyTimeout (%s) from its own start", elapsed, limit, timeouts.SlackInteractivityReplyTimeout)
			}
			var hung int
			for len(requests) > 0 {
				if got := <-requests; got.path == tc.hang {
					hung++
				}
			}
			if hung != 1 {
				t.Errorf("%s attempted %d time(s), want once", tc.hang, hung)
			}
			assertPlanStatus(ctx, t, pool, plan, tc.wantStatus)
		})
	}
}

// slackCallContext is what contextRecordingSlack saw of one call: the
// method, when it was made, its context's deadline and correlation id,
// and its text.
type slackCallContext struct {
	method        string
	at            time.Time
	deadline      time.Time
	hasDeadline   bool
	correlationID string
	text          string
}

// contextRecordingSlack is a Slack client that answers every call at once
// and records the context each was made on -- so a test can read which
// budget a reply or a notice ran on, and whether it kept the request's
// values. GetUserEmail answers email, matching no account.
type contextRecordingSlack struct {
	email string

	mu    sync.Mutex
	calls []slackCallContext
}

func (c *contextRecordingSlack) record(ctx context.Context, method, text string) {
	call := slackCallContext{method: method, at: time.Now(), text: text}
	call.deadline, call.hasDeadline = ctx.Deadline()
	call.correlationID, _ = platform.CorrelationIDFromContext(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
}

func (c *contextRecordingSlack) callsTo(method string) []slackCallContext {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []slackCallContext
	for _, call := range c.calls {
		if call.method == method {
			out = append(out, call)
		}
	}
	return out
}

func (c *contextRecordingSlack) GetUserEmail(context.Context, string) (string, bool, error) {
	return c.email, c.email != "", nil
}

func (c *contextRecordingSlack) PostAck(ctx context.Context, _, _, text string) error {
	c.record(ctx, "PostAck", text)
	return nil
}

func (c *contextRecordingSlack) PostIdentityLinkNotice(ctx context.Context, _, _, _, text string) error {
	c.record(ctx, "PostIdentityLinkNotice", text)
	return nil
}

func (c *contextRecordingSlack) PostEphemeral(ctx context.Context, _, _, _, text string) error {
	c.record(ctx, "PostEphemeral", text)
	return nil
}

func (c *contextRecordingSlack) UpdateMessage(ctx context.Context, _, _, text string) error {
	c.record(ctx, "UpdateMessage", text)
	return nil
}

func (c *contextRecordingSlack) OpenView(ctx context.Context, _, _, _ string) error {
	c.record(ctx, "OpenView", "")
	return nil
}

// TestSlackInteractive_ReplyAndNoticeContexts: the reply to a click -- an
// outcome's chat.update, or a private reply, the answer to an Approve
// refused for an open turn included, on a plan still awaiting approval or
// already decided -- runs on a context of its own: bounded by
// SlackInteractivityReplyTimeout from the moment it is made, never by the
// decision's deadline or by nothing, and carrying the request's values
// (its correlation id). The identity-link notice a first click gets is not
// the reply: it runs inside the decision's share, on the decision's own
// deadline.
func TestSlackInteractive_ReplyAndNoticeContexts(t *testing.T) {
	const correlationID = "corr-plan-click"
	tests := []struct {
		name    string
		clicker string
		// replyMethod is the client call that answers the click.
		replyMethod string
		wantNotice  bool
		// planStatus, when set, replaces the seeded plan's status;
		// openTurn, when set, seeds a turn of the session at that status.
		planStatus sqlcgen.PlanStatus
		openTurn   sqlcgen.TurnStatus
	}{
		{name: "a won decision's outcome", clicker: interactivityDefaultUserID, replyMethod: "UpdateMessage"},
		{name: "a first click with no link", clicker: "U0FIRSTCLICK", replyMethod: "PostEphemeral", wantNotice: true},
		{name: "an open-turn refusal on a plan awaiting approval", clicker: interactivityDefaultUserID, replyMethod: "PostEphemeral", openTurn: sqlcgen.TurnStatusProcessing},
		{name: "an open-turn refusal on a plan already approved", clicker: interactivityDefaultUserID, replyMethod: "UpdateMessage", planStatus: sqlcgen.PlanStatusApproved, openTurn: sqlcgen.TurnStatusProcessing},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			timeouts := platform.DefaultTimeouts()
			var deps slack.InteractiveDeps
			rig := newInteractiveTestRigWithDeps(t, pool, timeouts, func(d *slack.InteractiveDeps) { deps = *d })
			client := &contextRecordingSlack{email: "nobody-first-click@example.com"}
			deps.SlackClient = client
			session, plan := seedSessionTurnAndAwaitingPlan(ctx, t, rig)
			if tc.planStatus != "" {
				if _, err := pool.Exec(ctx, `UPDATE plans SET status = $1 WHERE id = $2`, tc.planStatus, plan.ID); err != nil {
					t.Fatalf("set the plan's status: %v", err)
				}
			}
			if tc.openTurn != "" {
				if _, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: tc.openTurn, PlanMode: tc.planStatus == ""}); err != nil {
					t.Fatalf("seed the open turn: %v", err)
				}
			}

			value := slackapi.EncodePlanActionValue(plan.ID.String(), session.ID.String())
			req := signedInteractivityRequest(t, blockActionsPayloadJSONWithUser(slackapi.ActionApprovePlan, value, planClickChannel, planClickMessageTS, "trigger-plan-click", tc.clicker))
			req = req.WithContext(platform.WithCorrelationID(req.Context(), correlationID))
			rec := httptest.NewRecorder()
			start := time.Now()
			slack.NewInteractivityHandler(deps)(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			replies := client.callsTo(tc.replyMethod)
			if len(replies) != 1 {
				t.Fatalf("%s calls = %v, want exactly one, the reply to the click", tc.replyMethod, replies)
			}
			reply := replies[0]
			if !reply.hasDeadline || reply.deadline.After(reply.at.Add(timeouts.SlackInteractivityReplyTimeout)) || !reply.deadline.After(reply.at) {
				t.Errorf("reply %s ran on a context with deadline %v (set: %v), want one at most SlackInteractivityReplyTimeout (%s) after the call at %v: its own budget, not the decision's and not none",
					tc.replyMethod, reply.deadline, reply.hasDeadline, timeouts.SlackInteractivityReplyTimeout, reply.at)
			}
			if reply.correlationID != correlationID {
				t.Errorf("reply %s ran with correlation id %q, want the request's %q: the reply is detached from the decision's deadline, not from its values", tc.replyMethod, reply.correlationID, correlationID)
			}

			notices := client.callsTo("PostIdentityLinkNotice")
			if !tc.wantNotice {
				if len(notices) != 0 {
					t.Errorf("identity-link notices = %v, want none for a linked clicker", notices)
				}
				return
			}
			if len(notices) != 1 {
				t.Fatalf("identity-link notices = %v, want exactly one for a first click with no link", notices)
			}
			notice := notices[0]
			share := timeouts.SlackInteractivityDecisionTimeout()
			if !notice.hasDeadline || notice.deadline.Before(start.Add(share)) || notice.deadline.After(notice.at.Add(share)) {
				t.Errorf("identity-link notice ran on a context with deadline %v (set: %v), want the decision's own, %s after the click began (%v): the notice belongs to the decision's share",
					notice.deadline, notice.hasDeadline, share, start)
			}
		})
	}
}

// TestSlackInteractive_OpenTurnRefusal_PlanReadStalled_BusyTextStillReachesTheClicker:
// an Approve click refused for an open turn reads the plan again, and that
// read can stall -- here another transaction holds every read of plans
// (LOCK TABLE plans IN ACCESS EXCLUSIVE MODE), standing in for a migration
// on plans or an exhausted pool. Nothing earlier in the click reads plans,
// so the refusal comes at once and the read waits. The read runs on what
// is left of the decision's share and the answer on a fresh reply budget,
// so the busy text still reaches the clicker alone, no chat.update is
// sent, and the handler answers within SlackInteractivityAckTimeout --
// a read and a reply sharing one budget answered nobody.
func TestSlackInteractive_OpenTurnRefusal_PlanReadStalled_BusyTextStillReachesTheClicker(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	timeouts := platform.DefaultTimeouts()
	timeouts.SlackInteractivityAckTimeout = 1500 * time.Millisecond
	timeouts.SlackInteractivityReplyTimeout = 500 * time.Millisecond
	rig := newInteractiveTestRigWithTimeouts(t, pool, timeouts)

	session, plan := seedSessionTurnAndAwaitingPlan(ctx, t, rig)
	if _, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusProcessing, PlanMode: true}); err != nil {
		t.Fatalf("seed the plan's open revision: %v", err)
	}

	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin competing lock tx: %v", err)
	}
	if _, err := lockTx.Exec(ctx, `LOCK TABLE plans IN ACCESS EXCLUSIVE MODE`); err != nil {
		_ = lockTx.Rollback(ctx)
		t.Fatalf("lock plans: %v", err)
	}
	// Released at the latest a few seconds on, so a read with no bound at
	// all fails this test on time instead of hanging it.
	releaseTimer := time.AfterFunc(5*time.Second, func() { _ = lockTx.Rollback(ctx) })
	t.Cleanup(func() {
		releaseTimer.Stop()
		_ = lockTx.Rollback(ctx)
	})

	elapsed := clickApprove(t, rig.handler, session, plan, interactivityDefaultUserID)
	if elapsed >= timeouts.SlackInteractivityAckTimeout {
		t.Errorf("handler answered after %s, want within SlackInteractivityAckTimeout (%s): the stalled read must end with the decision's share (%s)",
			elapsed, timeouts.SlackInteractivityAckTimeout, timeouts.SlackInteractivityDecisionTimeout())
	}
	updates, ephemerals := takeSlackCalls(rig.requests)
	assertAnsweredPrivately(t, updates, ephemerals, interactivityDefaultUserID, wantPlanOpenTurnText)

	releaseTimer.Stop()
	_ = lockTx.Rollback(ctx)
	assertPlanStatus(ctx, t, pool, plan, sqlcgen.PlanStatusAwaitingApproval)
}
