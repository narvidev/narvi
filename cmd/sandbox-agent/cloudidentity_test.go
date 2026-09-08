package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/domain/sandboxboot"
	"github.com/narvidev/narvi/internal/platform"
	"github.com/narvidev/narvi/internal/sandboxagent/boot"
	"github.com/narvidev/narvi/internal/sandboxagent/credentials"
	"github.com/narvidev/narvi/internal/sandboxagent/supervisor"
)

// fakeMinter is a minimal, in-memory credentials.CloudIdentityTokenMinter
// -- no real HTTP round trip -- used throughout this file. callCount lets
// a test assert exactly how many attempts were made (bounded-retry
// pinning).
type fakeMinter struct {
	// tokens maps an audience to what to return -- an absent entry means
	// "no canned response, fail with 404".
	tokens map[string]credentials.MintedCloudIdentityToken
	// failFirstN makes the first N calls (across ALL audiences combined --
	// this fake is deliberately simple) fail with statusCode before
	// returning tokens[audience].
	failFirstN int
	// statusCode is the DeliveryStatusError status every failure reports.
	statusCode int

	callCount int
}

func (f *fakeMinter) MintCloudIdentityToken(_ context.Context, _, _ string, _ int, audience string) (credentials.MintedCloudIdentityToken, error) {
	f.callCount++
	if f.callCount <= f.failFirstN {
		return credentials.MintedCloudIdentityToken{}, &credentials.DeliveryStatusError{Endpoint: "cloud-identity-token", StatusCode: f.statusCode}
	}
	tok, ok := f.tokens[audience]
	if !ok {
		return credentials.MintedCloudIdentityToken{}, &credentials.DeliveryStatusError{Endpoint: "cloud-identity-token", StatusCode: http.StatusNotFound}
	}
	return tok, nil
}

// fastMintTimeouts is a Timeouts value tuned for fast, deterministic
// tests -- real bounded-retry semantics (attempts/backoff), just with a
// millisecond-scale clock instead of platform.DefaultTimeouts' own
// production-scale one.
func fastMintTimeouts() platform.Timeouts {
	return platform.Timeouts{
		CloudIdentityTokenMintTimeout:        time.Second,
		CloudIdentityTokenMintMaxAttempts:    3,
		CloudIdentityTokenMintRetryBaseDelay: time.Millisecond,
		CloudIdentityTokenMintRetryMaxDelay:  5 * time.Millisecond,
		CloudIdentityTokenLifetime:           10 * time.Minute,
	}
}

func testSessionConfig(sessionID, sandboxToken string, gen int) *sessionconfig.SessionConfig {
	return &sessionconfig.SessionConfig{
		SessionId:    sessionID,
		SandboxToken: sandboxToken,
		Gen:          gen,
	}
}

// --- mintCloudIdentityToken ---

func TestMintCloudIdentityToken_SucceedsFirstTry(t *testing.T) {
	m := &fakeMinter{tokens: map[string]credentials.MintedCloudIdentityToken{"aud": {Token: "tok", ExpiresAt: time.Now()}}}
	got, ok := mintCloudIdentityToken(context.Background(), m, "sess", "tok", 1, "aud", fastMintTimeouts())
	if !ok {
		t.Fatalf("mintCloudIdentityToken() ok = false, want true")
	}
	if got.Token != "tok" {
		t.Errorf("Token = %q, want %q", got.Token, "tok")
	}
	if m.callCount != 1 {
		t.Errorf("callCount = %d, want 1 (no retry needed)", m.callCount)
	}
}

func TestMintCloudIdentityToken_RetriesTransient5xxThenSucceeds(t *testing.T) {
	m := &fakeMinter{
		tokens:     map[string]credentials.MintedCloudIdentityToken{"aud": {Token: "tok"}},
		failFirstN: 2,
		statusCode: http.StatusInternalServerError,
	}
	_, ok := mintCloudIdentityToken(context.Background(), m, "sess", "tok", 1, "aud", fastMintTimeouts())
	if !ok {
		t.Fatalf("mintCloudIdentityToken() ok = false, want true (should have retried past 2 transient 500s)")
	}
	if m.callCount != 3 {
		t.Errorf("callCount = %d, want 3 (2 failures + 1 success)", m.callCount)
	}
}

