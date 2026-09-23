package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/domain/sandboxsecret"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/credentials"
)

// testTimeouts is a small platform.Timeouts value for this package's own
// fast, deterministic tests -- a single attempt (MaxAttempts: 1) unless a
// specific test overrides it, so a test exercising a genuine failure path
// does not also have to wait out retry backoff.
func testTimeouts() platform.Timeouts {
	return platform.Timeouts{
		SandboxSecretFetchTimeout:        testSandboxSecretsFetchTimeout,
		SandboxSecretFetchMaxAttempts:    1,
		SandboxSecretFetchRetryBaseDelay: 1,
		SandboxSecretFetchRetryMaxDelay:  1,

		OpenCodeConfigFetchTimeout:        testSandboxSecretsFetchTimeout,
		OpenCodeConfigFetchMaxAttempts:    1,
		OpenCodeConfigFetchRetryBaseDelay: 1,
		OpenCodeConfigFetchRetryMaxDelay:  1,

		AutomationEnvVarFetchTimeout:        testSandboxSecretsFetchTimeout,
		AutomationEnvVarFetchMaxAttempts:    1,
		AutomationEnvVarFetchRetryBaseDelay: 1,
		AutomationEnvVarFetchRetryMaxDelay:  1,
	}
}

// TestSandboxSecretValidateName_RejectsOpenCodeConfigEnvVar is the direct
// regression test for an adversarial-review CRITICAL fix: this
// binary's own openCodeConfigEnvVar constant (the literal name this file
// injects) must itself be rejected by sandboxsecret.ValidateName -- proof
// the reservation (internal/domain/sandboxsecret.OpenCodeReservedPrefix)
// and this binary's own injection can never silently drift apart, since
// openCodeConfigEnvVar is built FROM that exact same exported constant
// (see this file's own opencodeconfig.go, "openCodeConfigEnvVar = sandboxsecret.OpenCodeReservedPrefix + "CONFIG""). If a future edit
// ever changed openCodeConfigEnvVar to a literal outside the reserved
// namespace, this test would fail immediately.
func TestSandboxSecretValidateName_RejectsOpenCodeConfigEnvVar(t *testing.T) {
	err := sandboxsecret.ValidateName(openCodeConfigEnvVar)
	if !errors.Is(err, sandboxsecret.ErrNameReservedOpenCodeNamespace) {
		t.Errorf("sandboxsecret.ValidateName(%q) = %v, want ErrNameReservedOpenCodeNamespace", openCodeConfigEnvVar, err)
	}
}

// TestApplyOpenCodeConfig_WritesBothDocuments proves the real mechanism:
// the global document lands at <homeDir>/.config/opencode/opencode.json,
// the environment document lands at the given environmentConfigPath, and
// the returned env slice carries exactly one OPENCODE_CONFIG=<path> entry
// -- adversarial-review HIGH fix: this is now a THREADED return value,
// never an os.Setenv call onto this test process's own environment (the
// pre-fix mechanism). Uses a t.TempDir()-based environmentConfigPath
// rather than the real openCodeEnvironmentConfigPath constant (production
// writes to "/narvi", root-owned/read-only in a local dev/test
// environment) -- applyOpenCodeConfig's own environmentConfigPath
// parameter exists specifically so this is possible without touching real
// filesystem roots.
func TestApplyOpenCodeConfig_WritesBothDocuments(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	envConfigPath := filepath.Join(t.TempDir(), "opencode-environment-config.json")

	delivery := credentials.OpenCodeConfigDelivery{
		Global:      []byte(`{"autoupdate":true}`),
		Environment: []byte(`{"model":"anthropic/claude"}`),
	}

	gotEnv := applyOpenCodeConfig(delivery, true, homeDir, envConfigPath)

	globalPath := filepath.Join(homeDir, openCodeGlobalConfigRelPath)
	globalBytes, err := os.ReadFile(globalPath)
	if err != nil {
		t.Fatalf("read global config at %s: %v", globalPath, err)
	}
	if string(globalBytes) != `{"autoupdate":true}` {
		t.Errorf("global config content = %s, want %s", globalBytes, `{"autoupdate":true}`)
	}

	envBytes, err := os.ReadFile(envConfigPath)
	if err != nil {
		t.Fatalf("read environment config at %s: %v", envConfigPath, err)
	}
	if string(envBytes) != `{"model":"anthropic/claude"}` {
		t.Errorf("environment config content = %s, want %s", envBytes, `{"model":"anthropic/claude"}`)
	}
	wantEnv := []string{openCodeConfigEnvVar + "=" + envConfigPath}
	if len(gotEnv) != 1 || gotEnv[0] != wantEnv[0] {
		t.Errorf("applyOpenCodeConfig() env = %v, want %v", gotEnv, wantEnv)
	}
}

