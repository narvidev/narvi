//go:build integration

// The session read check (actorauthz.AuthorizeSessionRead, technical plan
// §6.2, §13.3) at the ws-token mint, and its agreement with the REST
// session read: the client socket must never be wider than REST.
package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/actorauthz"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/platform"
)

// TestSessionReadGate_AgreesWithRESTRead pins the session read check to
// the REST session read it stands for: for every role, with the account
// active, disabled or deleted, GET /api/sessions/{id} through
// auth.Middleware answers 200 exactly when AuthorizeSessionRead answers
// Allowed. If the two ever disagree, the client socket would let someone
// read a session REST refuses them, or the reverse.
func TestSessionReadGate_AgreesWithRESTRead(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	states := []struct {
		name        string
		apply       func(t *testing.T, user pgtype.UUID)
		wantAllowed bool
	}{
		{name: "active", apply: func(*testing.T, pgtype.UUID) {}, wantAllowed: true},
		{name: "disabled", apply: func(t *testing.T, user pgtype.UUID) {
			if _, err := rig.pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1`, user); err != nil {
				t.Fatalf("disable user: %v", err)
			}
		}},
		{name: "deleted", apply: func(t *testing.T, user pgtype.UUID) {
			if _, err := rig.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, user); err != nil {
				t.Fatalf("delete user: %v", err)
			}
		}},
	}

	for _, state := range states {
		for _, role := range authz.AllRoles {
			t.Run(state.name+" "+string(role), func(t *testing.T) {
				user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRole(role))
				state.apply(t, user.ID)

				status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String(), nil, nil, cookie)
				verdict := actorauthz.AuthorizeSessionRead(ctx, logger, "test", rig.users, rig.sessions, user.ID, session.ID)

				if (status == http.StatusOK) != (verdict == actorauthz.LinkedActorAllowed) {
					t.Fatalf("GET /api/sessions/{id} = %d but AuthorizeSessionRead = %v: the two must agree", status, verdict)
				}
				if got := verdict == actorauthz.LinkedActorAllowed; got != state.wantAllowed {
					t.Errorf("allowed = %v (GET %d, verdict %v), want %v", got, status, verdict, state.wantAllowed)
				}
			})
		}
	}
}

// TestMintWSToken_ReadGate pins the mint's session read check: an active
// user gets a token that expires WSTokenTTL from now; a user the check
// refuses gets 403 and no token, even when the handler is reached without
// auth.Middleware (which already refuses a disabled user); a check that
// cannot be made gets 500 and no token; a missing session is still 404.
func TestMintWSToken_ReadGate(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	timeouts := platform.DefaultTimeouts()

	closedPool, err := pgxpool.New(ctx, "postgres://narvi@127.0.0.1:1/narvi?connect_timeout=1")
	if err != nil {
		t.Fatalf("build the pool: %v", err)
	}
	closedPool.Close()

	// mintDirect calls the handler with user in the request context, as
	// auth.Middleware would put it, without the middleware's own checks.
	mintDirect := func(t *testing.T, users *narvipg.UserStore, user sqlcgen.User, sessionID string) *httptest.ResponseRecorder {
		t.Helper()
		router := chi.NewRouter()
		router.Post("/api/sessions/{sessionID}/ws-token", httpapi.MintWSToken(rig.sessions, rig.wsTokens, users, timeouts))
		req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sessionID+"/ws-token", nil)
		req = req.WithContext(platform.WithUser(req.Context(), platform.AuthenticatedUser{
			ID: user.ID.String(), Role: string(user.Role), Email: user.PrimaryEmail,
		}))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	tokensFor := func(t *testing.T, user pgtype.UUID) int {
		t.Helper()
		var n int
		if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM ws_tokens WHERE user_id = $1`, user).Scan(&n); err != nil {
			t.Fatalf("count ws_tokens: %v", err)
		}
		return n
	}

	tests := []struct {
		name       string
		mint       func(t *testing.T, user sqlcgen.User, cookie string) (int, *restdtos.WSTokenResponse)
		disable    bool
		wantStatus int
	}{
		{
			name: "active user gets a WSTokenTTL token",
			mint: func(t *testing.T, _ sqlcgen.User, cookie string) (int, *restdtos.WSTokenResponse) {
				var got restdtos.WSTokenResponse
				return rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/ws-token", []byte{}, &got, cookie), &got
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "disabled user reaching the handler gets 403",
			mint: func(t *testing.T, user sqlcgen.User, _ string) (int, *restdtos.WSTokenResponse) {
				return mintDirect(t, rig.users, user, session.ID.String()).Code, nil
			},
			disable:    true,
			wantStatus: http.StatusForbidden,
		},
		{
			name: "unreadable users store gets 500",
			mint: func(t *testing.T, user sqlcgen.User, _ string) (int, *restdtos.WSTokenResponse) {
				return mintDirect(t, narvipg.NewUserStore(closedPool), user, session.ID.String()).Code, nil
			},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name: "missing session gets 404",
			mint: func(t *testing.T, _ sqlcgen.User, cookie string) (int, *restdtos.WSTokenResponse) {
				return rig.doJSON(t, http.MethodPost, "/api/sessions/11111111-1111-1111-1111-111111111111/ws-token", []byte{}, nil, cookie), nil
			},
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			user, cookie := rig.createAuthenticatedUser(ctx, t)
			if tc.disable {
				if _, err := rig.pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1`, user.ID); err != nil {
					t.Fatalf("disable user: %v", err)
				}
			}

			before := time.Now()
			status, got := tc.mint(t, user, cookie)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", status, tc.wantStatus)
			}
			if tc.wantStatus != http.StatusOK {
				if n := tokensFor(t, user.ID); n != 0 {
					t.Errorf("ws_tokens rows for the user = %d, want 0", n)
				}
				return
			}
			wantExpiry := before.Add(timeouts.WSTokenTTL)
			if got.ExpiresAt.Sub(wantExpiry).Abs() > 5*time.Second {
				t.Errorf("ExpiresAt = %v, want within 5s of %v", got.ExpiresAt, wantExpiry)
			}
			if n := tokensFor(t, user.ID); n != 1 {
				t.Errorf("ws_tokens rows for the user = %d, want 1", n)
			}
		})
	}
}
