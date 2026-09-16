//go:build integration

// Integration tests for §26.3's ("review: light/deep triage", §26.3) own
// wiring through this package's webhook ingress -- against a real Postgres
// instance, gated behind the "integration" build tag, sharing this
// package's own newTestPool/testWebhookSecret/testBotHandleIntegration/
// sign/issueCommentBodyWithCommenter/postWebhook*/createLinkedGitHubUser
// helpers (handler_integration_test.go, identity_integration_test.go, same
// package, same build tag).
//
// D1 (adversarial-review fix, "re-review depth floor applied at only 1 of
// 3 lanes"): coalesce.go's own REUSE branch (a second @mention or a label
// re-trigger on an ALREADY-TRACKED PR) used to feed the FRESH, unfloored
// triage decision straight through with no awareness of this PR's own
// prior depth at all -- TestGitHubIntegration_ReuseMention_DeepToLight_
// StaysFloorAtDeep below is the regression test for that lane.
//
// D6 (adversarial-review fix, "no end-to-end test that any wiring site
// persists the computed depth"): TestGitHubIntegration_SensitiveGlobDiff_
// RoutesDeep_PersistsThroughVerdict below is the primary-funnel end-to-end
// test -- webhook -> SessionCoalescer.CreateOrJoin -> real Postgres
// (asserting turns.review_depth), THEN a real POST /sessions/{id}/review/
// verdict call against the SAME turn (asserting review_verdicts.
// review_path) -- proving the chain D1/D2 depend on end to end, not just
// at either endpoint in isolation. Both verifiers in the adversarial
// review proved this gap by mutation: hardcoding coalesce.go's
// reviewDepthStr := "light" left the ENTIRE unit suite AND the full
// internal/adapters/inbound/github integration suite (156/156) green --
// ReviewPath appeared in zero test files repo-wide before this fix.
package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/adapters/inbound/wshub"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	appreviewtriage "github.com/narvidev/narvi/internal/app/reviewtriage"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// newTestRigWithReviewTriage mirrors newTestRig exactly (same registry/
// store construction), except the SessionCoalescer it builds ALSO carries
// real ReviewTriage deps (repo_settings + review_verdicts stores) --
// newTestRig's own default wiring leaves SessionCoalescer.ReviewTriage at
// its zero value, which is nil-store-safe (appreviewtriage.ComputeDecision
// degrades to the built-in default/no-prior-verdict signal, never a
// panic) but therefore never exercises a REAL repo_settings/review_verdicts
// read through this package's own webhook ingress at all -- exactly the
// gap D1/D6 close. fetcher (optional, mirrors newTestRigWithIntent
// Classifier's own single extra fixture parameter) lets a test control
// cfg.DiffFetcher directly -- typed as this file's own concrete
// *fakeReviewContextFetcher (handler_integration_test.go's own diffFetcher
// union interface is unexported, so an external _test package cannot name
// it directly as a parameter type; the concrete fake already satisfies it
// structurally, which is all cfg.DiffFetcher's assignment below needs). A
// nil fetcher leaves cfg.DiffFetcher unset, which is a legitimate,
// deliberately-exercised case too (D1's own test below: the fresh triage
// signal is the honest all-zero "nothing fetched" input, isolating the
// FLOOR as the one thing under test).
func newTestRigWithReviewTriage(t *testing.T, fetcher *fakeReviewContextFetcher) testRig {
	t.Helper()
	ctx := context.Background()
	pool := newTestPool(t)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	rig := testRig{
		pool:        pool,
		turns:       narvipg.NewTurnStore(pool),
		plans:       narvipg.NewPlanStore(pool),
		users:       narvipg.NewUserStore(pool),
		identities:  narvipg.NewIdentityStore(pool),
		linkNotices: narvipg.NewGitHubActorLinkNoticeStore(pool),
	}

	coalescer := &githubingress.SessionCoalescer{
		Pool:         pool,
		PRSessions:   narvipg.NewGitHubPRSessionStore(pool),
		Sessions:     narvipg.NewSessionStore(pool),
		Turns:        rig.turns,
		Environments: narvipg.NewEnvironmentStore(pool),
		Registry:     registry,
		AuditLog:     narvipg.NewAuditLogStore(pool),
		Plans:        rig.plans,
		Identities:   rig.identities,
		Users:        rig.users,
		Participants: narvipg.NewParticipantStore(pool),
		// ReviewTriage (§26.3) -- the ONE addition versus
		// newTestRig: real stores, so ComputeDecision's own repo_settings/
		// review_verdicts reads (and, via them, domainreviewtriage.Floor)
		// are genuinely exercised through this package's webhook ingress.
		ReviewTriage: appreviewtriage.Deps{
			RepoSettings:   narvipg.NewRepoSettingsStore(pool),
			ReviewVerdicts: narvipg.NewReviewVerdictStore(pool),
		},
	}
	deliveries := narvipg.NewWebhookDeliveryStore(pool)

	cfg := githubingress.Config{
		WebhookSecret: testWebhookSecret,
		BotHandle:     testBotHandleIntegration,
		LinkNotices:   rig.linkNotices,
		BotToken:      "test-bot-token",
		Timeouts:      platform.DefaultTimeouts(),
	}
	// fetcher assigned only when non-nil -- cfg.DiffFetcher is an
	// interface-typed field; assigning a nil *fakeReviewContextFetcher
	// directly would produce a NON-nil interface value wrapping a nil
	// concrete pointer (Go's classic "typed nil" trap), which would defeat
	// every "cfg.DiffFetcher != nil" nil-safety check in handler.go and
	// panic the first time it were actually dereferenced.
	if fetcher != nil {
		cfg.DiffFetcher = fetcher
	}

	handler := githubingress.NewHandler(coalescer, deliveries, cfg)

	mux := http.NewServeMux()
	mux.Handle("/webhooks/github", handler)
	rig.server = httptest.NewServer(mux)
	t.Cleanup(rig.server.Close)

	return rig
}

