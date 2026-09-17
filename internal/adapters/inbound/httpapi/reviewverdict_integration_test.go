//go:build integration

// Integration tests for §8.2's ("server-side verdict", §8.2/§5.2/§21.2)
// own verdict-posting tool (reviewverdict.go), against a real Postgres
// instance -- gated behind the "integration" build tag, sharing this
// package's own testRig (httpapi_integration_test.go).
package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
)

// validVerdictRequestJSON is the happy-path request body every test in
// this file starts from, mutating only the ONE field a given test cares
// about -- RiskLevel low, Premise ok, empty BlastRadius (legal), adequate
// coverage, no docs drift, the model proposing "auto" (irrelevant to what
// the server actually computes), a real, non-blank Summary, and (§26.2)
// an "ok" descriptionAdequacy with its own required explanation.
func validVerdictRequestJSON() string {
	return `{
		"riskLevel": "low",
		"premise": "ok",
		"blastRadius": [],
		"filesChanged": 3,
		"testsCoverage": "adequate",
		"docsDrift": "none",
		"proposedShippable": "auto",
		"summary": "Looks good overall, one minor nit below.",
		"digest": {
			"summary": "Adds a retry helper around the flaky upstream call and swaps every existing call site onto it.",
			"descriptionAdequacy": "ok",
			"adequacyExplanation": "The PR body accurately describes the retry helper this diff adds."
		},
		"factCheck": "done",
		"factCheckKilled": 0
	}`
}

// driftFiringVerdictRequestJSON is validVerdictRequestJSON with filesChanged
// overridden to 25 -- deliberately chosen, paired with a seeded
// server-computed count of 15 (TestPostReviewVerdict_
// FilesChangedDriftCanary_FiresOnDivergence_NeverAffectsVerdict), so the
// fired/not-fired OUTCOME actually depends on which of the two values
// FilesChangedDrifted's own two parameters receive: delta is 10 either
// way (|25-15| == |15-25|), but the RATIO does not, since it divides by
// whichever value lands in the "serverComputed" position -- 10/15 ≈ 0.67
// (>= FilesChangedDriftRatioThreshold, fires) using the CORRECT
// (self-reported, server-computed) order, versus 10/25 = 0.4 (does NOT
// fire) were the two ever accidentally swapped at the call site. A
// same-magnitude pair like 3-vs-50 would fire under EITHER order (both
// ratios land far past the threshold), silently passing a swapped-
// argument regression -- this pair is chosen specifically so it would not.
func driftFiringVerdictRequestJSON() string {
	return strings.Replace(validVerdictRequestJSON(), `"filesChanged": 3`, `"filesChanged": 25`, 1)
}

// testDispatchMessageID (finding F3 (§21.1's amendment)) is the shared, fixed
// sandboxws.Prompt MessageId value every turn-seeding fixture in this file
// stamps onto turns.dispatched_message_id, and postReviewVerdict presents
// back as the X-Sandbox-Dispatch-Message-Id header -- so a test asserting
// on the resulting review_verdicts row's own head_sha/context genuinely
// exercises the real dispatched-message-id lookup (turns.
// GetByDispatchedMessageID), never a coincidental degrade-to-empty path
// that happens to produce the same outward assertion for the wrong
// reason.
const testDispatchMessageID = "test-dispatch-message-id"

// postReviewVerdict posts body to sessionID's own review/verdict endpoint,
// with bearer/gen as the SAME sandbox-bearer-auth headers scmcredentials_
// integration_test.go's own postScmCredentialsFull establishes -- gen ==
// "" omits the X-Sandbox-Gen header entirely (matching a real caller that
// never sends it), mirroring that helper's own identical convention.
// dispatchMessageID (finding F3 (§21.1's amendment)) mirrors that SAME convention
// one header further: "" omits X-Sandbox-Dispatch-Message-Id entirely
// (matching a caller/turn that predates this fix, or a test that
// deliberately exercises the no-context-resolvable degradation).
func postReviewVerdict(t *testing.T, r testRig, sessionID, bearer, gen, dispatchMessageID, body string) (int, restdtos.PostReviewVerdictResponse) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, r.server.URL+"/sessions/"+sessionID+"/review/verdict", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if gen != "" {
		req.Header.Set("X-Sandbox-Gen", gen)
	}
	if dispatchMessageID != "" {
		req.Header.Set("X-Sandbox-Dispatch-Message-Id", dispatchMessageID)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var got restdtos.PostReviewVerdictResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	return resp.StatusCode, got
}

// setupReviewSessionWithSandbox creates an owned GitHub review session
// (createOwnedGitHubReviewSession, reviewretrigger_integration_test.go)
// plus a real sandbox row with a known bearer token (createSandboxWithToken,
// scmcredentials_integration_test.go) -- the fixture every happy/auth-path
// test in this file needs, reusing both existing helpers rather than a
// third, duplicated setup routine.
func setupReviewSessionWithSandbox(ctx context.Context, t *testing.T, r testRig, repoFullName string, prNumber int32) sqlcgen.Session {
	t.Helper()
	owner, _ := r.createAuthenticatedUser(ctx, t)
	session := r.createOwnedGitHubReviewSession(ctx, t, owner.ID, repoFullName, prNumber)
	createSandboxWithToken(ctx, t, r, session.ID, "sandbox-bearer-token")
	return session
}

// seedDispatchedTurn creates a processing turn for sessionID and stamps
// its dispatched_message_id to testDispatchMessageID -- D1/D4/D6's own
// fix (second adversarial-review round) means PostReviewVerdict now
// REFUSES (403) any call it cannot attribute to a specific, real turn, so
// every test below that used to pass an empty dispatch-message-id header
// and expect a 201 now needs a real turn to resolve. Mirrors
// TestPostReviewVerdict_PersistsReviewVerdictRow_WhenReviewHeadSHAKnown's
// own pre-existing two-step Create-then-UpdateStatus dance verbatim
// (turns.Create itself has no DispatchedMessageID param) -- reused here
// rather than duplicated once more for every caller that does not
// otherwise need a bespoke turn shape (a specific review_head_sha or
// review_depth_decision payload; those keep seeding their own turn
// directly, exactly as before this fix).
func seedDispatchedTurn(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID) sqlcgen.Turn {
	t.Helper()
	created, err := r.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionID, Status: sqlcgen.TurnStatusProcessing})
	if err != nil {
		t.Fatalf("seed processing turn: %v", err)
	}
	messageID := testDispatchMessageID
	updated, err := r.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: created.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedMessageID: &messageID})
	if err != nil {
		t.Fatalf("stamp dispatched_message_id on seeded turn: %v", err)
	}
	return updated
}

func TestPostReviewVerdict_NotFound(t *testing.T) {
	rig := newTestRig(t)

	status, _ := postReviewVerdict(t, rig, "00000000-0000-0000-0000-000000000000", "any-token", "1", "", validVerdictRequestJSON())
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

func TestPostReviewVerdict_MissingBearer_Unauthorized(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-nobearer", 1)

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "", "1", "", validVerdictRequestJSON())
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

func TestPostReviewVerdict_WrongToken_Unauthorized(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-wrongtoken", 2)

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "not-the-real-token", "1", "", validVerdictRequestJSON())
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

func TestPostReviewVerdict_GenMismatch_Forbidden(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-genmismatch", 3)

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "999", "", validVerdictRequestJSON())
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}
}

func TestPostReviewVerdict_MissingGen_Forbidden(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-missinggen", 4)

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "", "", validVerdictRequestJSON())
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d (a well-formed caller always sends X-Sandbox-Gen)", status, http.StatusForbidden)
	}
}

// TestPostReviewVerdict_DeadSandbox_Gone proves a terminalized sandbox
// (status stopped/failed/stale) can never post a verdict -- mirrors
// scm-credentials/snapshot-mint's own identical dead-sandbox check.
func TestPostReviewVerdict_DeadSandbox_Gone(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-dead", 5)

	if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET status = 'stopped' WHERE session_id = $1`, session.ID); err != nil {
		t.Fatalf("mark sandbox stopped: %v", err)
	}

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", "", validVerdictRequestJSON())
	if status != http.StatusGone {
		t.Errorf("status = %d, want %d", status, http.StatusGone)
	}
}

// TestPostReviewVerdict_NoGitHubPRMapping_BadRequest proves a session with
// a real sandbox but NO github_pr_sessions row (not a review session at
// all) is rejected 400.
func TestPostReviewVerdict_NoGitHubPRMapping_BadRequest(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	session, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	createSandboxWithToken(ctx, t, rig, session.ID, "sandbox-bearer-token")

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", "", validVerdictRequestJSON())
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d (no github_pr_sessions row for this session)", status, http.StatusBadRequest)
	}
}

// TestPostReviewVerdict_MalformedPartialPayload covers this Step's own
// named test-coverage item: "the tool rejecting a malformed or partial
// typed payload". Every case below is a DIFFERENT way a caller's typed
// tool call can be wrong -- missing a required field entirely, an
// out-of-enum value, a negative count, and a syntactically-present-but-
// semantically-empty summary (whitespace only) that only reviewpost.
// ValidateVerdictInput itself catches, since the generated DTO's own
// decode-time check only requires length >= 1, not non-blank content.
func TestPostReviewVerdict_MalformedPartialPayload(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty object -- every field missing", body: `{}`},
		{name: "missing riskLevel entirely (partial payload)", body: `{"premise":"ok","blastRadius":[],"filesChanged":1,"testsCoverage":"adequate","docsDrift":"none","proposedShippable":"auto","summary":"x"}`},
		{name: "missing summary entirely (partial payload)", body: `{"riskLevel":"low","premise":"ok","blastRadius":[],"filesChanged":1,"testsCoverage":"adequate","docsDrift":"none","proposedShippable":"auto"}`},
		{name: "garbled riskLevel enum value", body: `{"riskLevel":"extreme","premise":"ok","blastRadius":[],"filesChanged":1,"testsCoverage":"adequate","docsDrift":"none","proposedShippable":"auto","summary":"x"}`},
		{name: "unrecognized blastRadius tag", body: `{"riskLevel":"low","premise":"ok","blastRadius":["frontend"],"filesChanged":1,"testsCoverage":"adequate","docsDrift":"none","proposedShippable":"auto","summary":"x"}`},
		{name: "negative filesChanged", body: `{"riskLevel":"low","premise":"ok","blastRadius":[],"filesChanged":-1,"testsCoverage":"adequate","docsDrift":"none","proposedShippable":"auto","summary":"x"}`},
		{name: "whitespace-only summary (caught by ValidateVerdictInput, not schema decode)", body: `{"riskLevel":"low","premise":"ok","blastRadius":[],"filesChanged":1,"testsCoverage":"adequate","docsDrift":"none","proposedShippable":"auto","summary":"   "}`},
		{name: "malformed JSON", body: `{not json`},
		// (§26.1): "digest" is REQUIRED, and "digest.summary" is
		// the one field within it this Step actually validates.
		{name: "missing digest entirely (§26.1, partial payload)", body: `{"riskLevel":"low","premise":"ok","blastRadius":[],"filesChanged":1,"testsCoverage":"adequate","docsDrift":"none","proposedShippable":"auto","summary":"x"}`},
		{name: "digest present but missing digest.summary entirely (§26.1, partial payload)", body: `{"riskLevel":"low","premise":"ok","blastRadius":[],"filesChanged":1,"testsCoverage":"adequate","docsDrift":"none","proposedShippable":"auto","summary":"x","digest":{}}`},
		{name: "whitespace-only digest.summary (caught by ValidateVerdictInput, not schema decode)", body: `{"riskLevel":"low","premise":"ok","blastRadius":[],"filesChanged":1,"testsCoverage":"adequate","docsDrift":"none","proposedShippable":"auto","summary":"x","digest":{"summary":"   ","descriptionAdequacy":"ok","adequacyExplanation":"x"}}`},
		// §26.2: "descriptionAdequacy"/"adequacyExplanation" are
		// REQUIRED on every review from this Step on, the SAME treatment
		// as digest.summary above.
		{name: "digest present but missing digest.descriptionAdequacy entirely (§26.2, partial payload)", body: `{"riskLevel":"low","premise":"ok","blastRadius":[],"filesChanged":1,"testsCoverage":"adequate","docsDrift":"none","proposedShippable":"auto","summary":"x","digest":{"summary":"x","adequacyExplanation":"x"}}`},
		{name: "garbled digest.descriptionAdequacy enum value", body: `{"riskLevel":"low","premise":"ok","blastRadius":[],"filesChanged":1,"testsCoverage":"adequate","docsDrift":"none","proposedShippable":"auto","summary":"x","digest":{"summary":"x","descriptionAdequacy":"somewhat","adequacyExplanation":"x"}}`},
		{name: "digest present but missing digest.adequacyExplanation entirely (§26.2, partial payload)", body: `{"riskLevel":"low","premise":"ok","blastRadius":[],"filesChanged":1,"testsCoverage":"adequate","docsDrift":"none","proposedShippable":"auto","summary":"x","digest":{"summary":"x","descriptionAdequacy":"ok"}}`},
		{name: "whitespace-only digest.adequacyExplanation (caught by ValidateVerdictInput, not schema decode)", body: `{"riskLevel":"low","premise":"ok","blastRadius":[],"filesChanged":1,"testsCoverage":"adequate","docsDrift":"none","proposedShippable":"auto","summary":"x","digest":{"summary":"x","descriptionAdequacy":"ok","adequacyExplanation":"   "}}`},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			session := setupReviewSessionWithSandbox(ctx, t, rig, fmt.Sprintf("acme/verdict-malformed-%d", i), int32(i+1))

			status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", "", tc.body)
			if status != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
			}

			// No outbox row must ever be enqueued for a rejected payload.
			var count int
			if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1`, session.ID).Scan(&count); err != nil {
				t.Fatalf("count outbox rows: %v", err)
			}
			if count != 0 {
				t.Errorf("outbox row count = %d, want 0 (a rejected payload must never enqueue anything)", count)
			}
		})
	}
}

