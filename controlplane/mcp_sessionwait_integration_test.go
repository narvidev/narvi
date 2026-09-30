//go:build integration

// Row 182's piece (b) on the production router (technical plan §43.20):
// narvi_wait_for_session, the bounded wait, over the status route with
// ?waitSeconds=, driven through the official SDK client under a real
// mcp:read grant on the router Build returns. The row's own exit is proved
// here: a wait returns on a real terminal state -- a turn really completed,
// a plan really awaiting approval -- and never on a queue that has not
// started, nor while a completed turn is still being delivered or work is
// scheduled; any replica serves it, because it wakes by reading Postgres;
// a long wait runs to its bound through the SDK with nothing cutting it;
// the shutdown of Run's own HTTP server interrupts every wait at once;
// over the per-grant cap, and over the per-user cap that spans every grant
// of one user and their browser, a wait degrades to the plain status,
// never an error; and a grant without mcp:read is told the tool does not
// exist.
//
// Every state change below is written the way the session actor writes it
// -- one transaction, the turn moved through turn.Transition -- straight to
// the database the routers share: no router's process takes part in the
// write, so no in-process event hub hears it, and a wait that settles has
// read it from Postgres.
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// waitTestTimeouts is liftEndpointBrakes (newOAuthRouterRig's reason) with
// the wait in test time: a read every 100 ms, for at most five seconds. A
// session result's live-read budget follows the wait bound down, since
// Validate keeps it below. Every other timeout is as shipped.
func waitTestTimeouts(to *platform.Timeouts) {
	liftEndpointBrakes(to)
	to.MCPWaitPollInterval = 100 * time.Millisecond
	to.MCPWaitMaxDuration = 5 * time.Second
	to.SessionResultLiveReadBudget = 4 * time.Second
}

// waitSeed writes session state through the stores, as the session actor
// and the release worker write it.
type waitSeed struct {
	pool      *pgxpool.Pool
	sessions  *narvipg.SessionStore
	turns     *narvipg.TurnStore
	plans     *narvipg.PlanStore
	sandboxes *narvipg.SandboxStore
	releases  *narvipg.ReleaseManifestPendingStore
}

func newWaitSeed(pool *pgxpool.Pool) waitSeed {
	return waitSeed{
		pool:      pool,
		sessions:  narvipg.NewSessionStore(pool),
		turns:     narvipg.NewTurnStore(pool),
		plans:     narvipg.NewPlanStore(pool),
		sandboxes: narvipg.NewSandboxStore(pool),
		releases:  narvipg.NewReleaseManifestPendingStore(pool),
	}
}

func (s waitSeed) session(ctx context.Context, t *testing.T, userID pgtype.UUID) pgtype.UUID {
	t.Helper()
	row, err := s.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: userID})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return row.ID
}

// processingTurn creates a turn and moves it pending -> dispatched ->
// processing along the turn machine's own edges.
func (s waitSeed) processingTurn(ctx context.Context, t *testing.T, sessionID pgtype.UUID, planMode bool) pgtype.UUID {
	t.Helper()
	row, err := s.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusPending, PlanMode: planMode})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	state := turn.StatePending
	for _, trig := range []turn.Trigger{turn.TriggerDispatch, turn.TriggerStartProcessing} {
		to, err := turn.Transition(state, trig)
		if err != nil {
			t.Fatalf("turn.Transition(%s, %s): %v", state, trig, err)
		}
		arg := sqlcgen.UpdateTurnStatusParams{ID: row.ID, Status: sqlcgen.TurnStatus(to)}
		if to == turn.StateDispatched {
			arg.DispatchedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
		}
		if _, err := s.turns.UpdateStatus(ctx, arg); err != nil {
			t.Fatalf("turn %s -> %s: %v", state, to, err)
		}
		state = to
	}
	return row.ID
}

// completeTurn ends a processing turn the way completeProcessingTurn does:
// in ONE transaction, the turn moved processing -> completed through
// turn.Transition, the session's row derived, and -- inside must write --
// anything the completion itself writes (a plan-mode turn's plan, the
// delivery stamp).
func (s waitSeed) completeTurn(ctx context.Context, t *testing.T, sessionID, turnID pgtype.UUID, inside func(pgx.Tx) error) {
	t.Helper()
	to, err := turn.Transition(turn.StateProcessing, turn.TriggerComplete)
	if err != nil {
		t.Fatalf("turn.Transition: %v", err)
	}
	if err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := s.turns.WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: turnID, Status: sqlcgen.TurnStatus(to), CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}}); err != nil {
			return err
		}
		if _, err := s.sessions.WithTx(tx).UpdateStatus(ctx, sqlcgen.UpdateSessionStatusParams{ID: sessionID, Status: sqlcgen.SessionStatusCompleted}); err != nil {
			return err
		}
		if inside != nil {
			return inside(tx)
		}
		return nil
	}); err != nil {
		t.Fatalf("complete the turn: %v", err)
	}
}

