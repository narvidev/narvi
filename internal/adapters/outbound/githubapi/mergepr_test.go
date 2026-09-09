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

func TestMergePR_Success(t *testing.T) {
	t.Parallel()

	var gotPath, gotMethod, gotAuth string
	var gotBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": "merged-sha-123", "merged": true, "message": "Pull Request successfully merged"})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)

	sha, err := adapter.MergePR(context.Background(), ports.MergePRSpec{
		Owner: "acme", Repo: "widgets", Number: 1204,
		HeadSHA: "abc123", MergeMethod: "squash", Token: "gho_realtoken",
	})
	if err != nil {
		t.Fatalf("MergePR() error = %v, want nil", err)
	}
	if sha != "merged-sha-123" {
		t.Errorf("MergePR() sha = %q, want %q", sha, "merged-sha-123")
	}
	if gotMethod != http.MethodPut {
		t.Errorf("request method = %q, want PUT", gotMethod)
	}
	if gotPath != "/repos/acme/widgets/pulls/1204/merge" {
		t.Errorf("request path = %q, want %q", gotPath, "/repos/acme/widgets/pulls/1204/merge")
	}
	if gotAuth != "Bearer gho_realtoken" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer gho_realtoken")
	}
	if gotBody["sha"] != "abc123" || gotBody["merge_method"] != "squash" {
		t.Errorf("request body = %+v, missing expected fields", gotBody)
	}
}

func TestMergePR_NotMergeable405(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Pull Request is not mergeable"})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)

	_, err := adapter.MergePR(context.Background(), ports.MergePRSpec{
		Owner: "acme", Repo: "widgets", Number: 1204, HeadSHA: "abc123", Token: "tok",
	})
	if err == nil {
		t.Fatal("MergePR() error = nil, want a 405 MergePRError")
	}
	var mergeErr *ports.MergePRError
	if !errors.As(err, &mergeErr) {
		t.Fatalf("MergePR() error = %v (%T), want *ports.MergePRError", err, err)
	}
	if mergeErr.Status != http.StatusMethodNotAllowed {
		t.Errorf("MergePRError.Status = %d, want 405", mergeErr.Status)
	}
}

func TestMergePR_StaleSHA409(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Head branch was modified. Review and try the merge again."})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)

	_, err := adapter.MergePR(context.Background(), ports.MergePRSpec{
		Owner: "acme", Repo: "widgets", Number: 1204, HeadSHA: "stale-sha", Token: "tok",
	})
	var mergeErr *ports.MergePRError
	if !errors.As(err, &mergeErr) {
		t.Fatalf("MergePR() error = %v (%T), want *ports.MergePRError", err, err)
	}
	if mergeErr.Status != http.StatusConflict {
		t.Errorf("MergePRError.Status = %d, want 409", mergeErr.Status)
	}
}

func TestMergePR_ReportedFalseIsAnError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": "", "merged": false, "message": "should not happen"})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)

	_, err := adapter.MergePR(context.Background(), ports.MergePRSpec{
		Owner: "acme", Repo: "widgets", Number: 1204, HeadSHA: "abc123", Token: "tok",
	})
	if err == nil {
		t.Fatal("MergePR() error = nil, want an error when merged=false despite HTTP 200")
	}
}

// The tests below close the adapter-half classifier gap docs/
// TECHNICAL_PLAN.md §17's own adversarial review found: "the entire
// adapter half of the classifier -- doPut's RateLimited computation,
// MergePR's carry-through, APIError.Unwrap -- has zero tests." MergePR is
// the ONE MergePRError producer internal/app/automerge's own authGuard
// actually classifies (worker.go's own mergeCandidate), so this is the
// most direct, end-to-end proof that a real GitHub response really does
// reach ports.ErrAuthenticationFailed/ports.ErrPermissionDenied the way
// authguard.go's own classify() function assumes.

// TestMergePR_401_UnwrapsToErrAuthenticationFailed proves a real "Bad
// credentials" 401 through MergePR's own doPut call unwraps to
// ports.ErrAuthenticationFailed via errors.Is -- the exact classification
// authGuard's own classify() function relies on.
func TestMergePR_401_UnwrapsToErrAuthenticationFailed(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Bad credentials"})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)

	_, err := adapter.MergePR(context.Background(), ports.MergePRSpec{
		Owner: "acme", Repo: "widgets", Number: 1204, HeadSHA: "abc123", Token: "bad-token",
	})
	if !errors.Is(err, ports.ErrAuthenticationFailed) {
		t.Fatalf("MergePR() error = %v, want errors.Is(err, ports.ErrAuthenticationFailed) = true", err)
	}
	if errors.Is(err, ports.ErrPermissionDenied) {
		t.Error("errors.Is(err, ports.ErrPermissionDenied) = true, want false -- a 401 is never a permission denial")
	}
}

