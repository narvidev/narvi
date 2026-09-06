//go:build integration

// Integration tests for internal/adapters/inbound/httpapi's 5 REST
// endpoints (§6.3), against a real Postgres instance -- gated behind the
// "integration" build tag, matching internal/adapters/inbound/wshub's own
// testcontainers-Postgres-plus-embedded-migrations convention exactly
// (each DB-touching package builds its own copy of this small helper
// rather than sharing one across package boundaries, per that package's
// own precedent). Run via `make test-integration`. newTestPool (below)
// no longer starts its OWN container per call -- see sharedpool_
// integration_test.go's own top doc comment for why this package's
// uniquely large test count (~170 functions) made that worth changing,
// and for the container/pool this now shares with every other test in
// this package.
//
// As of §13.1 ("auth v1"), every route in this file is mounted behind
// internal/adapters/inbound/auth.Middleware, exactly like cmd/
// control-plane/main.go's own real wiring -- every request below now goes
// through a REAL session (createAuthenticatedUser constructs one directly
// via the stores: users.Create + identities.Create + userSessions.Create +
// attaching the resulting cookie, mirroring exactly how §6.2's own
// createSession helper already bypasses REST for test setup) rather than
// mocking auth away.
package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/inbound/automationwebhook"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/chatgptoauth"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/chatgptlink"
	"github.com/narvidev/narvi/internal/app/findingposition"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/reviewcontext"
	appreviewtriage "github.com/narvidev/narvi/internal/app/reviewtriage"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/app/shadowledger"
	"github.com/narvidev/narvi/internal/platform"
)

// newTestPool returns this package's own single, shared Postgres pool --
// started ONCE for the whole test binary by TestMain (sharedpool_
// integration_test.go, package httpapi), not freshly per test/container
// as this function used to do itself. Kept as a thin wrapper under its
// own original name/signature so every existing call site in this file
// (and turn_integration_test.go's own standalone-rig test) keeps
// compiling unchanged. See sharedpool_integration_test.go's own top doc
// comment for the full container-reuse story: why per-test containers
// were never a deliberate correctness requirement here, why sharing one
// is safe against this package's own async sessionactor.Actor background
// work, and why each test still gets a byte-for-byte-empty (plus
// restored seed data), freshly-migrated-equivalent database via a
// t.Cleanup-registered reset rather than a real fresh container.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return httpapi.IntegrationTestPool(t)
}

// testRig bundles a fresh pool + every store + an httptest.Server
// mounting all 5 REST routes exactly as cmd/control-plane/main.go does --
// including auth.Middleware gating the whole /api/sessions
// group.
type testRig struct {
	pool         *pgxpool.Pool
	sessions     *narvipg.SessionStore
	turns        *narvipg.TurnStore
	sandboxes    *narvipg.SandboxStore
	events       *narvipg.EventStore
	artifacts    *narvipg.ArtifactStore
	wsTokens     *narvipg.WSTokenStore
	environments *narvipg.EnvironmentStore
	users        *narvipg.UserStore
	identities   *narvipg.IdentityStore
	userSessions *narvipg.UserSessionStore
	registry     *sessionactor.Registry
	server       *httptest.Server

	// plans/participants are §8.1's ("plan mode, web", §8.1) own
	// additions, backing this rig's own approve/reject plan routes below
	// (planapprove_integration_test.go).
	plans        *narvipg.PlanStore
	participants *narvipg.ParticipantStore

	// planDocuments (§31.3) backs the approved-plan durability snapshot
	// DecidePlanOnTx now writes in the same transaction as the plans row's
	// own guarded approve UPDATE (decideplan.go).
	planDocuments *narvipg.PlanDocumentStore

	// outbox/linearAgentSessions are §8.1's ("plan mode, cross-channel",
	// §8.1/§13.3) own additions -- DecidePlanOnTx's own cross-channel-notify
	// dependencies (decideplan.go), now threaded through the approve/reject
	// routes below exactly like production wiring (cmd/control-plane/
	// main.go).
	outbox              *narvipg.OutboxStore
	linearAgentSessions *narvipg.LinearAgentSessionStore

	// auditLog is §13.2's ("identities + full RBAC", §13.3) own
	// addition -- threaded through to CreateSession/CreateTurn/
	// ApprovePlan/RejectPlan below exactly like production wiring
	// (cmd/control-plane/main.go).
	auditLog *narvipg.AuditLogStore

	// linkPrompts backs the members API's own GET /api/members (below) --
	// ListMembers's own "pending-link state" half (§13.2).
	linkPrompts *narvipg.IdentityLinkPromptStore

	// promptTemplates backs POST /api/intent-templates(/preview) below
	// (audit finding M5, completeness) -- the first-ever caller of
	// postgres.PromptTemplateStore's own Upsert method.
	promptTemplates *narvipg.PromptTemplateStore

	// digestChannels backs GET /api/repos/{owner}/{repo}/digest-scope
	// below ("ui settings + analytics", §12.2 item 5, §21.3) -- see
	// httpapi/digestscope.go's own doc comment.
	digestChannels *narvipg.DigestChannelStore

	// tokenEncryptionKey is a fixed, valid 32-byte AES-256-GCM key used by
	// this rig's own scm-credentials tests (real EncryptToken/DecryptToken
	// round trip, matching the SAME real flow §13.1's own OAuth callback
	// uses -- not a shortcut).
	tokenEncryptionKey []byte

	// provider is §3.2's ("snapshots & restore") own addition -- a
	// *fakeSnapshotProvider (snapshotmint_integration_test.go), configured
	// per-test via its own exported fields, backing this rig's own
	// snapshot-mint route below.
	provider *fakeSnapshotProvider

	// prSessions is §8.2's ("review sessions", §8.2) own addition --
	// backing this rig's own manual re-review REST button route
	// (reviewretrigger_integration_test.go). diffFetcher/botToken default
	// nil/"" (RetriggerReview's own nil-safe "skip the fetch" contract,
	// mirrored from internal/adapters/inbound/github's own identical
	// Config.DiffFetcher precedent): the pre-fetched-diff/stack-context
	// ASSEMBLY itself is already covered exhaustively, with no DB
	// interaction needed, by internal/app/reviewcontext's own
	// 100%-covered unit tests -- this rig's own job is proving the REST
	// endpoint's OWN behavior (404/400/403/201, the AlwaysQueue policy,
	// and real-Postgres concurrency), not re-proving a pure function
	// composition. A test that specifically wants to prove THIS call
	// site's own owner/repo/token args reach reviewcontext.Fetch
	// correctly (audit fix, test-coverage finding) overrides diffFetcher/
	// botToken via newTestRig's own mutate func, below.
	prSessions *narvipg.GitHubPRSessionStore

	diffFetcher reviewcontext.Fetcher
	botToken    string

	// positionResolver (§22.1.1) is review/verdict's own
	// relocation-fallback dependency -- nil by default (this rig's own
	// pre-existing tests never care about it, and a nil resolver is a
	// fully nil-safe, legitimate value: findingposition.ResolveAll's own
	// doc comment). A test that specifically wants to prove the
	// relocation fallback's own wiring overrides this via newTestRig's
	// own mutate func, mirroring diffFetcher's identical precedent
	// immediately above.
	positionResolver *findingposition.Resolver

	// repoSettings/botHandle ("server-side verdict", §8.2/§21.2)
	// back this rig's own verdict-posting-tool route (review/verdict,
	// reviewverdict_integration_test.go) and the admin repo-settings routes
	// (reposettings_integration_test.go). botHandle defaults to a fixed,
	// non-empty test value -- unlike diffFetcher/botToken above (which
	// default nil/"" because their OWN absence is meaningful/tested
	// elsewhere), review-verdict's rendered comment always needs a real
	// handle to build RerunGuidance from.
	repoSettings *narvipg.RepoSettingsStore
	botHandle    string

	// shadowLedger/readOnlyMinter/platformShadow (§30.4) back this rig's
	// own scm-credentials route's shadow-substitution branch. readOnlyMinter
	// defaults to a fakeReadOnlyMinter returning a fixed, obviously-fake
	// read-only token (scmcredentials_integration_test.go) -- every
	// pre-existing test in this file that relies on
	// createSessionWithGitHubIdentity/createSessionWithGitHubIdentityAndRepos
	// gets its session's own repo(s) upserted live (repo_settings.
	// live_egress_enabled = true) by that SAME helper, so those tests keep
	// exercising the LIVE creator-OAuth/bot-token path they always did --
	// this rig's own shadow-specific tests instead create a session whose
	// repo is deliberately left un-promoted (the default), or set
	// platformShadow true via newTestRig's own mutate func.
	// shadowLedger is typed as the interface (shadowledger.Store), not the
	// concrete *narvipg.ShadowSCMWriteStore, mirroring sourceControl's own
	// identical "swap for a fake" precedent immediately above -- a test
	// proving §30.4(4)'s own "record-or-fail" ledger-write-failure path
	// overrides this via newTestRig's own mutate func with a fake that
	// fails on demand, which a concrete Postgres-backed field could not
	// do without actually breaking the database connection.
	shadowLedger   shadowledger.Store
	readOnlyMinter httpapi.ReadOnlyMinter
	platformShadow bool

	// rolloutMode (§10 Phase 6, §32) backs this rig's own
	// CreateSession route below -- defaults to platform.RolloutModeOpen
	// (today's existing, unchanged behavior for every test in this file
	// that doesn't care about cohort rollout), overridable via
	// newTestRig's own mutate func exactly like every other rig field
	// with a meaningful non-zero default.
	rolloutMode platform.RolloutMode

	// reviewFindings/sentinelFixes ("sentinels + suggestions",
	// §17/§22.1) back this rig's own findings-upsert/rebut/apply-
	// suggestion routes (reviewfindings_integration_test.go) and the
	// verdict-posting route's own extended findings/sentinel-auto-fix
	// behavior (reviewverdict_integration_test.go).
	reviewFindings *narvipg.ReviewFindingStore
	sentinelFixes  *narvipg.SentinelFixStore

	// falsePositivePatterns ("review: learned false-positive
	// patterns", §22.2/§22.3/§22.4) backs this rig's own advisory-
	// injection/lifecycle behavior on the retrigger and verdict-posting
	// routes.
	falsePositivePatterns *narvipg.FalsePositivePatternStore

	// reviewVerdicts (§21.1) backs the verdict-posting route's
	// own review_verdicts insert (reviewverdict.go).
	reviewVerdicts *narvipg.ReviewVerdictStore

	// sourceControl backs the apply-suggestion route's own GetFileContent/
	// UpdateFileContent calls -- defaults to nil (ApplySuggestion's own
	// callers all fail closed on a nil port the same way every other
	// nil-safe optional dependency in this rig does); a test exercising
	// ApplySuggestion's happy path overrides this via newTestRig's own
	// mutate func with a fake implementing ports.SourceControl.
	sourceControl ports.SourceControl

	// automations ("automations: triggers & extras", §8.4) backs
	// this rig's own /api/automations routes (automations_integration_
	// test.go).
	automations *narvipg.AutomationStore

	// automationInvocations backs this rig's own /webhooks/automations/
	// {automationID} route (mounted below, mirroring cmd/control-plane/
	// main.go's own identical wiring) -- review-fix addition
	// (automationwebhooktoken_integration_test.go) so a rotate/revoke test
	// can drive the REAL inbound webhook handler end to end, not just
	// assert against AutomationStore.GetByWebhookTokenHash directly.
	automationInvocations *narvipg.AutomationInvocationStore

	// automationRuns backs this rig's own GET /api/automations/
	// {automationID}/invocations route (the automations UI, automationinvocations_
	// integration_test.go) -- the nested runs half of that read model.
	automationRuns *narvipg.AutomationRunStore

	// providerCredentials ("provider credential injection",
	// §25.1/§25.3) backs this rig's own 3 scoped provider-credentials CRUD
	// route groups (providercredentials_integration_test.go) and the
	// sandbox-facing delivery route (providercredentialsdelivery_
	// integration_test.go).
	providerCredentials *narvipg.ProviderCredentialStore

	// sandboxSecrets/openCodeConfigs ("sandbox secrets & opencode
	// config", §27.1/§27.2) back this rig's own sandbox-secrets CRUD route
	// groups + sandbox-facing delivery route
	// (sandboxsecrets_integration_test.go/sandboxsecretsdelivery_
	// integration_test.go) and opencode-config GET/PUT/DELETE route groups
	// + sandbox-facing delivery route (opencodeconfig_integration_test.go/
	// opencodeconfigdelivery_integration_test.go), mirroring
	// providerCredentials' own identical "one store, shared" pattern.
	sandboxSecrets  *narvipg.SandboxSecretStore
	openCodeConfigs *narvipg.OpenCodeConfigStore

	// chatGPTLinkAttempts/chatGPTDeviceFlow ("models: Codex via
	// ChatGPT-account OAuth", §29.3) back this rig's own /api/me/
	// chatgpt-link route group (chatgptlink_integration_test.go).
	// chatGPTDeviceFlow defaults to a client pointed at an unreachable
	// dummy address (mirroring diffFetcher/sourceControl's own "nil/dummy
	// by default, override via mutate" precedent above) -- safe for every
	// OTHER test in this rig, which never calls this route at all; a test
	// that DOES exercise the link flow overrides it via newTestRig's own
	// mutate func with a real fake auth.openai.com (httptest.Server).
	chatGPTLinkAttempts *narvipg.ChatGPTLinkAttemptStore
	chatGPTDeviceFlow   *chatgptoauth.Client

	// workflows ("workflow execution engine", §25.6) backs this
	// rig's own generic step-outcome-posting-tool route
	// (workflowstepoutcome_integration_test.go) -- the SAME store instance
	// createTurnLocked's own tests (turncore_integration_test.go,
	// workflowengine_characterization_integration_test.go) construct
	// independently via postgres.NewWorkflowStore(pool) directly (that
	// production code path constructs its own fresh instance from pool
	// too, see turn.go's own doc comment for why).
	workflows *narvipg.WorkflowStore

	// slackThreadSession ("workflow HITL gate + circuit breaker",
	// §25.9) backs this rig's own decide-endpoint route
	// (decideworkflowstep_integration_test.go) -- notification-destination
	// resolution for a Slack-origin session, the SAME store
	// internal/app/sessionactor's own outboxenqueue.go already uses
	// identically. No existing httpapi route needed this store before this
	// Step (only sessionactor did), so it is new to this rig.
	slackThreadSession *narvipg.SlackThreadSessionStore

	// blobStore/objCfg ("uploads, blob storage & the in-sandbox
	// download_file tool", §28) back this rig's own mint/confirm/content
	// routes, both auth variants (upload_integration_test.go). blobStore
	// defaults to a fresh *fakeBlobStore per rig (upload_integration_test.go's
	// own in-memory, httptest.Server-backed ports.BlobStore -- real
	// S3/MinIO behavior is covered separately, exhaustively, by
	// internal/adapters/outbound/objstore's own unit/integration tests;
	// this rig only needs to exercise the UPLOAD LIFECYCLE'S own logic).
	// objCfg defaults to small, deliberately test-friendly byte limits so
	// oversize/quota tests don't need to move real megabytes.
	blobStore ports.BlobStore
	objCfg    *platform.ObjectStorageConfig

	// broadcaster defaults to nil (ConfirmUpload/ConfirmUploadAPI's own
	// nil-safe contract -- ports.EventBroadcaster's doc comment) -- a test
	// that wants to assert a broadcast happened overrides this via
	// newTestRig's own mutate func with a *recordingBroadcaster
	// (upload_integration_test.go), mirroring diffFetcher/sourceControl's
	// own identical "nil by default, override via mutate" precedent above.
	broadcaster ports.EventBroadcaster

	// cloudIdentityBindings/oidcSigningKeys ("cloud identity:
	// OIDC issuer, bindings, minting", §27.3) back this rig's own 2
	// scoped cloud-identity-bindings CRUD route groups
	// (cloudidentitybindings_integration_test.go), the signing-key
	// rotation trigger, the public discovery/JWKS routes, and the
	// sandbox-facing minting route (cloudidentitytoken_integration_test.go)
	// -- mirrors providerCredentials/sandboxSecrets' own identical "one
	// store, shared" pattern. cloudIdentityIssuerURL defaults to a fixed,
	// non-empty test value (unlike diffFetcher/sourceControl's own
	// "absent by default" precedent) since the capability being OFF is
	// itself a distinct, separately-tested case
	// (TestMintCloudIdentityToken_IssuerUnset_MutationPin et al.) rather
	// than this rig's own default posture -- a test that specifically
	// wants the capability off overrides it via newTestRig's own mutate
	// func.
	cloudIdentityBindings  *narvipg.CloudIdentityBindingStore
	oidcSigningKeys        *narvipg.OIDCSigningKeyStore
	cloudIdentityIssuerURL string

	// clusterBindings ("cloud identity: sandbox-side consumption
	// + kubeconfig injection", §27.4) backs this rig's own cluster-binding
	// management route group (clusterbindings_integration_test.go) and the
	// sandbox-facing cloud-identity-config delivery route
	// (cloudidentityconfigdelivery_integration_test.go).
	clusterBindings *narvipg.ClusterBindingStore

	// webhookDeliveries (§12.5's own "integrations read model & routes"
	// amendment) backs this rig's own GET /api/integrations route
	// (integrations_integration_test.go) -- the inbound half of that read
	// model (webhook_deliveries.received_at, an exact provider match).
	webhookDeliveries *narvipg.WebhookDeliveryStore

	// cfg (§12.5 amendment) is the *platform.Config GetIntegrations
	// reads its own "is this surface configured" secrets from -- defaults
	// to a fully-configured value for all three surfaces (Slack/Linear/
	// GitHub), the SAME "everything wired, override the one field a test
	// specifically cares about" default posture rolloutMode/
	// cloudIdentityIssuerURL already establish above; a test proving the
	// partially-configured case overrides individual fields via
	// newTestRig's own mutate func.
	cfg *platform.Config
}