// TestApplyOpenCodeConfig_GlobalAbsent_OnlyWritesEnvironment proves the
// two writes are independent: an absent global document writes nothing
// to the global slot, but the environment document still lands.
func TestApplyOpenCodeConfig_GlobalAbsent_OnlyWritesEnvironment(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	envConfigPath := filepath.Join(t.TempDir(), "opencode-environment-config.json")

	delivery := credentials.OpenCodeConfigDelivery{
		Environment: []byte(`{"model":"anthropic/claude"}`),
	}
	gotEnv := applyOpenCodeConfig(delivery, true, homeDir, envConfigPath)

	globalPath := filepath.Join(homeDir, openCodeGlobalConfigRelPath)
	if _, err := os.Stat(globalPath); err == nil {
		t.Errorf("global config file exists at %s, want absent (delivery.Global was empty)", globalPath)
	}
	wantEnv := []string{openCodeConfigEnvVar + "=" + envConfigPath}
	if len(gotEnv) != 1 || gotEnv[0] != wantEnv[0] {
		t.Errorf("applyOpenCodeConfig() env = %v, want %v", gotEnv, wantEnv)
	}
}

// TestApplyOpenCodeConfig_BothAbsent_TouchesNothing proves the
// overwhelming common case (nothing configured at either scope, a
// successful fetch) leaves no env entry and writes no files at all.
func TestApplyOpenCodeConfig_BothAbsent_TouchesNothing(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	envConfigPath := filepath.Join(t.TempDir(), "opencode-environment-config.json")

	gotEnv := applyOpenCodeConfig(credentials.OpenCodeConfigDelivery{}, true, homeDir, envConfigPath)

	globalPath := filepath.Join(homeDir, openCodeGlobalConfigRelPath)
	if _, err := os.Stat(globalPath); err == nil {
		t.Errorf("global config file exists at %s, want absent", globalPath)
	}
	if _, err := os.Stat(envConfigPath); err == nil {
		t.Errorf("environment config file exists at %s, want absent", envConfigPath)
	}
	if gotEnv != nil {
		t.Errorf("applyOpenCodeConfig() env = %v, want nil", gotEnv)
	}
}

// TestApplyOpenCodeConfig_SuccessfulEmptyFetch_RemovesStaleFiles is the
// direct regression test for the adversarial-review MEDIUM finding
// (configuration revocation across a snapshot restore, §27.1/§27.2): a
// SUCCESSFUL fetch reporting nothing configured at either scope must
// REMOVE whatever stale files a PRIOR boot left on disk -- otherwise an
// admin deleting a global/environment opencode_configs row server-side
// would never actually take effect on a restored snapshot, since that
// filesystem persists across boots.
func TestApplyOpenCodeConfig_SuccessfulEmptyFetch_RemovesStaleFiles(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	envConfigPath := filepath.Join(t.TempDir(), "opencode-environment-config.json")

	globalPath := filepath.Join(homeDir, openCodeGlobalConfigRelPath)
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(globalPath, []byte(`{"stale":true}`), 0o644); err != nil {
		t.Fatalf("seed stale global config: %v", err)
	}
	if err := os.WriteFile(envConfigPath, []byte(`{"stale":true}`), 0o644); err != nil {
		t.Fatalf("seed stale environment config: %v", err)
	}

	// A successful fetch (fetchOK=true) reporting NOTHING configured at
	// either scope -- e.g. an admin deleted both rows server-side since
	// the image/snapshot this workspace was baked from.
	gotEnv := applyOpenCodeConfig(credentials.OpenCodeConfigDelivery{}, true, homeDir, envConfigPath)

	if _, err := os.Stat(globalPath); !os.IsNotExist(err) {
		t.Errorf("stale global config at %s still exists after a successful empty fetch, want removed (err = %v)", globalPath, err)
	}
	if _, err := os.Stat(envConfigPath); !os.IsNotExist(err) {
		t.Errorf("stale environment config at %s still exists after a successful empty fetch, want removed (err = %v)", envConfigPath, err)
	}
	if gotEnv != nil {
		t.Errorf("applyOpenCodeConfig() env = %v, want nil (no environment document, and the stale one was removed)", gotEnv)
	}
}

