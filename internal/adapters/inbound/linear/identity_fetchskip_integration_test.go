//go:build integration

// Integration tests proving the perf fix in identity.go (resolveActor's own
// identitylink.LookupLinkedUserID pre-check): an ALREADY-LINKED Linear
// identity must resolve without ever calling Linear's own user(id) {
// email } GraphQL query, while a not-yet-linked identity must still call
// it exactly as before -- mirrors identity_integration_test.go's own
// conventions (testcontainers Postgres, a real linear.NewWebhookHandler,
// synthetic real-shaped payloads), plus a GraphQL stub that COUNTS its own
// requests instead of just answering them.
package linear_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/inbound/linear"
	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/identitylink"
	"github.com/narvidev/narvi/internal/platform"
)

// narviGetUserEmailQueryMarker is the operation name linearapi's own
// getUserEmailQuery (user.go) carries ("query NarviGetUserEmail(...")  --
// this file's own counting stub greps the raw request body for it to tell
// GetUserEmail's own GraphQL call apart from every OTHER call this same
// deps.LinearClient makes over the identical single-endpoint GraphQL API
// (postAcknowledgment's/postIdentityNotice's own CreateThoughtActivity
// mutation, in particular -- handleCreated posts one on EVERY event,
// already-linked or not, so a stub that counted every request regardless
// of body would conflate that unrelated call with the one this fix
// actually targets).
const narviGetUserEmailQueryMarker = "NarviGetUserEmail"

// newLinearGraphQLStubCounting mirrors newLinearGraphQLStub
// (identity_integration_test.go, same package), except it also counts
// every GetUserEmail request it observes specifically (see
// narviGetUserEmailQueryMarker above for why body content, not just
// request count, is what this file's own tests need) -- and answers every
// OTHER GraphQL call (activity-creation mutations) with a generic
// success shape, since handleCreated's own postAcknowledgment call still
// fires regardless of link state and must not error out the request.
func newLinearGraphQLStubCounting(t *testing.T, email string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), narviGetUserEmailQueryMarker) {
			atomic.AddInt32(&calls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"user": map[string]any{"email": email}},
			})
			return
		}
		// Any other GraphQL call this deps.LinearClient makes (activity
		// creation, etc.) -- a generic empty-but-valid success response is
		// enough; these tests don't assert on IT, only on GetUserEmail.
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

