//go:build integration

package sessionactor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/platform"
)

// This file covers a deploy transition tokenframe.go documents: the
// control plane before per-frame keys receiving a sandbox's replay after
// this one stored frames (a rollback, or an old pod in a mixed fleet).

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
