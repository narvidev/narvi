package sessionactor

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// TestEnqueueOutboxNotification_UnknownSpawnSource pins the routing side
// of Session.spawnSource being an OPEN enum (contracts/manifest.json's
// openEnums): a turn completing on a session whose source this binary has
// no channel for -- as an older binary sees one during a rolling deploy
// after a newer migration added it -- enqueues nothing, fails nothing, and
// logs the source it did not recognise. The Actor has no stores, so a
// channel lookup or an outbox insert would dereference a nil store.
func TestEnqueueOutboxNotification_UnknownSpawnSource(t *testing.T) {
	t.Parallel()
	for _, source := range []sqlcgen.SessionSpawnSource{"a_future_source", "Mcp"} {
		t.Run(string(source), func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			a := &Actor{logger: slog.New(slog.NewJSONHandler(&logs, nil))}

			var noReason turn.FailureReason
			if err := a.enqueueOutboxNotification(context.Background(), nil, sqlcgen.Session{SpawnSource: source}, turn.TriggerComplete, noReason, sqlcgen.Turn{}, nil, ""); err != nil {
				t.Fatalf("enqueueOutboxNotification = %v, want nil", err)
			}

			var line struct {
				Level       string `json:"level"`
				Msg         string `json:"msg"`
				SpawnSource string `json:"spawn_source"`
			}
			if err := json.Unmarshal(logs.Bytes(), &line); err != nil {
				t.Fatalf("want exactly one JSON log line, got %q: %v", logs.String(), err)
			}
			if line.Level != "WARN" || !strings.Contains(line.Msg, "unrecognized spawn_source") || line.SpawnSource != string(source) {
				t.Errorf("log line = %+v, want a WARN naming spawn_source %q", line, source)
			}
		})
	}
}

// TestEnqueueOutboxNotification_McpEnqueuesNothingNoWarn pins how a turn
// completing on an 'mcp'-origin session is routed: exactly like a 'web'
// one. An MCP client has no channel to notify -- it polls the session's
// status or waits on it (technical plan §43.20) -- so nothing is enqueued,
// and nothing is logged either: 'mcp' is a source this binary knows, never
// the unrecognised-source WARN. That holds for a plan-mode completion too,
// which on a bot surface enqueues the richer plan-approval notice. The
// Actor has no stores, so a channel lookup or an outbox insert would
// dereference a nil store. The last row is the control: the same capture
// does see the WARN an unrecognised source logs.
func TestEnqueueOutboxNotification_McpEnqueuesNothingNoWarn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		source   sqlcgen.SessionSpawnSource
		wantWarn bool
	}{
		{source: sqlcgen.SessionSpawnSourceMcp},
		{source: sqlcgen.SessionSpawnSourceWeb},
		{source: "a_future_source", wantWarn: true},
	} {
		for _, completion := range []struct {
			name string
			trig turn.Trigger
			plan *sqlcgen.Plan
		}{
			{name: "completed", trig: turn.TriggerComplete},
			{name: "failed", trig: turn.TriggerFail},
			{name: "plan completed", trig: turn.TriggerComplete, plan: &sqlcgen.Plan{Version: 1}},
		} {
			t.Run(string(tc.source)+"/"+completion.name, func(t *testing.T) {
				t.Parallel()
				var logs bytes.Buffer
				a := &Actor{logger: slog.New(slog.NewJSONHandler(&logs, nil))}

				var noReason turn.FailureReason
				if err := a.enqueueOutboxNotification(context.Background(), nil, sqlcgen.Session{SpawnSource: tc.source}, completion.trig, noReason, sqlcgen.Turn{}, completion.plan, ""); err != nil {
					t.Fatalf("enqueueOutboxNotification = %v, want nil", err)
				}

				warned := strings.Contains(logs.String(), "unrecognized spawn_source")
				switch {
				case tc.wantWarn && !warned:
					t.Errorf("logs = %q, want the unrecognised-source WARN (the capture is broken)", logs.String())
				case !tc.wantWarn && logs.Len() != 0:
					t.Errorf("logs = %q, want nothing logged for a source with no channel", logs.String())
				}
			})
		}
	}
}

// TestOutcomeText_TerminalTriggers pins what a Slack or Linear thread is
// told for each way a turn ends: a turn given up on before it started
// (turn.TriggerAbandon, the dispatch gate's refusal) failed, and says so
// with its reason, never the generic "Turn finished.".
func TestOutcomeText_TerminalTriggers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		trig   turn.Trigger
		reason turn.FailureReason
		want   string
	}{
		{turn.TriggerComplete, "", "Turn completed successfully."},
		{turn.TriggerCancel, turn.FailureReasonCancelled, "Turn was cancelled."},
		{turn.TriggerFail, turn.FailureReasonFailed, "Turn failed (failed)."},
		{turn.TriggerTimeout, turn.FailureReasonTimeout, "Turn failed (timeout)."},
		{turn.TriggerAbandon, turn.FailureReasonNeverStarted, "Turn failed (never_started)."},
	}
	for _, tc := range tests {
		t.Run(tc.trig.String(), func(t *testing.T) {
			t.Parallel()
			if got := outcomeText(tc.trig, tc.reason); got != tc.want {
				t.Errorf("outcomeText(%s, %q) = %q, want %q", tc.trig, tc.reason, got, tc.want)
			}
		})
	}
}