// newTestRig builds the default rig. mutate (variadic so every EXISTING
// newTestRig(t) call site keeps compiling unchanged) lets a caller
// override this rig's own fields -- e.g. diffFetcher/botToken -- BEFORE
// the router below is built, mirroring internal/adapters/inbound/github's
// own newTestRig(t, mutate...) precedent (handler_integration_test.go)
// exactly.
func newTestRig(t *testing.T, mutate ...func(*testRig)) testRig {
	t.Helper()
	ctx := context.Background()
	pool := newTestPool(t)

	// nil provider/commander: this rig's own tests only assert that
	// EnsureDispatched is correctly TRIGGERED by CreateSession, not what
	// the full spawn/dispatch decision tree then does with it --
	// internal/app/sessionactor's own dispatch_integration_test.go covers
	// that decision tree exhaustively.
	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	rig := testRig{
		pool:                  pool,
		sessions:              narvipg.NewSessionStore(pool),
		turns:                 narvipg.NewTurnStore(pool),
		sandboxes:             narvipg.NewSandboxStore(pool),
		events:                narvipg.NewEventStore(pool),
		artifacts:             narvipg.NewArtifactStore(pool),
		wsTokens:              narvipg.NewWSTokenStore(pool),
		environments:          narvipg.NewEnvironmentStore(pool),
		users:                 narvipg.NewUserStore(pool),
		identities:            narvipg.NewIdentityStore(pool),
		userSessions:          narvipg.NewUserSessionStore(pool),
		registry:              registry,
		tokenEncryptionKey:    []byte("01234567890123456789012345678901"), // exactly 32 bytes
		provider:              &fakeSnapshotProvider{},
		plans:                 narvipg.NewPlanStore(pool),
		participants:          narvipg.NewParticipantStore(pool),
		planDocuments:         narvipg.NewPlanDocumentStore(pool),
		outbox:                narvipg.NewOutboxStore(pool, false),
		linearAgentSessions:   narvipg.NewLinearAgentSessionStore(pool),
		auditLog:              narvipg.NewAuditLogStore(pool),
		linkPrompts:           narvipg.NewIdentityLinkPromptStore(pool),
		promptTemplates:       narvipg.NewPromptTemplateStore(pool),
		digestChannels:        narvipg.NewDigestChannelStore(pool),
		prSessions:            narvipg.NewGitHubPRSessionStore(pool),
		repoSettings:          narvipg.NewRepoSettingsStore(pool),
		botHandle:             "narvi-test-bot",
		shadowLedger:          narvipg.NewShadowSCMWriteStore(pool),
		readOnlyMinter:        newFakeReadOnlyMinter(),
		rolloutMode:           platform.RolloutModeOpen,
		reviewFindings:        narvipg.NewReviewFindingStore(pool),
		sentinelFixes:         narvipg.NewSentinelFixStore(pool),
		falsePositivePatterns: narvipg.NewFalsePositivePatternStore(pool),
		reviewVerdicts:        narvipg.NewReviewVerdictStore(pool),
		automations:           narvipg.NewAutomationStore(pool),
		automationInvocations: narvipg.NewAutomationInvocationStore(pool),
		automationRuns:        narvipg.NewAutomationRunStore(pool),
		providerCredentials:   narvipg.NewProviderCredentialStore(pool),
		sandboxSecrets:        narvipg.NewSandboxSecretStore(pool),
		openCodeConfigs:       narvipg.NewOpenCodeConfigStore(pool),
		chatGPTLinkAttempts:   narvipg.NewChatGPTLinkAttemptStore(pool),
		chatGPTDeviceFlow:     chatgptoauth.New(http.DefaultClient, "http://127.0.0.1:1", time.Second),
		workflows:             narvipg.NewWorkflowStore(pool),
		slackThreadSession:    narvipg.NewSlackThreadSessionStore(pool),
		blobStore:             newFakeBlobStore(t),
		// MaxSessionUploadBytes is deliberately LESS than 2x MaxUploadBytes:
		// two individually-within-per-file-cap uploads (each <=
		// MaxUploadBytes) can still combine to exceed the session cap --
		// exactly the scenario upload_integration_test.go's own
		// TestConfirmUpload_SessionCapRace_SecondConfirmFailsQuotaExceeded
		// needs to construct (a session cap >= 2x the per-file cap would
		// make that scenario mathematically unreachable).
		objCfg: &platform.ObjectStorageConfig{
			MaxUploadBytes:        1024,
			MaxSessionUploadBytes: 1500,
		},
		cloudIdentityBindings:  narvipg.NewCloudIdentityBindingStore(pool),
		oidcSigningKeys:        narvipg.NewOIDCSigningKeyStore(pool),
		cloudIdentityIssuerURL: "https://issuer.narvi.example.test",
		clusterBindings:        narvipg.NewClusterBindingStore(pool),
		webhookDeliveries:      narvipg.NewWebhookDeliveryStore(pool),
		cfg: &platform.Config{
			SlackSigningSecret:      "test-slack-signing-secret",
			SlackBotToken:           "test-slack-bot-token",
			LinearWebhookSecret:     "test-linear-webhook-secret",
			LinearOAuthClientID:     "test-linear-client-id",
			LinearOAuthClientSecret: "test-linear-client-secret",
			GitHubWebhookSecret:     "test-github-webhook-secret",
			GitHubBotHandle:         "narvi-test-bot",
			GitHubBotToken:          "test-github-bot-token",
		},
	}
	t.Cleanup(func() { _ = rig.registry.Shutdown() })

	for _, m := range mutate {
		m(&rig)
	}

	router := chi.NewRouter()
	// OIDC discovery + JWKS (§27.3) -- mounted PUBLICLY,
	// UNAUTHENTICATED, exactly like cmd/control-plane/main.go's own real
	// wiring (see httpapi/oidcdiscovery.go's own doc comment for the
	// full "why" and cloudidentitydiscovery_integration_test.go's own
	// pinning test: a request with NO credential of any kind must get
	// 200 from both).
	router.Get("/.well-known/openid-configuration", httpapi.OIDCDiscovery(rig.cloudIdentityIssuerURL))
	router.Get("/.well-known/jwks.json", httpapi.OIDCJWKS(rig.oidcSigningKeys, rig.cloudIdentityIssuerURL, platform.DefaultTimeouts()))

	router.Route("/api/sessions", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Post("/", httpapi.CreateSession(rig.pool, rig.sessions, rig.turns, rig.environments, rig.auditLog, rig.registry, nil, false, rig.rolloutMode, rig.repoSettings))
		r.Get("/{sessionID}", httpapi.GetSession(rig.sessions))
		r.Get("/{sessionID}/events", httpapi.ListEvents(rig.sessions, rig.events))
		r.Get("/{sessionID}/artifacts", httpapi.ListArtifacts(rig.sessions, rig.artifacts))
		// uploads ("uploads, blob storage & the in-sandbox
		// download_file tool", §28.4/§28.5) -- mounted exactly like
		// cmd/control-plane/main.go's own wiring (see uploadmint.go/
		// uploadconfirm.go/uploadcontent.go's own doc comments).
		r.Post("/{sessionID}/uploads", httpapi.MintUploadAPI(rig.sessions, rig.participants, rig.artifacts, rig.blobStore, rig.objCfg, platform.DefaultTimeouts()))
		r.Post("/{sessionID}/uploads/{uploadID}/complete", httpapi.ConfirmUploadAPI(rig.sessions, rig.participants, rig.pool, rig.artifacts, rig.events, rig.outbox, rig.sandboxes, rig.broadcaster, rig.blobStore, rig.objCfg))
		r.Get("/{sessionID}/uploads/{uploadID}/content", httpapi.UploadContentAPI(rig.sessions, rig.artifacts, rig.blobStore, rig.objCfg, platform.DefaultTimeouts()))
		r.Post("/{sessionID}/ws-token", httpapi.MintWSToken(rig.sessions, rig.wsTokens, platform.DefaultTimeouts()))
		r.Post("/{sessionID}/turns", httpapi.CreateTurn(rig.pool, rig.sessions, rig.turns, rig.plans, rig.participants, rig.auditLog, rig.registry, nil, rig.objCfg, false))
		r.Post("/{sessionID}/plans/{planId}/approve", httpapi.ApprovePlan(rig.pool, rig.sessions, rig.turns, rig.plans, rig.events, rig.planDocuments, rig.participants, rig.outbox, rig.linearAgentSessions, rig.auditLog, rig.registry, false))
		r.Post("/{sessionID}/plans/{planId}/reject", httpapi.RejectPlan(rig.pool, rig.sessions, rig.turns, rig.plans, rig.events, rig.planDocuments, rig.participants, rig.outbox, rig.linearAgentSessions, rig.auditLog, false))
		// Audit-fix batch (completeness/discoverability, M3) -- see
		// httpapi/plans.go's own doc comment.
		r.Get("/{sessionID}/plans", httpapi.ListPlans(rig.sessions, rig.plans, rig.turns, rig.events))
		// review/retrigger ("review sessions", §8.2's own manual
		// re-trigger-via-BUTTON surface) -- see reviewretrigger.go's own doc
		// comment. rig.diffFetcher/rig.botToken default nil/"" -- see this
		// rig's own diffFetcher field doc comment for why, and for how a
		// test overrides them.
		// reviewTriageDeps (§26.3; wired to REAL stores as of D6's
		// own adversarial-review fix -- this used to pass the bare zero
		// value appreviewtriage.Deps{}, which meant NO test anywhere in
		// this package's own integration suite could exercise
		// ComputeDecision's real repo_settings/review_verdicts reads (or,
		// critically for D1, the re-review floor those reads feed) through
		// this endpoint at all -- appreviewtriage.ComputeDecision/LoadConfig
		// are both nil-store-safe by construction, but "degrades safely"
		// and "is ever actually tested" are different properties, and the
		// second one had zero coverage). rig.repoSettings/rig.reviewVerdicts
		// are the SAME stores every other route in this rig already shares
		// -- no new store, no new pool connection. reviewModelDeep stays ""
		// (unconfigured) -- no test in this package needs a specific
		// deep-tier model id, only the depth decision itself.
		r.Post("/{sessionID}/review/retrigger", httpapi.RetriggerReview(rig.pool, rig.sessions, rig.turns, rig.plans, rig.auditLog, rig.registry, rig.prSessions, rig.diffFetcher, rig.reviewFindings, rig.falsePositivePatterns, rig.botToken, platform.DefaultTimeouts(), appreviewtriage.Deps{RepoSettings: rig.repoSettings, ReviewVerdicts: rig.reviewVerdicts}, ""))
		// review/findings/{identityHash}/rebut + apply-suggestion (§8.2)
		// -- see reviewfindings.go's own doc comment.
		r.Post("/{sessionID}/review/findings/{identityHash}/rebut", httpapi.RebutReviewFinding(rig.sessions, rig.prSessions, rig.reviewFindings, rig.auditLog))
		r.Post("/{sessionID}/review/findings/{identityHash}/apply-suggestion", httpapi.ApplySuggestion(rig.sessions, rig.prSessions, rig.reviewFindings, rig.identities, rig.sourceControl, rig.tokenEncryptionKey, platform.DefaultTimeouts()))
		// workflow-runs (§25.10's own two run-read routes) -- see
		// httpapi/workflowruns.go's own doc comment.
		r.Get("/{sessionID}/workflow-runs", httpapi.ListSessionWorkflowRuns(rig.sessions, rig.workflows))
	})
	// /api/members, /api/audit-log ("identities + full RBAC",
	// §13.2/§13.3) -- mounted exactly like cmd/control-plane/main.go's own
	// wiring (this file's own doc comment on that file's own precedent):
	// gated behind auth.Middleware only, with each handler rendering its
	// own admin-only authz.Authorize verdict.
	router.Route("/api/members", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.ListMembers(rig.users, rig.identities, rig.linkPrompts))
		r.Patch("/{userID}/role", httpapi.UpdateMemberRole(rig.pool, rig.users, rig.identities, rig.auditLog))
		r.Post("/{userID}/identities", httpapi.LinkMemberIdentity(rig.pool, rig.users, rig.identities, rig.auditLog))
		r.Delete("/{userID}/identities/{identityID}", httpapi.UnlinkMemberIdentity(rig.pool, rig.identities, rig.auditLog))
	})
	router.Route("/api/audit-log", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.ListAuditLog(rig.auditLog))
	})
	// /api/me ("web UI: sign-in", §12.2 item 7/§13.1) -- mounted
	// exactly like cmd/control-plane/main.go's own wiring.
	router.Route("/api/me", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.GetMe(rig.users, rig.identities))
	})
	// /api/me/chatgpt-link ("models: Codex via ChatGPT-account
	// OAuth", §29.3/§29.9) -- mounted exactly like cmd/control-plane/
	// main.go's own wiring.
	router.Route("/api/me/chatgpt-link", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		chatGPTLinkDeps := chatgptlink.Deps{
			Pool:                rig.pool,
			LinkAttempts:        rig.chatGPTLinkAttempts,
			ProviderCredentials: rig.providerCredentials,
			AuditLog:            rig.auditLog,
			DeviceFlow:          rig.chatGPTDeviceFlow,
			TokenEncryptionKey:  rig.tokenEncryptionKey,
			Timeouts:            platform.DefaultTimeouts(),
		}
		r.Post("/", httpapi.StartChatGPTLink(chatGPTLinkDeps))
		r.Get("/", httpapi.GetChatGPTLinkStatus(chatGPTLinkDeps))
		r.Delete("/", httpapi.DeleteChatGPTLink(chatGPTLinkDeps))
	})
	// /api/models ("models: Catalog", §8 item 8/§29/§25.2) --
	// mounted exactly like cmd/control-plane/main.go's own wiring.
	router.Route("/api/models", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.GetModelCatalog())
	})
	// /api/admin/shadow-compare ("shadow-comparison tooling for
	// review", §9.4/§18.5) -- mounted exactly like cmd/control-plane/
	// main.go's own wiring.
	router.Route("/api/admin/shadow-compare", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.GetShadowComparison(rig.turns))
	})
	// /api/intent-templates, /api/intent-templates/preview (audit finding
	// M5, completeness) -- mounted exactly like cmd/control-plane/main.go's
	// own wiring (see classifiertemplates.go's own doc comment).
	router.Route("/api/intent-templates", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Post("/preview", httpapi.PreviewIntentTemplate())
		r.Post("/", httpapi.UpsertIntentTemplate(rig.pool, rig.promptTemplates, rig.auditLog))
		r.Get("/", httpapi.ListPromptTemplates(rig.promptTemplates))
	})
	// /api/environments ("ui settings + analytics", §12.2 item 5) --
	// mounted exactly like cmd/control-plane/main.go's own wiring (see
	// environments.go's own doc comment).
	router.Route("/api/environments", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.ListEnvironments(rig.environments))
	})
	// scm-credentials is deliberately mounted OUTSIDE /api/sessions and
	// outside auth.Middleware entirely -- see scmcredentials.go's own doc
	// comment.
	router.Post("/sessions/{sessionID}/scm-credentials",
		httpapi.ScmCredentials(rig.sessions, rig.sandboxes, rig.identities, rig.users, rig.prSessions, rig.repoSettings, rig.shadowLedger, rig.readOnlyMinter, rig.botToken, rig.tokenEncryptionKey, platform.DefaultTimeouts(), rig.platformShadow))
	// snapshot-mint (§3.2, "snapshots & restore") is mounted the SAME
	// way -- see snapshotmint.go's own doc comment.
	router.Post("/sessions/{sessionID}/snapshot",
		httpapi.SnapshotMint(rig.sandboxes, rig.provider))
	// review/verdict ("server-side verdict", §8.2/§5.2) is mounted
	// the SAME way -- see reviewverdict.go's own doc comment.
	router.Post("/sessions/{sessionID}/review/verdict",
		httpapi.PostReviewVerdict(rig.pool, rig.sandboxes, rig.sessions, rig.prSessions, rig.repoSettings, rig.reviewFindings, rig.sentinelFixes, rig.outbox, rig.reviewVerdicts, rig.turns, rig.events, rig.botHandle, rig.botToken, rig.diffFetcher, rig.positionResolver, platform.DefaultTimeouts(), false))
	// workflow/step-outcome ("workflow execution engine", §25.6)
	// is mounted the SAME way -- see workflowstepoutcome.go's own doc
	// comment.
	router.Post("/sessions/{sessionID}/workflow/step-outcome",
		httpapi.PostWorkflowStepOutcome(rig.sandboxes, rig.workflows))
	// turn/epistemic-outcome ("builder epistemic pre-action
	// check", §20.2) is mounted the SAME way -- see epistemicoutcome.go's
	// own doc comment.
	router.Post("/sessions/{sessionID}/turn/epistemic-outcome",
		httpapi.PostEpistemicOutcome(rig.sandboxes, rig.turns))
	// uploads mint/confirm/content (§28.4/§28.5) sandbox-bearer
	// variants are mounted the SAME way -- see uploadmint.go/
	// uploadconfirm.go/uploadcontent.go's own doc comments.
	router.Post("/sessions/{sessionID}/uploads",
		httpapi.MintUpload(rig.sandboxes, rig.artifacts, rig.blobStore, rig.objCfg, platform.DefaultTimeouts()))
	router.Post("/sessions/{sessionID}/uploads/{uploadID}/complete",
		httpapi.ConfirmUpload(rig.sandboxes, rig.pool, rig.artifacts, rig.events, rig.outbox, rig.broadcaster, rig.blobStore, rig.objCfg))
	router.Get("/sessions/{sessionID}/uploads/{uploadID}/content",
		httpapi.UploadContent(rig.sandboxes, rig.artifacts, rig.blobStore, rig.objCfg, platform.DefaultTimeouts()))
	// /api/workflow-runs/{runId}/steps/{stepRunId}/decide ("workflow
	// HITL gate + circuit breaker", §25.9/§25.10/§25.11) -- mounted behind
	// auth.Middleware exactly like cmd/control-plane/main.go's own real
	// wiring (see decideworkflowstep.go's own doc comment).
	router.Route("/api/workflow-runs", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Post("/{runId}/steps/{stepRunId}/decide", httpapi.DecideWorkflowStep(rig.pool, rig.sessions, rig.turns, rig.participants, rig.workflows, rig.slackThreadSession, rig.linearAgentSessions, rig.prSessions, rig.outbox, rig.registry, false))
		// GET /{runId} ("workflow definition & run API", §25.10) -- see
		// httpapi/workflowruns.go's own doc comment.
		r.Get("/{runId}", httpapi.GetWorkflowRun(rig.sessions, rig.workflows))
	})
	// /api/workflow-definitions, /api/workflow-bindings ("workflow
	// definition & run API", §25.10/§25.11) -- mounted exactly
	// like cmd/control-plane/main.go's own wiring (see httpapi/
	// workflowdefinitions.go/workflowbindings.go's own doc comments).
	router.Route("/api/workflow-definitions", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.ListWorkflowDefinitions(rig.workflows))
		r.Post("/", httpapi.CreateWorkflowDefinition(rig.pool, rig.workflows))
		r.Get("/{id}", httpapi.GetWorkflowDefinition(rig.workflows))
		r.Put("/{id}", httpapi.PutWorkflowDefinition(rig.pool, rig.workflows))
		r.Delete("/{id}", httpapi.DeleteWorkflowDefinition(rig.pool, rig.workflows))
	})
	router.Route("/api/workflow-bindings", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.ListWorkflowBindings(rig.workflows))
		r.Put("/", httpapi.PutWorkflowBinding(rig.pool, rig.workflows))
	})
	// /api/repos/{owner}/{repo}/settings (§8.2) -- mounted behind
	// auth.Middleware, exactly like cmd/control-plane/main.go's own wiring
	// (see reposettings.go's own doc comment).
	//
	// reviewVerdictDeps (§21.1/§21.2) is built fresh here, from
	// stores this rig already constructs elsewhere (rig.reviewVerdicts/
	// rig.reviewFindings) plus two one-off stores no other route in this
	// rig needs -- mirrors cmd/control-plane/main.go's own identical
	// bundle, never a second, independently-maintained Deps shape.
	reviewVerdictDeps := appreviewverdict.Deps{
		ReviewVerdicts:       rig.reviewVerdicts,
		RepoSettings:         rig.repoSettings,
		ReviewFindings:       rig.reviewFindings,
		AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(rig.pool),
		// DigestSectionFeedback (§26.5) backs
		// appreviewverdict.DigestContestationRate -- the SAME one-off
		// "constructed inline, no dedicated rig field" treatment
		// AutoApprovalOutcomes immediately above already gets.
		DigestSectionFeedback: narvipg.NewReviewDigestSectionFeedbackStore(rig.pool),
		Timeouts:              platform.DefaultTimeouts(),
	}
	router.Route("/api/repos/{owner}/{repo}/settings", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.GetRepoSettings(rig.repoSettings, reviewVerdictDeps, rig.prSessions))
		r.Put("/", httpapi.PutRepoSettings(rig.repoSettings, rig.prSessions))
	})
	// /api/repos/{owner}/{repo}/preview-config (§4.1.2 amendment) --
	// mounted behind auth.Middleware, exactly like cmd/control-plane/
	// main.go's own wiring (see previewconfig.go's own doc comment).
	router.Route("/api/repos/{owner}/{repo}/preview-config", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.GetPreviewConfig(rig.repoSettings, rig.prSessions))
		r.Put("/", httpapi.PutPreviewConfig(rig.repoSettings, rig.prSessions))
	})
	// /api/repos/{owner}/{repo}/shadow-ledger[/activate] (§30.6/§30.8/
	// §30.9) -- mounted behind auth.Middleware, exactly like cmd/
	// control-plane/main.go's own wiring (see shadowledger.go's own doc
	// comment). A FRESH *narvipg.ShadowSCMWriteStore over rig.pool, not a
	// type assertion on rig.shadowLedger -- that field is typed as the
	// narrow shadowledger.Store interface elsewhere in this rig
	// specifically so scmcredentials_integration_test.go's own shadow-mint
	// failure tests can swap in a fake that always errors; asserting it
	// back to the concrete type here would panic the moment ANY test in
	// this package builds a rig with that fake installed, whether or not
	// that test ever touches these routes. Mirrors reviewfindings_
	// integration_test.go/scmcredentials_integration_test.go's own
	// identical "construct a fresh reader over rig.pool" precedent for
	// reaching ListForRepo, which the interface does not expose.
	router.Route("/api/repos/{owner}/{repo}/shadow-ledger", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		shadowLedgerStore := narvipg.NewShadowSCMWriteStore(rig.pool)
		shadowOperatorReads := narvipg.NewShadowOperatorReadStore(rig.pool)
		r.Get("/", httpapi.GetShadowLedger(shadowLedgerStore, shadowOperatorReads, rig.repoSettings, rig.prSessions))
		r.Post("/activate", httpapi.PostActivateShadowLedger(shadowLedgerStore, shadowOperatorReads, rig.repoSettings, rig.auditLog, rig.prSessions))
	})
	// /api/integrations (§12.5 amendment) -- mounted behind
	// auth.Middleware, exactly like cmd/control-plane/main.go's own
	// wiring (see integrations.go's own doc comment).
	router.Route("/api/integrations", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.GetIntegrations(rig.cfg, rig.outbox, rig.webhookDeliveries))
	})
	// /api/repos/{owner}/{repo}/false-positive-patterns (§22.4) --
	// mounted behind auth.Middleware, exactly like cmd/control-plane/
	// main.go's own wiring (see falsepositivepatterns.go's own doc
	// comment).
	router.Route("/api/repos/{owner}/{repo}/false-positive-patterns", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.ListFalsePositivePatterns(rig.falsePositivePatterns, rig.prSessions))
		r.Post("/{patternID}/retire", httpapi.RetireFalsePositivePattern(rig.falsePositivePatterns, rig.auditLog, rig.prSessions))
	})
	router.Route("/api/repos/{owner}/{repo}/auto-approval-settings", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Put("/", httpapi.PutAutoApprovalSettings(rig.repoSettings, reviewVerdictDeps, rig.prSessions))
	})
	router.Route("/api/repos/{owner}/{repo}/auto-merge", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Put("/", httpapi.PutAutoMergeToggle(rig.repoSettings, reviewVerdictDeps, rig.prSessions))
	})
	// /api/repos/{owner}/{repo}/auto-retrigger-review (§24.5) --
	// mounted behind auth.Middleware, exactly like cmd/control-plane/
	// main.go's own wiring (see reposettings.go's own
	// PutAutoRetriggerReviewToggle doc comment).
	router.Route("/api/repos/{owner}/{repo}/auto-retrigger-review", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Put("/", httpapi.PutAutoRetriggerReviewToggle(rig.repoSettings, rig.prSessions))
	})
	// /api/repos/{owner}/{repo}/description-autofix (§26.2) --
	// mounted behind auth.Middleware, exactly like cmd/control-plane/
	// main.go's own wiring (see reposettings.go's own
	// PutDescriptionAutofixToggle doc comment).
	router.Route("/api/repos/{owner}/{repo}/description-autofix", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Put("/", httpapi.PutDescriptionAutofixToggle(rig.repoSettings, rig.prSessions))
	})
	// /api/repos/{owner}/{repo}/review-depth (§26.3) -- mounted
	// behind auth.Middleware, exactly like cmd/control-plane/main.go's own
	// wiring (see reposettings.go's own PutReviewDepthConfig doc comment).
	// Adversarial-review fix, D8: this route was NOT mounted in this test
	// rig at all before this fix -- PutReviewDepthConfig/
	// ActionConfigureReviewDepth had zero test coverage anywhere in this
	// codebase (reproduced: widening the RBAC action's allowed roles from
	// admin-only to include maintainer/member/viewer left the entire test
	// suite green).
	router.Route("/api/repos/{owner}/{repo}/review-depth", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Put("/", httpapi.PutReviewDepthConfig(rig.repoSettings, rig.prSessions))
	})
	// /api/repos/{owner}/{repo}/review-cost-budget (§26.7) --
	// mounted behind auth.Middleware, exactly like cmd/control-plane/
	// main.go's own wiring (see reposettings.go's own PutReviewCostBudget
	// doc comment). B9 fix: this route was NOT mounted in this test rig at
	// all before this fix -- the SAME "zero test coverage anywhere in this
	// codebase" gap review-depth had before its own D8 fix immediately
	// above, which is exactly how the <0-vs-<=0 zero-ceiling defect this
	// Step's own review round found went unnoticed.
	router.Route("/api/repos/{owner}/{repo}/review-cost-budget", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Put("/", httpapi.PutReviewCostBudget(rig.repoSettings, rig.prSessions))
	})
	// /api/repos/{owner}/{repo}/review-analytics (§21.1) --
	// mounted behind auth.Middleware exactly like cmd/control-plane/
	// main.go's own wiring (see reviewanalytics.go's own doc comment).
	router.Route("/api/repos/{owner}/{repo}/review-analytics", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.GetReviewAnalytics(reviewVerdictDeps, rig.prSessions))
	})
	// /api/repos/{owner}/{repo}/digest-scope ("ui settings +
	// analytics", §12.2 item 5, §21.3) -- mounted exactly like
	// cmd/control-plane/main.go's own wiring (see digestscope.go's own
	// doc comment).
	router.Route("/api/repos/{owner}/{repo}/digest-scope", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.GetRepoDigestScope(rig.digestChannels, rig.prSessions, platform.DefaultTimeouts()))
	})
	// /api/repos/{owner}/{repo}/provider-credentials,
	// /api/environments/{environmentID}/provider-credentials,
	// /api/provider-credentials ("provider credential injection",
	// §25.1/§25.3) -- mounted exactly like cmd/control-plane/main.go's own
	// wiring (see providercredentials.go's own doc comment).
	router.Route("/api/repos/{owner}/{repo}/provider-credentials", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Post("/", httpapi.CreateRepoProviderCredential(rig.providerCredentials, rig.tokenEncryptionKey, rig.prSessions))
		r.Get("/", httpapi.ListRepoProviderCredentials(rig.providerCredentials, rig.prSessions))
		r.Put("/{credentialID}", httpapi.UpdateRepoProviderCredentialValue(rig.providerCredentials, rig.tokenEncryptionKey, rig.prSessions))
		r.Delete("/{credentialID}", httpapi.DeleteRepoProviderCredential(rig.providerCredentials, rig.prSessions))
	})
	router.Route("/api/environments/{environmentID}/provider-credentials", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Post("/", httpapi.CreateEnvironmentProviderCredential(rig.providerCredentials, rig.tokenEncryptionKey))
		r.Get("/", httpapi.ListEnvironmentProviderCredentials(rig.providerCredentials))
		r.Put("/{credentialID}", httpapi.UpdateEnvironmentProviderCredentialValue(rig.providerCredentials, rig.tokenEncryptionKey))
		r.Delete("/{credentialID}", httpapi.DeleteEnvironmentProviderCredential(rig.providerCredentials))
	})
	router.Route("/api/provider-credentials", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Post("/", httpapi.CreateGlobalProviderCredential(rig.providerCredentials, rig.tokenEncryptionKey))
		r.Get("/", httpapi.ListGlobalProviderCredentials(rig.providerCredentials))
		r.Put("/{credentialID}", httpapi.UpdateGlobalProviderCredentialValue(rig.providerCredentials, rig.tokenEncryptionKey))
		r.Delete("/{credentialID}", httpapi.DeleteGlobalProviderCredential(rig.providerCredentials))
	})
	// provider-credentials delivery (§25.1) is mounted the SAME way as
	// scm-credentials -- see providercredentialsdelivery.go's own doc
	// comment.
	router.Post("/sessions/{sessionID}/provider-credentials",
		httpapi.ProviderCredentialsDelivery(rig.sessions, rig.sandboxes, rig.providerCredentials, rig.tokenEncryptionKey))
	// /api/repos/{owner}/{repo}/sandbox-secrets,
	// /api/environments/{environmentID}/sandbox-secrets,
	// /api/sandbox-secrets, and their sandbox-facing delivery route
	// (§27.1) -- mounted exactly like cmd/control-plane/main.go's own
	// wiring (see sandboxsecrets.go's own doc comment).
	router.Route("/api/repos/{owner}/{repo}/sandbox-secrets", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Post("/", httpapi.CreateRepoSandboxSecret(rig.sandboxSecrets, rig.tokenEncryptionKey, rig.prSessions))
		r.Get("/", httpapi.ListRepoSandboxSecrets(rig.sandboxSecrets, rig.prSessions))
		r.Put("/{secretID}", httpapi.UpdateRepoSandboxSecretValue(rig.sandboxSecrets, rig.tokenEncryptionKey, rig.prSessions))
		r.Delete("/{secretID}", httpapi.DeleteRepoSandboxSecret(rig.sandboxSecrets, rig.prSessions))
	})
	router.Route("/api/environments/{environmentID}/sandbox-secrets", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Post("/", httpapi.CreateEnvironmentSandboxSecret(rig.sandboxSecrets, rig.tokenEncryptionKey))
		r.Get("/", httpapi.ListEnvironmentSandboxSecrets(rig.sandboxSecrets))
		r.Put("/{secretID}", httpapi.UpdateEnvironmentSandboxSecretValue(rig.sandboxSecrets, rig.tokenEncryptionKey))
		r.Delete("/{secretID}", httpapi.DeleteEnvironmentSandboxSecret(rig.sandboxSecrets))
	})
	router.Route("/api/sandbox-secrets", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Post("/", httpapi.CreateGlobalSandboxSecret(rig.sandboxSecrets, rig.tokenEncryptionKey))
		r.Get("/", httpapi.ListGlobalSandboxSecrets(rig.sandboxSecrets))
		r.Put("/{secretID}", httpapi.UpdateGlobalSandboxSecretValue(rig.sandboxSecrets, rig.tokenEncryptionKey))
		r.Delete("/{secretID}", httpapi.DeleteGlobalSandboxSecret(rig.sandboxSecrets))
	})
	router.Post("/sessions/{sessionID}/sandbox-secrets",
		httpapi.SandboxSecretsDelivery(rig.sessions, rig.sandboxes, rig.sandboxSecrets, rig.tokenEncryptionKey))
	// /api/environments/{environmentID}/cloud-identity-bindings,
	// /api/cloud-identity-bindings, /api/cloud-identity/signing-keys/rotate,
	// and the sandbox-facing minting route ("cloud identity:
	// OIDC issuer, bindings, minting", §27.3) -- mounted exactly like
	// cmd/control-plane/main.go's own wiring (see httpapi/
	// cloudidentitybindings.go/cloudidentitykeys.go/cloudidentitytoken.go's
	// own doc comments).
	router.Route("/api/environments/{environmentID}/cloud-identity-bindings", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Use(httpapi.RequireCloudIdentityCapability(rig.cloudIdentityIssuerURL))
		r.Post("/", httpapi.CreateEnvironmentCloudIdentityBinding(rig.pool, rig.cloudIdentityBindings, rig.auditLog))
		r.Get("/", httpapi.ListEnvironmentCloudIdentityBindings(rig.cloudIdentityBindings))
		r.Put("/{bindingID}", httpapi.UpdateEnvironmentCloudIdentityBinding(rig.pool, rig.cloudIdentityBindings, rig.auditLog))
		r.Delete("/{bindingID}", httpapi.DeleteEnvironmentCloudIdentityBinding(rig.pool, rig.cloudIdentityBindings, rig.auditLog))
	})
	router.Route("/api/cloud-identity-bindings", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Use(httpapi.RequireCloudIdentityCapability(rig.cloudIdentityIssuerURL))
		r.Post("/", httpapi.CreateGlobalCloudIdentityBinding(rig.pool, rig.cloudIdentityBindings, rig.auditLog))
		r.Get("/", httpapi.ListGlobalCloudIdentityBindings(rig.cloudIdentityBindings))
		r.Put("/{bindingID}", httpapi.UpdateGlobalCloudIdentityBinding(rig.pool, rig.cloudIdentityBindings, rig.auditLog))
		r.Delete("/{bindingID}", httpapi.DeleteGlobalCloudIdentityBinding(rig.pool, rig.cloudIdentityBindings, rig.auditLog))
	})
	router.Route("/api/cloud-identity/signing-keys", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Use(httpapi.RequireCloudIdentityCapability(rig.cloudIdentityIssuerURL))
		r.Post("/rotate", httpapi.RotateCloudIdentitySigningKey(rig.pool, rig.oidcSigningKeys, rig.auditLog, rig.tokenEncryptionKey, platform.DefaultTimeouts()))
	})
	router.Post("/sessions/{sessionID}/cloud-identity-token",
		httpapi.MintCloudIdentityToken(rig.sessions, rig.sandboxes, rig.cloudIdentityBindings, rig.oidcSigningKeys, rig.tokenEncryptionKey, rig.cloudIdentityIssuerURL, platform.DefaultTimeouts()))
	// /api/environments/{environmentID}/cluster-binding and its
	// sandbox-facing cloud-identity-config delivery route (
	// §27.3/§27.4) -- mounted exactly like cmd/control-plane/main.go's own
	// wiring (see httpapi/clusterbindings.go/cloudidentityconfigdelivery.go's
	// own doc comments).
	router.Route("/api/environments/{environmentID}/cluster-binding", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.GetEnvironmentClusterBinding(rig.clusterBindings))
		r.Put("/", httpapi.PutEnvironmentClusterBinding(rig.clusterBindings))
		r.Delete("/", httpapi.DeleteEnvironmentClusterBinding(rig.clusterBindings))
	})
	router.Post("/sessions/{sessionID}/cloud-identity-config",
		httpapi.CloudIdentityConfigDelivery(rig.sessions, rig.sandboxes, rig.cloudIdentityBindings, rig.clusterBindings))
	// /api/environments/{environmentID}/opencode-config, /api/opencode-config,
	// and their sandbox-facing delivery route (§27.2) -- mounted
	// exactly like cmd/control-plane/main.go's own wiring (see
	// opencodeconfig.go's own doc comment).
	router.Route("/api/environments/{environmentID}/opencode-config", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.GetEnvironmentOpenCodeConfig(rig.openCodeConfigs))
		r.Put("/", httpapi.PutEnvironmentOpenCodeConfig(rig.openCodeConfigs))
		r.Delete("/", httpapi.DeleteEnvironmentOpenCodeConfigHandler(rig.openCodeConfigs))
	})
	router.Route("/api/opencode-config", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Get("/", httpapi.GetGlobalOpenCodeConfig(rig.openCodeConfigs))
		r.Put("/", httpapi.PutGlobalOpenCodeConfig(rig.openCodeConfigs))
		r.Delete("/", httpapi.DeleteGlobalOpenCodeConfigHandler(rig.openCodeConfigs))
	})
	router.Post("/sessions/{sessionID}/opencode-config",
		httpapi.OpenCodeConfigDelivery(rig.sessions, rig.sandboxes, rig.openCodeConfigs))
	// /api/automations ("automations: triggers & extras", §8.4) --
	// mounted exactly like cmd/control-plane/main.go's own wiring (see
	// automations.go's own doc comment).
	router.Route("/api/automations", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Post("/", httpapi.CreateAutomation(rig.automations))
		r.Get("/", httpapi.ListAutomations(rig.automations))
		r.Get("/{automationID}", httpapi.GetAutomation(rig.automations))
		r.Get("/{automationID}/invocations", httpapi.ListAutomationInvocations(rig.automations, rig.automationInvocations, rig.automationRuns))
		r.Post("/{automationID}/pause", httpapi.PauseAutomation(rig.automations))
		r.Post("/{automationID}/resume", httpapi.ResumeAutomation(rig.automations))
		r.Post("/{automationID}/webhook-token", httpapi.RotateAutomationWebhookToken(rig.automations))
		r.Delete("/{automationID}/webhook-token", httpapi.RevokeAutomationWebhookToken(rig.automations))
	})
	// /webhooks/automations/{automationID} -- mounted OUTSIDE auth.Middleware
	// entirely, exactly like cmd/control-plane/main.go's own real wiring
	// (see internal/adapters/inbound/automationwebhook's own doc comment).
	// Review-fix addition: this rig's own rotate/revoke tests
	// (automationwebhooktoken_integration_test.go) need the REAL inbound
	// webhook handler mounted to prove a rotated/revoked token actually
	// stops authenticating against it, not just against the store layer
	// directly.
	router.Post("/webhooks/automations/{automationID}", automationwebhook.NewHandler(rig.automations, rig.automationInvocations))

	rig.server = httptest.NewServer(router)
	t.Cleanup(rig.server.Close)

	return rig
}

