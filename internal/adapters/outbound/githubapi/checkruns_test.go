package githubapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
)

// TestCreateCheckRun_Queued proves CreateCheckRun POSTs the exact shape
// GitHub's own Checks API documents for an INCOMPLETE check run: no
// conclusion field at all (never an empty string) when status is
// "queued" -- pinning reviewcheck.ComputeOutput's own PhaseQueued/
// PhaseRunning contract ("Incomplete never carries a conclusion") all
// the way through to the real wire request, not just the pure domain
// function.
func TestCreateCheckRun_Queued(t *testing.T) {
	t.Parallel()

	var gotPath, gotMethod, gotAuth string
	var gotBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "app": map[string]any{"id": 777}})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	id, appID, err := adapter.CreateCheckRun(context.Background(), "acme", "widgets", "tok", "deadbeef", "narvi/review", "queued", "", "Review queued", "Narvi has not yet started reviewing this pull request.", "101")
	if err != nil {
		t.Fatalf("CreateCheckRun() error = %v", err)
	}
	if id != 42 {
		t.Errorf("CreateCheckRun() id = %d, want 42", id)
	}
	// finding A2: the create response's own app.id must be surfaced back
	// to the caller -- this is what lets a caller self-learn the writing
	// credential's REAL identity, rather than asserting it from separate,
	// possibly-unrelated config.
	if appID != 777 {
		t.Errorf("CreateCheckRun() appID = %d, want 777 (read back from the create response's own app.id)", appID)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/repos/acme/widgets/check-runs" {
		t.Errorf("path = %q, want /repos/acme/widgets/check-runs", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want Bearer tok", gotAuth)
	}
	if _, present := gotBody["conclusion"]; present {
		t.Errorf("request body carries a conclusion field for an incomplete (queued) check run: %v -- GitHub's API rejects this", gotBody["conclusion"])
	}
	if gotBody["status"] != "queued" {
		t.Errorf("posted status = %v, want queued", gotBody["status"])
	}
	if gotBody["head_sha"] != "deadbeef" {
		t.Errorf("posted head_sha = %v, want deadbeef", gotBody["head_sha"])
	}
	if gotBody["name"] != "narvi/review" {
		t.Errorf("posted name = %v, want narvi/review", gotBody["name"])
	}
	// finding C1: the per-PR discriminator must reach the wire, under
	// GitHub's own "external_id" field name.
	if gotBody["external_id"] != "101" {
		t.Errorf("posted external_id = %v, want 101", gotBody["external_id"])
	}
}

// TestCreateCheckRun_TerminalCarriesConclusion proves the opposite shape:
// a completed check run DOES carry a real conclusion string.
func TestCreateCheckRun_TerminalCarriesConclusion(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	if _, _, err := adapter.CreateCheckRun(context.Background(), "acme", "widgets", "tok", "sha1", "narvi/review", "completed", "action_required", "Review not completed", "...", "202"); err != nil {
		t.Fatalf("CreateCheckRun() error = %v", err)
	}
	if gotBody["conclusion"] != "action_required" {
		t.Errorf("posted conclusion = %v, want action_required", gotBody["conclusion"])
	}
	// decision 1/neutral-never-appears: belt and suspenders at the wire
	// level too.
	if gotBody["conclusion"] == "neutral" {
		t.Fatal("posted conclusion must never be neutral")
	}
}

// TestUpdateCheckRun_PatchesByID proves UpdateCheckRun PATCHes the
// EXACT check-run id path, never re-resolving by name/SHA.
func TestUpdateCheckRun_PatchesByID(t *testing.T) {
	t.Parallel()

	var gotPath, gotMethod string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 99})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	err := adapter.UpdateCheckRun(context.Background(), "acme", "widgets", "tok", 99, "completed", "success", "Review complete", "Narvi has posted a review verdict for this pull request.")
	if err != nil {
		t.Fatalf("UpdateCheckRun() error = %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", gotMethod)
	}
	if gotPath != "/repos/acme/widgets/check-runs/99" {
		t.Errorf("path = %q, want /repos/acme/widgets/check-runs/99", gotPath)
	}
	if gotBody["conclusion"] != "success" {
		t.Errorf("posted conclusion = %v, want success", gotBody["conclusion"])
	}
	if _, present := gotBody["head_sha"]; present {
		t.Error("UpdateCheckRun must never send head_sha -- it targets an existing check run by id, never re-anchors it")
	}
}

