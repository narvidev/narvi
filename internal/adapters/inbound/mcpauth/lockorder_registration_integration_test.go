//go:build integration

package mcpauth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// This file extends lockorder_integration_test.go's proof of the lock
// order (the top of internal/adapters/outbound/postgres/
// mcpoauthgrant_store.go, technical plan §43.16) to the four writers
// client registration adds (§43.15): a metadata document's re-fetch (the
// upsert of its client row), the unused-client sweep, and a dynamic
// registration. Same method: every race is made deterministic by holding
// the one row the side that goes first needs next -- or, where the side
// that goes first is a single statement that cannot be stopped midway,
// by running that exact store statement inside a test-held transaction --
// and the other side is then SEEN in pg_locks queued behind it alone, or
// seen finishing without ever queuing. Every wait is on observed state,
// never on elapsed time.

// newCIMDRaceRig is newASRig whose r.client is a metadata-document
// client, its document cached and fresh -- so every race fixture below
// runs the metadata-document path end to end -- and the fake fetcher
// serves a RENAMED document, so a re-fetch is visible.
func newCIMDRaceRig(t *testing.T) *asRig {
	t.Helper()
	r := newASRig(t)
	now := time.Now()
	c, err := r.clients.UpsertMetadataDocument(context.Background(), sqlcgen.UpsertMCPOAuthMetadataDocumentClientParams{
		ClientID:          docClientURL,
		ClientName:        "Editor Plugin",
		RedirectUris:      []string{loopbackRedirect, httpsRedirect},
		MetadataFetchedAt: pgtype.Timestamptz{Time: now, Valid: true},
		MetadataStaleAt:   pgtype.Timestamptz{Time: now.Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatalf("create the metadata-document client: %v", err)
	}
	r.client = c
	r.serveDocument("Editor Plugin (renamed)")
	return r
}

// refetchDocument runs GET /oauth/authorize for the rig's
// metadata-document client, whose cached document the caller made stale:
// the authorization endpoint re-fetches it and upserts the client row.
func (r *asRig) refetchDocument(t *testing.T) *httptest.ResponseRecorder {
	return r.authorize(forClient(r.authorizeParams(newVerifier(t)), docClientURL), "")
}

// assertRefetched: the re-fetching authorization went on to the consent
// flow (through sign-in: it carried no cookie), and the client carrying
// the URL now holds the renamed document.
func assertRefetched(t *testing.T, r *asRig, rec *httptest.ResponseRecorder) sqlcgen.McpOauthClient {
	t.Helper()
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/sign-in?next=") {
		t.Fatalf("re-fetching authorization: status %d Location %q, want a 302 on to sign-in", rec.Code, rec.Header().Get("Location"))
	}
	c, ok := r.clientRow(t, docClientURL)
	if !ok || c.ClientName != "Editor Plugin (renamed)" {
		t.Fatalf("client after the re-fetch = %+v (found %v), want the renamed document", c, ok)
	}
	return c
}

// waitingFor names the one lock pid is queued for: its lock type and, for
// a row's tuple lock, the row's table. A backend queued behind a row
// another transaction is deleting waits either on that row's tuple lock
// or directly on the deleting transaction (locktype "transactionid") --
// INSERT ... ON CONFLICT does the latter.
func waitingFor(ctx context.Context, t *testing.T, pool *pgxpool.Pool, pid int32) (locktype, table string) {
	t.Helper()
	if err := pool.QueryRow(ctx, `
		SELECT locktype, coalesce(relation::regclass::text, '')
		FROM pg_locks WHERE pid = $1 AND NOT granted
		ORDER BY locktype LIMIT 1`, pid).Scan(&locktype, &table); err != nil {
		t.Fatalf("pg_locks: the lock pid %d waits for: %v", pid, err)
	}
	return locktype, table
}

// heldTx is a test-held transaction running one production store
// statement, so the lock that statement takes is held until the test
// ends it.
type heldTx struct {
	tx  pgx.Tx
	pid int32
}

func holdInTx(ctx context.Context, t *testing.T, pool *pgxpool.Pool, run func(tx pgx.Tx)) *heldTx {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h := &heldTx{tx: tx}
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&h.pid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	run(tx)
	return h
}

// oldUnusedClient creates a client of kind with no grant and no request,
// registered -- and, for a metadata-document client, last fetched --
// longer ago than MCPDynamicClientUnusedTTL: a sweep candidate.
func (r *asRig) oldUnusedClient(t *testing.T, kind sqlcgen.McpOauthClientKind, clientID string) sqlcgen.McpOauthClient {
	t.Helper()
	ctx := context.Background()
	old := time.Now().Add(-platform.DefaultTimeouts().MCPDynamicClientUnusedTTL - time.Hour)
	var c sqlcgen.McpOauthClient
	var err error
	if kind == sqlcgen.McpOauthClientKindMetadataDocument {
		c, err = r.clients.UpsertMetadataDocument(ctx, sqlcgen.UpsertMCPOAuthMetadataDocumentClientParams{
			ClientID: clientID, ClientName: "Old", RedirectUris: []string{loopbackRedirect},
			MetadataFetchedAt: pgtype.Timestamptz{Time: old, Valid: true},
			MetadataStaleAt:   pgtype.Timestamptz{Time: old.Add(time.Hour), Valid: true},
		})
		r.documents.set(clientID, fakeDocument{body: metadataDocument(clientID, "Old, fetched again", loopbackRedirect)})
	} else {
		c, err = r.clients.Create(ctx, sqlcgen.CreateMCPOAuthClientParams{ClientID: clientID, Kind: kind, ClientName: "Old", RedirectUris: []string{loopbackRedirect}})
	}
	if err != nil {
		t.Fatalf("create client %s: %v", clientID, err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE mcp_oauth_clients SET created_at = $2 WHERE id = $1`, c.ID, old); err != nil {
		t.Fatal(err)
	}
	return c
}

// sweepCutoff is the unused-client sweep's own cutoff.
func sweepCutoff() time.Time {
	return time.Now().Add(-platform.DefaultTimeouts().MCPDynamicClientUnusedTTL)
}

// TestLockOrder_ClientRegistrationWriters proves the four client
// registration writers follow the one lock order and deadlock with
// nothing (technical plan §43.15/§43.16):
//
//   - A metadata document's re-fetch -- ONE statement, ONE row lock on the
//     client, FOR NO KEY UPDATE -- never queues behind a consent, a code
//     exchange, a refresh or a grant revocation holding the client FOR KEY
//     SHARE, and none of them queues behind it; it queues on the client row
//     behind a client deletion alone, and, the client gone, registers the
//     document afresh; a client deletion queues on the client row behind
//     it alone.
//   - The unused-client sweep and a re-fetch serialize on the client row:
//     a re-fetch behind the sweep registers the document afresh, and a
//     sweep behind a re-fetch re-checks the row and spares it. An
//     authorization of a client the sweep is deleting queues behind it and
//     is then refused with a page -- never a 500. A client whose consent
//     is in flight is never a sweep candidate at all.
//   - A dynamic registration inserts a new row and queues behind nothing.
func TestLockOrder_ClientRegistrationWriters(t *testing.T) {
	// A re-fetch racing a holder of the client's FOR KEY SHARE: whichever
	// goes first, neither waits for the other.
	for _, tc := range []struct {
		name        string
		fixture     issuance
		issuer      bool // the holder is the fixture's issuance; else revoker
		revoker     revocation
		refetchHeld bool // the re-fetch holds the client first (test-held upsert)
	}{
		{"ConsentHoldsClient_MetadataRefetchProceeds", consentApproval, true, "", false},
		{"ExchangeHoldsClient_MetadataRefetchProceeds", codeExchange, true, "", false},
		{"RefreshHoldsClient_MetadataRefetchProceeds", refreshExchange, true, "", false},
		{"GrantRevocationHoldsClient_MetadataRefetchProceeds", refreshExchange, false, grantRevocation, false},
		{"TokenRevocationHoldsClient_MetadataRefetchProceeds", refreshExchange, false, tokenRevocation, false},
		{"MetadataRefetchHoldsClient_ConsentProceeds", consentApproval, true, "", true},
		{"MetadataRefetchHoldsClient_ExchangeProceeds", codeExchange, true, "", true},
		{"MetadataRefetchHoldsClient_RefreshProceeds", refreshExchange, true, "", true},
		{"MetadataRefetchHoldsClient_GrantRevocationProceeds", refreshExchange, false, grantRevocation, true},
		{"MetadataRefetchHoldsClient_TokenRevocationProceeds", refreshExchange, false, tokenRevocation, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := newCIMDRaceRig(t)
			f := newRaceFixture(t, r, tc.fixture)
			r.staleDocument(t, docClientURL)
			errs := captureErrorLog(t)

			var eg errgroup.Group
			var held, refetched *httptest.ResponseRecorder
			heldDone, refetchDone := make(chan struct{}), make(chan struct{})
			runHolder := func() {
				eg.Go(func() error {
					defer close(heldDone)
					if tc.issuer {
						held = f.issue(r, tc.fixture)
					} else {
						held = f.revoke(r, tc.revoker)
					}
					return nil
				})
			}

			if tc.refetchHeld {
				// The re-fetch's own statement, held open: the client row
				// FOR NO KEY UPDATE, exactly what the upsert takes.
				hold := holdInTx(ctx, t, r.pool, func(tx pgx.Tx) {
					now := time.Now()
					if _, err := r.clients.WithTx(tx).UpsertMetadataDocument(ctx, sqlcgen.UpsertMCPOAuthMetadataDocumentClientParams{
						ClientID: docClientURL, ClientName: "Editor Plugin (renamed)", RedirectUris: []string{loopbackRedirect, httpsRedirect},
						MetadataFetchedAt: pgtype.Timestamptz{Time: now, Valid: true},
						MetadataStaleAt:   pgtype.Timestamptz{Time: now.Add(time.Hour), Valid: true},
					}); err != nil {
						t.Fatalf("held upsert: %v", err)
					}
				})
				runHolder()
				if pid := waitForLockWaiter(ctx, t, r.pool, []int32{hold.pid}, heldDone); pid != 0 {
					locktype, table := waitingFor(ctx, t, r.pool, pid)
					t.Fatalf("the %s queued (on a %s lock of %q, behind %v) behind a held metadata re-fetch", tc.name, locktype, table, blockingPIDs(ctx, t, r.pool, pid))
				}
				if err := hold.tx.Commit(ctx); err != nil {
					t.Fatal(err)
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
				gateSQL, gateArg := f.gate(tc.fixture, tc.issuer)
				if tag, err := blocker.Exec(ctx, gateSQL, gateArg); err != nil || tag.RowsAffected() != 1 {
					t.Fatalf("blocker: hold the gate row: err %v, rows %d", err, tag.RowsAffected())
				}
				release := func() { _ = blocker.Rollback(ctx); _ = eg.Wait() }
				defer release()

				runHolder()
				holderPID := waitForLockWaiter(ctx, t, r.pool, []int32{blockerPID}, heldDone)
				if holderPID == 0 {
					t.Fatalf("the holder finished without reaching the row the blocker holds")
				}
				eg.Go(func() error { defer close(refetchDone); refetched = r.refetchDocument(t); return nil })
				if pid := waitForLockWaiter(ctx, t, r.pool, []int32{blockerPID, holderPID}, refetchDone); pid != 0 {
					locktype, table := waitingFor(ctx, t, r.pool, pid)
					t.Fatalf("the metadata re-fetch queued (on a %s lock of %q, behind %v) behind a holder of the client's FOR KEY SHARE", locktype, table, blockingPIDs(ctx, t, r.pool, pid))
				}
				assertRefetched(t, r, refetched)
				release()
			}
			if err := eg.Wait(); err != nil {
				t.Fatal(err)
			}

			if log := errs.String(); log != "" {
				t.Errorf("a handler failed (a deadlock victim logs SQLSTATE 40P01):\n%s", log)
			}
			if tc.issuer {
				assertHolderIssued(t, tc.fixture, held)
			} else {
				assertRevoked(ctx, t, r, f, tc.revoker, held)
			}
		})
	}

	// A client deletion and a re-fetch serialize on the client row, in
	// either order.
	t.Run("ClientDeletionHoldsClient_MetadataRefetchQueues", func(t *testing.T) {
		ctx := context.Background()
		r := newCIMDRaceRig(t)
		f := newRaceFixture(t, r, refreshExchange)
		r.staleDocument(t, docClientURL)
		errs := captureErrorLog(t)

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

		var deleted, refetched *httptest.ResponseRecorder
		deletionDone, refetchDone := make(chan struct{}), make(chan struct{})
		eg.Go(func() error { defer close(deletionDone); deleted = f.revoke(r, clientDeletion); return nil })
		deletionPID := waitForLockWaiter(ctx, t, r.pool, []int32{blockerPID}, deletionDone)
		if deletionPID == 0 {
			t.Fatal("the client deletion finished without reaching the row the blocker holds")
		}
		eg.Go(func() error { defer close(refetchDone); refetched = r.refetchDocument(t); return nil })
		refetchPID := waitForLockWaiter(ctx, t, r.pool, []int32{blockerPID, deletionPID}, refetchDone)
		if refetchPID == 0 {
			t.Fatal("the metadata re-fetch finished without queuing behind the client deletion")
		}
		assertQueuedOnClientBehind(ctx, t, r.pool, refetchPID, deletionPID)
		release()

		if log := errs.String(); log != "" {
			t.Errorf("a handler failed:\n%s", log)
		}
		if deleted.Code != http.StatusNoContent {
			t.Fatalf("client deletion: status %d body %s, want 204", deleted.Code, deleted.Body.String())
		}
		fresh := assertRefetched(t, r, refetched)
		if fresh.ID == r.client.ID {
			t.Fatal("the re-fetch revived the deleted client row instead of registering the document afresh")
		}
		assertNothingSurvives(ctx, t, r, f, clientDeletion)
	})

	t.Run("MetadataRefetchHoldsClient_ClientDeletionQueues", func(t *testing.T) {
		ctx := context.Background()
		r := newCIMDRaceRig(t)
		f := newRaceFixture(t, r, refreshExchange)
		errs := captureErrorLog(t)

		hold := holdInTx(ctx, t, r.pool, func(tx pgx.Tx) {
			now := time.Now()
			if _, err := r.clients.WithTx(tx).UpsertMetadataDocument(ctx, sqlcgen.UpsertMCPOAuthMetadataDocumentClientParams{
				ClientID: docClientURL, ClientName: "Editor Plugin (renamed)", RedirectUris: []string{loopbackRedirect},
				MetadataFetchedAt: pgtype.Timestamptz{Time: now, Valid: true},
				MetadataStaleAt:   pgtype.Timestamptz{Time: now.Add(time.Hour), Valid: true},
			}); err != nil {
				t.Fatalf("held upsert: %v", err)
			}
		})
		var eg errgroup.Group
		var deleted *httptest.ResponseRecorder
		deletionDone := make(chan struct{})
		eg.Go(func() error { defer close(deletionDone); deleted = f.revoke(r, clientDeletion); return nil })
		deletionPID := waitForLockWaiter(ctx, t, r.pool, []int32{hold.pid}, deletionDone)
		if deletionPID == 0 {
			t.Fatal("the client deletion finished without queuing behind the held re-fetch")
		}
		assertQueuedOnClientBehind(ctx, t, r.pool, deletionPID, hold.pid)
		if err := hold.tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := eg.Wait(); err != nil {
			t.Fatal(err)
		}
		if log := errs.String(); log != "" {
			t.Errorf("a handler failed:\n%s", log)
		}
		if deleted.Code != http.StatusNoContent {
			t.Fatalf("client deletion: status %d body %s, want 204", deleted.Code, deleted.Body.String())
		}
		assertNothingSurvives(ctx, t, r, f, clientDeletion)
	})

	// The unused-client sweep and a re-fetch, in either order.
	t.Run("SweepHoldsClient_MetadataRefetchQueues", func(t *testing.T) {
		ctx := context.Background()
		r := newASRig(t)
		const unusedURL = "https://unused.example/client.json"
		old := r.oldUnusedClient(t, sqlcgen.McpOauthClientKindMetadataDocument, unusedURL)
		errs := captureErrorLog(t)

		hold := holdInTx(ctx, t, r.pool, func(tx pgx.Tx) {
			if n, err := r.clients.WithTx(tx).DeleteUnused(ctx, sweepCutoff()); err != nil || n != 1 {
				t.Fatalf("held sweep: deleted %d, err %v; want the one unused client", n, err)
			}
		})
		var eg errgroup.Group
		var refetched *httptest.ResponseRecorder
		refetchDone := make(chan struct{})
		eg.Go(func() error {
			defer close(refetchDone)
			refetched = r.authorize(forClient(r.authorizeParams(newVerifier(t)), unusedURL), "")
			return nil
		})
		refetchPID := waitForLockWaiter(ctx, t, r.pool, []int32{hold.pid}, refetchDone)
		if refetchPID == 0 {
			t.Fatal("the re-fetch finished without queuing behind the sweep deleting its client")
		}
		assertQueuedOnClientBehind(ctx, t, r.pool, refetchPID, hold.pid)
		if err := hold.tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := eg.Wait(); err != nil {
			t.Fatal(err)
		}
		if log := errs.String(); log != "" {
			t.Errorf("a handler failed:\n%s", log)
		}
		if refetched.Code != http.StatusFound {
			t.Fatalf("re-fetch behind the sweep: status %d body %s, want 302", refetched.Code, refetched.Body.String())
		}
		if c, ok := r.clientRow(t, unusedURL); !ok || c.ID == old.ID || c.ClientName != "Old, fetched again" {
			t.Fatalf("after the sweep and the re-fetch: client %+v (found %v), want the document registered afresh", c, ok)
		}
	})

	t.Run("MetadataRefetchHoldsClient_SweepQueuesAndSparesIt", func(t *testing.T) {
		ctx := context.Background()
		r := newASRig(t)
		const unusedURL = "https://unused.example/client.json"
		old := r.oldUnusedClient(t, sqlcgen.McpOauthClientKindMetadataDocument, unusedURL)
		errs := captureErrorLog(t)

		hold := holdInTx(ctx, t, r.pool, func(tx pgx.Tx) {
			now := time.Now()
			if _, err := r.clients.WithTx(tx).UpsertMetadataDocument(ctx, sqlcgen.UpsertMCPOAuthMetadataDocumentClientParams{
				ClientID: unusedURL, ClientName: "Old, fetched again", RedirectUris: []string{loopbackRedirect},
				MetadataFetchedAt: pgtype.Timestamptz{Time: now, Valid: true},
				MetadataStaleAt:   pgtype.Timestamptz{Time: now.Add(time.Hour), Valid: true},
			}); err != nil {
				t.Fatalf("held upsert: %v", err)
			}
		})
		var eg errgroup.Group
		var swept int64
		sweepDone := make(chan struct{})
		eg.Go(func() error {
			defer close(sweepDone)
			var err error
			swept, err = r.clients.DeleteUnused(ctx, sweepCutoff())
			return err
		})
		sweepPID := waitForLockWaiter(ctx, t, r.pool, []int32{hold.pid}, sweepDone)
		if sweepPID == 0 {
			t.Fatal("the sweep finished without queuing behind the re-fetch updating its candidate")
		}
		assertQueuedOnClientBehind(ctx, t, r.pool, sweepPID, hold.pid)
		if err := hold.tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := eg.Wait(); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if log := errs.String(); log != "" {
			t.Errorf("a handler failed:\n%s", log)
		}
		if swept != 0 {
			t.Fatalf("the sweep deleted %d clients, want 0: the re-fetched client is no longer unused", swept)
		}
		if c, ok := r.clientRow(t, unusedURL); !ok || c.ID != old.ID {
			t.Fatalf("after the re-fetch and the sweep: client %+v (found %v), want the same client kept", c, ok)
		}
	})

	t.Run("SweepHoldsClient_AuthorizationQueuesAndIsRefusedWithAPage", func(t *testing.T) {
		ctx := context.Background()
		r := newASRig(t)
		dynamic := r.oldUnusedClient(t, sqlcgen.McpOauthClientKindDynamic, "narvi_mcp_d_old")
		errs := captureErrorLog(t)

		hold := holdInTx(ctx, t, r.pool, func(tx pgx.Tx) {
			if n, err := r.clients.WithTx(tx).DeleteUnused(ctx, sweepCutoff()); err != nil || n != 1 {
				t.Fatalf("held sweep: deleted %d, err %v; want the one unused client", n, err)
			}
		})
		var eg errgroup.Group
		var authorized *httptest.ResponseRecorder
		authorizeDone := make(chan struct{})
		eg.Go(func() error {
			defer close(authorizeDone)
			authorized = r.authorize(forClient(r.authorizeParams(newVerifier(t)), dynamic.ClientID), "")
			return nil
		})
		authorizePID := waitForLockWaiter(ctx, t, r.pool, []int32{hold.pid}, authorizeDone)
		if authorizePID == 0 {
			t.Fatal("the authorization finished without queuing behind the sweep deleting its client")
		}
		assertQueuedOnClientBehind(ctx, t, r.pool, authorizePID, hold.pid)
		if err := hold.tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := eg.Wait(); err != nil {
			t.Fatal(err)
		}
		if log := errs.String(); log != "" {
			t.Errorf("a handler failed (the swept client surfaced as a server error):\n%s", log)
		}
		if authorized.Code != http.StatusBadRequest || authorized.Header().Get("Location") != "" {
			t.Fatalf("authorization of a client swept from under it: status %d Location %q, want a 400 page and no redirect", authorized.Code, authorized.Header().Get("Location"))
		}
	})

	t.Run("ConsentInFlight_SweepNeverTakesItsClient", func(t *testing.T) {
		ctx := context.Background()
		r := newASRig(t)
		dynamic := r.oldUnusedClient(t, sqlcgen.McpOauthClientKindDynamic, "narvi_mcp_d_consenting")
		_, cookie := r.newUser(t, sqlcgen.UserRoleMember)
		requestID := r.startConsent(t, forClient(r.authorizeParams(newVerifier(t)), dynamic.ClientID), cookie)
		_, nonce := r.renderConsent(t, requestID, cookie)
		errs := captureErrorLog(t)

		blocker, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var blockerPID int32
		if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
			t.Fatal(err)
		}
		if tag, err := blocker.Exec(ctx, `SELECT 1 FROM mcp_oauth_authorization_requests WHERE id = $1::uuid FOR UPDATE`, requestID); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("blocker: hold the request: err %v rows %d", err, tag.RowsAffected())
		}
		var eg errgroup.Group
		release := func() { _ = blocker.Rollback(ctx); _ = eg.Wait() }
		defer release()

		var decided *httptest.ResponseRecorder
		decisionDone, sweepDone := make(chan struct{}), make(chan struct{})
		eg.Go(func() error {
			defer close(decisionDone)
			decided = r.postConsent(url.Values{"request": {requestID}, "nonce": {nonce}, "decision": {"approve"}, "scope": {"mcp:read"}}, sameOriginHeaders(), cookie)
			return nil
		})
		decisionPID := waitForLockWaiter(ctx, t, r.pool, []int32{blockerPID}, decisionDone)
		if decisionPID == 0 {
			t.Fatal("the consent decision finished without reaching the request the blocker holds")
		}
		var swept int64
		eg.Go(func() error {
			defer close(sweepDone)
			var err error
			swept, err = r.clients.DeleteUnused(ctx, sweepCutoff())
			return err
		})
		if pid := waitForLockWaiter(ctx, t, r.pool, []int32{blockerPID, decisionPID}, sweepDone); pid != 0 {
			t.Fatalf("the sweep queued behind %v: a client with a consent in flight must not even be a candidate", blockingPIDs(ctx, t, r.pool, pid))
		}
		release()
		if log := errs.String(); log != "" {
			t.Errorf("a handler failed:\n%s", log)
		}
		if swept != 0 {
			t.Fatalf("the sweep deleted %d clients, want 0", swept)
		}
		if decided.Code != http.StatusFound || !strings.Contains(decided.Header().Get("Location"), "code=") {
			t.Fatalf("consent decision: status %d Location %q, want a 302 carrying a code", decided.Code, decided.Header().Get("Location"))
		}
		if _, ok := r.clientRow(t, dynamic.ClientID); !ok {
			t.Fatal("the client of the consent was swept")
		}
	})

	// A dynamic registration locks no existing row: it never queues, even
	// behind a client deletion holding its client FOR UPDATE mid-cascade.
	t.Run("ClientDeletionInFlight_RegistrationNeverQueues", func(t *testing.T) {
		ctx := context.Background()
		r := newASRig(t)
		f := newRaceFixture(t, r, refreshExchange)
		errs := captureErrorLog(t)

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

		var deleted *httptest.ResponseRecorder
		var registered int
		deletionDone, registerDone := make(chan struct{}), make(chan struct{})
		eg.Go(func() error { defer close(deletionDone); deleted = f.revoke(r, clientDeletion); return nil })
		deletionPID := waitForLockWaiter(ctx, t, r.pool, []int32{blockerPID}, deletionDone)
		if deletionPID == 0 {
			t.Fatal("the client deletion finished without reaching the row the blocker holds")
		}
		eg.Go(func() error {
			defer close(registerDone)
			registered, _, _, _ = r.postRegister(`{"client_name":"Desktop Assistant","redirect_uris":["`+loopbackRedirect+`"]}`, "application/json")
			return nil
		})
		if pid := waitForLockWaiter(ctx, t, r.pool, []int32{blockerPID, deletionPID}, registerDone); pid != 0 {
			t.Fatalf("the registration queued behind %v", blockingPIDs(ctx, t, r.pool, pid))
		}
		if registered != http.StatusCreated {
			t.Fatalf("registration: status %d, want 201", registered)
		}
		release()
		if log := errs.String(); log != "" {
			t.Errorf("a handler failed:\n%s", log)
		}
		if deleted.Code != http.StatusNoContent {
			t.Fatalf("client deletion: status %d, want 204", deleted.Code)
		}
	})
}

// assertQueuedOnClientBehind: pid is queued behind holder alone, on a
// client row -- its tuple lock, or the holding transaction itself (a wait
// on a row that transaction is deleting) -- never on a row of any table
// under the client.
func assertQueuedOnClientBehind(ctx context.Context, t *testing.T, pool *pgxpool.Pool, pid, holder int32) {
	t.Helper()
	by := blockingPIDs(ctx, t, pool, pid)
	locktype, table := waitingFor(ctx, t, pool, pid)
	onClient := (locktype == "tuple" && table == "mcp_oauth_clients") || locktype == "transactionid"
	if len(by) != 1 || by[0] != holder || !onClient {
		t.Fatalf("pid %d is queued on a %s lock of %q behind %v, want on an mcp_oauth_clients row behind pid %d alone", pid, locktype, table, by, holder)
	}
	if tables := queuedOnTables(ctx, t, pool, pid); len(tables) > 1 || (len(tables) == 1 && tables[0] != "mcp_oauth_clients") {
		t.Fatalf("pid %d holds or waits for tuple locks on %v, want only an mcp_oauth_clients row", pid, tables)
	}
}

// assertHolderIssued: an issuance that raced a metadata re-fetch issued
// exactly what it would have alone.
func assertHolderIssued(t *testing.T, issuer issuance, rec *httptest.ResponseRecorder) {
	t.Helper()
	if issuer == consentApproval {
		if loc, err := url.Parse(rec.Header().Get("Location")); rec.Code != http.StatusFound || err != nil || loc.Query().Get("code") == "" {
			t.Fatalf("consent: status %d Location %q, want a 302 carrying a code", rec.Code, rec.Header().Get("Location"))
		}
		return
	}
	if body := decodeToken(t, rec); rec.Code != http.StatusOK || body.AccessToken == "" {
		t.Fatalf("%s: status %d body %s, want 200 and a token", issuer, rec.Code, rec.Body.String())
	}
}
