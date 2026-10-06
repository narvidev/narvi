//go:build integration

// Integration test for §8.3's ("intent classifier", §8.3/§18) own
// GitHub ingress wiring, against a real Postgres instance -- gated behind
// the "integration" build tag, reusing newTestPool/testWebhookSecret/
// testBotHandleIntegration/sign/issueCommentBody/postWebhook/testRig from
// handler_integration_test.go (same package, same build tag). Proves a
// real session gets a real IntentDecisionRecord persisted end to end:
// webhook -> SessionCoalescer.CreateOrJoin -> real Postgres write-once
// UPDATE (migrations/000033_intent_classifier.up.sql's own seeded
// template) -- only the outbound LLM call itself is faked (a
// fakeIntentLLM canned response), mirroring this codebase's own existing
// convention of stubbing the one genuinely external network dependency
// in an otherwise fully-real-Postgres integration test.
package github_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	githubingress "github.com/narvidev/narvi/internal/adapters/inbound/github"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/intentclassifier"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/app/turnguard"
	intentdomain "github.com/narvidev/narvi/internal/domain/intent"
	"github.com/narvidev/narvi/internal/platform"
)

// intentTestCommenterID is the single already-linked commenter every test
// in this file mentions the bot as -- batch fix/deny-unlinked-github-
// actors means an unresolved commenter's mention is now denied outright
// (never bot-attributed), so this file's own coalescer (below) must wire
// real Identities/Users/Participants stores and every mention body must
// carry a genuinely linked comment.user, exactly like every other test in
// this package now does (handler_integration_test.go's own
// createLinkedGitHubUser).
const intentTestCommenterID = 80009000

// fakeIntentLLM implements ports.LLM with a fixed, canned structured-
// output response -- this test's only stub, standing in for a real
// outbound Anthropic call so the test stays hermetic and fast while
// exercising every other piece (template assembly, Postgres persistence,
// the write-once guarded UPDATE) for real. target is configurable per
// test so both the "classifier agrees with the deterministic review
// signal" and "classifier disagrees" corroboration paths (§18.2,
// intentdomain.CorroborateTarget) can be exercised against a real
// coalesce.go call, not just unit-tested in isolation.
type fakeIntentLLM struct {
	target string
}

func (f fakeIntentLLM) Complete(_ context.Context, _ ports.CompletionRequest) (ports.CompletionResponse, error) {
	body := map[string]string{
		"target":     f.target,
		"mode":       intentdomain.ModeBuild,
		"confidence": intentdomain.ConfidenceHigh,
		"reasoning":  "the comment directly asks for a review",
	}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return ports.CompletionResponse{Raw: raw}, nil
}

// newTestRigWithIntentClassifier mirrors newTestRig exactly (same
// registry/store construction), except the SessionCoalescer it builds
// also carries a real intentclassifier.Service (backed by the real
// Postgres-backed prompt template store + session store, with only
// fakeIntentLLM standing in for the outbound Anthropic call).
// classifierTarget is the Target fakeIntentLLM's canned response reports --
// callers pass intentdomain.TargetReview to exercise the "agrees with the
// deterministic review signal" path, or intentdomain.TargetRequest to
// exercise the "disagrees" path (coalesce.go's own DeterministicTarget for
// every GitHub mention is always intentdomain.TargetReview -- see
// coalesce.go's own doc comment on that call).
func newTestRigWithIntentClassifier(t *testing.T, classifierTarget string) testRig {
	t.Helper()
	ctx := context.Background()
	pool := newTestPool(t)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	sessions := narvipg.NewSessionStore(pool)
	templates := narvipg.NewPromptTemplateStore(pool)
	intentSvc := intentclassifier.New(fakeIntentLLM{target: classifierTarget}, "anthropic", "claude-haiku-4-5", templates, sessions, nil)

	rig := testRig{
		pool:       pool,
		turns:      narvipg.NewTurnStore(pool),
		users:      narvipg.NewUserStore(pool),
		identities: narvipg.NewIdentityStore(pool),
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
		// Identities/Users/Participants (batch fix/deny-unlinked-github-
		// actors): required now that every mention body in this file
		// carries a genuinely linked comment.user (intentTestCommenterID) --
		// an unresolved commenter's mention would otherwise be denied
		// outright before ever reaching the intent classifier this file
		// exists to test.
		Identities:   rig.identities,
		Users:        rig.users,
		Participants: narvipg.NewParticipantStore(pool),
	}
	deliveries := narvipg.NewWebhookDeliveryStore(pool)

	handler, handlerErr := githubingress.NewHandler(coalescer, deliveries, githubingress.Config{
		WebhookSecret: testWebhookSecret,
		BotHandle:     testBotHandleIntegration,
		Outbound:      platform.MustNewGitHubOutboundConfig("test-bot-token"),
	})
	if handlerErr != nil {
		t.Fatalf("githubingress.NewHandler: %v", handlerErr)
	}

	mux := http.NewServeMux()
	mux.Handle("/webhooks/github", handler)
	rig.server = httptest.NewServer(mux)
	t.Cleanup(rig.server.Close)

	return rig
}

