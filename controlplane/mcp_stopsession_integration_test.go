//go:build integration

// This file is row 183's stop over MCP (technical plan §43.22) on the
// router Build returns, driven by the official SDK client through the real
// consent flow, as TestOAuth_ProductionRouter's subtests.
//
//   - A stop over MCP runs the REST stop handler unchanged: on a session and
//     the child it started, its answer is REST's by the same user's cookie,
//     byte for byte once ids and the instant are written as their roles, and
//     the database state it leaves -- each session's flag, each turn's
//     status, flag and failure reason, the synthetic terminal events, the
//     timers and the session.stop audit rows -- equals REST's; every audit
//     row written over MCP, the child's included, carries the grant.
//   - For every role, on a session the user started, one they joined, one
//     another member started, and a pull request's review session they
//     started, the outcome and the state equal REST's: an admin or
//     maintainer stops any of them, a member only the first two, a viewer
//     none -- a token never does more than its user.
//   - A read-only grant is not told the tool exists, a call to it answers an
//     unknown tool's bytes, and nothing is written.
//   - The row's exit (PlanRevisionThenStop_NoOrphan_SDKClient): a plan is
//     approved and its implementation runs on a sandbox connected to this
//     router's own sandbox WebSocket; a revision is refused and the
//     implementation keeps running, approved; a child session is started by
//     the codebase's one child-spawn path, the sentinel auto-fix notifier,
//     and has work of its own queued; a stop on the parent, over MCP,
//     reaches both; the agent is told to stop and confirms; and once every
//     session the stop reached has settled, no turn of the parent or of any
//     session it started, however deep, is pending, dispatched or
//     processing in Postgres.
//
// Every stop that is accepted wakes the actor of each session it reaches,
// so the stop tests run on routers of their own (createRouterRig): the
// parity rig for the first three, and the row's exit on one of its own,
// whose sandbox provider is a local fake so that no spawn leaves this
// machine.
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// stopForbidden is REST's refusal of a stop the user's role, or the
// own/joined and review-session rule, does not allow (httpapi.authorize).
const stopForbidden = "not authorized to perform this action"

// stopTree is one seeded session tree: a parent and, when it has one, a
// child the parent started, each with one queued turn.
type stopTree struct {
	parent, child         pgtype.UUID
	parentTurn, childTurn pgtype.UUID
}

// sessions names the tree's sessions by role, parent first.
func (tr stopTree) sessions() []roleID {
	out := []roleID{{"parent", tr.parent}}
	if tr.child.Valid {
		out = append(out, roleID{"child", tr.child})
	}
	return out
}

// roleID is an id with the role it plays in a comparison.
type roleID struct {
	role string
	id   pgtype.UUID
}

// seedStopTree creates a session started by creator with one queued turn
// and, when withChild, a child session of it -- started by no one, as the
// code host's review starts one -- with one queued turn too.
func seedStopTree(ctx context.Context, t *testing.T, rig *oauthRouterRig, creator pgtype.UUID, withChild bool) stopTree {
	t.Helper()
	sessions := narvipg.NewSessionStore(rig.pool)
	parent, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: creator})
	if err != nil {
		t.Fatalf("create the parent session: %v", err)
	}
	tr := stopTree{parent: parent.ID, parentTurn: seedTurn(ctx, t, rig, parent.ID, sqlcgen.TurnStatusPending, false)}
	if withChild {
		child, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub, ParentSessionID: parent.ID, SpawnDepth: 1})
		if err != nil {
			t.Fatalf("create the child session: %v", err)
		}
		tr.child, tr.childTurn = child.ID, seedTurn(ctx, t, rig, child.ID, sqlcgen.TurnStatusPending, false)
	}
	return tr
}

// callStop calls narvi_stop_session on sessionID through the SDK session.
func callStop(ctx context.Context, t *testing.T, s *sdkmcp.ClientSession, sessionID pgtype.UUID) *sdkmcp.CallToolResult {
	t.Helper()
	return callTool(ctx, t, s, "narvi_stop_session", map[string]any{"sessionId": sessionID.String()})
}

