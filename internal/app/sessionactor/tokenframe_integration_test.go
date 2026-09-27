//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves tokenframe.go against a real Postgres instance, driving
// every frame through Actor.Send exactly as wshub's read loop does: each
// DISTINCT cumulative frame of a streamed text part is stored and
// broadcast once, so the newest stored frame -- what every reader shows --
// is the part's final text, not its first frame.

const (
	tokenTestPartID   = "prt_plan"
	tokenTestPrefix   = "1. Add the"
	tokenTestFullText = "1. Add the migration\n2. Wire the store\n3. Tests"
)

// tokenFrameRaw marshals a real, schema-valid sandboxws.Token frame the way
// the sandbox-agent's SendBestEffort does.
func tokenFrameRaw(t *testing.T, sessionID, partID, text string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(sandboxws.Token{
		Type:      "token",
		MessageId: partID,
		SessionId: sessionID,
		Gen:       1,
		Text:      text,
	})
	if err != nil {
		t.Fatalf("marshal token frame: %v", err)
	}
	return raw
}

// storedTokenRow is one persisted `token` row, as the assertions below
// need it.
type storedTokenRow struct {
	storageKey   string
	payloadMsgID string
	payloadText  string
	payload      json.RawMessage
}

// listStoredTokenRows returns sessionID's `token` rows, oldest first.
func listStoredTokenRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) []storedTokenRow {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT message_id, payload->>'messageId', payload->>'text', payload FROM events
		 WHERE session_id = $1 AND type = 'token' ORDER BY id`, sessionID)
	if err != nil {
		t.Fatalf("query token rows: %v", err)
	}
	defer rows.Close()
	var out []storedTokenRow
	for rows.Next() {
		var r storedTokenRow
		if err := rows.Scan(&r.storageKey, &r.payloadMsgID, &r.payloadText, &r.payload); err != nil {
			t.Fatalf("scan token row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate token rows: %v", err)
	}
	return out
}

// TestHandleSandboxEvent_TokenFrames_PersistEachDistinctFrame covers the
// frame sequences a text part can arrive as. The first two are what the
// runtime sends; the last three are resends and late replays after a
// reconnect (the sandbox-agent replays its whole outbound buffer).
func TestHandleSandboxEvent_TokenFrames_PersistEachDistinctFrame(t *testing.T) {
	tests := []struct {
		name   string
		frames []string
		// wantStored is every stored row's payload text, oldest first; its
		// last entry is what every reader shows for the part.
		wantStored []string
	}{
		{
			name:       "empty first frame, then the full text (the pinned runtime's cadence)",
			frames:     []string{"", tokenTestFullText},
			wantStored: []string{"", tokenTestFullText},
		},
		{
			name:       "prefix first frame, then the full text",
			frames:     []string{tokenTestPrefix, tokenTestFullText},
			wantStored: []string{tokenTestPrefix, tokenTestFullText},
		},
		{
			name:       "an identical resend dedupes",
			frames:     []string{"", tokenTestFullText, tokenTestFullText, ""},
			wantStored: []string{"", tokenTestFullText},
		},
		{
			name:       "an older frame replayed after a newer one adds no row",
			frames:     []string{"", tokenTestFullText, tokenTestPrefix},
			wantStored: []string{"", tokenTestFullText},
		},
		{
			name:       "an empty frame replayed after the full one adds no row",
			frames:     []string{tokenTestFullText, ""},
			wantStored: []string{tokenTestFullText},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			pool := newTestPool(t)
			sessionID := createTestSession(ctx, t, pool)
			if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			fb := &fakeBroadcaster{}
			r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), fb, nil, nil, "", nil, nil, "", nil, false)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			t.Cleanup(func() { _ = r.Shutdown() })
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}

			for i, text := range tt.frames {
				outcome := sendSandboxEventForTest(ctx, t, a, SandboxEvent{
					Type:      "token",
					Gen:       1,
					MessageID: tokenTestPartID,
					Raw:       tokenFrameRaw(t, sessionID.String(), tokenTestPartID, text),
				})
				if !outcome.Persisted || outcome.AckID != "" {
					t.Fatalf("frame %d: outcome = %+v, want Persisted with no ack (token is non-critical)", i, outcome)
				}
			}

			stored := listStoredTokenRows(ctx, t, pool, sessionID)
			var gotTexts []string
			seenKeys := map[string]bool{}
			for _, row := range stored {
				gotTexts = append(gotTexts, row.payloadText)
				if row.payloadMsgID != tokenTestPartID {
					t.Errorf("stored payload messageId = %q, want %q (the payload is stored verbatim)", row.payloadMsgID, tokenTestPartID)
				}
				if !strings.HasPrefix(row.storageKey, tokenTestPartID+"#") || seenKeys[row.storageKey] {
					t.Errorf("storage key %q: want a distinct %q-prefixed key per frame", row.storageKey, tokenTestPartID+"#")
				}
				seenKeys[row.storageKey] = true
			}
			if strings.Join(gotTexts, "|") != strings.Join(tt.wantStored, "|") {
				t.Fatalf("stored frame texts = %q, want %q", gotTexts, tt.wantStored)
			}

			// Every stored row -- and only a stored row -- went out to live
			// subscribers, once, byte-identical to what the sandbox sent.
			if len(fb.calls) != len(stored) {
				t.Fatalf("broadcasts = %d, want %d (one per stored row)", len(fb.calls), len(stored))
			}
			for i, call := range fb.calls {
				var got, want sandboxws.Token
				if err := json.Unmarshal(call.payload, &got); err != nil {
					t.Fatalf("broadcast %d: unmarshal: %v", i, err)
				}
				if err := json.Unmarshal(stored[i].payload, &want); err != nil {
					t.Fatalf("stored row %d: unmarshal: %v", i, err)
				}
				if got != want {
					t.Errorf("broadcast %d = %+v, want the stored row %+v", i, got, want)
				}
			}
		})
	}
}

// TestCompleteProcessingTurn_PlanApproval_MultiFrameToken_CarriesFinalText
// proves both plan-approval notifications -- chat and issue tracker --
// carry the plan's final text when the plan streamed as more than one
// frame. Before per-frame storage the empty first frame made them the
// "(plan content unavailable ...)" placeholder, and a prefix first frame
// made them that prefix.
func TestCompleteProcessingTurn_PlanApproval_MultiFrameToken_CarriesFinalText(t *testing.T) {
	firstFrames := []struct {
		name  string
		first string
	}{
		{name: "empty first frame", first: ""},
		{name: "prefix first frame", first: tokenTestPrefix},
	}
	origins := []struct {
		name  string
		setup func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID
		text  func(t *testing.T, row sqlcgen.Outbox) string
		want  string
	}{
		{
			name: "chat",
			setup: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
				sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceSlack)
				if _, ok, err := narvipg.NewSlackThreadSessionStore(pool).Claim(ctx, "C123", "1700000000.000100", sessionID); err != nil || !ok {
					t.Fatalf("claim slack thread session: ok=%v err=%v", ok, err)
				}
				return sessionID
			},
			text: func(t *testing.T, row sqlcgen.Outbox) string {
				if row.Kind != "slack_plan_approval" {
					t.Fatalf("Kind = %q, want slack_plan_approval", row.Kind)
				}
				var payload slackapi.PlanApprovalPayload
				if err := json.Unmarshal(row.Payload, &payload); err != nil {
					t.Fatalf("unmarshal slack plan approval payload: %v", err)
				}
				return payload.Text
			},
			want: tokenTestFullText,
		},
		{
			name: "issue tracker",
			setup: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
				sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceLinear)
				agentSessions := narvipg.NewLinearAgentSessionStore(pool)
				if _, err := agentSessions.Claim(ctx, "agent-session-frames", "org-frames"); err != nil {
					t.Fatalf("claim linear agent session: %v", err)
				}
				if err := agentSessions.SetSessionID(ctx, "agent-session-frames", sessionID); err != nil {
					t.Fatalf("set linear agent session id: %v", err)
				}
				return sessionID
			},
			text: func(t *testing.T, row sqlcgen.Outbox) string {
				if row.Kind != string(ports.NotificationKindLinear) {
					t.Fatalf("Kind = %q, want %q", row.Kind, ports.NotificationKindLinear)
				}
				var payload linearapi.Payload
				if err := json.Unmarshal(row.Payload, &payload); err != nil {
					t.Fatalf("unmarshal linear payload: %v", err)
				}
				return payload.Text
			},
			want: planApprovalLinearText(1, tokenTestFullText),
		},
	}

	for _, ff := range firstFrames {
		for _, origin := range origins {
			t.Run(ff.name+"/"+origin.name, func(t *testing.T) {
				ctx := context.Background()
				pool := newTestPool(t)
				sessionID := origin.setup(ctx, t, pool)
				if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
					t.Fatalf("create sandbox: %v", err)
				}
				createProcessingTurnWithPlanMode(ctx, t, narvipg.NewTurnStore(pool), sessionID, true, nil)

				r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
				if err != nil {
					t.Fatalf("NewRegistry: %v", err)
				}
				t.Cleanup(func() { _ = r.Shutdown() })
				a, err := r.GetOrSpawn(ctx, sessionID)
				if err != nil {
					t.Fatalf("GetOrSpawn: %v", err)
				}

				for _, text := range []string{ff.first, tokenTestFullText} {
					sendSandboxEventForTest(ctx, t, a, SandboxEvent{
						Type:      "token",
						Gen:       1,
						MessageID: tokenTestPartID,
						Raw:       tokenFrameRaw(t, sessionID.String(), tokenTestPartID, text),
					})
				}
				sendSandboxEventForTest(ctx, t, a, SandboxEvent{
					Type: "execution_complete",
					Gen:  1,
					Raw:  executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
				})

				if got := origin.text(t, getSoleOutboxRowForSession(ctx, t, pool, sessionID)); got != origin.want {
					t.Errorf("plan approval text = %q, want %q (the plan's last frame)", got, origin.want)
				}
			})
		}
	}
}
