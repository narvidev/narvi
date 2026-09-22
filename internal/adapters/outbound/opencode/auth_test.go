package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSetOAuthAuth_SendsTheVerifiedWireShape proves SetOAuthAuth's own PUT
// /auth/{providerID} body matches §29.1/§29.6's verified shape exactly --
// most importantly, that "refresh" is present as a real (if empty)
// string key, never omitted, and that OAuthCredential's own caller-facing
// type carries no field a caller COULD populate it from even if it tried
// (see OAuthCredential's own doc comment).
func TestSetOAuthAuth_SendsTheVerifiedWireShape(t *testing.T) {
	var captured map[string]any
	var capturedPath string
	var capturedMethod string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/openai" {
			capturedPath = r.URL.Path
			capturedMethod = r.Method
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Errorf("decode request body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(true)
			return
		}
		// Every other path (in particular the adapter's own background
		// GET /event connection, started immediately by New) responds
		// immediately -- mirrors client_test.go's own identical precedent.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := New(srv.URL, testSSEInactivityTimeout, testReconnectInterval, clientRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	err := a.SetOAuthAuth(context.Background(), "openai", OAuthCredential{
		Access:    "access-token-abc",
		Expires:   1234567890123,
		AccountID: "acct-xyz-789",
	})
	if err != nil {
		t.Fatalf("SetOAuthAuth() error = %v, want nil", err)
	}

	if capturedMethod != http.MethodPut {
		t.Errorf("method = %q, want %q", capturedMethod, http.MethodPut)
	}
	if capturedPath != "/auth/openai" {
		t.Errorf("path = %q, want %q", capturedPath, "/auth/openai")
	}
	if captured["type"] != "oauth" {
		t.Errorf(`body["type"] = %v, want "oauth"`, captured["type"])
	}
	if captured["access"] != "access-token-abc" {
		t.Errorf(`body["access"] = %v, want "access-token-abc"`, captured["access"])
	}
	if captured["accountId"] != "acct-xyz-789" {
		t.Errorf(`body["accountId"] = %v, want "acct-xyz-789"`, captured["accountId"])
	}
	if got, want := captured["expires"], float64(1234567890123); got != want {
		t.Errorf(`body["expires"] = %v, want %v`, got, want)
	}
	refresh, present := captured["refresh"]
	if !present {
		t.Fatal(`body["refresh"] key is ABSENT, want present (as "") -- §29.1's own verified Auth.OAuth schema requires it, and OpenCode's own PUT /auth/{id} was verified live to require the key present`)
	}
	if refresh != "" {
		t.Errorf(`body["refresh"] = %q, want "" -- the refresh token must NEVER be sent to a sandbox (§29.5/§29.6)`, refresh)
	}
}

// TestSetOAuthAuth_PropagatesFailure proves a non-2xx response surfaces as
// a real error -- the caller (cmd/sandbox-agent/main.go) is the one that
// decides this is non-fatal to boot, never this method itself.
func TestSetOAuthAuth_PropagatesFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/openai" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := New(srv.URL, testSSEInactivityTimeout, testReconnectInterval, clientRequestTimeout, testSummarizeTimeout, testTransientRetryBackoff, testRuntimeVersion, testSandboxID)
	t.Cleanup(a.Close)

	err := a.SetOAuthAuth(context.Background(), "openai", OAuthCredential{Access: "a", Expires: 1, AccountID: "acct"})
	if err == nil {
		t.Fatal("SetOAuthAuth() error = nil, want non-nil for a 500 response")
	}
}
