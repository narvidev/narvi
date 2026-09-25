//go:build integration

package mcpauth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves the lock order written at the top of
// internal/adapters/outbound/postgres/mcpoauthgrant_store.go (technical
// plan §43.16) with the real handlers on both sides: a revocation racing a
// consent decision or a code exchange never deadlocks, never fails, and
// leaves nothing the issuance produced alive.
//
// Every race is deterministic. A third transaction, the blocker, holds the
// one row the side that goes first needs right after its parent lock --
// the issuance's own request or code, or a token inside the revocation's
// cascade -- so that side stops there, holding the parent. The other side
// is then started and must be SEEN in pg_locks queued on that parent row,
// behind the first side and nothing else. Only then is the blocker
// released. Every wait is on observed state, never on elapsed time.

// lockWaitObservationTimeout bounds how long a test waits to SEE a backend
// queued on a lock. It is a failure deadline, not a pacing delay: the poll
// returns on the first observation, normally within a few round trips.
const lockWaitObservationTimeout = 10 * time.Second

// issuance is the side of a race that issues something.
type issuance string

const (
	// consentApproval is POST /oauth/consent approving the member's
	// first grant for the client: the grant insert whose foreign-key
	// check on the client closed the deadlock this file guards against.
	consentApproval issuance = "consent"
	// codeExchange is POST /oauth/token for the member's code.
	codeExchange issuance = "code exchange"
)

// revocation is the side of a race that revokes something.
type revocation string

const (
	// clientDeletion is an administrator's DELETE /api/mcp-clients/{id}.
	clientDeletion revocation = "client deletion"
	// grantRevocation is the member's own DELETE
	// /api/me/mcp-authorizations/{id}.
	grantRevocation revocation = "grant revocation"
	// codeReuse is POST /oauth/token replaying the member's already-spent
	// code, which revokes the grant it was issued under.
	codeReuse revocation = "code reuse"
)

// parentTable is the row both sides of a race lock first: the one the
// side that goes second must be seen queued on.
func (v revocation) parentTable() string {
	if v == clientDeletion {
		return "mcp_oauth_clients"
	}
	return "mcp_oauth_grants"
}

// raceFixture is one race's starting state.
type raceFixture struct {
	member       sqlcgen.User
	memberCookie string
	adminCookie  string
	// heldToken is a live token under what the revocation deletes that the
	// issuance never touches: the row the blocker holds to stop a
	// revocation inside its cascade.
	heldToken string
	// grantID is the member's grant (code-exchange races only).
	grantID pgtype.UUID
	// requestID and nonce are the member's rendered, undecided
	// authorization request (consent races); code is the member's
	// unexchanged authorization code (code-exchange races). verifier is
	// the PKCE verifier behind either.
	requestID, nonce, code, verifier string
	// spentCode is the code heldToken was exchanged for, and
	// spentVerifier its PKCE verifier (code-exchange races): replaying it
	// is the code-reuse revocation.
	spentCode, spentVerifier string
}