// TestMintCloudIdentityToken_503IsTerminalNotRetried is this Step's own
// explicit gap resolution: a 503 (capability off / no signing key) from
// the mint endpoint must NOT be retried, unlike a generic 5xx -- pins
// classifyMintTokenError's own divergence from classifyDeliveryFetchError.
func TestMintCloudIdentityToken_503IsTerminalNotRetried(t *testing.T) {
	m := &fakeMinter{failFirstN: 100, statusCode: http.StatusServiceUnavailable}
	_, ok := mintCloudIdentityToken(context.Background(), m, "sess", "tok", 1, "aud", fastMintTimeouts())
	if ok {
		t.Fatalf("mintCloudIdentityToken() ok = true, want false (503 must be terminal)")
	}
	if m.callCount != 1 {
		t.Errorf("callCount = %d, want 1 -- a 503 must never be retried (classifyMintTokenError's own deliberate divergence from classifyDeliveryFetchError)", m.callCount)
	}
}

func TestMintCloudIdentityToken_403IsTerminalNotRetried(t *testing.T) {
	m := &fakeMinter{failFirstN: 100, statusCode: http.StatusForbidden}
	_, ok := mintCloudIdentityToken(context.Background(), m, "sess", "tok", 1, "aud", fastMintTimeouts())
	if ok {
		t.Fatalf("mintCloudIdentityToken() ok = true, want false")
	}
	if m.callCount != 1 {
		t.Errorf("callCount = %d, want 1 -- 403 (no binding declares this audience) must never be retried", m.callCount)
	}
}

// --- writeTokenFile ---

func TestWriteTokenFile_Permissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "token")
	if err := writeTokenFile(path, "secret-token-value"); err != nil {
		t.Fatalf("writeTokenFile() error = %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 0600", perm)
	}

	dirInfo, err := os.Stat(filepath.Join(dir, "sub"))
	if err != nil {
		t.Fatalf("stat token dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("token dir mode = %o, want 0700", perm)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(got) != "secret-token-value" {
		t.Errorf("token file content = %q, want %q", got, "secret-token-value")
	}
}

// --- resetCloudIdentityDir ---

// TestResetCloudIdentityDir_RemovesStaleFiles is this Step's own gap-2
// mutation-test-visible pinning: removing resetCloudIdentityDir's own
// os.RemoveAll call (or removing the call to resetCloudIdentityDir from
// run()) would let a stale file from a restored snapshot survive into a
// new boot -- this test proves the function itself makes that
// impossible for whatever dir already holds, and that dir is left empty
// and 0700 afterward.
func TestResetCloudIdentityDir_RemovesStaleFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	staleFile := filepath.Join(dir, "aws-token")
	if err := os.WriteFile(staleFile, []byte("stale-token-from-a-prior-boot"), 0o600); err != nil {
		t.Fatalf("seed stale file: %v", err)
	}
	staleKubeconfig := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(staleKubeconfig, []byte("stale-kubeconfig"), 0o600); err != nil {
		t.Fatalf("seed stale kubeconfig: %v", err)
	}

	resetCloudIdentityDir(dir)

	if _, err := os.Stat(staleFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale token file survived the reset: stat error = %v, want os.ErrNotExist", err)
	}
	if _, err := os.Stat(staleKubeconfig); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale kubeconfig survived the reset: stat error = %v, want os.ErrNotExist", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("recreated dir mode = %o, want 0700", perm)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("recreated dir is not empty: %v", entries)
	}
}

// TestResetCloudIdentityDir_FreshDirIsANoOp confirms the unconditional
// call site (run(), main.go -- every boot mode, not just
// snapshot_restore) is harmless when there is nothing stale to remove.
func TestResetCloudIdentityDir_FreshDirIsANoOp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	resetCloudIdentityDir(dir)
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %o, want 0700", perm)
	}
}

// --- cloudIdentityDir: outside every repo tree ---

