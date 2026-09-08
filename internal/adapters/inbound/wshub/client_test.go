//go:build integration

// End-to-end integration tests proving internal/adapters/inbound/wshub's
// CLIENT handshake (§6.2, client.go) against a REAL client
// (github.com/coder/websocket.Dial), a real Postgres instance, and (for
// the live-broadcast test) a real internal/app/sessionactor.Actor --
// gated behind the "integration" build tag. Run via `make
// test-integration`.
package wshub_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/clientws"
	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// clientTestRig bundles a fresh pool + every store + registry + hub +
// dispatcher test server a client-hub test needs, built once per test.
type clientTestRig struct {
	pool      *pgxpool.Pool
	sessions  *narvipg.SessionStore
	turns     *narvipg.TurnStore
	sandboxes *narvipg.SandboxStore
	events    *narvipg.EventStore
	artifacts *narvipg.ArtifactStore
	wsTokens  *narvipg.WSTokenStore
	registry  *sessionactor.Registry
	hub       *wshub.Hub
	wsURL     string
}

// newClientTestRig spins up a fresh Postgres-backed rig (registry built
// WITHOUT the hub as its broadcaster -- see newClientTestRigWithBroadcast
// below for the live-broadcast test's own variant that needs that wiring)
// with the given timeouts, and returns it alongside a freshly created
// session row. t.Cleanup tears everything down.
func newClientTestRig(t *testing.T, timeouts platform.Timeouts) (clientTestRig, sqlcgen.Session) {
	t.Helper()
	return newClientTestRigImpl(t, timeouts, false)
}

// newClientTestRigWithBroadcast is newClientTestRig's variant that wires
// rig.hub as the registry's own ports.EventBroadcaster -- needed only by
// the live-broadcast test, since every other test in this file exercises
// the read-only handshake/fetch_history paths, which never touch the
// actor at all.
func newClientTestRigWithBroadcast(t *testing.T, timeouts platform.Timeouts) (clientTestRig, sqlcgen.Session) {
	t.Helper()
	return newClientTestRigImpl(t, timeouts, true)
}

func newClientTestRigImpl(t *testing.T, timeouts platform.Timeouts, wireBroadcaster bool) (clientTestRig, sqlcgen.Session) {
	t.Helper()
	ctx := context.Background()
	pool := newTestPool(t)

	sessionRow, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
	})
	if err != nil {
		t.Fatalf("create test session: %v", err)
	}

	hub := wshub.NewHub()

	var broadcaster ports.EventBroadcaster
	if wireBroadcaster {
		broadcaster = hub
	}
	registry, err := sessionactor.NewRegistry(ctx, pool, timeouts, broadcaster, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	rig := clientTestRig{
		pool:      pool,
		sessions:  narvipg.NewSessionStore(pool),
		turns:     narvipg.NewTurnStore(pool),
		sandboxes: narvipg.NewSandboxStore(pool),
		events:    narvipg.NewEventStore(pool),
		artifacts: narvipg.NewArtifactStore(pool),
		wsTokens:  narvipg.NewWSTokenStore(pool),
		registry:  registry,
		hub:       hub,
	}

	server, wsURL := newDispatcherTestServer(rig.registry, rig.sessions, rig.turns, rig.sandboxes, rig.events, rig.artifacts, rig.wsTokens, rig.hub, timeouts)
	t.Cleanup(server.Close)
	rig.wsURL = wsURL

	return rig, sessionRow
}

// subscribeClient dials ?type=client for sessionID, sends a subscribe
// frame with token, reads and discards the subscribed reply, and returns
// the live connection (caller owns closing it).
func subscribeClient(ctx context.Context, t *testing.T, wsURL, sessionID, token string) *websocket.Conn {
	t.Helper()

	conn, _, err := websocket.Dial(ctx, wsURL+"/sessions/"+sessionID+"/ws?type=client", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	req := clientws.SubscribeRequest{Token: token, ClientId: "test-client"}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal subscribe request: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("Write subscribe: %v", err)
	}

	rc, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, _, err := conn.Read(rc); err != nil {
		t.Fatalf("Read subscribed reply: %v", err)
	}
	return conn
}

// waitForClose reads from conn until it errors (the server closing the
// connection) and returns the close status code via
// websocket.CloseStatus (-1 if the error was not a clean WS close).
func waitForClose(ctx context.Context, t *testing.T, conn *websocket.Conn) websocket.StatusCode {
	t.Helper()
	_, _, err := conn.Read(ctx)
	if err == nil {
		t.Fatal("conn.Read succeeded, want the server to have closed the connection")
	}
	return websocket.CloseStatus(err)
}

// TestClientHandler_HandshakeRejections_PreUpgrade covers the two
// rejections visible as a plain (non-upgrading) HTTP status, mirroring
// sandbox_test.go's own doHandshake-based precedent: a malformed session
// id, and a well-formed id with no matching session row -- both 404
// (client.go's own doc comment, outcome steps 1-2).
func TestClientHandler_HandshakeRejections_PreUpgrade(t *testing.T) {
	rig, _ := newClientTestRig(t, platform.DefaultTimeouts())

	tests := []struct {
		name string
		path string
	}{
		{"malformed session id", "/sessions/not-a-valid-uuid/ws"},
		{"session not found", "/sessions/11111111-1111-1111-1111-111111111111/ws"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := doHandshake(t, rig.wsURL, tc.path, "type=client", nil)
			if got != http.StatusNotFound {
				t.Errorf("status = %d, want %d", got, http.StatusNotFound)
			}
		})
	}
}

