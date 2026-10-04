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
