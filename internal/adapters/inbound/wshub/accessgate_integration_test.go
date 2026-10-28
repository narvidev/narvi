//go:build integration

// The client socket's session read check (technical plan §6.2, §13.3;
// client.go's step 5b and recheckClientAccessLoop), end to end over a real
// client and real Postgres: at subscribe, before the connection is
// registered, and again on the open socket.
package wshub_test

import (
	"context"
	"encoding/json"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/clientws"
	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// closedTestPool is a pool whose every query fails with a real,
// non-ErrNoRows error: built without connecting, then closed. It shares
// nothing with the binary's shared pool, so closing it costs no other
// test anything.
func closedTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://narvi@127.0.0.1:1/narvi?connect_timeout=1")
	if err != nil {
		t.Fatalf("build the pool: %v", err)
	}
	pool.Close()
	return pool
}

// createTestWSTokenWithoutUser stores a ws-token whose ws_tokens row names
// no user: a row minted before auth v1, or one whose minting user was
// deleted since (user_id is SET NULL, migrations/000016).
func createTestWSTokenWithoutUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) string {
	t.Helper()
	token, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("generate test ws-token: %v", err)
	}
	if _, err := narvipg.NewWSTokenStore(pool).Create(ctx, sqlcgen.CreateWSTokenParams{
		SessionID: sessionID,
		TokenHash: platform.HashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("create test ws-token without a user: %v", err)
	}
	return token
}

