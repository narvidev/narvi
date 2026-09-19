//go:build integration

// U3/U4/U6 audit fixes' own required proofs, plus W2's own fail-closed
// proofs (which replaced U7's own positive/negative machine-origin-fires
// pair -- see the two Test...NonUserActor... functions below's own doc
// comments for why) -- mirrors
// automationdispatch_integration_test.go's own conventions
// (issueEventPayloadWithActor/createLinearAutomation/installLinearFixture/
// countAutomationInvocations, this package's own shared helpers) plus
// identity_fetchskip_integration_test.go's own GraphQL-call-counting stub,
// this package's own shared syncLogBuffer capture, and slog.SetDefault
// redirection (webhook_integration_test.go's own established convention
// for asserting on log lines a webhook call emits).
package linear_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/inbound/linear"
	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/identitylink"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
)

// issueEventPayloadWithActorType is issueEventPayloadWithActor's own
// actor-type-carrying variant -- U7's own required fixture for a
// machine-originated actor (Linear's own docs: "Could be a User, OAuth
// client, or Integration").
func issueEventPayloadWithActorType(teamKey, actorID, actorType string) []byte {
	body := fmt.Sprintf(`{
		"action": "create",
		"type": "Issue",
		"organizationId": "org-automation-dispatch",
		"actor": {"id": %q, "type": %q, "name": "Automation Actor"},
		"webhookTimestamp": %d,
		"data": {"team": {"key": %q}}
	}`, actorID, actorType, time.Now().UnixMilli(), teamKey)
	return []byte(body)
}

func countIdentityLinkPrompts(t *testing.T, pool *pgxpool.Pool, provider sqlcgen.IdentityProvider, externalID string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM identity_link_prompts WHERE provider = $1 AND external_id = $2", provider, externalID,
	).Scan(&count); err != nil {
		t.Fatalf("count identity_link_prompts: %v", err)
	}
	return count
}

func countLinearIdentities(t *testing.T, pool *pgxpool.Pool, externalID string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM identities WHERE provider = 'linear' AND external_id = $1", externalID,
	).Scan(&count); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	return count
}

// TestWebhookHandler_AutomationDispatchDeniesUnlinkedActorWithoutAutoLinkingOrMintingPrompt
// is U3/U4's own required, missing proof. BEFORE this fix, the automation-
// dispatch authorization gate resolved its actor through deps.resolveActor
// -- the SAME side-effecting auto-linking algorithm the pre-existing
// AgentSessionEvent path uses -- so a never-before-seen Linear actor whose
// EMAIL happens to already match a real Narvi user would be auto-linked
// (and thereby AUTHORIZED) on the very request that was supposed to be
// checking whether they were already a known, authorized identity: an
// authorization gate that manufactures its own key authorizes everyone
// eventually. This fixture is built EXACTLY to trigger that old bug were
// it still present -- a Narvi user already exists with the email the
// GraphQL stub would return for this actor id, and NO identities row
// links them yet. Proves, in one request: (1) the delivery is still
// denied (0 invocations -- an unlinked actor must not authorize), (2) the
// Linear GraphQL user-email query is NEVER called (the pure lookup this
// fix uses touches no provider API at all), (3) no identities row is
// created as a side effect of this authorization check, and (4) no
// identity_link_prompts row (a live magic-link nonce) is minted and then
// discarded either.
func TestWebhookHandler_AutomationDispatchDeniesUnlinkedActorWithoutAutoLinkingOrMintingPrompt(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	automations := narvipg.NewAutomationStore(pool)
	invocations := narvipg.NewAutomationInvocationStore(pool)
	target := domainautomation.Target{Name: "repo", URL: "https://github.com/narvidev/narvi"}
	auto := createLinearAutomation(ctx, t, automations, "on ENG issue create (no auto-link)", domainautomation.LinearTriggerConfig{EventType: "Issue", Action: "create", TeamKey: "ENG", OrganizationID: "org-automation-dispatch"}, target)

	deps := newHandlerDeps(t, pool)
	deps.Automations = automations
	deps.AutomationInvocations = invocations
	installLinearFixture(ctx, t, pool, "org-automation-dispatch", deps.TokenEncryptionKey)

	const linearActorID = "linear-actor-would-auto-link-1"
	const matchingEmail = "would-auto-link@narvi-test.example.com"

	// A REAL Narvi user already exists with the email the (never-called)
	// GraphQL stub below would return -- if the OLD resolveActor-based
	// gate still ran, this is exactly the shape that auto-links AND
	// authorizes on this one request.
	users := narvipg.NewUserStore(pool)
	if _, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: matchingEmail, DisplayName: "Would Auto Link", Role: sqlcgen.UserRoleMaintainer,
	}); err != nil {
		t.Fatalf("create fixture user: %v", err)
	}

	graphqlStub, calls := newLinearGraphQLStubCounting(t, matchingEmail)
	deps.LinearClient = linearapi.New(graphqlStub.Client(), graphqlStub.URL)
	deps.IdentityLink = identitylink.Deps{
		Pool:          pool,
		Users:         users,
		Identities:    narvipg.NewIdentityStore(pool),
		LinkPrompts:   narvipg.NewIdentityLinkPromptStore(pool),
		AuditLog:      deps.AuditLog,
		PublicBaseURL: "https://narvi.example.com",
		PromptTTL:     time.Hour,
	}

	handler := linear.NewWebhookHandler(deps)
	rec := postWebhookEventType(t, handler, issueEventPayloadWithActor("ENG", linearActorID), "delivery-linear-no-auto-link-1", "Issue")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if got := countAutomationInvocations(t, pool, auto.ID); got != 0 {
		t.Errorf("automation_invocations = %d, want 0 (a not-yet-linked actor must never authorize, even if their email would eventually match a real user)", got)
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Errorf("Linear GraphQL GetUserEmail call count = %d, want 0 (U3 audit fix: the authorization gate must use a PURE lookup, never the auto-linking algorithm)", got)
	}
	if got := countLinearIdentities(t, pool, linearActorID); got != 0 {
		t.Errorf("identities rows for %q = %d, want 0 (this authorization gate must never auto-link as a side effect of checking)", linearActorID, got)
	}
	if got := countIdentityLinkPrompts(t, pool, sqlcgen.IdentityProviderLinear, linearActorID); got != 0 {
		t.Errorf("identity_link_prompts rows for %q = %d, want 0 (U4 audit fix: this gate must never mint a magic-link nonce it then discards)", linearActorID, got)
	}
}

