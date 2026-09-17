package githubapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/app/ports"
)

// TestGetOpenPR_StackPresent_AncestorChainPopulated is §21.1's amendment's
// own finding F5 regression test for this adapter's own ancestor-chain
// producer (ancestorChainFromDetailStack, listopenprs.go): before this
// test, nothing anywhere in this repository called that function with a
// real, populated "stack" object and inspected the result -- a mutation
// gutting it to `return nil` unconditionally broke no test. This proves
// the full wiring end to end, from the REAL GitHub "stack" JSON shape
// (mirroring TestGetPullRequest_StackPresent's own verified shape,
// adapter_test.go) through GetOpenPR's own buildOpenPRFromDetail
// construction, to a non-nil ports.OpenPR.AncestorChain.
func TestGetOpenPR_StackPresent_AncestorChainPopulated(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/42":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "title": "fix: retry loop", "html_url": "https://github.com/acme/widgets/pull/42",
				"state": "open", "draft": false, "additions": 10, "deletions": 2, "changed_files": 1,
				"created_at": "2026-08-05T10:00:00Z", "updated_at": "2026-08-05T11:00:00Z",
				"user":                map[string]any{"id": 500, "login": "narvi-bot"},
				"head":                map[string]any{"sha": "headsha42"},
				"base":                map[string]any{"ref": "main", "sha": "basesha-stale"},
				"labels":              []map[string]any{},
				"assignees":           []map[string]any{},
				"requested_reviewers": []map[string]any{},
				"requested_teams":     []map[string]any{},
				// A real stacked PR further up its own stack (position 2 of
				// 2) -- mirrors TestGetPullRequest_StackPresent's own
				// verified-live shape exactly.
				"stack": map[string]any{
					"size": 2, "position": 2,
					"base": map[string]any{"ref": "main", "sha": "stack-ultimate-base-sha"},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/42/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/commits/headsha42/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "success"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/commits/headsha42/check-runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []map[string]any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/42/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"filename": "internal/app/widget/retry.go"}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)

	pr, found, err := adapter.GetOpenPR(context.Background(), "acme", "widgets", 42, "tok")
	if err != nil {
		t.Fatalf("GetOpenPR() error = %v, want nil", err)
	}
	if !found {
		t.Fatal("GetOpenPR() found = false, want true")
	}

	// THE decisive F5 assertion: a real stacked PR (position 2 of 2)
	// reports its stack's own ultimate base as a real, non-nil ancestor
	// link -- mutation-test target: gutting ancestorChainFromDetailStack
	// to `return nil` unconditionally turns this assertion from a pass
	// into a failure.
	want := []ports.PRAncestorLink{{Ref: "main", SHA: "stack-ultimate-base-sha"}}
	if len(pr.AncestorChain) != len(want) {
		t.Fatalf("GetOpenPR() AncestorChain = %+v, want %+v", pr.AncestorChain, want)
	}
	for i := range want {
		if pr.AncestorChain[i] != want[i] {
			t.Errorf("GetOpenPR() AncestorChain[%d] = %+v, want %+v", i, pr.AncestorChain[i], want[i])
		}
	}
}

// TestGetOpenPR_NoStack_AncestorChainNil proves the ordinary, non-stacked
// case stays nil -- the counterpart to the populated case above, so a
// mutation that ALWAYS returns a non-nil link (the opposite failure mode)
// is caught too.
func TestGetOpenPR_NoStack_AncestorChainNil(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/43":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 43, "title": "fix: retry loop take 2", "html_url": "https://github.com/acme/widgets/pull/43",
				"state": "open", "draft": false, "additions": 10, "deletions": 2, "changed_files": 1,
				"created_at": "2026-08-05T10:00:00Z", "updated_at": "2026-08-05T11:00:00Z",
				"user":                map[string]any{"id": 500, "login": "narvi-bot"},
				"head":                map[string]any{"sha": "headsha43"},
				"base":                map[string]any{"ref": "main", "sha": "basesha-stale"},
				"labels":              []map[string]any{},
				"assignees":           []map[string]any{},
				"requested_reviewers": []map[string]any{},
				"requested_teams":     []map[string]any{},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/43/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/commits/headsha43/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "success"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/commits/headsha43/check-runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []map[string]any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/43/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"filename": "internal/app/widget/retry.go"}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)

	pr, found, err := adapter.GetOpenPR(context.Background(), "acme", "widgets", 43, "tok")
	if err != nil {
		t.Fatalf("GetOpenPR() error = %v, want nil", err)
	}
	if !found {
		t.Fatal("GetOpenPR() found = false, want true")
	}
	if pr.AncestorChain != nil {
		t.Errorf("GetOpenPR() AncestorChain = %+v, want nil (no stack object reported)", pr.AncestorChain)
	}
}