// TestPostReviewVerdict_Success_EnqueuesGitHubVerdictOutboxRow proves the
// happy path end to end: 201, a real response body with the SERVER-
// computed Shippable/formalReviewEvent/syncedLabel, and exactly one
// ports.NotificationKindGitHubVerdict outbox row shaped as githubapi.
// VerdictPayload with the right owner/repo/PR/risk data.
func TestPostReviewVerdict_Success_EnqueuesGitHubVerdictOutboxRow(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-success", 42)
	seedDispatchedTurn(ctx, t, rig, session.ID)

	status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, validVerdictRequestJSON())
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	if resp.Shippable != restdtos.PostReviewVerdictResponseShippableAuto {
		t.Errorf("Shippable = %q, want %q (low risk, ok premise, adequate coverage -> auto)", resp.Shippable, restdtos.PostReviewVerdictResponseShippableAuto)
	}
	if resp.FormalReviewEvent != restdtos.PostReviewVerdictResponseFormalReviewEventCOMMENT {
		t.Errorf("FormalReviewEvent = %q, want %q", resp.FormalReviewEvent, restdtos.PostReviewVerdictResponseFormalReviewEventCOMMENT)
	}
	if resp.SyncedLabel != reviewpost.LabelLowRisk {
		t.Errorf("SyncedLabel = %q, want %q", resp.SyncedLabel, reviewpost.LabelLowRisk)
	}

	var row sqlcgen.Outbox
	if err := rig.pool.QueryRow(ctx, `SELECT id, session_id, kind, payload, status FROM outbox WHERE session_id = $1`, session.ID).
		Scan(&row.ID, &row.SessionID, &row.Kind, &row.Payload, &row.Status); err != nil {
		t.Fatalf("query outbox row: %v", err)
	}
	if row.Kind != string(ports.NotificationKindGitHubVerdict) {
		t.Errorf("Kind = %q, want %q", row.Kind, ports.NotificationKindGitHubVerdict)
	}

	var payload githubapi.VerdictPayload
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatalf("unmarshal outbox payload: %v", err)
	}
	if payload.Owner != "acme" || payload.Repo != "verdict-success" {
		t.Errorf("Owner/Repo = %q/%q, want %q/%q", payload.Owner, payload.Repo, "acme", "verdict-success")
	}
	if payload.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", payload.PRNumber)
	}
	if payload.Event != string(reviewpost.FormalReviewEventComment) {
		t.Errorf("Event = %q, want %q", payload.Event, reviewpost.FormalReviewEventComment)
	}
	if payload.RiskLevel != string(review.RiskLevelLow) {
		t.Errorf("RiskLevel = %q, want %q", payload.RiskLevel, review.RiskLevelLow)
	}
	if !strings.Contains(payload.Body, "Looks good overall, one minor nit below.") {
		t.Errorf("Body = %q, want it to contain the request's own summary", payload.Body)
	}
	if !strings.Contains(payload.Body, "narvi-test-bot") {
		t.Errorf("Body = %q, want it to contain the rendered re-run guidance's bot handle", payload.Body)
	}
}

// TestPostReviewVerdict_PersistsReviewVerdictRow_WhenReviewHeadSHAKnown
// is §21's own (§21.1, updated for) end-to-end
// persistence test: when the session's own CURRENTLY-PROCESSING turn
// carries a review_head_sha (set here exactly the way turn-creation
// itself sets it in production -- turns.Create's own ReviewHeadSha
// param, internal/adapters/inbound/httpapi's createTurnLocked/
// CreateSessionOnTx -- never fabricated some other way), the verdict
// POST appends a review_verdicts row forwarding EVERY field verbatim --
// repo/PR identity, head_sha, and the full Verdict, including the
// server-computed (never client-trusted) Shippable.
func TestPostReviewVerdict_PersistsReviewVerdictRow_WhenReviewHeadSHAKnown(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-persist", 55)

	// the head sha lives on this specific turn
	// (turns.review_head_sha) -- mirrors how a real review turn is
	// dispatched (status='processing') by the time its own agent calls
	// this endpoint. Resolved via turns.GetByDispatchedMessageID below
	// (finding F3), never TurnStore.GetProcessingTurnForSession -- see
	// that lookup's own doc comment two lines down.
	reviewHeadSHA := "sha-persist-abc123"
	createdTurn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusProcessing, ReviewHeadSha: &reviewHeadSHA})
	if err != nil {
		t.Fatalf("seed processing turn with review head sha: %v", err)
	}
	// dispatched_message_id (finding F3 (§21.1's amendment)): stamped here so
	// postReviewVerdict's own testDispatchMessageID header resolves THIS
	// turn via the real turns.GetByDispatchedMessageID lookup.
	turnMessageID := testDispatchMessageID
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: createdTurn.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedMessageID: &turnMessageID}); err != nil {
		t.Fatalf("stamp dispatched_message_id on seeded turn: %v", err)
	}

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, validVerdictRequestJSON())
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var row sqlcgen.ReviewVerdict
	if err := rig.pool.QueryRow(ctx, `SELECT repo_full_name, pr_number, head_sha, risk_level, premise, files_changed, tests_coverage, docs_drift, shippable FROM review_verdicts WHERE repo_full_name = $1 AND pr_number = $2`, "acme/verdict-persist", 55).
		Scan(&row.RepoFullName, &row.PrNumber, &row.HeadSha, &row.RiskLevel, &row.Premise, &row.FilesChanged, &row.TestsCoverage, &row.DocsDrift, &row.Shippable); err != nil {
		t.Fatalf("query review_verdicts row: %v", err)
	}
	if row.HeadSha != "sha-persist-abc123" {
		t.Errorf("head_sha = %q, want %q (forwarded verbatim from this turn's own turns.review_head_sha)", row.HeadSha, "sha-persist-abc123")
	}
	if row.RiskLevel != string(review.RiskLevelLow) {
		t.Errorf("risk_level = %q, want %q", row.RiskLevel, review.RiskLevelLow)
	}
	if row.Premise != string(review.PremiseStateOK) {
		t.Errorf("premise = %q, want %q", row.Premise, review.PremiseStateOK)
	}
	if row.FilesChanged != 3 {
		t.Errorf("files_changed = %d, want 3", row.FilesChanged)
	}
	if row.TestsCoverage != string(review.TestsCoverageStateAdequate) {
		t.Errorf("tests_coverage = %q, want %q", row.TestsCoverage, review.TestsCoverageStateAdequate)
	}
	if row.DocsDrift != string(review.DocsDriftStateNone) {
		t.Errorf("docs_drift = %q, want %q", row.DocsDrift, review.DocsDriftStateNone)
	}
	if row.Shippable != string(review.ShippableAuto) {
		t.Errorf("shippable = %q, want %q (server-computed from risk=low/premise=ok/coverage=adequate)", row.Shippable, review.ShippableAuto)
	}
}

// TestPostReviewVerdict_DispatchedMessageIDScopedToSession_NeverResolvesAnotherSessionsTurn
// is round-10 finding A3's own regression test: turns.
// GetTurnByDispatchedMessageID (queries/turns.sql) is scoped by BOTH
// session_id AND dispatched_message_id -- session_id is the ONLY thing
// stopping a request authenticated for one sandbox/session from
// addressing a DIFFERENT session's own turn, since dispatched_message_id
// alone is unique only in PRACTICE (a UUID minted fresh per real
// dispatch, never DB-enforced -- GetTurnByDispatchedMessageID's own doc
// comment, queries/turns.sql), not by construction. Two real sessions,
// each with their own real sandbox and their own real dispatched turn,
// both deliberately stamped with the SAME dispatched_message_id
// (testDispatchMessageID, seedDispatchedTurn's own shared fixed
// constant -- an artificial collision no two real dispatches would ever
// produce on their own, but exactly what a DB-level uniqueness gap
// permits) -- proving a verdict POSTed against session A, authenticated
// with session A's own bearer, is attributed to session A's OWN turn
// (via review_verdicts.attempt_id, finding A4) and never session B's,
// even though both turns match on dispatched_message_id alone.
//
// Mutation-test target: deleting "AND dispatched_message_id = $2"'s own
// sibling "session_id = $1" predicate from GetTurnByDispatchedMessageID
// (queries/turns.sql) leaves this package's OWN suite green apart from
// THIS test -- see this Step's own PR body for the exact mutation run
// and the observed failure.
func TestPostReviewVerdict_DispatchedMessageIDScopedToSession_NeverResolvesAnotherSessionsTurn(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	sessionA := setupReviewSessionWithSandbox(ctx, t, rig, "acme/scoped-session-a", 501)
	sessionB := setupReviewSessionWithSandbox(ctx, t, rig, "acme/scoped-session-b", 502)

	// A real, dispatched, PROCESSING turn per session, each carrying its
	// own distinguishable review_head_sha (never seedDispatchedTurn,
	// which leaves review_head_sha NULL -- PostReviewVerdict skips the
	// review_verdicts insert entirely for a NULL head sha, which would
	// make this test's own attempt_id query find nothing to assert on)
	// -- mirrors TestPostReviewVerdict_PersistsReviewVerdictRow_
	// WhenReviewHeadSHAKnown's own Create-then-UpdateStatus dance.
	//
	// Deliberate ORDER (both turns created first, then session B's own
	// turn stamped with the shared dispatch id BEFORE session A's own):
	// Postgres MVCC gives an UPDATE's new row version a fresh physical
	// tuple, appended after every tuple that already exists -- a plain
	// sequential scan with no ORDER BY (this query's own shape) walks
	// tuples in that physical order, so stamping B's turn first makes
	// B's live tuple sort BEFORE A's. Without this ordering, an
	// unscoped query would still happen to return A's own turn FIRST
	// (created and stamped earliest) even with the session_id predicate
	// removed, and this test would pass for the wrong reason regardless
	// of whether the scoping the mutation below removes is present at
	// all.
	headSHAA := "sha-session-a"
	turnA, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionA.ID, Status: sqlcgen.TurnStatusProcessing, ReviewHeadSha: &headSHAA})
	if err != nil {
		t.Fatalf("seed session A's own turn: %v", err)
	}
	headSHAB := "sha-session-b"
	turnB, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sessionB.ID, Status: sqlcgen.TurnStatusProcessing, ReviewHeadSha: &headSHAB})
	if err != nil {
		t.Fatalf("seed session B's own turn: %v", err)
	}
	// Round-10 finding A3's own artificial collision: session B's turn
	// deliberately shares session A's own dispatched_message_id --
	// dispatched_message_id is unique only in PRACTICE (a UUID minted
	// fresh per real dispatch, never DB-enforced), so this is exactly the
	// shape a DB-level uniqueness gap permits, never something two real
	// dispatches would produce on their own. Stamped BEFORE session A's
	// own turn -- see the ordering note above.
	turnBMessageID := testDispatchMessageID
	if turnB, err = rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: turnB.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedMessageID: &turnBMessageID}); err != nil {
		t.Fatalf("stamp dispatched_message_id on session B's own turn: %v", err)
	}
	turnAMessageID := testDispatchMessageID
	if turnA, err = rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: turnA.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedMessageID: &turnAMessageID}); err != nil {
		t.Fatalf("stamp dispatched_message_id on session A's own turn: %v", err)
	}
	if turnA.ID == turnB.ID {
		t.Fatal("turnA.ID == turnB.ID -- test setup is broken, this assertion would be vacuous")
	}

	status, resp := postReviewVerdict(t, rig, sessionA.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, validVerdictRequestJSON())
	if status != http.StatusCreated {
		t.Fatalf("status = %d, body = %+v, want %d", status, resp, http.StatusCreated)
	}

	var attemptID pgtype.UUID
	if err := rig.pool.QueryRow(ctx, `SELECT attempt_id FROM review_verdicts WHERE repo_full_name = $1 AND pr_number = $2`, "acme/scoped-session-a", 501).Scan(&attemptID); err != nil {
		t.Fatalf("query review_verdicts attempt_id: %v", err)
	}
	if !attemptID.Valid {
		t.Fatal("review_verdicts.attempt_id is NULL, want a valid id -- this assertion needs a real value to compare")
	}
	if attemptID.Bytes == turnB.ID.Bytes {
		t.Fatalf("review_verdicts.attempt_id = session B's own turn id (%s) -- posting against session A resolved a DIFFERENT session's turn, proving turns.GetTurnByDispatchedMessageID's own session_id scoping is not being applied", turnB.ID)
	}
	if attemptID.Bytes != turnA.ID.Bytes {
		t.Errorf("review_verdicts.attempt_id = %s, want session A's own turn id %s", attemptID, turnA.ID)
	}
}

