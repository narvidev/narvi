package reviewcontext_test

import (
	"context"
	"testing"

	"github.com/narvidev/narvi/internal/app/reviewcontext"
)

// TestRecordKnowledgeBlockTokens_NeverPanics is a smoke test against the
// DEFAULT global (no-op) MeterProvider -- exactly what a process that
// never called platform.SetupOTel has (early boot, some cmd/sandbox-agent
// contexts) -- proving §31.2's own fail-safe requirement holds even
// then: losing this ONE gauge must never be a reason to fail, delay, or
// panic a real review turn's own creation. Every one of this package's
// three real call sites (internal/adapters/inbound/github/handler.go,
// internal/adapters/inbound/httpapi/reviewretrigger.go,
// internal/app/sessionactor/reviewretrigger.go) calls this
// unconditionally, so a panic here would take down a real review turn.
func TestRecordKnowledgeBlockTokens_NeverPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RecordKnowledgeBlockTokens panicked: %v", r)
		}
	}()

	reviewcontext.RecordKnowledgeBlockTokens(context.Background(), "", "")
	reviewcontext.RecordKnowledgeBlockTokens(context.Background(), "some false-positive advisory text", "")
	reviewcontext.RecordKnowledgeBlockTokens(context.Background(), "", "some arch-decisions text")
	reviewcontext.RecordKnowledgeBlockTokens(context.Background(), "some false-positive advisory text", "some arch-decisions text")
}
