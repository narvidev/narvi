//go:build integration

// This file holds the seed the Slack cut-plan tests share: a plan whose
// final text is a `token` frame the sandbox-agent cut on its way to the
// control plane (technical plan §6.1), injected straight into `events` as
// the session actor stores it, since nothing produces a cut yet.
package slack_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/framecut"
)

// seedCutPlanText places turnID's window at the session's current
// watermark and stores, inside it, a text part whose only text is a cut
// frame: its empty first frame under the bare part id, then the cut one.
// It returns the cut, whose framecut.Reason every surface relays.
func seedCutPlanText(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID, turnID pgtype.UUID) framecut.Cut {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE turns SET dispatched_event_id = (SELECT COALESCE(MAX(id), 0) FROM events WHERE session_id = $1) WHERE id = $2`, sessionID, turnID); err != nil {
		t.Fatalf("place the plan turn's window: %v", err)
	}
	cut := framecut.Cut{Kept: 20, Total: 40960}
	frames := []struct {
		key  string
		text string
		cut  *framecut.Cut
	}{
		{key: "prt_plan", text: ""},
		{key: "prt_plan#cut", text: "1. Add the migration\n[text cut at 20 of 40960 bytes on its way from the sandbox]", cut: &cut},
	}
	for _, f := range frames {
		payload := map[string]any{"type": "token", "messageId": "prt_plan", "sessionId": sessionID.String(), "gen": 1, "text": f.text}
		if f.cut != nil {
			payload["cut"] = f.cut
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal token frame: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO events (session_id, type, payload, message_id) VALUES ($1, 'token', $2, $3)`, sessionID, raw, f.key); err != nil {
			t.Fatalf("store token frame: %v", err)
		}
	}
	return cut
}

// TestHandler_ReplyOnMappedThread_AwaitingCutPlan_OrdinaryText_OffersNoApprove
// proves the reply to a thread message that neither decides nor revises a
// plan awaiting approval reads that plan's cut report (technical plan
// §6.1): for a plan whose text was cut on its way from the sandbox, the
// reply gives the reason and names no approve keyword and no Approve
// button, since the approval would be refused; for a whole plan it offers
// them as before. Either way the reply starts nothing and decides nothing.
func TestHandler_ReplyOnMappedThread_AwaitingCutPlan_OrdinaryText_OffersNoApprove(t *testing.T) {
	tests := []struct {
		name    string
		channel string
		cut     bool
	}{
		{name: "a cut plan", channel: "C0AWAITCUT", cut: true},
		{name: "a whole plan", channel: "C0AWAITWHOLE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			auditLog := narvipg.NewAuditLogStore(pool)
			recordingServer, recordedBodies := newFakeSlackRecordingWithUsersInfo(t, "unused", "unused@example.com")
			linkSlackIdentityForTest(ctx, t, pool, "U0TESTUSER", sqlcgen.UserRoleMaintainer)
			linkSlackIdentityForTest(ctx, t, pool, "U0OTHERUSER", sqlcgen.UserRoleMaintainer)
			rig := newSlackPlanGateTestRig(t, pool, recordingServer, auditLog)

			rec := httptest.NewRecorder()
			rig.handler(rec, signedSlackRequest(t, appMentionEnvelope("Ev0"+tt.channel+"001", tt.channel, "1700000080.000100", "", "start this task")))
			if rec.Code != http.StatusOK {
				t.Fatalf("first mention: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			mapping, err := rig.threads.Get(ctx, tt.channel, "1700000080.000100")
			if err != nil {
				t.Fatalf("Get thread mapping: %v", err)
			}
			sessionID := mapping.SessionID
			firstTurns, err := rig.turns.ListForSession(ctx, sessionID)
			if err != nil || len(firstTurns) != 1 {
				t.Fatalf("ListForSession after first mention: turns=%v err=%v, want exactly 1", firstTurns, err)
			}
			if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: firstTurns[0].ID, Status: sqlcgen.TurnStatusCompleted, CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}); err != nil {
				t.Fatalf("UpdateStatus: %v", err)
			}
			var cut framecut.Cut
			if tt.cut {
				cut = seedCutPlanText(ctx, t, pool, sessionID, firstTurns[0].ID)
			} else {
				seedWholePlanText(ctx, t, pool, sessionID, firstTurns[0].ID)
			}
			plan := seedAwaitingApprovalPlanForSlack(ctx, t, rig.plans, sessionID, firstTurns[0].ID)

			rec = httptest.NewRecorder()
			rig.handler(rec, signedSlackRequest(t, messageEnvelope("Ev0"+tt.channel+"002", tt.channel, "1700000080.000200", "1700000080.000100", "looks good, go ahead")))
			if rec.Code != http.StatusOK {
				t.Fatalf("reply: status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}

			if turns, err := rig.turns.ListForSession(ctx, sessionID); err != nil || len(turns) != 1 {
				t.Errorf("turns after the reply = %d (err %v), want 1", len(turns), err)
			}
			var dbStatus sqlcgen.PlanStatus
			if err := pool.QueryRow(ctx, `SELECT status FROM plans WHERE id = $1`, plan.ID).Scan(&dbStatus); err != nil {
				t.Fatalf("query plan row: %v", err)
			}
			if dbStatus != sqlcgen.PlanStatusAwaitingApproval {
				t.Errorf("db status = %q, want %q", dbStatus, sqlcgen.PlanStatusAwaitingApproval)
			}

			var reply string
		drain:
			for {
				select {
				case got := <-recordedBodies:
					if text, ok := got.body["text"].(string); ok && got.path == "/chat.postMessage" && strings.Contains(text, "A plan is awaiting") {
						reply = text
					}
				default:
					break drain
				}
			}
			if reply == "" {
				t.Fatal("no awaiting-plan reply was posted")
			}
			offersApprove := false
			for _, w := range strings.FieldsFunc(reply, func(r rune) bool { return !unicode.IsLetter(r) }) {
				offersApprove = offersApprove || w == "approve" || w == "approved" || w == "lgtm" || w == "Approve"
			}
			if offersApprove == tt.cut {
				t.Errorf("reply %q offers Approve: %v, want %v", reply, offersApprove, !tt.cut)
			}
			if tt.cut && !strings.Contains(reply, framecut.Reason(&cut)) {
				t.Errorf("reply %q, want the reason %q", reply, framecut.Reason(&cut))
			}
		})
	}
}

// seedWholePlanText is seedCutPlanText for a plan whose text is whole.
func seedWholePlanText(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID, turnID pgtype.UUID) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE turns SET dispatched_event_id = (SELECT COALESCE(MAX(id), 0) FROM events WHERE session_id = $1) WHERE id = $2`, sessionID, turnID); err != nil {
		t.Fatalf("place the plan turn's window: %v", err)
	}
	raw, err := json.Marshal(map[string]any{"type": "token", "messageId": "prt_plan", "sessionId": sessionID.String(), "gen": 1, "text": "1. Add the migration\n2. Wire the store\n"})
	if err != nil {
		t.Fatalf("marshal token frame: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO events (session_id, type, payload, message_id) VALUES ($1, 'token', $2, 'prt_plan')`, sessionID, raw); err != nil {
		t.Fatalf("store token frame: %v", err)
	}
}