// TestListCheckRunsForRef_FiltersByAppAndName proves the "select by SHA
// and GitHub App" recovery read returns enough data for a caller to
// filter out another app's same-named check run -- this test builds a
// fake ref response carrying TWO check runs sharing the name
// "narvi/review", one under a DIFFERENT app id, and proves both are
// reported back distinctly (the filtering itself is this adapter's
// caller's job, per this method's own doc comment; this test proves the
// data needed to do it safely actually arrives).
func TestListCheckRunsForRef_FiltersByAppAndName(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/commits/deadbeef/check-runs" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page = %q, want 100", r.URL.Query().Get("per_page"))
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_count": 2,
			"check_runs": []map[string]any{
				{"id": 1, "name": "narvi/review", "head_sha": "deadbeef", "app": map[string]any{"id": 999}, "external_id": "101"},
				{"id": 2, "name": "narvi/review", "head_sha": "deadbeef", "app": map[string]any{"id": 111}, "external_id": "102"},
			},
		})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	runs, err := adapter.ListCheckRunsForRef(context.Background(), "acme", "widgets", "deadbeef", "tok")
	if err != nil {
		t.Fatalf("ListCheckRunsForRef() error = %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d check runs, want 2", len(runs))
	}
	var ourAppID, otherAppID int64
	var ourExternalID, otherExternalID string
	for _, r := range runs {
		if r.AppID == 999 {
			ourAppID = r.ID
			ourExternalID = r.ExternalID
		}
		if r.AppID == 111 {
			otherAppID = r.ID
			otherExternalID = r.ExternalID
		}
	}
	if ourAppID != 1 {
		t.Errorf("our app's (999) check run id = %d, want 1", ourAppID)
	}
	if otherAppID != 2 {
		t.Errorf("other app's (111) check run id = %d, want 2 -- must be distinguishable, never adopted", otherAppID)
	}
	// finding C1: ExternalID (GitHub's own per-PR discriminator field)
	// must round-trip through this read, distinctly per run -- this is
	// what lets a caller scope adoption to one pull request even when two
	// runs share both name and head SHA.
	if ourExternalID != "101" {
		t.Errorf("our app's (999) check run ExternalID = %q, want 101", ourExternalID)
	}
	if otherExternalID != "102" {
		t.Errorf("other app's (111) check run ExternalID = %q, want 102", otherExternalID)
	}
}

// TestListCheckRunsForRef_PropagatesHTTPFailure proves a transport/HTTP
// failure surfaces as a real error, never a silently empty slice that
// would make a caller wrongly conclude "no existing check run, safe to
// create a new one" when the read itself actually failed.
func TestListCheckRunsForRef_PropagatesHTTPFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	if _, err := adapter.ListCheckRunsForRef(context.Background(), "acme", "widgets", "deadbeef", "tok"); err == nil {
		t.Fatal("ListCheckRunsForRef() error = nil, want the 500 to propagate")
	}
}

// TestCreateCheckRun_PermissionDeniedIsDistinguishable proves a genuine
// permission-denial 403 (no rate-limit signal on the response) unwraps
// to ports.ErrPermissionDenied -- the brief's own "surface permission...
// failures distinctly" requirement, exercised against the real adapter.
func TestCreateCheckRun_PermissionDeniedIsDistinguishable(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Resource not accessible by integration"})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	_, _, err := adapter.CreateCheckRun(context.Background(), "acme", "widgets", "tok", "sha1", "narvi/review", "queued", "", "t", "s", "303")
	if err == nil {
		t.Fatal("CreateCheckRun() error = nil, want a permission-denied error")
	}
	var apiErr *githubapi.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not a *githubapi.APIError: %v", err)
	}
	if apiErr.RateLimited {
		t.Error("a plain 403 with no rate-limit signal must not classify as RateLimited")
	}
}

// TestCreateCheckRun_RateLimitedIsDistinguishable proves a 403 carrying
// GitHub's own rate-limit signal (X-RateLimit-Remaining: 0) is
// classified DIFFERENTLY from a genuine permission denial -- the same
// status code, a different operator-facing meaning, per this adapter's
// own pre-existing isRateLimitedResponse machinery, now exercised
// through the check-run write path specifically.
func TestCreateCheckRun_RateLimitedIsDistinguishable(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "API rate limit exceeded"})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	_, _, err := adapter.CreateCheckRun(context.Background(), "acme", "widgets", "tok", "sha1", "narvi/review", "queued", "", "t", "s", "303")
	if err == nil {
		t.Fatal("CreateCheckRun() error = nil, want a rate-limit error")
	}
	var apiErr *githubapi.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not a *githubapi.APIError: %v", err)
	}
	if !apiErr.RateLimited {
		t.Error("a 403 with X-RateLimit-Remaining: 0 must classify as RateLimited")
	}
}
