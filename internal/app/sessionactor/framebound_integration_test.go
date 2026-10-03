//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// This file is the exit of technical plan §3.3's per-gen prompt bound
// (framebound.go) on the real actor and real Postgres: the bound a dispatch
// measures against is read from the gen's latest ready, whichever binary
// recorded it, and from no other gen's.

// previousBinaryRecordSandboxReady is RecordSandboxReady as a control plane
// built before migration 000157 sends it: it counts the ready and records
// promptReceipt, and names neither column 000157 added.
const previousBinaryRecordSandboxReady = `UPDATE sandboxes
SET ready_seq = ready_seq + 1,
    prompt_receipt_gen = CASE WHEN $1::boolean THEN gen ELSE NULL END,
    updated_at = now()
WHERE session_id = $2 AND gen = $3::integer`

// frameBoundReady is a ready of gen as an agent sends it, its capabilities
// object's members given raw ("" for none: an agent built before 226).
func frameBoundReady(gen int, capabilities string) SandboxEvent {
	id := "ready-" + uuid.NewString()
	caps := ""
	if capabilities != "" {
		caps = `,"capabilities":{` + capabilities + `}`
	}
	agentVersion, imageDigest := "dev", "unknown"
	return SandboxEvent{Type: "ready", Gen: gen, MessageID: id, AgentVersion: &agentVersion, ImageDigest: &imageDigest,
		Raw: json.RawMessage(fmt.Sprintf(`{"type":"ready","messageId":%q,"sessionId":"s","gen":%d,"timestamp":"2026-10-01T12:00:00Z","agentVersion":"dev","imageDigest":"unknown"%s}`, id, gen, caps))}
}

// frameBoundStep is one thing that happens to the session's sandbox before
// the dispatch: a ready this release records (event), a ready a replica
// built before 000157 records (previousBinaryReady, with its promptReceipt),
// or a respawn to the next gen, Ready.
type frameBoundStep struct {
	event               *SandboxEvent
	previousBinaryReady *bool
	respawn             bool
}

func readyByThisRelease(e SandboxEvent) frameBoundStep { return frameBoundStep{event: &e} }

func readyByPreviousBinary(promptReceipt bool) frameBoundStep {
	return frameBoundStep{previousBinaryReady: &promptReceipt}
}

