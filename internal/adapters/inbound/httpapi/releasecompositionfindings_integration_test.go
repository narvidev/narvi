//go:build integration

// Integration tests for the release composition findings
// §15.3/§12.2 item 9) composition-findings-posting tool
// (releasecompositionfindings.go), against a real Postgres instance --
// sharing this package's own testRig (httpapi_integration_test.go),
// mirroring epistemicoutcome_integration_test.go's own house style
// exactly (both are sandbox-bearer-authenticated tools).
package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

func validCompositionFindingsRequestJSON(findingsJSON string) string {
	return `{"findings":[` + findingsJSON + `]}`
}

// postReleaseCompositionFindings posts body to sessionID's own
// release-manifest/composition-findings endpoint, mirroring
// postEpistemicOutcome's own identical bearer/gen header convention.
func postReleaseCompositionFindings(t *testing.T, r testRig, sessionID, bearer, gen, body string) (int, restdtos.PostReleaseCompositionFindingsResponse) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, r.server.URL+"/sessions/"+sessionID+"/release-manifest/composition-findings", strings.NewReader(body))
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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var got restdtos.PostReleaseCompositionFindingsResponse
	_ = json.NewDecoder(resp.Body).Decode(&got)
	return resp.StatusCode, got
}

// createReleaseManifestCheck inserts a real release_manifest_checks row
// for sessionID, mirroring internal/app/releasereview.Run's own
// persistReleaseManifestCheck insert -- the zero-config fixture every
// test below starts from: aggregate_review_triggered true,
// composition_reviewed_at still NULL (composition review not yet
// completed, this file's own tests' starting state).
func createReleaseManifestCheck(ctx context.Context, t *testing.T, r testRig, sessionID pgtype.UUID) sqlcgen.ReleaseManifestCheck {
	t.Helper()
	check, err := r.releaseManifestChecks.Insert(ctx, sqlcgen.InsertReleaseManifestCheckParams{
		SessionID:                     sessionID,
		RepoFullName:                  "acme/widgets",
		PrNumber:                      99,
		BaseRef:                       "main",
		HeadRef:                       "release/1.0",
		ConstituentPrCount:            3,
		AggregateReviewTriggered:      true,
		AggregateReviewTriggerReasons: []byte(`["a high-risk pull request is included in this release"]`),
		Findings:                      []byte(`[]`),
		MergedPrs:                     []byte(`[]`),
	})
	if err != nil {
		t.Fatalf("insert release manifest check: %v", err)
	}
	return check
}

func TestPostReleaseCompositionFindings_NotFound(t *testing.T) {
	rig := newTestRig(t)

	status, _ := postReleaseCompositionFindings(t, rig, "00000000-0000-0000-0000-000000000000", "any-token", "1", validCompositionFindingsRequestJSON(""))
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want %d", status, http.StatusNotFound)
	}
}

func TestPostReleaseCompositionFindings_MissingBearer_Unauthorized(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := bareSessionWithSandbox(ctx, t, rig, "composition-nobearer")

	status, _ := postReleaseCompositionFindings(t, rig, session.ID.String(), "", "1", validCompositionFindingsRequestJSON(""))
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

func TestPostReleaseCompositionFindings_WrongToken_Unauthorized(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := bareSessionWithSandbox(ctx, t, rig, "composition-wrongtoken")

	status, _ := postReleaseCompositionFindings(t, rig, session.ID.String(), "not-the-real-token", "1", validCompositionFindingsRequestJSON(""))
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", status, http.StatusUnauthorized)
	}
}

func TestPostReleaseCompositionFindings_GenMismatch_Forbidden(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := bareSessionWithSandbox(ctx, t, rig, "composition-genmismatch")

	status, _ := postReleaseCompositionFindings(t, rig, session.ID.String(), "composition-genmismatch", "999", validCompositionFindingsRequestJSON(""))
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want %d", status, http.StatusForbidden)
	}
}

