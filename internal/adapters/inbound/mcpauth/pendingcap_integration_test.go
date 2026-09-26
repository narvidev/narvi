//go:build integration

package mcpauth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/mcpclient"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves the authorization endpoint's pending-request cap
// (technical plan §43.14): a client holds at most
// MCPMaxPendingAuthorizationRequestsPerClient authorization requests
// waiting for a decision, the next is refused with a page and stores
// nothing, and the count and the insert run under the client's lock, so
// however many authorizations race, the cap is never exceeded.

// newCappedRig is newASRig on a deployment whose pending-request cap is
// maxPending.
func newCappedRig(t *testing.T, maxPending int) *asRig {
	t.Helper()
	to := platform.DefaultTimeouts()
	to.MCPMaxPendingAuthorizationRequestsPerClient = maxPending
	if err := to.Validate(); err != nil {
		t.Fatalf("the capped timeouts do not validate: %v", err)
	}
	return newASRigWith(t, rigOptions{mechanisms: mcpclient.Mechanisms{MetadataDocuments: true, DynamicRegistration: true}, timeouts: &to})
}

// pendingRequests counts clientID's authorization requests waiting for a
// decision: unexpired and not consumed.
func (r *asRig) pendingRequests(t *testing.T, clientID pgtype.UUID) int {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM mcp_oauth_authorization_requests
		WHERE client_id = $1 AND consumed_at IS NULL AND expires_at > now()`, clientID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// startAuthorization runs one valid authorization for the rig's client,
// signed out: stored, it is a 302 on to sign-in.
func (r *asRig) startAuthorization(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return r.authorize(r.authorizeParams(newVerifier(t)), "")
}

// assertStored: the authorization was stored and went on to sign-in.
func assertStored(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/sign-in?next=") {
		t.Fatalf("authorization: status %d Location %q, want a 302 on to sign-in", rec.Code, rec.Header().Get("Location"))
	}
}

// assertCapPage: the authorization was refused by the pending-request
// cap -- temporarily unavailable, as the 503 error page, never a redirect.
func assertCapPage(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Location") != "" || !strings.Contains(rec.Body.String(), "This app has too many sign-ins waiting") {
		t.Fatalf("authorization past the cap: status %d Location %q body %s, want the 503 page and no redirect", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
}

// holdAuthorization runs the authorization endpoint's own store
// transaction for the rig's client -- CreatePendingAuthorizationRequest:
// the client FOR NO KEY UPDATE, the count, the insert -- inside a
// test-held transaction, so it holds the client's lock, its request
// stored and uncommitted, until the test ends it.
func (r *asRig) holdAuthorization(ctx context.Context, t *testing.T, maxPending int) *heldTx {
	t.Helper()
	return holdInTx(ctx, t, r.pool, func(tx pgx.Tx) {
		if _, err := r.grants.WithTx(tx).CreatePendingAuthorizationRequest(ctx, sqlcgen.CreateMCPOAuthAuthorizationRequestParams{
			ClientID: r.client.ID, RedirectUri: loopbackRedirect, Scopes: []string{"mcp:read"},
			CodeChallenge: s256(newVerifier(t)), CodeChallengeMethod: "S256",
			Resource: r.server.Identifiers().Resource, ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
		}, maxPending); err != nil {
			t.Fatalf("held authorization: %v", err)
		}
	})
}

// TestAuthorize_PendingCapRefused: at most maxPending authorization
// requests of one client wait for a decision at once. The next is refused
// with the 503 page -- never a redirect -- and stores nothing; a decided or
// expired request frees its place, and another client is never affected.
// An authorization holding the client's lock makes the next one wait, and
// that one then counts what the first stored -- refused at the cap, stored
// below it -- and a concurrent flood never leaves more than maxPending
// pending, whatever the interleaving.
func TestAuthorize_PendingCapRefused(t *testing.T) {
	const maxPending = 3

	t.Run("the request past the cap is a page and stores nothing", func(t *testing.T) {
		r := newCappedRig(t, maxPending)
		errs := captureErrorLog(t)
		for i := range maxPending {
			assertStored(t, r.startAuthorization(t))
			if n := r.pendingRequests(t, r.client.ID); n != i+1 {
				t.Fatalf("after %d authorizations: %d pending", i+1, n)
			}
		}
		for range 2 {
			assertCapPage(t, r.startAuthorization(t))
		}
		if n := r.pendingRequests(t, r.client.ID); n != maxPending {
			t.Fatalf("pending after the refusals = %d, want %d: a refused authorization stored a request", n, maxPending)
		}
		if log := errs.String(); log != "" {
			t.Errorf("a refusal was logged as an error:\n%s", log)
		}
	})

	t.Run("a decided or expired request frees its place, and another client is unaffected", func(t *testing.T) {
		r := newCappedRig(t, maxPending)
		_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
		for range maxPending - 1 {
			assertStored(t, r.startAuthorization(t))
		}
		// The member's own approval takes the last place, and deciding it
		// frees that place again.
		r.approve(t, r.authorizeParams(newVerifier(t)), cookie, "mcp:read")
		assertStored(t, r.startAuthorization(t))
		assertCapPage(t, r.startAuthorization(t))

		other := r.newClient(t, "narvi_mcp_c_other", "Other Plugin", loopbackRedirect)
		assertStored(t, r.authorize(forClient(r.authorizeParams(newVerifier(t)), other.ClientID), ""))

		if _, err := r.pool.Exec(context.Background(), `
			UPDATE mcp_oauth_authorization_requests SET expires_at = now() - interval '1 second'
			WHERE id = (SELECT id FROM mcp_oauth_authorization_requests WHERE client_id = $1 AND consumed_at IS NULL ORDER BY created_at LIMIT 1)`, r.client.ID); err != nil {
			t.Fatal(err)
		}
		assertStored(t, r.startAuthorization(t))
		assertCapPage(t, r.startAuthorization(t))
	})

	// Deterministic, both outcomes: the held authorization stores the
	// request that brings the client to held pending, uncommitted; the next
	// authorization is SEEN queued on the client row behind it alone, and,
	// once it commits, counts that request -- the cap is decided on what is
	// committed after the wait, never on a count taken before it.
	for _, tc := range []struct {
		name      string
		committed int // pending before the held authorization
		refused   bool
	}{
		{"an authorization holding the client makes the next wait, then refuses it at the cap", maxPending - 1, true},
		{"an authorization holding the client makes the next wait, then stores it below the cap", maxPending - 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := newCappedRig(t, maxPending)
			errs := captureErrorLog(t)
			for range tc.committed {
				assertStored(t, r.startAuthorization(t))
			}
			hold := r.holdAuthorization(ctx, t, maxPending)

			var eg errgroup.Group
			var next *httptest.ResponseRecorder
			done := make(chan struct{})
			eg.Go(func() error { defer close(done); next = r.startAuthorization(t); return nil })
			pid := waitForLockWaiter(ctx, t, r.pool, []int32{hold.pid}, done)
			if pid == 0 {
				_ = hold.tx.Rollback(ctx)
				_ = eg.Wait()
				t.Fatalf("the authorization finished (status %d) without waiting for the one holding its client: the count and the insert are not under the client's lock", next.Code)
			}
			assertQueuedOnClientBehind(ctx, t, r.pool, pid, hold.pid)
			if err := hold.tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := eg.Wait(); err != nil {
				t.Fatal(err)
			}
			if tc.refused {
				assertCapPage(t, next)
			} else {
				assertStored(t, next)
			}
			if n := r.pendingRequests(t, r.client.ID); n != maxPending {
				t.Fatalf("pending = %d, want exactly %d", n, maxPending)
			}
			if log := errs.String(); log != "" {
				t.Errorf("a handler failed:\n%s", log)
			}
		})
	}

	t.Run("a concurrent flood never exceeds the cap", func(t *testing.T) {
		const flood = 60
		r := newCappedRig(t, maxPending)
		errs := captureErrorLog(t)
		results := make([]*httptest.ResponseRecorder, flood)
		var eg errgroup.Group
		for i := range flood {
			eg.Go(func() error { results[i] = r.startAuthorization(t); return nil })
		}
		if err := eg.Wait(); err != nil {
			t.Fatal(err)
		}
		stored, capped := 0, 0
		for _, rec := range results {
			switch {
			case rec.Code == http.StatusFound:
				stored++
			case rec.Code == http.StatusServiceUnavailable && rec.Header().Get("Location") == "":
				capped++
			default:
				t.Errorf("an authorization of the flood answered %d Location %q", rec.Code, rec.Header().Get("Location"))
			}
		}
		if stored != maxPending || capped != flood-maxPending {
			t.Fatalf("flood of %d: %d stored, %d refused at the cap; want %d and %d", flood, stored, capped, maxPending, flood-maxPending)
		}
		if n := r.pendingRequests(t, r.client.ID); n != maxPending {
			t.Fatalf("pending after the flood = %d, want exactly %d", n, maxPending)
		}
		if log := errs.String(); log != "" {
			t.Errorf("a handler failed:\n%s", log)
		}
	})
}
