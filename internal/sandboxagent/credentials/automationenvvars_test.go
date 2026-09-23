package credentials_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/sandboxagent/credentials"
)

// TestCPClient_FetchAutomationEnvVars_RequestShape mirrors
// TestCPClient_FetchSandboxSecrets_RequestShape exactly (sandboxsecrets_test.go)
// -- W2's own regression test: before this fix, nothing pinned the actual
// request FetchAutomationEnvVars sends (path, method, Authorization,
// X-Sandbox-Gen), so the client and CP's own AutomationEnvVarsDelivery
// handler (internal/adapters/inbound/httpapi/automationenvvarsdelivery.go)
// could silently drift apart on the wire contract with no test noticing.
func TestCPClient_FetchAutomationEnvVars_RequestShape(t *testing.T) {
	t.Parallel()

	var gotMethod, gotAuth, gotPath, gotGen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotGen = r.Header.Get("X-Sandbox-Gen")

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"envVars": map[string]string{"TARGET_ENV": "staging"},
		})
	}))
	defer server.Close()

	client, err := credentials.NewCPClient(wsEquivalent(server.URL), testFetchTimeout)
	if err != nil {
		t.Fatalf("NewCPClient() error = %v", err)
	}

	got, err := client.FetchAutomationEnvVars(context.Background(), "sess-1", "sandbox-tok", 42)
	if err != nil {
		t.Fatalf("FetchAutomationEnvVars() error = %v, want nil", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want %q", gotMethod, http.MethodPost)
	}
	if gotPath != "/sessions/sess-1/automation-env-vars" {
		t.Errorf("path = %q, want %q", gotPath, "/sessions/sess-1/automation-env-vars")
	}
	if gotAuth != "Bearer sandbox-tok" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer sandbox-tok")
	}
	if gotGen != "42" {
		t.Errorf("X-Sandbox-Gen header = %q, want %q", gotGen, "42")
	}
	if got["TARGET_ENV"] != "staging" {
		t.Errorf("EnvVars[TARGET_ENV] = %q, want %q", got["TARGET_ENV"], "staging")
	}
}

// TestCPClient_FetchAutomationEnvVars_EmptyMapIsNotAnError mirrors
// TestCPClient_FetchSandboxSecrets_EmptyMapIsNotAnError -- the
// overwhelming common case (this session was never created by app/
// automation's own fanout.go at all) is a plain, successful empty map,
// never an error.
func TestCPClient_FetchAutomationEnvVars_EmptyMapIsNotAnError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"envVars": map[string]string{}})
	}))
	defer server.Close()

	client, err := credentials.NewCPClient(wsEquivalent(server.URL), testFetchTimeout)
	if err != nil {
		t.Fatalf("NewCPClient() error = %v", err)
	}

	got, err := client.FetchAutomationEnvVars(context.Background(), "sess-1", "tok", 1)
	if err != nil {
		t.Fatalf("FetchAutomationEnvVars() error = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("EnvVars = %v, want empty", got)
	}
}

func TestCPClient_FetchAutomationEnvVars_NonTwoXXIsAnError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"no usable automation env vars for this session"}`))
	}))
	defer server.Close()

	client, err := credentials.NewCPClient(wsEquivalent(server.URL), testFetchTimeout)
	if err != nil {
		t.Fatalf("NewCPClient() error = %v", err)
	}

	_, err = client.FetchAutomationEnvVars(context.Background(), "sess-1", "tok", 1)
	if err == nil {
		t.Fatal("FetchAutomationEnvVars() error = nil, want an error for a 403 response")
	}
}

// TestCPClient_FetchAutomationEnvVars_ErrorResponseBodyNeverLeaks mirrors
// TestCPClient_FetchSandboxSecrets_ErrorResponseBodyNeverLeaks exactly,
// for this method.
func TestCPClient_FetchAutomationEnvVars_ErrorResponseBodyNeverLeaks(t *testing.T) {
	t.Parallel()

	const value = "leaked-env-var-value"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal error","echo":{"TARGET_ENV":"` + value + `"}}`))
	}))
	defer server.Close()

	client, err := credentials.NewCPClient(wsEquivalent(server.URL), testFetchTimeout)
	if err != nil {
		t.Fatalf("NewCPClient() error = %v", err)
	}

	_, err = client.FetchAutomationEnvVars(context.Background(), "sess-1", "tok", 1)
	if err == nil {
		t.Fatal("FetchAutomationEnvVars() error = nil, want an error for a 500 response")
	}
	if strings.Contains(err.Error(), value) {
		t.Errorf("FetchAutomationEnvVars() error = %q, must never contain the raw response body/value %q", err.Error(), value)
	}
}

func TestCPClient_FetchAutomationEnvVars_MalformedResponseBody(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer server.Close()

	client, err := credentials.NewCPClient(wsEquivalent(server.URL), testFetchTimeout)
	if err != nil {
		t.Fatalf("NewCPClient() error = %v", err)
	}

	_, err = client.FetchAutomationEnvVars(context.Background(), "sess-1", "tok", 1)
	if err == nil {
		t.Fatal("FetchAutomationEnvVars() error = nil, want an error for a malformed 2xx response body")
	}
}