// TestApplyOpenCodeConfig_FailedFetch_KeepsStaleFilesAndEnv is the
// complementary regression test: a FAILED fetch (fetchOK=false, every
// retry attempt exhausted) must NEVER remove what is already on disk --
// this is the deliberate "keep last known good" judgement call
// (applyOpenCodeConfig's own doc comment cites this codebase's
// established §13.2/identitylink precedent: never null-out on a
// transient failure). The environment document's own env entry must
// still be returned, pointing at the file that is still genuinely there.
func TestApplyOpenCodeConfig_FailedFetch_KeepsStaleFilesAndEnv(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	envConfigPath := filepath.Join(t.TempDir(), "opencode-environment-config.json")

	globalPath := filepath.Join(homeDir, openCodeGlobalConfigRelPath)
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(globalPath, []byte(`{"lastKnownGood":true}`), 0o644); err != nil {
		t.Fatalf("seed last-known-good global config: %v", err)
	}
	if err := os.WriteFile(envConfigPath, []byte(`{"lastKnownGood":true}`), 0o644); err != nil {
		t.Fatalf("seed last-known-good environment config: %v", err)
	}

	gotEnv := applyOpenCodeConfig(credentials.OpenCodeConfigDelivery{}, false, homeDir, envConfigPath)

	globalBytes, err := os.ReadFile(globalPath)
	if err != nil {
		t.Fatalf("read global config: %v", err)
	}
	if string(globalBytes) != `{"lastKnownGood":true}` {
		t.Errorf("global config content = %s, want unchanged %s (a failed fetch must never remove it)", globalBytes, `{"lastKnownGood":true}`)
	}
	envBytes, err := os.ReadFile(envConfigPath)
	if err != nil {
		t.Fatalf("read environment config: %v", err)
	}
	if string(envBytes) != `{"lastKnownGood":true}` {
		t.Errorf("environment config content = %s, want unchanged %s (a failed fetch must never remove it)", envBytes, `{"lastKnownGood":true}`)
	}
	wantEnv := []string{openCodeConfigEnvVar + "=" + envConfigPath}
	if len(gotEnv) != 1 || gotEnv[0] != wantEnv[0] {
		t.Errorf("applyOpenCodeConfig() env = %v, want %v (still pointing at the last-known-good file)", gotEnv, wantEnv)
	}
}

// TestApplyOpenCodeConfig_FailedFetch_NoStaleFile_NoEnv proves the failed-
// fetch path on a FRESH sandbox (nothing ever written to environmentConfigPath
// by a prior boot): no file to point at means no env entry either --
// "as if this feature did not exist", exactly as fetchOpenCodeConfig's own
// doc comment describes for that specific (first-boot) case.
func TestApplyOpenCodeConfig_FailedFetch_NoStaleFile_NoEnv(t *testing.T) {
	t.Parallel()

	homeDir := t.TempDir()
	envConfigPath := filepath.Join(t.TempDir(), "opencode-environment-config.json")

	gotEnv := applyOpenCodeConfig(credentials.OpenCodeConfigDelivery{}, false, homeDir, envConfigPath)

	if _, err := os.Stat(envConfigPath); !os.IsNotExist(err) {
		t.Errorf("environment config file unexpectedly created at %s", envConfigPath)
	}
	if gotEnv != nil {
		t.Errorf("applyOpenCodeConfig() env = %v, want nil (no file ever existed to point at)", gotEnv)
	}
}