func (r testRig) createSession(ctx context.Context, t *testing.T) sqlcgen.Session {
	t.Helper()
	row, err := r.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
	if err != nil {
		t.Fatalf("create test session: %v", err)
	}
	return row
}

// markRepoKnown seeds a committed github_pr_sessions row for repoFullName --
// fix/repo-scoped-authorization's own entitlement signal (httpapi's own
// resolveKnownRepo, reposettings.go): every repo-scoped route this batch
// touches now 404s unless a row like this one exists, exactly mirroring
// what a REAL, HMAC-verified GitHub webhook mention would have produced
// (internal/adapters/inbound/github/coalesce.go's own EnsureRow ->
// LockForUpdate -> SetSessionID sequence, reused directly here rather than
// reinventing it -- the SAME pattern reviewfindings_integration_test.go/
// reviewretrigger_integration_test.go/scmcredentials_integration_test.go
// already establish for seeding a claimed PR). prNumber is fixed at 1 --
// none of this package's own repo-scoped tests care about a specific PR
// number, only that SOME committed row exists for repoFullName.
func (r testRig) markRepoKnown(ctx context.Context, t *testing.T, repoFullName string) {
	t.Helper()
	session := r.createSession(ctx, t)
	if err := r.prSessions.EnsureRow(ctx, repoFullName, 1); err != nil {
		t.Fatalf("markRepoKnown: EnsureRow(%q): %v", repoFullName, err)
	}
	if err := r.prSessions.SetSessionID(ctx, repoFullName, 1, session.ID); err != nil {
		t.Fatalf("markRepoKnown: SetSessionID(%q): %v", repoFullName, err)
	}
}