// TestCloudIdentityDir_OutsideEveryRepoTree pins the property that stops
// a token being committed: cloudIdentityDir must never be, or be inside,
// cfg.WorkspaceDir (where every cloned repo lives) -- mirrors this
// codebase's own established "/narvi/..." convention
// (boot.ImageManifestPath, openCodeEnvironmentConfigPath) exactly.
func TestCloudIdentityDir_OutsideEveryRepoTree(t *testing.T) {
	if !filepath.IsAbs(cloudIdentityDir) {
		t.Fatalf("cloudIdentityDir = %q, want an absolute path", cloudIdentityDir)
	}
	// defaultWorkspaceDir (boot/config.go) -- deliberately re-typed here
	// rather than imported (that constant is unexported), matching this
	// codebase's own convention that a black-box _test.go pins an
	// unexported production value against its own documented literal.
	const workspaceDir = "/workspace"
	if strings.HasPrefix(cloudIdentityDir, workspaceDir) {
		t.Errorf("cloudIdentityDir = %q is inside the workspace (%q) -- a repo's own `git add` could commit it", cloudIdentityDir, workspaceDir)
	}
	if cloudIdentityDir == workspaceDir {
		t.Errorf("cloudIdentityDir must not equal the workspace dir itself")
	}
}

// --- applyCloudIdentityBinding: per-kind rendering ---

func awsBinding(audience, roleARN string) credentials.CloudIdentityConfigBinding {
	params, _ := json.Marshal(map[string]string{"roleArn": roleARN})
	return credentials.CloudIdentityConfigBinding{Kind: "aws", Audience: audience, Params: params}
}

// TestApplyCloudIdentityBinding_NeverMutatesTestProcessEnvironment is the
// mutation-test target for guard A (threaded, never os.Setenv): every
// value this file's own functions build is returned as a plain []string
// -- reverting to an os.Setenv-based mechanism (sandboxsecrets.go's own
// documented pre-fix incident, mirrored here for this Step's own values)
// would leak AWS_ROLE_ARN/etc. into the CALLING (here, the test) process's
// own os.Environ(). This test proves that never happens: the full
// os.Environ() snapshot before and after calling applyCloudIdentityBinding
// (and reading the resulting env back out) is byte-for-byte identical.
func TestApplyCloudIdentityBinding_NeverMutatesTestProcessEnvironment(t *testing.T) {
	before := os.Environ()

	dir := t.TempDir()
	m := &fakeMinter{tokens: map[string]credentials.MintedCloudIdentityToken{
		"sts.amazonaws.com": {Token: "aws-jwt"},
	}}
	binding := awsBinding("sts.amazonaws.com", "arn:aws:iam::123456789012:role/narvi")
	env, _, ok := applyCloudIdentityBinding(context.Background(), m, "sess-1", "tok", 1, binding, fastMintTimeouts(), dir)
	if !ok {
		t.Fatalf("applyCloudIdentityBinding() ok = false, want true")
	}
	if len(env) == 0 {
		t.Fatalf("env is empty, nothing to prove the isolation of")
	}

	for _, e := range env {
		name, _, _ := strings.Cut(e, "=")
		if v, isSet := os.LookupEnv(name); isSet {
			t.Errorf("applyCloudIdentityBinding leaked %q into the TEST process's own environment (value %q) -- must be threaded, never os.Setenv (see sandboxsecrets.go's own top doc comment for the incident this guards against)", name, v)
		}
	}

	after := os.Environ()
	if len(before) != len(after) {
		t.Fatalf("os.Environ() length changed: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("os.Environ()[%d] changed: before=%q after=%q", i, before[i], after[i])
		}
	}
}

func TestApplyCloudIdentityBinding_AWS(t *testing.T) {
	dir := t.TempDir()
	m := &fakeMinter{tokens: map[string]credentials.MintedCloudIdentityToken{
		"sts.amazonaws.com": {Token: "aws-jwt", ExpiresAt: time.Now().Add(10 * time.Minute)},
	}}
	binding := awsBinding("sts.amazonaws.com", "arn:aws:iam::123456789012:role/narvi")

	env, tokenPath, ok := applyCloudIdentityBinding(context.Background(), m, "sess-1", "sandbox-token", 3, binding, fastMintTimeouts(), dir)
	if !ok {
		t.Fatalf("applyCloudIdentityBinding() ok = false, want true")
	}
	wantPath := filepath.Join(dir, awsTokenFileName)
	if tokenPath != wantPath {
		t.Errorf("tokenPath = %q, want %q", tokenPath, wantPath)
	}

	assertEnvContains(t, env, map[string]string{
		"AWS_WEB_IDENTITY_TOKEN_FILE": wantPath,
		"AWS_ROLE_ARN":                "arn:aws:iam::123456789012:role/narvi",
		"AWS_ROLE_SESSION_NAME":       "narvi-sess-1",
	})

	got, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(got) != "aws-jwt" {
		t.Errorf("token file content = %q, want %q", got, "aws-jwt")
	}
}