// TestFetchOpenCodeConfig_RealRoundTrip mirrors
// TestFetchSandboxSecrets_RealRoundTrip -- this package's own thin
// wrapper (client construction, logging, degrade-on-failure) over
// CPClient.FetchOpenCodeConfig, whose own wire-shape correctness is
// covered exhaustively in internal/sandboxagent/credentials' own test
// suite.
func TestFetchOpenCodeConfig_RealRoundTrip(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"global":{"autoupdate":true}}`))
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

	got, ok := fetchOpenCodeConfig(context.Background(), cfg, testTimeouts())
	if !ok {
		t.Fatal("fetchOpenCodeConfig() ok = false, want true")
	}
	if string(got.Global) != `{"autoupdate":true}` {
		t.Errorf("fetchOpenCodeConfig().Global = %s, want %s", got.Global, `{"autoupdate":true}`)
	}
}

// TestFetchOpenCodeConfig_DegradesToZeroValueOnFailure proves the "warn
// and continue" posture directly: every retry attempt failing (here, a
// persistent 500) degrades to a zero-value delivery and ok=false.
func TestFetchOpenCodeConfig_DegradesToZeroValueOnFailure(t *testing.T) {
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

	got, ok := fetchOpenCodeConfig(context.Background(), cfg, testTimeouts())
	if ok {
		t.Error("fetchOpenCodeConfig() ok = true, want false on a persistent 500 response")
	}
	if len(got.Global) != 0 || len(got.Environment) != 0 {
		t.Errorf("fetchOpenCodeConfig() = %+v, want zero value on a 500 response", got)
	}
}

// TestFetchOpenCodeConfig_RetriesTransientFailureThenSucceeds is the
// direct regression test for the adversarial-review MEDIUM fix (§27.1:
// "with bounded retry"): a 500 on the first attempt, then a 2xx on the
// second, must still succeed -- proving this is a genuine multi-attempt
// retry loop, not merely a differently-worded single attempt.
func TestFetchOpenCodeConfig_RetriesTransientFailureThenSucceeds(t *testing.T) {
	t.Parallel()

	attempt := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt++
		if attempt == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"global":{"autoupdate":true}}`))
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
	timeouts.OpenCodeConfigFetchMaxAttempts = 3
	timeouts.OpenCodeConfigFetchRetryBaseDelay = 1
	timeouts.OpenCodeConfigFetchRetryMaxDelay = 1

	got, ok := fetchOpenCodeConfig(context.Background(), cfg, timeouts)
	if !ok {
		t.Fatal("fetchOpenCodeConfig() ok = false, want true (the second attempt succeeds)")
	}
	if string(got.Global) != `{"autoupdate":true}` {
		t.Errorf("fetchOpenCodeConfig().Global = %s, want %s", got.Global, `{"autoupdate":true}`)
	}
	if attempt != 2 {
		t.Errorf("server saw %d attempts, want exactly 2 (one failure, one success)", attempt)
	}
}

// TestFetchOpenCodeConfig_TerminalStatusNeverRetries is the direct
// regression test for the OTHER half of the bounded-retry fix: a 404
// (unknown session -- one of this delivery endpoint's own terminal
// handshake fences) must NOT be retried, even though MaxAttempts allows
// for it -- retrying a fence that can never resolve differently only adds
// load for zero chance of success.
func TestFetchOpenCodeConfig_TerminalStatusNeverRetries(t *testing.T) {
	t.Parallel()

	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusNotFound)
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
	timeouts.OpenCodeConfigFetchMaxAttempts = 3
	timeouts.OpenCodeConfigFetchRetryBaseDelay = 1
	timeouts.OpenCodeConfigFetchRetryMaxDelay = 1

	_, ok := fetchOpenCodeConfig(context.Background(), cfg, timeouts)
	if ok {
		t.Error("fetchOpenCodeConfig() ok = true, want false for a 404 response")
	}
	if attempts != 1 {
		t.Errorf("server saw %d attempts, want exactly 1 (a 404 is a terminal fence, never retried)", attempts)
	}
}

// TestFetchOpenCodeConfig_NilSessionConfigPanics mirrors
// TestFetchSandboxSecrets_NilSessionConfigPanics exactly -- see that
// test's own doc comment for the full "never reaches BuildImage"
// reasoning this canary defends.
func TestFetchOpenCodeConfig_NilSessionConfigPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("fetchOpenCodeConfig(cfg with nil SessionConfig) did not panic, want a nil-pointer panic (see this test's own doc comment)")
		}
	}()
	fetchOpenCodeConfig(context.Background(), boot.Config{}, testTimeouts())
}