// createAuthenticatedUser builds a real user + linked GitHub identity + a
// user_sessions row directly via the stores (bypassing the OAuth flow
// entirely -- internal/adapters/inbound/auth's own package is what
// integration-tests that flow; this package only needs a REAL, valid
// session to attach as a cookie), and returns the created user row plus
// the PLAINTEXT session token.
func (r testRig) createAuthenticatedUser(ctx context.Context, t *testing.T) (sqlcgen.User, string) {
	t.Helper()

	externalID := fmt.Sprintf("test-github-id-%d", time.Now().UnixNano())
	email := externalID + "@example.com"

	user, err := r.users.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: email,
		DisplayName:  "Test User",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}

	if _, err := r.identities.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:        user.ID,
		Provider:      sqlcgen.IdentityProviderGithub,
		ExternalID:    externalID,
		Email:         &email,
		EmailVerified: true,
		LinkedVia:     sqlcgen.IdentityLinkedViaAdmin,
	}); err != nil {
		t.Fatalf("create test identity: %v", err)
	}

	token, err := platform.GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	if _, err := r.userSessions.Create(ctx, sqlcgen.CreateUserSessionParams{
		UserID:    user.ID,
		TokenHash: platform.HashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(platform.DefaultTimeouts().UserSessionTTL), Valid: true},
	}); err != nil {
		t.Fatalf("create test user session: %v", err)
	}

	return user, token
}

