package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
)

// TestAutomationEnvVarSpawnEnv_BuildsSortedNameValueEntries mirrors
// TestSandboxSecretSpawnEnv_BuildsSortedNameValueEntries exactly -- the
// same "resolved map -> sorted []string of NAME=VALUE entries" shape,
// never os.Setenv'd onto this (or sandbox-agent's own) process.
func TestAutomationEnvVarSpawnEnv_BuildsSortedNameValueEntries(t *testing.T) {
	t.Parallel()

	got := automationEnvVarSpawnEnv(map[string]string{
		"SECOND_VAR": "value-two",
		"FIRST_VAR":  "value-one",
	})

	want := []string{"FIRST_VAR=value-one", "SECOND_VAR=value-two"}
	if len(got) != len(want) {
		t.Fatalf("automationEnvVarSpawnEnv() = %v, want %v", got, want)
	}
	for i, entry := range want {
		if got[i] != entry {
			t.Errorf("automationEnvVarSpawnEnv()[%d] = %q, want %q (sorted by name)", i, got[i], entry)
		}
	}
}

// TestAutomationEnvVarSpawnEnv_EmptyValueIsLegitimate proves the exit
// criterion's own zero-value case: an empty string is a legitimate
// value, never dropped or treated as absent.
func TestAutomationEnvVarSpawnEnv_EmptyValueIsLegitimate(t *testing.T) {
	t.Parallel()

	got := automationEnvVarSpawnEnv(map[string]string{"EMPTY_FLAG": ""})
	want := []string{"EMPTY_FLAG="}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("automationEnvVarSpawnEnv() = %v, want %v", got, want)
	}
}

// TestAutomationEnvVarSpawnEnv_EmptyMapIsNoop proves the overwhelming
// common case (this session carries no automation env vars at all)
// produces a nil slice.
func TestAutomationEnvVarSpawnEnv_EmptyMapIsNoop(t *testing.T) {
	t.Parallel()

	if got := automationEnvVarSpawnEnv(nil); got != nil {
		t.Errorf("automationEnvVarSpawnEnv(nil) = %v, want nil", got)
	}
	if got := automationEnvVarSpawnEnv(map[string]string{}); got != nil {
		t.Errorf("automationEnvVarSpawnEnv({}) = %v, want nil", got)
	}
}

// TestFetchAutomationEnvVars_RealRoundTrip proves fetchAutomationEnvVars'
// own thin wrapper actually reaches a real (test) CP server and returns
// its resolved map.
func TestFetchAutomationEnvVars_RealRoundTrip(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"envVars": map[string]string{"TARGET_ENV": "staging"}})
	}))
	defer server.Close()

	cfg := boot.Config{
		SessionConfig: &sessionconfig.SessionConfig{
			ControlPlaneWsUrl: wsEquivalentForTest(server.URL),
			SessionId:         "sess-1",
			SandboxToken:      "tok",
			Gen:               1,
		},
	}

	got, ok := fetchAutomationEnvVars(context.Background(), cfg, testTimeouts())
	if !ok {
		t.Fatal("fetchAutomationEnvVars() ok = false, want true")
	}
	if got["TARGET_ENV"] != "staging" {
		t.Errorf("fetchAutomationEnvVars() = %v, want map containing TARGET_ENV=staging", got)
	}
}

// TestFetchAutomationEnvVars_NonAutomationSession_EmptyOK proves the exit
// criterion's own "non-automation sessions must be unaffected": CP's own
// delivery endpoint returns {} for a session with no automation_runs row
// at all, and this wrapper reports that as a SUCCESSFUL, non-degraded
// fetch (ok=true, empty map) -- never a failure.
func TestFetchAutomationEnvVars_NonAutomationSession_EmptyOK(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"envVars": map[string]string{}})
	}))
	defer server.Close()

	cfg := boot.Config{
		SessionConfig: &sessionconfig.SessionConfig{
			ControlPlaneWsUrl: wsEquivalentForTest(server.URL),
			SessionId:         "sess-1",
			SandboxToken:      "tok",
			Gen:               1,
		},
	}

	got, ok := fetchAutomationEnvVars(context.Background(), cfg, testTimeouts())
	if !ok {
		t.Fatal("fetchAutomationEnvVars() ok = false, want true (an empty map is a successful, non-degraded outcome)")
	}
	if len(got) != 0 {
		t.Errorf("fetchAutomationEnvVars() = %v, want empty", got)
	}
}

