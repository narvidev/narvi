//go:build integration

// Row 182's piece (c) on the production router (technical plan §43.20):
// narvi_get_session_result, called through the official SDK client under a
// real mcp:read grant, answers the bytes its REST twin answers the same
// user's cookie, carries no transcript, and a grant without mcp:read is
// told the tool does not exist.
//
// The production router's source control is the real code host adapter,
// so nothing here may make a live read: every pull request seeded below
// resolves its review from the record alone -- absent, not assessed, or a
// verdict produced under an older policy (stale with no live read,
// reviewfreshness.Assess's record-only pass). The live read itself is
// proven against a fake code host in internal/adapters/inbound/httpapi and
// internal/adapters/inbound/mcp.
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/narvidev/narvi/contracts"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// seedResultSession creates a session for userID whose result needs no
// live read: it is the review session of acme/widgets#7, whose newest
// review attempt -- a run that streamed text -- ended without posting over
// an older verdict (not_assessed); it opened acme/widgets#8, never reviewed
// (absent), and acme/widgets#9, whose only verdict was produced under a
// policy version older than the current one (stale, decided on the record
// alone).
func seedResultSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID pgtype.UUID) pgtype.UUID {
	t.Helper()
	sessions := narvipg.NewSessionStore(pool)
	turns := narvipg.NewTurnStore(pool)
	events := narvipg.NewEventStore(pool)
	artifacts := narvipg.NewArtifactStore(pool)
	claims := narvipg.NewGitHubPRSessionStore(pool)
	verdicts := narvipg.NewReviewVerdictStore(pool)

	repos := []byte(`[{"branch":null,"name":"widgets","url":"https://github.com/acme/widgets.git"}]`)
	sess, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceWeb, CreatedBy: userID, Repos: repos})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := claims.EnsureRow(ctx, "acme/widgets", 7); err != nil {
		t.Fatalf("ensure claim: %v", err)
	}
	if err := claims.SetSessionID(ctx, "acme/widgets", 7, sess.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	verdict := func(n int32, attemptID pgtype.UUID, policy int32) {
		t.Helper()
		baseRef, baseSHA := "main", "b1"
		if _, err := verdicts.Insert(ctx, sqlcgen.InsertReviewVerdictParams{
			RepoFullName: "acme/widgets", PrNumber: n, HeadSha: "h1",
			RiskLevel: "medium", Premise: "ok", BlastRadius: []byte(`[]`), FilesChanged: 3,
			TestsCoverage: "adequate", DocsDrift: "none", ProposedShippable: "needs_human", Shippable: "needs_human",
			SessionID: sess.ID, ArchDecisionTags: []byte(`[]`), ArchDecisionRoots: []byte(`[]`), AncestorChain: []byte(`[]`),
			BaseRef: &baseRef, BaseSha: &baseSHA, PolicyVersion: policy, AttemptID: attemptID,
		}); err != nil {
			t.Fatalf("verdict #%d: %v", n, err)
		}
	}
	endAttempt := func(text string) sqlcgen.Turn {
		t.Helper()
		attempt, err := turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sess.ID, Status: sqlcgen.TurnStatusPending, IsReviewAttempt: true})
		if err != nil {
			t.Fatalf("create attempt: %v", err)
		}
		watermark, err := events.MaxEventIDForSession(ctx, sess.ID)
		if err != nil {
			t.Fatalf("watermark: %v", err)
		}
		if _, err := turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{ID: attempt.ID, Status: sqlcgen.TurnStatusDispatched, DispatchedAt: now, DispatchedEventID: &watermark}); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if text != "" {
			part := "prt-" + attempt.ID.String()
			payload, _ := json.Marshal(map[string]string{"type": "token", "messageId": part, "text": text})
			if _, err := events.Create(ctx, sqlcgen.CreateEventParams{SessionID: sess.ID, Type: "token", MessageID: part, Payload: payload}); err != nil {
				t.Fatalf("token: %v", err)
			}
		}
		for _, status := range []sqlcgen.TurnStatus{sqlcgen.TurnStatusProcessing, sqlcgen.TurnStatusCompleted} {
			arg := sqlcgen.UpdateTurnStatusParams{ID: attempt.ID, Status: status}
			if status == sqlcgen.TurnStatusCompleted {
				arg.CompletedAt = now
			}
			if _, err := turns.UpdateStatus(ctx, arg); err != nil {
				t.Fatalf("end attempt: %v", err)
			}
		}
		return attempt
	}

	first := endAttempt("First review: medium risk.")
	verdict(7, first.ID, 1)
	endAttempt("The second review ran out of time before posting.")

	for _, n := range []int{8, 9} {
		meta := fmt.Sprintf(`{"repo":"widgets","number":%d}`, n)
		if _, err := artifacts.Create(ctx, sqlcgen.CreateArtifactParams{SessionID: sess.ID, Type: sqlcgen.ArtifactTypePr, Url: fmt.Sprintf("https://github.com/acme/widgets/pull/%d", n), Metadata: []byte(meta)}); err != nil {
			t.Fatalf("pull request artifact #%d: %v", n, err)
		}
	}
	verdict(9, pgtype.UUID{}, 0)
	return sess.ID
}