// pendingCall is one tool call running in the background.
type pendingCall struct {
	group      errgroup.Group
	res        *sdkmcp.CallToolResult
	answeredAt atomic.Int64
}

// startWait calls narvi_wait_for_session through s with args, in the
// background.
func startWait(ctx context.Context, s *sdkmcp.ClientSession, args map[string]any) *pendingCall {
	c := &pendingCall{}
	c.group.Go(func() error {
		res, err := s.CallTool(ctx, &sdkmcp.CallToolParams{Name: "narvi_wait_for_session", Arguments: args})
		c.answeredAt.Store(time.Now().UnixNano())
		if err != nil {
			return err
		}
		c.res = res
		return nil
	})
	return c
}

func (c *pendingCall) answered() bool { return c.answeredAt.Load() != 0 }

// result waits for the call and returns its SessionActivity and when it
// answered.
func (c *pendingCall) result(t *testing.T) (restdtos.SessionActivity, time.Time) {
	t.Helper()
	if err := c.group.Wait(); err != nil {
		t.Fatalf("CallTool narvi_wait_for_session: %v", err)
	}
	return decodeWaitResult(t, c.res), time.Unix(0, c.answeredAt.Load())
}

// decodeWaitResult decodes a successful wait result's text block -- the
// twin's own body -- through the generated DTO, and requires the wait
// object.
func decodeWaitResult(t *testing.T, res *sdkmcp.CallToolResult) restdtos.SessionActivity {
	t.Helper()
	var got restdtos.SessionActivity
	if err := json.Unmarshal(toolText(t, res), &got); err != nil {
		t.Fatalf("decode the wait result: %v", err)
	}
	if got.Wait == nil {
		t.Fatalf("wait result %+v carries no wait object", got)
	}
	return got
}

// stillWaiting fails unless c has not answered after polls poll intervals
// of rig's timeouts.
func stillWaiting(t *testing.T, rig *oauthRouterRig, c *pendingCall, polls int, stage string) {
	t.Helper()
	time.Sleep(time.Duration(polls) * rig.cfg.Timeouts.MCPWaitPollInterval)
	if c.answered() {
		res, _ := c.result(t)
		t.Fatalf("%s: the wait answered after %d polls with %+v (wait %+v), want it still waiting", stage, polls, res, res.Wait)
	}
}

// restStatus reads the plain status route with cookie through rig.
func restStatus(t *testing.T, rig *oauthRouterRig, sessionID pgtype.UUID, cookie string) restdtos.SessionActivity {
	t.Helper()
	code, body := rig.restRaw(t, "/api/sessions/"+sessionID.String()+"/status", cookie)
	var got restdtos.SessionActivity
	if code != http.StatusOK || json.Unmarshal(body, &got) != nil {
		t.Fatalf("GET status: %d %s", code, body)
	}
	return got
}

// answeredWithinAPoll fails unless answeredAt is at most one poll (and
// scheduling slack) after changedAt.
func answeredWithinAPoll(t *testing.T, rig *oauthRouterRig, changedAt, answeredAt time.Time) {
	t.Helper()
	if after := answeredAt.Sub(changedAt); after > rig.cfg.Timeouts.MCPWaitPollInterval+time.Second {
		t.Fatalf("the wait answered %v after the state changed, want within about one poll (%v)", after, rig.cfg.Timeouts.MCPWaitPollInterval)
	}
}

// sdkWaitReturnsOnRealTerminalState is TestOAuth_ProductionRouter's
// Wait_ReturnsOnRealTerminalState_SDKClient: a session whose one turn is
// processing -- its row still saying "created" -- keeps the wait blocked
// through three polls; the turn is completed as the actor completes it
// (one transaction, turn.Transition), and the wait answers within about a
// poll: finished, settled, reason "settled", having waited. Mutations: a
// wait that returns on its first read regardless fails the first check.
//
// The three polls are counted from the moment the server holds the wait
// (the replica's Waiter reports it active), never from the client's call:
// the request can reach the server a poll or more after it was sent, and
// the waited time the answer reports is the server's own.
func sdkWaitReturnsOnRealTerminalState(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, nil)
	seed := newWaitSeed(rig.pool)
	sid := seed.session(ctx, t, flow.member.ID)
	turnID := seed.processingTurn(ctx, t, sid, false)
	base := rig.app.sessionWaiter.Active()

	call := startWait(ctx, flow.session, map[string]any{"sessionId": sid.String(), "waitSeconds": 5})
	held := time.Now().Add(3 * time.Second)
	for rig.app.sessionWaiter.Active() <= base && !call.answered() && time.Now().Before(held) {
		time.Sleep(5 * time.Millisecond)
	}
	stillWaiting(t, rig, call, 3, "a processing turn")
	completedAt := time.Now()
	seed.completeTurn(ctx, t, sid, turnID, nil)
	got, answeredAt := call.result(t)
	answeredWithinAPoll(t, rig, completedAt, answeredAt)
	minWaited := int(3 * rig.cfg.Timeouts.MCPWaitPollInterval / time.Millisecond)
	if got.Activity != restdtos.SessionActivityActivityFinished || !got.Settled || got.Wait.Reason != restdtos.SessionActivityWaitReasonSettled || got.Wait.WaitedMs < minWaited {
		t.Fatalf("the wait = %+v (wait %+v), want finished, settled, reason settled, after at least %d ms", got, got.Wait, minWaited)
	}
	if got.LastRun == nil || got.LastRun.TurnId != turnID.String() || got.LastRun.Outcome != restdtos.SessionActivityLastRunOutcomeCompleted {
		t.Fatalf("lastRun = %+v, want the turn that completed", got.LastRun)
	}
}