// sendSubscribe dials ?type=client for sessionID and writes the subscribe
// frame; the caller reads the reply and owns the connection.
func sendSubscribe(ctx context.Context, t *testing.T, wsURL, sessionID, token string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.Dial(ctx, wsURL+"/sessions/"+sessionID+"/ws?type=client", nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	raw, err := json.Marshal(clientws.SubscribeRequest{Token: token, ClientId: "access-gate"})
	if err != nil {
		t.Fatalf("marshal subscribe request: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("Write subscribe: %v", err)
	}
	return conn
}

// TestClientHandler_SubscribeAccessGate pins step 5b: a token that is
// valid for the session subscribes only if its user may read the session.
// A "no" closes 4001, the code an unknown token gets; a check that cannot
// be made closes 1011. A refused connection is never registered: the
// subscribeDBReadHook seam, which fires right after Hub.Register, never
// fires for it, and the Hub reports no participant for the session.
func TestClientHandler_SubscribeAccessGate(t *testing.T) {
	rig, _ := newClientTestRig(t, platform.DefaultTimeouts())
	ctx := context.Background()
	expires := time.Now().Add(time.Hour)

	tests := []struct {
		name string
		// token stores the presented token for session and returns it with
		// the user it names (invalid for a token with no user).
		token       func(t *testing.T, session pgtype.UUID) (string, pgtype.UUID)
		brokenUsers bool
		wantClose   websocket.StatusCode // 0: subscribes
	}{
		{
			name: "active member subscribes",
			token: func(t *testing.T, session pgtype.UUID) (string, pgtype.UUID) {
				user := createTestUserWithRole(ctx, t, rig.pool, uniqueTestEmail("gate-member"), "Gate Member", sqlcgen.UserRoleMember)
				return createTestWSTokenForUser(ctx, t, rig.pool, session, user, expires), user
			},
		},
		{
			name: "active viewer subscribes",
			token: func(t *testing.T, session pgtype.UUID) (string, pgtype.UUID) {
				user := createTestUserWithRole(ctx, t, rig.pool, uniqueTestEmail("gate-viewer"), "Gate Viewer", sqlcgen.UserRoleViewer)
				return createTestWSTokenForUser(ctx, t, rig.pool, session, user, expires), user
			},
		},
		{
			name: "disabled user is refused 4001",
			token: func(t *testing.T, session pgtype.UUID) (string, pgtype.UUID) {
				user := createTestUser(ctx, t, rig.pool, uniqueTestEmail("gate-disabled"), "Gate Disabled")
				token := createTestWSTokenForUser(ctx, t, rig.pool, session, user, expires)
				if _, err := rig.pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1`, user); err != nil {
					t.Fatalf("disable user: %v", err)
				}
				return token, user
			},
			wantClose: websocket.StatusCode(4001),
		},
		{
			name: "deleted user is refused 4001",
			token: func(t *testing.T, session pgtype.UUID) (string, pgtype.UUID) {
				user := createTestUser(ctx, t, rig.pool, uniqueTestEmail("gate-deleted"), "Gate Deleted")
				token := createTestWSTokenForUser(ctx, t, rig.pool, session, user, expires)
				if _, err := rig.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, user); err != nil {
					t.Fatalf("delete user: %v", err)
				}
				return token, user
			},
			wantClose: websocket.StatusCode(4001),
		},
		{
			name: "token with no user is refused 4001",
			token: func(t *testing.T, session pgtype.UUID) (string, pgtype.UUID) {
				return createTestWSTokenWithoutUser(ctx, t, rig.pool, session), pgtype.UUID{}
			},
			wantClose: websocket.StatusCode(4001),
		},
		{
			name: "unreadable users store closes 1011",
			token: func(t *testing.T, session pgtype.UUID) (string, pgtype.UUID) {
				user := createTestUser(ctx, t, rig.pool, uniqueTestEmail("gate-broken"), "Gate Broken")
				return createTestWSTokenForUser(ctx, t, rig.pool, session, user, expires), user
			},
			brokenUsers: true,
			wantClose:   websocket.StatusInternalError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessionRow, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			token, user := tc.token(t, sessionRow.ID)

			wsURL := rig.wsURL
			if tc.brokenUsers {
				server, url := newDispatcherTestServer(rig.registry, rig.sessions, rig.turns, rig.sandboxes, rig.events, rig.artifacts, rig.wsTokens,
					narvipg.NewUserStore(closedTestPool(t)), rig.hub, platform.DefaultTimeouts())
				t.Cleanup(server.Close)
				wsURL = url
			}

			var registered atomic.Int32
			wshub.SetSubscribeDBReadHookForTest(func() { registered.Add(1) })
			defer wshub.SetSubscribeDBReadHookForTest(nil)

			conn := sendSubscribe(ctx, t, wsURL, sessionRow.ID.String(), token)
			defer func() { _ = conn.CloseNow() }()

			readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			_, data, err := conn.Read(readCtx)

			if tc.wantClose == 0 {
				if err != nil {
					t.Fatalf("Read subscribed reply: %v (close code %d), want a subscribed reply", err, websocket.CloseStatus(err))
				}
				var payload clientws.SubscribedPayload
				if err := json.Unmarshal(data, &payload); err != nil || payload.SessionId != sessionRow.ID.String() {
					t.Fatalf("first frame = %s (%v), want this session's subscribed reply", data, err)
				}
				if got := registered.Load(); got != 1 {
					t.Errorf("registration seam fired %d times, want 1", got)
				}
				if got := rig.hub.Participants(sessionRow.ID.String()); !slices.Contains(got, user.String()) {
					t.Errorf("Hub.Participants = %v, want it to hold %s", got, user.String())
				}
				return
			}

			if err == nil {
				t.Fatalf("first frame = %s, want the connection closed %d", data, tc.wantClose)
			}
			if got := websocket.CloseStatus(err); got != tc.wantClose {
				t.Errorf("close code = %d, want %d", got, tc.wantClose)
			}
			if got := registered.Load(); got != 0 {
				t.Errorf("registration seam fired %d times, want 0: a refused connection must never be registered", got)
			}
			if got := rig.hub.Participants(sessionRow.ID.String()); len(got) != 0 {
				t.Errorf("Hub.Participants = %v, want none", got)
			}
		})
	}
}

// TestClientHandler_RecheckClosesOpenSocket pins the open socket's
// re-check (recheckClientAccessLoop): with ClientWSAccessRecheckInterval
// at 100ms (and pings out of the way), a subscriber that loses read access
// after subscribing is closed 4001, an unreadable users store closes 1011,
// and a subscriber that keeps access stays open and keeps receiving
// broadcasts. When the client sees the close, the connection has already
// left the Hub: the re-check unregisters it before it writes the close
// frame.
func TestClientHandler_RecheckClosesOpenSocket(t *testing.T) {
	timeouts := platform.DefaultTimeouts()
	timeouts.ClientWSAccessRecheckInterval = 100 * time.Millisecond
	timeouts.ClientWSPingInterval = time.Hour
	rig, _ := newClientTestRig(t, timeouts)
	ctx := context.Background()

	tests := []struct {
		name string
		// ownUsersPool gives the handler a users store on a pool of its
		// own, which change may close.
		ownUsersPool bool
		change       func(t *testing.T, user pgtype.UUID, usersPool *pgxpool.Pool)
		wantClose    websocket.StatusCode // 0: stays open
	}{
		{
			name: "user disabled closes 4001",
			change: func(t *testing.T, user pgtype.UUID, _ *pgxpool.Pool) {
				if _, err := rig.pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1`, user); err != nil {
					t.Fatalf("disable user: %v", err)
				}
			},
			wantClose: websocket.StatusCode(4001),
		},
		{
			name: "user deleted closes 4001",
			change: func(t *testing.T, user pgtype.UUID, _ *pgxpool.Pool) {
				if _, err := rig.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, user); err != nil {
					t.Fatalf("delete user: %v", err)
				}
			},
			wantClose: websocket.StatusCode(4001),
		},
		{
			name:         "users store unreadable closes 1011",
			ownUsersPool: true,
			change: func(_ *testing.T, _ pgtype.UUID, usersPool *pgxpool.Pool) {
				usersPool.Close()
			},
			wantClose: websocket.StatusInternalError,
		},
		{
			name: "role changed to viewer stays open",
			change: func(t *testing.T, user pgtype.UUID, _ *pgxpool.Pool) {
				if _, err := rig.pool.Exec(ctx, `UPDATE users SET role = 'viewer' WHERE id = $1`, user); err != nil {
					t.Fatalf("change role: %v", err)
				}
			},
		},
		{
			name:   "unchanged user stays open",
			change: func(*testing.T, pgtype.UUID, *pgxpool.Pool) {},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessionRow, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			sid := sessionRow.ID.String()
			user := createTestUser(ctx, t, rig.pool, uniqueTestEmail("recheck"), "Recheck User")
			token := createTestWSTokenForUser(ctx, t, rig.pool, sessionRow.ID, user, time.Now().Add(time.Hour))

			wsURL := rig.wsURL
			var usersPool *pgxpool.Pool
			if tc.ownUsersPool {
				usersPool, err = pgxpool.NewWithConfig(ctx, rig.pool.Config())
				if err != nil {
					t.Fatalf("open a users pool: %v", err)
				}
				t.Cleanup(usersPool.Close)
				server, url := newDispatcherTestServer(rig.registry, rig.sessions, rig.turns, rig.sandboxes, rig.events, rig.artifacts, rig.wsTokens,
					narvipg.NewUserStore(usersPool), rig.hub, timeouts)
				t.Cleanup(server.Close)
				wsURL = url
			}

			conn := subscribeClient(ctx, t, wsURL, sid, token)
			defer func() { _ = conn.CloseNow() }()
			if got := rig.hub.Participants(sid); !slices.Contains(got, user.String()) {
				t.Fatalf("Hub.Participants after subscribe = %v, want it to hold %s", got, user.String())
			}

			tc.change(t, user, usersPool)

			if tc.wantClose != 0 {
				readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				_, data, err := conn.Read(readCtx)
				if err == nil {
					t.Fatalf("Read = %s, want the socket closed %d", data, tc.wantClose)
				}
				if got := websocket.CloseStatus(err); got != tc.wantClose {
					t.Fatalf("close code = %d (%v), want %d", got, err, tc.wantClose)
				}
				// No polling: the re-check unregistered the connection before
				// it wrote the close frame the client just read.
				if got := rig.hub.Participants(sid); len(got) != 0 {
					t.Errorf("Hub.Participants once the close is seen = %v, want none", got)
				}
				return
			}

			// At least three re-checks, then a broadcast still arrives.
			time.Sleep(5 * timeouts.ClientWSAccessRecheckInterval)
			const tick = `{"type":"tick","n":"after-rechecks"}`
			rig.hub.Broadcast(sid, json.RawMessage(tick))
			readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			_, data, err := conn.Read(readCtx)
			if err != nil {
				t.Fatalf("Read = %v (close code %d), want the socket open and the broadcast delivered", err, websocket.CloseStatus(err))
			}
			if string(data) != tick {
				t.Errorf("Read = %s, want the broadcast %s", data, tick)
			}
		})
	}
}