// TestFrameBound_LatestReadyPerGen_ReadAtDispatch: a turn is dispatched
// after its sandbox's readies have been recorded, and its prompt frame is
// measured against the bound the live gen's latest ready gives: written
// when it fits, and otherwise refused at dispatch -- nothing written, the
// turn failed with both sizes named and its synthetic execution_complete
// marked "delivered": false.
func TestFrameBound_LatestReadyPerGen_ReadAtDispatch(t *testing.T) {
	const (
		over32KiB        = 40 * 1024 // a text of plain bytes: its frame is ~40.4 KiB
		statedSmall      = 36 * 1024
		statedOverTheMax = 64 << 20
	)
	// Each '<' encodes as the six bytes \u003c, so this text's frame is over
	// MaxPromptFrameBytes and under statedOverTheMax -- by more than the
	// rounding of the banner's one decimal, so it names both in MiB.
	overTheMax := strings.Repeat("<", platform.MaxPromptFrameBytes/6+64*1024)

	for _, tc := range []struct {
		name      string
		steps     []frameBoundStep
		prompt    string
		dispatch  int // the gen whose heartbeat triggers the dispatch
		wantBound int // 0: the prompt is written
	}{
		{
			name:     "a 226 agent's ready, recorded by this release",
			steps:    []frameBoundStep{readyByThisRelease(frameBoundReady(1, `"promptReceipt":true`))},
			prompt:   strings.Repeat("a", over32KiB),
			dispatch: 1,
		},
		{
			name:     "the same ready, recorded by a replica built before the column",
			steps:    []frameBoundStep{readyByPreviousBinary(true)},
			prompt:   strings.Repeat("a", over32KiB),
			dispatch: 1,
		},
		{
			name: "a later ready of the gen without promptReceipt",
			steps: []frameBoundStep{
				readyByThisRelease(frameBoundReady(1, `"promptReceipt":true`)),
				readyByThisRelease(frameBoundReady(1, "")),
			},
			prompt:    strings.Repeat("a", over32KiB),
			dispatch:  1,
			wantBound: platform.DefaultFrameReadLimitBytes,
		},
		{
			name: "a later ready of the gen that states no limit",
			steps: []frameBoundStep{
				readyByThisRelease(frameBoundReady(1, fmt.Sprintf(`"maxFrameBytes":%d`, platform.MaxPromptFrameBytes))),
				readyByThisRelease(frameBoundReady(1, "")),
			},
			prompt:    strings.Repeat("a", over32KiB),
			dispatch:  1,
			wantBound: platform.DefaultFrameReadLimitBytes,
		},
		{
			name:     "a ready of this release, stating the agent's limit without promptReceipt",
			steps:    []frameBoundStep{readyByThisRelease(frameBoundReady(1, fmt.Sprintf(`"maxFrameBytes":%d`, platform.MaxPromptFrameBytes)))},
			prompt:   strings.Repeat("a", over32KiB),
			dispatch: 1,
		},
		{
			name:      "a stated limit smaller than the frame, with promptReceipt",
			steps:     []frameBoundStep{readyByThisRelease(frameBoundReady(1, fmt.Sprintf(`"promptReceipt":true,"maxFrameBytes":%d`, statedSmall)))},
			prompt:    strings.Repeat("a", over32KiB),
			dispatch:  1,
			wantBound: statedSmall,
		},
		{
			name:      "a stated limit over MaxPromptFrameBytes, clamped",
			steps:     []frameBoundStep{readyByThisRelease(frameBoundReady(1, fmt.Sprintf(`"promptReceipt":true,"maxFrameBytes":%d`, statedOverTheMax)))},
			prompt:    overTheMax,
			dispatch:  1,
			wantBound: platform.MaxPromptFrameBytes,
		},
		{
			name: "a respawned gen inherits nothing",
			steps: []frameBoundStep{
				readyByThisRelease(frameBoundReady(1, fmt.Sprintf(`"promptReceipt":true,"maxFrameBytes":%d`, platform.MaxPromptFrameBytes))),
				{respawn: true},
				readyByPreviousBinary(false),
			},
			prompt:    strings.Repeat("a", over32KiB),
			dispatch:  2,
			wantBound: platform.DefaultFrameReadLimitBytes,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rig := newReceiptRig(ctx, t, receiptRigOptions{noTurn: true})
			for _, step := range tc.steps {
				switch {
				case step.event != nil:
					sendAndSettle(ctx, t, rig.actor, *step.event, step.event.Gen)
				case step.previousBinaryReady != nil:
					row, err := rig.sandboxes.Get(ctx, rig.sessionID)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := rig.pool.Exec(ctx, previousBinaryRecordSandboxReady, *step.previousBinaryReady, rig.sessionID, row.Gen); err != nil {
						t.Fatalf("the previous binary's RecordSandboxReady: %v", err)
					}
				case step.respawn:
					if _, err := rig.sandboxes.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: rig.sessionID}); err != nil {
						t.Fatalf("respawn: %v", err)
					}
					if _, err := rig.sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: rig.sessionID, Status: sqlcgen.SandboxStatusReady}); err != nil {
						t.Fatalf("move sandbox to ready: %v", err)
					}
				}
			}
			if got := len(rig.commander.prompts(t)); got != 0 {
				t.Fatalf("%d prompts sent before the turn existed, want 0", got)
			}

			rig.turnID = createPendingTurn(ctx, t, rig.turns, rig.sessionID, tc.prompt).ID
			sendAndSettle(ctx, t, rig.actor, receiptHeartbeat(tc.dispatch), tc.dispatch)

			prompts := rig.commander.prompts(t)
			got := rig.turn(ctx, t)
			if tc.wantBound == 0 {
				if len(prompts) != 1 || got.Status != sqlcgen.TurnStatusProcessing {
					t.Fatalf("%d prompts sent, turn %s; want the prompt written once and the turn processing", len(prompts), got.Status)
				}
				if size := len(prompts[0].raw); size <= platform.DefaultFrameReadLimitBytes {
					t.Fatalf("the prompt frame is %d bytes, want over %d: the case must exceed the library's default", size, platform.DefaultFrameReadLimitBytes)
				}
				return
			}

			if len(prompts) != 0 || got.Status != sqlcgen.TurnStatusFailed {
				t.Fatalf("%d prompts sent, turn %s; want nothing written and the turn failed", len(prompts), got.Status)
			}
			var reason, warning string
			var payload map[string]any
			var raw []byte
			if err := rig.pool.QueryRow(ctx,
				`SELECT (SELECT payload->>'reason' FROM events WHERE session_id = $1 AND type = 'execution_complete'),
				        (SELECT payload FROM events WHERE session_id = $1 AND type = 'execution_complete'),
				        (SELECT payload->>'message' FROM events WHERE session_id = $1 AND type = 'warning')`,
				rig.sessionID).Scan(&reason, &raw, &warning); err != nil {
				t.Fatalf("read the turn's end: %v", err)
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatalf("decode execution_complete: %v", err)
			}
			if delivered, marked := payload["delivered"]; !marked || delivered != false {
				t.Fatalf("synthetic execution_complete = %s, want it marked \"delivered\": false", raw)
			}
			if want := fmt.Sprintf("is larger than the %d bytes this sandbox's agent reads", tc.wantBound); !strings.Contains(reason, want) {
				t.Fatalf("synthetic execution_complete reason = %q, want it to contain %q", reason, want)
			}
			if want := fmt.Sprintf("more than the %s this sandbox's agent reads", formatFrameBytes(tc.wantBound)); !strings.Contains(warning, want) {
				t.Fatalf("session warning = %q, want it to contain %q", warning, want)
			}
		})
	}
}