// TestClientHandler_SubscribeTimeout proves a connection that upgrades
// but never sends a subscribe frame is closed with 4001 once
// ClientSubscribeTimeout elapses.
func TestClientHandler_SubscribeTimeout(t *testing.T) {
	timeouts := platform.DefaultTimeouts()
	timeouts.ClientSubscribeTimeout = 200 * time.Millisecond
	rig, sessionRow := newClientTestRig(t, timeouts)

	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, rig.wsURL+"/sessions/"+sessionRow.ID.String()+"/ws?type=client", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if got := waitForClose(readCtx, t, conn); got != websocket.StatusCode(4001) {
		t.Errorf("close code = %d, want 4001", got)
	}
}

// TestClientHandler_MalformedSubscribeFrame proves a subscribe frame that
// isn't valid JSON closes the connection with 4001.
func TestClientHandler_MalformedSubscribeFrame(t *testing.T) {
	rig, sessionRow := newClientTestRig(t, platform.DefaultTimeouts())

	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, rig.wsURL+"/sessions/"+sessionRow.ID.String()+"/ws?type=client", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	if err := conn.Write(ctx, websocket.MessageText, []byte("not json")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if got := waitForClose(readCtx, t, conn); got != websocket.StatusCode(4001) {
		t.Errorf("close code = %d, want 4001", got)
	}
}

// TestClientHandler_TokenRejections is table-driven over the 3 ways a
// presented ws-token can fail verification: unknown hash, a real hash but
// scoped to a DIFFERENT session, and a real hash for THIS session that has
// already expired -- the first two close 4001, the third closes 4002
// (client.go's own doc comment, outcome step 5).
func TestClientHandler_TokenRejections(t *testing.T) {
	rig, sessionRow := newClientTestRig(t, platform.DefaultTimeouts())
	ctx := context.Background()

	otherSessionRow, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
	if err != nil {
		t.Fatalf("create other session: %v", err)
	}
	wrongSessionToken := createTestWSToken(ctx, t, rig.pool, otherSessionRow.ID, time.Now().Add(24*time.Hour))
	expiredToken := createTestWSToken(ctx, t, rig.pool, sessionRow.ID, time.Now().Add(-1*time.Hour))

	tests := []struct {
		name      string
		token     string
		wantClose websocket.StatusCode
	}{
		{"unknown token", "this-token-was-never-minted", websocket.StatusCode(4001)},
		{"wrong-session token", wrongSessionToken, websocket.StatusCode(4001)},
		{"expired token", expiredToken, websocket.StatusCode(4002)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn, _, err := websocket.Dial(ctx, rig.wsURL+"/sessions/"+sessionRow.ID.String()+"/ws?type=client", nil)
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			defer func() { _ = conn.CloseNow() }()

			req := clientws.SubscribeRequest{Token: tc.token, ClientId: "test-client"}
			raw, err := json.Marshal(req)
			if err != nil {
				t.Fatalf("marshal subscribe request: %v", err)
			}
			if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
				t.Fatalf("Write: %v", err)
			}

			readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if got := waitForClose(readCtx, t, conn); got != tc.wantClose {
				t.Errorf("close code = %d, want %d", got, tc.wantClose)
			}
		})
	}
}

// TestClientHandler_TokenLookupBackendError proves a genuine backend
// failure during the ws-token lookup (as opposed to "no matching row") is
// reported as StatusInternalError, NOT folded into close code 4001 -- a
// real bug found in review: a transient Postgres blip was previously
// indistinguishable from "this token is bad", which would have told every
// subscribing client its credential was invalid during an unrelated
// infrastructure hiccup. Forces a genuine, non-ErrNoRows error by pointing
// the handler's own wsTokens store at an already-closed pool, while every
// other store (sessions, etc.) still uses the rig's normal, working pool.
func TestClientHandler_TokenLookupBackendError(t *testing.T) {
	rig, sessionRow := newClientTestRig(t, platform.DefaultTimeouts())
	ctx := context.Background()

	// newDedicatedTestPool, not newTestPool: this test deliberately closes
	// its own pool below, which would take down the SHARED pool (and every
	// other test in this binary) if it used the shared one -- see
	// sharedpool_integration_test.go's own top doc comment ("One
	// deliberate exception") for the full reasoning.
	brokenPool := newDedicatedTestPool(t)
	brokenWSTokens := narvipg.NewWSTokenStore(brokenPool)
	brokenPool.Close() // any subsequent query now fails with a real, non-ErrNoRows error.

	server, wsURL := newDispatcherTestServer(rig.registry, rig.sessions, rig.turns, rig.sandboxes, rig.events, rig.artifacts, brokenWSTokens, rig.hub, platform.DefaultTimeouts())
	t.Cleanup(server.Close)

	conn, _, err := websocket.Dial(ctx, wsURL+"/sessions/"+sessionRow.ID.String()+"/ws?type=client", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	req := clientws.SubscribeRequest{Token: "irrelevant-the-lookup-itself-fails", ClientId: "test-client"}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal subscribe request: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("Write: %v", err)
	}

	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if got, want := waitForClose(readCtx, t, conn), websocket.StatusInternalError; got != want {
		t.Errorf("close code = %d, want %d (a backend error must never be reported as 4001 re-auth)", got, want)
	}
}