func TestApplyCloudIdentityBinding_GCP(t *testing.T) {
	dir := t.TempDir()
	const wip = "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/pr"
	m := &fakeMinter{tokens: map[string]credentials.MintedCloudIdentityToken{wip: {Token: "gcp-jwt"}}}
	params, _ := json.Marshal(map[string]string{
		"workloadIdentityProvider": wip,
		"serviceAccountEmail":      "sa@project.iam.gserviceaccount.com",
	})
	binding := credentials.CloudIdentityConfigBinding{Kind: "gcp", Audience: wip, Params: params}

	env, tokenPath, ok := applyCloudIdentityBinding(context.Background(), m, "sess-1", "sandbox-token", 3, binding, fastMintTimeouts(), dir)
	if !ok {
		t.Fatalf("applyCloudIdentityBinding() ok = false, want true")
	}

	wantConfigPath := filepath.Join(dir, gcpCredentialConfigFileName)
	assertEnvContains(t, env, map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": wantConfigPath})

	tokenContent, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(tokenContent) != "gcp-jwt" {
		t.Errorf("token file content = %q, want %q", tokenContent, "gcp-jwt")
	}

	configContent, err := os.ReadFile(wantConfigPath)
	if err != nil {
		t.Fatalf("read gcp credential config: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(configContent, &parsed); err != nil {
		t.Fatalf("gcp credential config is not valid JSON: %v", err)
	}
	if parsed["type"] != "external_account" {
		t.Errorf("gcp credential config type = %v, want external_account", parsed["type"])
	}
	if s, _ := parsed["service_account_impersonation_url"].(string); s == "" {
		t.Errorf("gcp credential config missing service_account_impersonation_url despite serviceAccountEmail being set")
	}
	credSource, _ := parsed["credential_source"].(map[string]any)
	if credSource["file"] != tokenPath {
		t.Errorf("gcp credential_source.file = %v, want %q", credSource["file"], tokenPath)
	}
}

func TestApplyCloudIdentityBinding_Azure(t *testing.T) {
	dir := t.TempDir()
	m := &fakeMinter{tokens: map[string]credentials.MintedCloudIdentityToken{
		"api://AzureADTokenExchange": {Token: "azure-jwt"},
	}}
	params, _ := json.Marshal(map[string]string{"clientId": "client-1", "tenantId": "tenant-1"})
	binding := credentials.CloudIdentityConfigBinding{Kind: "azure", Audience: "api://AzureADTokenExchange", Params: params}

	env, tokenPath, ok := applyCloudIdentityBinding(context.Background(), m, "sess-1", "sandbox-token", 3, binding, fastMintTimeouts(), dir)
	if !ok {
		t.Fatalf("applyCloudIdentityBinding() ok = false, want true")
	}
	assertEnvContains(t, env, map[string]string{
		"AZURE_FEDERATED_TOKEN_FILE": tokenPath,
		"AZURE_CLIENT_ID":            "client-1",
		"AZURE_TENANT_ID":            "tenant-1",
	})
}

func TestApplyCloudIdentityBinding_Generic(t *testing.T) {
	dir := t.TempDir()
	m := &fakeMinter{tokens: map[string]credentials.MintedCloudIdentityToken{"vault-aud": {Token: "vault-jwt"}}}
	params, _ := json.Marshal(map[string]string{"envVar": "VAULT_TOKEN_PATH"})
	binding := credentials.CloudIdentityConfigBinding{Kind: "generic", Audience: "vault-aud", Params: params}

	env, tokenPath, ok := applyCloudIdentityBinding(context.Background(), m, "sess-1", "sandbox-token", 3, binding, fastMintTimeouts(), dir)
	if !ok {
		t.Fatalf("applyCloudIdentityBinding() ok = false, want true")
	}
	assertEnvContains(t, env, map[string]string{"VAULT_TOKEN_PATH": tokenPath})
}

// TestApplyCloudIdentityBinding_GenericRejectsReservedEnvVar is a direct
// mutation-test target for the reservation guard (§27.3's own "one
// owning mechanism per env-var name"): a generic binding whose params
// name an ALREADY-reserved var (here, one of §27.3's OWN fixed AWS
// names) must be refused, never threaded into the spawn env -- proving
// the sandboxsecret.ValidateName call inside applyCloudIdentityBinding's
// own KindGeneric branch is load-bearing. Removing that call makes this
// test fail (env would be ["AWS_ROLE_ARN=<path>"] instead of nil).
func TestApplyCloudIdentityBinding_GenericRejectsReservedEnvVar(t *testing.T) {
	dir := t.TempDir()
	m := &fakeMinter{tokens: map[string]credentials.MintedCloudIdentityToken{"vault-aud": {Token: "vault-jwt"}}}
	params, _ := json.Marshal(map[string]string{"envVar": "AWS_ROLE_ARN"})
	binding := credentials.CloudIdentityConfigBinding{Kind: "generic", Audience: "vault-aud", Params: params}

	env, tokenPath, ok := applyCloudIdentityBinding(context.Background(), m, "sess-1", "sandbox-token", 3, binding, fastMintTimeouts(), dir)
	if ok || env != nil || tokenPath != "" {
		t.Fatalf("applyCloudIdentityBinding() = (%v, %q, %v), want (nil, \"\", false) -- a generic binding naming an already-reserved env var must be refused", env, tokenPath, ok)
	}
}

func TestApplyCloudIdentityBinding_UnrecognizedKind(t *testing.T) {
	dir := t.TempDir()
	binding := credentials.CloudIdentityConfigBinding{Kind: "digitalocean", Audience: "aud", Params: json.RawMessage(`{}`)}
	env, tokenPath, ok := applyCloudIdentityBinding(context.Background(), &fakeMinter{}, "sess-1", "tok", 1, binding, fastMintTimeouts(), dir)
	if ok || env != nil || tokenPath != "" {
		t.Fatalf("applyCloudIdentityBinding() = (%v, %q, %v), want (nil, \"\", false)", env, tokenPath, ok)
	}
}

func TestApplyCloudIdentityBinding_InvalidParams(t *testing.T) {
	dir := t.TempDir()
	binding := credentials.CloudIdentityConfigBinding{Kind: "aws", Audience: "aud", Params: json.RawMessage(`{}`)} // missing roleArn
	env, tokenPath, ok := applyCloudIdentityBinding(context.Background(), &fakeMinter{}, "sess-1", "tok", 1, binding, fastMintTimeouts(), dir)
	if ok || env != nil || tokenPath != "" {
		t.Fatalf("applyCloudIdentityBinding() = (%v, %q, %v), want (nil, \"\", false)", env, tokenPath, ok)
	}
}

// TestApplyCloudIdentityBinding_MintFailureSkipsBinding is this Step's
// own gap-1 resolution applied to the INITIAL population path: a binding
// whose very first mint exhausts its retries contributes NOTHING (no env,
// no token file) -- never a boot failure, never a half-written file.
func TestApplyCloudIdentityBinding_MintFailureSkipsBinding(t *testing.T) {
	dir := t.TempDir()
	m := &fakeMinter{failFirstN: 100, statusCode: http.StatusForbidden}
	binding := awsBinding("sts.amazonaws.com", "arn:aws:iam::123456789012:role/narvi")
	env, tokenPath, ok := applyCloudIdentityBinding(context.Background(), m, "sess-1", "tok", 1, binding, fastMintTimeouts(), dir)
	if ok || env != nil || tokenPath != "" {
		t.Fatalf("applyCloudIdentityBinding() = (%v, %q, %v), want (nil, \"\", false)", env, tokenPath, ok)
	}
	if _, err := os.Stat(filepath.Join(dir, awsTokenFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("token file must not exist after a failed mint, stat error = %v", err)
	}
}

// --- populateCloudIdentityTokenFiles ---

func TestPopulateCloudIdentityTokenFiles_MultipleBindings(t *testing.T) {
	dir := t.TempDir()
	m := &fakeMinter{tokens: map[string]credentials.MintedCloudIdentityToken{
		"sts.amazonaws.com":          {Token: "aws-jwt"},
		"api://AzureADTokenExchange": {Token: "azure-jwt"},
	}}
	awsParams, _ := json.Marshal(map[string]string{"roleArn": "arn:aws:iam::123456789012:role/narvi"})
	azureParams, _ := json.Marshal(map[string]string{"clientId": "c", "tenantId": "t"})
	bindings := []credentials.CloudIdentityConfigBinding{
		{Kind: "aws", Audience: "sts.amazonaws.com", Params: awsParams},
		{Kind: "azure", Audience: "api://AzureADTokenExchange", Params: azureParams},
	}

	cfg := boot.Config{SessionConfig: testSessionConfig("sess-1", "sandbox-token", 3)}
	env, states := populateCloudIdentityTokenFiles(context.Background(), cfg, fastMintTimeouts(), m, bindings, dir)

	if len(states) != 2 {
		t.Fatalf("len(states) = %d, want 2", len(states))
	}
	assertEnvContains(t, env, map[string]string{
		"AWS_ROLE_ARN":    "arn:aws:iam::123456789012:role/narvi",
		"AZURE_CLIENT_ID": "c",
		"AZURE_TENANT_ID": "t",
	})
}

// --- refreshOneBinding ---

func TestRefreshOneBinding_SuccessRewritesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aws-token")
	if err := writeTokenFile(path, "old-token"); err != nil {
		t.Fatalf("seed old token: %v", err)
	}

	m := &fakeMinter{tokens: map[string]credentials.MintedCloudIdentityToken{"aud": {Token: "new-token", ExpiresAt: time.Now().Add(10 * time.Minute)}}}
	ctx, cancel := context.WithCancel(context.Background())
	timeouts := fastMintTimeouts()
	timeouts.CloudIdentityTokenLifetime = 2 * time.Millisecond // half-life = 1ms, so the loop ticks almost immediately

	cfg := boot.Config{SessionConfig: testSessionConfig("sess-1", "tok", 1)}
	done := make(chan struct{})
	// errgroup.Group.Go, not a bare `go` statement: §11's no-naked-
	// goroutine rule (tools/lint/narvichecks/nakedgoroutine) applies to
	// tests too -- mirrors processgroup_test.go's own identical "local
	// Group exists solely as a lint-satisfying Go() call site, never
	// Wait()ed on -- done is this function's own actual synchronization
	// signal" precedent.
	var group errgroup.Group
	group.Go(func() error {
		refreshOneBinding(ctx, cfg, timeouts, m, cloudIdentityBindingState{Kind: "aws", Audience: "aud", TokenPath: path})
		close(done)
		return nil
	})

	deadline := time.After(2 * time.Second)
	for {
		got, _ := os.ReadFile(path)
		if string(got) == "new-token" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("token file was never refreshed to the new value; last read = %q", got)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

// TestRefreshOneBinding_DeletesFileOnExhaustedRetries is this Step's own
// gap-1 resolution's direct mutation-test target: a refresh whose every
// retry attempt fails must DELETE the stale token file, not leave it
// sitting there to simply expire.
func TestRefreshOneBinding_DeletesFileOnExhaustedRetries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aws-token")
	if err := writeTokenFile(path, "stale-token"); err != nil {
		t.Fatalf("seed stale token: %v", err)
	}

	m := &fakeMinter{failFirstN: 1000, statusCode: http.StatusForbidden}
	ctx, cancel := context.WithCancel(context.Background())
	timeouts := fastMintTimeouts()
	timeouts.CloudIdentityTokenLifetime = 2 * time.Millisecond

	cfg := boot.Config{SessionConfig: testSessionConfig("sess-1", "tok", 1)}
	done := make(chan struct{})
	// errgroup.Group.Go, not a bare `go` statement -- see
	// TestRefreshOneBinding_SuccessRewritesFile's own identical comment.
	var group errgroup.Group
	group.Go(func() error {
		refreshOneBinding(ctx, cfg, timeouts, m, cloudIdentityBindingState{Kind: "aws", Audience: "aud", TokenPath: path})
		close(done)
		return nil
	})

	deadline := time.After(2 * time.Second)
	for {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("stale token file was never removed after exhausted refresh retries")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

// --- runCloudIdentityRefreshLoop ---

func TestRunCloudIdentityRefreshLoop_EmptyStatesBlocksThenReturnsOnCtxDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// errgroup.Group.Go, not a bare `go` statement -- see
	// TestRefreshOneBinding_SuccessRewritesFile's own identical comment.
	var group errgroup.Group
	group.Go(func() error {
		done <- runCloudIdentityRefreshLoop(ctx, boot.Config{}, fastMintTimeouts(), &fakeMinter{}, nil)
		return nil
	})

	select {
	case <-done:
		t.Fatal("runCloudIdentityRefreshLoop returned before ctx was done, with zero states")
	case <-time.After(20 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runCloudIdentityRefreshLoop() error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runCloudIdentityRefreshLoop did not return after ctx was canceled")
	}
}

// --- real-process proof: cloud identity token file reaches a REAL
// spawned hook through the existing hook path ---

// TestCloudIdentityTokenReachesRealSpawnedHook proves the file-based
// consumption mechanism §27.3 specifies actually works end to end,
// rather than merely asserting it: a REAL process (a spawned setup.sh,
// via boot.RunHooks -- the SAME threaded-env seam every OTHER §27.1/§27.3
// injected value already goes through) reads the token file at the exact
// path the AWS_WEB_IDENTITY_TOKEN_FILE env var advertises, using ONLY
// that env var -- never a hardcoded path -- and the file's own content is
// exactly what applyCloudIdentityBinding minted, via a fake CP client
// (no real network dependency).
//
// On the 5-second hookTimeout below: this test (with its
// TestOIDCClusterBindingTokenReachesRealSpawnedHook sibling,
// kubeconfig_test.go) failed once under a fully-loaded local `make test`
// with concurrent compile load and passed in isolation immediately after.
// Measured rather than assumed before touching the ceiling: two
// independent full `go test -race ./...` runs (the same concurrent-load
// shape that produced the original failure, on a machine already at a
// 2x-oversubscribed load average from unrelated concurrent work) both
// timed this test's actual setup.sh round trip at 0.32s -- under 7% of
// the 5s budget, a ~15x margin, identical across both runs. That is a
// real spawn-latency measurement under load, not a clean-room number, and
// it does not support raising the ceiling: the one observed failure reads
// as a transient extreme spike consistent with its own "passed isolated
// immediately after" observation, not as evidence the budget is too
// tight. Left unchanged.
func TestCloudIdentityTokenReachesRealSpawnedHook(t *testing.T) {
	dir := t.TempDir()
	m := &fakeMinter{tokens: map[string]credentials.MintedCloudIdentityToken{"sts.amazonaws.com": {Token: "real-spawned-process-jwt"}}}
	binding := awsBinding("sts.amazonaws.com", "arn:aws:iam::123456789012:role/narvi")
	env, _, ok := applyCloudIdentityBinding(context.Background(), m, "sess-1", "tok", 1, binding, fastMintTimeouts(), dir)
	if !ok {
		t.Fatalf("applyCloudIdentityBinding() ok = false, want true")
	}

	workspaceDir := t.TempDir()
	probeFile := filepath.Join(t.TempDir(), "probe")
	writeCloudIdentityTestScript(t, filepath.Join(workspaceDir, "repo-a", "setup.sh"),
		`cat "$AWS_WEB_IDENTITY_TOKEN_FILE" > `+probeFile)

	sup := supervisor.New()
	repos := []boot.RepoInfo{{Name: "repo-a", Primary: true}}

	err := boot.RunHooks(context.Background(), sup, workspaceDir, repos, sandboxboot.BootModeBuild, nil, nil, env,
		func(_, _, _ string, _, _ bool, _ float64) {}, 5*time.Second, time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("RunHooks() error = %v, want nil", err)
	}

	got, readErr := os.ReadFile(probeFile)
	if readErr != nil {
		t.Fatalf("read probe file: %v", readErr)
	}
	if string(got) != "real-spawned-process-jwt" {
		t.Errorf("token content as read by the REAL spawned setup.sh (via $AWS_WEB_IDENTITY_TOKEN_FILE) = %q, want %q", got, "real-spawned-process-jwt")
	}
}

// --- test helpers ---

func writeCloudIdentityTestScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	content := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func assertEnvContains(t *testing.T, env []string, want map[string]string) {
	t.Helper()
	got := make(map[string]string, len(env))
	for _, e := range env {
		name, value, _ := strings.Cut(e, "=")
		got[name] = value
	}
	for name, wantValue := range want {
		gotValue, ok := got[name]
		if !ok {
			t.Errorf("env missing %q, want %q=%q (full env: %v)", name, name, wantValue, env)
			continue
		}
		if gotValue != wantValue {
			t.Errorf("env[%q] = %q, want %q", name, gotValue, wantValue)
		}
	}
}