// TestPostReviewVerdict_PersistsDigestColumns is §26.1's own (§26.1)
// persistence proof: digest_summary/digest_arch_decisions/
// digest_stack_risks/digest_unverified_limits (migrations/
// 000077_review_verdicts_digest.up.sql) are all populated from the
// posted digest, verbatim, on the SAME review_verdicts row §21
// already writes -- "digest quality measurable from day one". It ALSO
// closes the one end-to-end gap on this Step's actual deliverable -- the
// rendered merge readout -- by asserting the SAME posted digest reaches
// the enqueued outbox row's own Body (reviewpost.RenderVerdictComment's
// output, githubapi.VerdictNotifier's own delivery payload): persistence
// and rendering were previously only ever proven in isolation from each
// other (this test proving persistence, TestRenderVerdictComment* proving
// rendering from a hand-built Digest), so a regression that silently
// stopped the digest from ever reaching the POSTED COMMENT -- e.g.
// PostReviewVerdict building the outbox payload from the wrong Digest, or
// dropping it before RenderVerdictComment ever saw it -- would have been
// caught by NEITHER.
func TestPostReviewVerdict_PersistsDigestColumns(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-digest-persist", 66)

	reviewHeadSHA := "sha-digest-persist-abc123"
	createdTurn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusProcessing, ReviewHeadSha: &reviewHeadSHA})
	if err != nil {
		t.Fatalf("seed processing turn with review head sha: %v", err)
	}
	turnMessageID := testDispatchMessageID
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: createdTurn.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedMessageID: &turnMessageID}); err != nil {
		t.Fatalf("stamp dispatched_message_id on seeded turn: %v", err)
	}

	body := `{
		"riskLevel": "low",
		"premise": "ok",
		"blastRadius": [],
		"filesChanged": 2,
		"testsCoverage": "adequate",
		"docsDrift": "none",
		"proposedShippable": "auto",
		"summary": "Looks good.",
		"digest": {
			"summary": "Adds a retry helper around the flaky upstream call.",
			"archDecisions": [
				{"decision": "Centralize retries in one helper.", "rejectedAlternative": "Inline retry logic per call site.", "conventionConformance": "Matches CLAUDE.md's shared-helper convention."}
			],
			"stackRisks": "Touches every call site of the upstream client; a regression here is broad.",
			"unverifiedLimits": "Did not verify behavior under a real network partition.",
			"descriptionAdequacy": "drift",
			"adequacyExplanation": "The PR body doesn't mention the new retry helper at all."
		},
		"factCheck": "done",
		"factCheckKilled": 0
	}`

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, body)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var digestSummary, digestStackRisks, digestUnverifiedLimits, digestDescriptionAdequacy, digestAdequacyExplanation *string
	var digestArchDecisions []byte
	if err := rig.pool.QueryRow(ctx, `SELECT digest_summary, digest_arch_decisions, digest_stack_risks, digest_unverified_limits, digest_description_adequacy, digest_adequacy_explanation FROM review_verdicts WHERE repo_full_name = $1 AND pr_number = $2`, "acme/verdict-digest-persist", 66).
		Scan(&digestSummary, &digestArchDecisions, &digestStackRisks, &digestUnverifiedLimits, &digestDescriptionAdequacy, &digestAdequacyExplanation); err != nil {
		t.Fatalf("query review_verdicts digest columns: %v", err)
	}

	if digestSummary == nil || *digestSummary != "Adds a retry helper around the flaky upstream call." {
		t.Errorf("digest_summary = %v, want %q", digestSummary, "Adds a retry helper around the flaky upstream call.")
	}
	if digestStackRisks == nil || *digestStackRisks != "Touches every call site of the upstream client; a regression here is broad." {
		t.Errorf("digest_stack_risks = %v, want the posted stackRisks text", digestStackRisks)
	}
	if digestUnverifiedLimits == nil || *digestUnverifiedLimits != "Did not verify behavior under a real network partition." {
		t.Errorf("digest_unverified_limits = %v, want the posted unverifiedLimits text", digestUnverifiedLimits)
	}
	if digestDescriptionAdequacy == nil || *digestDescriptionAdequacy != "drift" {
		t.Errorf("digest_description_adequacy = %v, want %q", digestDescriptionAdequacy, "drift")
	}
	if digestAdequacyExplanation == nil || *digestAdequacyExplanation != "The PR body doesn't mention the new retry helper at all." {
		t.Errorf("digest_adequacy_explanation = %v, want the posted adequacyExplanation text", digestAdequacyExplanation)
	}
	if !strings.Contains(string(digestArchDecisions), "Centralize retries in one helper.") {
		t.Errorf("digest_arch_decisions = %s, want it to contain the posted decision text", digestArchDecisions)
	}
	if !strings.Contains(string(digestArchDecisions), "Matches CLAUDE.md's shared-helper convention.") {
		t.Errorf("digest_arch_decisions = %s, want it to contain the posted conventionConformance text", digestArchDecisions)
	}

	// The end-to-end proof: the SAME digest that just landed in
	// review_verdicts above must ALSO be present in the outbox row's own
	// rendered comment body -- the text that actually gets posted to the
	// PR. Queried the same way TestPostReviewVerdict_
	// Success_EnqueuesGitHubVerdictOutboxRow already does -- scoped to
	// kind = github_verdict specifically (§26.2's own no
	// descriptionAutofix candidate: this request has no proposedBody, so
	// only this ONE outbox row is ever enqueued for this session; the
	// explicit kind filter keeps this query correct regardless).
	var row sqlcgen.Outbox
	if err := rig.pool.QueryRow(ctx, `SELECT id, session_id, kind, payload, status FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindGitHubVerdict)).
		Scan(&row.ID, &row.SessionID, &row.Kind, &row.Payload, &row.Status); err != nil {
		t.Fatalf("query outbox row: %v", err)
	}

	var payload githubapi.VerdictPayload
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatalf("unmarshal outbox payload: %v", err)
	}

	for _, want := range []string{
		"### What this PR does",
		"Adds a retry helper around the flaky upstream call.",
		"### Architecture choices",
		"Centralize retries in one helper.",
		"Inline retry logic per call site.",
		"Matches CLAUDE.md's shared-helper convention.",
		"### Risks to the stack",
		"Touches every call site of the upstream client; a regression here is broad.",
		"Did not verify behavior under a real network partition.",
		"Description adequacy",
		"drift",
		"The PR body doesn't mention the new retry helper at all.",
	} {
		if !strings.Contains(payload.Body, want) {
			t.Errorf("outbox payload Body missing %q -- the digest persisted to review_verdicts did not reach the rendered comment. Body:\n%s", want, payload.Body)
		}
	}
}

// TestPostReviewVerdict_SkipsReviewVerdictInsert_WhenNoReviewHeadSHA
// proves a PR whose own processing turn carries NO review_head_sha (e.g.
// a review turn whose own context fetch degraded to no head sha at all,
// reviewcontext.Fetch's own doc comment) still posts successfully --
// review_verdicts persistence is best-effort enrichment, never a
// precondition for this tool call to succeed (see reviewverdict.go's own
// doc comment on this exact point). A real processing turn IS seeded
// here (updated for a verdict POST in
// production always corresponds to some real, currently-processing
// turn), just with review_head_sha left nil.
func TestPostReviewVerdict_SkipsReviewVerdictInsert_WhenNoReviewHeadSHA(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-no-head-sha", 56)
	// Deliberately no ReviewHeadSha -- this is the fact under test.
	createdTurn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusProcessing})
	if err != nil {
		t.Fatalf("seed processing turn with no review head sha: %v", err)
	}
	turnMessageID := testDispatchMessageID
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: createdTurn.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedMessageID: &turnMessageID}); err != nil {
		t.Fatalf("stamp dispatched_message_id on seeded turn: %v", err)
	}

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, validVerdictRequestJSON())
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d (a missing head sha must never fail the verdict post itself)", status, http.StatusCreated)
	}

	var count int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM review_verdicts WHERE repo_full_name = $1 AND pr_number = $2`, "acme/verdict-no-head-sha", 56).Scan(&count); err != nil {
		t.Fatalf("count review_verdicts rows: %v", err)
	}
	if count != 0 {
		t.Errorf("review_verdicts row count = %d, want 0 (no head sha on record, insert must be skipped, never inserted with a fabricated/empty value)", count)
	}
}

// dispatchMissingHeaderRejectedLogMsg/dispatchPlaceholderRejectedLogMsg/
// dispatchNoTurnRejectedLogMsg are reviewverdict.go's own exact log
// messages for D1/D4/D10's three distinct "cannot attribute" branches --
// named here as literals (this is package httpapi_test, a black-box test
// package) rather than re-deriving them, mirroring
// filesChangedDriftCanaryLogMsg's own identical "named literal for an
// unexported production string" precedent below. Distinct log lines for
// distinct causes are the whole point of D4's own fix: a mutation that
// deletes one dedicated branch and lets it fall through to another still
// returns the identical 403 (status/persistence alone cannot catch it),
// but logs the WRONG line -- which the tests below assert on directly.
const (
	dispatchMissingHeaderRejectedLogMsg = "httpapi: review-verdict: rejecting: no X-Sandbox-Dispatch-Message-Id header presented, cannot attribute this verdict to a specific turn"
	dispatchPlaceholderRejectedLogMsg   = "httpapi: review-verdict: rejecting: presented X-Sandbox-Dispatch-Message-Id is the literal unsubstituted placeholder -- sandbox image predates this substitution, cannot attribute this verdict to a specific turn"
	dispatchNoTurnRejectedLogMsg        = "httpapi: review-verdict: rejecting: no turn found for this session's own presented dispatch message id"
)

// assertRejectedNoVerdictOfRecord is the shared assertion every D1/D4/D6
// "cannot be attributed to a turn" test below needs: 403, and NEITHER an
// outbox row NOR a review_verdicts row was ever created -- the whole tool
// call was refused, never a degraded-but-successful post (D6's own
// "confident normal state" failure mode this fix removes).
func assertRejectedNoVerdictOfRecord(t *testing.T, rig testRig, ctx context.Context, sessionID, repoFullName string, prNumber int32, gotStatus int) {
	t.Helper()
	if gotStatus != http.StatusForbidden {
		t.Fatalf("status = %d, want %d (a verdict this endpoint cannot attribute to a real turn must be refused outright, never degraded to a successful post)", gotStatus, http.StatusForbidden)
	}

	var verdictCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM review_verdicts WHERE repo_full_name = $1 AND pr_number = $2`, repoFullName, prNumber).Scan(&verdictCount); err != nil {
		t.Fatalf("count review_verdicts rows: %v", err)
	}
	if verdictCount != 0 {
		t.Errorf("review_verdicts row count = %d, want 0 (a refused request must never persist a verdict)", verdictCount)
	}

	var outboxCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1`, sessionID).Scan(&outboxCount); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if outboxCount != 0 {
		t.Errorf("outbox row count = %d, want 0 (a refused request must never post a comment)", outboxCount)
	}
}

// TestPostReviewVerdict_RejectsWhenDispatchMessageIDHeaderMissing is D1's
// own regression test (second adversarial-review round, HIGH,
// prompt-injectable): before this fix, an omitted
// X-Sandbox-Dispatch-Message-Id header degraded IDENTICALLY to a
// not-found lookup -- logged, reviewDepth left at its own zero value
// (satisfying neither light- nor deep-path enforcement), verdict posted
// anyway, review_verdicts insert skipped, 201 returned. Since a review
// turn's own prompt is the ONLY place this header's value lives before
// the agent composes its own HTTP call (§30.5: the agent runtime is not a
// trusted process boundary), an attacker's injected PR-body instruction
// to "omit that header" used to be a strictly BETTER outcome for evading
// deep-path enforcement than sending it correctly. This proves the
// server-side posture instead: a caller-suppressible header can no
// longer buy a degraded-but-successful outcome -- omitting it now fails
// the WHOLE call, exactly like an omitted Authorization header does.
func TestPostReviewVerdict_RejectsWhenDispatchMessageIDHeaderMissing(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	repoFullName := "acme/verdict-dispatch-header-missing"
	session := setupReviewSessionWithSandbox(ctx, t, rig, repoFullName, 57)
	seedDispatchedTurn(ctx, t, rig, session.ID)

	buf := captureDefaultLoggerJSON(t)

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", "", validVerdictRequestJSON())
	assertRejectedNoVerdictOfRecord(t, rig, ctx, session.ID.String(), repoFullName, 57, status)

	if !hasLogEntry(t, buf, dispatchMissingHeaderRejectedLogMsg) {
		t.Errorf("did not find the dedicated missing-header rejection log line; full log:\n%s", buf.String())
	}
}

// TestPostReviewVerdict_RejectsWhenDispatchMessageIDIsUnsubstitutedPlaceholder
// is D4's own regression test (second adversarial-review round, HIGH): an
// older sandbox image, mid rolling-upgrade (§35.2: "this system runs
// mixed image versions for weeks"), predates renderVerdictToolPromptText's
// own dispatch-message-id substitution, so the agent's prompt still
// carried the RAW, unsubstituted placeholder text and it echoed that back
// verbatim as the header value rather than a real id.
// review.VerdictToolDispatchMessageIDPlaceholder is a real, well-formed
// string -- distinct from a bare-empty header -- so it must be
// recognized explicitly (never merely fall through to a generic
// not-found lookup, where it would be indistinguishable from a genuine
// race or a forged value in every log line). The response is identical
// to the missing-header case (403, nothing persisted): an old image gets
// an honest, visible refusal instead of a false 201 with no verdict of
// record.
func TestPostReviewVerdict_RejectsWhenDispatchMessageIDIsUnsubstitutedPlaceholder(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	repoFullName := "acme/verdict-dispatch-header-placeholder"
	session := setupReviewSessionWithSandbox(ctx, t, rig, repoFullName, 58)
	seedDispatchedTurn(ctx, t, rig, session.ID)

	buf := captureDefaultLoggerJSON(t)

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", review.VerdictToolDispatchMessageIDPlaceholder, validVerdictRequestJSON())
	assertRejectedNoVerdictOfRecord(t, rig, ctx, session.ID.String(), repoFullName, 58, status)

	// The distinguishing half of D4: this must reach the DEDICATED
	// placeholder branch, never merely fall through to the generic
	// not-found lookup's own log line -- an operator watching for a
	// rolling-upgrade artifact must be able to tell "an old image sent
	// the raw placeholder" apart from "a genuine race or a forged value"
	// (TestPostReviewVerdict_RejectsWhenDispatchMessageIDNamesNoTurn,
	// below). A regression that deleted the dedicated placeholder case
	// (letting it fall through to the ordinary GetByDispatchedMessageID
	// lookup) would still return the identical 403 -- this assertion is
	// what actually catches that regression, since status/persistence
	// alone cannot.
	if !hasLogEntry(t, buf, dispatchPlaceholderRejectedLogMsg) {
		t.Errorf("did not find the dedicated placeholder-rejection log line -- the placeholder must be recognized explicitly, never merely fall through to the generic not-found lookup; full log:\n%s", buf.String())
	}
	if hasLogEntry(t, buf, dispatchMissingHeaderRejectedLogMsg) {
		t.Errorf("found the MISSING-header log line for a PRESENT (if unsubstituted) header -- these two cases must log distinguishably")
	}
}