// TestClientHandler_ValidHandshakeSubscribes proves a fully valid
// handshake replies with a single `subscribed` payload of the right
// shape: sessionId matches, participants is an empty array (§6.2 design
// decision -- participants stays untouched this Step), and events/
// artifacts/state are present.
func TestClientHandler_ValidHandshakeSubscribes(t *testing.T) {
	rig, sessionRow := newClientTestRig(t, platform.DefaultTimeouts())
	ctx := context.Background()
	token := createTestWSToken(ctx, t, rig.pool, sessionRow.ID, time.Now().Add(24*time.Hour))

	conn, _, err := websocket.Dial(ctx, rig.wsURL+"/sessions/"+sessionRow.ID.String()+"/ws?type=client", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	req := clientws.SubscribeRequest{Token: token, ClientId: "test-client"}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal subscribe request: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("Write: %v", err)
	}

	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("Read subscribed reply: %v", err)
	}

	var payload clientws.SubscribedPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unmarshal SubscribedPayload: %v (%s)", err, data)
	}

	if payload.SessionId != sessionRow.ID.String() {
		t.Errorf("SessionId = %q, want %q", payload.SessionId, sessionRow.ID.String())
	}
	if len(payload.Participants) != 0 {
		t.Errorf("Participants = %v, want an empty array (untouched this Step)", payload.Participants)
	}
	if payload.Events == nil {
		t.Error("Events = nil, want a (possibly empty) array")
	}
	if payload.Artifacts == nil {
		t.Error("Artifacts = nil, want a (possibly empty) array")
	}
	if payload.State == nil {
		t.Fatal("State = nil, want a populated map")
	}
	for _, key := range []string{"session", "turns", "sandbox"} {
		if _, ok := payload.State[key]; !ok {
			t.Errorf("State missing key %q", key)
		}
	}
}

// TestClientHandler_SubscribeReplaysFailedUploadStatusAndFailureReason is
// a review-fix coverage addition (FIX I): ArtifactWireMap (client.go)
// hand-builds its wire shape as a map[string]interface{}
// (additionalProperties:true, this schema's own design) rather than a
// generated, field-checked struct -- a typo'd or accidentally-dropped
// "status"/"failureReason" key would decode cleanly and silently show a
// reconnecting client a failed upload as ready, with a 404ing download
// link. Seeds a genuinely 'failed' upload via the REAL production path
// (CreateUpload + MarkUploadFailedIfPending, never a hand-written INSERT)
// and asserts both fields land correctly on the subscribe-time REPLAY
// payload -- httpapi's own TestListArtifacts_FailedUploadStatusAndFailureReason
// (httpapi_integration_test.go) is this test's own REST-side twin.
func TestClientHandler_SubscribeReplaysFailedUploadStatusAndFailureReason(t *testing.T) {
	rig, sessionRow := newClientTestRig(t, platform.DefaultTimeouts())
	ctx := context.Background()
	token := createTestWSToken(ctx, t, rig.pool, sessionRow.ID, time.Now().Add(24*time.Hour))

	id := uuid.New()
	var artifactID pgtype.UUID
	if err := artifactID.Scan(id.String()); err != nil {
		t.Fatalf("scan artifact id: %v", err)
	}
	blobKey := "sessions/" + sessionRow.ID.String() + "/uploads/" + id.String()
	size := int64(10)
	contentType := "application/octet-stream"
	filename := "never-arrived.bin"
	if _, err := rig.artifacts.CreateUpload(ctx, sqlcgen.CreateUploadArtifactParams{
		ID:          artifactID,
		SessionID:   sessionRow.ID,
		Url:         "/api/sessions/" + sessionRow.ID.String() + "/uploads/" + id.String() + "/content",
		BlobKey:     &blobKey,
		SizeBytes:   &size,
		ContentType: &contentType,
		Filename:    &filename,
	}); err != nil {
		t.Fatalf("create upload artifact: %v", err)
	}
	if _, err := rig.artifacts.MarkUploadFailedIfPending(ctx, artifactID, sessionRow.ID, sqlcgen.ArtifactFailureReasonVerificationFailed); err != nil {
		t.Fatalf("mark upload artifact failed: %v", err)
	}

	conn, _, err := websocket.Dial(ctx, rig.wsURL+"/sessions/"+sessionRow.ID.String()+"/ws?type=client", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	req := clientws.SubscribeRequest{Token: token, ClientId: "test-client"}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal subscribe request: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("Write: %v", err)
	}

	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("Read subscribed reply: %v", err)
	}

	var payload clientws.SubscribedPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unmarshal SubscribedPayload: %v (%s)", err, data)
	}
	if len(payload.Artifacts) != 1 {
		t.Fatalf("len(Artifacts) = %d, want 1", len(payload.Artifacts))
	}
	elem := payload.Artifacts[0]
	if elem["status"] != "failed" {
		t.Errorf(`Artifacts[0]["status"] = %v, want "failed"`, elem["status"])
	}
	if elem["failureReason"] != "verification_failed" {
		t.Errorf(`Artifacts[0]["failureReason"] = %v, want "verification_failed"`, elem["failureReason"])
	}
	// §12.2 item 1's own rail (the rail's own artifacts panel): filename/sizeBytes/
	// contentType, the SAME addition as this test's own REST-side twin
	// (httpapi_integration_test.go's TestListArtifacts_
	// FailedUploadStatusAndFailureReason) -- ArtifactWireMap (client.go)
	// dropped all three before this Step.
	if elem["filename"] != filename {
		t.Errorf(`Artifacts[0]["filename"] = %v, want %q`, elem["filename"], filename)
	}
	if elem["contentType"] != contentType {
		t.Errorf(`Artifacts[0]["contentType"] = %v, want %q`, elem["contentType"], contentType)
	}
	if elem["sizeBytes"] != float64(size) {
		t.Errorf(`Artifacts[0]["sizeBytes"] = %v, want %v`, elem["sizeBytes"], size)
	}
}

