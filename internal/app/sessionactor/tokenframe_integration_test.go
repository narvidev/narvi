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
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves tokenframe.go against a real Postgres instance, driving
// every frame through Actor.Send exactly as wshub's read loop does: each
// DISTINCT cumulative frame of a streamed text part is stored and
// broadcast once while its turn is Processing, so the newest stored frame
// -- what every reader shows -- is the part's final text, not its first
// frame; and a frame that arrives after its turn has ended (the sandbox
// replays its outbound buffer on every reconnect) adds no row anywhere.

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

// createDispatchedProcessingTurn seeds a Processing turn stamped the way
// dispatch stamps one (tryPlanDispatch, dispatch.go): its
// dispatched_event_id is the session's events high-water mark at that
// moment, so every event the turn goes on to produce lies above it. A turn
// seeded with no stamp would place no window at all (tokenframe.go).
func createDispatchedProcessingTurn(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) sqlcgen.Turn {
	t.Helper()
	turns := narvipg.NewTurnStore(pool)
	created, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusProcessing})
	if err != nil {
		t.Fatalf("create processing turn: %v", err)
	}
	watermark, err := narvipg.NewEventStore(pool).MaxEventIDForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("read events high-water mark: %v", err)
	}
	gen := int32(1)
	stamped, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:                   created.ID,
		Status:               sqlcgen.TurnStatusProcessing,
		DispatchedSandboxGen: &gen,
		DispatchedEventID:    &watermark,
	})
	if err != nil {
		t.Fatalf("stamp dispatched_event_id: %v", err)
	}
	return stamped
}

// sendTokenFrame drives one `token` frame of partID through Actor.Send.
func sendTokenFrame(ctx context.Context, t *testing.T, a *Actor, sessionID pgtype.UUID, partID, text string) SandboxEventOutcome {
	t.Helper()
	return sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type:      "token",
		Gen:       1,
		MessageID: partID,
		Raw:       tokenFrameRaw(t, sessionID.String(), partID, text),
	})
}

// sendExecutionComplete drives a completed execution_complete through
// Actor.Send under its own wire messageId, as wshub's read loop does, so
// two turns' execution_complete events never share a storage key.
func sendExecutionComplete(ctx context.Context, t *testing.T, a *Actor, sessionID pgtype.UUID) {
	t.Helper()
	raw := executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted)
	var wire struct {
		MessageID string `json:"messageId"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode execution_complete: %v", err)
	}
	sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "execution_complete", Gen: 1, MessageID: wire.MessageID, Raw: raw})
}

// eventRow is one persisted event of any type, as the turn-window
// assertions below need it.
type eventRow struct {
	id           int64
	eventType    string
	storageKey   string
	payloadMsgID string
	payloadText  string
}

// listEventRows returns every event row of sessionID, oldest first.
func listEventRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) []eventRow {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT id, type, message_id, COALESCE(payload->>'messageId', ''), COALESCE(payload->>'text', '')
		 FROM events WHERE session_id = $1 ORDER BY id`, sessionID)
	if err != nil {
		t.Fatalf("query event rows: %v", err)
	}
	defer rows.Close()
	var out []eventRow
	for rows.Next() {
		var r eventRow
		if err := rows.Scan(&r.id, &r.eventType, &r.storageKey, &r.payloadMsgID, &r.payloadText); err != nil {
			t.Fatalf("scan event row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate event rows: %v", err)
	}
	return out
}

