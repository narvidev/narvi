//go:build integration

package slack_test

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
)

// This file is technical plan §40.1's spend cap as the chat surface meets
// it: a reply in a session's thread and a Request-changes submission on a
// session that has spent its cap are refused, answered with the refusal's
// own text, and create nothing.

// capSessionAt caps repo at $1.00 and records that sessionID's turns cost
// $1.25 in all: turnID, when valid, becomes that completed, dispatched turn;
// otherwise one is stored. It returns the refusal the guard then makes,
// with the session's turns as they stand: a refusal adds none.
func capSessionAt(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID, turnID pgtype.UUID, repo string) sessionguard.Refusal {
	t.Helper()
	if turnID.Valid {
		if _, err := pool.Exec(ctx, `UPDATE turns SET status = 'completed', dispatched_at = now(), completed_at = now(), cost_usd = 1.25 WHERE id = $1`, turnID); err != nil {
			t.Fatalf("record the turn's spend: %v", err)
		}
	} else if _, err := pool.Exec(ctx, `INSERT INTO turns (session_id, status, dispatched_at, completed_at, cost_usd) VALUES ($1, 'completed', now(), now(), 1.25)`, sessionID); err != nil {
		t.Fatalf("store the session's spend: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repo_settings (repo_full_name, session_spend_cap_usd) VALUES ($1, 1.00)
		ON CONFLICT (repo_full_name) DO UPDATE SET session_spend_cap_usd = EXCLUDED.session_spend_cap_usd`, repo); err != nil {
		t.Fatalf("cap the repository: %v", err)
	}
	var turns int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, sessionID).Scan(&turns); err != nil {
		t.Fatalf("count the session's turns: %v", err)
	}
	return sessionguard.Refusal{
		Reason: sessionguard.ReasonSpendCap, SessionID: sessionID.Bytes, Cap: 1_000_000, Spent: 1_250_000,
		Source: sessionguard.CapSource{Kind: sessionguard.CapSourceRepo, Name: repo, ID: repo}, Turns: turns,
	}
}

// guardRecords counts sessionID's warnings at refusal's crossing and its
// Slack guard notices.
func guardRecords(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, refusal sessionguard.Refusal) (warnings, notices int) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND message_id = $2`, sessionID, turnguard.WarningMessageID(refusal)).Scan(&warnings); err != nil {
		t.Fatalf("count warnings: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, sessionID, string(ports.NotificationKindSlackSessionGuard)).Scan(&notices); err != nil {
		t.Fatalf("count notices: %v", err)
	}
	return warnings, notices
}

// TestSlackAddTurn_AtSpendCap_HonestReply: a reply in the thread of a
// session that has spent its cap creates no turn and is answered in the
// thread with the refusal's own text -- never a failed delivery. The
// crossing's warning is recorded, and that reply in the session's own
// thread is the crossing's one telling: no outbox notice repeats it there.
func TestSlackAddTurn_AtSpendCap_HonestReply(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	auditLog := narvipg.NewAuditLogStore(pool)
	recordingServer, recordedBodies := newFakeSlackRecordingWithUsersInfo(t, "unused", "unused@example.com")
	linkSlackIdentityForTest(ctx, t, pool, "U0TESTUSER", sqlcgen.UserRoleMaintainer)
	linkSlackIdentityForTest(ctx, t, pool, "U0OTHERUSER", sqlcgen.UserRoleMaintainer)
	rig := newSlackPlanGateTestRig(t, pool, recordingServer, auditLog)

	rec := httptest.NewRecorder()
	rig.handler(rec, signedSlackRequest(t, appMentionEnvelope("Ev0SPENDCAP001", "C0SPENDCAP", "1700000050.000100", "", "start this task")))
	if rec.Code != http.StatusOK {
		t.Fatalf("first mention: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	mapping, err := rig.threads.Get(ctx, "C0SPENDCAP", "1700000050.000100")
	if err != nil {
		t.Fatalf("Get thread mapping: %v", err)
	}
	firstTurns, err := rig.turns.ListForSession(ctx, mapping.SessionID)
	if err != nil || len(firstTurns) != 1 {
		t.Fatalf("ListForSession after first mention: turns=%v err=%v, want exactly 1", firstTurns, err)
	}
	refusal := capSessionAt(ctx, t, pool, mapping.SessionID, firstTurns[0].ID, "narvidev/narvi")
	// Drain the first mention's own acknowledgements.
	for drained := false; !drained; {
		select {
		case <-recordedBodies:
		case <-time.After(50 * time.Millisecond):
			drained = true
		}
	}

	rec = httptest.NewRecorder()
	rig.handler(rec, signedSlackRequest(t, messageEnvelope("Ev0SPENDCAP002", "C0SPENDCAP", "1700000051.000200", "1700000050.000100", "one more thing please")))
	if rec.Code != http.StatusOK {
		t.Fatalf("reply: status = %d, want 200: a session at its cap is no failed delivery (body=%s)", rec.Code, rec.Body.String())
	}
	if turns, err := rig.turns.ListForSession(ctx, mapping.SessionID); err != nil || len(turns) != 1 {
		t.Fatalf("turns after the reply = %d (%v), want the first mention's alone", len(turns), err)
	}

	var replies []string
drain:
	for {
		select {
		case got := <-recordedBodies:
			if got.path != "/chat.postMessage" {
				continue
			}
			if text, ok := got.body["text"].(string); ok {
				replies = append(replies, text)
			}
		default:
			break drain
		}
	}
	if len(replies) != 1 || replies[0] != sessionguard.Text(refusal) {
		t.Fatalf("thread replies = %q, want the refusal's text alone", replies)
	}
	if warnings, notices := guardRecords(ctx, t, pool, mapping.SessionID, refusal); warnings != 1 || notices != 0 {
		t.Fatalf("warnings %d, notices %d; want the crossing's warning, and no notice beside the thread reply", warnings, notices)
	}
}

// TestSlackRequestChanges_AtSpendCap_ModalError: a Request-changes
// submission on a session that has spent its cap keeps the modal open with
// the refusal's own text under its feedback block -- the feedback is not
// lost -- and creates no turn; once the cap is raised, the same submission
// creates the revision.
func TestSlackRequestChanges_AtSpendCap_ModalError(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newInteractiveTestRig(t, pool)

	session, plan, impl := seedApprovedImplementation(ctx, t, rig, sqlcgen.TurnStatusCompleted)
	if _, err := pool.Exec(ctx, `UPDATE sessions SET repos = '[{"url": "https://github.com/acme/modal-cap.git"}]'::jsonb WHERE id = $1`, session.ID); err != nil {
		t.Fatalf("name the session's repository: %v", err)
	}
	refusal := capSessionAt(ctx, t, pool, session.ID, impl.ID, "acme/modal-cap")
	modal := openRequestChangesModal(t, rig, session, plan)

	const feedback = "split the migration in two"
	got := modalErrors(t, submitRequestChanges(t, rig, modal, feedback))
	if want := map[string]string{modal.blockID: sessionguard.Text(refusal)}; !maps.Equal(got, want) {
		t.Fatalf("errors = %v, want the refusal's text under the feedback block %q", got, modal.blockID)
	}
	if turns, err := rig.turns.ListForSession(ctx, session.ID); err != nil || len(turns) != 2 {
		t.Fatalf("turns after the refused submission = %d (%v), want the 2 seeded turns", len(turns), err)
	}
	var warnings int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND message_id = $2`, session.ID, turnguard.WarningMessageID(refusal)).Scan(&warnings); err != nil {
		t.Fatal(err)
	}
	if warnings != 1 {
		t.Fatalf("warnings = %d, want the crossing's one", warnings)
	}

	if _, err := pool.Exec(ctx, `UPDATE repo_settings SET session_spend_cap_usd = 10 WHERE repo_full_name = 'acme/modal-cap'`); err != nil {
		t.Fatal(err)
	}
	rec := submitRequestChanges(t, rig, modal, feedback)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("submission after the raise: status = %d body = %q, want an empty 200 (closes the modal)", rec.Code, rec.Body.String())
	}
	if turns, err := rig.turns.ListForSession(ctx, session.ID); err != nil || len(turns) != 3 {
		t.Fatalf("turns after the raise = %d (%v), want the revision added", len(turns), err)
	}
}

// slackSessionAtCapWithAwaitingPlan is a Slack-origin session mapped to
// channel's thread threadTS, naming repo, with a plan awaiting approval
// and a completed turn that took it past repo's $1.00 cap. It returns the
// session, the plan and the refusal the guard makes for it.
func slackSessionAtCapWithAwaitingPlan(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessions *narvipg.SessionStore, turns *narvipg.TurnStore, plans *narvipg.PlanStore, channel, threadTS, repo string) (sqlcgen.Session, sqlcgen.Plan, sessionguard.Refusal) {
	t.Helper()
	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceSlack,
		Repos:       []byte(`[{"url": "https://github.com/` + repo + `.git"}]`),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, ok, err := narvipg.NewSlackThreadSessionStore(pool).Claim(ctx, channel, threadTS, session.ID); err != nil || !ok {
		t.Fatalf("map the thread: %v %v", ok, err)
	}
	producing, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("seed the producing turn: %v", err)
	}
	plan, err := plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: session.ID, TurnID: producing.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
	if err != nil {
		t.Fatalf("seed the awaiting plan: %v", err)
	}
	refusal := capSessionAt(ctx, t, pool, session.ID, pgtype.UUID{}, repo)
	return session, plan, refusal
}

// TestSlackApproveButton_AtSpendCap_ClickerToldAndTheThreadNoticed: an
// Approve click on the plan of a session that has spent its cap is
// refused: the plan stays awaiting approval and its message keeps its
// buttons, and the clicker alone is told why, with the refusal's own text.
// That reply reaches no one else, so the crossing's one notice still goes
// to the session's thread, beside its warning.
func TestSlackApproveButton_AtSpendCap_ClickerToldAndTheThreadNoticed(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	rig := newInteractiveTestRig(t, pool)
	session, plan, refusal := slackSessionAtCapWithAwaitingPlan(ctx, t, pool, rig.sessions, rig.turns, rig.plans, planClickChannel, planClickMessageTS, "acme/button-cap")

	clickApprove(t, rig.handler, session, plan, interactivityDefaultUserID)
	updates, ephemerals := takeSlackCalls(rig.requests)
	assertAnsweredPrivately(t, updates, ephemerals, interactivityDefaultUserID, sessionguard.Text(refusal))
	assertPlanStatus(ctx, t, pool, plan, sqlcgen.PlanStatusAwaitingApproval)
	if turns, err := rig.turns.ListForSession(ctx, session.ID); err != nil || len(turns) != 2 {
		t.Fatalf("turns = %d (%v), want the 2 seeded: no implementation turn", len(turns), err)
	}
	if warnings, notices := guardRecords(ctx, t, pool, session.ID, refusal); warnings != 1 || notices != 1 {
		t.Fatalf("warnings %d, notices %d; want the crossing's warning and its one notice to the thread", warnings, notices)
	}
}

// TestSlackTypedApprove_AtSpendCap_HonestReplyInTheThread: "approve" typed
// in the thread of a session that has spent its cap is refused: the plan
// stays awaiting approval, and the thread is answered with the refusal's
// own text -- the crossing's one telling, so no notice repeats it.
func TestSlackTypedApprove_AtSpendCap_HonestReplyInTheThread(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	auditLog := narvipg.NewAuditLogStore(pool)
	recordingServer, recordedBodies := newFakeSlackRecordingWithUsersInfo(t, "unused", "unused@example.com")
	linkSlackIdentityForTest(ctx, t, pool, "U0TESTUSER", sqlcgen.UserRoleMaintainer)
	linkSlackIdentityForTest(ctx, t, pool, "U0OTHERUSER", sqlcgen.UserRoleMaintainer)
	rig := newSlackPlanGateTestRig(t, pool, recordingServer, auditLog)
	const channel, thread = "C0TYPEDCAP", "1700000090.000100"
	session, plan, refusal := slackSessionAtCapWithAwaitingPlan(ctx, t, pool, rig.sessions, rig.turns, rig.plans, channel, thread, "acme/typed-cap")

	rec := httptest.NewRecorder()
	rig.handler(rec, signedSlackRequest(t, messageEnvelope("Ev0TYPEDCAP002", channel, "1700000090.000200", thread, "approve")))
	if rec.Code != http.StatusOK {
		t.Fatalf("reply: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	assertPlanStatus(ctx, t, pool, plan, sqlcgen.PlanStatusAwaitingApproval)
	if turns, err := rig.turns.ListForSession(ctx, session.ID); err != nil || len(turns) != 2 {
		t.Fatalf("turns = %d (%v), want the 2 seeded: no implementation turn", len(turns), err)
	}
	var replies []string
drain:
	for {
		select {
		case got := <-recordedBodies:
			if text, ok := got.body["text"].(string); ok && got.path == "/chat.postMessage" {
				replies = append(replies, text)
			}
		default:
			break drain
		}
	}
	if len(replies) != 1 || replies[0] != sessionguard.Text(refusal) {
		t.Fatalf("thread replies = %q, want the refusal's text alone", replies)
	}
	if warnings, notices := guardRecords(ctx, t, pool, session.ID, refusal); warnings != 1 || notices != 0 {
		t.Fatalf("warnings %d, notices %d; want the crossing's warning, and no notice beside the thread reply", warnings, notices)
	}
}