// TestPostReviewVerdict_RejectsWhenDispatchMessageIDNamesNoTurn restores
// coverage of the pgx.ErrNoRows branch this test was originally named
// for (D10, LOW: a prior round repurposed
// TestPostReviewVerdict_SkipsReviewVerdictInsert_WhenNoProcessingTurnAtAll
// to exercise the missing-header case instead, leaving the real
// GetByDispatchedMessageID-returns-ErrNoRows path -- a real, well-formed
// header naming a turn this session never dispatched, e.g. a genuine
// race where the turn already completed/failed/was cancelled between the
// agent's own HTTP call landing and this read, or a forged value -- with
// no test of its own). Distinct from BOTH of the two tests immediately
// above: this header is neither empty nor the known placeholder, so it
// must reach the real store lookup and be refused there.
func TestPostReviewVerdict_RejectsWhenDispatchMessageIDNamesNoTurn(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	repoFullName := "acme/verdict-no-processing-turn"
	session := setupReviewSessionWithSandbox(ctx, t, rig, repoFullName, 59)
	// Deliberately no turn created at all -- "some-other-message-id" names
	// no row in turns.dispatched_message_id for this (or any) session, so
	// turns.GetByDispatchedMessageID returns pgx.ErrNoRows.

	buf := captureDefaultLoggerJSON(t)

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", "some-other-message-id", validVerdictRequestJSON())
	assertRejectedNoVerdictOfRecord(t, rig, ctx, session.ID.String(), repoFullName, 59, status)

	if !hasLogEntry(t, buf, dispatchNoTurnRejectedLogMsg) {
		t.Errorf("did not find the dedicated no-turn-found rejection log line; full log:\n%s", buf.String())
	}
}

// TestPostReviewVerdict_ShippableNeverTrustsProposedShippable proves the
// central security property end to end through the real HTTP surface:
// the model's own ProposedShippable ("auto") never overrides the
// server-computed Shippable, which here must be "block" (a not_a_pr
// premise is a hard floor breach regardless of risk/coverage).
func TestPostReviewVerdict_ShippableNeverTrustsProposedShippable(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-not-a-pr", 9)
	seedDispatchedTurn(ctx, t, rig, session.ID)

	body := `{
		"riskLevel": "low",
		"premise": "not_a_pr",
		"blastRadius": [],
		"filesChanged": 0,
		"testsCoverage": "skipped",
		"docsDrift": "skipped",
		"proposedShippable": "auto",
		"summary": "This diff is empty; nothing to review.",
		"digest": {
			"summary": "This PR's diff is empty; there is nothing to describe.",
			"descriptionAdequacy": "ok",
			"adequacyExplanation": "Nothing to compare against; the diff itself is empty."
		},
		"factCheck": "done",
		"factCheckKilled": 0
	}`

	status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, body)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if resp.Shippable != restdtos.PostReviewVerdictResponseShippableBlock {
		t.Errorf("Shippable = %q, want %q (the model's own ProposedShippableAuto must never override the premise floor)", resp.Shippable, restdtos.PostReviewVerdictResponseShippableBlock)
	}
	if resp.FormalReviewEvent != restdtos.PostReviewVerdictResponseFormalReviewEventREQUESTCHANGES {
		t.Errorf("FormalReviewEvent = %q, want %q", resp.FormalReviewEvent, restdtos.PostReviewVerdictResponseFormalReviewEventREQUESTCHANGES)
	}
}

// TestPostReviewVerdict_BlockOnHighRisk covers this Step's own named
// test-coverage item: "blockOnHighRisk both on and off". Same verdict
// input both times (every floor clean, but RiskLevel high -- which alone
// already raises the BASELINE Shippable to needs_human, review.
// baselineFromRisk's own documented behavior; RiskLevel is not one of the
// two named raise-only floors, see review/doc.go's design call #2) --
// only the repo's own repo_settings.block_on_high_risk flag differs, and
// only the resulting FormalReviewEvent (never Shippable itself, which
// stays "needs_human" either way -- blockOnHighRisk changes ONLY which
// GitHub review event this call submits, never the authoritative
// classification) should differ.
func TestPostReviewVerdict_BlockOnHighRisk(t *testing.T) {
	highRiskBody := `{
		"riskLevel": "high",
		"premise": "ok",
		"blastRadius": [],
		"filesChanged": 5,
		"testsCoverage": "adequate",
		"docsDrift": "none",
		"proposedShippable": "auto",
		"summary": "High risk per my own assessment, but every floor is clean.",
		"digest": {
			"summary": "Rewrites the token-refresh path to retry on transient failures.",
			"descriptionAdequacy": "ok",
			"adequacyExplanation": "The PR body accurately describes the token-refresh rewrite."
		},
		"factCheck": "done",
		"factCheckKilled": 0
	}`

	t.Run("off (default, no repo_settings row)", func(t *testing.T) {
		rig := newTestRig(t)
		ctx := context.Background()
		session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-blockonhighrisk-off", 10)
		seedDispatchedTurn(ctx, t, rig, session.ID)

		status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, highRiskBody)
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want %d", status, http.StatusCreated)
		}
		if resp.Shippable != restdtos.PostReviewVerdictResponseShippableNeedsHuman {
			t.Errorf("Shippable = %q, want %q (blockOnHighRisk never changes Shippable itself)", resp.Shippable, restdtos.PostReviewVerdictResponseShippableNeedsHuman)
		}
		if resp.FormalReviewEvent != restdtos.PostReviewVerdictResponseFormalReviewEventCOMMENT {
			t.Errorf("FormalReviewEvent = %q, want %q (blockOnHighRisk is off)", resp.FormalReviewEvent, restdtos.PostReviewVerdictResponseFormalReviewEventCOMMENT)
		}
	})

	t.Run("on (repo_settings.block_on_high_risk = true)", func(t *testing.T) {
		rig := newTestRig(t)
		ctx := context.Background()
		repoFullName := "acme/verdict-blockonhighrisk-on"
		session := setupReviewSessionWithSandbox(ctx, t, rig, repoFullName, 11)

		if _, err := rig.repoSettings.Upsert(ctx, repoFullName, true, false); err != nil {
			t.Fatalf("upsert repo settings: %v", err)
		}
		seedDispatchedTurn(ctx, t, rig, session.ID)

		status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, highRiskBody)
		if status != http.StatusCreated {
			t.Fatalf("status = %d, want %d", status, http.StatusCreated)
		}
		if resp.Shippable != restdtos.PostReviewVerdictResponseShippableNeedsHuman {
			t.Errorf("Shippable = %q, want %q (blockOnHighRisk never changes Shippable itself)", resp.Shippable, restdtos.PostReviewVerdictResponseShippableNeedsHuman)
		}
		if resp.FormalReviewEvent != restdtos.PostReviewVerdictResponseFormalReviewEventREQUESTCHANGES {
			t.Errorf("FormalReviewEvent = %q, want %q (blockOnHighRisk is on and risk is high)", resp.FormalReviewEvent, restdtos.PostReviewVerdictResponseFormalReviewEventREQUESTCHANGES)
		}
	})
}

// TestPostReviewVerdict_ConcurrentCalls_AllSucceedNoDeadlock is this
// endpoint's own real-concurrency proof, mirroring reviewretrigger.go's
// own identical precedent -- N concurrent verdict-posting-tool calls
// against the SAME review session all succeed, enqueuing exactly N
// outbox rows.
func TestPostReviewVerdict_ConcurrentCalls_AllSucceedNoDeadlock(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-concurrent", 77)
	seedDispatchedTurn(ctx, t, rig, session.ID)

	const n = 6
	start := make(chan struct{})
	statuses := make([]int, n)

	done := make(chan int, n)
	for i := 0; i < n; i++ {
		idx := i
		go func() {
			<-start
			status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, validVerdictRequestJSON())
			statuses[idx] = status
			done <- idx
		}()
	}
	close(start)
	for i := 0; i < n; i++ {
		<-done
	}

	for i, status := range statuses {
		if status != http.StatusCreated {
			t.Errorf("statuses[%d] = %d, want %d", i, status, http.StatusCreated)
		}
	}

	var count int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1`, session.ID).Scan(&count); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	if count != n {
		t.Errorf("outbox row count = %d, want %d", count, n)
	}
}

// TestPostReviewVerdict_MisleadingAdequacyRaisesShippable is this Step's
// own end-to-end pin, through the real HTTP surface: an otherwise-clean
// verdict (low risk, ok premise, adequate coverage -- Shippable would
// otherwise compute auto) whose digest.descriptionAdequacy is
// "misleading" must come back as needs_human, and the rendered outbox
// comment must carry the tri-state and its explanation.
func TestPostReviewVerdict_MisleadingAdequacyRaisesShippable(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-misleading-adequacy", 12)
	seedDispatchedTurn(ctx, t, rig, session.ID)

	body := `{
		"riskLevel": "low",
		"premise": "ok",
		"blastRadius": [],
		"filesChanged": 1,
		"testsCoverage": "adequate",
		"docsDrift": "none",
		"proposedShippable": "auto",
		"summary": "Small change, but the description doesn't match the diff.",
		"digest": {
			"summary": "Rewrites the auth token refresh path to retry on transient network failures.",
			"descriptionAdequacy": "misleading",
			"adequacyExplanation": "The PR title/body claim this is a docs-only change; the diff rewrites the auth token refresh path."
		},
		"factCheck": "done",
		"factCheckKilled": 0
	}`

	status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, body)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if resp.Shippable != restdtos.PostReviewVerdictResponseShippableNeedsHuman {
		t.Errorf("Shippable = %q, want %q (a misleading description must raise Shippable off an otherwise-clean auto baseline)", resp.Shippable, restdtos.PostReviewVerdictResponseShippableNeedsHuman)
	}

	var row sqlcgen.Outbox
	if err := rig.pool.QueryRow(ctx, `SELECT id, session_id, kind, payload, status FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindGitHubVerdict)).
		Scan(&row.ID, &row.SessionID, &row.Kind, &row.Payload, &row.Status); err != nil {
		t.Fatalf("query outbox row: %v", err)
	}
	var payload githubapi.VerdictPayload
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		t.Fatalf("unmarshal outbox payload: %v", err)
	}
	if !strings.Contains(payload.Body, "misleading") {
		t.Errorf("outbox payload Body missing the %q tri-state value, Body:\n%s", "misleading", payload.Body)
	}
	if !strings.Contains(payload.Body, "The PR title/body claim this is a docs-only change") {
		t.Errorf("outbox payload Body missing the posted adequacyExplanation, Body:\n%s", payload.Body)
	}
}

// TestPostReviewVerdict_AdequacyNeverAffectsRiskLevel is this Step's own
// end-to-end pin of §26.2's explicit asymmetry: RiskLevel is what the
// caller posted, completely untouched by descriptionAdequacy -- through
// the real HTTP surface, via the SAME risk_level column
// TestPostReviewVerdict_PersistsReviewVerdictRow_WhenReviewHeadSHAKnown
// already asserts, this time with an adequacy value that DOES raise
// Shippable (misleading) alongside a risk the model self-reported as
// medium.
func TestPostReviewVerdict_AdequacyNeverAffectsRiskLevel(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-adequacy-risklevel", 13)

	reviewHeadSHA := "sha-adequacy-risklevel-abc123"
	createdTurn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusProcessing, ReviewHeadSha: &reviewHeadSHA})
	if err != nil {
		t.Fatalf("seed processing turn with review head sha: %v", err)
	}
	turnMessageID := testDispatchMessageID
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: createdTurn.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedMessageID: &turnMessageID}); err != nil {
		t.Fatalf("stamp dispatched_message_id on seeded turn: %v", err)
	}

	body := `{
		"riskLevel": "medium",
		"premise": "ok",
		"blastRadius": [],
		"filesChanged": 1,
		"testsCoverage": "adequate",
		"docsDrift": "none",
		"proposedShippable": "auto",
		"summary": "Medium risk per my own assessment.",
		"digest": {
			"summary": "Rewrites the auth token refresh path.",
			"descriptionAdequacy": "misleading",
			"adequacyExplanation": "The PR body claims a docs-only change."
		},
		"factCheck": "done",
		"factCheckKilled": 0
	}`

	status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, body)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	// Shippable is raised by the misleading floor (medium risk baseline ->
	// needs_human already, so this alone doesn't distinguish the floor
	// firing -- the persisted risk_level column below is the real proof).
	if resp.Shippable != restdtos.PostReviewVerdictResponseShippableNeedsHuman {
		t.Errorf("Shippable = %q, want %q", resp.Shippable, restdtos.PostReviewVerdictResponseShippableNeedsHuman)
	}

	var riskLevel string
	if err := rig.pool.QueryRow(ctx, `SELECT risk_level FROM review_verdicts WHERE repo_full_name = $1 AND pr_number = $2`, "acme/verdict-adequacy-risklevel", 13).Scan(&riskLevel); err != nil {
		t.Fatalf("query review_verdicts risk_level: %v", err)
	}
	if riskLevel != string(review.RiskLevelMedium) {
		t.Errorf("risk_level = %q, want %q (a misleading descriptionAdequacy must never influence the persisted RiskLevel)", riskLevel, review.RiskLevelMedium)
	}
}