// sessionCountForUser returns how many rows exist in sessions with
// created_by = userID -- used by this file's own repo-validation rejection
// tests to prove a 400 happens strictly BEFORE sessions.WithTx(tx).Create,
// not merely that the handler returns the right status code.
func (r testRig) sessionCountForUser(ctx context.Context, t *testing.T, userID pgtype.UUID) int {
	t.Helper()
	var count int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE created_by = $1`, userID).Scan(&count); err != nil {
		t.Fatalf("count sessions for user: %v", err)
	}
	return count
}

// doJSON issues method against r.server.URL+path with an optional body,
// decoding the response body into v (if non-nil) and returning the status
// code. If token is non-empty, it is attached as the narvi_auth_session
// cookie -- pass "" to exercise the no-auth-at-all case.
func (r testRig) doJSON(t *testing.T, method, path string, body []byte, v any, token string) int {
	t.Helper()

	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, r.server.URL+path, reqBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: token})
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatalf("decode response body: %v", err)
		}
	}
	return resp.StatusCode
}

// --- Auth gate itself ---

// TestRoutes_RequireAuth proves every one of the 5 routes rejects a request
// with NO narvi_auth_session cookie at all with 401 -- the concrete proof
// §6.2's old open-access behavior is gone.
func TestRoutes_RequireAuth(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "CreateSession", method: http.MethodPost, path: "/api/sessions"},
		{name: "GetSession", method: http.MethodGet, path: "/api/sessions/" + session.ID.String()},
		{name: "ListEvents", method: http.MethodGet, path: "/api/sessions/" + session.ID.String() + "/events"},
		{name: "ListArtifacts", method: http.MethodGet, path: "/api/sessions/" + session.ID.String() + "/artifacts"},
		{name: "ListPlans", method: http.MethodGet, path: "/api/sessions/" + session.ID.String() + "/plans"},
		{name: "MintWSToken", method: http.MethodPost, path: "/api/sessions/" + session.ID.String() + "/ws-token"},
		{name: "CreateTurn", method: http.MethodPost, path: "/api/sessions/" + session.ID.String() + "/turns"},
		// uploads (§28.4/§28.5) -- the three /api-mounted browser
		// twins, review-fix coverage addition (FIX F): the sandbox-bearer
		// variants of these same three routes are deliberately mounted
		// OUTSIDE auth.Middleware entirely (their own bearer+gen handshake
		// is the auth), so only these /api ones belong in this table.
		{name: "MintUploadAPI", method: http.MethodPost, path: "/api/sessions/" + session.ID.String() + "/uploads"},
		{name: "ConfirmUploadAPI", method: http.MethodPost, path: "/api/sessions/" + session.ID.String() + "/uploads/00000000-0000-0000-0000-000000000000/complete"},
		{name: "UploadContentAPI", method: http.MethodGet, path: "/api/sessions/" + session.ID.String() + "/uploads/00000000-0000-0000-0000-000000000000/content"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status := rig.doJSON(t, tc.method, tc.path, []byte{}, nil, "" /* no cookie */)
			if status != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d (no auth cookie presented)", status, http.StatusUnauthorized)
			}
		})
	}
}

// --- CreateSession ---

func TestCreateSession_HappyPath(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{
		"spawnSource": "web",
		"title": "my session",
		"prompt": "do the thing",
		"repos": [{"name": "narvi", "url": "https://github.com/narvidev/narvi", "branch": null}],
		"modelId": null,"effort":null,
		"planMode": false
	}`)

	var got restdtos.Session
	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, &got, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if got.Id == "" {
		t.Error("Id is empty")
	}
	if got.Status != restdtos.SessionStatusCreated {
		t.Errorf("Status = %q, want %q", got.Status, restdtos.SessionStatusCreated)
	}
	if got.SpawnSource != restdtos.SessionSpawnSourceWeb {
		t.Errorf("SpawnSource = %q, want %q", got.SpawnSource, restdtos.SessionSpawnSourceWeb)
	}
	// The concrete proof §6.2's own "created_by always NULL" gap is
	// closed: it now matches the REAL authenticated caller's id.
	if got.CreatedBy == nil {
		t.Fatal("CreatedBy = nil, want the authenticated user's id")
	}
	if *got.CreatedBy != user.ID.String() {
		t.Errorf("CreatedBy = %q, want %q", *got.CreatedBy, user.ID.String())
	}
	if got.Title == nil || *got.Title != "my session" {
		t.Errorf("Title = %v, want \"my session\"", got.Title)
	}
	if got.Archived {
		t.Error("Archived = true, want false for a freshly created session")
	}

	// §9.3 ("e2e happy path"): repos is now actually persisted, and a
	// pending turn was created carrying the prompt/planMode -- the
	// concrete proof create.go's own doc comment describes.
	var sessionID pgtype.UUID
	if err := sessionID.Scan(got.Id); err != nil {
		t.Fatalf("scan session id: %v", err)
	}
	var reposJSON []byte
	if err := rig.pool.QueryRow(ctx, `SELECT repos FROM sessions WHERE id = $1`, sessionID).Scan(&reposJSON); err != nil {
		t.Fatalf("query persisted repos: %v", err)
	}
	var repos []map[string]any
	if err := json.Unmarshal(reposJSON, &repos); err != nil {
		t.Fatalf("unmarshal persisted repos: %v", err)
	}
	if len(repos) != 1 || repos[0]["name"] != "narvi" {
		t.Errorf("persisted repos = %s, want one entry named %q", reposJSON, "narvi")
	}

	turns, err := rig.turns.ListForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("len(turns) = %d, want 1 (prompt was non-nil)", len(turns))
	}
	if turns[0].Status != sqlcgen.TurnStatusPending {
		t.Errorf("turn status = %s, want %s", turns[0].Status, sqlcgen.TurnStatusPending)
	}
	if turns[0].Prompt == nil || *turns[0].Prompt != "do the thing" {
		t.Errorf("turn prompt = %v, want %q", turns[0].Prompt, "do the thing")
	}
}

// TestCreateSession_HappyPath_WritesSessionCreateAuditRow proves the
// audit-fix batch's own M9 fix: unlike plan.approve/plan.reject (already
// fully covered, existence AND detail-JSON shape, by
// decideplan_integration_test.go's own TestDecidePlanOnTx_Approve_
// AuditDetailCarriesTurnID/_Reject_AuditDetailHasNoTurnID), session.create
// (create.go's own CreateSessionOnTx, "session.create" written right after
// the session insert) had ZERO test coverage of any kind before this fix
// -- no existence check, nothing. This proves a real session.create row
// exists after a successful POST /api/sessions, with the correct action
// string/actor, AND that its own detail_json actually carries
// {"spawn_source": ...} -- mirrors internal/app/identitylink/
// service_integration_test.go's own TestResolve_ExactlyOneMatchAutoLinks
// AuditLog.List(ctx, 10, 0) idiom, extended (that precedent itself never
// does) to also decode entries[i].DetailJson.
func TestCreateSession_HappyPath_WritesSessionCreateAuditRow(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{"spawnSource":"web","title":null,"prompt":null,"repos":[{"name":"narvi","url":"https://example.com","branch":null}],"modelId":null,"effort":null,"planMode":false}`)
	var got restdtos.Session
	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, &got, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	entries, err := rig.auditLog.List(ctx, 10, 0)
	if err != nil {
		t.Fatalf("AuditLog.List: %v", err)
	}
	var entry *sqlcgen.AuditLog
	for i := range entries {
		if entries[i].Action == "session.create" && entries[i].ResourceID == got.Id {
			entry = &entries[i]
			break
		}
	}
	if entry == nil {
		t.Fatalf("no session.create audit_log row found for session %s among %d entries", got.Id, len(entries))
	}
	if entry.ResourceType != "session" {
		t.Errorf("ResourceType = %q, want %q", entry.ResourceType, "session")
	}
	if !entry.ActorUserID.Valid || entry.ActorUserID != user.ID {
		t.Errorf("ActorUserID = %v, want %v (the authenticated caller)", entry.ActorUserID, user.ID)
	}

	var detail map[string]any
	if err := json.Unmarshal(entry.DetailJson, &detail); err != nil {
		t.Fatalf("unmarshal detail_json: %v", err)
	}
	if detail["spawn_source"] != "web" {
		t.Errorf("detail_json[spawn_source] = %v, want %q", detail["spawn_source"], "web")
	}
}

// TestCreateSession_NoPrompt_NoTurnCreated proves a nil prompt creates the
// session with NO turn row at all -- CreateSessionRequest.Prompt being
// nil means "create the session without dispatching a first turn".
func TestCreateSession_NoPrompt_NoTurnCreated(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{"spawnSource":"web","title":null,"prompt":null,"repos":[{"name":"narvi","url":"https://example.com","branch":null}],"modelId":null,"effort":null,"planMode":false}`)

	var got restdtos.Session
	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, &got, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var sessionID pgtype.UUID
	if err := sessionID.Scan(got.Id); err != nil {
		t.Fatalf("scan session id: %v", err)
	}
	turns, err := rig.turns.ListForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("list turns: %v", err)
	}
	if len(turns) != 0 {
		t.Errorf("len(turns) = %d, want 0 (prompt was nil)", len(turns))
	}
}

func TestCreateSession_NoAuth(t *testing.T) {
	rig := newTestRig(t)

	body := []byte(`{"spawnSource":"web","title":null,"prompt":null,"repos":[{"name":"narvi","url":"https://example.com","branch":null}],"modelId":null,"effort":null,"planMode":false}`)
	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, nil, "" /* no cookie */)
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

