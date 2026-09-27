//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
	"github.com/narvidev/narvi/internal/platform"
)

// This file covers the two deploy transitions tokenframe.go documents:
// the control plane before per-frame keys receiving a sandbox's replay
// after this one stored frames (a rollback, or an old pod in a mixed
// fleet), and this one receiving the replay for the turn still running
// when it was deployed.

// previousBinary stores sandbox events exactly as the control plane built
// before per-frame `token` keys did, for every event type, `token`
// included: handleSandboxEvent's appendRawEvent(ctx, tx, cmd.Type,
// cmd.MessageID, cmd.Raw) inside transact -- CreateEvent's first-wins
// insert on the bare wire messageId, with no turn check, and a broadcast
// only for an inserted row. Neither function changed since that binary,
// so this is its storage path, not a copy of it. It runs on an Actor value
// of its own that no mailbox drives, fenced on the session's actor_epoch
// as it stands when it is built.
type previousBinary struct {
	actor *Actor
	fb    *fakeBroadcaster
}

func newPreviousBinary(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) *previousBinary {
	t.Helper()
	var epoch int64
	if err := pool.QueryRow(ctx, `SELECT actor_epoch FROM sessions WHERE id = $1`, sessionID).Scan(&epoch); err != nil {
		t.Fatalf("read the session's actor_epoch: %v", err)
	}
	fb := &fakeBroadcaster{}
	return &previousBinary{
		actor: &Actor{sessionID: sessionID, epoch: epoch, pool: pool, stores: newStoreBundle(pool, false), broadcaster: fb},
		fb:    fb,
	}
}

// store runs cmd through the previous binary's storage path and reports
// whether it inserted a row.
func (p *previousBinary) store(ctx context.Context, t *testing.T, cmd SandboxEvent) bool {
	t.Helper()
	var inserted bool
	if err := p.actor.transact(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		inserted, err = p.actor.appendRawEvent(ctx, tx, cmd.Type, cmd.MessageID, cmd.Raw)
		return err
	}); err != nil {
		t.Fatalf("previous binary: store %s %q: %v", cmd.Type, cmd.MessageID, err)
	}
	return inserted
}

// tokenEvent is one `token` frame of partID as wshub hands it to the actor.
func tokenEvent(t *testing.T, sessionID pgtype.UUID, partID, text string) SandboxEvent {
	t.Helper()
	return SandboxEvent{Type: "token", Gen: 1, MessageID: partID, Raw: tokenFrameRaw(t, sessionID.String(), partID, text)}
}

// stepStartEvent is step stepID's step_start, under the step's message id.
func stepStartEvent(t *testing.T, sessionID pgtype.UUID, stepID string) SandboxEvent {
	t.Helper()
	return SandboxEvent{Type: "step_start", Gen: 1, MessageID: "msg_" + stepID, Raw: stepStartRaw(t, sessionID.String(), stepID)}
}

// stepFinishEvent is step stepID's step_finish in the production shape:
// under the same message id as its step_start, so it adds no row.
func stepFinishEvent(t *testing.T, sessionID pgtype.UUID, stepID string) SandboxEvent {
	t.Helper()
	raw, err := json.Marshal(sandboxws.StepFinish{
		Type: "step_finish", MessageId: "msg_" + stepID, SessionId: sessionID.String(), Gen: 1, StepId: stepID,
		Cost: sandboxws.StepFinishCost{Tokens: sandboxws.StepFinishCostTokens{Input: 10, Output: 5}},
	})
	if err != nil {
		t.Fatalf("marshal step_finish: %v", err)
	}
	return SandboxEvent{Type: "step_finish", Gen: 1, MessageID: "msg_" + stepID, Raw: raw}
}