// TestPostReviewVerdict_ProposedBodyPresent_EnqueuesDescriptionAutofixOutboxRow
// proves §26.2's own enqueue-time contract: a posted digest.proposedBody
// enqueues exactly one ports.NotificationKindGitHubDescriptionAutofix
// outbox row, alongside (never instead of) the ordinary
// NotificationKindGitHubVerdict row -- carrying owner/repo/prNumber/
// proposedBody, verbatim. This handler performs NO authorship/flag check
// of its own (DescriptionAutofixPayload's own doc comment) -- that is
// entirely the delivering notifier's own job, at delivery time.
func TestPostReviewVerdict_ProposedBodyPresent_EnqueuesDescriptionAutofixOutboxRow(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-autofix-candidate", 14)
	seedDispatchedTurn(ctx, t, rig, session.ID)

	proposedBody := "This PR rewrites the auth token refresh path to retry on transient network failures."
	body := `{
		"riskLevel": "low",
		"premise": "ok",
		"blastRadius": [],
		"filesChanged": 1,
		"testsCoverage": "adequate",
		"docsDrift": "none",
		"proposedShippable": "auto",
		"summary": "Small change, description was stale.",
		"digest": {
			"summary": "Rewrites the auth token refresh path.",
			"descriptionAdequacy": "drift",
			"adequacyExplanation": "The PR body is stale.",
			"proposedBody": "` + proposedBody + `"
		},
		"factCheck": "done",
		"factCheckKilled": 0
	}`

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, body)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var count int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindGitHubDescriptionAutofix)).Scan(&count); err != nil {
		t.Fatalf("count description-autofix outbox rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("description-autofix outbox row count = %d, want 1", count)
	}

	var payloadBytes []byte
	if err := rig.pool.QueryRow(ctx, `SELECT payload FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindGitHubDescriptionAutofix)).Scan(&payloadBytes); err != nil {
		t.Fatalf("query description-autofix outbox payload: %v", err)
	}
	var payload ports.DescriptionAutofixPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("unmarshal description-autofix outbox payload: %v", err)
	}
	if payload.Owner != "acme" || payload.Repo != "verdict-autofix-candidate" {
		t.Errorf("Owner/Repo = %q/%q, want %q/%q", payload.Owner, payload.Repo, "acme", "verdict-autofix-candidate")
	}
	if payload.PRNumber != 14 {
		t.Errorf("PRNumber = %d, want 14", payload.PRNumber)
	}
	if payload.ProposedBody != proposedBody {
		t.Errorf("ProposedBody = %q, want %q", payload.ProposedBody, proposedBody)
	}
	if payload.DescriptionAdequacy != review.DescriptionAdequacyDrift {
		t.Errorf("DescriptionAdequacy = %q, want %q (adversarial-review fix: carried onto the payload verbatim, never re-derived)", payload.DescriptionAdequacy, review.DescriptionAdequacyDrift)
	}

	// The ordinary verdict outbox row must ALSO still be present -- this
	// is an ADDITIVE second row, never a replacement.
	var verdictCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindGitHubVerdict)).Scan(&verdictCount); err != nil {
		t.Fatalf("count verdict outbox rows: %v", err)
	}
	if verdictCount != 1 {
		t.Errorf("verdict outbox row count = %d, want 1", verdictCount)
	}
}

// TestPostReviewVerdict_AdequacyOKWithProposedBody_NeverEnqueuesDescriptionAutofixOutboxRow
// is the adversarial-review fix's own central regression test (item 2,
// HIGH: "nothing gates the description rewrite on adequacy"): a verdict
// reporting descriptionAdequacy "ok" -- an ACCURATE description -- plus a
// non-blank, unsolicited proposedBody (routine over-filling of an
// optional free-text field, never itself a sign the floor fired) must
// enqueue ZERO description-autofix outbox rows. Before this fix, the
// enqueue's ONLY precondition was "proposedBody non-blank", so this exact
// input would have silently queued a write that collapses an already-
// accurate, human-visible description -- on a verdict that had just
// certified it adequate.
func TestPostReviewVerdict_AdequacyOKWithProposedBody_NeverEnqueuesDescriptionAutofixOutboxRow(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-adequacy-ok-candidate", 16)
	seedDispatchedTurn(ctx, t, rig, session.ID)

	body := `{
		"riskLevel": "low",
		"premise": "ok",
		"blastRadius": [],
		"filesChanged": 1,
		"testsCoverage": "adequate",
		"docsDrift": "none",
		"proposedShippable": "auto",
		"summary": "Small change, description is accurate.",
		"digest": {
			"summary": "Rewrites the auth token refresh path.",
			"descriptionAdequacy": "ok",
			"adequacyExplanation": "The PR body already honestly describes the diff.",
			"proposedBody": "An unsolicited stylistic rewrite the agent proposed anyway."
		},
		"factCheck": "done",
		"factCheckKilled": 0
	}`

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, body)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var count int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindGitHubDescriptionAutofix)).Scan(&count); err != nil {
		t.Fatalf("count description-autofix outbox rows: %v", err)
	}
	if count != 0 {
		t.Errorf("description-autofix outbox row count = %d, want 0 (descriptionAdequacy was \"ok\" -- the floor never fired, so a proposedBody must never be enqueued for a real write)", count)
	}

	// The ordinary verdict outbox row must still be present -- this gate is
	// specific to the description-autofix candidate row, never the verdict
	// posting itself.
	var verdictCount int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindGitHubVerdict)).Scan(&verdictCount); err != nil {
		t.Fatalf("count verdict outbox rows: %v", err)
	}
	if verdictCount != 1 {
		t.Errorf("verdict outbox row count = %d, want 1", verdictCount)
	}
}

// TestPostReviewVerdict_ProposedBodyAbsent_NeverEnqueuesDescriptionAutofixOutboxRow
// proves the common case (no rewrite proposed -- validVerdictRequestJSON's
// own digest carries no proposedBody at all) never enqueues the
// description-autofix outbox kind.
func TestPostReviewVerdict_ProposedBodyAbsent_NeverEnqueuesDescriptionAutofixOutboxRow(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-no-autofix-candidate", 15)
	seedDispatchedTurn(ctx, t, rig, session.ID)

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, validVerdictRequestJSON())
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var count int
	if err := rig.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, session.ID, string(ports.NotificationKindGitHubDescriptionAutofix)).Scan(&count); err != nil {
		t.Fatalf("count description-autofix outbox rows: %v", err)
	}
	if count != 0 {
		t.Errorf("description-autofix outbox row count = %d, want 0 (no proposedBody was posted)", count)
	}
}

// seedProcessingTurnWithChangedFilesCount creates a processing turn for
// session carrying reviewHeadSHA (turns.review_head_sha, exactly as
// production turn-creation sets it) and a review_depth_decision JSON blob
// whose own changedFilesCount field is serverComputedChangedFiles --
// mirrors reviewtriage.DecisionRecord's own real wire shape (record.go)
// rather than a hand-typed JSON literal, so a future field rename there
// is caught here at compile time instead of silently producing a JSON
// blob PostReviewVerdict can no longer unmarshal the field it wants out
// of.
func seedProcessingTurnWithChangedFilesCount(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, reviewHeadSHA string, serverComputedChangedFiles int) {
	t.Helper()
	recordJSON, err := json.Marshal(reviewtriage.DecisionRecord{ChangedFilesCount: serverComputedChangedFiles})
	if err != nil {
		t.Fatalf("marshal review depth decision record: %v", err)
	}
	created, err := r.turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID:           sessionID,
		Status:              sqlcgen.TurnStatusProcessing,
		ReviewHeadSha:       &reviewHeadSHA,
		ReviewDepthDecision: recordJSON,
	})
	if err != nil {
		t.Fatalf("seed processing turn with review_depth_decision: %v", err)
	}
	// dispatched_message_id (finding F3 (§21.1's amendment)): stamped here, exactly
	// like seedProcessingDeepPathTurn's own identical re-stamp, so
	// postReviewVerdict's own testDispatchMessageID header resolves THIS
	// turn via the real turns.GetByDispatchedMessageID lookup.
	messageID := testDispatchMessageID
	if _, err := r.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:                  created.ID,
		Status:              sqlcgen.TurnStatusProcessing,
		DispatchedMessageID: &messageID,
	}); err != nil {
		t.Fatalf("stamp dispatched_message_id on seeded turn: %v", err)
	}
}

// filesChangedDriftCanaryLogMsg is reviewverdict.go's own exact log
// message for a fired canary -- named here as a literal (this is package
// httpapi_test, a black-box test package) rather than re-deriving it,
// mirroring wantProseFallback's own identical "named literal for an
// unexported production string" precedent elsewhere in this package's
// tests.
const filesChangedDriftCanaryLogMsg = "httpapi: review-verdict: filesChanged drift canary fired -- self-reported and server-computed changed-file counts diverge beyond both thresholds; diagnostic only, verdict unaffected"

// hasLogEntry is findLogEntry's own non-fatal sibling (planapprove_
// integration_test.go) -- reports whether buf contains a line whose "msg"
// equals wantMsg, without failing the test when it does not (the ABSENCE
// of a log line is itself the fact several of this file's own new tests
// below assert).
func hasLogEntry(t *testing.T, buf *bytes.Buffer, wantMsg string) bool {
	t.Helper()
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		if entry["msg"] == wantMsg {
			return true
		}
	}
	return false
}

// TestPostReviewVerdict_FilesChangedDriftCanary_FiresOnDivergence_NeverAffectsVerdict
// is §21.1's own filesChanged drift canary, wired end to end: a processing
// turn's own review_depth_decision records a server-computed
// changedFilesCount (15) against a self-reported filesChanged (25, this
// test's own request body override -- see driftFiringVerdictRequestJSON's
// own doc comment for why 25/15, not e.g. validVerdictRequestJSON's plain
// default, is deliberately chosen) clearing BOTH reviewverdict.
// FilesChangedDriftRatioThreshold and FilesChangedDriftAbsoluteThreshold
// -- the canary must fire a diagnostic log line, and (§21.1's own first
// load-bearing constraint) the posted verdict, the review_verdicts row it
// persists, and this request's own 201 response must all be COMPLETELY
// unaffected: nothing here is a filter or a gate.
func TestPostReviewVerdict_FilesChangedDriftCanary_FiresOnDivergence_NeverAffectsVerdict(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-drift-fires", 70)
	seedProcessingTurnWithChangedFilesCount(ctx, t, rig, session.ID, "sha-drift-fires", 15)

	buf := captureDefaultLoggerJSON(t)

	status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, driftFiringVerdictRequestJSON())
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d (a fired canary must never fail the request)", status, http.StatusCreated)
	}
	if resp.Shippable != restdtos.PostReviewVerdictResponseShippable(review.ShippableAuto) {
		t.Errorf("Shippable = %q, want %q (a fired canary must never move the shippable classification)", resp.Shippable, review.ShippableAuto)
	}

	entry := findLogEntry(t, buf, filesChangedDriftCanaryLogMsg)
	if got, want := fmt.Sprintf("%v", entry["self_reported_files_changed"]), "25"; got != want {
		t.Errorf("self_reported_files_changed = %v, want %v", got, want)
	}
	if got, want := fmt.Sprintf("%v", entry["server_computed_files_changed"]), "15"; got != want {
		t.Errorf("server_computed_files_changed = %v, want %v", got, want)
	}

	var filesChanged int
	if err := rig.pool.QueryRow(ctx, `SELECT files_changed FROM review_verdicts WHERE repo_full_name = $1 AND pr_number = $2`, "acme/verdict-drift-fires", 70).Scan(&filesChanged); err != nil {
		t.Fatalf("query review_verdicts row: %v", err)
	}
	if filesChanged != 25 {
		t.Errorf("review_verdicts.files_changed = %d, want 25 (the self-reported value, verbatim -- a fired canary must never rewrite it)", filesChanged)
	}
}

// TestPostReviewVerdict_FilesChangedDriftCanary_NoDivergence_NeverFires is
// the fired test's own negative-space sibling: a server-computed count
// EQUAL to the self-reported one never logs the canary line at all.
func TestPostReviewVerdict_FilesChangedDriftCanary_NoDivergence_NeverFires(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-drift-quiet", 71)
	// validVerdictRequestJSON's own filesChanged is 3 -- matching it here
	// exactly so there is nothing to diverge on.
	seedProcessingTurnWithChangedFilesCount(ctx, t, rig, session.ID, "sha-drift-quiet", 3)

	buf := captureDefaultLoggerJSON(t)

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, validVerdictRequestJSON())
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	if hasLogEntry(t, buf, filesChangedDriftCanaryLogMsg) {
		t.Errorf("filesChanged drift canary fired with no real divergence (self-reported and server-computed both 3); full log:\n%s", buf.String())
	}
}

// TestPostReviewVerdict_FilesChangedDriftCanary_ServerComputedZero_NeverFires
// is §21.1's own second load-bearing constraint, proven end to end: a
// processing turn whose own review_depth_decision was never set at all
// (no ReviewDepthDecision passed to turns.Create, mirroring a turn that
// predates this field, or one whose own marshal step failed at creation
// time) leaves serverComputedChangedFiles at its honest zero value --
// indistinguishable from a genuinely failed GetPullRequest fetch
// (review.PreFetchedContext.ChangedFilesCount's own doc comment) -- and
// the canary must NEVER read that as "confidently zero, so any self-
// report is 100% drift." Deliberately posts a self-reported filesChanged
// of 3 (validVerdictRequestJSON's own default) against a real, wildly
// different server-computed value that would otherwise obviously fire,
// were it not for the zero-guard: this test does not control
// serverComputedChangedFiles at all (no seedProcessingTurnWithChangedFilesCount
// call), which is the point -- it is 0 precisely because it was never
// recorded.
func TestPostReviewVerdict_FilesChangedDriftCanary_ServerComputedZero_NeverFires(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-drift-zero-guard", 72)
	reviewHeadSHA := "sha-drift-zero-guard"
	// Deliberately no ReviewDepthDecision -- review_depth_decision stays
	// SQL NULL, exactly like a turn that predates this field.
	createdTurn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: session.ID, Status: sqlcgen.TurnStatusProcessing, ReviewHeadSha: &reviewHeadSHA})
	if err != nil {
		t.Fatalf("seed processing turn with no review_depth_decision: %v", err)
	}
	turnMessageID := testDispatchMessageID
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: createdTurn.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedMessageID: &turnMessageID}); err != nil {
		t.Fatalf("stamp dispatched_message_id on seeded turn: %v", err)
	}

	buf := captureDefaultLoggerJSON(t)

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, validVerdictRequestJSON())
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	if hasLogEntry(t, buf, filesChangedDriftCanaryLogMsg) {
		t.Errorf("filesChanged drift canary fired against an unset (zero) server-computed count -- must be treated as no reliable signal, never as a real zero; full log:\n%s", buf.String())
	}
}

// seedProcessingTurnWithUndeliveredDiff mirrors
// seedProcessingTurnWithChangedFilesCount, but ALSO sets DiffEmpty/
// DiffTruncated on the seeded review_depth_decision record -- D4's own
// (adversarial review of PR #182, MEDIUM) end-to-end fixture: a
// processing turn whose diff was never fully delivered to the reviewing
// agent, used below to prove FilesChangedDrifted's own diffDelivered
// guard actually suppresses the canary through the FULL httpapi wiring
// (the unmarshal, the diffDelivered computation, and the call site) --
// driftcanary_test.go already covers the pure function in isolation, this
// proves the wiring around it too.
func seedProcessingTurnWithUndeliveredDiff(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, reviewHeadSHA string, serverComputedChangedFiles int, diffEmpty, diffTruncated bool) {
	t.Helper()
	recordJSON, err := json.Marshal(reviewtriage.DecisionRecord{ChangedFilesCount: serverComputedChangedFiles, DiffEmpty: diffEmpty, DiffTruncated: diffTruncated})
	if err != nil {
		t.Fatalf("marshal review depth decision record: %v", err)
	}
	created, err := r.turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID:           sessionID,
		Status:              sqlcgen.TurnStatusProcessing,
		ReviewHeadSha:       &reviewHeadSHA,
		ReviewDepthDecision: recordJSON,
	})
	if err != nil {
		t.Fatalf("seed processing turn with undelivered-diff review_depth_decision: %v", err)
	}
	// dispatched_message_id (finding F3 (§21.1's amendment)): see
	// seedProcessingTurnWithChangedFilesCount's own identical re-stamp.
	messageID := testDispatchMessageID
	if _, err := r.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:                  created.ID,
		Status:              sqlcgen.TurnStatusProcessing,
		DispatchedMessageID: &messageID,
	}); err != nil {
		t.Fatalf("stamp dispatched_message_id on seeded turn: %v", err)
	}
}

// TestPostReviewVerdict_FilesChangedDriftCanary_DiffTruncated_NeverFires is
// D4's own (adversarial review of PR #182, MEDIUM) end-to-end fix: the
// EXACT SAME 25-vs-15 divergence as TestPostReviewVerdict_
// FilesChangedDriftCanary_FiresOnDivergence_NeverAffectsVerdict above --
// which clears both thresholds and fires there -- must NOT fire here,
// because this turn's own review_depth_decision also records
// DiffTruncated=true. The reviewing agent was handed only a partial diff
// (with an explicit truncation notice, review.RenderTurnPrompt's own
// doc comment), so its own under-count is the CORRECT, honest
// consequence of what it actually saw -- not evidence of a skipped
// review, and this canary must never blame it for a server-side delivery
// failure that was never its own fault.
func TestPostReviewVerdict_FilesChangedDriftCanary_DiffTruncated_NeverFires(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-drift-diff-truncated", 73)
	seedProcessingTurnWithUndeliveredDiff(ctx, t, rig, session.ID, "sha-drift-truncated", 15, false, true)

	buf := captureDefaultLoggerJSON(t)

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, driftFiringVerdictRequestJSON())
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	if hasLogEntry(t, buf, filesChangedDriftCanaryLogMsg) {
		t.Errorf("filesChanged drift canary fired against a TRUNCATED diff -- the reviewer was never handed the full diff, so its own count divergence is not evidence of a skipped review; full log:\n%s", buf.String())
	}
}

// TestPostReviewVerdict_FilesChangedDriftCanary_DiffEmpty_NeverFires is
// TestPostReviewVerdict_FilesChangedDriftCanary_DiffTruncated_NeverFires's
// own sibling for the OTHER diff-not-delivered case (D4): the diff fetch
// itself never even succeeded (GetCompareDiff failed, review.
// PreFetchedContext.Diff == ""), so review.RenderTurnPrompt rendered no
// diff block at all -- the reviewing agent had nothing to count files
// against, and its own self-report diverging from the server-computed
// count is, again, not evidence of anything this canary should surface.
func TestPostReviewVerdict_FilesChangedDriftCanary_DiffEmpty_NeverFires(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-drift-diff-empty", 74)
	seedProcessingTurnWithUndeliveredDiff(ctx, t, rig, session.ID, "sha-drift-empty", 15, true, false)

	buf := captureDefaultLoggerJSON(t)

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, driftFiringVerdictRequestJSON())
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	if hasLogEntry(t, buf, filesChangedDriftCanaryLogMsg) {
		t.Errorf("filesChanged drift canary fired when the diff was never delivered at all (empty); full log:\n%s", buf.String())
	}
}

// deepPathVerdictRequestJSON is validVerdictRequestJSON's own deep-path
// sibling (§26.4/§7.1): the three deep-path-only digest fields
// (§26.3 -- archDecisions/stackRisks/unverifiedLimits, all
// application-level required whenever this session's own resolved
// review-depth is deep) are populated, and counterReview is set to
// whatever the caller passes -- the minimal legal deep-path body every
// post-hoc-corroboration test below starts from.
func deepPathVerdictRequestJSON(counterReview string) string {
	return `{
		"riskLevel": "low",
		"premise": "ok",
		"blastRadius": [],
		"filesChanged": 3,
		"testsCoverage": "adequate",
		"docsDrift": "none",
		"proposedShippable": "auto",
		"summary": "Looks good overall, one minor nit below.",
		"digest": {
			"summary": "Adds a retry helper around the flaky upstream call and swaps every existing call site onto it.",
			"descriptionAdequacy": "ok",
			"adequacyExplanation": "The PR body accurately describes the retry helper this diff adds.",
			"archDecisions": [{"decision": "Reused the existing retry helper rather than writing a new one."}],
			"stackRisks": "None of note -- purely additive.",
			"unverifiedLimits": "Did not run against production traffic."
		},
		"factCheck": "done",
		"factCheckKilled": 0,
		"counterReview": "` + counterReview + `"
	}`
}

// seedProcessingDeepPathTurn (§26.4/§7.1) creates a processing
// turn on sessionID with review_depth "deep" (§26.3) and
// dispatched_sandbox_gen/dispatched_at stamped -- the SAME gen a real
// sandbox row created by createSandboxWithToken starts at (1, matching
// every other test in this file that posts X-Sandbox-Gen: "1"). This is
// the fixture every post-hoc-corroboration test below needs: a turn whose
// own dispatched_sandbox_gen AND dispatched_at are what
// corroborateCounterReview's own queries (ListSubTaskStartsForTurn/
// ListSubTaskFinishesForTurn) actually filter on.
//
// Two dispatch columns are stamped here, and only ONE of them is what the
// corroboration queries actually filter on:
//
//   - dispatched_event_id -- the events-log high-water mark AS OF THIS
//     CALL, i.e. before the caller seeds any sub-task events of its own.
//     This is the real bound (`id > dispatched_event_id`,
//     queries/events.sql), so every event a caller seeds afterwards gets a
//     strictly higher BIGSERIAL id and lands in this turn's own scope.
//     TestSeedProcessingDeepPathTurn_StampsWatermarkBelowItsOwnEvents pins
//     exactly that ordering, because stamping it at the wrong moment would
//     silently exclude the trace the positive tests assert on.
//
//   - dispatched_at -- still stamped, because the column still exists and
//     still has a genuine consumer (turn.EvaluateTurnDeadline, which
//     compares it against the application's own time.Now()), but NO LONGER
//     read by corroboration at all.
//
// dispatched_at is sourced from the DATABASE's clock (now() - a
// seedDispatchBackdate cushion) rather than this test process's time.Now().
// That was originally load-bearing: the bound used to be `created_at >=
// dispatched_at`, comparing a Postgres-stamped events.created_at against a
// Go-stamped turns.dispatched_at, with the whole safety margin being the
// ~1-3ms elapsed between this UPDATE and the caller's own subsequent
// INSERT. A containerized DB clock a few ms behind the host's inverted the
// comparison and silently emptied both queries -- a non-deterministic,
// wrong-computed-value flake that could only ever hit the POSITIVE tests,
// since dropping events moves a verdict toward needs_human only. Migration
// 000089 removed that clock from the comparison entirely; the DB-clock
// sourcing is kept here anyway, as the honest source for a column whose
// sibling values all come from the database.
//
// The backdate cushion additionally models what a real dispatch looks
// like: tryPlanDispatch (dispatch.go) stamps these columns when the prompt
// is SENT, and a real counter-reviewer sub-task then starts and finishes
// seconds-to-minutes later.
func seedProcessingDeepPathTurn(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, reviewHeadSHA string, gen int32) sqlcgen.Turn {
	t.Helper()
	deepDepth := string(reviewtriage.DepthDeep)
	created, err := r.turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID:     sessionID,
		Status:        sqlcgen.TurnStatusProcessing,
		ReviewHeadSha: &reviewHeadSHA,
		ReviewDepth:   &deepDepth,
	})
	if err != nil {
		t.Fatalf("seed processing deep-path turn: %v", err)
	}
	// The events-log high-water mark AS OF THIS MOMENT -- before any
	// caller seeds its own sub_task_start/finish events, exactly as a real
	// dispatch stamps it before the sub-task activity it later corroborates
	// against exists. This is the bound the corroboration queries actually
	// filter on (migrations/000089_turns_dispatched_event_id.up.sql); every
	// event a caller seeds after this call gets a strictly higher id and is
	// therefore in scope, with no clock involved on either side.
	watermark, err := r.events.MaxEventIDForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("read events high-water mark for seeded deep-path turn: %v", err)
	}
	messageID := testDispatchMessageID
	updated, err := r.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:                   created.ID,
		Status:               sqlcgen.TurnStatusProcessing,
		DispatchedAt:         pgtype.Timestamptz{Time: dbDispatchedAt(ctx, t, r), Valid: true},
		DispatchedSandboxGen: &gen,
		DispatchedEventID:    &watermark,
		// dispatched_message_id (finding F3 (§21.1's amendment)) mirrors gen/
		// event-id's own identical "stamped at the moment the prompt was
		// (would be) dispatched" convention -- postReviewVerdict's own
		// callers present testDispatchMessageID back as a request header,
		// so PostReviewVerdict's real turns.GetByDispatchedMessageID
		// lookup resolves THIS exact turn, never session-wide "current"
		// status.
		DispatchedMessageID: &messageID,
	})
	if err != nil {
		t.Fatalf("stamp dispatch columns on seeded deep-path turn: %v", err)
	}
	return updated
}

