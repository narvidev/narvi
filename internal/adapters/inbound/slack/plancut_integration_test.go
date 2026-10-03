//go:build integration

// This file holds the seed the Slack cut-plan tests share: a plan whose
// final text is a `token` frame the sandbox-agent cut on its way to the
// control plane (technical plan §6.1), injected straight into `events` as
// the session actor stores it, since nothing produces a cut yet.
package slack_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/domain/framecut"
)

// seedCutPlanText places turnID's window at the session's current
// watermark and stores, inside it, a text part whose only text is a cut
// frame: its empty first frame under the bare part id, then the cut one.
// It returns the cut, whose framecut.Reason every surface relays.
func seedCutPlanText(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID, turnID pgtype.UUID) framecut.Cut {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE turns SET dispatched_event_id = (SELECT COALESCE(MAX(id), 0) FROM events WHERE session_id = $1) WHERE id = $2`, sessionID, turnID); err != nil {
		t.Fatalf("place the plan turn's window: %v", err)
	}
	cut := framecut.Cut{Kept: 20, Total: 40960}
	frames := []struct {
		key  string
		text string
		cut  *framecut.Cut
	}{
		{key: "prt_plan", text: ""},
		{key: "prt_plan#cut", text: "1. Add the migration\n[text cut at 20 of 40960 bytes on its way from the sandbox]", cut: &cut},
	}
	for _, f := range frames {
		payload := map[string]any{"type": "token", "messageId": "prt_plan", "sessionId": sessionID.String(), "gen": 1, "text": f.text}
		if f.cut != nil {
			payload["cut"] = f.cut
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal token frame: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO events (session_id, type, payload, message_id) VALUES ($1, 'token', $2, $3)`, sessionID, raw, f.key); err != nil {
			t.Fatalf("store token frame: %v", err)
		}
	}
	return cut
}