// sdkWaitNeverReturnsOnAQueueThatHasNotStarted is
// TestOAuth_ProductionRouter's
// Wait_NeverReturnsOnAQueueThatHasNotStarted_SDKClient -- the row's own
// exit, its second half. Two queues that have not started, neither with a
// sandbox: a follow-up queued under a row that says "completed" (the lag
// §43.20 exists for), and a first turn queued under a row that says
// "created". Each wait runs its whole second and answers "timeout", queued
// and unsettled, one pending turn. Mutations: a settle predicate reading
// sessions.status answers the first at once; one treating "no turn in
// flight" as settled answers both at once.
func sdkWaitNeverReturnsOnAQueueThatHasNotStarted(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, nil)
	seed := newWaitSeed(rig.pool)
	lagging := seedLaggingSession(ctx, t, rig.pool, flow.member.ID, 0)
	fresh := seed.session(ctx, t, flow.member.ID)
	if _, err := seed.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: fresh, Status: sqlcgen.TurnStatusPending}); err != nil {
		t.Fatalf("queue a first turn: %v", err)
	}

	for _, tc := range []struct {
		name string
		id   pgtype.UUID
		row  string
	}{
		{"a follow-up queued under a completed row", lagging, `"status":"completed"`},
		{"a first turn queued under a created row", fresh, `"status":"created"`},
	} {
		start := time.Now()
		res, err := flow.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "narvi_wait_for_session", Arguments: map[string]any{"sessionId": tc.id.String(), "waitSeconds": 1}})
		took := time.Since(start)
		if err != nil {
			t.Fatalf("%s: CallTool: %v", tc.name, err)
		}
		got := decodeWaitResult(t, res)
		if took < time.Second || got.Wait.Reason != restdtos.SessionActivityWaitReasonTimeout || got.Wait.WaitedMs < 1000 {
			t.Fatalf("%s: answered after %v with wait %+v, want the whole second waited out, reason timeout", tc.name, took, got.Wait)
		}
		if got.Activity != restdtos.SessionActivityActivityQueued || got.Settled || got.PendingTurns != 1 || got.InFlightTurn != nil {
			t.Fatalf("%s: %+v, want queued, unsettled, one pending turn, none in flight", tc.name, got)
		}
		if code, row := rig.restRaw(t, "/api/sessions/"+tc.id.String(), flow.cookie); code != http.StatusOK || !strings.Contains(string(row), tc.row) {
			t.Fatalf("%s: GET session %d %s, want the row still saying %s", tc.name, code, row, tc.row)
		}
	}
}

// sdkWaitReturnsOnAwaitingApproval is TestOAuth_ProductionRouter's
// Wait_ReturnsOnAwaitingApproval_SDKClient: a plan-mode turn processing
// keeps the wait blocked; its completion and its plan awaiting approval
// commit in one transaction (recordPlanIfNeeded's shape), and the wait
// answers awaiting_approval -- settled, a person must act -- naming the
// plan. Mutation: a wait ignoring the gate runs to its bound.
func sdkWaitReturnsOnAwaitingApproval(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, nil)
	seed := newWaitSeed(rig.pool)
	sid := seed.session(ctx, t, flow.member.ID)
	turnID := seed.processingTurn(ctx, t, sid, true)

	call := startWait(ctx, flow.session, map[string]any{"sessionId": sid.String(), "waitSeconds": 5})
	stillWaiting(t, rig, call, 3, "a plan-mode turn processing")
	var plan sqlcgen.Plan
	completedAt := time.Now()
	seed.completeTurn(ctx, t, sid, turnID, func(tx pgx.Tx) error {
		var err error
		plan, err = seed.plans.WithTx(tx).Create(ctx, sqlcgen.CreatePlanParams{SessionID: sid, TurnID: turnID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval})
		return err
	})
	got, answeredAt := call.result(t)
	answeredWithinAPoll(t, rig, completedAt, answeredAt)
	if got.Activity != restdtos.SessionActivityActivityAwaitingApproval || !got.Settled || got.Wait.Reason != restdtos.SessionActivityWaitReasonSettled {
		t.Fatalf("the wait = %+v (wait %+v), want awaiting_approval, settled, reason settled", got, got.Wait)
	}
	if got.Awaiting == nil || got.Awaiting.Kind != restdtos.SessionActivityAwaitingKindPlan || got.Awaiting.Id != plan.ID.String() {
		t.Fatalf("awaiting = %+v, want the plan %s", got.Awaiting, plan.ID.String())
	}
}