// seedDispatchBackdate is how far before the database's own now()
// seedProcessingDeepPathTurn stamps dispatched_at. It models a real
// dispatch's lead time over the sub-task activity that follows it (see
// that helper's own doc comment). It is far smaller than the ±24h offsets
// TestPostReviewVerdict_CounterReviewCorroborated_
// EarlierTurnSameGenDoesNotCorroborate uses to separate its two turns, so
// it cannot perturb that test's own ordering.
const seedDispatchBackdate = 5 * time.Second

// dbDispatchedAt returns the DATABASE's own current time, backdated by
// seedDispatchBackdate -- the single-clock source seedProcessingDeepPathTurn
// stamps dispatched_at from, so that `created_at >= dispatched_at` never
// straddles the host and container clocks. See seedProcessingDeepPathTurn's
// own doc comment for the flake this closes.
func dbDispatchedAt(ctx context.Context, t *testing.T, r testRig) time.Time {
	t.Helper()
	var dbNow time.Time
	if err := r.pool.QueryRow(ctx, "SELECT now()").Scan(&dbNow); err != nil {
		t.Fatalf("read database clock for dispatched_at: %v", err)
	}
	return dbNow.Add(-seedDispatchBackdate)
}

// seedSubTaskStart/seedSubTaskFinish (§26.4/§7.1) persist a REAL
// sub_task_start/sub_task_finish event row via r.events -- the SAME
// EventStore.Create path sessionactor's own appendRawEvent uses in
// production (sandboxevent.go) to persist every recognized sandbox event
// unconditionally -- so these tests exercise corroborateCounterReview's
// own two queries against genuinely persisted rows, decoded back out of
// real JSONB, never a hand-built in-memory fixture. The payload shapes
// mirror contracts/sandbox-ws/v1/events.schema.json's own SubTaskStart/
// SubTaskFinish defs field-for-field (including fields this call site
// never reads, like label/parentMessageId/ackId, so a real producer's
// payload is not the ARTIFICIALLY minimal shape a narrower fixture might
// tempt one into writing).
func seedSubTaskStart(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, messageID, subTaskID, subAgentType string, gen int32) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"type":            "sub_task_start",
		"messageId":       messageID,
		"sessionId":       sessionID.String(),
		"gen":             gen,
		"subTaskId":       subTaskID,
		"label":           "test sub-task",
		"parentMessageId": "msg_parent",
		"subAgentType":    subAgentType,
	})
	if err != nil {
		t.Fatalf("marshal sub_task_start payload: %v", err)
	}
	if _, err := r.events.Create(ctx, sqlcgen.CreateEventParams{SessionID: sessionID, Type: "sub_task_start", MessageID: messageID, Payload: payload}); err != nil {
		t.Fatalf("persist sub_task_start event: %v", err)
	}
}

func seedSubTaskFinish(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID, messageID, subTaskID, outcome string, gen int32) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"type":      "sub_task_finish",
		"messageId": messageID,
		"sessionId": sessionID.String(),
		"gen":       gen,
		"ackId":     "sub_task_finish:" + messageID,
		"subTaskId": subTaskID,
		"outcome":   outcome,
	})
	if err != nil {
		t.Fatalf("marshal sub_task_finish payload: %v", err)
	}
	if _, err := r.events.Create(ctx, sqlcgen.CreateEventParams{SessionID: sessionID, Type: "sub_task_finish", MessageID: messageID, Payload: payload}); err != nil {
		t.Fatalf("persist sub_task_finish event: %v", err)
	}
}

