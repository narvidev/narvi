//go:build integration

// Row 182's piece (a) on the production router (technical plan §43.20):
// narvi_get_session_status and narvi_get_session_transcript, called through
// the official SDK client under a real mcp:read grant, answer the bytes
// their REST twins answer the same user's cookie; the status carries no
// transcript; and a grant without mcp:read is told neither tool exists.
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/narvidev/narvi/contracts"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// seedLaggingSession creates a session for userID whose row says
// "completed" -- the outcome of its first, finished turn -- while a
// follow-up turn is queued (the lag technical plan §43.20 exists for), and
// gives it n events.
func seedLaggingSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID pgtype.UUID, n int) pgtype.UUID {
	t.Helper()
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	events := narvipg.NewEventStore(pool)
	sess, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: userID})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	first, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sess.ID, Status: sqlcgen.TurnStatusPending})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	if _, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: first.ID, Status: sqlcgen.TurnStatusCompleted, CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}); err != nil {
		t.Fatalf("complete turn: %v", err)
	}
	if _, err := sessions.UpdateStatus(ctx, sqlcgen.UpdateSessionStatusParams{ID: sess.ID, Status: sqlcgen.SessionStatusCompleted}); err != nil {
		t.Fatalf("derive the row: %v", err)
	}
	if _, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sess.ID, Status: sqlcgen.TurnStatusPending}); err != nil {
		t.Fatalf("queue a follow-up: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := events.Create(ctx, sqlcgen.CreateEventParams{
			SessionID: sess.ID,
			Type:      "token",
			MessageID: fmt.Sprintf("status-%s-%d", sess.ID.String(), i),
			Payload:   []byte(fmt.Sprintf(`{"n":%d}`, i)),
		}); err != nil {
			t.Fatalf("create event %d: %v", i, err)
		}
	}
	return sess.ID
}

// restRaw GETs path on the rig's router with cookie and returns the status
// and the body's exact bytes.
func (r *oauthRouterRig) restRaw(t *testing.T, path, cookie string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, r.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

// toolText returns a successful tool result's one text block: the twin's
// own response body, verbatim (technical plan §43.8).
func toolText(t *testing.T, res *sdkmcp.CallToolResult) []byte {
	t.Helper()
	if res == nil || res.IsError || len(res.Content) != 1 {
		t.Fatalf("tool result %+v, want one successful text block", res)
	}
	text, ok := res.Content[0].(*sdkmcp.TextContent)
	if !ok {
		t.Fatalf("tool result content %T, want text", res.Content[0])
	}
	return []byte(text.Text)
}

// sessionActivityKeys is SessionActivity's own property set, from the
// embedded contract.
func sessionActivityKeys(t *testing.T) []string {
	t.Helper()
	data, err := contracts.FS.ReadFile("rest/v1/dtos.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0)
	for k := range doc.Defs["SessionActivity"].Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// assertStatusBytesEqual compares a status body read through REST with
// one read through the tool, byte for byte, except observedAt: that is
// each snapshot's own database clock, so two reads never share it. The
// MCP read came second, so its observedAt is not earlier; substituting it
// into the REST bytes must then give the MCP bytes exactly. Both bodies
// carry exactly SessionActivity's keys: no events, no transcript.
func assertStatusBytesEqual(t *testing.T, label string, restBody, mcpBody []byte) map[string]any {
	t.Helper()
	var rest, viaMCP map[string]any
	if err := json.Unmarshal(restBody, &rest); err != nil {
		t.Fatalf("%s: REST body %s: %v", label, restBody, err)
	}
	if err := json.Unmarshal(mcpBody, &viaMCP); err != nil {
		t.Fatalf("%s: MCP body %s: %v", label, mcpBody, err)
	}
	want := sessionActivityKeys(t)
	for side, body := range map[string]map[string]any{"REST": rest, "MCP": viaMCP} {
		keys := make([]string, 0, len(body))
		for k := range body {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: %s status keys %v, want exactly SessionActivity's %v -- the status must carry no transcript", label, side, keys, want)
		}
	}
	restAt, _ := rest["observedAt"].(string)
	mcpAt, _ := viaMCP["observedAt"].(string)
	restTime, err1 := time.Parse(time.RFC3339Nano, restAt)
	mcpTime, err2 := time.Parse(time.RFC3339Nano, mcpAt)
	if err1 != nil || err2 != nil || mcpTime.Before(restTime) {
		t.Fatalf("%s: observedAt REST %q MCP %q, want two timestamps in read order", label, restAt, mcpAt)
	}
	if bytes.Count(restBody, []byte(`"observedAt":"`+restAt+`"`)) != 1 {
		t.Fatalf("%s: REST body does not carry observedAt exactly once: %s", label, restBody)
	}
	substituted := bytes.Replace(restBody, []byte(`"observedAt":"`+restAt+`"`), []byte(`"observedAt":"`+mcpAt+`"`), 1)
	if !bytes.Equal(bytes.TrimSpace(substituted), bytes.TrimSpace(mcpBody)) {
		t.Fatalf("%s: bytes differ beyond observedAt.\nREST: %s\nMCP:  %s", label, restBody, mcpBody)
	}
	return viaMCP
}

// sdkSessionStatusAndTranscript is TestOAuth_ProductionRouter's
// SessionStatusAndTranscript_SDKClient: the official SDK client, holding a
// real mcp:read grant from the consent flow, reads a session whose row
// says "completed" while a follow-up is queued. narvi_get_session_status
// answers queued -- the REST twin's bytes -- and no transcript field;
// narvi_get_session_transcript walks the event history a page at a time,
// each page byte-identical to the REST twin's for the same cursor.
func sdkSessionStatusAndTranscript(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, nil)
	sessionID := seedLaggingSession(ctx, t, rig.pool, flow.member.ID, 5)
	id := sessionID.String()

	status, restStatus := rig.restRaw(t, "/api/sessions/"+id+"/status", flow.cookie)
	if status != http.StatusOK {
		t.Fatalf("REST status: %d %s", status, restStatus)
	}
	res, err := flow.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "narvi_get_session_status", Arguments: map[string]any{"sessionId": id}})
	if err != nil {
		t.Fatalf("CallTool narvi_get_session_status: %v", err)
	}
	got := assertStatusBytesEqual(t, "narvi_get_session_status", restStatus, toolText(t, res))
	structured, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var structuredMap map[string]any
	if err := json.Unmarshal(structured, &structuredMap); err != nil || fmt.Sprint(structuredMap) != fmt.Sprint(got) {
		t.Fatalf("structuredContent %s differs from the text block", structured)
	}
	if got["activity"] != "queued" || got["settled"] != false || got["pendingTurns"] != float64(1) {
		t.Fatalf("status = %v, want queued, unsettled, one pending turn", got)
	}
	if code, row := rig.restRaw(t, "/api/sessions/"+id, flow.cookie); code != http.StatusOK || !bytes.Contains(row, []byte(`"status":"completed"`)) {
		t.Fatalf("GET session: %d %s, want the row still saying completed (the lag this tool exists for)", code, row)
	}

	var cursor *string
	pages, events := 0, 0
	for {
		path := "/api/sessions/" + id + "/events?limit=2"
		args := map[string]any{"sessionId": id, "limit": 2}
		if cursor != nil {
			path += "&cursor=" + *cursor
			args["cursor"] = *cursor
		}
		code, restPage := rig.restRaw(t, path, flow.cookie)
		if code != http.StatusOK {
			t.Fatalf("REST events page %d: %d %s", pages, code, restPage)
		}
		res, err := flow.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "narvi_get_session_transcript", Arguments: args})
		if err != nil {
			t.Fatalf("CallTool narvi_get_session_transcript page %d: %v", pages, err)
		}
		mcpPage := toolText(t, res)
		if !bytes.Equal(bytes.TrimSpace(restPage), bytes.TrimSpace(mcpPage)) {
			t.Fatalf("transcript page %d differs from REST.\nREST: %s\nMCP:  %s", pages, restPage, mcpPage)
		}
		var page struct {
			Events     []json.RawMessage `json:"events"`
			NextCursor *string           `json:"nextCursor"`
		}
		if err := json.Unmarshal(mcpPage, &page); err != nil {
			t.Fatal(err)
		}
		pages++
		events += len(page.Events)
		if cursor = page.NextCursor; cursor == nil {
			break
		}
		if pages > 10 {
			t.Fatal("the walk did not end")
		}
	}
	if pages != 3 || events != 5 {
		t.Fatalf("walked %d pages and %d events, want 3 pages (2+2+1) and 5 events", pages, events)
	}
}

