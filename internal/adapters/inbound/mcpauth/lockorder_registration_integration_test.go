//go:build integration

package mcpauth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/mcpclient"
	"github.com/narvidev/narvi/internal/platform"
)

// This file extends lockorder_integration_test.go's proof of the lock
// order (the top of internal/adapters/outbound/postgres/
// mcpoauthgrant_store.go, technical plan §43.16) to the four writers
// client registration adds (§43.15): a metadata document's re-fetch (the
// upsert of its client row), the record of a failed re-fetch
// (MarkMetadataRefetchFailed), the unused-client sweep, and a dynamic
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

// documentHostDown is a metadata document whose host answers nothing: a
// re-fetch of it fails.
var documentHostDown = fakeDocument{err: errors.New("document host unreachable")}

// holdRefetchFailure runs the failed re-fetch's own statement inside a
// test-held transaction -- MarkMetadataRefetchFailed, one UPDATE of the
// client row, FOR NO KEY UPDATE, exactly what keepAfterFailedRefetch runs
// -- for the client carrying clientIDURL: a failure now, and the grace
// refetchGraceEnd gives it under a cache lifetime of ttl. It returns the
// transaction and the row as the held statement left it.
func (r *asRig) holdRefetchFailure(ctx context.Context, t *testing.T, clientIDURL string, ttl time.Duration) (*heldTx, sqlcgen.McpOauthClient) {
	t.Helper()
	c, ok := r.clientRow(t, clientIDURL)
	if !ok {
		t.Fatalf("no client carries %s", clientIDURL)
	}
	var recorded sqlcgen.McpOauthClient
	hold := holdInTx(ctx, t, r.pool, func(tx pgx.Tx) {
		now := time.Now()
		graceEnd := now.Add(ttl)
		if bound := c.MetadataFetchedAt.Time.Add(2 * ttl); bound.Before(graceEnd) {
			graceEnd = bound
		}
		if !now.Before(graceEnd) {
			t.Fatalf("%s was last fetched at %v, too long ago for a failure to record any grace", clientIDURL, c.MetadataFetchedAt.Time)
		}
		var err error
		if recorded, err = r.clients.WithTx(tx).MarkMetadataRefetchFailed(ctx, c.ID, c.MetadataFetchedAt.Time, now, graceEnd); err != nil {
			t.Fatalf("held failed re-fetch: %v", err)
		}
	})
	return hold, recorded
}

// assertFailureRecorded: the client carrying clientIDURL still holds the
// document named name, and records a failed re-fetch whose grace runs past
// now.
func assertFailureRecorded(t *testing.T, r *asRig, clientIDURL, name string) sqlcgen.McpOauthClient {
	t.Helper()
	c, ok := r.clientRow(t, clientIDURL)
	if !ok || c.ClientName != name || !c.MetadataRefetchFailedAt.Valid || !c.MetadataStaleAt.Time.After(time.Now()) {
		t.Fatalf("client after the failed re-fetch = %+v (found %v), want %q kept, the failure recorded and its grace running", c, ok, name)
	}
	return c
}

// assertKeptAfterFailedRefetch: the authorization whose re-fetch failed
// went on to the consent flow from the cached document (through sign-in:
// it carried no cookie), which the client row still holds, the failure
// recorded.
func assertKeptAfterFailedRefetch(t *testing.T, r *asRig, rec *httptest.ResponseRecorder) sqlcgen.McpOauthClient {
	t.Helper()
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/sign-in?next=") {
		t.Fatalf("authorization whose re-fetch failed: status %d Location %q, want a 302 on to sign-in from the cached document", rec.Code, rec.Header().Get("Location"))
	}
	return assertFailureRecorded(t, r, docClientURL, "Editor Plugin")
}