// TestHandleSandboxEvent_TokenFrames_PreviousBinaryReplayAddsNoRow is the
// compatibility contract of tokenframe.go's storage keys. This binary
// stores several parts across several turns, one turn still Processing,
// and is then replaced by the control plane before per-frame keys -- a
// rollback, or an old pod taking the session over mid-rollout -- to which
// the sandbox reconnects and replays its whole buffer. That binary dedupes
// a `token` only on its bare wire messageId, first wins; every frame of
// every part must hit a stored row there, adding no row and broadcasting
// nothing. Had this binary hashed a part's first frame too, the replay
// would append that frame after the turn's execution_complete: a turn of
// its own that never ends.
func TestHandleSandboxEvent_TokenFrames_PreviousBinaryReplayAddsNoRow(t *testing.T) {
	type part struct {
		id     string
		frames []string
	}
	turns := [][]part{
		{
			{id: "prt_t1_a", frames: []string{"", "Turn one answer."}},
			{id: "prt_t1_b", frames: []string{"Turn one", "Turn one, second part."}},
		},
		{
			{id: "prt_t2_a", frames: []string{"", "Turn two", "Turn two, longer", "Turn two, longer answer."}},
			{id: "prt_t2_b", frames: []string{"Turn two, a single frame."}},
		},
		{
			// Still Processing when the binary is replaced.
			{id: "prt_t3_a", frames: []string{"", "Turn three so far."}},
		},
	}

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

	// This binary: every distinct frame of every part is stored, its
	// first frame under the bare part id, each later one under its own key.
	wantRows := 0
	for i, parts := range turns {
		createDispatchedProcessingTurn(ctx, t, pool, sessionID)
		sendSandboxEventForTest(ctx, t, a, stepStartEvent(t, sessionID, fmt.Sprintf("s%d", i+1)))
		for _, p := range parts {
			for _, text := range p.frames {
				sendSandboxEventForTest(ctx, t, a, tokenEvent(t, sessionID, p.id, text))
				wantRows++
			}
		}
		if i < len(turns)-1 {
			sendExecutionComplete(ctx, t, a, sessionID)
		}
	}
	stored := listStoredTokenRows(ctx, t, pool, sessionID)
	if len(stored) != wantRows {
		t.Fatalf("this binary stored %d token rows, want %d (one per distinct frame)", len(stored), wantRows)
	}
	seenPart := map[string]bool{}
	for _, row := range stored {
		if !seenPart[row.payloadMsgID] {
			seenPart[row.payloadMsgID] = true
			if row.storageKey != row.payloadMsgID {
				t.Errorf("first stored frame of %s keyed %q, want the bare part id", row.payloadMsgID, row.storageKey)
			}
			continue
		}
		if !strings.HasPrefix(row.storageKey, row.payloadMsgID+"#") {
			t.Errorf("later frame of %s keyed %q, want a %q-prefixed per-frame key", row.payloadMsgID, row.storageKey, row.payloadMsgID+"#")
		}
	}

	// The binary is replaced. The sandbox reconnects to the previous one
	// and replays every frame it holds, in the order it sent them.
	if err := r.Shutdown(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("shut down this binary's registry: %v", err)
	}
	broadcastsBefore := len(fb.calls)
	before := listEventRows(ctx, t, pool, sessionID)
	prev := newPreviousBinary(ctx, t, pool, sessionID)
	for _, parts := range turns {
		for _, p := range parts {
			for _, text := range p.frames {
				if prev.store(ctx, t, tokenEvent(t, sessionID, p.id, text)) {
					t.Errorf("the previous binary inserted the replayed frame %q of %s", text, p.id)
				}
			}
		}
	}
	after := listEventRows(ctx, t, pool, sessionID)
	if added := len(after) - len(before); added != 0 {
		t.Errorf("the replay added %d rows %+v, want none", added, after[len(before):])
	}
	if got := len(prev.fb.calls); got != 0 {
		t.Errorf("the previous binary broadcast %d replayed frames, want none", got)
	}
	if got := len(fb.calls) - broadcastsBefore; got != 0 {
		t.Errorf("this binary broadcast %d frames after it was shut down, want none", got)
	}
}

