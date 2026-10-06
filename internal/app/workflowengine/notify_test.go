package workflowengine

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestEnqueueWorkflowNotice_UnknownSpawnSource pins the routing side of
// Session.spawnSource being an OPEN enum (contracts/manifest.json's
// openEnums): a workflow notice for a session whose source this binary has
// no channel for -- as an older binary sees one during a rolling deploy
// after a newer migration added it -- is skipped without an error, like a
// 'web' session's. Deps is empty, so a channel lookup or an outbox insert
// would dereference a nil store.
func TestEnqueueWorkflowNotice_UnknownSpawnSource(t *testing.T) {
	t.Parallel()
	for _, source := range []sqlcgen.SessionSpawnSource{"a_future_source", "Mcp"} {
		t.Run(string(source), func(t *testing.T) {
			t.Parallel()
			if _, err := enqueueWorkflowNotice(context.Background(), Deps{}, sqlcgen.Session{SpawnSource: source}, "A step is waiting for a decision."); err != nil {
				t.Fatalf("enqueueWorkflowNotice = %v, want nil", err)
			}
		})
	}
}

// TestEnqueueWorkflowNotice_McpEnqueuesNothingNoWarn pins how a workflow
// notice for an 'mcp'-origin session is routed: exactly like a 'web' one.
// An MCP client has no channel to notify -- it polls the session's status
// or waits on it (technical plan §43.20) -- so nothing is enqueued, and
// nothing is logged either: 'mcp' is a source this binary knows, never the
// unrecognised-source WARN. Deps is empty, so a channel lookup or an outbox
// insert would dereference a nil store. The last row is the control: the
// same capture does see the WARN an unrecognised source logs.
//
// Not parallel: enqueueWorkflowNotice logs through platform.Logger, which
// reads slog.Default(), so the capture replaces the process default for
// the test's duration. Parallel tests are paused until the serial ones
// have finished; a goroutine an earlier serial integration test left
// behind may still log, so only a WARN or ERROR line, or a line naming a
// spawn_source, counts.
func TestEnqueueWorkflowNotice_McpEnqueuesNothingNoWarn(t *testing.T) {
	logs := &lockedBuffer{}
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	for _, tc := range []struct {
		source   sqlcgen.SessionSpawnSource
		wantWarn bool
	}{
		{source: sqlcgen.SessionSpawnSourceMcp},
		{source: sqlcgen.SessionSpawnSourceWeb},
		{source: "a_future_source", wantWarn: true},
	} {
		t.Run(string(tc.source), func(t *testing.T) {
			logs.Reset()
			if _, err := enqueueWorkflowNotice(context.Background(), Deps{}, sqlcgen.Session{SpawnSource: tc.source}, "A step is waiting for a decision."); err != nil {
				t.Fatalf("enqueueWorkflowNotice = %v, want nil", err)
			}

			var relevant []string
			warned := false
			for _, raw := range bytes.Split(logs.Bytes(), []byte("\n")) {
				if len(bytes.TrimSpace(raw)) == 0 {
					continue
				}
				var line struct {
					Level       string  `json:"level"`
					Msg         string  `json:"msg"`
					SpawnSource *string `json:"spawn_source"`
				}
				if err := json.Unmarshal(raw, &line); err != nil {
					t.Fatalf("unmarshal log line %q: %v", raw, err)
				}
				if line.Level == "WARN" || line.Level == "ERROR" || line.SpawnSource != nil {
					relevant = append(relevant, string(raw))
				}
				if line.Level == "WARN" && strings.Contains(line.Msg, "unrecognized spawn_source") && line.SpawnSource != nil && *line.SpawnSource == string(tc.source) {
					warned = true
				}
			}
			switch {
			case tc.wantWarn && !warned:
				t.Errorf("logs = %q, want the unrecognised-source WARN naming %q (the capture is broken)", relevant, tc.source)
			case !tc.wantWarn && len(relevant) != 0:
				t.Errorf("logs = %q, want no WARN and no spawn_source for a source with no channel", relevant)
			}
		})
	}
}

// lockedBuffer is a bytes.Buffer safe to share with a goroutine that may
// still be logging.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

func (b *lockedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}