// insertPriorReviewVerdict seeds a review_verdicts row directly via SQL
// (session_id NULL -- that column is nullable, carried for traceability
// only, migrations/000067_review_verdicts.up.sql's own doc comment) --
// the fixture both tests below need: a PRIOR verdict already on record
// for (repoFullName, prNumber), with a specific review_path, that
// existed BEFORE the webhook delivery under test.
func insertPriorReviewVerdict(ctx context.Context, t *testing.T, rig testRig, repoFullName string, prNumber int32, headSHA, reviewPath string) {
	t.Helper()
	if _, err := rig.pool.Exec(ctx,
		`INSERT INTO review_verdicts (repo_full_name, pr_number, head_sha, risk_level, premise, blast_radius, files_changed, tests_coverage, docs_drift, proposed_shippable, shippable, review_path)
		 VALUES ($1, $2, $3, 'low', 'ok', '[]'::jsonb, 1, 'adequate', 'none', 'auto', 'auto', $4)`,
		repoFullName, prNumber, headSHA, reviewPath,
	); err != nil {
		t.Fatalf("seed prior review_verdicts row: %v", err)
	}
}

// TestGitHubIntegration_ReuseMention_DeepToLight_StaysFloorAtDeep is D1's
// own regression test for coalesce.go's REUSE branch (a second @mention
// on an already-tracked PR): before this fix, this branch fed the FRESH,
// unfloored triage decision straight through -- a light-looking second
// mention (no diffFetcher wired here, so the fresh signal is the honest
// all-zero "nothing fetched" input, isolating the floor specifically)
// on a PR that had already gone deep once would have produced
// turns.review_depth = "light", silently defeating §24's own "once deep,
// a PR stays deep" floor.
func TestGitHubIntegration_ReuseMention_DeepToLight_StaysFloorAtDeep(t *testing.T) {
	ctx := context.Background()
	rig := newTestRigWithReviewTriage(t, nil)

	const repoFullName = "acme/reuse-deep-to-light-repo"
	const cloneURL = "https://github.com/acme/reuse-deep-to-light-repo.git"
	const prNumber = 5150
	const commenterID = 80005150

	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)

	// First mention -- WINNER branch, creates the session/claim row (and,
	// since no prior verdict exists yet, its own turn routes light: a
	// brand-new PR with no diffFetcher wired has nothing deep-routing
	// about it).
	first := postWebhook(t, rig, issueCommentBodyWithCommenter(repoFullName, "reuse-deep-to-light-repo", cloneURL, prNumber, "first-mention", commenterID, "reuse-user"), "delivery-reuse-deep-1")
	if first != http.StatusOK {
		t.Fatalf("first (mention) delivery status = %d, want %d", first, http.StatusOK)
	}

	// A verdict is now on record for this PR, review_path "deep" --
	// exactly as if that first turn's own agent had posted a genuine deep
	// verdict (this test seeds it directly rather than round-tripping
	// through the real verdict-posting endpoint, a different package's
	// own concern already covered by internal/adapters/inbound/httpapi's
	// own reviewverdict_integration_test.go).
	insertPriorReviewVerdict(ctx, t, rig, repoFullName, prNumber, "sha-first-deep-verdict", "deep")

	// Second mention -- REUSE branch. Still no diffFetcher wired, so the
	// FRESH decision this firing computes is deterministically light.
	second := postWebhook(t, rig, issueCommentBodyWithCommenter(repoFullName, "reuse-deep-to-light-repo", cloneURL, prNumber, "second-mention", commenterID, "reuse-user"), "delivery-reuse-deep-2")
	if second != http.StatusOK {
		t.Fatalf("second (mention) delivery status = %d, want %d", second, http.StatusOK)
	}

	var sessionID string
	if err := rig.pool.QueryRow(ctx, `SELECT id::text FROM sessions WHERE spawn_source = 'github'`).Scan(&sessionID); err != nil {
		t.Fatalf("query session id: %v", err)
	}

	var reviewDepth *string
	var reviewDepthDecisionJSON []byte
	if err := rig.pool.QueryRow(ctx,
		`SELECT review_depth, review_depth_decision FROM turns WHERE session_id = $1 ORDER BY created_at DESC LIMIT 1`,
		sessionID,
	).Scan(&reviewDepth, &reviewDepthDecisionJSON); err != nil {
		t.Fatalf("query second turn review_depth: %v", err)
	}
	if reviewDepth == nil || *reviewDepth != "deep" {
		got := "<nil>"
		if reviewDepth != nil {
			got = *reviewDepth
		}
		t.Errorf("second turn's turns.review_depth = %s, want %q (a light-looking re-review must still floor at this PR's own prior deep depth)", got, "deep")
	}

	if reviewDepthDecisionJSON == nil {
		t.Fatal("second turn's turns.review_depth_decision is nil, want a recorded decision")
	}
	var record struct {
		Depth   string `json:"depth"`
		Floored bool   `json:"floored"`
	}
	if err := json.Unmarshal(reviewDepthDecisionJSON, &record); err != nil {
		t.Fatalf("unmarshal review_depth_decision: %v", err)
	}
	if record.Depth != "deep" {
		t.Errorf("review_depth_decision.depth = %q, want %q", record.Depth, "deep")
	}
	if !record.Floored {
		t.Error("review_depth_decision.floored = false, want true (the fresh decision was light; the floor is what actually decided)")
	}
}