func TestCreateSession_EmptyRepos(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{"spawnSource":"web","title":null,"prompt":null,"repos":[],"modelId":null,"effort":null,"planMode":false}`)
	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, nil, token)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

// TestCreateSession_InvalidRepoURL_Rejected proves every one of
// reposource.ValidateRepoURL's own rejection reasons is checked at this
// handler's own trust boundary (before any Postgres write), not only much
// later at actual git-invocation time deep inside the sandbox agent: a
// non-https scheme, a URL that fails net/url.Parse outright, and a URL
// that parses but has no host.
func TestCreateSession_InvalidRepoURL_Rejected(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{name: "non-https scheme", url: "http://example.com"},
		{name: "git scheme", url: "git://example.com/repo.git"},
		{name: "fails to parse", url: "https://%zz"},
		{name: "no host", url: "https://"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			user, token := rig.createAuthenticatedUser(ctx, t)

			body := []byte(fmt.Sprintf(`{"spawnSource":"web","title":null,"prompt":null,"repos":[{"name":"narvi","url":%q,"branch":null}],"modelId":null,"effort":null,"planMode":false}`, tc.url))
			status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, nil, token)
			if status != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
			}
			if count := rig.sessionCountForUser(ctx, t, user.ID); count != 0 {
				t.Errorf("sessions for user = %d, want 0 (rejected before any Postgres write)", count)
			}
		})
	}
}

// TestCreateSession_InvalidRepoName_PathTraversal_Rejected proves a repo
// name shaped like a path-traversal payload is rejected here -- repo.Name
// later reaches filepath.Join(workspaceDir, repo.Name) inside gitclone, so
// it is exactly as much a risk as an unvalidated Url/Branch, even though
// the audit finding's own summary names only url/branch explicitly.
func TestCreateSession_InvalidRepoName_PathTraversal_Rejected(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{"spawnSource":"web","title":null,"prompt":null,"repos":[{"name":"../escaped","url":"https://example.com","branch":null}],"modelId":null,"effort":null,"planMode":false}`)
	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, nil, token)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
	if count := rig.sessionCountForUser(ctx, t, user.ID); count != 0 {
		t.Errorf("sessions for user = %d, want 0 (rejected before any Postgres write)", count)
	}
}

// TestCreateSession_InvalidRepoBranch_DashPrefix_Rejected proves a branch
// beginning with "-" -- the argument-injection-shaped payload
// internal/domain/reposource's own tests already use as their canonical
// example -- is rejected here too.
func TestCreateSession_InvalidRepoBranch_DashPrefix_Rejected(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{"spawnSource":"web","title":null,"prompt":null,"repos":[{"name":"narvi","url":"https://example.com","branch":"--upload-pack=evil"}],"modelId":null,"effort":null,"planMode":false}`)
	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, nil, token)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
	if count := rig.sessionCountForUser(ctx, t, user.ID); count != 0 {
		t.Errorf("sessions for user = %d, want 0 (rejected before any Postgres write)", count)
	}
}

// TestCreateSession_NilBranch_Succeeds proves a nil branch on an otherwise
// -valid repo is never accidentally rejected -- nil means "use the repo's
// own default branch" and must never reach reposource.ValidateBranch at
// all.
func TestCreateSession_NilBranch_Succeeds(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{"spawnSource":"web","title":null,"prompt":null,"repos":[{"name":"narvi","url":"https://example.com","branch":null}],"modelId":null,"effort":null,"planMode":false}`)
	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, nil, token)
	if status != http.StatusCreated {
		t.Errorf("status = %d, want %d", status, http.StatusCreated)
	}
}

// TestCreateSession_MultipleRepos_SecondInvalid_Rejected proves the repo
// validation loop actually inspects EVERY repo, in order -- a valid first
// repo must never cause an invalid second repo to be skipped.
func TestCreateSession_MultipleRepos_SecondInvalid_Rejected(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{"spawnSource":"web","title":null,"prompt":null,"repos":[{"name":"narvi","url":"https://example.com","branch":null},{"name":"../escaped","url":"https://example.com","branch":null}],"modelId":null,"effort":null,"planMode":false}`)
	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, nil, token)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
	if count := rig.sessionCountForUser(ctx, t, user.ID); count != 0 {
		t.Errorf("sessions for user = %d, want 0 (rejected before any Postgres write)", count)
	}
}

// --- CreateSession: spawnSource forced to "web" (audit remediation,
// wire-contract/security-adjacent lens) ---

// TestCreateSession_NonWebSpawnSource_Rejected proves this REST handler --
// the ONLY authenticated REST session-creation path -- rejects a request
// that explicitly claims a non-"web" spawnSource, rather than persisting
// it verbatim. Before this fix, an authenticated web caller could POST
// spawnSource: "slack" (or linear/github) and have it land on the new
// session row as-is, forging provenance in the UI's own source icons/
// filters and audit rows, and steering app/sessionactor/outboxenqueue.go's
// own turn-completion outbox routing (which genuinely branches on
// sessions.spawn_source) down a channel this session was never actually
// created through. Table-driven over the three non-"web" enum values;
// each is rejected 400 BEFORE any Postgres write, matching the established
// repo/pathScope/mockConfig rejection-test precedent above exactly
// (asserting zero session rows for the calling user, not merely the status
// code).
func TestCreateSession_NonWebSpawnSource_Rejected(t *testing.T) {
	tests := []struct {
		name        string
		spawnSource string
	}{
		{name: "slack", spawnSource: "slack"},
		{name: "linear", spawnSource: "linear"},
		{name: "github", spawnSource: "github"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			user, token := rig.createAuthenticatedUser(ctx, t)

			body := []byte(fmt.Sprintf(`{"spawnSource":%q,"title":null,"prompt":null,"repos":[{"name":"narvi","url":"https://example.com","branch":null}],"modelId":null,"effort":null,"planMode":false}`, tc.spawnSource))
			var errBody map[string]string
			status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, &errBody, token)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", status, http.StatusBadRequest)
			}
			if errBody["error"] == "" {
				t.Error(`error body["error"] is empty, want a clear validation error naming spawnSource`)
			}
			if count := rig.sessionCountForUser(ctx, t, user.ID); count != 0 {
				t.Errorf("sessions for user = %d, want 0 (rejected before any Postgres write, and no forged spawn_source ever reached Postgres)", count)
			}
		})
	}
}

// TestCreateSession_WebSpawnSource_PersistsAsWeb is the companion positive
// case to TestCreateSession_NonWebSpawnSource_Rejected above: an explicit
// "web" spawnSource -- the one legitimate value on this endpoint -- is
// still accepted and persists exactly as sent, both in the response DTO
// and (queried directly, not merely trusting the handler's own echo) in
// the sessions.spawn_source column itself.
func TestCreateSession_WebSpawnSource_PersistsAsWeb(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{"spawnSource":"web","title":null,"prompt":null,"repos":[{"name":"narvi","url":"https://example.com","branch":null}],"modelId":null,"effort":null,"planMode":false}`)
	var got restdtos.Session
	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, &got, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if got.SpawnSource != restdtos.SessionSpawnSourceWeb {
		t.Errorf("SpawnSource = %q, want %q", got.SpawnSource, restdtos.SessionSpawnSourceWeb)
	}

	var sessionID pgtype.UUID
	if err := sessionID.Scan(got.Id); err != nil {
		t.Fatalf("scan session id: %v", err)
	}
	var persisted string
	if err := rig.pool.QueryRow(ctx, `SELECT spawn_source FROM sessions WHERE id = $1`, sessionID).Scan(&persisted); err != nil {
		t.Fatalf("query persisted spawn_source: %v", err)
	}
	if persisted != "web" {
		t.Errorf("persisted spawn_source = %q, want %q", persisted, "web")
	}
}

// TestCreateSession_EpistemicCheckEnabled_PersistsToPostgres is the
// test-wiring bundle's own addition (adversarial review): proves the REST
// epistemicCheckEnabled field (§20.4) actually reaches
// sessions.epistemic_check_enabled -- before this test existed, mutating
// CreateSessionOnTx's own EpistemicCheckEnabled: (*bool)(req.
// EpistemicCheckEnabled) to nil (create.go's own CreateSessionParams
// literal) failed nothing, mirroring TestCreateSession_WebSpawnSource_
// PersistsAsWeb's own identical shape for spawn_source. Also proves the
// negative: an ABSENT epistemicCheckEnabled field persists NULL (§20.4's
// own "session's own nullable override, unset means never opted in"), not
// a coerced false -- see internal/domain/turn.ResolveEpistemicCheckEnabled's
// own doc comment for why NULL vs. false is a real, load-bearing
// distinction here, not a cosmetic one.
func TestCreateSession_EpistemicCheckEnabled_PersistsToPostgres(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	t.Run("true carries through", func(t *testing.T) {
		_, token := rig.createAuthenticatedUser(ctx, t)

		body := []byte(`{"spawnSource":"web","title":null,"prompt":null,"repos":[{"name":"narvi","url":"https://example.com","branch":null}],"modelId":null,"effort":null,"planMode":false,"epistemicCheckEnabled":true}`)
		var got restdtos.Session
		status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, &got, token)
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want %d", status, http.StatusCreated)
		}

		var sessionID pgtype.UUID
		if err := sessionID.Scan(got.Id); err != nil {
			t.Fatalf("scan session id: %v", err)
		}
		var persisted *bool
		if err := rig.pool.QueryRow(ctx, `SELECT epistemic_check_enabled FROM sessions WHERE id = $1`, sessionID).Scan(&persisted); err != nil {
			t.Fatalf("query persisted epistemic_check_enabled: %v", err)
		}
		if persisted == nil || !*persisted {
			t.Errorf("persisted epistemic_check_enabled = %v, want true", persisted)
		}
	})

	t.Run("false carries through", func(t *testing.T) {
		_, token := rig.createAuthenticatedUser(ctx, t)

		body := []byte(`{"spawnSource":"web","title":null,"prompt":null,"repos":[{"name":"narvi","url":"https://example.com","branch":null}],"modelId":null,"effort":null,"planMode":false,"epistemicCheckEnabled":false}`)
		var got restdtos.Session
		status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, &got, token)
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want %d", status, http.StatusCreated)
		}

		var sessionID pgtype.UUID
		if err := sessionID.Scan(got.Id); err != nil {
			t.Fatalf("scan session id: %v", err)
		}
		var persisted *bool
		if err := rig.pool.QueryRow(ctx, `SELECT epistemic_check_enabled FROM sessions WHERE id = $1`, sessionID).Scan(&persisted); err != nil {
			t.Fatalf("query persisted epistemic_check_enabled: %v", err)
		}
		if persisted == nil || *persisted {
			t.Errorf("persisted epistemic_check_enabled = %v, want false", persisted)
		}
	})

	t.Run("absent leaves NULL, not coerced false", func(t *testing.T) {
		_, token := rig.createAuthenticatedUser(ctx, t)

		body := []byte(`{"spawnSource":"web","title":null,"prompt":null,"repos":[{"name":"narvi","url":"https://example.com","branch":null}],"modelId":null,"effort":null,"planMode":false}`)
		var got restdtos.Session
		status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, &got, token)
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want %d", status, http.StatusCreated)
		}

		var sessionID pgtype.UUID
		if err := sessionID.Scan(got.Id); err != nil {
			t.Fatalf("scan session id: %v", err)
		}
		var persisted *bool
		if err := rig.pool.QueryRow(ctx, `SELECT epistemic_check_enabled FROM sessions WHERE id = $1`, sessionID).Scan(&persisted); err != nil {
			t.Fatalf("query persisted epistemic_check_enabled: %v", err)
		}
		if persisted != nil {
			t.Errorf("persisted epistemic_check_enabled = %v, want NULL (absent means no override, never coerced to false)", *persisted)
		}
	})
}

// --- CreateSession: pathScope / Environment (row 10, "domain: Environment
// scoping", §14.1) ---

// TestCreateSession_InvalidPathScope_Rejected proves a pathScope pattern
// containing a ".." segment -- internal/domain/environment.
// ValidatePathScope's own ErrPathTraversal case -- is rejected 400 BEFORE
// any Postgres write, matching the established repo-validation precedent
// exactly (assert zero session rows for the calling user).
func TestCreateSession_InvalidPathScope_Rejected(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{
		"spawnSource": "web",
		"title": null,
		"prompt": null,
		"repos": [{"name": "narvi", "url": "https://example.com", "branch": null}],
		"modelId": null,"effort":null,
		"planMode": false,
		"pathScope": ["apps/../etc"]
	}`)

	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, nil, token)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
	if count := rig.sessionCountForUser(ctx, t, user.ID); count != 0 {
		t.Errorf("sessions for user = %d, want 0 (rejected before any Postgres write)", count)
	}
}

// TestCreateSession_ValidPathScope_CreatesEnvironment proves a valid,
// non-empty pathScope creates a real environments row (with the exact
// pattern list persisted), sets the new session's environment_id to that
// row's id, and sets provenance_tag to CreateSession's own chosen
// scopedEnvironmentProvenanceTag value.
func TestCreateSession_ValidPathScope_CreatesEnvironment(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{
		"spawnSource": "web",
		"title": null,
		"prompt": null,
		"repos": [{"name": "narvi", "url": "https://example.com", "branch": null}],
		"modelId": null,"effort":null,
		"planMode": false,
		"pathScope": ["/apps/web/*", "/apps/api/*"]
	}`)

	var got restdtos.Session
	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, &got, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var sessionID pgtype.UUID
	if err := sessionID.Scan(got.Id); err != nil {
		t.Fatalf("scan session id: %v", err)
	}

	var environmentID pgtype.UUID
	var provenanceTag *string
	if err := rig.pool.QueryRow(ctx,
		`SELECT environment_id, provenance_tag FROM sessions WHERE id = $1`, sessionID,
	).Scan(&environmentID, &provenanceTag); err != nil {
		t.Fatalf("query persisted session: %v", err)
	}
	if !environmentID.Valid {
		t.Fatal("environment_id = NULL, want a real environments row id")
	}
	if provenanceTag == nil || *provenanceTag == "" {
		t.Fatal("provenance_tag = nil/empty, want a non-empty value")
	}

	var pathScopeJSON []byte
	var mockConfigured bool
	if err := rig.pool.QueryRow(ctx,
		`SELECT path_scope, mock_configured FROM environments WHERE id = $1`, environmentID,
	).Scan(&pathScopeJSON, &mockConfigured); err != nil {
		t.Fatalf("query persisted environment: %v", err)
	}
	var pathScope []string
	if err := json.Unmarshal(pathScopeJSON, &pathScope); err != nil {
		t.Fatalf("unmarshal persisted path_scope: %v", err)
	}
	want := []string{"/apps/web/*", "/apps/api/*"}
	if len(pathScope) != len(want) || pathScope[0] != want[0] || pathScope[1] != want[1] {
		t.Errorf("persisted path_scope = %v, want %v", pathScope, want)
	}
	if mockConfigured {
		t.Error("mock_configured = true, want false (nothing in this call path sets it)")
	}
}