// stepStartRaw marshals a schema-valid sandboxws.StepStart frame.
func stepStartRaw(t *testing.T, sessionID, stepID string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(sandboxws.StepStart{
		Type: "step_start", MessageId: "msg_" + stepID, SessionId: sessionID, Gen: 1, StepId: stepID,
	})
	if err != nil {
		t.Fatalf("marshal step_start: %v", err)
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

			// Every frame of a turn arrives while it Processes (tokenframe.go).
			createDispatchedProcessingTurn(ctx, t, pool, sessionID)

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

// TestHandleSandboxEvent_TokenFrames_LateFramesOfAnEndedTurnAddNoRow is the
// deploy-time replay, end to end through Actor.Send. Before per-frame keys
// the store kept only a part's first frame (here the empty one, under the
// bare part id, exactly as the first-wins insert wrote it) and swallowed
// the full text; the sandbox still holds every frame in its outbound
// buffer and replays them all on the first reconnect after the deploy,
// long after the turn's execution_complete. Stored there, the full text
// would land at the tail of the log: the web timeline opens a turn at any
// token that follows an execution_complete, so it would show a new,
// still-running turn (and disable the composer), and a later turn's
// window would read it as that turn's text. The replay must add no row
// and broadcast nothing, whether or not a later turn is Processing when it
// arrives; a later turn's own frames are unaffected.
func TestHandleSandboxEvent_TokenFrames_LateFramesOfAnEndedTurnAddNoRow(t *testing.T) {
	const (
		legacyPart = "prt_legacy" // stored before per-frame keys: first frame only
		fixedPart  = "prt_fixed"  // stored under per-frame keys while its turn ran
		fixedFinal = "Turn one closing note."
		nextPart   = "prt_next"
		nextFinal  = "Turn two answer."
	)
	cadences := []struct {
		name   string
		frames []string
	}{
		{name: "the pinned runtime's cadence", frames: []string{"", "Turn one final answer."}},
		{name: "a longer cadence", frames: []string{"", "Turn one", "Turn one final", "Turn one final answer."}},
	}
	for _, cadence := range cadences {
		for _, nextTurnProcessing := range []bool{false, true} {
			name := cadence.name + "/no turn Processing during the replay"
			if nextTurnProcessing {
				name = cadence.name + "/the next turn Processing during the replay"
			}
			t.Run(name, func(t *testing.T) {
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

				// Turn one, as history stored it.
				createDispatchedProcessingTurn(ctx, t, pool, sessionID)
				sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "step_start", Gen: 1, MessageID: "msg_s1", Raw: stepStartRaw(t, sessionID.String(), "s1")})
				if _, err := narvipg.NewEventStore(pool).Create(ctx, sqlcgen.CreateEventParams{
					SessionID: sessionID,
					Type:      "token",
					MessageID: legacyPart,
					Payload:   tokenFrameRaw(t, sessionID.String(), legacyPart, cadence.frames[0]),
				}); err != nil {
					t.Fatalf("seed the pre-fix first frame: %v", err)
				}
				sendTokenFrame(ctx, t, a, sessionID, fixedPart, "")
				sendTokenFrame(ctx, t, a, sessionID, fixedPart, fixedFinal)
				sendExecutionComplete(ctx, t, a, sessionID)
				if _, err := narvipg.NewTurnStore(pool).GetProcessingTurnForSession(ctx, sessionID); err == nil {
					t.Fatal("turn one still Processing after its execution_complete")
				}

				var nextTurn sqlcgen.Turn
				if nextTurnProcessing {
					nextTurn = createDispatchedProcessingTurn(ctx, t, pool, sessionID)
				}
				before := listEventRows(ctx, t, pool, sessionID)
				broadcastsBefore := len(fb.calls)

				// The reconnect replay: the sandbox resends its whole buffer.
				sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "step_start", Gen: 1, MessageID: "msg_s1", Raw: stepStartRaw(t, sessionID.String(), "s1")})
				for _, text := range cadence.frames {
					if outcome := sendTokenFrame(ctx, t, a, sessionID, legacyPart, text); !outcome.Persisted {
						t.Fatalf("replayed frame %q: outcome = %+v, want Persisted (handled, gen current)", text, outcome)
					}
				}
				sendTokenFrame(ctx, t, a, sessionID, fixedPart, "")
				sendTokenFrame(ctx, t, a, sessionID, fixedPart, fixedFinal)

				after := listEventRows(ctx, t, pool, sessionID)
				if len(after) != len(before) {
					t.Fatalf("the replay added %d rows %+v, want none", len(after)-len(before), after[len(before):])
				}
				if got := len(fb.calls) - broadcastsBefore; got != 0 {
					t.Fatalf("the replay broadcast %d frames, want none", got)
				}
				var legacyTexts []string
				for _, row := range after {
					if row.eventType == "token" && row.payloadMsgID == legacyPart {
						legacyTexts = append(legacyTexts, row.payloadText)
					}
				}
				if len(legacyTexts) != 1 || legacyTexts[0] != cadence.frames[0] {
					t.Errorf("pre-fix part's stored frames = %q, want only its first frame %q (history stays truncated)", legacyTexts, cadence.frames[0])
				}

				if !nextTurnProcessing {
					return
				}
				// The next turn's own part is stored and broadcast as usual, and
				// its window reads its own text only.
				sendTokenFrame(ctx, t, a, sessionID, nextPart, "")
				sendTokenFrame(ctx, t, a, sessionID, nextPart, nextFinal)
				var inWindow []string
				for _, row := range listEventRows(ctx, t, pool, sessionID) {
					if row.id > *nextTurn.DispatchedEventID && row.eventType == "token" {
						inWindow = append(inWindow, row.payloadMsgID+"="+row.payloadText)
					}
				}
				if want := []string{nextPart + "=", nextPart + "=" + nextFinal}; strings.Join(inWindow, "|") != strings.Join(want, "|") {
					t.Errorf("token rows in the next turn's window = %q, want %q", inWindow, want)
				}
				if got := len(fb.calls) - broadcastsBefore; got != 2 {
					t.Errorf("broadcasts after the replay = %d, want 2 (the next turn's own frames)", got)
				}
				recent, err := narvipg.NewEventStore(pool).ListRecentForSession(ctx, sessionID, 1000)
				if err != nil {
					t.Fatalf("list recent events: %v", err)
				}
				if got := plandomain.ExtractContent(ToContentEvents(recent), nextTurn.DispatchedEventID, nil); got != nextFinal {
					t.Errorf("the next turn's window reads %q, want %q", got, nextFinal)
				}
			})
		}
	}
}

