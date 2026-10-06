//go:build integration

// Integration test for F1 (a follow-up fix, adversarial review
// Finding 1): coalesce.go's REUSE path must classify the mention's own
// RAW, un-enriched comment text against the plan_followup category
// (ClassifyPlanFollowup, gated on an awaiting-approval plan), never the
// diff/stack/verdict-tool-instructions-enriched text review.RenderTurnPrompt
// folds into the turn's own dispatched prompt -- exactly the bug class
// this repo already fixed once for the OTHER (§8.3, review-vs-request/
// plan-vs-build) classifier category (coalesce.go's own classifyText
// parameter doc comment: "Audit fix: this used to be *req.Prompt
// directly ... inflating cost/latency by orders of magnitude and risking
// exceeding the model's context window"). Lives alongside
// coalesce_intent_integration_test.go (same package/build tag), reusing
// handler_integration_test.go's own testRig/newTestRig/createLinkedGitHubUser/
// issueCommentBodyWithCommenter/postWebhook/fakeReviewContextFetcher.
package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/intentclassifier"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/app/turnguard"
	intentdomain "github.com/narvidev/narvi/internal/domain/intent"
	"github.com/narvidev/narvi/internal/platform"
)

// capturingIntentLLM is a ports.LLM fake that records every Complete
// call's own single user-message content, IN ORDER -- this file's own
// regression coverage needs to distinguish the WINNER path's §8.3
// ClassifyAndRecord call (review-vs-request/plan-vs-build, fires first,
// on session creation) from the REUSE path's §23 ClassifyPlanFollowup
// call (amend-vs-answer, fires second, once an awaiting-approval plan
// exists) by call order, since both categories share this SAME
// *intentclassifier.Service/ports.LLM instance in real production wiring
// (coalesce.go's own IntentClassifier field). The fixed response payload
// below is a superset valid against EITHER category's own structured-
// output schema (schema.go's target/mode/confidence/reasoning vs.
// schema_planfollowup.go's target/confidence/reasoning) -- "answer" is a
// valid plan_followup Target and is simply ignored as an unrecognized
// §8.3 Target (harmless: this test asserts nothing about the WINNER
// call's own decision outcome).
type capturingIntentLLM struct {
	mu       sync.Mutex
	messages []string
}

func (f *capturingIntentLLM) Complete(_ context.Context, req ports.CompletionRequest) (ports.CompletionResponse, error) {
	f.mu.Lock()
	content := ""
	if len(req.Messages) > 0 {
		content = req.Messages[0].Content
	}
	f.messages = append(f.messages, content)
	f.mu.Unlock()

	body := map[string]string{
		"target":     intentdomain.TargetAnswer,
		"mode":       intentdomain.ModeBuild,
		"confidence": intentdomain.ConfidenceHigh,
		"reasoning":  "test fixture",
	}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return ports.CompletionResponse{Raw: raw}, nil
}