// restStop posts the REST stop of sessionID with cookie, no body, returning
// the status and the body exactly as the route wrote it.
func restStop(t *testing.T, rig *oauthRouterRig, sessionID pgtype.UUID, cookie string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rig.server.URL+"/api/sessions/"+sessionID.String()+"/stop", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST stop: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

// requestedAtField matches a StopSessionResponse's requestedAt, the one
// field two stops made at different instants never share.
var requestedAtField = regexp.MustCompile(`"requestedAt":"[^"]*"`)

// stopBodyByRole writes body's session ids as their roles and its
// requestedAt as a placeholder, textually -- the bytes otherwise untouched
// -- after checking the instant is a real one.
func stopBodyByRole(t *testing.T, body string, ids []roleID) string {
	t.Helper()
	var decoded restdtos.StopSessionResponse
	if err := json.Unmarshal([]byte(body), &decoded); err != nil || decoded.RequestedAt.IsZero() {
		t.Fatalf("stop answer %s (err %v), want a StopSessionResponse with an instant", body, err)
	}
	for _, r := range ids {
		body = strings.ReplaceAll(body, r.id.String(), "<"+r.role+">")
	}
	return requestedAtField.ReplaceAllString(body, `"requestedAt":"<requestedAt>"`)
}

// stopState is what a stop left on one session tree, with ids written as
// their roles; the MCP grant stamps on the audit rows are taken out and
// returned apart.
type stopState struct {
	Sessions  []string
	Turns     []string
	Synthetic []string
	Timers    []string
	Audit     []string
}

// readStopState reads stopState for the tree's sessions, as audited for
// actor.
func readStopState(ctx context.Context, t *testing.T, rig *oauthRouterRig, tr stopTree, actor pgtype.UUID) (stopState, []map[string]any) {
	t.Helper()
	ids := tr.sessions()
	byRole := func(s string) string {
		for _, r := range ids {
			s = strings.ReplaceAll(s, r.id.String(), "<"+r.role+">")
		}
		return s
	}
	var st stopState
	var stamps []map[string]any
	for _, r := range ids {
		var status, reason string
		var flagged bool
		if err := rig.pool.QueryRow(ctx, `SELECT status::text, coalesce(failure_reason::text, ''), stop_requested_at IS NOT NULL FROM sessions WHERE id = $1`, r.id).Scan(&status, &reason, &flagged); err != nil {
			t.Fatalf("read the %s session: %v", r.role, err)
		}
		st.Sessions = append(st.Sessions, fmt.Sprintf("%s status=%s reason=%q flagged=%v", r.role, status, reason, flagged))

		st.Turns = append(st.Turns, collectRows(ctx, t, rig, func(row pgx.Rows) (string, error) {
			var tstatus string
			var tflagged bool
			err := row.Scan(&tstatus, &tflagged)
			return fmt.Sprintf("%s %s flagged=%v", r.role, tstatus, tflagged), err
		}, `SELECT status::text, stop_requested_at IS NOT NULL FROM turns WHERE session_id = $1 ORDER BY created_at`, r.id)...)

		var synthetic int
		if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'execution_complete' AND (payload->>'synthetic')::boolean`, r.id).Scan(&synthetic); err != nil {
			t.Fatalf("count the %s synthetic events: %v", r.role, err)
		}
		st.Synthetic = append(st.Synthetic, fmt.Sprintf("%s %d", r.role, synthetic))

		for _, name := range collectRows(ctx, t, rig, func(row pgx.Rows) (string, error) {
			var name string
			err := row.Scan(&name)
			return name, err
		}, `SELECT name FROM session_timers WHERE session_id = $1 ORDER BY name`, r.id) {
			st.Timers = append(st.Timers, r.role+" "+name)
		}

		type auditRow struct {
			resourceType string
			actorID      pgtype.UUID
			raw          []byte
		}
		for _, a := range collectRows(ctx, t, rig, func(row pgx.Rows) (auditRow, error) {
			var a auditRow
			err := row.Scan(&a.resourceType, &a.actorID, &a.raw)
			return a, err
		}, `SELECT resource_type, actor_user_id, detail_json FROM audit_log WHERE action = 'session.stop' AND resource_id = $1 ORDER BY created_at`, r.id.String()) {
			var detail map[string]any
			if err := json.Unmarshal(a.raw, &detail); err != nil {
				t.Fatalf("decode audit detail %s: %v", a.raw, err)
			}
			stamp, _ := detail["mcp"].(map[string]any)
			stamps = append(stamps, stamp)
			delete(detail, "mcp")
			canon, _ := json.Marshal(detail)
			st.Audit = append(st.Audit, fmt.Sprintf("session.stop %s <%s> actor=%v %s", a.resourceType, r.role, a.actorID == actor, byRole(string(canon))))
		}
	}
	return st, stamps
}

// collectRows runs query on rig's pool and scans every row with scan,
// returning them only once the rows are closed and their error checked:
// the caller asserts on a slice, never inside an open result set. A
// t.Fatal with rows still open would keep a pool connection acquired, and
// the rig's pool.Close cleanup would then wait on it for ever -- the
// package timing out instead of failing with its message.
func collectRows[T any](ctx context.Context, t *testing.T, rig *oauthRouterRig, scan func(pgx.Rows) (T, error), query string, args ...any) []T {
	t.Helper()
	rows, err := rig.pool.Query(ctx, query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	var out []T
	var scanErr error
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			scanErr = err
			break
		}
		out = append(out, v)
	}
	rows.Close()
	if scanErr != nil {
		t.Fatalf("scan a row of %q: %v", query, scanErr)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return out
}

// sameStopState fails the test unless mcp and rest are the same state.
func sameStopState(t *testing.T, stage string, mcp, rest stopState) {
	t.Helper()
	a, _ := json.Marshal(mcp)
	b, _ := json.Marshal(rest)
	if string(a) != string(b) {
		t.Fatalf("%s: the database differs after the MCP and the REST stop:\nMCP:  %s\nREST: %s", stage, a, b)
	}
}

// openTurnsOf counts the turns of the tree's sessions still pending,
// dispatched or processing.
func openTurnsOf(ctx context.Context, t *testing.T, rig *oauthRouterRig, tr stopTree) int {
	t.Helper()
	var ids []pgtype.UUID
	for _, r := range tr.sessions() {
		ids = append(ids, r.id)
	}
	return rig.countOf(ctx, t, `SELECT count(*) FROM turns WHERE session_id = ANY($1) AND status IN ('pending', 'dispatched', 'processing')`, ids)
}

// waitStopped waits until no turn of any of trees is open: each accepted
// stop's actors have cancelled what it flagged.
func waitStopped(ctx context.Context, t *testing.T, rig *oauthRouterRig, trees ...stopTree) {
	t.Helper()
	waitFor(t, 20*time.Second, "every flagged turn cancelled by its session's actor", func() bool {
		for _, tr := range trees {
			if openTurnsOf(ctx, t, rig, tr) != 0 {
				return false
			}
		}
		return true
	})
}

// sdkStopSession is TestOAuth_ProductionRouter's StopSession_SDKClient: a
// member stops a session they started, which started a child, over MCP;
// the same member stops a tree seeded alike by cookie. The two answers are
// the same bytes once ids and the instant are written as their roles -- the
// MCP text block and structured content being the twin's body verbatim --
// and once each tree's actors have cancelled what the stop flagged, the two
// trees' states are the same: both sessions flagged, both turns cancelled
// with failure reason cancelled and one synthetic terminal event each, no
// timer left, and two session.stop audit rows by the member, the child's
// naming the parent it was reached through. Every MCP row carries the
// grant, the child's included; no REST row does.
func sdkStopSession(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, nil)
	mcpTree := seedStopTree(ctx, t, rig, flow.member.ID, true)
	restTree := seedStopTree(ctx, t, rig, flow.member.ID, true)

	res := callStop(ctx, t, flow.session, mcpTree.parent)
	restStatus, restBody := restStop(t, rig, restTree.parent, flow.cookie)
	if restStatus != http.StatusAccepted || res.IsError {
		t.Fatalf("REST %d %s, MCP %+v -- want both accepted", restStatus, restBody, res.Content)
	}
	text := resultText(t, res)
	var fromText, fromStructured any
	_, structuredBytes := structured(t, res)
	if json.Unmarshal([]byte(text), &fromText) != nil || json.Unmarshal(structuredBytes, &fromStructured) != nil {
		t.Fatalf("text block %s, structured content %s: want both JSON", text, structuredBytes)
	}
	if a, b := fmt.Sprint(fromText), fmt.Sprint(fromStructured); a != b {
		t.Fatalf("structured content %s differs from the text block %s: both are the twin's body", structuredBytes, text)
	}
	mcpBody := stopBodyByRole(t, text, mcpTree.sessions())
	restCanon := stopBodyByRole(t, string(restBody), restTree.sessions())
	if mcpBody != restCanon {
		t.Fatalf("the MCP and REST answers differ beyond ids and the instant:\nMCP:  %s\nREST: %s", mcpBody, restCanon)
	}
	if want := `{"openTurns":2,"reachedSessionIds":["<parent>","<child>"],"requestedAt":"<requestedAt>","sessionId":"<parent>"}` + "\n"; mcpBody != want {
		t.Fatalf("the MCP answer %s, want %s", mcpBody, want)
	}

	waitStopped(ctx, t, rig, mcpTree, restTree)
	mcpState, mcpStamps := readStopState(ctx, t, rig, mcpTree, flow.member.ID)
	restState, restStamps := readStopState(ctx, t, rig, restTree, flow.member.ID)
	sameStopState(t, "a member's own session and its child", mcpState, restState)
	assertMCPStamps(ctx, t, rig, flow, "a member's own session and its child", mcpStamps, restStamps)
	want := stopState{
		Sessions:  []string{`parent status=cancelled reason="cancelled" flagged=true`, `child status=cancelled reason="cancelled" flagged=true`},
		Turns:     []string{"parent cancelled flagged=true", "child cancelled flagged=true"},
		Synthetic: []string{"parent 1", "child 1"},
		Audit: []string{
			`session.stop session <parent> actor=true {"open_turns":1}`,
			`session.stop session <child> actor=true {"open_turns":1,"requested_session_id":"<parent>","via_parent_session_id":"<parent>"}`,
		},
	}
	a, _ := json.Marshal(mcpState)
	b, _ := json.Marshal(want)
	if string(a) != string(b) {
		t.Fatalf("the state after the stop:\n got  %s\n want %s", a, b)
	}
	if len(mcpStamps) != 2 {
		t.Fatalf("%d audit rows over MCP, want the parent's and the child's", len(mcpStamps))
	}
}

// sdkStopSessionParityEveryRole is TestOAuth_ProductionRouter's
// StopSession_ParityEveryRole_SDKClient (technical plan §13.3, owner
// decision O1): for every role, a stop over MCP and one by the same user's
// cookie over REST -- each on its own tree, seeded alike: a session with a
// queued turn and a child with one -- answer the same (the body with ids
// and the instant as their roles, or the same refusal text) and leave the
// same database state. The session is one the user started, one they
// joined, one another member started, or a pull request's review session
// the user started. An admin or maintainer stops any of them, a member only
// the first two -- never a review session, which every review of its pull
// request shares -- and a viewer none. A refused stop writes nothing: no
// flag, no timer, no audit row, the turns still queued. An accepted one
// reaches the child too, authorized by the check on the session named.
func sdkStopSessionParityEveryRole(t *testing.T, rig *oauthRouterRig) {
	var prNumber int32
	for _, role := range []sqlcgen.UserRole{sqlcgen.UserRoleAdmin, sqlcgen.UserRoleMaintainer, sqlcgen.UserRoleMember, sqlcgen.UserRoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			ctx := oauthTestCtx(t)
			flow := rig.connectSDKClientAs(ctx, t, role, nil)
			other, _ := createRouterUser(ctx, t, rig.pool, sqlcgen.UserRoleMember)
			for _, relation := range []string{"own", "joined", "another member's", "own review"} {
				stage := fmt.Sprintf("%s, %s session", role, relation)
				creator := other.ID
				if relation == "own" || relation == "own review" {
					creator = flow.member.ID
				}
				seed := func() stopTree {
					tr := seedStopTree(ctx, t, rig, creator, true)
					switch relation {
					case "joined":
						if _, err := narvipg.NewParticipantStore(rig.pool).Create(ctx, tr.parent, flow.member.ID); err != nil {
							t.Fatalf("join the session: %v", err)
						}
					case "own review":
						prNumber++
						if _, err := rig.pool.Exec(ctx, `INSERT INTO github_pr_sessions (repo_full_name, pr_number, session_id) VALUES ('example-org/stop-parity', $1, $2)`, prNumber, tr.parent); err != nil {
							t.Fatalf("make it a review session: %v", err)
						}
					}
					return tr
				}
				mcpTree, restTree := seed(), seed()
				allowed := role == sqlcgen.UserRoleAdmin || role == sqlcgen.UserRoleMaintainer ||
					(role == sqlcgen.UserRoleMember && (relation == "own" || relation == "joined"))

				res := callStop(ctx, t, flow.session, mcpTree.parent)
				restStatus, restBody := restStop(t, rig, restTree.parent, flow.cookie)
				if allowed {
					if restStatus != http.StatusAccepted || res.IsError {
						t.Fatalf("%s: REST %d %s, MCP isError %v %+v -- want both accepted", stage, restStatus, restBody, res.IsError, res.Content)
					}
					if a, b := stopBodyByRole(t, resultText(t, res), mcpTree.sessions()), stopBodyByRole(t, string(restBody), restTree.sessions()); a != b {
						t.Fatalf("%s: MCP %s, REST %s -- want the same answer, ids and instant aside", stage, a, b)
					}
					waitStopped(ctx, t, rig, mcpTree, restTree)
				} else {
					var refused restError
					_ = json.Unmarshal(restBody, &refused)
					if restStatus != http.StatusForbidden || refused.Error != stopForbidden || !res.IsError || resultText(t, res) != stopForbidden {
						t.Fatalf("%s: REST %d %q, MCP isError %v %+v -- want both refused with %q", stage, restStatus, refused.Error, res.IsError, res.Content, stopForbidden)
					}
				}

				mcpState, mcpStamps := readStopState(ctx, t, rig, mcpTree, flow.member.ID)
				restState, restStamps := readStopState(ctx, t, rig, restTree, flow.member.ID)
				sameStopState(t, stage, mcpState, restState)
				assertMCPStamps(ctx, t, rig, flow, stage, mcpStamps, restStamps)
				wantTurn, wantAudit := "pending flagged=false", 0
				if allowed {
					wantTurn, wantAudit = "cancelled flagged=true", 2
				}
				if len(mcpState.Audit) != wantAudit || len(mcpState.Turns) != 2 || !strings.HasPrefix(mcpState.Turns[0], "parent "+wantTurn) || !strings.HasPrefix(mcpState.Turns[1], "child "+wantTurn) || len(mcpState.Timers) != 0 {
					t.Fatalf("%s: state %+v, want both turns %s, no timer and %d audit row(s)", stage, mcpState, wantTurn, wantAudit)
				}
			}
		})
	}
}

// sdkStopSessionReadGrant is TestOAuth_ProductionRouter's
// StopSession_ReadGrantDoesNotSeeIt: the member keeps mcp:read alone, so
// tools/list does not name narvi_stop_session, the SDK's call to it fails,
// a raw call to it on the member's own session answers exactly an unknown
// tool's bytes (the name the caller sent aside), and nothing is written:
// the session and its turn are not flagged, no stop timer is armed and no
// session.stop is audited.
func sdkStopSessionReadGrant(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, func(d *consentDriver) {
		d.keepScopes = func([]string) []string { return []string{"mcp:read"} }
	})
	for _, name := range toolNames(ctx, t, flow.session) {
		if name == "narvi_stop_session" {
			t.Fatal("tools/list names narvi_stop_session under a grant without mcp:write")
		}
	}
	tr := seedStopTree(ctx, t, rig, flow.member.ID, false)
	if _, err := flow.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "narvi_stop_session", Arguments: map[string]any{"sessionId": tr.parent.String()}}); err == nil {
		t.Fatal("calling the hidden stop tool through the SDK succeeded")
	}
	hiddenToken := flow.recorder.lastBearer()
	other, _ := createRouterUser(ctx, t, rig.pool, sqlcgen.UserRoleMember)
	full := mintBuildBearer(ctx, t, rig.pool, rig.cfg, other.ID)
	arguments := fmt.Sprintf(`{"sessionId":%q}`, tr.parent.String())
	unknownStatus, _, unknown := rig.rawCall(t, full, "narvi_does_not_exist", arguments)
	hiddenStatus, _, hidden := rig.rawCall(t, hiddenToken, "narvi_stop_session", arguments)
	if want := strings.ReplaceAll(string(unknown), "narvi_does_not_exist", "narvi_stop_session"); hiddenStatus != unknownStatus || string(hidden) != want {
		t.Fatalf("the hidden stop tool answers differently from an unknown tool:\n hidden:  %d %s\n unknown: %d %s", hiddenStatus, hidden, unknownStatus, want)
	}
	st, _ := readStopState(ctx, t, rig, tr, flow.member.ID)
	if strings.Join(st.Sessions, ",") != `parent status=created reason="" flagged=false` || strings.Join(st.Turns, ",") != "parent pending flagged=false" || len(st.Timers) != 0 || len(st.Audit) != 0 {
		t.Fatalf("a grant without mcp:write left %+v, want nothing written", st)
	}
}

// deref reads an optional string, "" for none.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// exitSandboxGen is the gen of the sandbox the row's exit seeds for the
// parent, and the one its fake agent connects with.
const exitSandboxGen = 3

// fakeProvider stands in for the sandbox provider's API: the row's exit
// builds its router with NARVI_MODAL_BASE_URL pointing here, so a sandbox
// spawn reaches this machine and nothing else. Every spawn gets a sandbox
// id, which then waits to connect; nothing else is supported.
type fakeProvider struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []string
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	p := &fakeProvider{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.calls = append(p.calls, r.Method+" "+r.URL.Path)
		p.mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sandboxId":"fake-provider-` + uuid.NewString() + `"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(p.server.Close)
	return p
}

// spawns counts the sandbox spawns the provider was asked for.
func (p *fakeProvider) spawns() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if c == "POST /v1/sandboxes" {
			n++
		}
	}
	return n
}

// fakeAgent is a sandbox agent connected to the router's own sandbox
// WebSocket (GET /sessions/{sessionID}/ws?type=sandbox), as the in-tree
// agent connects: it records every command frame, answers a `stop` with the
// cancelled execution_complete the agent sends once it has stopped its
// turn, and a `snapshot` with snapshot_ready. It never completes a turn on
// its own.
type fakeAgent struct {
	conn      *websocket.Conn
	sessionID string
	gen       int
	mu        sync.Mutex
	frames    []map[string]any
}

// connectFakeAgent connects a fakeAgent for sessionID's sandbox at gen.
// The connection is closed, and its reader waited for, when the test ends.
func connectFakeAgent(ctx context.Context, t *testing.T, rig *oauthRouterRig, sessionID pgtype.UUID, gen int) *fakeAgent {
	t.Helper()
	url := "ws" + strings.TrimPrefix(rig.server.URL, "http") + "/sessions/" + sessionID.String() + "/ws?type=sandbox"
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{
		"Authorization": {"Bearer fake-agent-token"},
		"X-Sandbox-ID":  {"fake-sandbox"},
		"X-Sandbox-Gen": {strconv.Itoa(gen)},
	}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("connect the fake agent: %v", err)
	}
	a := &fakeAgent{conn: conn, sessionID: sessionID.String(), gen: gen}
	readCtx, cancel := context.WithCancel(context.Background())
	var group errgroup.Group
	group.Go(func() error { return a.read(readCtx) })
	t.Cleanup(func() {
		cancel()
		_ = conn.Close(websocket.StatusNormalClosure, "test over")
		if err := group.Wait(); err != nil {
			t.Errorf("fake agent: %v", err)
		}
	})
	return a
}

// read handles command frames until the connection ends.
func (a *fakeAgent) read(ctx context.Context) error {
	for {
		_, data, err := a.conn.Read(ctx)
		if err != nil {
			return nil
		}
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err != nil {
			return fmt.Errorf("undecodable command frame %s: %w", data, err)
		}
		a.mu.Lock()
		a.frames = append(a.frames, frame)
		a.mu.Unlock()
		commandID, _ := frame["messageId"].(string)
		var reply any
		switch frame["type"] {
		case "stop":
			messageID := uuid.NewString()
			reply = sandboxws.ExecutionComplete{
				Type: "execution_complete", MessageId: messageID, SessionId: a.sessionID, Gen: a.gen,
				AckId: "execution_complete:" + messageID, Outcome: sandboxws.ExecutionCompleteOutcomeCancelled,
			}
		case "snapshot":
			messageID := uuid.NewString()
			reply = sandboxws.SnapshotReady{
				Type: "snapshot_ready", MessageId: messageID, SessionId: a.sessionID, Gen: a.gen,
				AckId: "snapshot_ready:" + messageID, CommandMessageId: &commandID, SnapshotId: "snapshot-" + messageID,
			}
		}
		if reply == nil {
			continue
		}
		raw, err := json.Marshal(reply)
		if err != nil {
			return err
		}
		if err := a.conn.Write(ctx, websocket.MessageText, raw); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("answer %s: %w", frame["type"], err)
		}
	}
}

// ofType returns the command frames of type typ received so far.
func (a *fakeAgent) ofType(typ string) []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []map[string]any
	for _, f := range a.frames {
		if f["type"] == typ {
			out = append(out, f)
		}
	}
	return out
}

// fixCodeHost stands in for the code host the sentinel auto-fix notifier
// creates the fix session's branch on: it resolves every branch to one
// commit and creates any branch. Nothing else is called on this path; the
// embedded interface is nil, so anything else would fail the test loudly.
type fixCodeHost struct {
	ports.SourceControl
	mu       sync.Mutex
	branches []string
}

func (h *fixCodeHost) ResolveBranchSHA(_ context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	return "5f3c0a9b8e7d6c5b4a39281706f5e4d3c2b1a098", spec.Branch, nil
}

func (h *fixCodeHost) CreateBranch(_ context.Context, spec ports.CreateBranchSpec) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.branches = append(h.branches, spec.Branch)
	return nil
}

// newExitRouterRig is the row's exit's own router: a createRouterRig whose
// sandbox provider is fake (its API a local server, so a spawn never leaves
// this machine) and whose bounded wait polls every 100 ms. The shared
// NARVI_MODAL_BASE_URL is restored for every router built after it.
func newExitRouterRig(t *testing.T, connStr string) (*oauthRouterRig, *fakeProvider) {
	t.Helper()
	provider := newFakeProvider(t)
	shipped := os.Getenv("NARVI_MODAL_BASE_URL")
	rig := createRouterRigWith(t, connStr, map[string]string{"NARVI_MODAL_BASE_URL": provider.server.URL}, waitTestTimeouts)
	t.Setenv("NARVI_MODAL_BASE_URL", shipped)
	if rig.cfg.ModalBaseURL != provider.server.URL {
		t.Fatalf("the router's provider is %q, want the fake at %q", rig.cfg.ModalBaseURL, provider.server.URL)
	}
	return rig, provider
}

// spawnFixChild starts a child session of parentID through the sentinel
// auto-fix notifier -- the one path in this codebase that starts a child
// session -- built as controlplane builds it, on the router's own stores
// and session registry, with a fake code host for the fix branch and the
// repository live. Its Deliver claims the fix, creates the child with its
// first turn queued, in one transaction that reads the parent FOR SHARE,
// and asks the child's actor to dispatch.
func spawnFixChild(ctx context.Context, t *testing.T, rig *oauthRouterRig, parentID pgtype.UUID) pgtype.UUID {
	t.Helper()
	pool := rig.pool
	const repoFullName, prNumber, headBranch = "example-org/stop-exit", 7, "feature/stop-exit"
	fixes, findings := narvipg.NewSentinelFixStore(pool), narvipg.NewReviewFindingStore(pool)
	fix, err := fixes.Claim(ctx, repoFullName, prNumber, parentID, headBranch)
	if err != nil {
		t.Fatalf("claim the sentinel fix: %v", err)
	}
	const identityHash = "7e1d2c3b4a5968778695a4b3c2d1e0f9e8d7c6b5a4938271605f4e3d2c1b0a99"
	if _, err := findings.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
		RepoFullName: repoFullName, PrNumber: prNumber, IdentityHash: identityHash,
		Severity: "medium", FilePath: "internal/retry/retry.go", Description: "Missing test coverage for the backoff.",
	}); err != nil {
		t.Fatalf("record the finding: %v", err)
	}
	notifier := outboxworker.NewSentinelAutoFixNotifier(pool, narvipg.NewSessionStore(pool), narvipg.NewTurnStore(pool), narvipg.NewEnvironmentStore(pool), narvipg.NewAuditLogStore(pool), rig.app.registry,
		fixes, findings, &fixCodeHost{}, rig.cfg.GitHubBotToken, rig.cfg.Timeouts, rig.cfg.EpistemicCheckDefault, rig.cfg.RolloutMode, narvipg.NewRepoSettingsStore(pool), narvipg.NewGitHubPRSessionStore(pool),
		func(context.Context, string) bool { return true }, narvipg.NewShadowSCMWriteStore(pool))
	payload, err := json.Marshal(ports.SentinelAutoFixPayload{
		SentinelFixID: fix.ID.String(), RepoFullName: repoFullName, OriginPRNumber: prNumber,
		OriginReviewSessionID: parentID.String(), OriginHeadBranch: headBranch,
		RepoName: "widgets", RepoCloneURL: "https://github.com/" + repoFullName + ".git",
		FindingIdentityHashes: []string{identityHash}, FindingDescriptions: []string{"Missing test coverage for the backoff."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindSentinelAutoFix, Payload: payload}); err != nil {
		t.Fatalf("Deliver the sentinel auto-fix: %v", err)
	}
	var child pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT fix_child_session_id FROM sentinel_fixes WHERE id = $1`, fix.ID).Scan(&child); err != nil || !child.Valid {
		t.Fatalf("the fix's child session: %v (err %v), want one", child, err)
	}
	return child
}

// descendantOpenTurns lists, for rootID and every session it started,
// however deep (parent_session_id, recursively, read from Postgres -- not
// from any answer), the turns still pending, dispatched or processing, and
// counts the sessions that tree holds.
func descendantOpenTurns(ctx context.Context, t *testing.T, rig *oauthRouterRig, rootID pgtype.UUID) (open []string, sessions int) {
	t.Helper()
	const tree = `WITH RECURSIVE tree(id) AS (SELECT $1::uuid UNION SELECT s.id FROM sessions s JOIN tree ON s.parent_session_id = tree.id)`
	open = collectRows(ctx, t, rig, func(row pgx.Rows) (string, error) {
		var sessionID, turnID, status string
		err := row.Scan(&sessionID, &turnID, &status)
		return fmt.Sprintf("session %s turn %s %s", sessionID, turnID, status), err
	}, tree+` SELECT t.session_id::text, t.id::text, t.status::text FROM turns t JOIN tree ON t.session_id = tree.id WHERE t.status IN ('pending', 'dispatched', 'processing') ORDER BY t.created_at`, rootID)
	return open, rig.countOf(ctx, t, tree+` SELECT count(*) FROM tree`, rootID)
}

// turnStatus reads one turn's status.
func turnStatus(ctx context.Context, t *testing.T, rig *oauthRouterRig, turnID pgtype.UUID) string {
	t.Helper()
	var status string
	if err := rig.pool.QueryRow(ctx, `SELECT status::text FROM turns WHERE id = $1`, turnID).Scan(&status); err != nil {
		t.Fatalf("read turn %s: %v", turnID.String(), err)
	}
	return status
}

// planStatus reads one plan's status.
func planStatus(ctx context.Context, t *testing.T, rig *oauthRouterRig, planID pgtype.UUID) string {
	t.Helper()
	var status string
	if err := rig.pool.QueryRow(ctx, `SELECT status::text FROM plans WHERE id = $1`, planID).Scan(&status); err != nil {
		t.Fatalf("read plan %s: %v", planID.String(), err)
	}
	return status
}

// sdkPlanRevisionThenStopNoOrphan is TestOAuth_ProductionRouter's
// PlanRevisionThenStop_NoOrphan_SDKClient, row 183's exit, every step
// through the official SDK client on the production router:
//
//  1. A member's session has a plan awaiting approval and a sandbox whose
//     agent is connected to the router's own sandbox WebSocket; the plan is
//     approved over MCP.
//  2. The actor dispatches the implementation to that agent: the turn is
//     processing and the agent has its prompt.
//  3. A revision requested over MCP is refused with REST's 409 text; the
//     implementation is still processing, the plan still approved, and no
//     turn was queued.
//  4. The sentinel auto-fix notifier starts a child of the session; before
//     the stop, the child has its own first turn queued and its actor has
//     asked the provider for its sandbox -- real open work.
//  5. narvi_stop_session on the parent is accepted.
//  6. The agent receives `stop` for its gen and confirms it; narvi_wait_for_
//     session settles for every session the stop says it reached.
//  7. In Postgres, across the parent and every session it started however
//     deep -- read from the tree, never from the answer -- no turn is
//     pending, dispatched or processing. Only then is the answer checked:
//     it named the parent and the child as reached, with both open turns.
//     The implementation ended cancelled with nothing pushed, the child's
//     turn cancelled with its synthetic terminal event, the plan is still
//     approved, and both stop audit rows carry the grant.
func sdkPlanRevisionThenStopNoOrphan(t *testing.T, rig *oauthRouterRig, provider *fakeProvider) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, nil)
	grant := rig.grantOf(ctx, t, flow.member.ID)

	// 1. The plan, the sandbox and its agent, then the approval.
	parent, plan := seedPlannedSessionFrom(ctx, t, rig, flow.member.ID, sqlcgen.PlanStatusAwaitingApproval, sqlcgen.SessionSpawnSourceMcp)
	if _, err := narvipg.NewSandboxStore(rig.pool).Create(ctx, parent); err != nil {
		t.Fatalf("create the sandbox: %v", err)
	}
	if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET status = 'ready', gen = $2, provider_id = 'fake-provider-parent' WHERE session_id = $1`, parent, exitSandboxGen); err != nil {
		t.Fatalf("make the sandbox ready: %v", err)
	}
	agent := connectFakeAgent(ctx, t, rig, parent, exitSandboxGen)
	approved := callTool(ctx, t, flow.session, "narvi_approve_plan", map[string]any{"sessionId": parent.String(), "planId": plan.String()})
	if approved.IsError {
		t.Fatalf("approve over MCP: %+v", approved.Content)
	}
	approval, _ := structured(t, approved)
	implementationID, _ := approval["turnId"].(string)
	var implementation pgtype.UUID
	if err := implementation.Scan(implementationID); err != nil || approval["status"] != "approved" {
		t.Fatalf("the approval answered %v, want approved with the implementation turn", approval)
	}

	// 2. The implementation runs on the agent.
	waitFor(t, 20*time.Second, "the implementation dispatched to the agent", func() bool {
		return turnStatus(ctx, t, rig, implementation) == "processing" && len(agent.ofType("prompt")) == 1
	})

	// 3. A revision mid-implementation is refused, and changes nothing.
	revision := callTool(ctx, t, flow.session, "narvi_request_plan_revision", map[string]any{"sessionId": parent.String(), "feedback": "keep the env fallback after all"})
	if !revision.IsError || resultText(t, revision) != busyRefusal {
		t.Fatalf("a revision mid-implementation: %+v, want isError %q", revision.Content, busyRefusal)
	}
	if turns, audited := sessionTurns(ctx, t, rig, parent); strings.Join(turns, ", ") != "completed plan_mode=true, processing plan_mode=false" || audited != 0 {
		t.Fatalf("after the refused revision the turns are %v with %d turn.create audit row(s), want the plan's and the running implementation alone", turns, audited)
	}
	if status := planStatus(ctx, t, rig, plan); status != "approved" {
		t.Fatalf("after the refused revision the plan is %s, want still approved", status)
	}

	// 4. A child with work of its own.
	spawnsBefore := provider.spawns()
	child := spawnFixChild(ctx, t, rig, parent)
	var childTurn pgtype.UUID
	if err := rig.pool.QueryRow(ctx, `SELECT id FROM turns WHERE session_id = $1`, child).Scan(&childTurn); err != nil {
		t.Fatalf("the child's first turn: %v", err)
	}
	waitFor(t, 20*time.Second, "the child's actor asking the provider for its sandbox", func() bool {
		return provider.spawns() == spawnsBefore+1
	})
	if open, sessions := descendantOpenTurns(ctx, t, rig, parent); sessions != 2 || len(open) != 2 || turnStatus(ctx, t, rig, childTurn) != "pending" || turnStatus(ctx, t, rig, implementation) != "processing" {
		t.Fatalf("before the stop: %d session(s) in the tree with open turns %v; want the parent's implementation processing and the child's turn pending", sessions, open)
	}

	// 5. The stop, over MCP, on the parent.
	stopped := callStop(ctx, t, flow.session, parent)
	if stopped.IsError {
		t.Fatalf("stop over MCP: %+v", stopped.Content)
	}
	var answer restdtos.StopSessionResponse
	if err := json.Unmarshal([]byte(resultText(t, stopped)), &answer); err != nil || answer.SessionId != parent.String() {
		t.Fatalf("the stop answered %s (err %v), want the parent's StopSessionResponse", resultText(t, stopped), err)
	}

	// 6. The agent is told to stop and confirms; every reached session
	// settles.
	waitFor(t, 20*time.Second, "the agent told to stop", func() bool { return len(agent.ofType("stop")) == 1 })
	if gen, _ := agent.ofType("stop")[0]["gen"].(float64); int(gen) != exitSandboxGen {
		t.Fatalf("the stop command names gen %v, want the sandbox's %d", gen, exitSandboxGen)
	}
	for _, id := range answer.ReachedSessionIds {
		waited := decodeWaitResult(t, callTool(ctx, t, flow.session, "narvi_wait_for_session", map[string]any{"sessionId": id}))
		if !waited.Settled || waited.Wait.Reason != restdtos.SessionActivityWaitReasonSettled {
			t.Fatalf("narvi_wait_for_session on %s: %+v (wait %+v), want settled", id, waited, waited.Wait)
		}
	}

	// 7. No orphan: nothing open anywhere below the parent, read from
	// Postgres -- whatever the answer said it reached, which is checked
	// only afterwards.
	if open, sessions := descendantOpenTurns(ctx, t, rig, parent); len(open) != 0 || sessions != 2 {
		t.Fatalf("after the stop settled: %d session(s) in the tree, open turns %v -- want none open", sessions, open)
	}
	if strings.Join(answer.ReachedSessionIds, ",") != parent.String()+","+child.String() || answer.OpenTurns != 2 {
		t.Fatalf("the stop answered %+v, want the parent then the child reached, with 2 open turns", answer)
	}
	if status := turnStatus(ctx, t, rig, implementation); status != "cancelled" {
		t.Fatalf("the implementation ended %s, want cancelled", status)
	}
	var parentReason string
	if err := rig.pool.QueryRow(ctx, `SELECT coalesce(failure_reason::text, '') FROM sessions WHERE id = $1`, parent).Scan(&parentReason); err != nil || parentReason != "cancelled" {
		t.Fatalf("the parent's failure reason is %q (err %v), want cancelled", parentReason, err)
	}
	if pushes := agent.ofType("push"); len(pushes) != 0 {
		t.Fatalf("the agent was asked to push %d time(s) for a stopped implementation", len(pushes))
	}
	if status := turnStatus(ctx, t, rig, childTurn); status != "cancelled" {
		t.Fatalf("the child's turn is %s, want cancelled", status)
	}
	if n := rig.countOf(ctx, t, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'execution_complete' AND (payload->>'synthetic')::boolean AND payload->>'turn_id' = $2`, child, childTurn.String()); n != 1 {
		t.Fatalf("the child's cancelled turn has %d synthetic terminal event(s), want one", n)
	}
	if status := planStatus(ctx, t, rig, plan); status != "approved" {
		t.Fatalf("after the stop the plan is %s, want still approved", status)
	}
	type stopRow struct {
		resource string
		stamp    *string
	}
	var stamped []string
	for _, row := range collectRows(ctx, t, rig, func(r pgx.Rows) (stopRow, error) {
		var row stopRow
		err := r.Scan(&row.resource, &row.stamp)
		return row, err
	}, `SELECT resource_id, detail_json->'mcp'->>'grant_id' FROM audit_log WHERE action = 'session.stop' AND resource_id = ANY($1) ORDER BY resource_id`, []string{parent.String(), child.String()}) {
		if row.stamp == nil || *row.stamp != grant {
			t.Fatalf("the session.stop row of %s carries grant %v, want %s", row.resource, deref(row.stamp), grant)
		}
		stamped = append(stamped, row.resource)
	}
	want := []string{parent.String(), child.String()}
	sort.Strings(want)
	if strings.Join(stamped, ",") != strings.Join(want, ",") {
		t.Fatalf("session.stop audit rows for %v, want one each for the parent and the child", stamped)
	}
}