// TestHandleSandboxEvent_TokenFrames_FinalFrameStoredWhileTurnProcesses is
// the live path the turn-window rule must leave alone: the runtime sends a
// part's full text before the turn's execution_complete, so it is stored
// and broadcast, it is the part's newest row, and nothing of the part is
// stored after the turn ends.
func TestHandleSandboxEvent_TokenFrames_FinalFrameStoredWhileTurnProcesses(t *testing.T) {
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

	// A turn earlier in the session, so this one's window starts above a
	// real watermark rather than at an empty log.
	createDispatchedProcessingTurn(ctx, t, pool, sessionID)
	sendTokenFrame(ctx, t, a, sessionID, "prt_earlier", "")
	sendTokenFrame(ctx, t, a, sessionID, "prt_earlier", "Earlier turn.")
	sendExecutionComplete(ctx, t, a, sessionID)

	current := createDispatchedProcessingTurn(ctx, t, pool, sessionID)
	broadcastsBefore := len(fb.calls)
	sendTokenFrame(ctx, t, a, sessionID, tokenTestPartID, "")
	sendTokenFrame(ctx, t, a, sessionID, tokenTestPartID, tokenTestFullText)
	sendExecutionComplete(ctx, t, a, sessionID)

	rows := listEventRows(ctx, t, pool, sessionID)
	var partTexts []string
	var completeID, lastPartID int64
	for _, row := range rows {
		if row.id <= *current.DispatchedEventID {
			continue
		}
		switch {
		case row.eventType == "token" && row.payloadMsgID == tokenTestPartID:
			partTexts = append(partTexts, row.payloadText)
			lastPartID = row.id
		case row.eventType == "execution_complete":
			completeID = row.id
		}
	}
	if want := []string{"", tokenTestFullText}; strings.Join(partTexts, "|") != strings.Join(want, "|") {
		t.Fatalf("part's frames stored in the turn's window = %q, want %q", partTexts, want)
	}
	if lastPartID >= completeID {
		t.Errorf("part's newest frame id %d is not before the turn's execution_complete id %d", lastPartID, completeID)
	}
	// The two frames, then the execution_complete that ended the turn.
	if got := len(fb.calls) - broadcastsBefore; got != 3 {
		t.Errorf("broadcasts for the turn = %d, want 3 (two frames and the execution_complete)", got)
	}
}