// TestWebhookHandler_AutomationDispatchSkipsInstallationLookupForUnclassifiedEventType
// is U6/U10's own required proof: an event type domainautomation.
// LinearDispatchAllowlist does not cover must never reach the
// installation lookup (or anything past it) at all -- classification
// needs only the event type, already available before ANY of that work.
// Reproduced against a workspace that is delib deliberately NOT installed:
// if classification ran AFTER (or was skipped), the installation lookup
// would fail and log a WARN "organization_not_installed" line; this test
// asserts that line is ABSENT.
func TestWebhookHandler_AutomationDispatchSkipsInstallationLookupForUnclassifiedEventType(t *testing.T) {
	pool := newTestPool(t)
	deps := newHandlerDeps(t, pool)
	deps.Automations = narvipg.NewAutomationStore(pool)
	deps.AutomationInvocations = narvipg.NewAutomationInvocationStore(pool)
	// Deliberately NO installLinearFixture call: "org-never-installed"
	// has no installation row at all. If the installation lookup were
	// ever reached, it would fail with "organization_not_installed".
	handler := linear.NewWebhookHandler(deps)

	logBuf := &syncLogBuffer{}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	body := []byte(`{
		"action": "update",
		"type": "Project",
		"organizationId": "org-never-installed",
		"webhookTimestamp": ` + fmt.Sprint(time.Now().UnixMilli()) + `,
		"data": {}
	}`)
	rec := postWebhookEventType(t, handler, body, "delivery-linear-unclassified-1", "Project")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	logs := logBuf.String()
	if strings.Contains(logs, "organization_not_installed") {
		t.Errorf("logs contain \"organization_not_installed\" -- the installation lookup was reached for an event type (%q) domainautomation.LinearDispatchAllowlist does not even cover; classification must run FIRST and return before any Postgres/Linear API call. Logs:\n%s", "Project", logs)
	}
}