// createSandboxWithToken creates a real sandbox row for sessionID with a
// known plaintext bearer token, hashed at rest exactly like production --
// mirrors internal/adapters/inbound/httpapi's own identically-named
// fixture (scmcredentials_integration_test.go), duplicated here rather
// than shared across package boundaries (this codebase's own established
// per-package testcontainers convention, this file's own top doc comment).
func createSandboxWithToken(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID, plaintextToken string) {
	t.Helper()
	sandboxes := narvipg.NewSandboxStore(rig.pool)
	if _, err := sandboxes.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	hash := wshub.HashSandboxToken(plaintextToken)
	if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET token_hash = $2 WHERE session_id = $1`, sessionID, hash); err != nil {
		t.Fatalf("set token_hash: %v", err)
	}
}

// markTurnProcessing flips a turn to 'processing' AND stamps its own
// dispatched_message_id, directly via SQL -- standing in for the real
// dispatch machinery (a different package's own concern, internal/app/
// sessionactor), the SAME shortcut internal/adapters/inbound/httpapi's own
// reviewverdict_integration_test.go takes (creating turns directly at
// sqlcgen.TurnStatusProcessing) so this test can call the real POST
// /sessions/{id}/review/verdict handler against a turn
// PostReviewVerdict's own turns.GetByDispatchedMessageID will actually
// find (finding F3, §21.1's amendment: that lookup is now keyed by
// dispatched_message_id, never by session-wide "current" status alone --
// see reviewverdict.go's own doc comment). dispatchMessageID is the SAME
// value the caller must then present as this turn's own
// X-Sandbox-Dispatch-Message-Id header.
func markTurnProcessing(ctx context.Context, t *testing.T, rig testRig, turnID pgtype.UUID, dispatchMessageID string) {
	t.Helper()
	if _, err := rig.pool.Exec(ctx, `UPDATE turns SET status = 'processing', dispatched_message_id = $2 WHERE id = $1`, turnID, dispatchMessageID); err != nil {
		t.Fatalf("mark turn processing: %v", err)
	}
}

// validDeepVerdictRequestJSON is a deep-path-valid verdict body -- D2's own
// fix means archDecisions/stackRisks/unverifiedLimits are REQUIRED, not
// merely requested, on a turn whose own review_depth is "deep" (this
// file's own end-to-end test posts this against exactly such a turn).
// factCheck/factCheckKilled (§26.6) are REQUIRED unconditionally,
// both paths; counterReview (§26.4) is REQUIRED on this deep-path
// turn specifically.
const validDeepVerdictRequestJSON = `{
	"riskLevel": "high",
	"premise": "ok",
	"blastRadius": ["migrations"],
	"filesChanged": 3,
	"testsCoverage": "adequate",
	"docsDrift": "none",
	"proposedShippable": "needs_human",
	"summary": "Adds a new migration touching a sensitive path.",
	"digest": {
		"summary": "Adds a new database migration.",
		"archDecisions": [
			{"decision": "Added a new column", "rejectedAlternative": "A separate table", "conventionConformance": "Matches this repo's own migration conventions"}
		],
		"stackRisks": "A schema migration -- requires a deploy window.",
		"unverifiedLimits": "Did not test against a production-sized table.",
		"descriptionAdequacy": "ok",
		"adequacyExplanation": "The PR body accurately describes the migration."
	},
	"factCheck": "done",
	"factCheckKilled": 0,
	"counterReview": "done"
}`

// TestGitHubIntegration_SensitiveGlobDiff_RoutesDeep_PersistsThroughVerdict
// is D6's own primary end-to-end test: a PR whose diff touches a
// migrations/ path (the fixed sensitive-glob set, internal/domain/
// reviewtriage.classifySensitivePaths) routes deep -- webhook ->
// SessionCoalescer.CreateOrJoin -> real Postgres, asserting
// turns.review_depth = 'deep' -- THEN a real verdict is posted against
// that SAME turn (POST /sessions/{id}/review/verdict, the real
// httpapi.PostReviewVerdict handler, not a shortcut), asserting
// review_verdicts.review_path = 'deep'. This is the ONE test proving the
// full chain D1/D2 both depend on: the depth computed at turn-creation
// time is the SAME depth later read back and persisted at verdict-post
// time (reviewverdict.go's own processingTurn.ReviewDepth -> input.
// ReviewDepth -> review_verdicts.review_path), not two independently
// computed values that could silently disagree.
func TestGitHubIntegration_SensitiveGlobDiff_RoutesDeep_PersistsThroughVerdict(t *testing.T) {
	fetcher := &fakeReviewContextFetcher{
		pr:   githubapi.PullRequest{HeadSHA: "sha-sensitive-migration-head", BaseRef: "main"},
		diff: "diff --git a/migrations/000099_add_column.up.sql b/migrations/000099_add_column.up.sql\nindex 111..222 100644\n--- a/migrations/000099_add_column.up.sql\n+++ b/migrations/000099_add_column.up.sql\n@@ -1 +1,2 @@\n ALTER TABLE widgets\n+ADD COLUMN sku TEXT;\n",
	}
	rig := newTestRigWithReviewTriage(t, fetcher)
	ctx := context.Background()

	const repoFullName = "acme/sensitive-migration-repo"
	const cloneURL = "https://github.com/acme/sensitive-migration-repo.git"
	const prNumber = 6161
	const commenterID = 80006161

	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)

	status := postWebhook(t, rig, issueCommentBodyWithCommenter(repoFullName, "sensitive-migration-repo", cloneURL, prNumber, "migration-review", commenterID, "migration-user"), "delivery-sensitive-migration-1")
	if status != http.StatusOK {
		t.Fatalf("webhook delivery status = %d, want %d", status, http.StatusOK)
	}

	var sessionIDText string
	if err := rig.pool.QueryRow(ctx, `SELECT id::text FROM sessions WHERE spawn_source = 'github'`).Scan(&sessionIDText); err != nil {
		t.Fatalf("query session id: %v", err)
	}
	var sessionID pgtype.UUID
	if err := sessionID.Scan(sessionIDText); err != nil {
		t.Fatalf("scan session id: %v", err)
	}

	var turnIDText string
	var reviewDepth *string
	var prompt string
	if err := rig.pool.QueryRow(ctx,
		`SELECT id::text, review_depth, prompt FROM turns WHERE session_id = $1 ORDER BY created_at DESC LIMIT 1`,
		sessionIDText,
	).Scan(&turnIDText, &reviewDepth, &prompt); err != nil {
		t.Fatalf("query turn: %v", err)
	}
	if reviewDepth == nil || *reviewDepth != "deep" {
		got := "<nil>"
		if reviewDepth != nil {
			got = *reviewDepth
		}
		t.Fatalf("turns.review_depth = %s, want %q (a diff touching migrations/ must route deep via the fixed sensitive-glob rule)", got, "deep")
	}
	// D2's own fix, proven at the prompt-text level too: the deep-path
	// digest fields must read REQUIRED, not merely requested, in the
	// text the agent actually receives for THIS turn.
	if !strings.Contains(prompt, "REQUIRED on this deep-path review") {
		t.Errorf("turn prompt does not contain deep-path REQUIRED wording for archDecisions/stackRisks/unverifiedLimits -- prompt = %q", prompt)
	}

	var turnID pgtype.UUID
	if err := turnID.Scan(turnIDText); err != nil {
		t.Fatalf("scan turn id: %v", err)
	}

	// Post a real verdict against this SAME turn, through the real
	// httpapi.PostReviewVerdict handler -- proving the OTHER half of the
	// chain: the depth persisted above is read back and forwarded onto
	// review_verdicts.review_path, never silently dropped or recomputed.
	createSandboxWithToken(ctx, t, rig, sessionID, "sandbox-bearer-token")
	const dispatchMessageID = "sensitive-migration-dispatch-message-id"
	markTurnProcessing(ctx, t, rig, turnID, dispatchMessageID)

	verdictMux := chi.NewRouter()
	verdictMux.Post("/sessions/{sessionID}/review/verdict", httpapi.PostReviewVerdict(
		rig.pool,
		narvipg.NewSandboxStore(rig.pool),
		narvipg.NewSessionStore(rig.pool),
		narvipg.NewGitHubPRSessionStore(rig.pool),
		narvipg.NewRepoSettingsStore(rig.pool),
		narvipg.NewReviewFindingStore(rig.pool),
		narvipg.NewSentinelFixStore(rig.pool),
		narvipg.NewOutboxStore(rig.pool, false),
		narvipg.NewReviewVerdictStore(rig.pool),
		rig.turns,
		narvipg.NewEventStore(rig.pool),
		testBotHandleIntegration,
		"test-bot-token",
		nil,
		nil,
		platform.DefaultTimeouts(),
		false,
	))
	verdictServer := httptest.NewServer(verdictMux)
	t.Cleanup(verdictServer.Close)

	req, err := http.NewRequest(http.MethodPost, verdictServer.URL+"/sessions/"+sessionID.String()+"/review/verdict", strings.NewReader(validDeepVerdictRequestJSON))
	if err != nil {
		t.Fatalf("build verdict request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sandbox-bearer-token")
	req.Header.Set("X-Sandbox-Gen", "1")
	req.Header.Set("X-Sandbox-Dispatch-Message-Id", dispatchMessageID)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post verdict: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post verdict status = %d, want %d (D2's own fix: this deep-path-valid body must be accepted)", resp.StatusCode, http.StatusCreated)
	}

	var verdictReviewPath *string
	if err := rig.pool.QueryRow(ctx,
		`SELECT review_path FROM review_verdicts WHERE repo_full_name = $1 AND pr_number = $2 ORDER BY created_at DESC LIMIT 1`,
		repoFullName, prNumber,
	).Scan(&verdictReviewPath); err != nil {
		t.Fatalf("query review_verdicts.review_path: %v", err)
	}
	if verdictReviewPath == nil || *verdictReviewPath != "deep" {
		got := "<nil>"
		if verdictReviewPath != nil {
			got = *verdictReviewPath
		}
		t.Errorf("review_verdicts.review_path = %s, want %q (the turn's own persisted review_depth must be forwarded, verbatim, onto the posted verdict)", got, "deep")
	}
}