// sessionOutcomeKeys is SessionOutcome's own property list from
// /contracts: the result carries exactly these keys, so no transcript.
func sessionOutcomeKeys(t *testing.T) []string {
	t.Helper()
	data, err := contracts.FS.ReadFile("rest/v1/dtos.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0)
	for k := range doc.Defs["SessionOutcome"].Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// assertResultShape checks the seeded result as the tool returned it: the
// review of #7 not assessed with its older verdict superseded, #8 absent,
// #9 stale from the record alone, the summary the second attempt's text,
// and exactly SessionOutcome's keys.
func assertResultShape(t *testing.T, body []byte) {
	t.Helper()
	var got struct {
		Activity    string `json:"activity"`
		ReviewScope string `json:"reviewScope"`
		LastRun     struct {
			Summary struct {
				Text      *string `json:"text"`
				Truncated bool    `json:"truncated"`
			} `json:"summary"`
		} `json:"lastRun"`
		PullRequests []struct {
			RepoFullName string `json:"repoFullName"`
			Number       int    `json:"number"`
			Review       struct {
				State     string `json:"state"`
				Freshness struct {
					State  string  `json:"state"`
					Reason *string `json:"reason"`
				} `json:"freshness"`
			} `json:"review"`
		} `json:"pullRequests"`
		ReviewedPullRequest *struct {
			Number int `json:"number"`
			Review struct {
				State             string          `json:"state"`
				Verdict           json.RawMessage `json:"verdict"`
				SupersededVerdict json.RawMessage `json:"supersededVerdict"`
			} `json:"review"`
		} `json:"reviewedPullRequest"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("result %s: %v", body, err)
	}
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(body, &keys)
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	if want := sessionOutcomeKeys(t); strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("result keys %v, want exactly SessionOutcome's %v -- the result carries no transcript", names, want)
	}
	if got.Activity != "finished" || got.ReviewScope != "reviewed" {
		t.Fatalf("activity %q scope %q, want finished, reviewed", got.Activity, got.ReviewScope)
	}
	if got.LastRun.Summary.Text == nil || *got.LastRun.Summary.Text != "The second review ran out of time before posting." || got.LastRun.Summary.Truncated {
		t.Fatalf("summary = %v, want the last run's final text", got.LastRun.Summary.Text)
	}
	r := got.ReviewedPullRequest
	if r == nil || r.Number != 7 || r.Review.State != "not_assessed" || string(r.Review.Verdict) != "null" || string(r.Review.SupersededVerdict) == "null" {
		t.Fatalf("reviewedPullRequest = %+v, want #7 not_assessed with the older verdict superseded", r)
	}
	if len(got.PullRequests) != 2 {
		t.Fatalf("pullRequests = %+v, want #8 and #9", got.PullRequests)
	}
	pr8, pr9 := got.PullRequests[0], got.PullRequests[1]
	if pr8.RepoFullName != "acme/widgets" || pr8.Number != 8 || pr8.Review.State != "absent" || pr8.Review.Freshness.State != "not_applicable" {
		t.Fatalf("#8 = %+v, want absent", pr8)
	}
	if pr9.Number != 9 || pr9.Review.State != "assessed" || pr9.Review.Freshness.State != "stale" || pr9.Review.Freshness.Reason == nil ||
		!strings.Contains(*pr9.Review.Freshness.Reason, "earlier eligibility policy") {
		t.Fatalf("#9 = %+v, want assessed and stale on the policy, decided with no live read", pr9)
	}
}

// sdkSessionResult is TestOAuth_ProductionRouter's SessionResult_SDKClient:
// the official SDK client, holding a real mcp:read grant from the consent
// flow, reads a session's result; narvi_get_session_result answers the
// REST twin's bytes exactly -- the result has no per-read clock -- its
// structured content is the same document, and it carries no transcript.
func sdkSessionResult(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, nil)
	id := seedResultSession(ctx, t, rig.pool, flow.member.ID).String()

	status, restBody := rig.restRaw(t, "/api/sessions/"+id+"/result", flow.cookie)
	if status != http.StatusOK {
		t.Fatalf("REST result: %d %s", status, restBody)
	}
	res, err := flow.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "narvi_get_session_result", Arguments: map[string]any{"sessionId": id}})
	if err != nil {
		t.Fatalf("CallTool narvi_get_session_result: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool narvi_get_session_result: isError %+v", res.Content)
	}
	text := toolText(t, res)
	if !bytes.Equal(bytes.TrimSpace(restBody), bytes.TrimSpace(text)) {
		t.Fatalf("result bytes differ from the REST twin.\nREST: %s\nMCP:  %s", restBody, text)
	}
	structured, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var fromText, fromStructured any
	if err := json.Unmarshal(text, &fromText); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(structured, &fromStructured); err != nil || fmt.Sprint(fromText) != fmt.Sprint(fromStructured) {
		t.Fatalf("structuredContent %s differs from the text block", structured)
	}
	assertResultShape(t, text)
}

// sdkSessionResultScopeless is TestOAuth_ProductionRouter's
// SessionResult_ScopelessGrantDoesNotSeeIt: a grant the member approved
// without mcp:read is told the result tool does not exist -- absent from
// tools/list, the SDK's own call fails, and a raw call answers exactly what
// a tool that never existed answers, once the name is substituted.
func sdkSessionResultScopeless(t *testing.T, rig *oauthRouterRig) {
	ctx := oauthTestCtx(t)
	flow := rig.connectSDKClient(ctx, t, func(d *consentDriver) {
		d.keepScopes = func([]string) []string { return nil }
	})
	id := seedResultSession(ctx, t, rig.pool, flow.member.ID).String()

	for _, name := range toolNames(ctx, t, flow.session) {
		if name == "narvi_get_session_result" {
			t.Fatal("a scope-less grant lists narvi_get_session_result")
		}
	}
	if _, err := flow.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "narvi_get_session_result", Arguments: map[string]any{"sessionId": id}}); err == nil {
		t.Fatal("calling the hidden result tool through the SDK succeeded")
	}
	scopeless := flow.recorder.lastBearer()
	other, _ := createRouterUser(ctx, t, rig.pool, sqlcgen.UserRoleMember)
	full := mintBuildBearer(ctx, t, rig.pool, rig.cfg, other.ID)
	call := func(token, tool string) (int, string) {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":%q,"arguments":{"sessionId":%q},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, tool, id)
		status, _, raw := rig.postMCP(t, "tools/call", tool, body, token)
		return status, string(raw)
	}
	const unknown = "narvi_does_not_exist"
	unknownStatus, unknownBody := call(full, unknown)
	status, body := call(scopeless, "narvi_get_session_result")
	if status != unknownStatus || body != strings.ReplaceAll(unknownBody, unknown, "narvi_get_session_result") {
		t.Fatalf("the hidden result tool answers differently from an unknown tool:\n hidden:  %d %s\n unknown: %d %s", status, body, unknownStatus, unknownBody)
	}
	// The very same tool under a full mcp:read grant is there.
	if status, body := call(full, "narvi_get_session_result"); status != http.StatusOK || !strings.Contains(body, `"reviewScope"`) {
		t.Fatalf("under a full grant: %d %s, want the result", status, body)
	}
}