// TestPostReviewVerdict_CounterReviewCorroborated_NotFloored (
// §26.4/§7.1) is this Step's own primary positive case, exercising the
// FULL path end to end against real Postgres: a real sub_task_start
// (subAgentType "counter-reviewer") + sub_task_finish (outcome
// "completed") pair, persisted at the SAME gen the turn was dispatched
// at, backing a deep-path verdict that self-reports counterReview: done.
// Shippable must NOT be floored -- the corroborated claim keeps whatever
// CounterReviewDone's own floor already gives on this otherwise-clean
// input (auto).
func TestPostReviewVerdict_CounterReviewCorroborated_NotFloored(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-corroborated", 80)
	seedProcessingDeepPathTurn(ctx, t, rig, session.ID, "sha-corroborated", 1)
	seedSubTaskStart(ctx, t, rig, session.ID, "msg-start-1", "subtask-1", review.CounterReviewerAgentName, 1)
	seedSubTaskFinish(ctx, t, rig, session.ID, "msg-finish-1", "subtask-1", "completed", 1)

	status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, deepPathVerdictRequestJSON("done"))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if resp.Shippable != restdtos.PostReviewVerdictResponseShippableAuto {
		t.Errorf("Shippable = %q, want %q (a corroborated counter-review claim must not be floored)", resp.Shippable, restdtos.PostReviewVerdictResponseShippableAuto)
	}
}

// TestPostReviewVerdict_CounterReviewUncorroborated_NoFinishEvent_
// FloorsToNeedsHuman (§26.4) is the central negative case: a
// counter-reviewer sub_task_start was persisted, but no matching
// sub_task_finish at all -- the sub-task never (verifiably) completed.
// The self-report still claims counterReview: done, but with nothing in
// the persisted trace to back it up, Shippable must be floored to
// needs_human, exactly as an honest "skipped" self-report already would.
func TestPostReviewVerdict_CounterReviewUncorroborated_NoFinishEvent_FloorsToNeedsHuman(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-uncorroborated-no-finish", 81)
	seedProcessingDeepPathTurn(ctx, t, rig, session.ID, "sha-uncorroborated-no-finish", 1)
	seedSubTaskStart(ctx, t, rig, session.ID, "msg-start-2", "subtask-2", review.CounterReviewerAgentName, 1)
	// Deliberately no matching sub_task_finish event at all.

	status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, deepPathVerdictRequestJSON("done"))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if resp.Shippable != restdtos.PostReviewVerdictResponseShippableNeedsHuman {
		t.Errorf("Shippable = %q, want %q (a claimed-but-uncorroborated done must floor to needs_human)", resp.Shippable, restdtos.PostReviewVerdictResponseShippableNeedsHuman)
	}
}

// TestPostReviewVerdict_CounterReviewUncorroborated_OnlyDifferentSubAgent
// Type_FloorsToNeedsHuman (§26.4) is the false-positive guard: the ONLY
// sub-task this session's own trace shows is a DIFFERENT named sub-agent
// (fact-check, §26.6) that genuinely started and completed -- proving
// corroborateCounterReview/reviewverdict.CounterReviewCorroborated find
// the RIGHT sub-agent's own trace, not merely "some completed sub-task
// exists somewhere in this session". Shippable must still floor to
// needs_human.
func TestPostReviewVerdict_CounterReviewUncorroborated_OnlyDifferentSubAgentType_FloorsToNeedsHuman(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-uncorroborated-wrong-agent", 82)
	seedProcessingDeepPathTurn(ctx, t, rig, session.ID, "sha-uncorroborated-wrong-agent", 1)
	seedSubTaskStart(ctx, t, rig, session.ID, "msg-start-3", "subtask-3", "fact-check", 1)
	seedSubTaskFinish(ctx, t, rig, session.ID, "msg-finish-3", "subtask-3", "completed", 1)

	status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, deepPathVerdictRequestJSON("done"))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if resp.Shippable != restdtos.PostReviewVerdictResponseShippableNeedsHuman {
		t.Errorf("Shippable = %q, want %q (a different sub-agent's own completed trace must never corroborate the counter-reviewer's own claim)", resp.Shippable, restdtos.PostReviewVerdictResponseShippableNeedsHuman)
	}
}

// TestPostReviewVerdict_CounterReviewCorroborated_MultipleSubAgentTypes_
// NotFloored (§26.4/§7.1; adversarial-review LOW finding) closes
// an integration-level gap: before this test, only reviewverdict.
// CounterReviewCorroborated's own pure-function test (corroboration_test.
// go, "multiple unrelated sub-tasks alongside the real counter-reviewer
// pair") covered a session+gen whose trace carries MORE THAN ONE
// sub-agent type -- the real gen-scoped Postgres queries
// (ListSubTaskStartEventsForTurn/ListSubTaskFinishEventsForTurn) were
// never exercised against that exact shape at this level. Seeds a
// genuine, completed fact-check sub-task pair ALONGSIDE the real
// counter-reviewer pair, both at the SAME turn/gen, and confirms
// Shippable is still NOT floored: corroborateCounterReview's own real I/O
// path correctly finds the counter-reviewer's own trace among several,
// not merely "some completed sub-task exists in this scope".
func TestPostReviewVerdict_CounterReviewCorroborated_MultipleSubAgentTypes_NotFloored(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-corroborated-multi-agent", 83)
	seedProcessingDeepPathTurn(ctx, t, rig, session.ID, "sha-corroborated-multi-agent", 1)
	seedSubTaskStart(ctx, t, rig, session.ID, "msg-start-fact-check", "subtask-fact-check", "fact-check", 1)
	seedSubTaskFinish(ctx, t, rig, session.ID, "msg-finish-fact-check", "subtask-fact-check", "completed", 1)
	seedSubTaskStart(ctx, t, rig, session.ID, "msg-start-counter", "subtask-counter", review.CounterReviewerAgentName, 1)
	seedSubTaskFinish(ctx, t, rig, session.ID, "msg-finish-counter", "subtask-counter", "completed", 1)

	status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, deepPathVerdictRequestJSON("done"))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if resp.Shippable != restdtos.PostReviewVerdictResponseShippableAuto {
		t.Errorf("Shippable = %q, want %q (a real counter-reviewer trace alongside an unrelated fact-check trace, same turn/gen, must still corroborate)", resp.Shippable, restdtos.PostReviewVerdictResponseShippableAuto)
	}
}

// TestSeedProcessingDeepPathTurn_StampsWatermarkBelowItsOwnEvents (
// §26.4/§7.1) guards the fixture invariant the corroboration queries now
// depend on: a seeded turn's dispatched_event_id must sit strictly BELOW
// every event that turn goes on to seed, so those events are in its own
// scope.
//
// This replaces an earlier clock-cushion assertion. That test existed
// because the bound used to be `created_at >= dispatched_at`, which
// straddled the Postgres server clock and the test process's own -- a few
// ms of ordinary drift silently emptied both queries and floored the
// verdict. The bound is now `id > dispatched_event_id` over a BIGSERIAL
// assigned by the one database
// (migrations/000089_turns_dispatched_event_id.up.sql), so there is no
// longer a clock on either side of it and no cushion left to protect;
// what CAN still go wrong is a fixture that stamps the watermark at the
// wrong moment -- after seeding its events rather than before -- which
// would silently exclude the very trace the positive tests assert on.
// That is what this pins.
func TestSeedProcessingDeepPathTurn_StampsWatermarkBelowItsOwnEvents(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-watermark", 85)
	seedProcessingDeepPathTurn(ctx, t, rig, session.ID, "sha-watermark", 1)
	seedSubTaskStart(ctx, t, rig, session.ID, "msg-start-watermark", "subtask-watermark", review.CounterReviewerAgentName, 1)
	seedSubTaskFinish(ctx, t, rig, session.ID, "msg-finish-watermark", "subtask-watermark", "completed", 1)

	turnRow, err := rig.turns.GetProcessingTurnForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("get processing turn: %v", err)
	}
	if turnRow.DispatchedEventID == nil {
		t.Fatal("seeded turn has a NULL dispatched_event_id: corroboration would take the fail-conservative NULL path, and every positive test above would pass or fail for a reason unrelated to what it asserts")
	}

	// Every event this turn seeded must be strictly above its watermark.
	var minSeededID int64
	if err := rig.pool.QueryRow(ctx, `
		SELECT MIN(id) FROM events
		 WHERE session_id = $1 AND type IN ('sub_task_start', 'sub_task_finish')`,
		session.ID).Scan(&minSeededID); err != nil {
		t.Fatalf("read lowest seeded sub-task event id: %v", err)
	}
	if minSeededID <= *turnRow.DispatchedEventID {
		t.Errorf("lowest seeded sub-task event id %d is at or below the turn's own watermark %d, so this turn's own trace falls outside its own corroboration scope", minSeededID, *turnRow.DispatchedEventID)
	}

	// The REAL production query, at the seeded gen and watermark, finds it.
	starts, err := rig.events.ListSubTaskStartsForTurn(ctx, session.ID, *turnRow.DispatchedSandboxGen, *turnRow.DispatchedEventID)
	if err != nil {
		t.Fatalf("ListSubTaskStartsForTurn: %v", err)
	}
	if len(starts) == 0 {
		t.Error("ListSubTaskStartsForTurn returned no rows for a freshly seeded counter-reviewer sub_task_start: the id > dispatched_event_id bound excluded this turn's own event")
	}

	// ...and the bound is genuinely load-bearing, not incidentally
	// satisfied: raising the watermark above those same events excludes
	// them. Without this, the assertion above would still pass against a
	// query that had quietly stopped filtering at all.
	raised := minSeededID
	excluded, err := rig.events.ListSubTaskStartsForTurn(ctx, session.ID, *turnRow.DispatchedSandboxGen, raised)
	if err != nil {
		t.Fatalf("ListSubTaskStartsForTurn (raised watermark): %v", err)
	}
	if len(excluded) != 0 {
		t.Errorf("raising the watermark to %d still returned %d sub_task_start row(s): the id > dispatched_event_id bound is not actually filtering", raised, len(excluded))
	}
}