// TestMergePR_403NonRateLimited_UnwrapsToErrPermissionDenied proves a
// genuine, non-rate-limited 403 (branch protection / no write access)
// unwraps to ports.ErrPermissionDenied.
func TestMergePR_403NonRateLimited_UnwrapsToErrPermissionDenied(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Resource not accessible by integration"})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)

	_, err := adapter.MergePR(context.Background(), ports.MergePRSpec{
		Owner: "acme", Repo: "widgets", Number: 1204, HeadSHA: "abc123", Token: "tok",
	})
	if !errors.Is(err, ports.ErrPermissionDenied) {
		t.Fatalf("MergePR() error = %v, want errors.Is(err, ports.ErrPermissionDenied) = true", err)
	}
	if errors.Is(err, ports.ErrAuthenticationFailed) {
		t.Error("errors.Is(err, ports.ErrAuthenticationFailed) = true, want false -- a per-repo denial is never worker-wide")
	}

	var mergeErr *ports.MergePRError
	if !errors.As(err, &mergeErr) {
		t.Fatalf("MergePR() error = %v (%T), want *ports.MergePRError", err, err)
	}
	if mergeErr.RateLimited {
		t.Error("MergePRError.RateLimited = true, want false -- a genuine denial, no rate-limit signal present")
	}
}

// TestMergePR_403RateLimitedHeader_NeverUnwrapsToEitherSentinel proves
// doPut's own RateLimited computation (adapter.go) carries all the way
// through MergePR's *ports.MergePRError conversion -- a live, transient
// rate limit must NEVER unwrap to ports.ErrPermissionDenied, which
// authGuard would otherwise misclassify as a permanent, per-repo
// dead-letter candidate for a condition that resolves on its own.
func TestMergePR_403RateLimitedHeader_NeverUnwrapsToEitherSentinel(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "API rate limit exceeded for xxx.xxx.xxx.xxx."})
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)

	_, err := adapter.MergePR(context.Background(), ports.MergePRSpec{
		Owner: "acme", Repo: "widgets", Number: 1204, HeadSHA: "abc123", Token: "tok",
	})
	if errors.Is(err, ports.ErrPermissionDenied) {
		t.Error("errors.Is(err, ports.ErrPermissionDenied) = true, want false -- a rate-limited 403 must never classify as a permanent denial")
	}
	if errors.Is(err, ports.ErrAuthenticationFailed) {
		t.Error("errors.Is(err, ports.ErrAuthenticationFailed) = true, want false")
	}

	var mergeErr *ports.MergePRError
	if !errors.As(err, &mergeErr) {
		t.Fatalf("MergePR() error = %v (%T), want *ports.MergePRError", err, err)
	}
	if !mergeErr.RateLimited {
		t.Error("MergePRError.RateLimited = false, want true -- doPut's own classification must carry through MergePR's *APIError -> *ports.MergePRError conversion")
	}
}

// TestMergePR_403EdgePageBody_RateLimitedViaMessageFallback is doPut's
// own regression test for the classifier's "wrong direction" bug: a 403
// whose body is NOT GitHub's JSON error envelope at all (an edge/WAF
// page) and carries NEITHER rate-limit header must still classify as
// RateLimited via isRateLimitedResponse's own message-text fallback.
// Before this fix, doPut overwrote the body text with a fixed
// placeholder string BEFORE handing it to isRateLimitedResponse, so that
// fallback -- documented as existing "in case a header got stripped
// somewhere between GitHub and this adapter (a proxy, a test double)" --
// could never fire for exactly the case it names, misclassifying a
// transient block as ports.ErrPermissionDenied's own permanent,
// per-repository denial.
func TestMergePR_403EdgePageBody_RateLimitedViaMessageFallback(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Deliberately NOT valid JSON, and NO rate-limit headers -- a
		// WAF/edge page is the only signal available, mirroring a real
		// Cloudflare 1015 response reaching this adapter through a proxy.
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html><body>error code: 1015 (you are being rate limited)</body></html>"))
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)

	_, err := adapter.MergePR(context.Background(), ports.MergePRSpec{
		Owner: "acme", Repo: "widgets", Number: 1204, HeadSHA: "abc123", Token: "tok",
	})
	if errors.Is(err, ports.ErrPermissionDenied) {
		t.Error("errors.Is(err, ports.ErrPermissionDenied) = true, want false -- an edge/WAF rate-limit page must never classify as a permanent denial")
	}

	var mergeErr *ports.MergePRError
	if !errors.As(err, &mergeErr) {
		t.Fatalf("MergePR() error = %v (%T), want *ports.MergePRError", err, err)
	}
	if !mergeErr.RateLimited {
		t.Error("MergePRError.RateLimited = false, want true -- the message-text fallback must see the RAW body, not a placeholder string overwriting it")
	}
}