// pollIntentDecision polls the most recently created "github" session's
// intent_decision column until it's non-NULL (or t fails on timeout),
// then unmarshals it -- the shared polling body both corroboration-path
// tests below use, keeping their own bodies focused on the assertions
// that actually differ between them.
func pollIntentDecision(t *testing.T, ctx context.Context, rig testRig) intentdomain.IntentDecisionRecord {
	t.Helper()

	// Poll briefly: CreateOrJoin's own classify+record step runs
	// synchronously inside the request handler in this wiring, but poll
	// anyway to keep this test robust against any future async change.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var decisionJSON []byte
		err := rig.pool.QueryRow(ctx,
			`SELECT intent_decision FROM sessions WHERE spawn_source = 'github' ORDER BY created_at DESC LIMIT 1`,
		).Scan(&decisionJSON)
		if err != nil {
			t.Fatalf("query session: %v", err)
		}

		if decisionJSON != nil {
			var rec intentdomain.IntentDecisionRecord
			if err := json.Unmarshal(decisionJSON, &rec); err != nil {
				t.Fatalf("unmarshal intent_decision: %v", err)
			}
			return rec
		}

		if time.Now().After(deadline) {
			t.Fatal("session row's intent_decision stayed NULL within the deadline")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestCreateOrJoin_RecordsIntentDecision covers the AGREEING corroboration
// path: coalesce.go's own DeterministicTarget for every GitHub mention is
// always intentdomain.TargetReview (this webhook path is only ever
// reachable for a genuine PR-scoped mention -- see coalesce.go's own doc
// comment on that call), and here the stubbed classifier also reports
// "review" -- both signals agree, so CorroborateTarget (§18.2) leaves
// Confidence exactly as the classifier reported it (untouched, "high").
func TestCreateOrJoin_RecordsIntentDecision(t *testing.T) {
	ctx := context.Background()
	rig := newTestRigWithIntentClassifier(t, intentdomain.TargetReview)
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, intentTestCommenterID, sqlcgen.UserRoleMaintainer)

	body := issueCommentBodyWithCommenter("owner/repo", "repo", "https://github.com/owner/repo.git", 42, "intent-decision-test", intentTestCommenterID, "intent-test-user")

	status := postWebhook(t, rig, body, "delivery-intent-decision-1")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	rec := pollIntentDecision(t, ctx, rig)

	if rec.Surface != "github" {
		t.Errorf("Surface = %q, want %q", rec.Surface, "github")
	}
	if rec.Source != intentdomain.RecordSourceClassifier {
		t.Errorf("Source = %q, want %q", rec.Source, intentdomain.RecordSourceClassifier)
	}
	if rec.Target != intentdomain.TargetReview {
		t.Errorf("Target = %q, want %q", rec.Target, intentdomain.TargetReview)
	}
	if rec.Mode != intentdomain.ModeBuild {
		t.Errorf("Mode = %q, want %q", rec.Mode, intentdomain.ModeBuild)
	}
	if rec.Confidence == nil || *rec.Confidence != intentdomain.ConfidenceHigh {
		t.Errorf("Confidence = %v, want %q (both signals agree -- confidence untouched)", rec.Confidence, intentdomain.ConfidenceHigh)
	}
	if rec.DecidedAtStage != intentdomain.DecidedAtStageCreate {
		t.Errorf("DecidedAtStage = %q, want %q", rec.DecidedAtStage, intentdomain.DecidedAtStageCreate)
	}
}

// TestCreateOrJoin_RecordsIntentDecision_DisagreesWithDeterministicSignal
// covers the DISAGREEING corroboration path (§18.2, intentdomain.
// CorroborateTarget): the stubbed classifier reports "request" even
// though this webhook path's own deterministic signal is always "review"
// (coalesce.go's own doc comment). On disagreement, the deterministic,
// verifiable signal wins the recorded Target ("review", not the
// classifier's raw "request"), and Confidence is forced to "low" rather
// than trusting the classifier's own (higher) reported confidence --
// exactly the "ask for clarification rather than guessing" posture §18.2
// requires for an irreversible action.
func TestCreateOrJoin_RecordsIntentDecision_DisagreesWithDeterministicSignal(t *testing.T) {
	ctx := context.Background()
	rig := newTestRigWithIntentClassifier(t, intentdomain.TargetRequest)
	createLinkedGitHubUser(ctx, t, rig.users, rig.identities, intentTestCommenterID, sqlcgen.UserRoleMaintainer)

	body := issueCommentBodyWithCommenter("owner/repo", "repo", "https://github.com/owner/repo.git", 43, "intent-decision-disagree-test", intentTestCommenterID, "intent-test-user")

	status := postWebhook(t, rig, body, "delivery-intent-decision-2")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}

	rec := pollIntentDecision(t, ctx, rig)

	if rec.Target != intentdomain.TargetReview {
		t.Errorf("Target = %q, want %q (deterministic signal wins on disagreement)", rec.Target, intentdomain.TargetReview)
	}
	if rec.Confidence == nil || *rec.Confidence != intentdomain.ConfidenceLow {
		t.Errorf("Confidence = %v, want %q (forced low on corroboration disagreement)", rec.Confidence, intentdomain.ConfidenceLow)
	}
}