// TestGetOpenPR_DegradedStack_AncestorChainUnknownMarker is D1's own
// regression test (round-12 sweep): a stack object reporting position > 1
// (proving a link exists) but an EMPTY base ref -- a degraded GitHub
// stack read -- must report a non-nil, single-element chain with an
// empty Ref, this port's own dedicated "could not be established"
// marker (ports.PRAncestorLink's own doc comment), NEVER the same nil
// TestGetOpenPR_NoStack_AncestorChainNil proves for "genuinely no
// ancestor at all". Before this fix, ancestorChainFromDetailStack
// (listopenprs.go) carried its own stack.Base.Ref-is-empty clause and
// collapsed this exact input to nil -- indistinguishable, one port
// boundary up, from a PR that was never stacked -- while
// review.AncestorChainFromStack, its domain-side sibling, already
// reported a real (if empty-ref) link for the identical degraded input.
//
// Mutation-test target: reinstating `|| stack.Base.Ref == ""` in
// ancestorChainFromDetailStack's nil condition turns this test's own
// "AncestorChain != nil" assertion from a pass into a failure.
func TestGetOpenPR_DegradedStack_AncestorChainUnknownMarker(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/44":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 44, "title": "fix: retry loop take 3", "html_url": "https://github.com/acme/widgets/pull/44",
				"state": "open", "draft": false, "additions": 10, "deletions": 2, "changed_files": 1,
				"created_at": "2026-08-05T10:00:00Z", "updated_at": "2026-08-05T11:00:00Z",
				"user":                map[string]any{"id": 500, "login": "narvi-bot"},
				"head":                map[string]any{"sha": "headsha44"},
				"base":                map[string]any{"ref": "main", "sha": "basesha-stale"},
				"labels":              []map[string]any{},
				"assignees":           []map[string]any{},
				"requested_reviewers": []map[string]any{},
				"requested_teams":     []map[string]any{},
				// A degraded stack read: position 2 of 2 (proving a link
				// exists) but no base ref/sha decoded at all -- the exact
				// D1 shape.
				"stack": map[string]any{
					"size": 2, "position": 2,
					"base": map[string]any{"ref": "", "sha": ""},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/44/reviews":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/commits/headsha44/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "success"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/commits/headsha44/check-runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []map[string]any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/44/files":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"filename": "internal/app/widget/retry.go"}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)

	pr, found, err := adapter.GetOpenPR(context.Background(), "acme", "widgets", 44, "tok")
	if err != nil {
		t.Fatalf("GetOpenPR() error = %v, want nil", err)
	}
	if !found {
		t.Fatal("GetOpenPR() found = false, want true")
	}
	want := []ports.PRAncestorLink{{Ref: "", SHA: ""}}
	if len(pr.AncestorChain) != len(want) {
		t.Fatalf("GetOpenPR() AncestorChain = %+v, want %+v (a non-nil unknown marker, not nil)", pr.AncestorChain, want)
	}
	for i := range want {
		if pr.AncestorChain[i] != want[i] {
			t.Errorf("GetOpenPR() AncestorChain[%d] = %+v, want %+v", i, pr.AncestorChain[i], want[i])
		}
	}
}