// TestWebhookHandler_AutomationDispatchDeniesNonUserActorEvenWithAuthorizedCreator
// is W2's own required proof (confirmed HIGH, SECURITY finding against
// U7's own fix, which this test used to prove the OPPOSITE of -- see git
// blame): an "Issue" event whose actor is reported as something other than
// a real Linear account (actor.type != "user") must be denied outright,
// EVEN WHEN the matching automation's own creator is genuinely linked and
// authorized. Before W2, this exact fixture (a real, authorized creator)
// fired the automation via the creator-authorization substitute path --
// the bypass this fix closes: actor.type is a per-payload field the SAME
// "Issue" category carries either value for depending only on how the
// issue was filed, not something the sender is structurally prevented
// from influencing, so it must never select a weaker authorization path.
func TestWebhookHandler_AutomationDispatchDeniesNonUserActorEvenWithAuthorizedCreator(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	automations := narvipg.NewAutomationStore(pool)
	invocations := narvipg.NewAutomationInvocationStore(pool)

	users := narvipg.NewUserStore(pool)
	creator, err := users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: "non-user-origin-creator@narvi-test.example.com", DisplayName: "Automation Creator", Role: sqlcgen.UserRoleMaintainer,
	})
	if err != nil {
		t.Fatalf("create fixture creator: %v", err)
	}

	target := domainautomation.Target{Name: "repo", URL: "https://github.com/narvidev/narvi"}
	reposJSON, err := json.Marshal([]domainautomation.Target{target})
	if err != nil {
		t.Fatalf("marshal repos: %v", err)
	}
	triggerConfigJSON, err := json.Marshal(map[string]string{"eventType": "Issue", "action": "create", "teamKey": "ENG", "organizationId": "org-automation-dispatch"})
	if err != nil {
		t.Fatalf("marshal trigger config: %v", err)
	}
	auto, err := automations.Create(ctx, sqlcgen.CreateAutomationParams{
		Name: "on ENG issue create (non-user actor, authorized creator)", Repos: reposJSON, CreatedBy: creator.ID,
		TriggerType: sqlcgen.AutomationTriggerTypeLinear, TriggerConfig: triggerConfigJSON, EnvVars: []byte("[]"),
	})
	if err != nil {
		t.Fatalf("create linear automation: %v", err)
	}

	deps := newHandlerDeps(t, pool)
	deps.Automations = automations
	deps.AutomationInvocations = invocations
	installLinearFixture(ctx, t, pool, "org-automation-dispatch", deps.TokenEncryptionKey)
	deps.IdentityLink = identitylink.Deps{
		Pool: pool, Users: users, Identities: narvipg.NewIdentityStore(pool),
		LinkPrompts: narvipg.NewIdentityLinkPromptStore(pool), AuditLog: deps.AuditLog,
		PublicBaseURL: "https://narvi.example.com", PromptTTL: time.Hour,
	}
	handler := linear.NewWebhookHandler(deps)

	// actor.type "application" -- NOT "user". This deployment has never
	// observed Linear's real wire value for a non-"user" actor (Linear's
	// own docs name "OAuth client"/"Integration" as the two kinds but do
	// not publish their exact strings) -- "application" is an invented
	// test value standing in for "anything that is not literally 'user'",
	// which is exactly the point: the fix must not depend on knowing the
	// real value.
	rec := postWebhookEventType(t, handler, issueEventPayloadWithActorType("ENG", "linear-integration-actor-1", "application"), "delivery-linear-non-user-origin-1", "Issue")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if got := countAutomationInvocations(t, pool, auto.ID); got != 0 {
		t.Fatalf("automation_invocations = %d, want 0 (W2 audit fix: a non-\"user\" actor must be denied outright, never routed through the automation-creator's own authorization even when that creator IS authorized)", got)
	}

	// The denial must NOT surface as a creator-unauthorized mark: this
	// creator was never evaluated at all (the adapter denies before any
	// automation is even listed), and marking a perfectly healthy creator
	// "unauthorized" would itself be dishonest state.
	row, err := automations.Get(ctx, auto.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.CreatorUnauthorizedSince.Valid {
		t.Error("CreatorUnauthorizedSince.Valid = true, want false (this automation's own creator was never evaluated -- the non-\"user\" actor was denied one layer up, before any automation was listed)")
	}
}