// sdkWaitUnsettledThroughDeliveringAndScheduled is
// TestOAuth_ProductionRouter's
// Wait_UnsettledThroughDeliveringAndScheduled_SDKClient: neither a
// completed turn's delivery nor scheduled work ends a wait. The turn
// completes with its delivery stamped in the same transaction
// (delivering); a release manifest check is enqueued and then the delivery
// ends (scheduled); the check is claimed and finished without a turn
// (finished). The wait stays blocked through the first two, each confirmed
// by a plain status read while it waits, and answers only after the
// third. Mutation: "no turn in flight" read as settled answers at the
// first.
func sdkWaitUnsettledThroughDeliveringAndScheduled(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, nil)
	seed := newWaitSeed(rig.pool)
	sid := seed.session(ctx, t, flow.member.ID)
	if _, err := seed.sandboxes.Create(ctx, sid); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	turnID := seed.processingTurn(ctx, t, sid, false)

	call := startWait(ctx, flow.session, map[string]any{"sessionId": sid.String(), "waitSeconds": 5})
	seed.completeTurn(ctx, t, sid, turnID, func(tx pgx.Tx) error {
		return seed.sandboxes.WithTx(tx).StartPRDelivery(ctx, sid)
	})
	stillWaiting(t, rig, call, 3, "delivering")
	if got := restStatus(t, rig, sid, flow.cookie); got.Activity != restdtos.SessionActivityActivityDelivering {
		t.Fatalf("while delivering: the plain read says %q", got.Activity)
	}

	if _, err := seed.releases.Create(ctx, sqlcgen.CreateReleaseManifestPendingParams{
		SessionID: sid, Owner: "example", Repo: "wait-" + sid.String(), PrNumber: 1, BaseRef: "main", HeadRef: "release/x",
	}); err != nil {
		t.Fatalf("enqueue the release check: %v", err)
	}
	if err := seed.sandboxes.EndPRDelivery(ctx, sid); err != nil {
		t.Fatalf("end the delivery: %v", err)
	}
	stillWaiting(t, rig, call, 3, "scheduled")
	if got := restStatus(t, rig, sid, flow.cookie); got.Activity != restdtos.SessionActivityActivityScheduled {
		t.Fatalf("while scheduled: the plain read says %q", got.Activity)
	}

	// The worker's claim -- the pending row deleted and the check recorded
	// as running in one transaction -- then its finish, no turn inserted.
	var pendingID pgtype.UUID
	if err := pgx.BeginFunc(ctx, rig.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `DELETE FROM release_manifest_pending WHERE session_id = $1 RETURNING id`, sid).Scan(&pendingID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO release_manifest_checks_running (pending_id, session_id) VALUES ($1, $2)`, pendingID, sid)
		return err
	}); err != nil {
		t.Fatalf("claim the release check: %v", err)
	}
	stillWaiting(t, rig, call, 2, "the release check running")
	finishedAt := time.Now()
	if err := seed.releases.Finish(ctx, pendingID); err != nil {
		t.Fatalf("finish the release check: %v", err)
	}
	got, answeredAt := call.result(t)
	answeredWithinAPoll(t, rig, finishedAt, answeredAt)
	if got.Activity != restdtos.SessionActivityActivityFinished || !got.Settled || got.Wait.Reason != restdtos.SessionActivityWaitReasonSettled {
		t.Fatalf("the wait = %+v (wait %+v), want finished and settled once the delivery and the scheduled work are over", got, got.Wait)
	}
}

// sdkWaitParityWithTheRESTTwin is TestOAuth_ProductionRouter's
// Wait_BytesEqualTheRESTTwin_SDKClient: on a finished session the wait
// answers at once through either surface, and the tool's text is the REST
// twin's body for the member's cookie byte for byte, but for observedAt
// (each snapshot's own clock) -- wait object included.
func sdkWaitParityWithTheRESTTwin(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, nil)
	seed := newWaitSeed(rig.pool)
	sid := seed.session(ctx, t, flow.member.ID)
	seed.completeTurn(ctx, t, sid, seed.processingTurn(ctx, t, sid, false), nil)

	code, rest := rig.restRaw(t, "/api/sessions/"+sid.String()+"/status?waitSeconds=5", flow.cookie)
	if code != http.StatusOK {
		t.Fatalf("REST wait: %d %s", code, rest)
	}
	res, err := flow.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "narvi_wait_for_session", Arguments: map[string]any{"sessionId": sid.String(), "waitSeconds": 5}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	got := assertStatusBytesEqual(t, "narvi_wait_for_session", rest, toolText(t, res), true)
	if wait, _ := got["wait"].(map[string]any); got["activity"] != "finished" || wait["reason"] != "settled" || wait["waitedMs"] != float64(0) {
		t.Fatalf("the wait = %v, want finished, settled after 0 ms", got)
	}
}

// getWithCookieE GETs url as the cookie's user and returns the status and
// the body -- an error, never a t.Fatal, so a background goroutine may
// call it.
func getWithCookieE(ctx context.Context, url, cookie string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

// rawWait POSTs one narvi_wait_for_session call to rig's /mcp with bearer
// and returns its result's text and isError -- an error, never a t.Fatal,
// so a background goroutine may call it.
func rawWait(ctx context.Context, rig *oauthRouterRig, bearer string, sessionID pgtype.UUID, waitSeconds int) (string, bool, error) {
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"narvi_wait_for_session","arguments":{"sessionId":%q,"waitSeconds":%d},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, sessionID.String(), waitSeconds)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rig.server.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "narvi_wait_for_session")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false, err
	}
	var env struct {
		Result *struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &env) != nil || env.Result == nil || len(env.Result.Content) != 1 {
		return "", false, fmt.Errorf("raw wait: %d %s, want a tool result", resp.StatusCode, raw)
	}
	return env.Result.Content[0].Text, env.Result.IsError, nil
}

// sdkWaitCapacityPerGrant is TestOAuth_ProductionRouter's
// Wait_CapacityDegradesToSnapshot_PerGrant: with the grant's two waits
// (the shipped cap) blocked on a queued session, its third answers at
// once -- a normal result, never an error: the status, reason "capacity",
// after 0 ms -- while the same member's cookie wait, a key of its own, is
// admitted. Mutation: an over-cap wait that blocks, or errors.
func sdkWaitCapacityPerGrant(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, nil)
	bearer := flow.recorder.lastBearer()
	sid := seedLaggingSession(ctx, t, rig.pool, flow.member.ID, 0)
	waiter := rig.app.sessionWaiter
	perKey := rig.cfg.Timeouts.MCPWaitMaxConcurrentPerKey
	if perKey != 2 {
		t.Fatalf("MCPWaitMaxConcurrentPerKey = %d, want the shipped 2 on this router", perKey)
	}
	base := waiter.Active()

	var g errgroup.Group
	for range perKey {
		g.Go(func() error {
			text, isError, err := rawWait(ctx, rig, bearer, sid, 2)
			if err != nil {
				return err
			}
			if isError || !strings.Contains(text, `"reason":"timeout"`) {
				return fmt.Errorf("an admitted wait answered %s (isError %v), want a timeout", text, isError)
			}
			return nil
		})
	}
	deadline := time.Now().Add(2 * time.Second)
	for waiter.Active() < base+perKey && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if waiter.Active() != base+perKey {
		t.Fatalf("Active = %d, want the grant's %d waits blocked", waiter.Active()-base, perKey)
	}

	start := time.Now()
	text, isError, err := rawWait(ctx, rig, bearer, sid, 2)
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	var over restdtos.SessionActivity
	if isError || json.Unmarshal([]byte(text), &over) != nil || over.Wait == nil {
		t.Fatalf("the over-cap wait = %s (isError %v), want a normal status result", text, isError)
	}
	if over.Wait.Reason != restdtos.SessionActivityWaitReasonCapacity || over.Wait.WaitedMs != 0 || took > time.Second || over.Activity != restdtos.SessionActivityActivityQueued {
		t.Fatalf("the over-cap wait = %+v after %v, want the queued status at once, reason capacity, 0 ms", over, took)
	}

	g.Go(func() error {
		code, body, err := getWithCookieE(ctx, rig.server.URL+"/api/sessions/"+sid.String()+"/status?waitSeconds=1", flow.cookie)
		if err != nil {
			return err
		}
		if code != http.StatusOK || !strings.Contains(string(body), `"reason":"timeout"`) {
			return fmt.Errorf("the member's cookie wait = %d %s, want admitted and timed out", code, body)
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
}

// sdkWaitCapacityPerUser is TestOAuth_ProductionRouter's
// Wait_CapacityDegradesToSnapshot_PerUser (review round 1's P1): a member
// who authorized three clients holds three grants, and the per-grant cap
// alone would give them two waits each. With two waits blocked under each
// of the first two grants -- the shipped four per user -- a wait under the
// third grant, which runs none of its own, and the member's own cookie
// wait each answer at once: the queued status, reason "capacity", 0 ms, a
// normal result. Another member's wait on the same replica is admitted and
// runs to its bound. Mutation: the per-user check dropped (the third
// grant's wait is admitted and times out).
func sdkWaitCapacityPerUser(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	perKey, perUser := rig.cfg.Timeouts.MCPWaitMaxConcurrentPerKey, rig.cfg.Timeouts.MCPWaitMaxConcurrentPerUser
	if perKey != 2 || perUser != 4 {
		t.Fatalf("MCPWaitMaxConcurrentPerKey %d, MCPWaitMaxConcurrentPerUser %d, want the shipped 2 and 4 on this router", perKey, perUser)
	}
	first := rig.connectSDKClient(ctx, t, nil)
	bearers := []string{first.recorder.lastBearer()}
	for i := range 2 {
		var client restdtos.MCPClient
		body := fmt.Sprintf(`{"clientName":"Editor Plugin %d","redirectUris":["http://127.0.0.1/callback"]}`, i+2)
		if status := rig.doJSON(t, http.MethodPost, "/api/mcp-clients", []byte(body), &client, first.adminCookie); status != http.StatusCreated {
			t.Fatalf("register client %d: status %d", i+2, status)
		}
		flow, err := rig.dialSDKClient(ctx, t, first.member, first.cookie, nil, func(c *sdkauth.AuthorizationCodeHandlerConfig) {
			c.PreregisteredClient = &oauthex.ClientCredentials{ClientID: client.ClientId}
			c.RedirectURL = "http://127.0.0.1:1/callback"
		})
		if err != nil {
			t.Fatalf("SDK Connect under client %d: %v", i+2, err)
		}
		bearers = append(bearers, flow.recorder.lastBearer())
	}
	sid := seedLaggingSession(ctx, t, rig.pool, first.member.ID, 0)
	other := rig.connectSDKClient(ctx, t, nil)
	otherSid := seedLaggingSession(ctx, t, rig.pool, other.member.ID, 0)
	waiter := rig.app.sessionWaiter
	base := waiter.Active()

	var g errgroup.Group
	for _, bearer := range bearers[:2] {
		for range perKey {
			g.Go(func() error {
				text, isError, err := rawWait(ctx, rig, bearer, sid, 4)
				if err != nil {
					return err
				}
				if isError || !strings.Contains(text, `"reason":"timeout"`) {
					return fmt.Errorf("an admitted wait answered %s (isError %v), want a timeout", text, isError)
				}
				return nil
			})
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for waiter.Active() < base+perUser && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if waiter.Active() != base+perUser {
		t.Fatalf("Active = %d, want the member's %d waits blocked", waiter.Active()-base, perUser)
	}

	capacity := func(surface string, code int, body []byte, took time.Duration) {
		t.Helper()
		var over restdtos.SessionActivity
		if code != http.StatusOK || json.Unmarshal(body, &over) != nil || over.Wait == nil {
			t.Fatalf("the member's wait through %s = %d %s, want a normal status answer", surface, code, body)
		}
		if over.Wait.Reason != restdtos.SessionActivityWaitReasonCapacity || over.Wait.WaitedMs != 0 || took > time.Second || over.Activity != restdtos.SessionActivityActivityQueued {
			t.Fatalf("the member's wait through %s = %+v after %v, want the queued status at once, reason capacity, 0 ms", surface, over, took)
		}
	}
	start := time.Now()
	text, isError, err := rawWait(ctx, rig, bearers[2], sid, 4)
	if err != nil {
		t.Fatal(err)
	}
	if isError {
		t.Fatalf("the third grant's wait = %s, a tool error, want a normal result", text)
	}
	capacity("a third grant with no wait of its own", http.StatusOK, []byte(text), time.Since(start))
	start = time.Now()
	code, body, err := getWithCookieE(ctx, rig.server.URL+"/api/sessions/"+sid.String()+"/status?waitSeconds=4", first.cookie)
	if err != nil {
		t.Fatal(err)
	}
	capacity("the browser", code, body, time.Since(start))

	g.Go(func() error {
		text, isError, err := rawWait(ctx, rig, other.recorder.lastBearer(), otherSid, 1)
		if err != nil {
			return err
		}
		var got restdtos.SessionActivity
		if isError || json.Unmarshal([]byte(text), &got) != nil || got.Wait == nil ||
			got.Wait.Reason != restdtos.SessionActivityWaitReasonTimeout || got.Wait.WaitedMs < 900 {
			return fmt.Errorf("another member's wait = %s (isError %v), want admitted and waited to its one-second bound", text, isError)
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
}

// sdkWaitScopelessGrantDoesNotSeeIt is TestOAuth_ProductionRouter's
// Wait_ScopelessGrantDoesNotSeeIt: a grant approved without mcp:read has
// no narvi_wait_for_session in its tool list, the SDK's own call fails,
// and a raw call answers exactly what a tool that never existed answers,
// byte for byte once the chosen name is substituted -- while the same call
// under an mcp:read grant is a result.
func sdkWaitScopelessGrantDoesNotSeeIt(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, func(d *consentDriver) {
		d.keepScopes = func([]string) []string { return nil }
	})
	sid := seedLaggingSession(ctx, t, rig.pool, flow.member.ID, 0)
	const tool = "narvi_wait_for_session"
	for _, name := range toolNames(ctx, t, flow.session) {
		if name == tool {
			t.Fatalf("a scope-less grant lists %s", tool)
		}
	}
	if _, err := flow.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: tool, Arguments: map[string]any{"sessionId": sid.String(), "waitSeconds": 1}}); err == nil {
		t.Fatalf("calling hidden %s through the SDK succeeded", tool)
	}
	scopeless := flow.recorder.lastBearer()
	other, _ := createRouterUser(ctx, t, rig.pool, sqlcgen.UserRoleMember)
	full := mintBuildBearer(ctx, t, rig.pool, rig.cfg, other.ID)
	call := func(token, name string) (int, string) {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":%q,"arguments":{"sessionId":%q,"waitSeconds":1},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, name, sid.String())
		status, _, raw := rig.postMCP(t, "tools/call", name, body, token)
		return status, string(raw)
	}
	const unknown = "narvi_does_not_exist"
	unknownStatus, unknownBody := call(full, unknown)
	if status, body := call(scopeless, tool); status != unknownStatus || body != strings.ReplaceAll(unknownBody, unknown, tool) {
		t.Fatalf("hidden %s answers differently from an unknown tool:\n hidden:  %d %s\n unknown: %d %s", tool, status, body, unknownStatus, unknownBody)
	}
	if status, body := call(full, tool); status != http.StatusOK || !strings.Contains(body, `"reason":"timeout"`) {
		t.Fatalf("%s under mcp:read: %d %s, want a result", tool, status, body)
	}
}

// sdkWaitCrossReplica is TestOAuth_ProductionRouter's Wait_CrossReplica:
// two routers Build made -- two replicas, each with its own event hub and
// its own Waiter -- on one database. The same running session is waited
// on through both at once: through B by the official SDK client under a
// grant B's authorization server issued, and through A by the member's
// cookie. The turn's completion is committed to the shared database by
// neither process; both waits answer within about a poll, finished.
// Mutation: a wake-up that listened only to the in-process hub would
// leave both to their bound.
func sdkWaitCrossReplica(t *testing.T, a, b *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := b.connectSDKClient(ctx, t, nil)
	seed := newWaitSeed(a.pool)
	sid := seed.session(ctx, t, flow.member.ID)
	turnID := seed.processingTurn(ctx, t, sid, false)
	aBase, bBase := a.app.sessionWaiter.Active(), b.app.sessionWaiter.Active()

	viaB := startWait(ctx, flow.session, map[string]any{"sessionId": sid.String(), "waitSeconds": 5})
	var viaA errgroup.Group
	var aBody []byte
	var aAnsweredAt atomic.Int64
	viaA.Go(func() error {
		code, body, err := getWithCookieE(ctx, a.server.URL+"/api/sessions/"+sid.String()+"/status?waitSeconds=5", flow.cookie)
		aAnsweredAt.Store(time.Now().UnixNano())
		if err != nil {
			return err
		}
		if code != http.StatusOK {
			return fmt.Errorf("the wait through A: %d %s", code, body)
		}
		aBody = body
		return nil
	})
	stillWaiting(t, b, viaB, 3, "running, through B")
	if a.app.sessionWaiter.Active() != aBase+1 || b.app.sessionWaiter.Active() != bBase+1 {
		t.Fatalf("Active A %d, B %d, want one wait blocked on each replica", a.app.sessionWaiter.Active()-aBase, b.app.sessionWaiter.Active()-bBase)
	}
	completedAt := time.Now()
	seed.completeTurn(ctx, t, sid, turnID, nil)

	got, answeredAt := viaB.result(t)
	answeredWithinAPoll(t, b, completedAt, answeredAt)
	if got.Activity != restdtos.SessionActivityActivityFinished || got.Wait.Reason != restdtos.SessionActivityWaitReasonSettled {
		t.Fatalf("the wait through B = %+v (wait %+v), want finished, settled", got, got.Wait)
	}
	if err := viaA.Wait(); err != nil {
		t.Fatal(err)
	}
	answeredWithinAPoll(t, a, completedAt, time.Unix(0, aAnsweredAt.Load()))
	var gotA restdtos.SessionActivity
	if err := json.Unmarshal(aBody, &gotA); err != nil || gotA.Activity != restdtos.SessionActivityActivityFinished || gotA.Wait == nil || gotA.Wait.Reason != restdtos.SessionActivityWaitReasonSettled {
		t.Fatalf("the wait through A = %s (%v), want finished, settled", aBody, err)
	}
}

// sdkWaitLongCallThroughSDK is TestOAuth_ProductionRouter's
// Wait_LongCallThroughSDK, on the shipped timeouts: a wait with no
// waitSeconds on a queue that never starts runs the whole shipped
// MCPWaitMaxDuration (25 s), polling every second, end to end through the
// official SDK client -- nothing in the SDK, the transport or the server
// cuts it -- and answers "timeout" with the latest status.
func sdkWaitLongCallThroughSDK(t *testing.T, rig *oauthRouterRig) {
	shipped := platform.DefaultTimeouts()
	if rig.cfg.Timeouts.MCPWaitMaxDuration != shipped.MCPWaitMaxDuration || rig.cfg.Timeouts.MCPWaitPollInterval != shipped.MCPWaitPollInterval {
		t.Fatalf("this router's wait is %v every %v, want the shipped %v every %v", rig.cfg.Timeouts.MCPWaitMaxDuration, rig.cfg.Timeouts.MCPWaitPollInterval, shipped.MCPWaitMaxDuration, shipped.MCPWaitPollInterval)
	}
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, nil)
	sid := seedLaggingSession(ctx, t, rig.pool, flow.member.ID, 0)

	start := time.Now()
	res, err := flow.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "narvi_wait_for_session", Arguments: map[string]any{"sessionId": sid.String()}})
	took := time.Since(start)
	if err != nil {
		t.Fatalf("CallTool after %v: %v", took, err)
	}
	got := decodeWaitResult(t, res)
	maxMs := int(shipped.MCPWaitMaxDuration / time.Millisecond)
	if got.Wait.Reason != restdtos.SessionActivityWaitReasonTimeout || got.Wait.WaitedMs < maxMs || took < shipped.MCPWaitMaxDuration {
		t.Fatalf("the long wait answered after %v with wait %+v, want the whole %v, reason timeout", took, got.Wait, shipped.MCPWaitMaxDuration)
	}
	if took > shipped.MCPWaitMaxDuration+shipped.MCPWaitPollInterval+5*time.Second {
		t.Fatalf("the long wait took %v, want its bound %v plus about one poll", took, shipped.MCPWaitMaxDuration)
	}
	if got.Activity != restdtos.SessionActivityActivityQueued || got.Settled {
		t.Fatalf("the long wait = %+v, want queued and unsettled", got)
	}
}

// waitShutdownInterruptsPromptly is TestOAuth_ProductionRouter's
// Wait_ShutdownInterruptsPromptly_RunServer: Run's own HTTP server
// (newHTTPServer), serving the router Build returned on a real listener,
// with a wait blocked for up to 25 s at a one-second poll. Shutdown answers
// that wait at once -- "interrupted", with the status -- and drains well
// inside ShutdownGracePeriod. The server also sets no WriteTimeout that a
// full-length wait could outlast. Mutation: the RegisterOnShutdown wiring
// dropped leaves the drain held until the wait's own bound, past the grace
// period.
func waitShutdownInterruptsPromptly(t *testing.T, rig *oauthRouterRig) {
	cfg := rig.cfg.Timeouts
	if cfg.MCPWaitMaxDuration <= cfg.ShutdownGracePeriod {
		t.Fatalf("MCPWaitMaxDuration %v is not above ShutdownGracePeriod %v on this router: nothing to prove", cfg.MCPWaitMaxDuration, cfg.ShutdownGracePeriod)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPServer(ln.Addr().String(), rig.app.Router, rig.app.sessionWaiter)
	if srv.WriteTimeout != 0 && srv.WriteTimeout <= cfg.MCPWaitMaxDuration {
		t.Fatalf("WriteTimeout %v would cut a %v wait", srv.WriteTimeout, cfg.MCPWaitMaxDuration)
	}
	var serving errgroup.Group
	serving.Go(func() error {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	ctx := oauthTestCtx(t)
	member, cookie := createRouterUser(ctx, t, rig.pool, sqlcgen.UserRoleMember)
	sid := seedLaggingSession(ctx, t, rig.pool, member.ID, 0)

	var waiting errgroup.Group
	var body []byte
	waiting.Go(func() error {
		code, b, err := getWithCookieE(ctx, "http://"+ln.Addr().String()+"/api/sessions/"+sid.String()+"/status?waitSeconds=25", cookie)
		if err != nil {
			return err
		}
		if code != http.StatusOK {
			return fmt.Errorf("the wait: %d %s", code, b)
		}
		body = b
		return nil
	})
	deadline := time.Now().Add(3 * time.Second)
	for rig.app.sessionWaiter.Active() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if rig.app.sessionWaiter.Active() != 1 {
		t.Fatalf("Active = %d, want the wait blocked before the shutdown", rig.app.sessionWaiter.Active())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGracePeriod)
	defer cancel()
	start := time.Now()
	shutdownErr := srv.Shutdown(shutdownCtx)
	drained := time.Since(start)
	if err := waiting.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := serving.Wait(); err != nil {
		t.Fatal(err)
	}
	if shutdownErr != nil || drained > 2*time.Second {
		t.Fatalf("Shutdown = %v after %v, want a clean drain at once (the wait interrupted)", shutdownErr, drained)
	}
	var got restdtos.SessionActivity
	if err := json.Unmarshal(body, &got); err != nil || got.Wait == nil || got.Wait.Reason != restdtos.SessionActivityWaitReasonInterrupted || got.Activity != restdtos.SessionActivityActivityQueued {
		t.Fatalf("the wait answered %s (%v), want the queued status, reason interrupted", body, err)
	}
	if got.Wait.WaitedMs >= int(cfg.MCPWaitMaxDuration/time.Millisecond) {
		t.Fatalf("the wait ran %d ms, want it cut short by the shutdown", got.Wait.WaitedMs)
	}
}