// TestPromptReceipt_ResendMeasuredAgainstTheGensBound: a receipt re-send
// is measured against the gen's bound as its latest ready gives it, read
// with the row the re-send is built from, not against
// platform.MaxPromptFrameBytes nor against nothing: a 40 KiB prompt
// dispatched to a gen whose ready stated a limit it fits is re-sent after a
// later ready of the gen stating the same, and refused and counted as
// frame_too_large after one stating a limit it does not, the turn staying
// processing either way.
func TestPromptReceipt_ResendMeasuredAgainstTheGensBound(t *testing.T) {
	for _, tc := range []struct {
		name         string
		reconnect    string // the later ready's capabilities
		wantSent     int64
		wantTooLarge int64
		wantPrompts  int
	}{
		{name: "the later ready states a limit the frame fits", reconnect: `"promptReceipt":true,"maxFrameBytes":65536`, wantSent: 1, wantPrompts: 2},
		{name: "the later ready states one it does not", reconnect: `"promptReceipt":true,"maxFrameBytes":36864`, wantTooLarge: 1, wantPrompts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rig := newReceiptRig(ctx, t, receiptRigOptions{prompt: strings.Repeat("a", 40*1024)})
			tooLargeBefore := promptResendCount(ctx, t, promptResendOutcomeFrameTooLarge)
			sentBefore := promptResendCount(ctx, t, promptResendOutcomeSent)

			sendAndSettle(ctx, t, rig.actor, frameBoundReady(1, `"promptReceipt":true,"maxFrameBytes":65536`), 1)
			if got := len(rig.commander.prompts(t)); got != 1 {
				t.Fatalf("%d prompts sent, want the dispatch: the frame fits the 64 KiB the gen stated", got)
			}
			sendAndSettle(ctx, t, rig.actor, frameBoundReady(1, tc.reconnect), 1)
			if got := len(rig.commander.prompts(t)); got != tc.wantPrompts {
				t.Fatalf("%d prompts sent, want %d", got, tc.wantPrompts)
			}
			if got := promptResendCount(ctx, t, promptResendOutcomeSent) - sentBefore; got != tc.wantSent {
				t.Fatalf("turn_prompt_resend_total{sent} moved by %d, want %d", got, tc.wantSent)
			}
			if got := promptResendCount(ctx, t, promptResendOutcomeFrameTooLarge) - tooLargeBefore; got != tc.wantTooLarge {
				t.Fatalf("turn_prompt_resend_total{frame_too_large} moved by %d, want %d", got, tc.wantTooLarge)
			}
			if got := rig.turn(ctx, t); got.Status != sqlcgen.TurnStatusProcessing {
				t.Fatalf("turn status = %s, want processing", got.Status)
			}
		})
	}
}