// TestClientHandler_SubscribedPayloadExcludesSandboxTokenHash proves the
// subscribed reply's own state.sandbox never leaks sandboxes.token_hash
// (or the other internal-ops-only fields providerId/spawnFailureCount/
// lastSpawnFailureAt) to the browser -- a confirmed audit finding: the
// raw sqlcgen.Sandbox row used to be embedded verbatim into the
// `subscribed` reply. Creates a real sandbox row with a non-empty,
// distinctive TokenHash via UpsertForSpawn (the SAME production write
// path a real spawn uses), subscribes, and asserts the RAW JSON bytes of
// the subscribed reply do not contain that literal value anywhere, while
// state.sandbox still carries the fields a client-side UI legitimately
// needs (gen, status).
func TestClientHandler_SubscribedPayloadExcludesSandboxTokenHash(t *testing.T) {
	rig, sessionRow := newClientTestRig(t, platform.DefaultTimeouts())
	ctx := context.Background()

	const secretTokenHash = "definitely-secret-sandbox-token-hash-should-never-leak"
	tokenHash := secretTokenHash
	if _, err := rig.sandboxes.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{
		SessionID: sessionRow.ID,
		TokenHash: &tokenHash,
	}); err != nil {
		t.Fatalf("create test sandbox with token hash: %v", err)
	}

	token := createTestWSToken(ctx, t, rig.pool, sessionRow.ID, time.Now().Add(24*time.Hour))

	conn, _, err := websocket.Dial(ctx, rig.wsURL+"/sessions/"+sessionRow.ID.String()+"/ws?type=client", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	req := clientws.SubscribeRequest{Token: token, ClientId: "test-client"}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal subscribe request: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("Write: %v", err)
	}

	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("Read subscribed reply: %v", err)
	}

	if strings.Contains(string(data), secretTokenHash) {
		t.Fatalf("subscribed reply leaks the sandbox token hash: %s", data)
	}

	var payload clientws.SubscribedPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unmarshal SubscribedPayload: %v (%s)", err, data)
	}
	sandboxState, ok := payload.State["sandbox"].(map[string]interface{})
	if !ok {
		t.Fatalf("state.sandbox = %#v (%T), want a map", payload.State["sandbox"], payload.State["sandbox"])
	}
	for _, key := range []string{"gen", "status"} {
		if _, ok := sandboxState[key]; !ok {
			t.Errorf("state.sandbox missing key %q: %#v", key, sandboxState)
		}
	}
	for _, key := range []string{
		"tokenHash", "token_hash",
		"providerId", "provider_id",
		"spawnFailureCount", "spawn_failure_count",
		"lastSpawnFailureAt", "last_spawn_failure_at",
	} {
		if _, ok := sandboxState[key]; ok {
			t.Errorf("state.sandbox unexpectedly contains internal-only key %q: %#v", key, sandboxState)
		}
	}
}

// TestClientHandler_FetchHistoryPagination creates several events, then
// paginates through them via fetch_history with a small limit, confirming
// nextCursor is non-nil while more pages remain, produces the remaining
// events on the follow-up request, and is nil once exhausted.
//
// ClientFetchHistoryMinInterval is deliberately disabled (0) for this
// test: its own concern is cursor-pagination correctness, orthogonal to
// the per-connection rate limit (covered separately by
// TestClientHandler_FetchHistoryRateLimited below) -- a real test issuing
// several genuine page requests back-to-back, faster than any human
// pagination UI would, must not itself trip that unrelated limit.
func TestClientHandler_FetchHistoryPagination(t *testing.T) {
	timeouts := platform.DefaultTimeouts()
	timeouts.ClientFetchHistoryMinInterval = 0
	rig, sessionRow := newClientTestRig(t, timeouts)
	ctx := context.Background()
	token := createTestWSToken(ctx, t, rig.pool, sessionRow.ID, time.Now().Add(24*time.Hour))

	const total = 5
	for i := 0; i < total; i++ {
		if _, err := rig.events.Create(ctx, sqlcgen.CreateEventParams{
			SessionID: sessionRow.ID,
			Type:      "token",
			MessageID: fmt.Sprintf("msg-%d", i),
			Payload:   []byte(fmt.Sprintf(`{"n":%d}`, i)),
		}); err != nil {
			t.Fatalf("create event %d: %v", i, err)
		}
	}

	conn := subscribeClient(ctx, t, rig.wsURL, sessionRow.ID.String(), token)
	defer func() { _ = conn.CloseNow() }()

	fetchPage := func(cursor *string, limit int) clientws.FetchHistoryResponse {
		t.Helper()
		cursorJSON := "null"
		if cursor != nil {
			cursorJSON = strconv.Quote(*cursor)
		}
		msg := fmt.Sprintf(`{"type":"fetch_history","sessionId":%q,"cursor":%s,"limit":%d}`, sessionRow.ID.String(), cursorJSON, limit)
		if err := conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			t.Fatalf("Write fetch_history: %v", err)
		}
		rc, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, data, err := conn.Read(rc)
		if err != nil {
			t.Fatalf("Read fetch_history response: %v", err)
		}
		var resp clientws.FetchHistoryResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			t.Fatalf("unmarshal FetchHistoryResponse: %v (%s)", err, data)
		}
		return resp
	}

	// First page: limit 2 -> exactly 2 events, non-nil nextCursor (more
	// likely exist).
	page1 := fetchPage(nil, 2)
	if len(page1.Events) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1.Events))
	}
	if page1.NextCursor == nil {
		t.Fatal("page1.NextCursor = nil, want non-nil (3 more events remain)")
	}

	// Second page from that cursor: the next 2.
	page2 := fetchPage(page1.NextCursor, 2)
	if len(page2.Events) != 2 {
		t.Fatalf("page2 len = %d, want 2", len(page2.Events))
	}
	if page2.NextCursor == nil {
		t.Fatal("page2.NextCursor = nil, want non-nil (1 more event remains)")
	}

	// Third page: the last event, and nextCursor is now nil (exhausted).
	page3 := fetchPage(page2.NextCursor, 2)
	if len(page3.Events) != 1 {
		t.Fatalf("page3 len = %d, want 1", len(page3.Events))
	}
	if page3.NextCursor != nil {
		t.Errorf("page3.NextCursor = %v, want nil (exhausted)", *page3.NextCursor)
	}

	if got := len(page1.Events) + len(page2.Events) + len(page3.Events); got != total {
		t.Errorf("total events across pages = %d, want %d", got, total)
	}
}