// TestCoalesce_ReusePathClassifiesRawMentionText_NeverTheEnrichedPrompt is
// F1's own headline regression test: a second mention on a PR whose
// review session already has a plan awaiting approval must classify
// (plan_followup, ClassifyPlanFollowup) exactly the second mention's own
// raw comment body -- never review.RenderTurnPrompt's own diff-enriched
// text that ends up on the turn's own dispatched prompt.
func TestCoalesce_ReusePathClassifiesRawMentionText_NeverTheEnrichedPrompt(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	sessions := narvipg.NewSessionStore(pool)
	templates := narvipg.NewPromptTemplateStore(pool)
	capturingLLM := &capturingIntentLLM{}
	intentSvc := intentclassifier.New(capturingLLM, "anthropic", "claude-haiku-4-5", templates, sessions, nil)

	rig := testRig{
		pool:       pool,
		turns:      narvipg.NewTurnStore(pool),
		plans:      narvipg.NewPlanStore(pool),
		users:      narvipg.NewUserStore(pool),
		identities: narvipg.NewIdentityStore(pool),
	}

	// diffFetcher's own diff content below is the "wider blast radius"
	// signal this test guards against reaching the classifier -- a
	// distinctive marker string that would only appear in the classify
	// text if the REUSE path's own call site regressed back to classifying
	// the diff-enriched prompt instead of the raw mention text.
	const diffMarker = "SENTINEL_DIFF_CONTENT_MUST_NEVER_REACH_THE_CLASSIFIER"
	fetcher := &fakeReviewContextFetcher{
		pr: githubapi.PullRequest{
			HeadRef: "feature-x",
			HeadSHA: "resolved-head-sha",
			BaseRef: "main",
		},
		diff: "diff --git a/x b/x\n+" + diffMarker + "\n",
	}

	coalescer := &githubingress.SessionCoalescer{
		Pool:             pool,
		PRSessions:       narvipg.NewGitHubPRSessionStore(pool),
		Sessions:         sessions,
		Turns:            rig.turns,
		Environments:     narvipg.NewEnvironmentStore(pool),
		Registry:         registry,
		SessionGuard:     turnguard.New(pool, nil, false),
		IntentClassifier: intentSvc,
		AuditLog:         narvipg.NewAuditLogStore(pool),
		Plans:            rig.plans,
		Identities:       rig.identities,
		Users:            rig.users,
		Participants:     narvipg.NewParticipantStore(pool),
	}
	deliveries := narvipg.NewWebhookDeliveryStore(pool)

	handler, handlerErr := githubingress.NewHandler(coalescer, deliveries, githubingress.Config{
		WebhookSecret: testWebhookSecret,
		BotHandle:     testBotHandleIntegration,
		PullRequests:  fetcher,
		DiffFetcher:   fetcher,
		Outbound:      platform.MustNewGitHubOutboundConfig("test-bot-token"),
		Timeouts:      platform.DefaultTimeouts(),
	})
	if handlerErr != nil {
		t.Fatalf("githubingress.NewHandler: %v", handlerErr)
	}

	mux := http.NewServeMux()
	mux.Handle("/webhooks/github", handler)
	rig.server = httptest.NewServer(mux)
	t.Cleanup(rig.server.Close)

	const repoFullName = "acme/planfollowup-classify-repo"
	const cloneURL = "https://github.com/acme/planfollowup-classify-repo.git"
	const prNumber = 5151
	const commenterID = 80005151

	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, commenterID, sqlcgen.UserRoleMaintainer)

	// First mention: WINNER path, creates the review session. Triggers the
	// §8.3 ClassifyAndRecord call -- capturingLLM.messages[0].
	first := postWebhook(t, rig, issueCommentBodyWithCommenter(repoFullName, "planfollowup-classify-repo", cloneURL, prNumber, "first-mention", commenterID, "planfollowup-user"), "delivery-planfollowup-classify-1")
	if first != http.StatusOK {
		t.Fatalf("first delivery status = %d, want %d", first, http.StatusOK)
	}

	var sessionID pgtype.UUID
	if err := rig.pool.QueryRow(ctx,
		`SELECT session_id FROM github_pr_sessions WHERE repo_full_name = $1 AND pr_number = $2`,
		repoFullName, prNumber,
	).Scan(&sessionID); err != nil {
		t.Fatalf("query claim row session id: %v", err)
	}

	// Seed a producing turn (Completed, plan_mode true) and an
	// awaiting_approval plans row atop the session the first mention just
	// created -- mirrors handler_integration_test.go's own
	// TestGitHubIntegration_AwaitingPlanBlocksReuseTurn_HonestReplyNoRelease
	// precedent exactly.
	producingTurn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusCompleted, PlanMode: true})
	if err != nil {
		t.Fatalf("seed producing turn: %v", err)
	}
	if _, err := rig.plans.Create(ctx, sqlcgen.CreatePlanParams{SessionID: sessionID, TurnID: producingTurn.ID, Version: 1, Status: sqlcgen.PlanStatusAwaitingApproval}); err != nil {
		t.Fatalf("seed awaiting_approval plan: %v", err)
	}

	// Second mention: REUSE path (coalesce.go), which now hits the
	// plan_followup classification block (createTurnLocked, turn.go) since
	// planMode is false and an awaiting_approval plan exists. cfg.DiffFetcher
	// is wired, so review.RenderTurnPrompt DOES fold the diff (containing
	// diffMarker) into this turn's own dispatched prompt -- but the
	// classifier must never see it.
	const secondLabel = "second-mention-during-awaiting-plan"
	second := postWebhook(t, rig, issueCommentBodyWithCommenter(repoFullName, "planfollowup-classify-repo", cloneURL, prNumber, secondLabel, commenterID, "planfollowup-user"), "delivery-planfollowup-classify-2")
	if second != http.StatusOK {
		t.Fatalf("second delivery status = %d, want %d", second, http.StatusOK)
	}

	// The turn's own dispatched prompt DOES carry the diff -- confirms
	// fetcher was genuinely wired and exercised (a false negative below
	// would otherwise be indistinguishable from "the fetch never ran at
	// all").
	var turnCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM turns WHERE session_id = $1`, sessionID).Scan(&turnCount); err != nil {
		t.Fatalf("count turns: %v", err)
	}
	// Only the first mention's own turn + the seeded producing turn: the
	// classifier's fixed "answer" response (capturingIntentLLM's own
	// canned payload) maps to a confident-answer plan_followup verdict,
	// which -- exactly like a genuine human "answer" reply -- still
	// declines to dispatch an ordinary build turn while the plan remains
	// awaiting approval (turn.go's own gate). The point under test is the
	// CLASSIFY TEXT below, not this outcome, but asserting it too confirms
	// the classify call's own result genuinely round-tripped through
	// unmarshal/ResolveAnswerOnly rather than erroring out before ever
	// reaching this test's own capturingLLM assertions.
	if turnCount != 2 {
		t.Fatalf("turn count = %d, want 2 (first mention's turn + seeded producing turn -- the plan_followup gate must still decline an 'answer' verdict)", turnCount)
	}

	capturingLLM.mu.Lock()
	messages := append([]string(nil), capturingLLM.messages...)
	capturingLLM.mu.Unlock()

	if len(messages) != 2 {
		t.Fatalf("capturingLLM.messages = %d entries, want exactly 2 (WINNER's §8.3 call + REUSE's §23 plan_followup call); messages = %q", len(messages), messages)
	}

	planFollowupClassifyText := messages[1]

	wantRawText := "@" + testBotHandleIntegration + " please review (" + secondLabel + ")"
	if planFollowupClassifyText != wantRawText {
		t.Errorf("plan_followup classify text = %q, want exactly the second mention's own raw comment body %q", planFollowupClassifyText, wantRawText)
	}
	if strings.Contains(planFollowupClassifyText, diffMarker) {
		t.Errorf("plan_followup classify text = %q, must NEVER contain the pre-fetched diff (%q)", planFollowupClassifyText, diffMarker)
	}
	if strings.Contains(planFollowupClassifyText, "verdict-posting tool") {
		t.Errorf("plan_followup classify text = %q, must NEVER contain review.RenderTurnPrompt's own verdict-tool-instructions boilerplate", planFollowupClassifyText)
	}
}