// TestFrameBound_ReenqueueToARespawnedGen_MeasuredAgainstItsBound: a turn
// in flight on a gen since lost is re-enqueued to the respawned gen and
// measured against that gen's bound, read with the row the re-enqueue is
// built from: written to an agent that states the larger read limit, and
// refused, the turn failed with both sizes named, for one that states
// nothing and advertises nothing.
func TestFrameBound_ReenqueueToARespawnedGen_MeasuredAgainstItsBound(t *testing.T) {
	for _, tc := range []struct {
		name        string
		gen2Ready   string // gen 2's ready's capabilities
		wantPrompts int
		wantStatus  sqlcgen.TurnStatus
	}{
		{name: "gen 2 states the agent's limit", gen2Ready: fmt.Sprintf(`"maxFrameBytes":%d`, platform.MaxPromptFrameBytes), wantPrompts: 2, wantStatus: sqlcgen.TurnStatusProcessing},
		{name: "gen 2 states nothing", gen2Ready: "", wantPrompts: 1, wantStatus: sqlcgen.TurnStatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rig := newReceiptRig(ctx, t, receiptRigOptions{prompt: strings.Repeat("a", 40*1024)})
			sendAndSettle(ctx, t, rig.actor, frameBoundReady(1, `"promptReceipt":true`), 1)
			if got := len(rig.commander.prompts(t)); got != 1 {
				t.Fatalf("%d prompts sent, want the dispatch to gen 1", got)
			}

			if _, err := rig.sandboxes.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: rig.sessionID}); err != nil {
				t.Fatalf("respawn: %v", err)
			}
			if _, err := rig.sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: rig.sessionID, Status: sqlcgen.SandboxStatusReady}); err != nil {
				t.Fatalf("move sandbox to ready: %v", err)
			}
			sendAndSettle(ctx, t, rig.actor, frameBoundReady(2, tc.gen2Ready), 2)

			if got := len(rig.commander.prompts(t)); got != tc.wantPrompts {
				t.Fatalf("%d prompts sent, want %d", got, tc.wantPrompts)
			}
			got := rig.turn(ctx, t)
			if got.Status != tc.wantStatus {
				t.Fatalf("turn status = %s, want %s", got.Status, tc.wantStatus)
			}
			if tc.wantStatus != sqlcgen.TurnStatusFailed {
				return
			}
			var reason string
			if err := rig.pool.QueryRow(ctx,
				`SELECT payload->>'reason' FROM events WHERE session_id = $1 AND type = 'execution_complete'`, rig.sessionID).Scan(&reason); err != nil {
				t.Fatalf("read the turn's end: %v", err)
			}
			if want := fmt.Sprintf("is larger than the %d bytes this sandbox's agent reads", platform.DefaultFrameReadLimitBytes); !strings.Contains(reason, want) {
				t.Fatalf("synthetic execution_complete reason = %q, want it to contain %q", reason, want)
			}
		})
	}
}