// TestWebhookHandler_Created_AlreadyLinkedIdentity_SkipsUserEmailFetch
// proves resolveActor (identity.go), reached via handleCreated
// (webhook.go): when agentSession.creatorId is ALREADY linked to a Narvi
// user, no Linear GraphQL user(id) { email } call is made at all -- the
// identity resolves via identitylink.LookupLinkedUserID's own pre-check,
// with the same eventual created_by Resolve's own internal fast path would
// have produced, but without the discarded fetch (and the discarded
// installation-token decrypt) this perf fix removes.
func TestWebhookHandler_Created_AlreadyLinkedIdentity_SkipsUserEmailFetch(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	deps := newHandlerDeps(t, pool)

	organizationID := "org-already-linked-1"
	installLinearFixture(ctx, t, pool, organizationID, deps.TokenEncryptionKey)

	graphqlStub, calls := newLinearGraphQLStubCounting(t, "already-linked@example.com")
	deps.LinearClient = linearapi.New(graphqlStub.Client(), graphqlStub.URL)

	users := narvipg.NewUserStore(pool)
	linkedUser, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "already-linked@example.com", DisplayName: "Already Linked", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	const linearCreatorID = "linear-already-linked-1"
	deps.IdentityLink = identitylink.Deps{
		Pool:           pool,
		Users:          users,
		Identities:     narvipg.NewIdentityStore(pool),
		LinkPrompts:    narvipg.NewIdentityLinkPromptStore(pool),
		AuditLog:       deps.AuditLog,
		PublicBaseURL:  "https://narvi.example.com",
		PromptTTL:      time.Hour,
		EvidenceMaxAge: platform.DefaultTimeouts().IdentityEmailEvidenceMaxAge,
	}
	if _, err := narvipg.NewIdentityStore(pool).Create(ctx, sqlcgen.CreateIdentityParams{
		UserID: linkedUser.ID, Provider: sqlcgen.IdentityProviderLinear, ExternalID: linearCreatorID,
		LinkedVia: sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		t.Fatalf("seed linked identity: %v", err)
	}

	handler := linear.NewWebhookHandler(deps)
	agentSessionID := "agent-session-already-linked-1"
	body := agentSessionCreatedPayloadWithCreator(agentSessionID, organizationID, linearCreatorID)

	rec := postWebhook(t, handler, body, "delivery-already-linked-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if got := atomic.LoadInt32(calls); got != 0 {
		t.Errorf("Linear GraphQL call count = %d, want 0 (identity already linked -- the fetch must be skipped entirely)", got)
	}

	var createdBy string
	if err := pool.QueryRow(ctx,
		`SELECT created_by::text FROM sessions WHERE spawn_source = 'linear' ORDER BY created_at DESC LIMIT 1`,
	).Scan(&createdBy); err != nil {
		t.Fatalf("query session created_by: %v", err)
	}
	if createdBy != linkedUser.ID.String() {
		t.Errorf("session created_by = %q, want %q (the already-linked user)", createdBy, linkedUser.ID.String())
	}
}

// TestWebhookHandler_Created_UnlinkedIdentity_CallsUserEmailFetch is this
// file's own counterpart proving the OTHER half still holds: a genuinely
// not-yet-linked identity still calls the Linear GraphQL user(id) { email }
// query exactly as before this perf fix (the pre-check must never suppress
// a fetch that's actually needed).
func TestWebhookHandler_Created_UnlinkedIdentity_CallsUserEmailFetch(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	deps := newHandlerDeps(t, pool)

	organizationID := "org-unlinked-1"
	installLinearFixture(ctx, t, pool, organizationID, deps.TokenEncryptionKey)

	graphqlStub, calls := newLinearGraphQLStubCounting(t, "unlinked@example.com")
	deps.LinearClient = linearapi.New(graphqlStub.Client(), graphqlStub.URL)

	users := narvipg.NewUserStore(pool)
	matchedUser, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "unlinked@example.com", DisplayName: "Unlinked", Role: sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create fixture user: %v", err)
	}
	addEmailEvidenceForTest(t, pool, matchedUser.ID, "unlinked@example.com")

	deps.IdentityLink = identitylink.Deps{
		Pool:           pool,
		Users:          users,
		Identities:     narvipg.NewIdentityStore(pool),
		LinkPrompts:    narvipg.NewIdentityLinkPromptStore(pool),
		AuditLog:       deps.AuditLog,
		PublicBaseURL:  "https://narvi.example.com",
		PromptTTL:      time.Hour,
		EvidenceMaxAge: platform.DefaultTimeouts().IdentityEmailEvidenceMaxAge,
	}

	handler := linear.NewWebhookHandler(deps)
	agentSessionID := "agent-session-unlinked-1"
	const linearCreatorID = "linear-unlinked-1"
	body := agentSessionCreatedPayloadWithCreator(agentSessionID, organizationID, linearCreatorID)

	rec := postWebhook(t, handler, body, "delivery-unlinked-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("Linear GraphQL call count = %d, want 1 (not yet linked -- must still fetch)", got)
	}

	identity, err := narvipg.NewIdentityStore(pool).GetByProviderAndExternalID(ctx, sqlcgen.IdentityProviderLinear, linearCreatorID)
	if err != nil {
		t.Fatalf("GetByProviderAndExternalID: %v", err)
	}
	if identity.UserID != matchedUser.ID {
		t.Errorf("identity.UserID = %v, want %v (auto-linked via the fetched email)", identity.UserID, matchedUser.ID)
	}
}