func newRaceFixture(t *testing.T, r *asRig, issuer issuance) raceFixture {
	t.Helper()
	var f raceFixture
	f.member, f.memberCookie = r.newUser(t, sqlcgen.UserRoleMember)
	_, f.adminCookie = r.newUser(t, sqlcgen.UserRoleAdmin)
	f.verifier = newVerifier(t)
	switch issuer {
	case consentApproval:
		// The member holds no grant, so approving inserts one. The held
		// token belongs to another user of the same client.
		_, bystanderCookie := r.newUser(t, sqlcgen.UserRoleMember)
		f.heldToken, _ = r.issueToken(t, bystanderCookie, "mcp:read")
		f.requestID = r.startConsent(t, r.authorizeParams(f.verifier), f.memberCookie)
		if _, f.nonce = r.renderConsent(t, f.requestID, f.memberCookie); f.nonce == "" {
			t.Fatal("consent page did not render")
		}
	case codeExchange:
		f.spentVerifier = newVerifier(t)
		f.spentCode = r.approve(t, r.authorizeParams(f.spentVerifier), f.memberCookie, "mcp:read").Query().Get("code")
		rec := r.exchange(r.exchangeForm(f.spentCode, f.spentVerifier), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("first exchange: status %d body %s", rec.Code, rec.Body.String())
		}
		f.heldToken = decodeToken(t, rec).AccessToken
		f.code = r.approve(t, r.authorizeParams(f.verifier), f.memberCookie, "mcp:read").Query().Get("code")
		ids := r.grantIDs(t, f.member.ID)
		if len(ids) != 1 {
			t.Fatalf("member grants = %v, want exactly one", ids)
		}
		if err := f.grantID.Scan(ids[0]); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f raceFixture) issue(r *asRig, issuer issuance) *httptest.ResponseRecorder {
	if issuer == consentApproval {
		form := url.Values{"request": {f.requestID}, "nonce": {f.nonce}, "decision": {"approve"}, "scope": {"mcp:read"}}
		return r.postConsent(form, sameOriginHeaders(), f.memberCookie)
	}
	return r.exchange(r.exchangeForm(f.code, f.verifier), nil)
}

func (f raceFixture) revoke(r *asRig, revoker revocation) *httptest.ResponseRecorder {
	switch revoker {
	case grantRevocation:
		return r.do(http.MethodDelete, "/api/me/mcp-authorizations/"+f.grantID.String(), "", nil, f.memberCookie)
	case codeReuse:
		return r.exchange(r.exchangeForm(f.spentCode, f.spentVerifier), nil)
	default:
		return r.do(http.MethodDelete, "/api/mcp-clients/"+r.client.ID.String(), "", nil, f.adminCookie)
	}
}

// assertRevoked: the revocation succeeded -- 204 from a Settings route; a
// replayed code is refused invalid_grant, and its grant's revocation is
// audited with reason code_reuse.
func assertRevoked(ctx context.Context, t *testing.T, r *asRig, f raceFixture, revoker revocation, rec *httptest.ResponseRecorder) {
	t.Helper()
	if revoker != codeReuse {
		if rec.Code != http.StatusNoContent {
			t.Errorf("%s: status %d body %s, want 204", revoker, rec.Code, rec.Body.String())
		}
		return
	}
	if body := decodeToken(t, rec); rec.Code != http.StatusBadRequest || body.Error != "invalid_grant" {
		t.Errorf("%s: status %d body %s, want 400 invalid_grant", revoker, rec.Code, rec.Body.String())
	}
	var reused int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'mcp_authorization.revoked' AND resource_id = $1 AND detail_json->>'reason' = 'code_reuse'`, f.grantID.String()).Scan(&reused); err != nil || reused != 1 {
		t.Errorf("code_reuse revocation audit rows for the grant = %d (err %v), want 1", reused, err)
	}
}

// gate is the row the blocker holds: the child the first side locks right
// after its parent.
func (f raceFixture) gate(issuer issuance, issuanceFirst bool) (string, any) {
	switch {
	case !issuanceFirst:
		return `SELECT 1 FROM mcp_oauth_access_tokens WHERE token_hash = $1 FOR UPDATE`, platform.HashToken(f.heldToken)
	case issuer == consentApproval:
		return `SELECT 1 FROM mcp_oauth_authorization_requests WHERE id = $1::uuid FOR UPDATE`, f.requestID
	default:
		return `SELECT 1 FROM mcp_oauth_authorization_codes WHERE code_hash = $1 FOR UPDATE`, platform.HashToken(f.code)
	}
}

// TestLockOrder_RevocationRacingIssuance: whichever side of an (issuance,
// revocation) pair takes the shared parent row first, the other queues on
// that parent -- never on a child -- and once released: no transaction is
// aborted (a deadlock victim answers 500 and logs SQLSTATE 40P01), the
// revocation succeeds, and nothing the issuance produced is left usable
// (technical plan §43.16). An issuance that went first is revoked by the
// revocation it raced; one that went second finds its parent gone and
// issues nothing.
func TestLockOrder_RevocationRacingIssuance(t *testing.T) {
	for _, tc := range []struct {
		name          string
		issuer        issuance
		revoker       revocation
		issuanceFirst bool
	}{
		{"ConsentHoldsClient_ClientDeletionQueues", consentApproval, clientDeletion, true},
		{"ClientDeletionHoldsClient_ConsentQueues", consentApproval, clientDeletion, false},
		{"ExchangeHoldsGrant_GrantRevocationQueues", codeExchange, grantRevocation, true},
		{"GrantRevocationHoldsGrant_ExchangeQueues", codeExchange, grantRevocation, false},
		{"ExchangeHoldsClient_ClientDeletionQueues", codeExchange, clientDeletion, true},
		{"ClientDeletionHoldsClient_ExchangeQueues", codeExchange, clientDeletion, false},
		{"ExchangeHoldsGrant_CodeReuseQueues", codeExchange, codeReuse, true},
		{"CodeReuseHoldsGrant_ExchangeQueues", codeExchange, codeReuse, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := newASRig(t)
			f := newRaceFixture(t, r, tc.issuer)
			errs := captureErrorLog(t)

			blocker, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var blockerPID int32
			if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
				t.Fatal(err)
			}
			gateSQL, gateArg := f.gate(tc.issuer, tc.issuanceFirst)
			if tag, err := blocker.Exec(ctx, gateSQL, gateArg); err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("blocker: hold the gate row: err %v, rows %d", err, tag.RowsAffected())
			}

			var eg errgroup.Group
			release := func() {
				_ = blocker.Rollback(ctx)
				_ = eg.Wait()
			}
			defer release()
			var issued, revoked *httptest.ResponseRecorder
			issuerDone, revokerDone := make(chan struct{}), make(chan struct{})
			startIssuer := func() {
				eg.Go(func() error { defer close(issuerDone); issued = f.issue(r, tc.issuer); return nil })
			}
			startRevoker := func() {
				eg.Go(func() error { defer close(revokerDone); revoked = f.revoke(r, tc.revoker); return nil })
			}
			first, second := string(tc.issuer), string(tc.revoker)
			startFirst, startSecond := startIssuer, startRevoker
			firstDone, secondDone := issuerDone, revokerDone
			if !tc.issuanceFirst {
				first, second = second, first
				startFirst, startSecond = startSecond, startFirst
				firstDone, secondDone = secondDone, firstDone
			}

			startFirst()
			firstPID := waitForLockWaiter(ctx, t, r.pool, []int32{blockerPID}, firstDone)
			if firstPID == 0 {
				t.Fatalf("the %s finished without reaching the row the blocker holds", first)
			}
			if by := blockingPIDs(ctx, t, r.pool, firstPID); len(by) != 1 || by[0] != blockerPID {
				t.Fatalf("the %s (pid %d) is queued behind %v, want only the blocker (pid %d)", first, firstPID, by, blockerPID)
			}

			startSecond()
			if secondPID := waitForLockWaiter(ctx, t, r.pool, []int32{blockerPID, firstPID}, secondDone); secondPID == 0 {
				t.Errorf("the %s finished without queuing behind the %s", second, first)
			} else {
				by := blockingPIDs(ctx, t, r.pool, secondPID)
				on := queuedOnTables(ctx, t, r.pool, secondPID)
				if len(by) != 1 || by[0] != firstPID || len(on) != 1 || on[0] != tc.revoker.parentTable() {
					t.Errorf("the %s (pid %d) is queued on a row of %v behind %v, want on %s behind the %s (pid %d) alone",
						second, secondPID, on, by, tc.revoker.parentTable(), first, firstPID)
				}
			}
			release()

			if log := errs.String(); log != "" {
				t.Errorf("a handler failed (a deadlock victim logs SQLSTATE 40P01):\n%s", log)
			}
			assertRevoked(ctx, t, r, f, tc.revoker, revoked)
			assertIssuanceOutcome(t, r, f, tc.issuer, tc.issuanceFirst, issued)
			assertNothingSurvives(ctx, t, r, f, tc.revoker)
		})
	}
}

// assertIssuanceOutcome: an issuance that held the parent first succeeded,
// and what it issued no longer works; one that queued behind the
// revocation issued nothing.
func assertIssuanceOutcome(t *testing.T, r *asRig, f raceFixture, issuer issuance, succeeded bool, rec *httptest.ResponseRecorder) {
	t.Helper()
	switch {
	case issuer == consentApproval && succeeded:
		loc, err := url.Parse(rec.Header().Get("Location"))
		if rec.Code != http.StatusFound || err != nil || loc.Query().Get("code") == "" {
			t.Fatalf("consent: status %d Location %q, want a 302 carrying a code", rec.Code, rec.Header().Get("Location"))
		}
		if got := r.exchange(r.exchangeForm(loc.Query().Get("code"), f.verifier), nil); got.Code == http.StatusOK {
			t.Errorf("the code the consent issued was still exchangeable after the revocation: %s", got.Body.String())
		}
	case issuer == consentApproval:
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Errorf("consent: status %d Location %q, want a 400 page and no redirect", rec.Code, rec.Header().Get("Location"))
		}
	case succeeded:
		if rec.Code != http.StatusOK {
			t.Fatalf("exchange: status %d body %s, want 200", rec.Code, rec.Body.String())
		}
		if got := r.callMCP(decodeToken(t, rec).AccessToken); got != http.StatusUnauthorized {
			t.Errorf("the token the exchange issued answers %d at /mcp after the revocation, want 401", got)
		}
	default:
		if body := decodeToken(t, rec); rec.Code != http.StatusBadRequest || body.Error != "invalid_grant" || body.AccessToken != "" {
			t.Errorf("exchange: status %d body %s, want 400 invalid_grant and no token", rec.Code, rec.Body.String())
		}
	}
}

// assertNothingSurvives: after the race, the member holds no grant, no code
// or token exists under anything the revocation removed (every one this
// fixture made), a deleted client is gone, and every authorization ever
// granted was audited as revoked.
func assertNothingSurvives(ctx context.Context, t *testing.T, r *asRig, f raceFixture, revoker revocation) {
	t.Helper()
	var grants, codes, tokens, unaudited int
	if err := r.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM mcp_oauth_grants WHERE user_id = $1),
		       (SELECT count(*) FROM mcp_oauth_authorization_codes),
		       (SELECT count(*) FROM mcp_oauth_access_tokens),
		       (SELECT count(*) FROM audit_log g
		         WHERE g.action = 'mcp_authorization.granted'
		           AND NOT EXISTS (SELECT 1 FROM audit_log v
		                            WHERE v.action = 'mcp_authorization.revoked' AND v.resource_id = g.resource_id))`,
		f.member.ID).Scan(&grants, &codes, &tokens, &unaudited); err != nil {
		t.Fatal(err)
	}
	if grants != 0 || codes != 0 || tokens != 0 {
		t.Errorf("after the race: member grants %d, codes %d, tokens %d, want none left", grants, codes, tokens)
	}
	if unaudited != 0 {
		t.Errorf("%d granted authorizations have no mcp_authorization.revoked row", unaudited)
	}
	if revoker == clientDeletion {
		if _, err := r.clients.GetByID(ctx, r.client.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("client after its deletion: err = %v, want pgx.ErrNoRows", err)
		}
	}
}

