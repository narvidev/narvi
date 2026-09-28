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
	for _, source := range []sqlcgen.SessionSpawnSource{"mcp", "a_future_source"} {
		t.Run(string(source), func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			a := &Actor{logger: slog.New(slog.NewJSONHandler(&logs, nil))}

			var noReason turn.FailureReason
			if err := a.enqueueOutboxNotification(context.Background(), nil, sqlcgen.Session{SpawnSource: source}, turn.TriggerComplete, noReason, sqlcgen.Turn{}, nil); err != nil {
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