// TestClientHandler_SubscribeSurvivesManyLargeEvents re-creates, end to
// end, a real bug found in review: initialReplayLimit (an item-count cap)
// alone did not bound the SubscribedPayload's own total marshaled size, so
// a session with enough sizeable event payloads could produce a
// `subscribed` reply exceeding coder/websocket's own default 32KiB
// per-message read limit, killing the ENTIRE subscribe handshake with
// ErrMessageTooBig for any stock client (default limits, unchanged on
// either side). Creates enough large events to reproduce that exact
// failure mode absent the fix, then subscribes with a real,
// default-configured websocket.Dial client and confirms the handshake
// still completes successfully.
func TestClientHandler_SubscribeSurvivesManyLargeEvents(t *testing.T) {
	rig, sessionRow := newClientTestRig(t, platform.DefaultTimeouts())
	ctx := context.Background()
	token := createTestWSToken(ctx, t, rig.pool, sessionRow.ID, time.Now().Add(24*time.Hour))

	// 200 events (matching initialReplayLimit) at ~8KB each -> ~1.6MB
	// total, far beyond coder/websocket's 32KiB default read limit if the
	// byte-budget fix were absent.
	largePayload := strings.Repeat("x", 8*1024)
	for i := 0; i < 200; i++ {
		if _, err := rig.events.Create(ctx, sqlcgen.CreateEventParams{
			SessionID: sessionRow.ID,
			Type:      "token",
			MessageID: fmt.Sprintf("msg-%d", i),
			Payload:   []byte(fmt.Sprintf(`{"text":"%s"}`, largePayload)),
		}); err != nil {
			t.Fatalf("create event %d: %v", i, err)
		}
	}

	// subscribeClient itself already fails the test (via t.Fatalf) if
	// Dial, the subscribe write, or reading the subscribed reply errors --
	// including a coder/websocket ErrMessageTooBig, which is exactly what
	// this test guards against. A stock websocket.Dial with no custom
	// SetReadLimit call is used deliberately, matching a real, unmodified
	// client.
	conn := subscribeClient(ctx, t, rig.wsURL, sessionRow.ID.String(), token)
	_ = conn.CloseNow()
}