// TestPostReviewVerdict_CounterReviewCorroborated_EarlierTurnSameGenDoesNot
// Corroborate (§26.4/§7.1) reproduces the EXACT bug an
// adversarial review of this PR found, and this same commit fixes: gen-
// scoping ALONE cannot distinguish two DIFFERENT turns on the SAME
// session dispatched to the SAME still-live sandbox incarnation, because
// tryPlanDispatch (internal/app/sessionactor/dispatch.go) stamps
// turns.dispatched_sandbox_gen with the sandbox's CURRENT gen VERBATIM,
// no bump, whenever a turn is dispatched to an already-Ready/Suspect
// sandbox -- gen is bumped ONLY on a fresh spawn/restore/resume. This is
// the ORDINARY case §24's automatic re-review on new commits already
// describes as normal, not a rare corner case: a session's sandbox
// routinely survives across multiple review turns.
//
// Setup: turn 1 is a genuine earlier deep-path review turn, dispatched
// (dispatched_sandbox_gen=1, dispatched_at stamped deliberately WELL IN
// THE PAST) with a REAL, genuine, completed counter-reviewer
// sub_task_start/sub_task_finish pair persisted for it, then completes
// (freeing turns_one_processing_per_session's own slot). Turn 2 (the SAME
// session) is then dispatched to the SAME sandbox incarnation --
// dispatched_sandbox_gen is ALSO 1, identical to turn 1's, and
// dispatched_at is stamped deliberately WELL IN THE FUTURE relative to
// when turn 1's own trace was persisted. Turn 2's own self-report claims
// counterReview: done, but NO sub-task trace of its own is ever
// persisted for it.
//
// Before this Step's fix (gen-scoping alone, no created_at bound):
// corroborateCounterReview's query would match turn 1's OLD
// sub_task_start/finish rows purely on (session_id, gen) -- exactly the
// SAME gen turn 2 was ALSO dispatched at -- wrongly corroborating turn
// 2's fabricated self-report and leaving Shippable at its permissive
// "auto" value: this exact test fails without the fix. After the fix
// (gen AND created_at >= this turn's own dispatched_at): turn 1's events
// all have created_at strictly BEFORE turn 2's own dispatched_at, so they
// are correctly excluded, corroboration returns false, and Shippable
// floors to needs_human -- exactly the unverified-self-report bypass this
// whole Step exists to close.
func TestPostReviewVerdict_CounterReviewCorroborated_EarlierTurnSameGenDoesNotCorroborate(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := setupReviewSessionWithSandbox(ctx, t, rig, "acme/verdict-earlier-turn-same-gen", 84)

	var sameGen int32 = 1
	turn1DispatchedAt := time.Now().Add(-24 * time.Hour)
	turn2DispatchedAt := time.Now().Add(24 * time.Hour)
	deepDepth := string(reviewtriage.DepthDeep)
	turn1SHA := "sha-turn-1-same-gen"
	turn2SHA := "sha-turn-2-same-gen"

	// Turn 1: dispatched to sandbox gen 1, dispatched_at deliberately far
	// in the past.
	turn1, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID:     session.ID,
		Status:        sqlcgen.TurnStatusProcessing,
		ReviewHeadSha: &turn1SHA,
		ReviewDepth:   &deepDepth,
	})
	if err != nil {
		t.Fatalf("create turn 1: %v", err)
	}
	// Turn 1's own watermark, read BEFORE its own trace is seeded below --
	// so turn 1's events are genuinely in ITS own scope, exactly as a real
	// dispatch would leave them.
	turn1Watermark, err := rig.events.MaxEventIDForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("read turn 1 watermark: %v", err)
	}
	// turn1MessageID (finding F3 (§21.1's amendment)) is deliberately DISTINCT from
	// turn2's own (below) -- this is the EXACT hazard this test's own
	// name describes, one layer up: two turns sharing the same session AND
	// gen must still be individually addressable, now by
	// dispatched_message_id rather than by "whichever is processing" --
	// see this test's own top doc comment for the identical gen-scoping
	// gap this closes for counter-review corroboration.
	turn1MessageID := "test-dispatch-message-id-turn-1"
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:                   turn1.ID,
		Status:               sqlcgen.TurnStatusProcessing,
		DispatchedAt:         pgtype.Timestamptz{Time: turn1DispatchedAt, Valid: true},
		DispatchedSandboxGen: &sameGen,
		DispatchedEventID:    &turn1Watermark,
		DispatchedMessageID:  &turn1MessageID,
	}); err != nil {
		t.Fatalf("stamp turn 1 dispatch columns: %v", err)
	}

	// Turn 1's OWN real, genuine counter-reviewer trace -- persisted
	// while turn 1 was the live processing turn, well before turn 2 is
	// ever dispatched below.
	seedSubTaskStart(ctx, t, rig, session.ID, "msg-start-turn1-same-gen", "subtask-turn1-same-gen", review.CounterReviewerAgentName, sameGen)
	seedSubTaskFinish(ctx, t, rig, session.ID, "msg-finish-turn1-same-gen", "subtask-turn1-same-gen", "completed", sameGen)

	// Turn 1 completes -- freeing turns_one_processing_per_session's own
	// partial-unique-index slot for turn 2, mirroring how a real turn
	// genuinely finishes before the next one on the same session starts.
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:          turn1.ID,
		Status:      sqlcgen.TurnStatusCompleted,
		CompletedAt: pgtype.Timestamptz{Time: turn1DispatchedAt.Add(time.Minute), Valid: true},
	}); err != nil {
		t.Fatalf("complete turn 1: %v", err)
	}

	// Turn 2: a LATER re-review turn on the SAME session (§24's "once
	// deep, stays deep" floor forces this to deep path too), dispatched
	// to the SAME sandbox incarnation -- dispatched_sandbox_gen is ALSO
	// 1, identical to turn 1's, because the sandbox never respawned
	// between the two turns. dispatched_at is stamped deliberately far in
	// the FUTURE relative to turn 1's own persisted trace above.
	// Deliberately NO sub_task_start/finish events are ever persisted for
	// turn 2 -- its own self-report below is entirely unverified.
	turn2, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID:     session.ID,
		Status:        sqlcgen.TurnStatusProcessing,
		ReviewHeadSha: &turn2SHA,
		ReviewDepth:   &deepDepth,
	})
	if err != nil {
		t.Fatalf("create turn 2: %v", err)
	}
	// Turn 2's own watermark, read now -- i.e. AFTER turn 1's genuine trace
	// was already persisted above, exactly as a real later dispatch on the
	// same session would see it. This is the value that must exclude turn
	// 1's events: every one of them has an id at or below it.
	//
	// Stamping this is what keeps this test HONEST. Left NULL, the handler
	// would refuse to corroborate on the fail-conservative NULL path and
	// this test would still report needs_human -- passing for a reason that
	// has nothing to do with the cross-turn bound it exists to guard.
	turn2Watermark, err := rig.events.MaxEventIDForSession(ctx, session.ID)
	if err != nil {
		t.Fatalf("read turn 2 watermark: %v", err)
	}
	if turn2Watermark <= turn1Watermark {
		t.Fatalf("turn 2 watermark %d must be strictly above turn 1's %d, otherwise this test cannot distinguish the two turns' traces at all", turn2Watermark, turn1Watermark)
	}
	// turn2MessageID is deliberately testDispatchMessageID -- the SAME
	// value the postReviewVerdict call below presents as its own header,
	// so the real turns.GetByDispatchedMessageID lookup resolves TURN 2
	// specifically (never turn 1, even though both share session_id AND
	// gen) -- proving this test's own real point end to end: which turn's
	// context/corroboration-scope applies is now decided by this
	// identifier, not by which turn happens to be 'processing'.
	turn2MessageID := testDispatchMessageID
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:                   turn2.ID,
		Status:               sqlcgen.TurnStatusProcessing,
		DispatchedAt:         pgtype.Timestamptz{Time: turn2DispatchedAt, Valid: true},
		DispatchedSandboxGen: &sameGen,
		DispatchedEventID:    &turn2Watermark,
		DispatchedMessageID:  &turn2MessageID,
	}); err != nil {
		t.Fatalf("stamp turn 2 dispatch columns: %v", err)
	}

	status, resp := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, deepPathVerdictRequestJSON("done"))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if resp.Shippable != restdtos.PostReviewVerdictResponseShippableNeedsHuman {
		t.Errorf("Shippable = %q, want %q (turn 2's fabricated counterReview:done self-report must NOT be corroborated by turn 1's own OLD trace, even though both turns share the identical dispatched_sandbox_gen)", resp.Shippable, restdtos.PostReviewVerdictResponseShippableNeedsHuman)
	}
}

// TestPostReviewVerdict_TimedOutTurnStillPostsAgainstOwnContext_NotTheNewerTurn
// is §21.1's amendment's own DECISIVE finding F3 regression test, reproducing the
// EXACT scenario finding F3 names: turn A is dispatched (and its own
// review context, including a distinct base ref and head sha, is
// recorded), then exceeds TurnDeadline and is marked 'failed' -- but its
// own agent process is still alive in the same sandbox (this codebase's
// own documented sandbox-lifetime gap: a timeout does not forcibly kill
// the sandbox). A queued retrigger then dispatches turn B on the SAME
// session, to the SAME sandbox incarnation (no respawn, same
// X-Sandbox-Gen), which becomes the new 'processing' row. Turn A's own
// agent -- having rendered its OWN prompt, carrying its OWN dispatch
// message id, before it ever knew it would time out -- finally posts.
//
// This test FAILS against the pre-fix code: GetProcessingTurnForSession
// resolves "whichever turn is processing right now" -- turn B -- so the
// posted verdict would be silently stamped with turn B's own head_sha/
// base_ref/base_sha, even though the verdict was genuinely produced
// against turn A's own commit. The fix resolves the turn by the SAME
// dispatch message id turn A's own prompt carried
// (turns.GetByDispatchedMessageID), regardless of turn A's own CURRENT
// status -- so this verdict is correctly attributed to turn A, never to
// the newer, unrelated turn B.
func TestPostReviewVerdict_TimedOutTurnStillPostsAgainstOwnContext_NotTheNewerTurn(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	repoFullName := "acme/verdict-timedout-turn-a"
	session := setupReviewSessionWithSandbox(ctx, t, rig, repoFullName, 90)

	sameGen := int32(1)
	const turnAMessageID = "turn-a-own-dispatch-message-id"
	const turnBMessageID = "turn-b-own-dispatch-message-id"

	turnAHeadSHA := "sha-turn-a-examined-this-commit"
	turnAContext, err := json.Marshal(reviewverdict.Context{BaseRef: "main", BaseSHA: "base-sha-turn-a", PolicyVersion: 1})
	if err != nil {
		t.Fatalf("marshal turn A context: %v", err)
	}

	// Turn A: dispatched, given its own real review context, then
	// TIMES OUT (marked 'failed') -- exactly like turn.DeriveFailureReason
	// forwards TriggerTimeout in production -- while its own agent is
	// still alive and has not yet posted.
	turnA, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID:            session.ID,
		Status:               sqlcgen.TurnStatusProcessing,
		ReviewHeadSha:        &turnAHeadSHA,
		ReviewVerdictContext: turnAContext,
	})
	if err != nil {
		t.Fatalf("create turn A: %v", err)
	}
	turnAMsgID := turnAMessageID
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:                   turnA.ID,
		Status:               sqlcgen.TurnStatusProcessing,
		DispatchedSandboxGen: &sameGen,
		DispatchedMessageID:  &turnAMsgID,
	}); err != nil {
		t.Fatalf("stamp turn A dispatch columns: %v", err)
	}
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:          turnA.ID,
		Status:      sqlcgen.TurnStatusFailed,
		CompletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("mark turn A failed (timeout): %v", err)
	}

	// Turn B: a queued retrigger, dispatched to the SAME sandbox
	// incarnation (no respawn -- same gen), carrying a COMPLETELY
	// DIFFERENT head sha/context. This is now the session's own
	// 'processing' turn.
	turnBHeadSHA := "sha-turn-b-a-newer-unrelated-commit"
	turnBContext, err := json.Marshal(reviewverdict.Context{BaseRef: "release/2026.09", BaseSHA: "base-sha-turn-b", PolicyVersion: 1})
	if err != nil {
		t.Fatalf("marshal turn B context: %v", err)
	}
	turnB, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID:            session.ID,
		Status:               sqlcgen.TurnStatusProcessing,
		ReviewHeadSha:        &turnBHeadSHA,
		ReviewVerdictContext: turnBContext,
	})
	if err != nil {
		t.Fatalf("create turn B: %v", err)
	}
	turnBMsgID := turnBMessageID
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:                   turnB.ID,
		Status:               sqlcgen.TurnStatusProcessing,
		DispatchedSandboxGen: &sameGen,
		DispatchedMessageID:  &turnBMsgID,
	}); err != nil {
		t.Fatalf("stamp turn B dispatch columns: %v", err)
	}

	// Turn A's own agent, unaware it has already been marked failed,
	// finally posts -- presenting turn A's OWN dispatch message id, never
	// turn B's.
	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", turnAMessageID, validVerdictRequestJSON())
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}

	var gotHeadSHA, gotBaseRef, gotBaseSHA string
	if err := rig.pool.QueryRow(ctx,
		`SELECT head_sha, base_ref, base_sha FROM review_verdicts WHERE repo_full_name = $1 AND pr_number = $2`,
		repoFullName, 90,
	).Scan(&gotHeadSHA, &gotBaseRef, &gotBaseSHA); err != nil {
		t.Fatalf("query review_verdicts row: %v", err)
	}

	// THE DECISIVE F3 ASSERTION: the persisted verdict must carry turn
	// A's own context -- the turn that actually produced it -- never turn
	// B's, even though turn B is the session's own CURRENTLY-PROCESSING
	// turn at the moment this request lands.
	if gotHeadSHA != turnAHeadSHA {
		t.Errorf("head_sha = %q, want %q (turn A's own examined commit -- got turn B's context instead, the exact F3 misattribution)", gotHeadSHA, turnAHeadSHA)
	}
	if gotBaseRef != "main" {
		t.Errorf("base_ref = %q, want %q (turn A's own recorded base ref)", gotBaseRef, "main")
	}
	if gotBaseSHA != "base-sha-turn-a" {
		t.Errorf("base_sha = %q, want %q (turn A's own recorded base sha)", gotBaseSHA, "base-sha-turn-a")
	}
}

// TestPostReviewVerdict_CorruptedReviewVerdictContext_LogsLoudErrorNeverSilentlyReadsAsUntracked
// is §21.1's amendment's own findings F4/F6 regression test for the "a silently-
// failing unmarshal must be a loud error, not a NULL" requirement: a turn
// whose own turns.review_verdict_context WAS recorded (len > 0) but is not
// valid JSON (a genuine bug -- a corrupted column, a schema drift between
// writer and reader -- never the legitimate "context tracking predates
// this row" case, which is an EMPTY/NULL column instead) must log at
// Error, not Warn, and with wording that does not read as the ordinary,
// expected backfill case -- an operator/alerting pipeline watching for
// Error-level lines must be able to tell "review context tracking is
// broken for this repo" apart from "an old verdict, nothing to fix".
func TestPostReviewVerdict_CorruptedReviewVerdictContext_LogsLoudErrorNeverSilentlyReadsAsUntracked(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	repoFullName := "acme/verdict-corrupted-context"
	session := setupReviewSessionWithSandbox(ctx, t, rig, repoFullName, 92)

	reviewHeadSHA := "sha-corrupted-context"
	// Syntactically VALID JSON (Postgres' own JSONB column type validates
	// syntax at INSERT time, so genuinely malformed JSON like "{not valid"
	// can never even reach this far) but the WRONG shape for
	// reviewverdict.Context (a JSON array, not an object) -- exactly the
	// class of corruption Postgres itself cannot catch but Go's own
	// json.Unmarshal into a typed struct does.
	createdTurn, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID:            session.ID,
		Status:               sqlcgen.TurnStatusProcessing,
		ReviewHeadSha:        &reviewHeadSHA,
		ReviewVerdictContext: []byte("[1,2,3]"),
	})
	if err != nil {
		t.Fatalf("seed processing turn with corrupted review_verdict_context: %v", err)
	}
	turnMessageID := testDispatchMessageID
	if _, err := rig.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: createdTurn.ID, Status: sqlcgen.TurnStatusProcessing, DispatchedMessageID: &turnMessageID}); err != nil {
		t.Fatalf("stamp dispatched_message_id on seeded turn: %v", err)
	}

	buf := captureDefaultLoggerJSON(t)

	status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", testDispatchMessageID, validVerdictRequestJSON())
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d (a corrupted context must never fail the verdict post itself)", status, http.StatusCreated)
	}

	entry := findLogEntry(t, buf, "httpapi: review-verdict: unmarshal review_verdict_context failed -- a context WAS recorded but could not be decoded (a bug, never the legitimate 'predates tracking' case); this verdict will read as ReasonContextUnknown and lose auto-approval/auto-merge eligibility exactly like a genuinely untracked verdict, which is misleading -- investigate this repo/PR directly")
	if level, _ := entry["level"].(string); level != "ERROR" {
		t.Errorf("log level = %q, want %q (a silently-failing unmarshal must be a LOUD error, not a Warn indistinguishable from the ordinary degraded-but-expected cases elsewhere in this handler)", level, "ERROR")
	}

	// The verdict itself must still degrade safely (never fabricate a base
	// ref) -- confirming this fix is about the LOG's own loudness, never
	// about changing the fail-safe outcome ComputeEligible already
	// enforces for an unknown context.
	var gotBaseRef *string
	if err := rig.pool.QueryRow(ctx, `SELECT base_ref FROM review_verdicts WHERE repo_full_name = $1 AND pr_number = $2`, repoFullName, 92).Scan(&gotBaseRef); err != nil {
		t.Fatalf("query review_verdicts row: %v", err)
	}
	if gotBaseRef != nil {
		t.Errorf("base_ref = %v, want NULL (a corrupted context must degrade to no recorded context, never a fabricated one)", *gotBaseRef)
	}
}