// TestCreateSession_NilPathScope_LeavesEnvironmentUnset proves an
// absent/nil pathScope leaves both environment_id and provenance_tag NULL,
// exactly matching pre-existing (pre-this-batch) behavior -- no
// environments row is created at all.
func TestCreateSession_NilPathScope_LeavesEnvironmentUnset(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{"spawnSource":"web","title":null,"prompt":null,"repos":[{"name":"narvi","url":"https://example.com","branch":null}],"modelId":null,"effort":null,"planMode":false,"pathScope":null}`)

	var got restdtos.Session
	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, &got, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var sessionID pgtype.UUID
	if err := sessionID.Scan(got.Id); err != nil {
		t.Fatalf("scan session id: %v", err)
	}

	var environmentID pgtype.UUID
	var provenanceTag *string
	if err := rig.pool.QueryRow(ctx,
		`SELECT environment_id, provenance_tag FROM sessions WHERE id = $1`, sessionID,
	).Scan(&environmentID, &provenanceTag); err != nil {
		t.Fatalf("query persisted session: %v", err)
	}
	if environmentID.Valid {
		t.Errorf("environment_id = %v, want NULL (pathScope was null)", environmentID)
	}
	if provenanceTag != nil {
		t.Errorf("provenance_tag = %q, want nil (pathScope was null)", *provenanceTag)
	}

	var environmentCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM environments`).Scan(&environmentCount); err != nil {
		t.Fatalf("count environments: %v", err)
	}
	if environmentCount != 0 {
		t.Errorf("environments row count = %d, want 0 (no pathScope was supplied)", environmentCount)
	}
}

// TestCreateSession_AbsentPathScope_LeavesEnvironmentUnset proves the
// pathScope key being entirely ABSENT from the request body (not merely
// present-and-null) behaves identically to TestCreateSession_
// NilPathScope_LeavesEnvironmentUnset -- pathScope is genuinely optional,
// not just nullable (unlike every other field on this DTO).
func TestCreateSession_AbsentPathScope_LeavesEnvironmentUnset(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	// No "pathScope" key at all -- this is TestCreateSession_HappyPath's own
	// exact request body, re-run unmodified to confirm this batch changed
	// nothing about it.
	body := []byte(`{"spawnSource":"web","title":null,"prompt":null,"repos":[{"name":"narvi","url":"https://example.com","branch":null}],"modelId":null,"effort":null,"planMode":false}`)

	var got restdtos.Session
	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, &got, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var sessionID pgtype.UUID
	if err := sessionID.Scan(got.Id); err != nil {
		t.Fatalf("scan session id: %v", err)
	}

	var environmentID pgtype.UUID
	var provenanceTag *string
	if err := rig.pool.QueryRow(ctx,
		`SELECT environment_id, provenance_tag FROM sessions WHERE id = $1`, sessionID,
	).Scan(&environmentID, &provenanceTag); err != nil {
		t.Fatalf("query persisted session: %v", err)
	}
	if environmentID.Valid {
		t.Errorf("environment_id = %v, want NULL (pathScope was absent)", environmentID)
	}
	if provenanceTag != nil {
		t.Errorf("provenance_tag = %q, want nil (pathScope was absent)", *provenanceTag)
	}
}

// TestCreateSession_EmptyPathScope_LeavesEnvironmentUnset proves an empty
// (present, non-null, zero-length) pathScope array is treated the same as
// nil/absent -- "unscoped" -- never creating an environments row nor
// calling ValidatePathScope (which would trivially accept an empty slice
// anyway, but this proves CreateSession's own hasPathScope gate, not just
// ValidatePathScope's tolerance of it).
func TestCreateSession_EmptyPathScope_LeavesEnvironmentUnset(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{"spawnSource":"web","title":null,"prompt":null,"repos":[{"name":"narvi","url":"https://example.com","branch":null}],"modelId":null,"effort":null,"planMode":false,"pathScope":[]}`)

	var got restdtos.Session
	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, &got, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var sessionID pgtype.UUID
	if err := sessionID.Scan(got.Id); err != nil {
		t.Fatalf("scan session id: %v", err)
	}

	var environmentID pgtype.UUID
	if err := rig.pool.QueryRow(ctx,
		`SELECT environment_id FROM sessions WHERE id = $1`, sessionID,
	).Scan(&environmentID); err != nil {
		t.Fatalf("query persisted session: %v", err)
	}
	if environmentID.Valid {
		t.Errorf("environment_id = %v, want NULL (pathScope was empty)", environmentID)
	}
}

// TestCreateSession_MockConfigPresent_ContractsPathOmitted_DefaultsAndCreatesEnvironment
// proves row 27's ("mocking + contract drift", §14.3) own core semantics:
// a present "mockConfig" key (even as {}, contractsPath absent) creates an
// environments row with mock_configured=true and contracts_path defaulting
// to the literal "contracts/api".
func TestCreateSession_MockConfigPresent_ContractsPathOmitted_DefaultsAndCreatesEnvironment(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{
		"spawnSource": "web",
		"title": null,
		"prompt": null,
		"repos": [{"name": "narvi", "url": "https://example.com", "branch": null}],
		"modelId": null,"effort":null,
		"planMode": false,
		"mockConfig": {}
	}`)

	var got restdtos.Session
	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, &got, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var sessionID pgtype.UUID
	if err := sessionID.Scan(got.Id); err != nil {
		t.Fatalf("scan session id: %v", err)
	}

	var environmentID pgtype.UUID
	if err := rig.pool.QueryRow(ctx,
		`SELECT environment_id FROM sessions WHERE id = $1`, sessionID,
	).Scan(&environmentID); err != nil {
		t.Fatalf("query persisted session: %v", err)
	}
	if !environmentID.Valid {
		t.Fatal("environment_id = NULL, want a real environments row id")
	}

	var mockConfigured bool
	var contractsPath *string
	if err := rig.pool.QueryRow(ctx,
		`SELECT mock_configured, contracts_path FROM environments WHERE id = $1`, environmentID,
	).Scan(&mockConfigured, &contractsPath); err != nil {
		t.Fatalf("query persisted environment: %v", err)
	}
	if !mockConfigured {
		t.Error("mock_configured = false, want true")
	}
	if contractsPath == nil || *contractsPath != "contracts/api" {
		t.Errorf("contracts_path = %v, want %q", contractsPath, "contracts/api")
	}
}

// TestCreateSession_MockConfigPresent_ContractsPathSet_StoredVerbatim
// proves an explicit mockConfig.contractsPath is stored verbatim, not
// defaulted.
func TestCreateSession_MockConfigPresent_ContractsPathSet_StoredVerbatim(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{
		"spawnSource": "web",
		"title": null,
		"prompt": null,
		"repos": [{"name": "narvi", "url": "https://example.com", "branch": null}],
		"modelId": null,"effort":null,
		"planMode": false,
		"mockConfig": {"contractsPath": "services/mock-api/contracts"}
	}`)

	var got restdtos.Session
	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, &got, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var sessionID pgtype.UUID
	if err := sessionID.Scan(got.Id); err != nil {
		t.Fatalf("scan session id: %v", err)
	}

	var environmentID pgtype.UUID
	if err := rig.pool.QueryRow(ctx,
		`SELECT environment_id FROM sessions WHERE id = $1`, sessionID,
	).Scan(&environmentID); err != nil {
		t.Fatalf("query persisted session: %v", err)
	}
	if !environmentID.Valid {
		t.Fatal("environment_id = NULL, want a real environments row id")
	}

	var mockConfigured bool
	var contractsPath *string
	if err := rig.pool.QueryRow(ctx,
		`SELECT mock_configured, contracts_path FROM environments WHERE id = $1`, environmentID,
	).Scan(&mockConfigured, &contractsPath); err != nil {
		t.Fatalf("query persisted environment: %v", err)
	}
	if !mockConfigured {
		t.Error("mock_configured = false, want true")
	}
	if contractsPath == nil || *contractsPath != "services/mock-api/contracts" {
		t.Errorf("contracts_path = %v, want %q", contractsPath, "services/mock-api/contracts")
	}
}

// TestCreateSession_MockConfigPresent_PathScopeAbsent_StillCreatesEnvironment
// proves the "either" gate (row 27's own doc comment on CreateSession):
// mockConfig ALONE, with pathScope entirely absent, is sufficient to
// create a new, session-scoped Environment row -- pathScope is NOT
// required.
func TestCreateSession_MockConfigPresent_PathScopeAbsent_StillCreatesEnvironment(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{"spawnSource":"web","title":null,"prompt":null,"repos":[{"name":"narvi","url":"https://example.com","branch":null}],"modelId":null,"effort":null,"planMode":false,"mockConfig":{}}`)

	var got restdtos.Session
	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, &got, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var sessionID pgtype.UUID
	if err := sessionID.Scan(got.Id); err != nil {
		t.Fatalf("scan session id: %v", err)
	}

	var environmentID pgtype.UUID
	var provenanceTag *string
	if err := rig.pool.QueryRow(ctx,
		`SELECT environment_id, provenance_tag FROM sessions WHERE id = $1`, sessionID,
	).Scan(&environmentID, &provenanceTag); err != nil {
		t.Fatalf("query persisted session: %v", err)
	}
	if !environmentID.Valid {
		t.Fatal("environment_id = NULL, want a real environments row id (mockConfig alone must be sufficient)")
	}
	// RequiresProvenanceTag depends only on PathScope (environment.
	// RequiresProvenanceTag's own doc comment) -- a mockConfig-only
	// Environment must NOT cause a provenance tag to be set.
	if provenanceTag != nil {
		t.Errorf("provenance_tag = %q, want nil (mockConfig alone does not require a provenance tag)", *provenanceTag)
	}

	var pathScope []byte
	if err := rig.pool.QueryRow(ctx,
		`SELECT path_scope FROM environments WHERE id = $1`, environmentID,
	).Scan(&pathScope); err != nil {
		t.Fatalf("query persisted environment: %v", err)
	}
	if pathScope != nil {
		t.Errorf("path_scope = %s, want NULL (pathScope was absent)", pathScope)
	}
}

// TestCreateSession_NeitherPathScopeNorMockConfig_NoEnvironmentRow is a
// regression guard: a request carrying NEITHER pathScope nor mockConfig
// behaves exactly as before this batch -- no environments row at all.
func TestCreateSession_NeitherPathScopeNorMockConfig_NoEnvironmentRow(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{"spawnSource":"web","title":null,"prompt":null,"repos":[{"name":"narvi","url":"https://example.com","branch":null}],"modelId":null,"effort":null,"planMode":false}`)

	var got restdtos.Session
	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, &got, token)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var sessionID pgtype.UUID
	if err := sessionID.Scan(got.Id); err != nil {
		t.Fatalf("scan session id: %v", err)
	}

	var environmentID pgtype.UUID
	if err := rig.pool.QueryRow(ctx,
		`SELECT environment_id FROM sessions WHERE id = $1`, sessionID,
	).Scan(&environmentID); err != nil {
		t.Fatalf("query persisted session: %v", err)
	}
	if environmentID.Valid {
		t.Errorf("environment_id = %v, want NULL (neither pathScope nor mockConfig was supplied)", environmentID)
	}
}

// --- CreateSession: mockConfig.contractsPath validation (audit
// remediation: security-crosscutting lens) ---

// TestCreateSession_ContractsPathTraversal_Rejected proves a
// mockConfig.contractsPath containing a ".." segment -- environment.
// ValidateContractsPath's own ErrContractsPathTraversal case -- is
// rejected 400 BEFORE any Postgres write, mirroring
// TestCreateSession_InvalidPathScope_Rejected's own identical precedent
// exactly (assert zero session rows for the calling user).
func TestCreateSession_ContractsPathTraversal_Rejected(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{
		"spawnSource": "web",
		"title": null,
		"prompt": null,
		"repos": [{"name": "narvi", "url": "https://example.com", "branch": null}],
		"modelId": null,"effort":null,
		"planMode": false,
		"mockConfig": {"contractsPath": "contracts/../etc"}
	}`)

	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, nil, token)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
	if count := rig.sessionCountForUser(ctx, t, user.ID); count != 0 {
		t.Errorf("sessions for user = %d, want 0 (rejected before any Postgres write)", count)
	}
}