// TestClientHandler_LiveBroadcast subscribes a client, then has the
// session's own actor append a new event via a REAL SandboxEvent command
// (handleSandboxEvent, internal/app/sessionactor) routed through the SAME
// registry+hub this test server was built with, and confirms the
// already-subscribed connection receives it unsolicited, raw, matching
// exactly what was stored (§6.2: no wrapper envelope for a live broadcast
// frame).
func TestClientHandler_LiveBroadcast(t *testing.T) {
	rig, sessionRow := newClientTestRigWithBroadcast(t, platform.DefaultTimeouts())
	ctx := context.Background()

	if _, err := rig.sandboxes.Create(ctx, sessionRow.ID); err != nil {
		t.Fatalf("create test sandbox: %v", err)
	}

	token := createTestWSToken(ctx, t, rig.pool, sessionRow.ID, time.Now().Add(24*time.Hour))
	conn := subscribeClient(ctx, t, rig.wsURL, sessionRow.ID.String(), token)
	defer func() { _ = conn.CloseNow() }()

	// Drive a real sandbox event through the actor -- this is what queues
	// a broadcast (via appendRawEvent) delivered only after the actor's
	// own transact commits.
	actor, err := rig.registry.GetOrSpawn(ctx, sessionRow.ID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	eventRaw := json.RawMessage(fmt.Sprintf(`{"type":"token","messageId":"m1","sessionId":%q,"gen":1,"text":"hi"}`, sessionRow.ID.String()))
	reply := make(chan sessionactor.SandboxEventOutcome, 1)
	if err := actor.Send(ctx, sessionactor.SandboxEvent{Type: "token", Gen: 1, Raw: eventRaw, Reply: reply}); err != nil {
		t.Fatalf("actor.Send: %v", err)
	}
	select {
	case outcome := <-reply:
		if !outcome.Persisted {
			t.Fatal("sandbox event was not persisted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the sandbox event outcome")
	}

	broadcastCtx, cancelBroadcast := context.WithTimeout(ctx, 5*time.Second)
	defer cancelBroadcast()
	_, data, err := conn.Read(broadcastCtx)
	if err != nil {
		t.Fatalf("Read live broadcast: %v", err)
	}
	if string(data) != string(eventRaw) {
		t.Errorf("live broadcast payload = %s, want exactly the stored raw event %s", data, eventRaw)
	}
}

// TestClientHandler_BroadcastDuringSubscribeWindowArrivesAfterSubscribed
// proves the F8 audit fix's own core invariant end-to-end, over a real
// websocket connection and a real Postgres-backed handshake: a broadcast
// fired DURING buildSubscribedPayload's own DB-read window -- injected
// deterministically via wshub.SetSubscribeDBReadHookForTest, not a
// sleep-based race -- is (1) delivered to the client only AFTER the
// subscribed reply, never before (the wire-order inversion F8 flagged),
// and (2) never lost (the ORIGINAL bug e2119b1 fixed, and this handler's
// own top comment still requires). Both properties are asserted from the
// same two frames read off the same connection, in order: losing property
// (2) while fixing property (1) would be a regression back to e2119b1's
// own bug, so a single test exercising both together is deliberate, not
// merely convenient.
func TestClientHandler_BroadcastDuringSubscribeWindowArrivesAfterSubscribed(t *testing.T) {
	rig, sessionRow := newClientTestRigWithBroadcast(t, platform.DefaultTimeouts())
	ctx := context.Background()
	token := createTestWSToken(ctx, t, rig.pool, sessionRow.ID, time.Now().Add(24*time.Hour))

	const injectedBroadcast = `{"type":"tick","n":"during-subscribe-window"}`

	// Fires exactly once, synchronously, from inside the handler's own
	// goroutine at the precise point between Hub.Register (step 6) and
	// buildSubscribedPayload's own DB reads (step 7) -- i.e. before the
	// subscribed reply has been assembled, marshaled, or written at all.
	wshub.SetSubscribeDBReadHookForTest(func() {
		rig.hub.Broadcast(sessionRow.ID.String(), json.RawMessage(injectedBroadcast))
	})
	defer wshub.SetSubscribeDBReadHookForTest(nil)

	conn, _, err := websocket.Dial(ctx, rig.wsURL+"/sessions/"+sessionRow.ID.String()+"/ws?type=client", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	req := clientws.SubscribeRequest{Token: token, ClientId: "test-client"}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal subscribe request: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("Write subscribe: %v", err)
	}

	// Frame 1 must be the subscribed reply -- proving property (1): the
	// broadcast injected during the DB-read window did NOT reach the wire
	// first, even though it was enqueued well before the subscribed reply
	// was even assembled.
	rc1, cancel1 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel1()
	_, data1, err := conn.Read(rc1)
	if err != nil {
		t.Fatalf("Read frame 1: %v", err)
	}
	var subscribed clientws.SubscribedPayload
	if err := json.Unmarshal(data1, &subscribed); err != nil {
		t.Fatalf("frame 1 is not a valid SubscribedPayload -- want the subscribed reply FIRST, got %s instead (the injected broadcast reaching the wire before it would be exactly the F8 wire-order inversion): %v", data1, err)
	}
	if subscribed.SessionId != sessionRow.ID.String() {
		t.Fatalf("frame 1 SubscribedPayload.SessionId = %q, want %q", subscribed.SessionId, sessionRow.ID.String())
	}

	// Frame 2 must be the injected broadcast, verbatim -- proving property
	// (2): it was never lost, only correctly deferred until after the
	// subscribed reply.
	rc2, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()
	_, data2, err := conn.Read(rc2)
	if err != nil {
		t.Fatalf("Read frame 2 (the injected broadcast, which must still arrive -- see e2119b1, the original lost-broadcast fix this must not regress): %v", err)
	}
	if string(data2) != injectedBroadcast {
		t.Errorf("frame 2 = %s, want the injected broadcast verbatim %s", data2, injectedBroadcast)
	}
}

// TestHandler_DispatchesByType proves the top-level dispatcher
// (wshub.NewHandler) routes ?type=sandbox to the sandbox handler and
// ?type=client to the client handler, and 400s on anything else --
// exercised through the SAME combined dispatcher server every other test
// in this file uses.
func TestHandler_DispatchesByType(t *testing.T) {
	rig, sessionRow := newClientTestRig(t, platform.DefaultTimeouts())
	ctx := context.Background()

	// type=sandbox, with no sandbox-auth headers at all: reaches the
	// SANDBOX handler's own step 3 (missing Authorization) -> 401. Only
	// the sandbox handler has this check, so a 401 here proves the
	// dispatcher routed to it, not the client handler.
	got := doHandshake(t, rig.wsURL, "/sessions/"+sessionRow.ID.String()+"/ws", "type=sandbox", nil)
	if got != http.StatusUnauthorized {
		t.Errorf("type=sandbox (no auth headers) status = %d, want %d", got, http.StatusUnauthorized)
	}

	// type=client: a real WS Dial (proper upgrade headers) for an existing
	// session succeeds (101), proving the dispatcher reached the CLIENT
	// handler (which has no sandbox-specific header requirements at all).
	conn, resp, err := websocket.Dial(ctx, rig.wsURL+"/sessions/"+sessionRow.ID.String()+"/ws?type=client", nil)
	if err != nil {
		t.Fatalf("Dial (type=client): %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("type=client handshake status = %d, want %d", resp.StatusCode, http.StatusSwitchingProtocols)
	}

	// missing/invalid type -> 400, from the dispatcher itself, before
	// either handler ever runs.
	for _, query := range []string{"", "type=banana"} {
		got := doHandshake(t, rig.wsURL, "/sessions/"+sessionRow.ID.String()+"/ws", query, nil)
		if got != http.StatusBadRequest {
			t.Errorf("query=%q status = %d, want %d", query, got, http.StatusBadRequest)
		}
	}
}

// TestClientHandler_SubscribedReplyImpliesAlreadyRegistered proves the
// real ordering bug this file's own client.go top comment documents is
// fixed: registering for live broadcast delivery happens BEFORE the
// "subscribed" reply is ever written, so a client observing that reply
// can rely on being already registered -- not merely "probably, on a fast
// enough machine". Deliberately fires exactly ONE broadcast immediately
// after subscribeClient returns (i.e. immediately after the client has
// already read the "subscribed" reply) and requires it to arrive well
// within a short, ordinary deadline: with the ordering fixed this is a
// deterministic happens-before guarantee, not a race that widening a
// deadline would only make less likely to lose -- unlike
// TestClientHandler_SlowConnectionDoesNotBlockOthers below, which tests a
// different property (many broadcasts, non-blocking on a slow peer) and
// still needs a generous deadline for that separate reason.
func TestClientHandler_SubscribedReplyImpliesAlreadyRegistered(t *testing.T) {
	rig, sessionRow := newClientTestRig(t, platform.DefaultTimeouts())
	ctx := context.Background()
	token := createTestWSToken(ctx, t, rig.pool, sessionRow.ID, time.Now().Add(24*time.Hour))

	conn := subscribeClient(ctx, t, rig.wsURL, sessionRow.ID.String(), token)
	defer func() { _ = conn.CloseNow() }()

	rig.hub.Broadcast(sessionRow.ID.String(), json.RawMessage(`{"type":"tick","n":0}`))

	rc, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, data, err := conn.Read(rc)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(data) != `{"type":"tick","n":0}` {
		t.Errorf("got %s, want the broadcast payload verbatim", data)
	}
}

// TestClientHandler_SlowConnectionDoesNotBlockOthers proves a slow/
// non-draining client connection does not block a broadcast from
// reaching OTHER connections subscribed to the same session -- the
// single most important correctness property of the Hub design (proven
// deterministically, without any network involved, in hub_test.go's own
// TestHub_BroadcastNonBlockingOnFullChannel; this test confirms the same
// property end-to-end over real WS connections). One connection
// subscribes and then is never read from again (simulating a stalled
// browser tab); a second connection subscribes and actively drains via a
// background reader (errgroup.Group.Go -- §11, no naked goroutines). A
// burst of direct Hub.Broadcast calls must complete promptly, and the
// fast connection must still receive all of them.
func TestClientHandler_SlowConnectionDoesNotBlockOthers(t *testing.T) {
	rig, sessionRow := newClientTestRig(t, platform.DefaultTimeouts())
	ctx := context.Background()
	token := createTestWSToken(ctx, t, rig.pool, sessionRow.ID, time.Now().Add(24*time.Hour))

	slowConn := subscribeClient(ctx, t, rig.wsURL, sessionRow.ID.String(), token)
	defer func() { _ = slowConn.CloseNow() }() // deliberately never read from again below.

	fastConn := subscribeClient(ctx, t, rig.wsURL, sessionRow.ID.String(), token)
	defer func() { _ = fastConn.CloseNow() }()

	// Deliberately fewer than hubConnBufferSize (64): the FAST connection's
	// own channel must never legitimately overflow just from ordinary
	// burst timing/network-write latency -- this test is about the SLOW
	// connection never blocking delivery to others, not about exercising
	// the fast connection's own buffer capacity limit (hub_test.go's
	// TestHub_BroadcastNonBlockingOnFullChannel already covers that
	// separately, deterministically, without any network involved).
	const broadcastCount = 50
	msgCh := make(chan []byte, broadcastCount)
	readCtx, cancelRead := context.WithCancel(ctx)
	defer cancelRead()

	var readerGroup errgroup.Group
	readerGroup.Go(func() error {
		for {
			_, data, err := fastConn.Read(readCtx)
			if err != nil {
				return nil
			}
			msgCh <- data
		}
	})

	completed := make(chan struct{})
	var burstGroup errgroup.Group
	burstGroup.Go(func() error {
		for i := 0; i < broadcastCount; i++ {
			rig.hub.Broadcast(sessionRow.ID.String(), json.RawMessage(fmt.Sprintf(`{"type":"tick","n":%d}`, i)))
		}
		close(completed)
		return nil
	})

	// Both deadlines below are deliberately generous -- they exist only to
	// bound the test if the non-blocking property under test were ever
	// violated (Broadcast genuinely stuck waiting on the slow connection),
	// not to assert anything about real-world timing. A tight bound here
	// previously produced a real, observed CI flake (0/50 received) on a
	// resource-contended runner where the reader goroutine's own
	// scheduling was delayed well past a tighter deadline, despite
	// Broadcast/Register (client.go) being provably non-blocking per
	// connection -- confirmed by 10 clean local reruns immediately after
	// that failure. Widening these is the fix: it does not change what
	// this test proves, only how much CI scheduling jitter it tolerates
	// before that proof's own bookkeeping times out.
	select {
	case <-completed:
	case <-time.After(30 * time.Second):
		t.Fatal("broadcasting with a slow/non-draining connection present took too long -- want it to never block on one slow subscriber")
	}
	if err := burstGroup.Wait(); err != nil {
		t.Fatalf("broadcast goroutine: %v", err)
	}

	received := 0
	timeout := time.After(20 * time.Second)
collect:
	for received < broadcastCount {
		select {
		case <-msgCh:
			received++
		case <-timeout:
			break collect
		}
	}
	if received != broadcastCount {
		t.Errorf("fast connection received %d/%d broadcasts, want all %d despite the slow connection never draining", received, broadcastCount, broadcastCount)
	}

	cancelRead()
	_ = readerGroup.Wait()
}

// TestClientHandler_IdlePingTimeoutClosesUnresponsiveConnection proves the
// idle-liveness mechanism (audit-remediation, inbound-hygiene lens,
// client.go's own pingClientLoop) genuinely closes a connection that never
// answers the server's own ping.
//
// This is the achievable form of "simulate an unresponsive-but-not-closed
// peer" against this library's real API: github.com/coder/websocket's own
// Conn.Ping doc is explicit that a pong can only ever be observed via a
// CONCURRENT Reader/Read call on the peer's side ("Ping must be called
// concurrently with Reader as it does not read from the connection but
// instead waits for a Reader call to read the pong") -- there is no
// lower-level knob this library exposes (no exposed access to the
// underlying net.Conn to half-close the read side) beyond simply never
// calling Read at all during the window the ping needs to go unanswered.
// Crucially, that means this test must NOT call conn.Read (directly or
// via a helper like waitForClose) until well AFTER the timeout has
// already fired server-side -- an Read call in progress at the wrong
// moment would itself let this library auto-answer the ping, exactly the
// healthy-connection behavior this test is deliberately not exercising
// (that direction is covered separately, immediately below, by
// TestClientHandler_HealthyConnectionSurvivesFrequentPings). Once the
// close has already happened on the wire, a single later Read correctly
// observes it (any single leftover, now-orphaned ping frame preceding the
// close frame in the stream is transparently absorbed by this library's
// own Reader loop before it reaches the close frame -- verified directly
// against this exact dependency version during this test's own design).
func TestClientHandler_IdlePingTimeoutClosesUnresponsiveConnection(t *testing.T) {
	timeouts := platform.DefaultTimeouts()
	timeouts.ClientWSPingInterval = 150 * time.Millisecond
	rig, sessionRow := newClientTestRig(t, timeouts)
	ctx := context.Background()
	token := createTestWSToken(ctx, t, rig.pool, sessionRow.ID, time.Now().Add(24*time.Hour))

	conn := subscribeClient(ctx, t, rig.wsURL, sessionRow.ID.String(), token)
	defer func() { _ = conn.CloseNow() }()

	// Deliberately do not read at all during this window -- see this
	// test's own doc comment above for why that is exactly what makes
	// this connection unable to ever answer the server's ping. By the
	// time this sleep elapses (several ping intervals), the server's own
	// ping must already have gone unanswered and closed the connection.
	time.Sleep(5 * timeouts.ClientWSPingInterval)

	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if got := waitForClose(readCtx, t, conn); got != websocket.StatusCode(4003) {
		t.Errorf("close code = %d, want 4003 (idle timeout)", got)
	}
}

// TestClientHandler_HealthyConnectionSurvivesFrequentPings proves the
// idle-liveness mechanism does NOT spuriously kill a live, healthy
// connection: with ClientWSPingInterval set very short, a real connection
// that actively answers every ping/pong (via conn.CloseRead, whose own doc
// comment guarantees it "will ensure that ping, pong and close frames are
// responded to") survives comfortably past many ping intervals.
func TestClientHandler_HealthyConnectionSurvivesFrequentPings(t *testing.T) {
	timeouts := platform.DefaultTimeouts()
	timeouts.ClientWSPingInterval = 100 * time.Millisecond
	rig, sessionRow := newClientTestRig(t, timeouts)
	ctx := context.Background()
	token := createTestWSToken(ctx, t, rig.pool, sessionRow.ID, time.Now().Add(24*time.Hour))

	conn := subscribeClient(ctx, t, rig.wsURL, sessionRow.ID.String(), token)
	defer func() { _ = conn.CloseNow() }()

	closedCtx := conn.CloseRead(ctx)

	select {
	case <-closedCtx.Done():
		t.Fatal("connection was closed even though it was actively answering every ping")
	case <-time.After(10 * timeouts.ClientWSPingInterval):
		// Survived comfortably past 10 ping intervals -- the mechanism does
		// not spuriously kill a live, responsive connection.
	}
}

// TestClientHandler_FetchHistoryRateLimited proves a burst of fetch_history
// requests sent faster than ClientFetchHistoryMinInterval only results in
// the first one being processed (a real reply observed), with the rest of
// the burst silently dropped -- and that this is a genuine RATE limit, not
// a permanent block: a follow-up request sent after the interval elapses
// is processed as normal.
func TestClientHandler_FetchHistoryRateLimited(t *testing.T) {
	timeouts := platform.DefaultTimeouts()
	timeouts.ClientFetchHistoryMinInterval = 300 * time.Millisecond
	rig, sessionRow := newClientTestRig(t, timeouts)
	ctx := context.Background()
	token := createTestWSToken(ctx, t, rig.pool, sessionRow.ID, time.Now().Add(24*time.Hour))

	const total = 3
	for i := 0; i < total; i++ {
		if _, err := rig.events.Create(ctx, sqlcgen.CreateEventParams{
			SessionID: sessionRow.ID,
			Type:      "token",
			MessageID: fmt.Sprintf("msg-%d", i),
			Payload:   []byte(fmt.Sprintf(`{"n":%d}`, i)),
		}); err != nil {
			t.Fatalf("create event %d: %v", i, err)
		}
	}

	conn := subscribeClient(ctx, t, rig.wsURL, sessionRow.ID.String(), token)
	defer func() { _ = conn.CloseNow() }()

	respCh := make(chan struct{}, 16)
	readCtx, cancelRead := context.WithCancel(ctx)
	defer cancelRead()
	var readerGroup errgroup.Group
	readerGroup.Go(func() error {
		for {
			_, _, err := conn.Read(readCtx)
			if err != nil {
				return nil
			}
			respCh <- struct{}{}
		}
	})

	fetchMsg := fmt.Sprintf(`{"type":"fetch_history","sessionId":%q,"cursor":null,"limit":1}`, sessionRow.ID.String())

	// Burst: several fetch_history frames sent back-to-back with no delay,
	// deliberately faster than ClientFetchHistoryMinInterval.
	const burst = 5
	for i := 0; i < burst; i++ {
		if err := conn.Write(ctx, websocket.MessageText, []byte(fetchMsg)); err != nil {
			t.Fatalf("Write burst fetch_history %d: %v", i, err)
		}
	}

	// Only the FIRST of the burst should be processed.
	select {
	case <-respCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first fetch_history response")
	}
	// The rest of the burst must be dropped outright (not merely delayed):
	// wait comfortably less than ClientFetchHistoryMinInterval and confirm
	// nothing else arrives from this same burst.
	select {
	case <-respCh:
		t.Fatal("received a second fetch_history response from the burst; want the rest dropped by the rate limit")
	case <-time.After(150 * time.Millisecond):
	}

	// After the interval elapses, a FRESH request must still be honored --
	// proving this is a rate limit, not a permanent block.
	time.Sleep(timeouts.ClientFetchHistoryMinInterval + 50*time.Millisecond)
	if err := conn.Write(ctx, websocket.MessageText, []byte(fetchMsg)); err != nil {
		t.Fatalf("Write follow-up fetch_history: %v", err)
	}
	select {
	case <-respCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the follow-up fetch_history response after the rate-limit interval elapsed")
	}

	cancelRead()
	_ = readerGroup.Wait()
}
