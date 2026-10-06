//go:build integration

package mcp_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/turnguard"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
)

// TestSendPrompt_AtSpendCap_ReturnsTheRefusal: narvi_send_prompt on a
// session that has spent its cap (technical plan §40.1) is a tool error
// carrying the refusal's own text -- the same text the REST twin's 409
// carries, beside its typed reason -- and creates no turn. The crossing's
// warning is recorded once.
func TestSendPrompt_AtSpendCap_ReturnsTheRefusal(t *testing.T) {
	ctx := context.Background()
	rig := newMCPTestRig(t)
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	session := createSessionForUser(ctx, t, rig, user.ID)
	bearer := mintMCPToken(ctx, t, rig, user.ID, []string{"mcp:read", "mcp:write"})

	const repo = "acme/mcp-cap"
	if _, err := rig.pool.Exec(ctx, `UPDATE sessions SET repos = '[{"url": "https://github.com/acme/mcp-cap.git"}]'::jsonb WHERE id = $1`, session.ID); err != nil {
		t.Fatalf("name the session's repository: %v", err)
	}
	if _, err := rig.pool.Exec(ctx, `INSERT INTO turns (session_id, status, dispatched_at, completed_at, cost_usd) VALUES ($1, 'completed', now(), now(), 1.25)`, session.ID); err != nil {
		t.Fatalf("store the session's spend: %v", err)
	}
	if _, err := rig.pool.Exec(ctx, `INSERT INTO repo_settings (repo_full_name, session_spend_cap_usd) VALUES ($1, 1.00)`, repo); err != nil {
		t.Fatalf("cap the repository: %v", err)
	}
	refusal := sessionguard.Refusal{
		Reason: sessionguard.ReasonSpendCap, SessionID: session.ID.Bytes, Cap: 1_000_000, Spent: 1_250_000,
		Source: sessionguard.CapSource{Kind: sessionguard.CapSourceRepo, Name: repo, ID: repo}, Turns: 1,
	}

	mcpStatus, env := rig.callTool(t, "narvi_send_prompt", fmt.Sprintf(`{"sessionId":%q,"prompt":"one more thing"}`, session.ID.String()), bearer)
	if mcpStatus != http.StatusOK || env.Result == nil || !env.Result.IsError || len(env.Result.Content) != 1 || env.Result.Content[0].Text != sessionguard.Text(refusal) {
		t.Fatalf("narvi_send_prompt: status %d, result %+v; want a tool error carrying the refusal's text", mcpStatus, env.Result)
	}

	var rest struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	restStatus := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/turns", []byte(`{"prompt":"one more thing","modelId":null,"effort":null,"planMode":false}`), &rest, cookie)
	if restStatus != http.StatusConflict || rest.Error != sessionguard.Text(refusal) || rest.Reason != string(sessionguard.ReasonSpendCap) {
		t.Fatalf("REST twin: %d %+v, want 409 with the same text and the typed reason", restStatus, rest)
	}

	var turns, warnings int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, session.ID).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	if turns != 1 {
		t.Fatalf("turns = %d, want the spend's alone", turns)
	}
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND message_id = $2`, session.ID, turnguard.WarningMessageID(refusal)).Scan(&warnings); err != nil {
		t.Fatal(err)
	}
	if warnings != 1 {
		t.Fatalf("warnings = %d, want the crossing's one", warnings)
	}
}