// TestCreateSession_ContractsPathQueryChar_Rejected proves a
// mockConfig.contractsPath containing a "?" -- the exact query-injection
// shape the githubapi adapter's own audit remediation independently
// guards against too -- is rejected 400 BEFORE any Postgres write.
func TestCreateSession_ContractsPathQueryChar_Rejected(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{
		"spawnSource": "web",
		"title": null,
		"prompt": null,
		"repos": [{"name": "narvi", "url": "https://example.com", "branch": null}],
		"modelId": null,"effort":null,
		"planMode": false,
		"mockConfig": {"contractsPath": "contracts/api?ref=attacker"}
	}`)

	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, nil, token)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
	if count := rig.sessionCountForUser(ctx, t, user.ID); count != 0 {
		t.Errorf("sessions for user = %d, want 0 (rejected before any Postgres write)", count)
	}
}

// TestCreateSession_ContractsPathFragmentChar_Rejected proves a
// mockConfig.contractsPath containing a "#" is ALSO rejected 400 BEFORE
// any Postgres write -- the fragment-truncation counterpart to the "?"
// case above.
func TestCreateSession_ContractsPathFragmentChar_Rejected(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	user, token := rig.createAuthenticatedUser(ctx, t)

	body := []byte(`{
		"spawnSource": "web",
		"title": null,
		"prompt": null,
		"repos": [{"name": "narvi", "url": "https://example.com", "branch": null}],
		"modelId": null,"effort":null,
		"planMode": false,
		"mockConfig": {"contractsPath": "contracts/api#evil"}
	}`)

	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, nil, token)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
	if count := rig.sessionCountForUser(ctx, t, user.ID); count != 0 {
		t.Errorf("sessions for user = %d, want 0 (rejected before any Postgres write)", count)
	}
}

func TestCreateSession_MalformedBody(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	status := rig.doJSON(t, http.MethodPost, "/api/sessions", []byte("not json"), nil, token)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

func TestCreateSession_OversizedBody(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	// A single "repos[0].name" value well beyond the 1 MiB request-body
	// cap -- json.Decoder will hit http.MaxBytesReader's own limit before
	// ever producing a complete value.
	huge := strings.Repeat("a", 2<<20) // 2 MiB
	body := []byte(fmt.Sprintf(`{"spawnSource":"web","title":null,"prompt":null,"repos":[{"name":%q,"url":"https://example.com","branch":null}],"modelId":null,"effort":null,"planMode":false}`, huge))

	status := rig.doJSON(t, http.MethodPost, "/api/sessions", body, nil, token)
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want %d", status, http.StatusRequestEntityTooLarge)
	}
}

// --- GetSession ---

func TestGetSession_HappyPath(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	_, token := rig.createAuthenticatedUser(ctx, t)

	var got restdtos.Session
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String(), nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.Id != session.ID.String() {
		t.Errorf("Id = %q, want %q", got.Id, session.ID.String())
	}
}

func TestGetSession_MalformedID(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	status := rig.doJSON(t, http.MethodGet, "/api/sessions/not-a-uuid", nil, nil, token)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

func TestGetSession_NotFound(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	status := rig.doJSON(t, http.MethodGet, "/api/sessions/11111111-1111-1111-1111-111111111111", nil, nil, token)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

// --- ListEvents ---

func TestListEvents_HappyPath(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	_, token := rig.createAuthenticatedUser(ctx, t)

	const total = 3
	for i := 0; i < total; i++ {
		if _, err := rig.events.Create(ctx, sqlcgen.CreateEventParams{
			SessionID: session.ID,
			Type:      "token",
			MessageID: fmt.Sprintf("msg-%d", i),
			Payload:   []byte(fmt.Sprintf(`{"n":%d}`, i)),
		}); err != nil {
			t.Fatalf("create event %d: %v", i, err)
		}
	}

	var got restdtos.EventsResponse
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/events?limit=2", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if len(got.Events) != 2 {
		t.Fatalf("len(Events) = %d, want 2", len(got.Events))
	}
	if got.NextCursor == nil {
		t.Fatal("NextCursor = nil, want non-nil (1 more event remains)")
	}

	var page2 restdtos.EventsResponse
	status = rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/events?cursor="+*got.NextCursor+"&limit=2", nil, &page2, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if len(page2.Events) != 1 {
		t.Fatalf("len(page2.Events) = %d, want 1", len(page2.Events))
	}
	if page2.NextCursor != nil {
		t.Errorf("page2.NextCursor = %v, want nil (exhausted)", *page2.NextCursor)
	}
}

func TestListEvents_SessionNotFound(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	status := rig.doJSON(t, http.MethodGet, "/api/sessions/11111111-1111-1111-1111-111111111111/events", nil, nil, token)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

func TestListEvents_MalformedSessionID(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	status := rig.doJSON(t, http.MethodGet, "/api/sessions/not-a-uuid/events", nil, nil, token)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

// --- ListArtifacts ---

func TestListArtifacts_HappyPath(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	_, token := rig.createAuthenticatedUser(ctx, t)

	if _, err := rig.pool.Exec(ctx,
		`INSERT INTO artifacts (session_id, type, url) VALUES ($1, 'pr', 'https://example.com/pr/1')`,
		session.ID,
	); err != nil {
		t.Fatalf("insert test artifact: %v", err)
	}

	var got restdtos.ArtifactsResponse
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/artifacts", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if len(got.Artifacts) != 1 {
		t.Fatalf("len(Artifacts) = %d, want 1", len(got.Artifacts))
	}
}

// TestListArtifacts_FailedUploadStatusAndFailureReason is a review-fix
// coverage addition (FIX I): artifactWireMap (artifacts.go) hand-builds
// its wire shape as a map[string]interface{} (additionalProperties:true,
// this schema's own design) rather than a generated, field-checked
// struct -- a typo'd or accidentally-dropped "status"/"failureReason" key
// would decode cleanly and silently show a failed upload as ready, with a
// 404ing download link. Seeds a genuinely 'failed' upload via the REAL
// production path (CreateUpload + MarkUploadFailedIfPending), never a
// hand-written INSERT, and asserts both fields land correctly on the REST
// list response. Extended here (the rail's own artifacts panel, §12.2 item
// 1, is the first real consumer of this endpoint) to also assert filename/
// sizeBytes/contentType land -- artifactWireMap dropped all three before
// this Step, even though sqlcgen.Artifact has carried them since migration
// 000060 and CreateUpload above already populates them on every row.
func TestListArtifacts_FailedUploadStatusAndFailureReason(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	_, token := rig.createAuthenticatedUser(ctx, t)

	var artifactID pgtype.UUID
	if err := artifactID.Scan(uuid.New().String()); err != nil {
		t.Fatalf("scan artifact id: %v", err)
	}
	blobKey := "sessions/" + session.ID.String() + "/uploads/" + artifactID.String()
	size := int64(10)
	contentType := "application/octet-stream"
	filename := "never-arrived.bin"
	if _, err := rig.artifacts.CreateUpload(ctx, sqlcgen.CreateUploadArtifactParams{
		ID:          artifactID,
		SessionID:   session.ID,
		Url:         "/api/sessions/" + session.ID.String() + "/uploads/" + artifactID.String() + "/content",
		BlobKey:     &blobKey,
		SizeBytes:   &size,
		ContentType: &contentType,
		Filename:    &filename,
	}); err != nil {
		t.Fatalf("create upload artifact: %v", err)
	}
	if _, err := rig.artifacts.MarkUploadFailedIfPending(ctx, artifactID, session.ID, sqlcgen.ArtifactFailureReasonVerificationFailed); err != nil {
		t.Fatalf("mark upload artifact failed: %v", err)
	}

	var got restdtos.ArtifactsResponse
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/artifacts", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if len(got.Artifacts) != 1 {
		t.Fatalf("len(Artifacts) = %d, want 1", len(got.Artifacts))
	}
	elem := got.Artifacts[0]
	if elem["status"] != "failed" {
		t.Errorf(`Artifacts[0]["status"] = %v, want "failed"`, elem["status"])
	}
	if elem["failureReason"] != "verification_failed" {
		t.Errorf(`Artifacts[0]["failureReason"] = %v, want "verification_failed"`, elem["failureReason"])
	}
	if elem["filename"] != filename {
		t.Errorf(`Artifacts[0]["filename"] = %v, want %q`, elem["filename"], filename)
	}
	if elem["contentType"] != contentType {
		t.Errorf(`Artifacts[0]["contentType"] = %v, want %q`, elem["contentType"], contentType)
	}
	// JSON numbers decode as float64 through the generated
	// additionalProperties:true map -- compared against size (int64) via
	// an explicit conversion rather than expecting Go's == to coerce it.
	if elem["sizeBytes"] != float64(size) {
		t.Errorf(`Artifacts[0]["sizeBytes"] = %v, want %v`, elem["sizeBytes"], size)
	}
}

func TestListArtifacts_SessionNotFound(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	status := rig.doJSON(t, http.MethodGet, "/api/sessions/11111111-1111-1111-1111-111111111111/artifacts", nil, nil, token)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

// --- ListPlans (audit finding M3, completeness) ---

// TestListPlans_HappyPath proves GET /api/sessions/:id/plans returns every
// plan VERSION for the session, in version order, with the right shape --
// a superseded v1 that was never decided, an approved v2 with decided_at/
// decided_by set, and a still-awaiting_approval v3 (the version a client
// would actually approve/reject) -- not just whichever version happens to
// be current.
func TestListPlans_HappyPath(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	owner, token := rig.createAuthenticatedUser(ctx, t)
	session := createSessionForUser(ctx, t, rig, owner.ID, nil)

	turn1, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create turn1: %v", err)
	}
	turn2, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create turn2: %v", err)
	}
	turn3, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("create turn3: %v", err)
	}

	const planModel = "claude-opus-4-8"
	// v1: superseded by v2's own "request changes" turn, before anyone ever
	// decided it.
	if _, err := rig.pool.Exec(ctx,
		`INSERT INTO plans (session_id, turn_id, version, status, plan_model_id) VALUES ($1, $2, 1, 'superseded', $3)`,
		session.ID, turn1.ID, planModel,
	); err != nil {
		t.Fatalf("seed v1 (superseded): %v", err)
	}
	// v2: approved, decided by the owner.
	if _, err := rig.pool.Exec(ctx,
		`INSERT INTO plans (session_id, turn_id, version, status, plan_model_id, decided_at, decided_by) VALUES ($1, $2, 2, 'approved', $3, now(), $4)`,
		session.ID, turn2.ID, planModel, owner.ID,
	); err != nil {
		t.Fatalf("seed v2 (approved): %v", err)
	}
	// v3: still awaiting_approval.
	if _, err := rig.pool.Exec(ctx,
		`INSERT INTO plans (session_id, turn_id, version, status, plan_model_id) VALUES ($1, $2, 3, 'awaiting_approval', $3)`,
		session.ID, turn3.ID, planModel,
	); err != nil {
		t.Fatalf("seed v3 (awaiting_approval): %v", err)
	}

	var got restdtos.ListPlansResponse
	status := rig.doJSON(t, http.MethodGet, "/api/sessions/"+session.ID.String()+"/plans", nil, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if len(got.Plans) != 3 {
		t.Fatalf("len(Plans) = %d, want 3", len(got.Plans))
	}

	if got.Plans[0].Version != 1 || got.Plans[1].Version != 2 || got.Plans[2].Version != 3 {
		t.Fatalf("versions = [%d, %d, %d], want [1, 2, 3] (ORDER BY version)",
			got.Plans[0].Version, got.Plans[1].Version, got.Plans[2].Version)
	}

	v1 := got.Plans[0]
	if v1.SessionId != session.ID.String() {
		t.Errorf("v1 SessionId = %q, want %q", v1.SessionId, session.ID.String())
	}
	if v1.Status != restdtos.PlanStatusSuperseded {
		t.Errorf("v1 Status = %q, want %q", v1.Status, restdtos.PlanStatusSuperseded)
	}
	if v1.DecidedAt != nil {
		t.Errorf("v1 DecidedAt = %v, want nil (never decided before being superseded)", v1.DecidedAt)
	}
	if v1.DecidedBy != nil {
		t.Errorf("v1 DecidedBy = %v, want nil", v1.DecidedBy)
	}
	if v1.PlanModelId == nil || *v1.PlanModelId != planModel {
		t.Errorf("v1 PlanModelId = %v, want %q", v1.PlanModelId, planModel)
	}

	v2 := got.Plans[1]
	if v2.Status != restdtos.PlanStatusApproved {
		t.Errorf("v2 Status = %q, want %q", v2.Status, restdtos.PlanStatusApproved)
	}
	if v2.DecidedAt == nil {
		t.Error("v2 DecidedAt = nil, want set")
	}
	if v2.DecidedBy == nil || *v2.DecidedBy != owner.ID.String() {
		t.Errorf("v2 DecidedBy = %v, want %q", v2.DecidedBy, owner.ID.String())
	}

	v3 := got.Plans[2]
	if v3.Status != restdtos.PlanStatusAwaitingApproval {
		t.Errorf("v3 Status = %q, want %q", v3.Status, restdtos.PlanStatusAwaitingApproval)
	}
	if v3.DecidedAt != nil {
		t.Errorf("v3 DecidedAt = %v, want nil (still awaiting approval)", v3.DecidedAt)
	}
}

// TestListPlans_SessionNotFound mirrors ListEvents/ListArtifacts's own
// identical precedent above -- every session-scoped GET endpoint 404s on a
// nonexistent session.
func TestListPlans_SessionNotFound(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	status := rig.doJSON(t, http.MethodGet, "/api/sessions/11111111-1111-1111-1111-111111111111/plans", nil, nil, token)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

// --- MintWSToken ---

func TestMintWSToken_HappyPath(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)
	user, token := rig.createAuthenticatedUser(ctx, t)

	before := time.Now()
	var got restdtos.WSTokenResponse
	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/ws-token", []byte{}, &got, token)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got.Token == "" {
		t.Fatal("Token is empty")
	}

	wantExpiry := before.Add(platform.DefaultTimeouts().WSTokenTTL)
	if got.ExpiresAt.Sub(wantExpiry).Abs() > time.Minute {
		t.Errorf("ExpiresAt = %v, want close to %v", got.ExpiresAt, wantExpiry)
	}

	// The stored row holds only the HASH, never the plaintext, is scoped
	// to this session, and -- the concrete proof §6.2's own
	// "ws_tokens.user_id always NULL" gap is closed -- carries the REAL
	// authenticated caller's id.
	stored, err := rig.wsTokens.GetByHash(ctx, platform.HashToken(got.Token))
	if err != nil {
		t.Fatalf("GetByHash: %v", err)
	}
	if stored.SessionID != session.ID {
		t.Errorf("stored.SessionID = %v, want %v", stored.SessionID, session.ID)
	}
	if !stored.UserID.Valid {
		t.Fatal("stored.UserID.Valid = false, want true (a real authenticated user)")
	}
	if stored.UserID != user.ID {
		t.Errorf("stored.UserID = %v, want %v", stored.UserID, user.ID)
	}
}

func TestMintWSToken_NoAuth(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := rig.createSession(ctx, t)

	status := rig.doJSON(t, http.MethodPost, "/api/sessions/"+session.ID.String()+"/ws-token", []byte{}, nil, "" /* no cookie */)
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

func TestMintWSToken_SessionNotFound(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	status := rig.doJSON(t, http.MethodPost, "/api/sessions/11111111-1111-1111-1111-111111111111/ws-token", []byte{}, nil, token)
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

func TestMintWSToken_MalformedSessionID(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	_, token := rig.createAuthenticatedUser(ctx, t)

	status := rig.doJSON(t, http.MethodPost, "/api/sessions/not-a-uuid/ws-token", []byte{}, nil, token)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
}