// TestWebhookHandler_AutomationDispatchDeniesNonUserActorWithUnauthorizedCreator
// is the companion proof at the OTHER extreme of the same fixture space:
// a non-"user" actor is denied identically even when the matching
// automation's own creator has no linked, authorized account at all
// (CreatedBy left invalid -- mirrors an automation whose creator was since
// deleted, ON DELETE SET NULL). Read together with the "authorized
// creator" test above, this pins that the creator's own authorization
// state is now IRRELEVANT to a non-"user" actor's own denial -- proving
// the fix is unconditional, not merely "still denies in the one case it
// already denied before".
func TestWebhookHandler_AutomationDispatchDeniesNonUserActorWithUnauthorizedCreator(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	automations := narvipg.NewAutomationStore(pool)
	invocations := narvipg.NewAutomationInvocationStore(pool)
	target := domainautomation.Target{Name: "repo", URL: "https://github.com/narvidev/narvi"}
	// createLinearAutomation (automationdispatch_integration_test.go) seeds
	// CreatedBy as an invalid pgtype.UUID -- no authorizing principal.
	auto := createLinearAutomation(ctx, t, automations, "on ENG issue create (non-user actor, unauthorized creator)", domainautomation.LinearTriggerConfig{EventType: "Issue", Action: "create", TeamKey: "ENG", OrganizationID: "org-automation-dispatch"}, target)

	deps := newHandlerDeps(t, pool)
	deps.Automations = automations
	deps.AutomationInvocations = invocations
	installLinearFixture(ctx, t, pool, "org-automation-dispatch", deps.TokenEncryptionKey)
	deps.IdentityLink = identitylink.Deps{
		Pool: pool, Users: narvipg.NewUserStore(pool), Identities: narvipg.NewIdentityStore(pool),
		LinkPrompts: narvipg.NewIdentityLinkPromptStore(pool), AuditLog: deps.AuditLog,
		PublicBaseURL: "https://narvi.example.com", PromptTTL: time.Hour,
	}
	handler := linear.NewWebhookHandler(deps)

	rec := postWebhookEventType(t, handler, issueEventPayloadWithActorType("ENG", "linear-integration-actor-2", "application"), "delivery-linear-non-user-origin-denied-1", "Issue")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if got := countAutomationInvocations(t, pool, auto.ID); got != 0 {
		t.Fatalf("automation_invocations = %d, want 0 (a non-\"user\" actor must not fire an automation regardless of its own creator's authorization state)", got)
	}

	row, err := automations.Get(ctx, auto.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.CreatorUnauthorizedSince.Valid {
		t.Error("CreatorUnauthorizedSince.Valid = true, want false (W2 audit fix: this automation's own creator authorization is never evaluated for a non-\"user\" actor any more, so nothing should mark it)")
	}
}

// TestWebhookHandler_AutomationDispatchDeniesNonUserActorEvenWhenActorIDIsLinkedAndAuthorized
// is Y3 audit fix's own required, missing proof. Both
// Test...NonUserActor... tests above use an actor id with NO identities
// row at all, so they pass identically with the machine-origin denial
// (automationdispatch.go's own `known && origin ==
// domainautomation.LinearEventOriginMachine` branch) replaced by
// `_ = actorType` -- the human-actor gate a few lines below (LookupLinkedUserID
// + actorauthz.AuthorizeLinkedActor) denies the unlinked id anyway, for an
// entirely different reason, so neither test can tell "denied because
// non-user" apart from "denied because unlinked". A verifier proved this
// directly against that exact mutant: both tests above, and the whole
// package's own integration suite, still passed.
//
// This test supplies the one fixture that actually distinguishes the two
// code paths: a non-"user" actor.type whose id IS linked to a genuinely
// authorized Narvi user (linkLinearIdentityForTest, identity_integration_
// test.go -- the SAME helper TestWebhookHandler_AutomationDispatchFiresOnRealWebhook
// uses for its own positive case, just with actor.type set to something
// other than "user"). With the machine-origin gate intact, this must still
// deny (0 invocations): D-07 (docs/DECISIONS.md) is unconditional on
// actor.type, never contingent on whether that id happens to resolve to an
// authorized user. Delete the gate instead, and this exact fixture falls
// through to the human-actor gate below, which WOULD find it linked and
// authorized, and WOULD dispatch -- the reproduction that motivated this
// test: HEAD gives 0 invocations, a `_ = actorType` mutant gives 1.
func TestWebhookHandler_AutomationDispatchDeniesNonUserActorEvenWhenActorIDIsLinkedAndAuthorized(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()

	automations := narvipg.NewAutomationStore(pool)
	invocations := narvipg.NewAutomationInvocationStore(pool)
	target := domainautomation.Target{Name: "repo", URL: "https://github.com/narvidev/narvi"}
	auto := createLinearAutomation(ctx, t, automations, "on ENG issue create (non-user actor, linked and authorized)", domainautomation.LinearTriggerConfig{EventType: "Issue", Action: "create", TeamKey: "ENG", OrganizationID: "org-automation-dispatch"}, target)

	deps := newHandlerDeps(t, pool)
	deps.Automations = automations
	deps.AutomationInvocations = invocations
	installLinearFixture(ctx, t, pool, "org-automation-dispatch", deps.TokenEncryptionKey)
	deps.IdentityLink = newIdentityLinkDepsForTest(pool, deps.AuditLog)

	const linearActorID = "linear-integration-actor-linked-1"
	// The distinguishing fixture: THIS actor id, unlike both tests above,
	// IS linked to a genuinely authorized user -- the human-actor gate
	// below would allow it through, if it were ever reached.
	linkLinearIdentityForTest(ctx, t, pool, linearActorID, sqlcgen.UserRoleMaintainer)

	handler := linear.NewWebhookHandler(deps)

	// actor.type "application" -- NOT "user". Mirrors the two tests above's
	// own reasoning for using an invented, non-"user" stand-in value.
	rec := postWebhookEventType(t, handler, issueEventPayloadWithActorType("ENG", linearActorID, "application"), "delivery-linear-non-user-linked-1", "Issue")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if got := countAutomationInvocations(t, pool, auto.ID); got != 0 {
		t.Fatalf("automation_invocations = %d, want 0 (a non-\"user\" actor must be denied outright regardless of whether its id happens to be linked and authorized -- D-07, docs/DECISIONS.md, is unconditional on actor.type)", got)
	}
}
