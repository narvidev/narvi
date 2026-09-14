package githubapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/app/ports"
)

// TestGetPRBody_Success proves a real GET .../pulls/{number} response's
// own "body" field is decoded, and the request itself is authenticated
// and pathed correctly.
func TestGetPRBody_Success(t *testing.T) {
	t.Parallel()

	var gotPath, gotMethod, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 42,
			"body":   "The current live body.",
		})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	body, found, err := adapter.GetPRBody(context.Background(), "acme", "widgets", 42, "gho_bottoken")
	if err != nil {
		t.Fatalf("GetPRBody() error = %v, want nil", err)
	}
	if !found {
		t.Fatal("GetPRBody() found = false, want true")
	}
	if body != "The current live body." {
		t.Errorf("GetPRBody() body = %q, want %q", body, "The current live body.")
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %s, want GET", gotMethod)
	}
	if gotPath != "/repos/acme/widgets/pulls/42" {
		t.Errorf("request path = %q, want %q", gotPath, "/repos/acme/widgets/pulls/42")
	}
	if gotAuth != "Bearer gho_bottoken" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer gho_bottoken")
	}
}

// TestGetPRBody_NullBodyIsEmptyString proves GitHub's own documented
// nullable "body" field (a PR opened with no description at all) decodes
// to an empty string, never a nil-pointer panic.
func TestGetPRBody_NullBodyIsEmptyString(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 42,
			"body":   nil,
		})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	body, found, err := adapter.GetPRBody(context.Background(), "acme", "widgets", 42, "tok")
	if err != nil {
		t.Fatalf("GetPRBody() error = %v, want nil", err)
	}
	if !found {
		t.Fatal("GetPRBody() found = false, want true")
	}
	if body != "" {
		t.Errorf("GetPRBody() body = %q, want empty string for a null body", body)
	}
}

// TestGetPRBody_404IsNotFoundNotError proves a confirmed GitHub 404
// degrades to found=false, err=nil -- never conflated with a genuine API
// failure, mirroring GetOpenPR's own identical discipline.
func TestGetPRBody_404IsNotFoundNotError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "Not Found"})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	_, found, err := adapter.GetPRBody(context.Background(), "acme", "widgets", 999, "tok")
	if err != nil {
		t.Fatalf("GetPRBody() error = %v, want nil on a confirmed 404", err)
	}
	if found {
		t.Error("GetPRBody() found = true, want false on a confirmed 404")
	}
}

// TestGetPRBody_5xxIsGenuineError proves a real server error (never
// confused with a confirmed-absent 404) surfaces as a genuine error.
func TestGetPRBody_5xxIsGenuineError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	_, found, err := adapter.GetPRBody(context.Background(), "acme", "widgets", 42, "tok")
	if err == nil {
		t.Fatal("GetPRBody() error = nil, want a genuine error on a 5xx")
	}
	if found {
		t.Error("GetPRBody() found = true, want false on a genuine error")
	}
}

// TestUpdatePRBody_PatchesBodyOnly proves UpdatePRBody sends a real PATCH
// to the pull request's own endpoint, with a request body carrying ONLY
// "body" -- never "title" or any other field this same GitHub endpoint
// also accepts (§26.2: "the title is never rewritten automatically").
func TestUpdatePRBody_PatchesBodyOnly(t *testing.T) {
	t.Parallel()

	var gotPath, gotMethod, gotAuth string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	err := adapter.UpdatePRBody(context.Background(), ports.UpdatePRBodySpec{
		Owner: "acme", Repo: "widgets", Number: 42, Body: "The new composed body.", Token: "gho_bottoken",
	})
	if err != nil {
		t.Fatalf("UpdatePRBody() error = %v, want nil", err)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", gotMethod)
	}
	if gotPath != "/repos/acme/widgets/pulls/42" {
		t.Errorf("request path = %q, want %q", gotPath, "/repos/acme/widgets/pulls/42")
	}
	if gotAuth != "Bearer gho_bottoken" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer gho_bottoken")
	}
	if len(gotBody) != 1 {
		t.Fatalf("PATCH body has %d top-level keys, want exactly 1 (body only), got: %v", len(gotBody), gotBody)
	}
	if gotBody["body"] != "The new composed body." {
		t.Errorf("PATCH body[\"body\"] = %v, want %q", gotBody["body"], "The new composed body.")
	}
	if _, hasTitle := gotBody["title"]; hasTitle {
		t.Error("PATCH body carries a \"title\" key -- UpdatePRBody must never send one")
	}
}

// TestUpdatePRBody_4xxSurfacesAsError proves a real GitHub rejection (a
// closed/locked PR, insufficient permissions, ...) surfaces as a genuine
// error, never silently swallowed.
func TestUpdatePRBody_4xxSurfacesAsError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "Validation Failed"})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	err := adapter.UpdatePRBody(context.Background(), ports.UpdatePRBodySpec{
		Owner: "acme", Repo: "widgets", Number: 42, Body: "x", Token: "tok",
	})
	if err == nil {
		t.Fatal("UpdatePRBody() error = nil, want a genuine error on a 422")
	}
}

// TestUpdatePRBody_403RateLimited_APIErrorCarriesRateLimited is doPatch's
// own regression test for docs/TECHNICAL_PLAN.md §17's own adversarial
// review: "doPost/doPatch never compute RateLimited, so a textbook
// primary-rate-limited 403 through either unwraps to
// ErrPermissionDenied." doPatch previously never computed
// APIError.RateLimited at all, so this 403 -- flagged by GitHub's own
// X-RateLimit-Remaining header -- would have unwrapped straight to
// ports.ErrPermissionDenied via APIError.Unwrap's own Status==403 branch.
func TestUpdatePRBody_403RateLimited_APIErrorCarriesRateLimited(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "API rate limit exceeded for xxx.xxx.xxx.xxx."})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)
	err := adapter.UpdatePRBody(context.Background(), ports.UpdatePRBodySpec{
		Owner: "acme", Repo: "widgets", Number: 42, Body: "x", Token: "tok",
	})
	if err == nil {
		t.Fatal("UpdatePRBody() error = nil, want a genuine error on a rate-limited 403")
	}
	if errors.Is(err, ports.ErrPermissionDenied) {
		t.Error("errors.Is(err, ports.ErrPermissionDenied) = true, want false -- a rate-limited 403 must never classify as a permanent denial")
	}

	var apiErr *githubapi.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("UpdatePRBody() error = %v (%T), want an error wrapping *githubapi.APIError", err, err)
	}
	if !apiErr.RateLimited {
		t.Error("APIError.RateLimited = false, want true -- doPatch must compute it exactly like doGet/doPut do")
	}
}
