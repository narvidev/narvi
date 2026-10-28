//go:build integration

package actorauthz_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/actorauthz"
	"github.com/narvidev/narvi/internal/domain/authz"
)

// TestAuthorizeSessionRead pins the session read check's verdict table
// (sessionread.go) against real Postgres: every role may read an existing
// session while its account is active; a disabled account, a token with no
// user, a user or a session that does not exist are each Denied, never
// Error; a store that cannot be read is Error.
func TestAuthorizeSessionRead(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	logger := discardLogger()
	users := narvipg.NewUserStore(pool)
	sessions := narvipg.NewSessionStore(pool)

	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// A pool on the same database, closed: every read through it fails
	// with a real error that is not ErrNoRows.
	closed, err := pgxpool.NewWithConfig(ctx, pool.Config())
	if err != nil {
		t.Fatalf("open a second pool: %v", err)
	}
	closed.Close()

	created := 0
	newUser := func(t *testing.T, role sqlcgen.UserRole, disabled bool) pgtype.UUID {
		t.Helper()
		created++
		user, err := users.Create(ctx, sqlcgen.CreateUserParams{
			PrimaryEmail: fmt.Sprintf("session-read-%d@example.com", created),
			DisplayName:  "Session Read User",
			Role:         role,
		})
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		if disabled {
			if _, err := pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1`, user.ID); err != nil {
				t.Fatalf("disable user: %v", err)
			}
		}
		return user.ID
	}
	var unknown pgtype.UUID
	if err := unknown.Scan("00000000-0000-0000-0000-000000000001"); err != nil {
		t.Fatalf("scan uuid: %v", err)
	}

	type row struct {
		name     string
		user     func(t *testing.T) pgtype.UUID
		session  pgtype.UUID
		users    *narvipg.UserStore
		sessions *narvipg.SessionStore
		want     actorauthz.LinkedActorVerdict
	}
	var tests []row
	for _, role := range authz.AllRoles {
		tests = append(tests, row{
			name:    "active " + string(role) + " allowed",
			user:    func(t *testing.T) pgtype.UUID { return newUser(t, sqlcgen.UserRole(role), false) },
			session: session.ID,
			want:    actorauthz.LinkedActorAllowed,
		})
	}
	tests = append(tests,
		row{
			name:    "disabled member denied",
			user:    func(t *testing.T) pgtype.UUID { return newUser(t, sqlcgen.UserRoleMember, true) },
			session: session.ID,
			want:    actorauthz.LinkedActorDenied,
		},
		row{
			name:    "disabled admin denied",
			user:    func(t *testing.T) pgtype.UUID { return newUser(t, sqlcgen.UserRoleAdmin, true) },
			session: session.ID,
			want:    actorauthz.LinkedActorDenied,
		},
		row{
			name:    "no user id denied",
			user:    func(*testing.T) pgtype.UUID { return pgtype.UUID{} },
			session: session.ID,
			want:    actorauthz.LinkedActorDenied,
		},
		row{
			name:    "unknown user denied",
			user:    func(*testing.T) pgtype.UUID { return unknown },
			session: session.ID,
			want:    actorauthz.LinkedActorDenied,
		},
		row{
			name:    "unknown session denied",
			user:    func(t *testing.T) pgtype.UUID { return newUser(t, sqlcgen.UserRoleAdmin, false) },
			session: unknown,
			want:    actorauthz.LinkedActorDenied,
		},
		row{
			name:    "unreadable users store is an error",
			user:    func(t *testing.T) pgtype.UUID { return newUser(t, sqlcgen.UserRoleMember, false) },
			session: session.ID,
			users:   narvipg.NewUserStore(closed),
			want:    actorauthz.LinkedActorError,
		},
		row{
			name:     "unreadable sessions store is an error",
			user:     func(t *testing.T) pgtype.UUID { return newUser(t, sqlcgen.UserRoleMember, false) },
			session:  session.ID,
			sessions: narvipg.NewSessionStore(closed),
			want:     actorauthz.LinkedActorError,
		},
	)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			userStore, sessionStore := users, sessions
			if tc.users != nil {
				userStore = tc.users
			}
			if tc.sessions != nil {
				sessionStore = tc.sessions
			}
			got := actorauthz.AuthorizeSessionRead(ctx, logger, "test", userStore, sessionStore, tc.user(t), tc.session)
			if got != tc.want {
				t.Errorf("AuthorizeSessionRead() = %v, want %v", got, tc.want)
			}
		})
	}
}
