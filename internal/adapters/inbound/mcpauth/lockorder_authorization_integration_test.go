//go:build integration

package mcpauth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// TestLockOrder_AuthorizationRequestWriter extends the lock-order proofs
// (the top of internal/adapters/outbound/postgres/mcpoauthgrant_store.go,
// technical plan §43.16) to the authorization endpoint's own transaction
// -- CreatePendingAuthorizationRequest: the client FOR NO KEY UPDATE, the
// count of its pending requests, the insert (§43.14's pending-request
// cap) -- raced, in both orders, against every other transaction that
// locks the client row. Same method as the other lock-order tests: the
// side that goes first is stopped holding the client -- the
// authorization's own statements held open in a test transaction, or a
// real handler stopped at the child row a blocker holds -- and the other
// is then SEEN queued behind it alone, or seen finishing without ever
// queuing. Every wait is on observed state, never on elapsed time.
//
//   - Against every holder of the client's FOR KEY SHARE -- a consent
//     decision, a code exchange, a refresh, and a grant's revocation by its
//     user, by an administrator or by its client (RFC 7009) -- neither
//     waits for the other, in either order: the cap's lock never slows an
//     issuance or a revocation, nor they an authorization.
//   - Against a client deletion, they serialize on the client row: an
//     authorization behind a deletion finds the client gone and is refused
//     with a page, never a 500; a deletion behind an authorization waits
//     for it to commit, then takes the request it stored with the client.
//   - Against a metadata document's re-fetch -- its upsert, or the record
//     of its failure -- they serialize on the client row, whichever goes
//     first, and each then goes on.
//
// Another authorization of the same client is TestAuthorize_PendingCapRefused's
// pair; the unused-client sweep is TestLockOrder_ClientRegistrationWriters'.
func TestLockOrder_AuthorizationRequestWriter(t *testing.T) {
	maxPending := platform.DefaultTimeouts().MCPMaxPendingAuthorizationRequestsPerClient

	for _, h := range []struct {
		name    string
		fixture issuance
		issuer  bool // the holder is the fixture's issuance; else revoker
		revoker revocation
	}{
		{"Consent", consentApproval, true, ""},
		{"Exchange", codeExchange, true, ""},
		{"Refresh", refreshExchange, true, ""},
		{"GrantRevocation", refreshExchange, false, grantRevocation},
		{"AdminRevocation", refreshExchange, false, adminRevocation},
		{"TokenRevocation", refreshExchange, false, tokenRevocation},
	} {
		for _, authorizationFirst := range []bool{true, false} {
			name := h.name + "HoldsClient_AuthorizationProceeds"
			if authorizationFirst {
				name = "AuthorizationHoldsClient_" + h.name + "Proceeds"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				r := newASRig(t)
				f := newRaceFixture(t, r, h.fixture)
				errs := captureErrorLog(t)
				runHolder := func() *httptest.ResponseRecorder {
					if h.issuer {
						return f.issue(r, h.fixture)
					}
					return f.revoke(r, h.revoker)
				}

				var eg errgroup.Group
				var held *httptest.ResponseRecorder
				if authorizationFirst {
					hold := r.holdAuthorization(ctx, t, maxPending)
					done := make(chan struct{})
					eg.Go(func() error { defer close(done); held = runHolder(); return nil })
					if pid := waitForLockWaiter(ctx, t, r.pool, []int32{hold.pid}, done); pid != 0 {
						locktype, table := waitingFor(ctx, t, r.pool, pid)
						t.Fatalf("the %s queued (on a %s lock of %q, behind %v) behind an authorization holding its client", h.name, locktype, table, blockingPIDs(ctx, t, r.pool, pid))
					}
					if err := hold.tx.Commit(ctx); err != nil {
						t.Fatalf("the held authorization: %v", err)
					}
				} else {
					blocker, err := r.pool.Begin(ctx)
					if err != nil {
						t.Fatal(err)
					}
					var blockerPID int32
					if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
						t.Fatal(err)
					}
					gateSQL, gateArg := f.gate(h.fixture, h.issuer)
					if tag, err := blocker.Exec(ctx, gateSQL, gateArg); err != nil || tag.RowsAffected() != 1 {
						t.Fatalf("blocker: hold the gate row: err %v, rows %d", err, tag.RowsAffected())
					}
					release := func() { _ = blocker.Rollback(ctx); _ = eg.Wait() }
					defer release()

					heldDone := make(chan struct{})
					eg.Go(func() error { defer close(heldDone); held = runHolder(); return nil })
					holderPID := waitForLockWaiter(ctx, t, r.pool, []int32{blockerPID}, heldDone)
					if holderPID == 0 {
						t.Fatalf("the %s finished without reaching the row the blocker holds", h.name)
					}
					var authorized *httptest.ResponseRecorder
					authDone := make(chan struct{})
					eg.Go(func() error { defer close(authDone); authorized = r.startAuthorization(t); return nil })
					if pid := waitForLockWaiter(ctx, t, r.pool, []int32{blockerPID, holderPID}, authDone); pid != 0 {
						locktype, table := waitingFor(ctx, t, r.pool, pid)
						t.Fatalf("the authorization queued (on a %s lock of %q, behind %v) behind a %s holding its client FOR KEY SHARE", locktype, table, blockingPIDs(ctx, t, r.pool, pid), h.name)
					}
					<-authDone
					assertStored(t, authorized)
					release()
				}
				if err := eg.Wait(); err != nil {
					t.Fatal(err)
				}
				if log := errs.String(); log != "" {
					t.Errorf("a handler failed (a deadlock victim logs SQLSTATE 40P01):\n%s", log)
				}
				if h.issuer {
					assertHolderIssued(t, h.fixture, held)
				} else {
					assertRevoked(ctx, t, r, f, h.revoker, held)
				}
			})
		}
	}

	t.Run("AuthorizationHoldsClient_ClientDeletionQueuesAndTakesTheRequest", func(t *testing.T) {
		ctx := context.Background()
		r := newASRig(t)
		f := newRaceFixture(t, r, refreshExchange)
		errs := captureErrorLog(t)
		hold := r.holdAuthorization(ctx, t, maxPending)

		var eg errgroup.Group
		var deleted *httptest.ResponseRecorder
		done := make(chan struct{})
		eg.Go(func() error { defer close(done); deleted = f.revoke(r, clientDeletion); return nil })
		pid := waitForLockWaiter(ctx, t, r.pool, []int32{hold.pid}, done)
		if pid == 0 {
			t.Fatal("the client deletion finished without queuing behind the authorization holding its client")
		}
		assertQueuedOnClientBehind(ctx, t, r.pool, pid, hold.pid)
		if err := hold.tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := eg.Wait(); err != nil {
			t.Fatal(err)
		}
		if log := errs.String(); log != "" {
			t.Errorf("a handler failed (a deadlock victim logs SQLSTATE 40P01):\n%s", log)
		}
		if deleted.Code != http.StatusNoContent {
			t.Fatalf("client deletion: status %d body %s, want 204", deleted.Code, deleted.Body.String())
		}
		var requests int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM mcp_oauth_authorization_requests WHERE client_id = $1`, r.client.ID).Scan(&requests); err != nil || requests != 0 {
			t.Fatalf("requests left under the deleted client = %d (err %v), want none: the committed one goes with its client", requests, err)
		}
		assertNothingSurvives(ctx, t, r, f, clientDeletion)
	})

	t.Run("ClientDeletionHoldsClient_AuthorizationQueuesAndIsRefusedWithAPage", func(t *testing.T) {
		ctx := context.Background()
		r := newASRig(t)
		f := newRaceFixture(t, r, refreshExchange)
		errs := captureErrorLog(t)

		// The blocker holds a token inside the deletion's cascade, so the
		// deletion stops there, holding the client FOR UPDATE.
		blocker, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var blockerPID int32
		if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
			t.Fatal(err)
		}
		gateSQL, gateArg := f.gate(refreshExchange, false)
		if tag, err := blocker.Exec(ctx, gateSQL, gateArg); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("blocker: hold the gate row: err %v, rows %d", err, tag.RowsAffected())
		}
		var eg errgroup.Group
		release := func() { _ = blocker.Rollback(ctx); _ = eg.Wait() }
		defer release()

		var deleted, authorized *httptest.ResponseRecorder
		deletionDone, authDone := make(chan struct{}), make(chan struct{})
		eg.Go(func() error { defer close(deletionDone); deleted = f.revoke(r, clientDeletion); return nil })
		deletionPID := waitForLockWaiter(ctx, t, r.pool, []int32{blockerPID}, deletionDone)
		if deletionPID == 0 {
			t.Fatal("the client deletion finished without reaching the row the blocker holds")
		}
		eg.Go(func() error { defer close(authDone); authorized = r.startAuthorization(t); return nil })
		authPID := waitForLockWaiter(ctx, t, r.pool, []int32{blockerPID, deletionPID}, authDone)
		if authPID == 0 {
			t.Fatal("the authorization finished without queuing behind the deletion of its client")
		}
		assertQueuedOnClientBehind(ctx, t, r.pool, authPID, deletionPID)
		release()

		if log := errs.String(); log != "" {
			t.Errorf("a handler failed (the deleted client surfaced as a server error):\n%s", log)
		}
		if deleted.Code != http.StatusNoContent {
			t.Fatalf("client deletion: status %d body %s, want 204", deleted.Code, deleted.Body.String())
		}
		if authorized.Code != http.StatusBadRequest || authorized.Header().Get("Location") != "" {
			t.Fatalf("authorization of a client deleted from under it: status %d Location %q, want a 400 page and no redirect", authorized.Code, authorized.Header().Get("Location"))
		}
		if _, err := r.clients.GetByID(ctx, r.client.ID); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("client after its deletion: err = %v, want pgx.ErrNoRows", err)
		}
	})

	for _, failing := range []bool{false, true} {
		failed, writer := "", "metadata re-fetch"
		if failing {
			failed, writer = "Failed", "failed metadata re-fetch"
		}

		t.Run("AuthorizationHoldsClient_"+failed+"MetadataRefetchQueues", func(t *testing.T) {
			ctx := context.Background()
			r := newCIMDRaceRig(t)
			r.staleDocument(t, docClientURL)
			if failing {
				r.documents.set(docClientURL, documentHostDown)
			}
			errs := captureErrorLog(t)
			hold := r.holdAuthorization(ctx, t, maxPending)

			var eg errgroup.Group
			var refetched *httptest.ResponseRecorder
			done := make(chan struct{})
			eg.Go(func() error { defer close(done); refetched = r.refetchDocument(t); return nil })
			pid := waitForLockWaiter(ctx, t, r.pool, []int32{hold.pid}, done)
			if pid == 0 {
				t.Fatalf("the %s finished without queuing behind the authorization holding its client", writer)
			}
			assertQueuedOnClientBehind(ctx, t, r.pool, pid, hold.pid)
			if err := hold.tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := eg.Wait(); err != nil {
				t.Fatal(err)
			}
			if log := errs.String(); log != "" {
				t.Errorf("a handler failed (a deadlock victim logs SQLSTATE 40P01):\n%s", log)
			}
			if failing {
				assertKeptAfterFailedRefetch(t, r, refetched)
			} else {
				assertRefetched(t, r, refetched)
			}
		})

		t.Run(failed+"MetadataRefetchHoldsClient_AuthorizationQueues", func(t *testing.T) {
			ctx := context.Background()
			r := newCIMDRaceRig(t)
			errs := captureErrorLog(t)
			var hold *heldTx
			if failing {
				hold, _ = r.holdRefetchFailure(ctx, t, docClientURL, platform.DefaultTimeouts().MCPClientMetadataCacheTTL)
			} else {
				// The re-fetch's own statement, held open: the client row
				// FOR NO KEY UPDATE, exactly what the upsert takes.
				hold = holdInTx(ctx, t, r.pool, func(tx pgx.Tx) {
					now := time.Now()
					if _, err := r.clients.WithTx(tx).UpsertMetadataDocument(ctx, sqlcgen.UpsertMCPOAuthMetadataDocumentClientParams{
						ClientID: docClientURL, ClientName: "Editor Plugin (renamed)", RedirectUris: []string{loopbackRedirect, httpsRedirect},
						MetadataFetchedAt: pgtype.Timestamptz{Time: now, Valid: true},
						MetadataStaleAt:   pgtype.Timestamptz{Time: now.Add(time.Hour), Valid: true},
					}); err != nil {
						t.Fatalf("held upsert: %v", err)
					}
				})
			}

			// The document is fresh as committed, so this authorization
			// fetches nothing: the client row it queues on is its
			// pending-request lock's.
			var eg errgroup.Group
			var authorized *httptest.ResponseRecorder
			done := make(chan struct{})
			eg.Go(func() error {
				defer close(done)
				authorized = r.authorize(forClient(r.authorizeParams(newVerifier(t)), docClientURL), "")
				return nil
			})
			pid := waitForLockWaiter(ctx, t, r.pool, []int32{hold.pid}, done)
			if pid == 0 {
				t.Fatalf("the authorization finished without queuing behind the %s holding its client", writer)
			}
			assertQueuedOnClientBehind(ctx, t, r.pool, pid, hold.pid)
			if err := hold.tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := eg.Wait(); err != nil {
				t.Fatal(err)
			}
			if log := errs.String(); log != "" {
				t.Errorf("a handler failed (a deadlock victim logs SQLSTATE 40P01):\n%s", log)
			}
			assertStored(t, authorized)
			if n := r.pendingRequests(t, r.client.ID); n != 1 {
				t.Fatalf("pending requests of the metadata-document client = %d, want the one just stored", n)
			}
		})
	}
}