// promptTextForFrame returns a prompt text whose prompt frame, as a
// dispatch of rig's pending turn to its sandbox's live gen builds it,
// encodes to exactly frameBytes bytes. The frame's other fields have a
// fixed length -- the messageId is a fresh UUID -- so its overhead is
// measured once with BuildPromptPayload on the rows the dispatch reads.
func promptTextForFrame(ctx context.Context, t *testing.T, rig *receiptRig, frameBytes int, receiptRequested bool) string {
	t.Helper()
	sessionRow, err := narvipg.NewSessionStore(rig.pool).Get(ctx, rig.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	sandboxRow, err := rig.sandboxes.Get(ctx, rig.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	turnRow := rig.turn(ctx, t)
	probe := "a"
	turnRow.Prompt = &probe
	payload, err := BuildPromptPayload(rig.sessionID.String(), sessionRow, sandboxRow, turnRow, uuid.NewString(), receiptRequested)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Repeat("a", frameBytes-(len(payload)-len(probe)))
}

// TestFrameBound_AFrameOfExactlyItsBound_WrittenOneByteMoreRefused pins the
// comparison against the gen's bound to the agent's own: an agent reads a
// message of exactly its read limit (TestDefaultFrameReadLimitBytes_IsTheLibraryDefault),
// so a prompt frame of exactly its gen's bound is written and one a byte
// longer is refused -- on a first dispatch to a gen held to the library's
// default, and on a receipt re-send to a gen whose latest ready states a
// limit. The refusal's banner shows the two sizes in exact bytes, where
// rounded they would read the same.
func TestFrameBound_AFrameOfExactlyItsBound_WrittenOneByteMoreRefused(t *testing.T) {
	t.Run("dispatch", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			frame    int
			wantSent bool
		}{
			{name: "exactly the bound", frame: platform.DefaultFrameReadLimitBytes, wantSent: true},
			{name: "a byte over it", frame: platform.DefaultFrameReadLimitBytes + 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ctx := context.Background()
				rig := newReceiptRig(ctx, t, receiptRigOptions{})
				text := promptTextForFrame(ctx, t, rig, tc.frame, false)
				if _, err := rig.pool.Exec(ctx, `UPDATE turns SET prompt = $2 WHERE id = $1`, rig.turnID, text); err != nil {
					t.Fatal(err)
				}
				// A ready that states nothing and advertises nothing: the gen is
				// held to the library's default.
				sendAndSettle(ctx, t, rig.actor, frameBoundReady(1, ""), 1)

				prompts := rig.commander.prompts(t)
				got := rig.turn(ctx, t)
				if !tc.wantSent {
					if len(prompts) != 0 || got.Status != sqlcgen.TurnStatusFailed {
						t.Fatalf("%d prompts sent, turn %s; want a %d-byte frame refused", len(prompts), got.Status, tc.frame)
					}
					var warning string
					if err := rig.pool.QueryRow(ctx, `SELECT payload->>'message' FROM events WHERE session_id = $1 AND type = 'warning'`, rig.sessionID).Scan(&warning); err != nil {
						t.Fatal(err)
					}
					if want := fmt.Sprintf("its prompt is %d bytes once encoded, more than the %d bytes this sandbox's agent reads", tc.frame, platform.DefaultFrameReadLimitBytes); !strings.Contains(warning, want) {
						t.Fatalf("session warning = %q, want it to contain %q", warning, want)
					}
					return
				}
				if len(prompts) != 1 || got.Status != sqlcgen.TurnStatusProcessing {
					t.Fatalf("%d prompts sent, turn %s; want a %d-byte frame written", len(prompts), got.Status, tc.frame)
				}
				if size := len(prompts[0].raw); size != tc.frame {
					t.Fatalf("the prompt frame written is %d bytes, want exactly %d", size, tc.frame)
				}
			})
		}
	})

	t.Run("receipt re-send", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			overBound int // how far the frame is over the bound the later ready states
			wantSent  bool
		}{
			{name: "exactly the bound", overBound: 0, wantSent: true},
			{name: "a byte over it", overBound: 1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ctx := context.Background()
				rig := newReceiptRig(ctx, t, receiptRigOptions{prompt: strings.Repeat("a", 40*1024)})
				tooLargeBefore := promptResendCount(ctx, t, promptResendOutcomeFrameTooLarge)

				sendAndSettle(ctx, t, rig.actor, frameBoundReady(1, `"promptReceipt":true,"maxFrameBytes":65536`), 1)
				prompts := rig.commander.prompts(t)
				if len(prompts) != 1 {
					t.Fatalf("%d prompts sent, want the dispatch", len(prompts))
				}
				frame := len(prompts[0].raw)
				sendAndSettle(ctx, t, rig.actor, frameBoundReady(1, fmt.Sprintf(`"promptReceipt":true,"maxFrameBytes":%d`, frame-tc.overBound)), 1)

				prompts = rig.commander.prompts(t)
				tooLarge := promptResendCount(ctx, t, promptResendOutcomeFrameTooLarge) - tooLargeBefore
				if tc.wantSent {
					if len(prompts) != 2 || len(prompts[1].raw) != frame || tooLarge != 0 {
						t.Fatalf("%d prompts sent, frame_too_large moved by %d; want the %d-byte frame re-sent at a bound of %d", len(prompts), tooLarge, frame, frame)
					}
					return
				}
				if len(prompts) != 1 || tooLarge != 1 {
					t.Fatalf("%d prompts sent, frame_too_large moved by %d; want the %d-byte frame refused at a bound of %d", len(prompts), tooLarge, frame, frame-1)
				}
				if got := rig.turn(ctx, t); got.Status != sqlcgen.TurnStatusProcessing {
					t.Fatalf("turn status = %s, want processing", got.Status)
				}
			})
		}
	})
}