// TestFetchAutomationEnvVars_DegradesToNilOnFailure mirrors
// TestFetchSandboxSecrets_DegradesToNilOnFailure exactly: a CP server
// returning a persistent error must degrade to nil/false, never a panic
// or a propagated error the caller would have to handle.
func TestFetchAutomationEnvVars_DegradesToNilOnFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := boot.Config{
		SessionConfig: &sessionconfig.SessionConfig{
			ControlPlaneWsUrl: wsEquivalentForTest(server.URL),
			SessionId:         "sess-1",
			SandboxToken:      "tok",
			Gen:               1,
		},
	}

	got, ok := fetchAutomationEnvVars(context.Background(), cfg, testTimeouts())
	if ok {
		t.Error("fetchAutomationEnvVars() ok = true, want false on a persistent 500 response")
	}
	if got != nil {
		t.Errorf("fetchAutomationEnvVars() = %v, want nil on a 500 response", got)
	}
}

// TestFetchAutomationEnvVars_RetriesTransientFailureThenSucceeds mirrors
// TestFetchSandboxSecrets_RetriesTransientFailureThenSucceeds exactly: a
// 500 on the first attempt, then a 2xx on the second, must still succeed
// (§27.1's own "with bounded retry" posture, reused unchanged here).
func TestFetchAutomationEnvVars_RetriesTransientFailureThenSucceeds(t *testing.T) {
	t.Parallel()

	attempt := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt++
		if attempt == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"envVars": map[string]string{"TARGET_ENV": "staging"}})
	}))
	defer server.Close()

	cfg := boot.Config{
		SessionConfig: &sessionconfig.SessionConfig{
			ControlPlaneWsUrl: wsEquivalentForTest(server.URL),
			SessionId:         "sess-1",
			SandboxToken:      "tok",
			Gen:               1,
		},
	}

	timeouts := testTimeouts()
	timeouts.AutomationEnvVarFetchMaxAttempts = 3
	timeouts.AutomationEnvVarFetchRetryBaseDelay = 1
	timeouts.AutomationEnvVarFetchRetryMaxDelay = 1

	got, ok := fetchAutomationEnvVars(context.Background(), cfg, timeouts)
	if !ok {
		t.Fatal("fetchAutomationEnvVars() ok = false, want true (the second attempt succeeds)")
	}
	if got["TARGET_ENV"] != "staging" {
		t.Errorf("fetchAutomationEnvVars() = %v, want map containing TARGET_ENV=staging", got)
	}
	if attempt != 2 {
		t.Errorf("server saw %d attempts, want exactly 2 (one failure, one success)", attempt)
	}
}

// TestFetchAutomationEnvVars_TerminalStatusNeverRetries mirrors
// TestFetchSandboxSecrets_TerminalStatusNeverRetries exactly: a 403 (gen
// mismatch, one of this delivery endpoint's own terminal handshake
// fences) must NOT be retried.
func TestFetchAutomationEnvVars_TerminalStatusNeverRetries(t *testing.T) {
	t.Parallel()

	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	cfg := boot.Config{
		SessionConfig: &sessionconfig.SessionConfig{
			ControlPlaneWsUrl: wsEquivalentForTest(server.URL),
			SessionId:         "sess-1",
			SandboxToken:      "tok",
			Gen:               1,
		},
	}

	timeouts := testTimeouts()
	timeouts.AutomationEnvVarFetchMaxAttempts = 3
	timeouts.AutomationEnvVarFetchRetryBaseDelay = 1
	timeouts.AutomationEnvVarFetchRetryMaxDelay = 1

	_, ok := fetchAutomationEnvVars(context.Background(), cfg, timeouts)
	if ok {
		t.Error("fetchAutomationEnvVars() ok = true, want false for a 403 response")
	}
	if attempts != 1 {
		t.Errorf("server saw %d attempts, want exactly 1 (a 403 is a terminal fence, never retried)", attempts)
	}
}