// sdkSessionStatusAndTranscriptScopeless is TestOAuth_ProductionRouter's
// SessionStatusAndTranscript_ScopelessGrantSeesNeither: a grant the
// member approved without mcp:read is told neither tool exists -- they are
// absent from tools/list, the SDK's own calls fail, and a raw call answers
// exactly what a tool that never existed answers, byte for byte once the
// name the caller chose is substituted.
func sdkSessionStatusAndTranscriptScopeless(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, func(d *consentDriver) {
		d.keepScopes = func([]string) []string { return nil }
	})
	sessionID := seedLaggingSession(ctx, t, rig.pool, flow.member.ID, 1).String()

	for _, name := range toolNames(ctx, t, flow.session) {
		if name == "narvi_get_session_status" || name == "narvi_get_session_transcript" {
			t.Fatalf("a scope-less grant lists %s", name)
		}
	}
	scopeless := flow.recorder.lastBearer()
	other, _ := createRouterUser(ctx, t, rig.pool, sqlcgen.UserRoleMember)
	full := mintBuildBearer(ctx, t, rig.pool, rig.cfg, other.ID)
	call := func(token, tool string) (int, string) {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":%q,"arguments":{"sessionId":%q},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, tool, sessionID)
		status, _, raw := rig.postMCP(t, "tools/call", tool, body, token)
		return status, string(raw)
	}
	const unknown = "narvi_does_not_exist"
	unknownStatus, unknownBody := call(full, unknown)
	for _, hidden := range []string{"narvi_get_session_status", "narvi_get_session_transcript"} {
		if _, err := flow.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: hidden, Arguments: map[string]any{"sessionId": sessionID}}); err == nil {
			t.Fatalf("calling hidden %s through the SDK succeeded", hidden)
		}
		status, body := call(scopeless, hidden)
		if status != unknownStatus || body != strings.ReplaceAll(unknownBody, unknown, hidden) {
			t.Fatalf("hidden %s answers differently from an unknown tool:\n hidden:  %d %s\n unknown: %d %s", hidden, status, body, unknownStatus, unknownBody)
		}
		var env struct {
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil || env.Error == nil || env.Error.Code != -32602 {
			t.Fatalf("hidden %s: %s, want a -32602 JSON-RPC error", hidden, body)
		}
		// The very same tool under a full mcp:read grant is there.
		if status, body := call(full, hidden); status != http.StatusOK || strings.Contains(body, `"error"`) {
			t.Fatalf("%s under mcp:read: %d %s, want a result", hidden, status, body)
		}
	}
}