// TestPostReleaseCompositionFindings_DeadSandbox_Gone mirrors
// TestPostEpistemicOutcome_DeadSandbox_Gone exactly: a terminalized
// sandbox can never post composition findings onto a release nobody could
// possibly still be legitimately reviewing from.
func TestPostReleaseCompositionFindings_DeadSandbox_Gone(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := bareSessionWithSandbox(ctx, t, rig, "composition-dead")

	if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET status = 'stopped' WHERE session_id = $1`, session.ID); err != nil {
		t.Fatalf("mark sandbox stopped: %v", err)
	}

	status, _ := postReleaseCompositionFindings(t, rig, session.ID.String(), "composition-dead", "1", validCompositionFindingsRequestJSON(""))
	if status != http.StatusGone {
		t.Errorf("status = %d, want %d", status, http.StatusGone)
	}
}

func TestPostReleaseCompositionFindings_MalformedBody_BadRequest(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := bareSessionWithSandbox(ctx, t, rig, "composition-malformed")

	status, _ := postReleaseCompositionFindings(t, rig, session.ID.String(), "composition-malformed", "1", `{`)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

// TestPostReleaseCompositionFindings_NoReleaseManifestCheck_BadRequest
// proves a call against a session with no release manifest check at all
// (not a release-review session, or the manifest check's own insert
// failed) is a 400, mirroring reviewverdict.go's own identical "no PR to
// act on" precedent.
func TestPostReleaseCompositionFindings_NoReleaseManifestCheck_BadRequest(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := bareSessionWithSandbox(ctx, t, rig, "composition-nocheck")

	status, _ := postReleaseCompositionFindings(t, rig, session.ID.String(), "composition-nocheck", "1", validCompositionFindingsRequestJSON(""))
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", status, http.StatusBadRequest)
	}
}

// TestPostReleaseCompositionFindings_HappyPath_PersistsFindings proves
// the core composition-findings mechanism end to end: a real finding posted through
// this endpoint lands in release_manifest_checks.composition_findings,
// and composition_reviewed_at is set -- the sentinel the release-review
// screen's readout depends on to stop rendering "not yet available".
func TestPostReleaseCompositionFindings_HappyPath_PersistsFindings(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := bareSessionWithSandbox(ctx, t, rig, "composition-happy")
	createReleaseManifestCheck(ctx, t, rig, session.ID)

	status, resp := postReleaseCompositionFindings(t, rig, session.ID.String(), "composition-happy", "1",
		validCompositionFindingsRequestJSON(`{"kind":"conflict","detail":"PR #1 and #2 both add migration 42"}`))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if resp.FindingsCount != 1 {
		t.Errorf("FindingsCount = %d, want 1", resp.FindingsCount)
	}
	if resp.ReviewedAt.IsZero() {
		t.Error("ReviewedAt is zero, want a real timestamp")
	}
	if resp.SessionId != session.ID.String() {
		t.Errorf("SessionId = %q, want %q", resp.SessionId, session.ID.String())
	}

	updated, err := rig.releaseManifestChecks.GetBySessionID(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetBySessionID: %v", err)
	}
	if !updated.CompositionReviewedAt.Valid {
		t.Error("composition_reviewed_at not set after posting findings")
	}
	if string(updated.CompositionFindings) == "[]" {
		t.Errorf("composition_findings still empty after posting a real finding, got %s", updated.CompositionFindings)
	}
	if !strings.Contains(string(updated.CompositionFindings), "migration 42") {
		t.Errorf("composition_findings = %s, want it to contain the posted detail text", updated.CompositionFindings)
	}
}

// TestPostReleaseCompositionFindings_EmptyFindings_HonestZero proves an
// empty findings array is still a REAL, positive result -- §15.3: "an
// empty findings array is a legitimate, positive result (this release
// composes cleanly)" -- composition_reviewed_at is still set, so the
// release-review screen can tell "reviewed, nothing found" apart from
// "not yet available" (this Step's own named guard against the "failed
// query renders as a confident zero" trap).
func TestPostReleaseCompositionFindings_EmptyFindings_HonestZero(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := bareSessionWithSandbox(ctx, t, rig, "composition-empty")
	createReleaseManifestCheck(ctx, t, rig, session.ID)

	status, resp := postReleaseCompositionFindings(t, rig, session.ID.String(), "composition-empty", "1", validCompositionFindingsRequestJSON(""))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if resp.FindingsCount != 0 {
		t.Errorf("FindingsCount = %d, want 0", resp.FindingsCount)
	}

	updated, err := rig.releaseManifestChecks.GetBySessionID(ctx, session.ID)
	if err != nil {
		t.Fatalf("GetBySessionID: %v", err)
	}
	if !updated.CompositionReviewedAt.Valid {
		t.Error("composition_reviewed_at not set -- an empty findings array is a real, POSITIVE result, never indistinguishable from 'not yet available'")
	}
}

// TestPostReleaseCompositionFindings_SecondCall_Conflict proves the
// guarded UPDATE's own idempotency: a retried/duplicate tool call for the
// same release never silently overwrites an already-posted result.
func TestPostReleaseCompositionFindings_SecondCall_Conflict(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()
	session := bareSessionWithSandbox(ctx, t, rig, "composition-twice")
	createReleaseManifestCheck(ctx, t, rig, session.ID)

	status, _ := postReleaseCompositionFindings(t, rig, session.ID.String(), "composition-twice", "1", validCompositionFindingsRequestJSON(""))
	if status != http.StatusCreated {
		t.Fatalf("first call status = %d, want %d", status, http.StatusCreated)
	}

	status, _ = postReleaseCompositionFindings(t, rig, session.ID.String(), "composition-twice", "1",
		validCompositionFindingsRequestJSON(`{"kind":"other","detail":"a second, different finding"}`))
	if status != http.StatusConflict {
		t.Errorf("second call status = %d, want %d (findings already posted)", status, http.StatusConflict)
	}
}