// newLongCacheRig is newASRig on a deployment whose
// MCPClientMetadataCacheTTL (14 hours) is over half its
// MCPDynamicClientUnusedTTL (24 hours). That is a valid setting, and the
// only kind under which a failed re-fetch can meet the unused-client
// sweep: a sweep candidate was last fetched more than
// MCPDynamicClientUnusedTTL ago, and a failure records a grace only
// within two cache lifetimes of the last successful fetch. Under the
// default hour, the failure path writes nothing to such a client.
func newLongCacheRig(t *testing.T) (*asRig, time.Duration) {
	t.Helper()
	long := platform.DefaultTimeouts()
	long.MCPClientMetadataCacheTTL = 14 * time.Hour
	if err := long.Validate(); err != nil {
		t.Fatalf("the long cache lifetime does not validate: %v", err)
	}
	r := newASRigWith(t, rigOptions{mechanisms: mcpclient.Mechanisms{MetadataDocuments: true, DynamicRegistration: true}, timeouts: &long})
	return r, long.MCPClientMetadataCacheTTL
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
	old := time.Now().Add(-platform.DefaultTimeouts().MCPDynamicClientUnusedTTL - 2*time.Hour)
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
//   - The record of a failed re-fetch is the same one statement and row
//     lock, and is raced against every one of those, in both orders: it
//     never queues behind a holder of the client's FOR KEY SHARE, nor does
//     any queue behind it; behind a client deletion or the sweep it queues
//     on the client row alone, then finds the client gone, and the
//     authorization is refused with a page, never a 500; either queues
//     behind it alone and then goes on. It and a successful re-fetch
//     serialize on the client row whichever goes first, and the newer
//     fetch stands; two failures serialize too, and the second uses the
//     first one's grace, unextended. The sweep pair runs under a cache
//     lifetime over half MCPDynamicClientUnusedTTL (newLongCacheRig): under
//     the default, a failure writes nothing to a sweep candidate.
//   - The unused-client sweep and a re-fetch serialize on the client row:
//     a re-fetch behind the sweep registers the document afresh, and a
//     sweep behind a re-fetch re-checks the row and spares it. An
//     authorization of a client the sweep is deleting queues behind it and
//     is then refused with a page -- never a 500; a sweep reaching a client
//     whose authorization request or grant insert is in flight queues
//     behind it, and spares the client once the insert commits. A client
//     whose consent is in flight is never a sweep candidate at all. On the
//     pool-built store production uses, the sweep's two statements are
//     one transaction: an insert under a candidate that comes between
//     them waits for the sweep, then fails its foreign-key check -- it
//     never commits under a client the second statement then deletes.
//   - A dynamic registration inserts a new row and queues behind nothing,
//     and nothing -- a client deletion, a failed re-fetch -- queues behind
//     it.
func TestLockOrder_ClientRegistrationWriters(t *testing.T) {
	// A re-fetch racing a holder of the client's FOR KEY SHARE: whichever
	// goes first, neither waits for the other. Each race runs twice: the
	// fetch succeeding, when the re-fetch's writer is the upsert, and the
	// fetch failing, when it is the record of that failure -- the same one
	// statement and one row lock.
	ttl := platform.DefaultTimeouts().MCPClientMetadataCacheTTL
	for _, tc := range []struct {
		name        string
		fixture     issuance
		issuer      bool // the holder is the fixture's issuance; else revoker
		revoker     revocation
		refetchHeld bool // the re-fetch holds the client first (its test-held statement)
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
		for _, failing := range []bool{false, true} {
			name, writer := tc.name, "metadata re-fetch"
			if failing {
				name, writer = strings.Replace(tc.name, "MetadataRefetch", "FailedMetadataRefetch", 1), "failed metadata re-fetch"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				r := newCIMDRaceRig(t)
				f := newRaceFixture(t, r, tc.fixture)
				r.staleDocument(t, docClientURL)
				if failing {
					r.documents.set(docClientURL, documentHostDown)
				}
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
					var hold *heldTx
					if failing {
						hold, _ = r.holdRefetchFailure(ctx, t, docClientURL, ttl)
					} else {
						// The re-fetch's own statement, held open: the client
						// row FOR NO KEY UPDATE, exactly what the upsert takes.
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
					runHolder()
					if pid := waitForLockWaiter(ctx, t, r.pool, []int32{hold.pid}, heldDone); pid != 0 {
						locktype, table := waitingFor(ctx, t, r.pool, pid)
						t.Fatalf("the %s queued (on a %s lock of %q, behind %v) behind a held %s", name, locktype, table, blockingPIDs(ctx, t, r.pool, pid), writer)
					}
					if err := hold.tx.Commit(ctx); err != nil {
						t.Fatal(err)
					}
					if failing {
						assertFailureRecorded(t, r, docClientURL, "Editor Plugin")
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
						t.Fatalf("the %s queued (on a %s lock of %q, behind %v) behind a holder of the client's FOR KEY SHARE", writer, locktype, table, blockingPIDs(ctx, t, r.pool, pid))
					}
					if failing {
						assertKeptAfterFailedRefetch(t, r, refetched)
					} else {
						assertRefetched(t, r, refetched)
					}
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
	}

	// A client deletion and a re-fetch serialize on the client row, in
	// either order, the fetch succeeding or failing. Queued behind the
	// deletion, a successful re-fetch registers the document afresh; a
	// failed one finds the client gone and is refused with a page.
	for _, failing := range []bool{false, true} {
		failed := ""
		if failing {
			failed = "Failed"
		}
		t.Run("ClientDeletionHoldsClient_"+failed+"MetadataRefetchQueues", func(t *testing.T) {
			ctx := context.Background()
			r := newCIMDRaceRig(t)
			f := newRaceFixture(t, r, refreshExchange)
			r.staleDocument(t, docClientURL)
			if failing {
				r.documents.set(docClientURL, documentHostDown)
			}
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
			if failing {
				assertPageNotRedirect(t, "a failed re-fetch behind the client's deletion", refetched)
				if c, ok := r.clientRow(t, docClientURL); ok {
					t.Fatalf("a failed re-fetch behind the client's deletion left client %+v, want none", c)
				}
			} else {
				fresh := assertRefetched(t, r, refetched)
				if fresh.ID == r.client.ID {
					t.Fatal("the re-fetch revived the deleted client row instead of registering the document afresh")
				}
			}
			assertNothingSurvives(ctx, t, r, f, clientDeletion)
		})

		t.Run(failed+"MetadataRefetchHoldsClient_ClientDeletionQueues", func(t *testing.T) {
			ctx := context.Background()
			r := newCIMDRaceRig(t)
			f := newRaceFixture(t, r, refreshExchange)
			errs := captureErrorLog(t)

			var hold *heldTx
			if failing {
				r.staleDocument(t, docClientURL)
				hold, _ = r.holdRefetchFailure(ctx, t, docClientURL, ttl)
			} else {
				hold = holdInTx(ctx, t, r.pool, func(tx pgx.Tx) {
					now := time.Now()
					if _, err := r.clients.WithTx(tx).UpsertMetadataDocument(ctx, sqlcgen.UpsertMCPOAuthMetadataDocumentClientParams{
						ClientID: docClientURL, ClientName: "Editor Plugin (renamed)", RedirectUris: []string{loopbackRedirect},
						MetadataFetchedAt: pgtype.Timestamptz{Time: now, Valid: true},
						MetadataStaleAt:   pgtype.Timestamptz{Time: now.Add(time.Hour), Valid: true},
					}); err != nil {
						t.Fatalf("held upsert: %v", err)
					}
				})
			}
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
	}

	// The unused-client sweep and a re-fetch, in either order, the fetch
	// succeeding or failing. A failed re-fetch can only meet the sweep on
	// a deployment whose cache lifetime is over half
	// MCPDynamicClientUnusedTTL, so those two races run on one
	// (newLongCacheRig).
	for _, failing := range []bool{false, true} {
		failed := ""
		if failing {
			failed = "Failed"
		}
		rig := func(t *testing.T) (*asRig, time.Duration) {
			if failing {
				return newLongCacheRig(t)
			}
			return newASRig(t), ttl
		}
		t.Run("SweepHoldsClient_"+failed+"MetadataRefetchQueues", func(t *testing.T) {
			ctx := context.Background()
			r, _ := rig(t)
			const unusedURL = "https://unused.example/client.json"
			old := r.oldUnusedClient(t, sqlcgen.McpOauthClientKindMetadataDocument, unusedURL)
			if failing {
				r.documents.set(unusedURL, documentHostDown)
			}
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
			if failing {
				assertPageNotRedirect(t, "a failed re-fetch behind the sweep", refetched)
				if c, ok := r.clientRow(t, unusedURL); ok {
					t.Fatalf("a failed re-fetch behind the sweep left client %+v, want none", c)
				}
				return
			}
			if refetched.Code != http.StatusFound {
				t.Fatalf("re-fetch behind the sweep: status %d body %s, want 302", refetched.Code, refetched.Body.String())
			}
			if c, ok := r.clientRow(t, unusedURL); !ok || c.ID == old.ID || c.ClientName != "Old, fetched again" {
				t.Fatalf("after the sweep and the re-fetch: client %+v (found %v), want the document registered afresh", c, ok)
			}
		})

		t.Run(failed+"MetadataRefetchHoldsClient_SweepQueuesAndSparesIt", func(t *testing.T) {
			ctx := context.Background()
			r, cacheTTL := rig(t)
			const unusedURL = "https://unused.example/client.json"
			old := r.oldUnusedClient(t, sqlcgen.McpOauthClientKindMetadataDocument, unusedURL)
			errs := captureErrorLog(t)

			var hold *heldTx
			if failing {
				hold, _ = r.holdRefetchFailure(ctx, t, unusedURL, cacheTTL)
			} else {
				hold = holdInTx(ctx, t, r.pool, func(tx pgx.Tx) {
					now := time.Now()
					if _, err := r.clients.WithTx(tx).UpsertMetadataDocument(ctx, sqlcgen.UpsertMCPOAuthMetadataDocumentClientParams{
						ClientID: unusedURL, ClientName: "Old, fetched again", RedirectUris: []string{loopbackRedirect},
						MetadataFetchedAt: pgtype.Timestamptz{Time: now, Valid: true},
						MetadataStaleAt:   pgtype.Timestamptz{Time: now.Add(time.Hour), Valid: true},
					}); err != nil {
						t.Fatalf("held upsert: %v", err)
					}
				})
			}
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
			if failing {
				assertFailureRecorded(t, r, unusedURL, "Old")
			}
		})
	}

	// A successful re-fetch and a failed one serialize on the client row,
	// in either order, and the newer fetch stands: a failure queued behind
	// the upsert records nothing over it and goes on from the newer
	// document; an upsert queued behind a failure's record clears it.
	t.Run("MetadataRefetchHoldsClient_FailedMetadataRefetchQueuesAndUsesIt", func(t *testing.T) {
		ctx := context.Background()
		r := newCIMDRaceRig(t)
		r.staleDocument(t, docClientURL)
		r.documents.set(docClientURL, documentHostDown)
		errs := captureErrorLog(t)

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
		var eg errgroup.Group
		var failedRefetch *httptest.ResponseRecorder
		failedDone := make(chan struct{})
		eg.Go(func() error { defer close(failedDone); failedRefetch = r.refetchDocument(t); return nil })
		failedPID := waitForLockWaiter(ctx, t, r.pool, []int32{hold.pid}, failedDone)
		if failedPID == 0 {
			t.Fatal("the failed re-fetch finished without queuing behind the re-fetch updating its client")
		}
		assertQueuedOnClientBehind(ctx, t, r.pool, failedPID, hold.pid)
		if err := hold.tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := eg.Wait(); err != nil {
			t.Fatal(err)
		}
		if log := errs.String(); log != "" {
			t.Errorf("a handler failed:\n%s", log)
		}
		if c := assertRefetched(t, r, failedRefetch); c.MetadataRefetchFailedAt.Valid {
			t.Fatalf("a failure queued behind a newer successful fetch was recorded over it: %+v", c)
		}
	})

	t.Run("FailedMetadataRefetchHoldsClient_MetadataRefetchQueuesAndClearsIt", func(t *testing.T) {
		ctx := context.Background()
		r := newCIMDRaceRig(t)
		r.staleDocument(t, docClientURL)
		errs := captureErrorLog(t)

		hold, _ := r.holdRefetchFailure(ctx, t, docClientURL, ttl)
		var eg errgroup.Group
		var refetched *httptest.ResponseRecorder
		refetchDone := make(chan struct{})
		eg.Go(func() error { defer close(refetchDone); refetched = r.refetchDocument(t); return nil })
		refetchPID := waitForLockWaiter(ctx, t, r.pool, []int32{hold.pid}, refetchDone)
		if refetchPID == 0 {
			t.Fatal("the re-fetch finished without queuing behind the failed re-fetch's record")
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
		if c := assertRefetched(t, r, refetched); c.MetadataRefetchFailedAt.Valid {
			t.Fatalf("a successful fetch behind a failure's record left the failure recorded: %+v", c)
		}
	})

	// Two failed re-fetches of one client serialize on its row: the second,
	// queued behind the first's record, records nothing and uses the grace
	// the first started, neither restarted nor extended.
	t.Run("FailedMetadataRefetchHoldsClient_FailedMetadataRefetchQueuesAndKeepsItsGrace", func(t *testing.T) {
		ctx := context.Background()
		r := newCIMDRaceRig(t)
		r.staleDocument(t, docClientURL)
		r.documents.set(docClientURL, documentHostDown)
		errs := captureErrorLog(t)

		hold, first := r.holdRefetchFailure(ctx, t, docClientURL, ttl)
		var eg errgroup.Group
		var second *httptest.ResponseRecorder
		secondDone := make(chan struct{})
		eg.Go(func() error { defer close(secondDone); second = r.refetchDocument(t); return nil })
		secondPID := waitForLockWaiter(ctx, t, r.pool, []int32{hold.pid}, secondDone)
		if secondPID == 0 {
			t.Fatal("the second failed re-fetch finished without queuing behind the first one's record")
		}
		assertQueuedOnClientBehind(ctx, t, r.pool, secondPID, hold.pid)
		if err := hold.tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := eg.Wait(); err != nil {
			t.Fatal(err)
		}
		if log := errs.String(); log != "" {
			t.Errorf("a handler failed:\n%s", log)
		}
		c := assertKeptAfterFailedRefetch(t, r, second)
		if !c.MetadataRefetchFailedAt.Time.Equal(first.MetadataRefetchFailedAt.Time) || !c.MetadataStaleAt.Time.Equal(first.MetadataStaleAt.Time) {
			t.Fatalf("after two failures: failure %v, grace until %v; want the first one's, %v and %v",
				c.MetadataRefetchFailedAt.Time, c.MetadataStaleAt.Time, first.MetadataRefetchFailedAt.Time, first.MetadataStaleAt.Time)
		}
	})

	// The other order of the pair below: an authorization request -- or a
	// grant -- inserted under an unused client holds that client FOR KEY
	// SHARE (its foreign-key check) when the sweep reaches it. The sweep
	// queues on the client row behind the insert alone, then, the insert
	// committed, looks again under the client's lock and spares it: the
	// row just committed keeps its client, never cascaded away.
	for _, tc := range []struct {
		name     string
		kind     sqlcgen.McpOauthClientKind
		clientID string
		grant    bool
	}{
		{"AuthorizationRequestHoldsDynamicClient_SweepQueuesAndSparesIt", sqlcgen.McpOauthClientKindDynamic, "narvi_mcp_d_old", false},
		{"AuthorizationRequestHoldsMetadataDocumentClient_SweepQueuesAndSparesIt", sqlcgen.McpOauthClientKindMetadataDocument, "https://unused.example/client.json", false},
		{"GrantHoldsDynamicClient_SweepQueuesAndSparesIt", sqlcgen.McpOauthClientKindDynamic, "narvi_mcp_d_old", true},
		{"GrantHoldsMetadataDocumentClient_SweepQueuesAndSparesIt", sqlcgen.McpOauthClientKindMetadataDocument, "https://unused.example/client.json", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := newASRig(t)
			old := r.oldUnusedClient(t, tc.kind, tc.clientID)
			user, _ := r.newUser(t, sqlcgen.UserRoleMember)
			errs := captureErrorLog(t)
			expires := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}

			hold := holdInTx(ctx, t, r.pool, func(tx pgx.Tx) {
				var err error
				if tc.grant {
					_, err = r.grants.WithTx(tx).UpsertGrant(ctx, sqlcgen.UpsertMCPOAuthGrantParams{
						UserID: user.ID, ClientID: old.ID, Scopes: []string{"mcp:read"}, Resource: r.server.Identifiers().Resource, ExpiresAt: expires,
					})
				} else {
					_, err = r.grants.WithTx(tx).CreateAuthorizationRequest(ctx, sqlcgen.CreateMCPOAuthAuthorizationRequestParams{
						ClientID: old.ID, RedirectUri: loopbackRedirect, CodeChallenge: "c", CodeChallengeMethod: "S256",
						Resource: r.server.Identifiers().Resource, ExpiresAt: expires,
					})
				}
				if err != nil {
					t.Fatalf("held insert: %v", err)
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
				t.Fatal("the sweep finished without queuing behind the insert holding its candidate")
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
				t.Fatalf("the sweep deleted %d clients, want 0: the client gained a committed row while the sweep waited", swept)
			}
			if c, ok := r.clientRow(t, tc.clientID); !ok || c.ID != old.ID {
				t.Fatalf("after the insert and the sweep: client %+v (found %v), want the same client kept", c, ok)
			}
			var under int
			if err := r.pool.QueryRow(ctx, `
				SELECT (SELECT count(*) FROM mcp_oauth_grants WHERE client_id = $1) + (SELECT count(*) FROM mcp_oauth_authorization_requests WHERE client_id = $1)`,
				old.ID).Scan(&under); err != nil || under != 1 {
				t.Fatalf("rows under the client after the sweep = %d (err %v), want the committed one", under, err)
			}
		})
	}

	// The pool path production takes (expiredcleanup.go builds the store
	// on the pool, not WithTx): the sweep's lock statement and its
	// re-checking DELETE are one transaction, so its candidates stay
	// locked FOR UPDATE from the first statement until it commits. An
	// authorization request or grant inserted BETWEEN the two statements
	// therefore waits on the client row behind the sweep, then fails its
	// foreign-key check -- never commits under a client the DELETE goes on
	// to remove, which would cascade the committed row away (round-1 C14).
	// A test-held SHARE lock on mcp_oauth_clients stops the sweep exactly
	// there: it lets the lock statement's ROW SHARE through and holds the
	// DELETE's ROW EXCLUSIVE back.
	for _, tc := range []struct {
		name  string
		grant bool
	}{
		{"SweepBetweenItsStatements_AuthorizationRequestWaitsThenFails", false},
		{"SweepBetweenItsStatements_GrantWaitsThenFails", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := newASRig(t)
			old := r.oldUnusedClient(t, sqlcgen.McpOauthClientKindDynamic, "narvi_mcp_d_old")
			user, _ := r.newUser(t, sqlcgen.UserRoleMember)
			errs := captureErrorLog(t)
			expires := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
			insertUnder := func(tx pgx.Tx) error {
				if tc.grant {
					_, err := r.grants.WithTx(tx).UpsertGrant(ctx, sqlcgen.UpsertMCPOAuthGrantParams{
						UserID: user.ID, ClientID: old.ID, Scopes: []string{"mcp:read"}, Resource: r.server.Identifiers().Resource, ExpiresAt: expires,
					})
					return err
				}
				_, err := r.grants.WithTx(tx).CreateAuthorizationRequest(ctx, sqlcgen.CreateMCPOAuthAuthorizationRequestParams{
					ClientID: old.ID, RedirectUri: loopbackRedirect, CodeChallenge: "c", CodeChallengeMethod: "S256",
					Resource: r.server.Identifiers().Resource, ExpiresAt: expires,
				})
				return err
			}
			rowsUnder := func() int {
				var n int
				if err := r.pool.QueryRow(ctx, `
					SELECT (SELECT count(*) FROM mcp_oauth_grants WHERE client_id = $1) + (SELECT count(*) FROM mcp_oauth_authorization_requests WHERE client_id = $1)`,
					old.ID).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}

			gate := holdInTx(ctx, t, r.pool, func(tx pgx.Tx) {
				if _, err := tx.Exec(ctx, `LOCK TABLE mcp_oauth_clients IN SHARE MODE`); err != nil {
					t.Fatalf("gate: lock the clients table: %v", err)
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
			sweepPID := waitForLockWaiter(ctx, t, r.pool, []int32{gate.pid}, sweepDone)
			if sweepPID == 0 {
				t.Fatal("the sweep finished without its DELETE reaching the gate")
			}
			if locktype, table := waitingFor(ctx, t, r.pool, sweepPID); locktype != "relation" || table != "mcp_oauth_clients" {
				t.Fatalf("the sweep waits for a %s lock on %q, want its DELETE waiting for the clients table", locktype, table)
			}

			inserter := holdInTx(ctx, t, r.pool, func(pgx.Tx) {})
			var insertErr error
			insertDone := make(chan struct{})
			eg.Go(func() error { defer close(insertDone); insertErr = insertUnder(inserter.tx); return nil })
			insertPID := waitForLockWaiter(ctx, t, r.pool, []int32{gate.pid, sweepPID}, insertDone)
			if insertPID == 0 {
				// Nothing held the candidate between the two statements.
				// What that costs: the DELETE goes on, queues behind the
				// insert's key-share lock, and once the insert commits
				// deletes the client and the committed row with it.
				if insertErr != nil {
					t.Fatalf("insert between the sweep's statements: %v", insertErr)
				}
				if err := gate.tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if pid := waitForLockWaiter(ctx, t, r.pool, []int32{inserter.pid}, sweepDone); pid == 0 {
					t.Fatalf("an insert under the sweep's candidate went through between its two statements: nothing held the client (the sweep then deleted %d)", swept)
				}
				if err := inserter.tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				_ = eg.Wait()
				_, found := r.clientRow(t, old.ClientID)
				t.Fatalf("an insert under the sweep's candidate went through between its two statements: nothing held the client; committed with the DELETE queued behind it, then: deleted %d, client found %v, committed rows left under it %d",
					swept, found, rowsUnder())
			}
			assertQueuedOnClientBehind(ctx, t, r.pool, insertPID, sweepPID)
			if err := gate.tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := eg.Wait(); err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if log := errs.String(); log != "" {
				t.Errorf("a handler failed:\n%s", log)
			}
			var pgErr *pgconn.PgError
			if !errors.As(insertErr, &pgErr) || pgErr.Code != "23503" {
				t.Fatalf("insert behind the sweep = %v, want a foreign-key violation (23503): the client it names is gone", insertErr)
			}
			if _, found := r.clientRow(t, old.ClientID); swept != 1 || found {
				t.Fatalf("the sweep deleted %d clients (candidate still found: %v), want its one candidate", swept, found)
			}
			if n := rowsUnder(); n != 0 {
				t.Fatalf("rows under the swept client = %d, want none: the insert behind the sweep must fail, not commit", n)
			}
		})
	}

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

	// The other orders: a dynamic registration's own statement (register.go:
	// one INSERT of a new client row) held open, a client deletion and a
	// failed re-fetch -- which lock only rows that exist -- never queue
	// behind it; and a failed re-fetch's record held open, a registration
	// never queues behind it.
	holdRegistration := func(ctx context.Context, t *testing.T, r *asRig) *heldTx {
		t.Helper()
		return holdInTx(ctx, t, r.pool, func(tx pgx.Tx) {
			if _, err := r.clients.WithTx(tx).Create(ctx, sqlcgen.CreateMCPOAuthClientParams{
				ClientID: "narvi_mcp_d_held", Kind: sqlcgen.McpOauthClientKindDynamic, ClientName: "Desktop Assistant", RedirectUris: []string{loopbackRedirect},
			}); err != nil {
				t.Fatalf("held registration: %v", err)
			}
		})
	}

	t.Run("RegistrationHoldsItsRow_ClientDeletionNeverQueues", func(t *testing.T) {
		ctx := context.Background()
		r := newASRig(t)
		f := newRaceFixture(t, r, refreshExchange)
		errs := captureErrorLog(t)

		hold := holdRegistration(ctx, t, r)
		var eg errgroup.Group
		var deleted *httptest.ResponseRecorder
		deletionDone := make(chan struct{})
		eg.Go(func() error { defer close(deletionDone); deleted = f.revoke(r, clientDeletion); return nil })
		if pid := waitForLockWaiter(ctx, t, r.pool, []int32{hold.pid}, deletionDone); pid != 0 {
			t.Fatalf("the client deletion queued behind %v, a registration in flight", blockingPIDs(ctx, t, r.pool, pid))
		}
		if err := eg.Wait(); err != nil {
			t.Fatal(err)
		}
		if err := hold.tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if log := errs.String(); log != "" {
			t.Errorf("a handler failed:\n%s", log)
		}
		if deleted.Code != http.StatusNoContent {
			t.Fatalf("client deletion: status %d, want 204", deleted.Code)
		}
		assertNothingSurvives(ctx, t, r, f, clientDeletion)
	})

	t.Run("RegistrationHoldsItsRow_FailedMetadataRefetchNeverQueues", func(t *testing.T) {
		ctx := context.Background()
		r := newCIMDRaceRig(t)
		r.staleDocument(t, docClientURL)
		r.documents.set(docClientURL, documentHostDown)
		errs := captureErrorLog(t)

		hold := holdRegistration(ctx, t, r)
		var eg errgroup.Group
		var failedRefetch *httptest.ResponseRecorder
		failedDone := make(chan struct{})
		eg.Go(func() error { defer close(failedDone); failedRefetch = r.refetchDocument(t); return nil })
		if pid := waitForLockWaiter(ctx, t, r.pool, []int32{hold.pid}, failedDone); pid != 0 {
			t.Fatalf("the failed re-fetch queued behind %v, a registration in flight", blockingPIDs(ctx, t, r.pool, pid))
		}
		if err := eg.Wait(); err != nil {
			t.Fatal(err)
		}
		if err := hold.tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if log := errs.String(); log != "" {
			t.Errorf("a handler failed:\n%s", log)
		}
		assertKeptAfterFailedRefetch(t, r, failedRefetch)
	})

	t.Run("FailedMetadataRefetchHoldsClient_RegistrationNeverQueues", func(t *testing.T) {
		ctx := context.Background()
		r := newCIMDRaceRig(t)
		r.staleDocument(t, docClientURL)
		errs := captureErrorLog(t)

		hold, _ := r.holdRefetchFailure(ctx, t, docClientURL, ttl)
		var eg errgroup.Group
		var registered int
		registerDone := make(chan struct{})
		eg.Go(func() error {
			defer close(registerDone)
			registered, _, _, _ = r.postRegister(`{"client_name":"Desktop Assistant","redirect_uris":["`+loopbackRedirect+`"]}`, "application/json")
			return nil
		})
		if pid := waitForLockWaiter(ctx, t, r.pool, []int32{hold.pid}, registerDone); pid != 0 {
			t.Fatalf("the registration queued behind %v, a failed re-fetch's record", blockingPIDs(ctx, t, r.pool, pid))
		}
		if err := eg.Wait(); err != nil {
			t.Fatal(err)
		}
		if registered != http.StatusCreated {
			t.Fatalf("registration: status %d, want 201", registered)
		}
		if err := hold.tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if log := errs.String(); log != "" {
			t.Errorf("a handler failed:\n%s", log)
		}
		assertFailureRecorded(t, r, docClientURL, "Editor Plugin")
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