// waitForLockWaiter returns the pid of the one client backend of this
// database, outside exclude, that is queued on a lock -- polling
// pg_stat_activity back to back, never sleeping -- or 0 if done closes
// first (that side finished without ever queuing). The observation
// deadline failing the test is lockWaitObservationTimeout.
func waitForLockWaiter(ctx context.Context, t *testing.T, pool *pgxpool.Pool, exclude []int32, done <-chan struct{}) int32 {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, lockWaitObservationTimeout)
	defer cancel()
	for {
		select {
		case <-done:
			return 0
		default:
		}
		var waiting []int32
		if err := pool.QueryRow(waitCtx, `
			SELECT coalesce(array_agg(pid ORDER BY pid), '{}')
			FROM pg_stat_activity
			WHERE datname = current_database() AND backend_type = 'client backend'
			  AND wait_event_type = 'Lock' AND pid <> ALL($1::int4[])`, exclude).Scan(&waiting); err != nil {
			t.Fatalf("poll pg_stat_activity for a lock waiter: %v", err)
		}
		switch len(waiting) {
		case 0:
		case 1:
			return waiting[0]
		default:
			t.Fatalf("backends %v are all queued on locks, want exactly one", waiting)
		}
	}
}

// blockingPIDs is pg_blocking_pids(pid): the backends pid is queued behind.
func blockingPIDs(ctx context.Context, t *testing.T, pool *pgxpool.Pool, pid int32) []int32 {
	t.Helper()
	var by []int32
	if err := pool.QueryRow(ctx, `SELECT pg_blocking_pids($1)`, pid).Scan(&by); err != nil {
		t.Fatalf("pg_blocking_pids(%d): %v", pid, err)
	}
	return by
}

// queuedOnTables names the table of the row pid is queued on: a backend
// waiting for a row lock holds, or waits for, that row's heavyweight tuple
// lock, whichever transaction it then waits on.
func queuedOnTables(ctx context.Context, t *testing.T, pool *pgxpool.Pool, pid int32) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT relation::regclass::text FROM pg_locks WHERE pid = $1 AND locktype = 'tuple' ORDER BY 1`, pid)
	if err != nil {
		t.Fatalf("pg_locks for pid %d: %v", pid, err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("pg_locks for pid %d: %v", pid, err)
	}
	return tables
}