// TestGetOpenPR_ClosedPR_FoundFalse is round-10 finding E's own regression
// test: GetOpenPR's own doc comment (ports/sourcecontrol.go) promises
// "found=false, err=nil means the PR does not exist, or is no longer open
// (closed/merged)", but before this fix a real, decodable 200 response
// for a CLOSED pull request reached found=TRUE identically to a
// genuinely open one -- nothing on openPRDetailResponse's own decode
// target was ever consulted to tell the two apart. This is the exact gap
// that made internal/app/decisioninbox.RevalidateForAutoMerge's own "this
// pull request is no longer open" refusal permanently unreachable for a
// PR that closed or merged between discovery and revalidation.
//
// This fixture registers NO handler for /reviews, /commits/.../status,
// /commits/.../check-runs, or /files -- GetOpenPR must return before ANY
// of buildOpenPRFromDetail's own sub-fetches run at all for a
// confirmed-closed PR, mirroring the confirmed-404 case's own identical
// "return before building anything" shape immediately above in this
// file. A regression that moved the state check to fire AFTER those
// sub-fetches, or dropped it entirely, would either fail this test's own
// "unexpected request" default case or (if dropped entirely) return
// found=true.
//
// Mutation-test target: deleting `if detail.State != "open" { return
// ports.OpenPR{}, false, nil }` from GetOpenPR (getopenpr.go) must turn
// this test's own "found = false" assertion into "found = true".
func TestGetOpenPR_ClosedPR_FoundFalse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/44":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 44, "title": "fix: already closed", "html_url": "https://github.com/acme/widgets/pull/44",
				"state": "closed", "draft": false, "additions": 10, "deletions": 2, "changed_files": 1,
				"created_at": "2026-08-05T10:00:00Z", "updated_at": "2026-08-05T11:00:00Z",
				"user":                map[string]any{"id": 500, "login": "narvi-bot"},
				"head":                map[string]any{"sha": "headsha44"},
				"base":                map[string]any{"ref": "main", "sha": "basesha-stale"},
				"labels":              []map[string]any{},
				"assignees":           []map[string]any{},
				"requested_reviewers": []map[string]any{},
				"requested_teams":     []map[string]any{},
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)

	pr, found, err := adapter.GetOpenPR(context.Background(), "acme", "widgets", 44, "tok")
	if err != nil {
		t.Fatalf("GetOpenPR() error = %v, want nil", err)
	}
	if found {
		t.Fatalf("GetOpenPR() found = true, want false (state == \"closed\") -- pr = %+v", pr)
	}
}

// TestGetOpenPR_MergedPR_FoundFalse mirrors TestGetOpenPR_ClosedPR_FoundFalse
// for a MERGED pull request specifically -- GitHub reports a merged PR
// with state == "closed" too (never a distinct "merged" state value), so
// this proves the SAME single-field check catches both, exactly as
// openPRDetailResponse.State's own doc comment claims.
func TestGetOpenPR_MergedPR_FoundFalse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/pulls/45":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 45, "title": "fix: already merged", "html_url": "https://github.com/acme/widgets/pull/45",
				"state": "closed", "merged": true, "draft": false, "additions": 10, "deletions": 2, "changed_files": 1,
				"created_at": "2026-08-05T10:00:00Z", "updated_at": "2026-08-05T11:00:00Z",
				"user":                map[string]any{"id": 500, "login": "narvi-bot"},
				"head":                map[string]any{"sha": "headsha45"},
				"base":                map[string]any{"ref": "main", "sha": "basesha-stale"},
				"labels":              []map[string]any{},
				"assignees":           []map[string]any{},
				"requested_reviewers": []map[string]any{},
				"requested_teams":     []map[string]any{},
			})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	adapter := githubapi.New(server.Client(), server.URL)

	pr, found, err := adapter.GetOpenPR(context.Background(), "acme", "widgets", 45, "tok")
	if err != nil {
		t.Fatalf("GetOpenPR() error = %v, want nil", err)
	}
	if found {
		t.Fatalf("GetOpenPR() found = true, want false (state == \"closed\", merged == true) -- pr = %+v", pr)
	}
}