// replayDuringRunningTurnFixture is the log
// TestHandleSandboxEvent_TokenFrames_ReplayDuringTheTurnRunningAtDeploy
// leaves, as the web client receives it; the web timeline test reads the
// same file (web/src/session/__tests__/timelineModel.test.ts), so the rows
// the actor stores are the rows the reader is pinned against. Set
// NARVI_UPDATE_FIXTURES=1 to rewrite it from a run.
const replayDuringRunningTurnFixture = "../../../web/src/session/__tests__/fixtures/tokenReplayDuringRunningTurn.json"

// fixtureSessionID stands in for the test session's random id in the
// fixture.
const fixtureSessionID = "00000000-0000-4000-8000-000000000001"

// timelineFixtureEvent is one row as the web client's EventEnvelope
// carries it (web/src/ws/types.ts).
type timelineFixtureEvent struct {
	ID        int            `json:"id"`
	Type      string         `json:"type"`
	Payload   map[string]any `json:"payload"`
	CreatedAt string         `json:"createdAt"`
}

// sessionLogAsFixture returns sessionID's log as the web client receives
// it, with what varies between runs pinned: ids renumbered from 1 in
// order, the session id replaced, and one fixed createdAt.
func sessionLogAsFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) []timelineFixtureEvent {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT type, payload FROM events WHERE session_id = $1 ORDER BY id`, sessionID)
	if err != nil {
		t.Fatalf("query the session's log: %v", err)
	}
	defer rows.Close()
	var out []timelineFixtureEvent
	for rows.Next() {
		var (
			eventType string
			raw       []byte
		)
		if err := rows.Scan(&eventType, &raw); err != nil {
			t.Fatalf("scan event row: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode %s payload: %v", eventType, err)
		}
		if _, ok := payload["sessionId"]; ok {
			payload["sessionId"] = fixtureSessionID
		}
		out = append(out, timelineFixtureEvent{ID: len(out) + 1, Type: eventType, Payload: payload, CreatedAt: "2026-09-27T10:00:00Z"})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate event rows: %v", err)
	}
	return out
}

// TestHandleSandboxEvent_TokenFrames_ReplayDuringTheTurnRunningAtDeploy is
// the turn still Processing when the control plane restarts onto this
// binary. The previous binary stored its history: step s1 streamed prt_a
// in full, whose full frame first-wins swallowed, and step s2 opened
// prt_b. The sandbox then reconnects and replays its buffer while the
// turn still runs. prt_a's full frame belongs to the live turn, so it is
// stored, at the tail of the turn's window, after step s2 opened; nothing
// else in the replay adds a row. The turn then goes on to finish prt_b and
// complete. The stored log is pinned byte for byte against the fixture the
// web timeline test reads, which shows each part in its own step.
func TestHandleSandboxEvent_TokenFrames_ReplayDuringTheTurnRunningAtDeploy(t *testing.T) {
	const (
		stepOneText = "Step one narration."
		stepTwoText = "Step two answer."
	)
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turn := createDispatchedProcessingTurn(ctx, t, pool, sessionID)

	// What the sandbox sent before the deploy, and still holds.
	buffer := []SandboxEvent{
		stepStartEvent(t, sessionID, "s1"),
		tokenEvent(t, sessionID, "prt_a", ""),
		tokenEvent(t, sessionID, "prt_a", stepOneText),
		stepFinishEvent(t, sessionID, "s1"),
		stepStartEvent(t, sessionID, "s2"),
		tokenEvent(t, sessionID, "prt_b", ""),
	}
	prev := newPreviousBinary(ctx, t, pool, sessionID)
	for _, cmd := range buffer {
		prev.store(ctx, t, cmd)
	}

	// The deploy: this binary takes the session over, and the sandbox
	// reconnects and replays that buffer while the turn is still running.
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
	before := listEventRows(ctx, t, pool, sessionID)
	for _, cmd := range buffer {
		if outcome := sendSandboxEventForTest(ctx, t, a, cmd); !outcome.Persisted {
			t.Fatalf("replayed %s: outcome = %+v, want Persisted", cmd.Type, outcome)
		}
	}
	after := listEventRows(ctx, t, pool, sessionID)
	if len(after) != len(before)+1 {
		t.Fatalf("the replay added %d rows %+v, want 1 (prt_a's full frame)", len(after)-len(before), after[len(before):])
	}
	recovered := after[len(after)-1]
	if recovered.eventType != "token" || recovered.payloadMsgID != "prt_a" || recovered.payloadText != stepOneText ||
		!strings.HasPrefix(recovered.storageKey, "prt_a#") {
		t.Errorf("the replay stored %+v, want prt_a's full frame under a per-frame key", recovered)
	}
	if recovered.id <= *turn.DispatchedEventID {
		t.Errorf("recovered frame id %d is not in the turn's window (dispatched_event_id %d)", recovered.id, *turn.DispatchedEventID)
	}
	if len(fb.calls) != 1 {
		t.Errorf("the replay broadcast %d frames, want 1 (prt_a's full frame)", len(fb.calls))
	}

	// The turn goes on.
	sendSandboxEventForTest(ctx, t, a, tokenEvent(t, sessionID, "prt_b", stepTwoText))
	sendSandboxEventForTest(ctx, t, a, stepFinishEvent(t, sessionID, "s2"))
	completeRaw, err := json.Marshal(sandboxws.ExecutionComplete{
		Type: "execution_complete", MessageId: "msg_done", SessionId: sessionID.String(), Gen: 1,
		AckId: "execution_complete:msg_done", Outcome: sandboxws.ExecutionCompleteOutcomeCompleted,
	})
	if err != nil {
		t.Fatalf("marshal execution_complete: %v", err)
	}
	sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "execution_complete", Gen: 1, MessageID: "msg_done", Raw: completeRaw})

	var gotLog []string
	for _, row := range listEventRows(ctx, t, pool, sessionID) {
		gotLog = append(gotLog, row.eventType+":"+row.payloadMsgID+"="+row.payloadText)
	}
	wantLog := []string{
		"step_start:msg_s1=",
		"token:prt_a=",
		"step_start:msg_s2=",
		"token:prt_b=",
		"token:prt_a=" + stepOneText,
		"token:prt_b=" + stepTwoText,
		"execution_complete:msg_done=",
	}
	if strings.Join(gotLog, "|") != strings.Join(wantLog, "|") {
		t.Fatalf("stored log = %q, want %q", gotLog, wantLog)
	}
	recent, err := narvipg.NewEventStore(pool).ListRecentForSession(ctx, sessionID, 1000)
	if err != nil {
		t.Fatalf("list recent events: %v", err)
	}
	if got := plandomain.ExtractContent(ToContentEvents(recent), turn.DispatchedEventID, nil); got != stepTwoText {
		t.Errorf("the turn's window reads %q, want %q (its last part)", got, stepTwoText)
	}

	got := sessionLogAsFixture(ctx, t, pool, sessionID)
	gotJSON, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal the log: %v", err)
	}
	gotJSON = append(gotJSON, '\n')
	path := filepath.FromSlash(replayDuringRunningTurnFixture)
	if os.Getenv("NARVI_UPDATE_FIXTURES") == "1" {
		if err := os.WriteFile(path, gotJSON, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	wantJSON, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (NARVI_UPDATE_FIXTURES=1 writes it): %v", path, err)
	}
	var gotAny, wantAny any
	if err := json.Unmarshal(gotJSON, &gotAny); err != nil {
		t.Fatalf("decode the log: %v", err)
	}
	if err := json.Unmarshal(wantJSON, &wantAny); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if !reflect.DeepEqual(gotAny, wantAny) {
		t.Errorf("stored log differs from %s, which the web timeline test reads; got:\n%s", path, gotJSON)
	}
}