// TestFetchAutomationEnvVars_NilSessionConfigPanics mirrors
// TestFetchSandboxSecrets_NilSessionConfigPanics exactly -- see that
// test's own doc comment for the full "why" (the same "never reaches
// BuildImage" structural guard applies identically here: run()'s own call
// site only ever calls fetchAutomationEnvVars inside its
// `if cfg.SessionConfig != nil` branch).
func TestFetchAutomationEnvVars_NilSessionConfigPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("fetchAutomationEnvVars(cfg with nil SessionConfig) did not panic, want a nil-pointer panic (see this test's own doc comment)")
		}
	}()
	fetchAutomationEnvVars(context.Background(), boot.Config{}, testTimeouts())
}

// TestFetchAutomationEnvVars_DropsReservedNamesDeliveredByControlPlane is
// the defense-in-depth half mirroring
// TestFetchSandboxSecrets_DropsReservedNamesDeliveredByControlPlane
// exactly: internal/domain/automation.ValidateEnvVars already refuses a
// reserved name at CreateAutomation's own write path, but this proves the
// SECOND, independent enforcement at the injection boundary -- even if
// such a name were somehow delivered anyway, sandbox-agent drops it
// rather than injecting it. A targeted drop, never a whole-payload
// failure: the legitimate sibling name must survive.
func TestFetchAutomationEnvVars_DropsReservedNamesDeliveredByControlPlane(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"envVars": map[string]string{
			"ANTHROPIC_API_KEY": "attacker-supplied-key",
			"OPENCODE_CONFIG":   "/tmp/attacker.json",
			"NARVI_SOMETHING":   "reserved-too",
			"TARGET_ENV":        "staging",
		}})
	}))
	defer server.Close()

	cfg := boot.Config{
		SessionConfig: &sessionconfig.SessionConfig{
			ControlPlaneWsUrl: wsEquivalentForTest(server.URL),
			SessionId:         "sess-1",
			SandboxToken:      "tok",
			Gen:               1,
		},
	}

	got, ok := fetchAutomationEnvVars(context.Background(), cfg, testTimeouts())
	if !ok {
		t.Fatal("fetchAutomationEnvVars() ok = false, want true -- a reserved name is dropped, never a whole-payload failure")
	}
	for _, reserved := range []string{"ANTHROPIC_API_KEY", "OPENCODE_CONFIG", "NARVI_SOMETHING"} {
		if _, present := got[reserved]; present {
			t.Errorf("fetchAutomationEnvVars() kept reserved name %q, want it dropped before injection", reserved)
		}
	}
	if got["TARGET_ENV"] != "staging" {
		t.Errorf("fetchAutomationEnvVars()[TARGET_ENV] = %q, want %q -- the legitimate sibling must survive the drop", got["TARGET_ENV"], "staging")
	}

	// The dropped names must also be absent from what actually gets
	// threaded into the spawned opencode process, not merely from the map.
	for _, entry := range automationEnvVarSpawnEnv(got) {
		if strings.HasPrefix(entry, "OPENCODE_") || strings.HasPrefix(entry, "NARVI_") || strings.HasPrefix(entry, "ANTHROPIC_") {
			t.Errorf("automationEnvVarSpawnEnv() built reserved entry %q, want it never reach a spawned process", entry)
		}
	}
}
