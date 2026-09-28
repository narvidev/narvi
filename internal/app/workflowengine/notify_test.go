package workflowengine

import (
	"context"
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
	for _, source := range []sqlcgen.SessionSpawnSource{"mcp", "a_future_source"} {
		t.Run(string(source), func(t *testing.T) {
			t.Parallel()
			if err := enqueueWorkflowNotice(context.Background(), Deps{}, sqlcgen.Session{SpawnSource: source}, "A step is waiting for a decision."); err != nil {
				t.Fatalf("enqueueWorkflowNotice = %v, want nil", err)
			}
		})
	}
}
