//go:build integration

package sessionactor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves the wiring an independent verifier's review of this
// Step found missing: a real execution_complete completing a Processing
// turn actually sends a real push command (sendPushBestEffort), and a
// real push_complete actually calls ports.SourceControl.CreatePR and
// records the result as a "pr"-typed artifact (createPRBestEffort) --
// see pushpr.go's own top comment for the full design.

// testTokenEncryptionKey is a fixed, obviously-fake 32-byte AES-256-GCM
// key -- exactly the length platform.EncryptToken/DecryptToken require
// (see internal/platform/tokenencrypt.go), used only by this file's own
// tests to round-trip a fake GitHub access token through the real
// encrypt/decrypt path, never a real secret.
var testTokenEncryptionKey = []byte("0123456789abcdef0123456789abcdef")

// fakeSourceControl is a test-only ports.SourceControl recording every
// CreatePR call it receives and returning a caller-configured (ref, err)
// pair.
//
// §8.5 ("image builds") extends this same fake with an identical
// recording shape for ResolveBranchSHA (shaCalls/nextSHA/nextSHAErr, plus
// an optional per-repo shaFor map so a test can return DIFFERENT SHAs for
// different repos in one multi-repo session) rather than introducing a
// second, parallel fake -- this package's own imageresolve tests need the
// exact same CreatePR-call-recording precedent, just for the other
// SourceControl method dispatch.go's resolveAndSetImage now calls.
//
// §14.3 ("mocking + contract drift") extends this SAME fake again, the
// identical way, for ResolveContractsFingerprint (fingerprintCalls/
// nextFingerprint/nextFingerprintExists/nextFingerprintErr, plus an
// optional per-repo fingerprintFor map) -- this package's own
// contractdrift tests need the exact same recording precedent for the
// third SourceControl method checkContractDrift (contractdrift.go) calls.
type fakeSourceControl struct {
	mu      sync.Mutex
	calls   []ports.CreatePRSpec
	nextRef ports.PRRef
	nextErr error

	// suppressCalls records SuppressCreatePR, the shadow decorator's own
	// suppress-and-record entry point (internal/app/shadowscm). Kept
	// SEPARATE from calls so a test can tell the two apart: "the PR was
	// suppressed" and "nothing happened at all" are different outcomes,
	// and only one of them leaves a ledger row. A count of zero on BOTH
	// is the silent-skip regression, not a pass.
	suppressCalls []ports.CreatePRSpec

	shaCalls   []ports.ResolveBranchSHASpec
	shaFor     map[string]string // keyed by repo name; falls back to nextSHA if absent
	nextSHA    string
	nextSHAErr error
	// defaultBranchName is the resolvedBranch ResolveBranchSHA returns
	// when the caller's spec.Branch is empty (simulating the real
	// adapter's own "resolve the repo's actual default branch name"
	// behavior, githubapi.Adapter.ResolveBranchSHA) -- defaults to "main"
	// (a realistic default-branch name) when a test never sets it.
	defaultBranchName string

	fingerprintCalls      []ports.ResolveContractsFingerprintSpec
	fingerprintFor        map[string]string // keyed by repo name; overrides nextFingerprint if present
	existsFor             map[string]bool   // keyed by repo name; overrides nextFingerprintExists if present
	nextFingerprint       string
	nextFingerprintExists bool
	nextFingerprintErr    error

	// accessCalls/accessAllowedFor/denyAllAccess/accessErr/accessErrFor are
	// the audit fix's ("warm-boot image access control", HIGH) own
	// extension of this same fake, for CheckRepoAccess -- the fourth
	// SourceControl method imageresolve.go's own repoAccessAllowedForSpawn
	// now calls. Defaults to ALLOWING every repo (accessAllowedFor/
	// denyAllAccess both unset/false) so every EXISTING test in this
	// package that configures a fakeSourceControl for an unrelated reason
	// (CreatePR, ResolveContractsFingerprint) and happens to also exercise
	// the spawn/dispatch path keeps passing unmodified -- only this file's
	// own and imagebuild_integration_test.go's own repo-access-specific
	// tests need to configure this explicitly.
	accessCalls      []ports.CheckRepoAccessSpec
	accessCtxs       []context.Context // parallel to accessCalls -- the ctx CheckRepoAccess actually received, each call
	accessAllowedFor map[string]bool   // keyed by "owner/repo"; overrides denyAllAccess if present
	denyAllAccess    bool
	accessErr        error
	accessErrFor     map[string]error // keyed by "owner/repo"; overrides accessErr if present

	// registerStackCalls ("sentinels + suggestions", §17.2/§17.6)
	// is this fake's own extension for RegisterPRStack -- recorded so a
	// test can prove createSentinelFixPRBestEffort (pushpr.go) calls it
	// exactly once, with the origin+fix PR numbers bottom-to-top, AFTER
	// CreatePR succeeds.
	registerStackCalls []ports.RegisterPRStackSpec
	registerStackErr   error

	// diffCalls/nextDiff/nextDiffErr are §14.4's ("handoff-readiness
	// sentinel", §14.4) own extension of this same fake for
	// GetPullRequestDiff -- this fake ALSO satisfies PRDiffFetcher
	// (handoffsentinel.go), mirroring how the real production adapter
	// (*githubapi.Adapter) is passed as both ports.SourceControl AND
	// sessionactor.PRDiffFetcher (cmd/control-plane/main.go's own doc
	// comment on that call site).
	diffCalls   []string // "owner/repo#number", one per GetPullRequestDiff call
	nextDiff    string
	nextDiffErr error
}

var _ ports.SourceControl = (*fakeSourceControl)(nil)

func (f *fakeSourceControl) CreatePR(_ context.Context, spec ports.CreatePRSpec) (ports.PRRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, spec)
	return f.nextRef, f.nextErr
}

func (f *fakeSourceControl) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSourceControl) lastSpec() ports.CreatePRSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func (f *fakeSourceControl) ResolveBranchSHA(_ context.Context, spec ports.ResolveBranchSHASpec) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shaCalls = append(f.shaCalls, spec)
	if f.nextSHAErr != nil {
		return "", "", f.nextSHAErr
	}
	resolvedBranch := spec.Branch
	if resolvedBranch == "" {
		// Mirrors the real adapter: an empty spec.Branch resolves to the
		// repo's own actual default branch name, never staying empty.
		resolvedBranch = f.defaultBranchName
		if resolvedBranch == "" {
			resolvedBranch = "main"
		}
	}
	if sha, ok := f.shaFor[spec.Repo]; ok {
		return sha, resolvedBranch, nil
	}
	return f.nextSHA, resolvedBranch, nil
}

// IsAncestor (D3, second adversarial-review round) is never exercised by
// this file's own tests -- stubbed only for ports.SourceControl interface
// satisfaction, mirroring this fake's own precedent for methods no test
// here calls.
func (f *fakeSourceControl) IsAncestor(context.Context, ports.IsAncestorSpec) (bool, error) {
	return false, nil
}

func (f *fakeSourceControl) shaCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.shaCalls)
}

// lastSHASpec returns the ResolveBranchSHASpec of the MOST RECENT
// ResolveBranchSHA call this fake received -- mirrors lastSpec's own
// identical "most recent call" precedent for CreatePR, used by
// TestHandleSandboxEvent_PushComplete_CreatesPRArtifact (fix/setup-drift-
// and-pr-base) to prove createPRBestEffort's own resolvePRBaseBranch calls
// ResolveBranchSHA with Branch: "" (the repo's own default branch, never a
// guess) BEFORE ever calling CreatePR.
func (f *fakeSourceControl) lastSHASpec() ports.ResolveBranchSHASpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shaCalls[len(f.shaCalls)-1]
}

func (f *fakeSourceControl) ResolveContractsFingerprint(_ context.Context, spec ports.ResolveContractsFingerprintSpec) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fingerprintCalls = append(f.fingerprintCalls, spec)
	if f.nextFingerprintErr != nil {
		return "", false, f.nextFingerprintErr
	}
	fingerprint := f.nextFingerprint
	if fp, ok := f.fingerprintFor[spec.Repo]; ok {
		fingerprint = fp
	}
	exists := f.nextFingerprintExists
	if e, ok := f.existsFor[spec.Repo]; ok {
		exists = e
	}
	return fingerprint, exists, nil
}

func (f *fakeSourceControl) fingerprintCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.fingerprintCalls)
}

func (f *fakeSourceControl) CheckRepoAccess(ctx context.Context, spec ports.CheckRepoAccessSpec) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accessCalls = append(f.accessCalls, spec)
	f.accessCtxs = append(f.accessCtxs, ctx)

	key := spec.Owner + "/" + spec.Repo
	if err, ok := f.accessErrFor[key]; ok {
		return false, err
	}
	if f.accessErr != nil {
		return false, f.accessErr
	}
	if allowed, ok := f.accessAllowedFor[key]; ok {
		return allowed, nil
	}
	return !f.denyAllAccess, nil
}

func (f *fakeSourceControl) accessCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.accessCalls)
}

// lastAccessCtxDeadline returns the ctx.Deadline() of the MOST RECENT
// CheckRepoAccess call this fake received, plus ok=true -- used by the
// audit-remediation regression test (test-adversarial, finding #11)
// proving repoAccessAllowedForSpawn actually bounds its real
// SourceControl.CheckRepoAccess call with platform.Timeouts.
// RepoAccessCheckTimeout (checkCtx), rather than accidentally passing the
// actor's own unbounded outer ctx through -- this fake, unlike the real
// adapter, deliberately captures whatever ctx it was actually handed so a
// test can inspect it directly.
func (f *fakeSourceControl) lastAccessCtxDeadline() (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.accessCtxs) == 0 {
		return time.Time{}, false
	}
	return f.accessCtxs[len(f.accessCtxs)-1].Deadline()
}

// GetFileContent/UpdateFileContent (§12.2 item 2) are never
// reached from this package (apply-suggestion is an httpapi-only surface)
// -- clear "not implemented" errors, mirroring whiteboxFakeSourceControl's
// own identical precedent (internal/app/imagebuild).
func (f *fakeSourceControl) GetFileContent(context.Context, ports.GetFileContentSpec) (string, string, bool, error) {
	return "", "", false, errors.New("fakeSourceControl: GetFileContent not implemented")
}

func (f *fakeSourceControl) UpdateFileContent(context.Context, ports.UpdateFileContentSpec) (string, error) {
	return "", errors.New("fakeSourceControl: UpdateFileContent not implemented")
}

// RegisterPRStack (§17.2/§17.6) records every call this fake
// receives and returns a caller-configured error -- registerStackErr
// defaults to nil (registration "succeeds"), mirroring nextErr's own
// default-success precedent for CreatePR above.
func (f *fakeSourceControl) RegisterPRStack(_ context.Context, spec ports.RegisterPRStackSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registerStackCalls = append(f.registerStackCalls, spec)
	return f.registerStackErr
}

func (f *fakeSourceControl) registerStackCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.registerStackCalls)
}

func (f *fakeSourceControl) lastRegisterStackSpec() ports.RegisterPRStackSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registerStackCalls[len(f.registerStackCalls)-1]
}

// CreateBranch (a confirmed-finding fix) is never reached from this
// package's own pushpr.go -- that call happens in internal/app/outboxworker
// (sentinelautofix.go), BEFORE this package's own code ever runs for a
// sentinel-auto-fix session -- clear "not implemented" mirrors
// GetFileContent/UpdateFileContent's own identical precedent above.
func (f *fakeSourceControl) CreateBranch(context.Context, ports.CreateBranchSpec) error {
	return errors.New("fakeSourceControl: CreateBranch not implemented")
}
func (f *fakeSourceControl) GetOpenPR(context.Context, string, string, int, string) (ports.OpenPR, bool, error) {
	return ports.OpenPR{}, false, errors.New("fakeSourceControl: GetOpenPR not implemented")
}
func (f *fakeSourceControl) GetPRBody(context.Context, string, string, int, string) (string, bool, error) {
	return "", false, errors.New("fakeSourceControl: GetPRBody not implemented")
}
func (f *fakeSourceControl) UpdatePRBody(context.Context, ports.UpdatePRBodySpec) error {
	return errors.New("fakeSourceControl: UpdatePRBody not implemented")
}

// ListMergedBetween ("release PR review", §15.2) is never
// reached from this package either -- same "not implemented" precedent
// as CreateBranch above.
func (f *fakeSourceControl) ListMergedBetween(context.Context, ports.ListMergedBetweenSpec) ([]ports.MergedPR, bool, error) {
	return nil, false, errors.New("fakeSourceControl: ListMergedBetween not implemented")
}

// ListOpenPRsForUser/ResolveCodeOwners/MergePR ("decision inbox:
// read model + API", §16.2) are never reached from this package -- same
// "not implemented" precedent as ListMergedBetween above.
func (f *fakeSourceControl) ListOpenPRsForUser(context.Context, ports.ListOpenPRsForUserSpec) ([]ports.OpenPR, bool, error) {
	return nil, false, errors.New("fakeSourceControl: ListOpenPRsForUser not implemented")
}

func (f *fakeSourceControl) ResolveCodeOwners(context.Context, ports.ResolveCodeOwnersSpec) ([]ports.Owner, error) {
	return nil, errors.New("fakeSourceControl: ResolveCodeOwners not implemented")
}

func (f *fakeSourceControl) MergePR(context.Context, ports.MergePRSpec) (string, error) {
	return "", errors.New("fakeSourceControl: MergePR not implemented")
}

// reposJSONForTest builds the sessions.repos JSONB shape (design decision
// 1) for exactly one repo, name/url/branch as given.
func reposJSONForTest(t *testing.T, name, url, branch string) []byte {
	t.Helper()
	raw, err := json.Marshal([]map[string]any{
		{"name": name, "url": url, "branch": branch},
	})
	if err != nil {
		t.Fatalf("marshal test repos: %v", err)
	}
	return raw
}

// createTestSessionWithRepos creates a session naming exactly one repo
// (design decision 1's own sessions.repos JSONB column), optionally owned
// by createdBy (a zero pgtype.UUID means no owner, matching
// createTestSession's own default).
func createTestSessionWithRepos(ctx context.Context, t *testing.T, pool *pgxpool.Pool, createdBy pgtype.UUID, name, url, branch string) pgtype.UUID {
	t.Helper()
	created, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
		CreatedBy:   createdBy,
		Repos:       reposJSONForTest(t, name, url, branch),
	})
	if err != nil {
		t.Fatalf("create test session with repos: %v", err)
	}
	return created.ID
}

// createProcessingTurn directly seeds a turn already in StateProcessing --
// a surgical, direct DB seed (bypassing the real dispatch path), matching
// this package's own resilience test precedent exactly: these tests exist
// to prove completeProcessingTurn/sendPushBestEffort/createPRBestEffort's
// OWN behavior in reaction to an inbound SandboxEvent, not dispatch
// correctness (already covered by dispatch_integration_test.go).
func createProcessingTurn(ctx context.Context, t *testing.T, turns *narvipg.TurnStore, sessionID pgtype.UUID) sqlcgen.Turn {
	t.Helper()
	created, err := turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: sessionID,
		Status:    sqlcgen.TurnStatusProcessing,
	})
	if err != nil {
		t.Fatalf("create processing turn: %v", err)
	}
	return created
}

// createProcessingTurnWithCorrelationID is createProcessingTurn's own
// correlation-id-parameterized twin -- §7.3's own C4 audit fix needs a
// turn whose OWN turns.correlation_id (migrations/000121) is set, set-once
// at creation exactly like a real httpapi.CreateTurnCore call would, to
// prove logProviderFailureDiagnostic reads THAT value rather than
// whatever this Actor happened to be hydrated with.
func createProcessingTurnWithCorrelationID(ctx context.Context, t *testing.T, turns *narvipg.TurnStore, sessionID pgtype.UUID, correlationID string) sqlcgen.Turn {
	t.Helper()
	created, err := turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID:     sessionID,
		Status:        sqlcgen.TurnStatusProcessing,
		CorrelationID: &correlationID,
	})
	if err != nil {
		t.Fatalf("create processing turn with correlation id: %v", err)
	}
	return created
}

// sendSandboxEventForTest drives cmd through Actor.Send, exactly mirroring
// TestHandleSandboxEvent_FullRoundTrip's own local `send` helper (this
// file's own package-level copy, since Go test helpers are function-
// scoped there).
func sendSandboxEventForTest(ctx context.Context, t *testing.T, a *Actor, cmd SandboxEvent) SandboxEventOutcome {
	t.Helper()
	reply := make(chan SandboxEventOutcome, 1)
	cmd.Reply = reply
	if err := a.Send(ctx, cmd); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case outcome := <-reply:
		return outcome
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for SandboxEventOutcome")
		return SandboxEventOutcome{}
	}
}

// executionCompleteRaw marshals a real, schema-valid sandboxws.
// ExecutionComplete wire payload.
func executionCompleteRaw(t *testing.T, sessionID string, gen int, outcome sandboxws.ExecutionCompleteOutcome) json.RawMessage {
	t.Helper()
	return executionCompleteRawWithDiagnostic(t, sessionID, gen, outcome, nil)
}

// executionCompleteRawWithDiagnostic is executionCompleteRaw's own sibling
// for §7.3: marshals a real, schema-valid sandboxws.
// ExecutionComplete wire payload carrying diagnostic (nil for none, the
// SAME shape executionCompleteRaw above produces).
func executionCompleteRawWithDiagnostic(t *testing.T, sessionID string, gen int, outcome sandboxws.ExecutionCompleteOutcome, diagnostic *sandboxws.ExecutionCompleteDiagnostic) json.RawMessage {
	t.Helper()
	messageID := uuid.NewString()
	evt := sandboxws.ExecutionComplete{
		Type:       "execution_complete",
		MessageId:  messageID,
		SessionId:  sessionID,
		Gen:        gen,
		AckId:      "execution_complete:" + messageID,
		Outcome:    outcome,
		Diagnostic: diagnostic,
	}
	raw, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal execution_complete: %v", err)
	}
	return raw
}

// pushCompleteRaw marshals a real, schema-valid sandboxws.PushComplete
// wire payload naming exactly one pushed repo, minting a fresh MessageId
// each call. Most callers never need to control (or later assert against)
// the MessageId itself, so this is the common case -- see
// pushCompleteRawWithMessageID below for the one that does.
func pushCompleteRaw(t *testing.T, sessionID string, gen int, repoName, branch, sha string) json.RawMessage {
	t.Helper()
	return pushCompleteRawWithMessageID(t, uuid.NewString(), sessionID, gen, repoName, branch, sha)
}

// pushCompleteRawWithMessageID is pushCompleteRaw's own explicit-MessageId
// variant. Needed wherever a test must control the wire MessageId itself
// -- e.g. proving a wire-level REDELIVERY (the identical MessageId, sent
// twice) is handled differently from two genuinely distinct pushes
// (different MessageIds) -- which pushCompleteRaw's own always-mint-a-
// fresh-uuid behavior cannot express.
//
// IMPORTANT: this only controls the value embedded in the marshaled wire
// bytes (Raw). SandboxEvent.MessageID (command.go) is a SEPARATE field --
// in production, wshub's own read loop peeks the wire frame to populate it
// before ever constructing a SandboxEvent, but this test harness has no
// equivalent peek step. A caller that cares about appendRawEvent's own
// (session_id, messageID) dedup behavior (actor.go) -- as any test
// exercising FIX-1's redelivery guard or FIX-4(b)'s two-distinct-pushes
// guard must -- needs to set SandboxEvent.MessageID to this SAME value
// itself; sendSandboxEventForTest does not do it automatically.
func pushCompleteRawWithMessageID(t *testing.T, messageID, sessionID string, gen int, repoName, branch, sha string) json.RawMessage {
	t.Helper()
	evt := sandboxws.PushComplete{
		Type:      "push_complete",
		MessageId: messageID,
		SessionId: sessionID,
		Gen:       gen,
		AckId:     "push_complete:" + messageID,
		Repos: []sandboxws.PushCompleteReposElem{
			{Name: repoName, Branch: branch, Sha: sha},
		},
	}
	raw, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal push_complete: %v", err)
	}
	return raw
}

// TestHandleSandboxEvent_ExecutionCompleteCompleted_CompletesTurnAndSendsPush
// proves: a real execution_complete (outcome "completed") arriving for a
// session with a Processing turn transitions that turn to Completed,
// deletes turn_deadline, and -- as a best-effort side effect run AFTER the
// event's own transact commits -- sends a real sandboxws.Push command
// naming the session's own repo/branch.
func TestHandleSandboxEvent_ExecutionCompleteCompleted_CompletesTurnAndSendsPush(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{},
		"repo1", "https://github.com/acme/repo1.git", "feature-x")
	// §30.7/§30.9: sendPushBestEffort now honors this turn's own frozen
	// push/PR egress decision (completeProcessingTurn resolves it against
	// repo_settings.live_egress_enabled) -- without this, the repo's
	// fail-closed default is shadow (egressmode's own doc comment), and
	// this test's own push-sending assertion below would never fire. This
	// test proves the general push-sending mechanism, unrelated to §30's
	// own shadow/live distinction, so it explicitly arms this repo live.
	if _, err := narvipg.NewRepoSettingsStore(pool).UpsertLiveEgressEnabled(ctx, "acme/repo1", true); err != nil {
		t.Fatalf("arm live egress: %v", err)
	}

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	turnStore := narvipg.NewTurnStore(pool)
	created := createProcessingTurn(ctx, t, turnStore, sessionID)

	// Arm turn_deadline exactly like a real dispatch would have, so this
	// test also proves it gets cleaned up on real completion.
	if _, err := narvipg.NewTimerStore(pool).Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: sessionID, Name: TimerTurnDeadline,
		FiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("arm turn_deadline: %v", err)
	}

	commander := &fakeSendCommander{}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, commander, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	outcome := sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
	})
	if !outcome.Persisted {
		t.Error("outcome.Persisted = false, want true")
	}
	// AckID's exact deterministic-format assertion is already covered by
	// TestHandleSandboxEvent_FullRoundTrip; here we only care that SOME
	// non-empty ack was produced for this critical event.
	if outcome.AckID == "" {
		t.Error("outcome.AckID is empty, want a real ack for this critical event")
	}

	got, err := turnStore.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	if got.Status != sqlcgen.TurnStatusCompleted {
		t.Errorf("turn status = %s, want %s", got.Status, sqlcgen.TurnStatusCompleted)
	}
	if !got.CompletedAt.Valid {
		t.Error("turn completed_at not set")
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM session_timers WHERE session_id = $1 AND name = $2`,
		sessionID, TimerTurnDeadline,
	).Scan(&n); err != nil {
		t.Fatalf("count turn_deadline timers: %v", err)
	}
	if n != 0 {
		t.Errorf("turn_deadline timer count = %d, want 0 (deleted on real completion)", n)
	}

	waitUntil(t, 5*time.Second, func() bool {
		return commander.callCount() == 1
	})

	var push sandboxws.Push
	if err := json.Unmarshal(commander.lastPayload(), &push); err != nil {
		t.Fatalf("unmarshal SendCommand payload as sandboxws.Push: %v", err)
	}
	if push.Type != "push" {
		t.Errorf("Push.Type = %q, want %q", push.Type, "push")
	}
	if push.SessionId != sessionID.String() {
		t.Errorf("Push.SessionId = %q, want %q", push.SessionId, sessionID.String())
	}
	if len(push.Repos) != 1 || push.Repos[0].Name != "repo1" || push.Repos[0].Branch != "feature-x" {
		t.Errorf("Push.Repos = %+v, want exactly one {Name: repo1, Branch: feature-x}", push.Repos)
	}
}

// TestHandleSandboxEvent_ExecutionCompleteFailed_NoPush proves: a real
// execution_complete reporting "failed" transitions the turn to Failed
// (session failure_reason "failed") and never sends a push command --
// only a genuine "completed" outcome has anything to push.
func TestHandleSandboxEvent_ExecutionCompleteFailed_NoPush(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{},
		"repo1", "https://github.com/acme/repo1.git", "feature-x")

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	turnStore := narvipg.NewTurnStore(pool)
	created := createProcessingTurn(ctx, t, turnStore, sessionID)

	commander := &fakeSendCommander{}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, commander, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeFailed),
	})

	waitUntil(t, 5*time.Second, func() bool {
		got, err := turnStore.Get(ctx, created.ID)
		return err == nil && got.Status == sqlcgen.TurnStatusFailed
	})

	// Give any (should-be-nonexistent) push send a moment to happen before
	// asserting it did not -- there is no positive DB signal to poll FOR
	// when asserting something did NOT happen (same precedent as
	// dispatch_integration_test.go's own circuit-breaker test).
	time.Sleep(300 * time.Millisecond)
	if got := commander.callCount(); got != 0 {
		t.Errorf("commander.SendCommand called %d times, want 0 (a failed turn has nothing to push)", got)
	}

	sessionStore := narvipg.NewSessionStore(pool)
	sessionRow, err := sessionStore.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sessionRow.FailureReason == nil || *sessionRow.FailureReason != sqlcgen.SessionFailureReasonFailed {
		t.Errorf("session.failure_reason = %v, want %q", sessionRow.FailureReason, sqlcgen.SessionFailureReasonFailed)
	}
}

// TestHandleSandboxEvent_ExecutionCompleteFailed_DiagnosticSurvivesTheRealPostgresRoundTrip
// is §7.3's own integration-level proof of the session-journal
// half of "one record, in both surfaces": a REAL execution_complete
// carrying a Diagnostic, appended through the REAL transact/appendRawEvent
// path (actor.go) against a REAL Postgres instance (this file's own
// testcontainers harness, newTestPool), is readable back from the
// session's own event log byte-for-byte -- proving the diagnostic
// actually survives the write+read round trip this adapter's own unit
// tests (internal/adapters/outbound/opencode/diagnostic_test.go) cannot
// exercise on their own, since they never touch a database at all.
func TestHandleSandboxEvent_ExecutionCompleteFailed_DiagnosticSurvivesTheRealPostgresRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{},
		"repo1", "https://github.com/acme/repo1.git", "feature-x")

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	turnStore := narvipg.NewTurnStore(pool)
	createProcessingTurn(ctx, t, turnStore, sessionID)

	commander := &fakeSendCommander{}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, commander, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	wantDiagnostic := &sandboxws.ExecutionCompleteDiagnostic{
		Message:           strPtr("The upstream provider rejected this request."),
		UnionMember:       strPtr("APIError"),
		ProviderRequestId: strPtr("req_visible_public_abc123"),
		Model:             strPtr("anthropic/claude-sonnet-4-5"),
		RuntimeVersion:    strPtr("0.0.0-test"),
		SandboxId:         strPtr("sbx-test-0001"),
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  executionCompleteRawWithDiagnostic(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeFailed, wantDiagnostic),
	})

	waitUntil(t, 5*time.Second, func() bool {
		got, err := turnStore.ListForSession(ctx, sessionID)
		if err != nil || len(got) == 0 {
			return false
		}
		return got[0].Status == sqlcgen.TurnStatusFailed
	})

	// The session journal itself (§7.3's own first surface): read the
	// persisted raw event row back from Postgres exactly as any consumer
	// of the session's own event log would (e.g. the WS client hub's
	// replay-on-reconnect path, §6.2), and confirm the diagnostic is
	// present byte-for-byte -- not re-derived, not a second copy, the
	// SAME bytes appendRawEvent persisted from the wire payload above.
	eventStore := narvipg.NewEventStore(pool)
	events, err := eventStore.ListForSession(ctx, sessionID, 0, 100)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	var found *sandboxws.ExecutionComplete
	for _, e := range events {
		if e.Type != "execution_complete" {
			continue
		}
		var evt sandboxws.ExecutionComplete
		if err := json.Unmarshal(e.Payload, &evt); err != nil {
			t.Fatalf("unmarshal persisted execution_complete: %v", err)
		}
		found = &evt
	}
	if found == nil {
		t.Fatal("no execution_complete event found in the session's own journal")
	}
	if found.Diagnostic == nil {
		t.Fatal("persisted execution_complete.Diagnostic = nil, want the diagnostic sent on the wire")
	}
	if got, want := found.Diagnostic.Message, wantDiagnostic.Message; got == nil || want == nil || *got != *want {
		t.Errorf("persisted Diagnostic.Message = %v, want %v", got, want)
	}
	if got, want := found.Diagnostic.ProviderRequestId, wantDiagnostic.ProviderRequestId; got == nil || want == nil || *got != *want {
		t.Errorf("persisted Diagnostic.ProviderRequestId = %v, want %v", got, want)
	}
	if got, want := found.Diagnostic.SandboxId, wantDiagnostic.SandboxId; got == nil || want == nil || *got != *want {
		t.Errorf("persisted Diagnostic.SandboxId = %v, want %v", got, want)
	}
}

// TestHandleSandboxEvent_ExecutionCompleteFailed_OperatorLogReachesTheRealPathAndMatchesTheJournal
// is §7.3's own C4 audit fix, proven three ways at once against a REAL
// Postgres instance:
//
//  1. Reachability (C1): before this test existed, deleting
//     completeProcessingTurn's own `logProviderFailureDiagnostic(...)`
//     call site (pushpr.go) left every test in this package green --
//     TestLogProviderFailureDiagnostic (pushpr_test.go) only ever calls
//     that function directly, never through a real SandboxEvent. This
//     test drives the real handleSandboxEvent path instead.
//  2. The join key (C4): this Actor is hydrated (GetOrSpawn below) under
//     a ctx carrying a DELIBERATELY WRONG correlation id
//     ("hydrate-time-wrong-correlation-id") -- exactly the "whichever
//     request first hydrated the Actor" value the confirmed finding says
//     used to leak into this log line. The turn itself is seeded with
//     its OWN, DIFFERENT correlation id (turns.correlation_id,
//     migrations/000121), mirroring what a real httpapi.CreateTurnCore
//     call sets at turn-creation time. The log line's own "correlation_id"
//     attribute must read the TURN's value, never the hydration one --
//     proving the join key actually points at the failing turn, not
//     whatever request happened to spawn this Actor.
//  3. "One record, in both surfaces" (§7.3), proven by comparing the two
//     surfaces to EACH OTHER, not each independently against a shared Go
//     literal (a reviewer's own explicit ask: two surfaces built from two
//     separately-typed "same" literals can drift from each other while
//     each still matches its own copy) -- the journal's own persisted
//     execution_complete.Diagnostic (already proven to survive the
//     Postgres round trip by the sibling test above) is decoded, the
//     operator log line is parsed back out of its own JSON bytes, and
//     every field is compared journal-value-to-log-value directly.
func TestHandleSandboxEvent_ExecutionCompleteFailed_OperatorLogReachesTheRealPathAndMatchesTheJournal(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{},
		"repo1", "https://github.com/acme/repo1.git", "feature-x")

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	turnStore := narvipg.NewTurnStore(pool)
	wantTurnCorrelationID := "corr-turn-own-" + uuid.NewString()
	created := createProcessingTurnWithCorrelationID(ctx, t, turnStore, sessionID, wantTurnCorrelationID)

	// Capture BEFORE hydration -- captureDefaultLoggerJSON's own doc
	// comment (planrecord_integration_test.go) explains why: a.logger is
	// resolved from slog.Default() exactly once, at hydrate time, and
	// cached for this Actor's whole life.
	logBuf := captureDefaultLoggerJSON(t)

	commander := &fakeSendCommander{}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, commander, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	// Hydrate under a WRONG correlation id -- simulates whatever OTHER
	// request first spawned this Actor (a Slack mention, an earlier turn
	// entirely) being unrelated to the turn that is about to fail.
	hydrateCtx := platform.WithCorrelationID(ctx, "hydrate-time-wrong-correlation-id")
	a, err := r.GetOrSpawn(hydrateCtx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	// F4: StatusCode included here (the earlier version of this fixture
	// omitted it) -- the field-by-field comparison below must exercise
	// every field this diagnostic actually carries, StatusCode included,
	// not six of its seven.
	wantStatusCode := 429
	wantDiagnostic := &sandboxws.ExecutionCompleteDiagnostic{
		Message:           strPtr("The upstream provider rejected this request."),
		UnionMember:       strPtr("APIError"),
		StatusCode:        &wantStatusCode,
		ProviderRequestId: strPtr("req_visible_public_abc123"),
		Model:             strPtr("anthropic/claude-sonnet-4-5"),
		RuntimeVersion:    strPtr("0.0.0-test"),
		SandboxId:         strPtr("sbx-test-0001"),
	}

	// Sent (and later processed) with a plain, uncorrelated ctx -- proves
	// the join key comes from the TURN row, not from any per-call ctx
	// propagation (Actor.Send only uses its own ctx to bound the mailbox
	// enqueue, never threading it into command handling -- actor.go).
	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  executionCompleteRawWithDiagnostic(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeFailed, wantDiagnostic),
	})

	waitUntil(t, 5*time.Second, func() bool {
		got, err := turnStore.Get(ctx, created.ID)
		return err == nil && got.Status == sqlcgen.TurnStatusFailed
	})

	// Surface 1: the session journal.
	eventStore := narvipg.NewEventStore(pool)
	events, err := eventStore.ListForSession(ctx, sessionID, 0, 100)
	if err != nil {
		t.Fatalf("ListForSession: %v", err)
	}
	var journal *sandboxws.ExecutionComplete
	for _, e := range events {
		if e.Type != "execution_complete" {
			continue
		}
		var evt sandboxws.ExecutionComplete
		if err := json.Unmarshal(e.Payload, &evt); err != nil {
			t.Fatalf("unmarshal persisted execution_complete: %v", err)
		}
		journal = &evt
	}
	if journal == nil {
		t.Fatal("no execution_complete event found in the session's own journal")
	}
	if journal.Diagnostic == nil {
		t.Fatal("persisted execution_complete.Diagnostic = nil, want the diagnostic sent on the wire")
	}

	// Surface 2: the operator log -- reachability (finding 1 above): if
	// logProviderFailureDiagnostic's own call site were ever deleted, this
	// find fails outright rather than comparing against zero values.
	entry := findLogEntry(t, logBuf, "sessionactor: turn failed with a provider-reported diagnostic")

	// The join key (finding 2 above).
	if got, ok := entry["correlation_id"].(string); !ok || got != wantTurnCorrelationID {
		t.Errorf("operator log correlation_id = %v, want %q (the FAILING TURN's own persisted correlation id, "+
			"not whatever request hydrated this Actor)", entry["correlation_id"], wantTurnCorrelationID)
	}
	if got, ok := entry["turn_id"].(string); !ok || got != created.ID.String() {
		t.Errorf("operator log turn_id = %v, want %q", entry["turn_id"], created.ID.String())
	}
	if got, ok := entry["session_id"].(string); !ok || got != sessionID.String() {
		t.Errorf("operator log session_id = %v, want %q", entry["session_id"], sessionID.String())
	}
	if _, ok := entry["message_id"].(string); !ok {
		t.Errorf("operator log message_id missing or not a string: %v", entry["message_id"])
	}

	// Finding 3 (F4): compare the two surfaces to EACH OTHER, field by
	// field -- never each against wantDiagnostic independently, which
	// would pass even if the journal and the log had silently diverged
	// from one another while each still happened to match the fixture.
	//
	// Driven by reflection over sandboxws.ExecutionCompleteDiagnostic
	// itself, not a hand-enumerated field list: a hand-enumerated list
	// silently stops covering a field the moment a new one is added to
	// the wire type -- exactly what had already happened here once
	// (statusCode had no entry in this comparison at all until this
	// change). Walking diagType's own fields below and requiring a
	// diagnosticFieldLogAttr entry (pushpr_test.go) for each turns that
	// omission into a test FAILURE instead of a silent gap: a future
	// field with no matching logProviderFailureDiagnostic case
	// (pushpr.go) AND no entry in diagnosticFieldLogAttr fails this test
	// outright, on the commit that adds the field, not on whatever later
	// commit happens to notice the operator log is missing something.
	diagType := reflect.TypeOf(sandboxws.ExecutionCompleteDiagnostic{})
	journalVal := reflect.ValueOf(*journal.Diagnostic)
	for i := 0; i < diagType.NumField(); i++ {
		field := diagType.Field(i)
		attr, ok := diagnosticFieldLogAttr[field.Name]
		if !ok {
			t.Fatalf("sandboxws.ExecutionCompleteDiagnostic gained a field %q with no entry in "+
				"diagnosticFieldLogAttr (pushpr_test.go) -- add one (and a matching case to "+
				"logProviderFailureDiagnostic, pushpr.go) so this comparison keeps covering every field",
				field.Name)
		}
		journalStr, journalPresent := diagnosticFieldString(journalVal.Field(i))
		logStr, logPresent := logAttrString(entry, attr)
		switch {
		case !journalPresent && !logPresent:
			// Both surfaces agree the field is absent -- fine.
		case journalPresent != logPresent:
			t.Errorf("%s: journal_present=%v (%q) operator_log_present=%v (%q) -- one surface has this field, the other does not",
				field.Name, journalPresent, journalStr, logPresent, logStr)
		case journalStr != logStr:
			t.Errorf("%s: journal=%q operator_log=%q -- the two §7.3 surfaces disagree", field.Name, journalStr, logStr)
		}
	}
}

// countLogEntriesForTest counts buf's own newline-delimited JSON log
// lines whose "msg" field equals wantMsg -- findLogEntry's own sibling
// (planrecord_integration_test.go), which returns only the FIRST match
// and fails outright if there is none. This one is for proving a line
// was emitted a SPECIFIC number of times (E6, below): zero is a valid,
// non-fatal result here, unlike findLogEntry's.
func countLogEntriesForTest(t *testing.T, buf *bytes.Buffer, wantMsg string) int {
	t.Helper()
	count := 0
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("unmarshal log line %q: %v", line, err)
		}
		if entry["msg"] == wantMsg {
			count++
		}
	}
	return count
}

// providerFailureDiagnosticLogMsg is the exact "msg" this file's own
// completeProcessingTurn (pushpr.go) logs every provider-failure
// diagnostic under -- named once here so the two tests below (and
// TestHandleSandboxEvent_ExecutionCompleteFailed_OperatorLogReachesTheRealPathAndMatchesTheJournal
// above) can never silently drift apart from each other on the literal
// string.
const providerFailureDiagnosticLogMsg = "sessionactor: turn failed with a provider-reported diagnostic"

// TestHandleSandboxEvent_ExecutionCompleteFailed_DiagnosticLoggedEvenWithNoProcessingTurnFound
// is E6's own pin for the FIRST of pushpr.go's two stated-but-previously-
// unpinned invariants on the diagnostic log call site (completeProcessingTurn):
// "Fires regardless of whether a Processing turn was actually found (`ok`
// below) ... a late/redelivered execution_complete for a turn that
// already finalized some other way must still be diagnosable, not
// silently swallowed because this specific delivery no longer has a
// Processing row to match."
//
// Reproduces TestHandleSandboxEvent_RedeliveredLateExecutionComplete_
// RecordsFalseFailureOnce's own exact "already finalized by a real
// turn_deadline fire" setup (opsmetrics_integration_test.go) -- the same,
// already-proven way to genuinely reach completeProcessingTurn's own
// `ok == false` branch -- then sends a LATE execution_complete carrying
// outcome=Failed and a real diagnostic, and asserts the operator log line
// still fires, with turn_id/correlation_id both absent (no Processing
// turn to source them from) but session_id/message_id still present
// (sourced off the wire event itself, unconditionally).
//
// Mutation-verified: wrapping pushpr.go's own diagnostic-logging block in
// `if ok { ... }` makes this test fail with "no log line with msg ...
// found", while TestHandleSandboxEvent_ExecutionCompleteFailed_
// OperatorLogReachesTheRealPathAndMatchesTheJournal (the `ok == true`
// case) stays green -- proving the two branches are independently
// covered.
func TestHandleSandboxEvent_ExecutionCompleteFailed_DiagnosticLoggedEvenWithNoProcessingTurnFound(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	timeouts := platform.DefaultTimeouts()
	timeouts.TurnDeadline = 50 * time.Millisecond // tiny, injected -- not the real 60m default

	turnStore := narvipg.NewTurnStore(pool)
	created, err := turnStore.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: sessionID,
		Status:    sqlcgen.TurnStatusPending,
	})
	if err != nil {
		t.Fatalf("create turn: %v", err)
	}
	dispatchedAt := time.Now().Add(-1 * time.Hour) // comfortably past the tiny deadline
	if _, err := turnStore.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:           created.ID,
		Status:       sqlcgen.TurnStatusProcessing,
		DispatchedAt: pgtype.Timestamptz{Time: dispatchedAt, Valid: true},
	}); err != nil {
		t.Fatalf("move turn to processing: %v", err)
	}

	// Capture BEFORE hydration -- see captureDefaultLoggerJSON's own doc
	// comment.
	logBuf := captureDefaultLoggerJSON(t)

	r, err := NewRegistry(ctx, pool, timeouts, nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	// The real turn_deadline timer fires first, terminalizing the turn as
	// Failed -- BEFORE any execution_complete ever arrives, exactly
	// mirroring TestHandleSandboxEvent_RedeliveredLateExecutionComplete_
	// RecordsFalseFailureOnce's own proven setup for reaching
	// completeProcessingTurn's own `ok == false` branch.
	if err := a.Send(ctx, TimerFired{Name: TimerTurnDeadline}); err != nil {
		t.Fatalf("Send TimerFired turn_deadline: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		got, err := turnStore.Get(ctx, created.ID)
		return err == nil && got.Status == sqlcgen.TurnStatusFailed
	})

	// The sandbox, unaware the control plane already gave up, genuinely
	// finishes -- late -- and reports a real provider failure diagnostic.
	// No Processing turn exists for this session any more.
	wantDiagnostic := &sandboxws.ExecutionCompleteDiagnostic{
		Message:     strPtr("The upstream provider rejected this request."),
		UnionMember: strPtr("APIError"),
		Model:       strPtr("anthropic/claude-sonnet-4-5"),
	}
	outcome := sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  executionCompleteRawWithDiagnostic(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeFailed, wantDiagnostic),
	})
	if !outcome.Persisted {
		t.Error("outcome.Persisted = false, want true (the raw event is always persisted, per this file's own top comment)")
	}

	entry := findLogEntry(t, logBuf, providerFailureDiagnosticLogMsg)
	if _, present := entry["turn_id"]; present {
		t.Errorf("operator log carries turn_id = %v, want it ABSENT -- no Processing turn was found for this delivery", entry["turn_id"])
	}
	if _, present := entry["correlation_id"]; present {
		t.Errorf("operator log carries correlation_id = %v, want it ABSENT -- no Processing turn was found for this delivery", entry["correlation_id"])
	}
	if got, ok := entry["session_id"].(string); !ok || got != sessionID.String() {
		t.Errorf("operator log session_id = %v, want %q -- unconditional, sourced off the wire event itself even with no Processing turn", entry["session_id"], sessionID.String())
	}
	if _, ok := entry["message_id"].(string); !ok {
		t.Errorf("operator log message_id missing or not a string: %v -- unconditional, sourced off the wire event itself", entry["message_id"])
	}
}

// TestHandleSandboxEvent_ExecutionCompleteFailed_DiagnosticLoggedOnEveryRedelivery
// is E6's own pin for the SECOND of pushpr.go's two stated-but-previously-
// unpinned invariants: "Logged unconditionally on every delivery of a
// failed execution_complete (including a wire-level redelivery of an
// already-processed one, unlike the turn_false_failure_total metric
// below, which IS gated on `inserted`) -- this is an informational log
// line, not a cardinality-sensitive counter, so a redundant line on
// redelivery costs nothing."
//
// Sends the IDENTICAL raw execution_complete bytes (same messageID, so
// appendRawEvent's own upsert-on-(session_id, message_id) sees the second
// send as a genuine redelivery, Inserted == false, not a new event) TWICE
// against a turn that stays genuinely Processing throughout, and asserts
// the operator log line appears exactly TWICE -- once per delivery, never
// gated the way the false-failure counter deliberately is.
//
// Mutation-verified: adding an `inserted` gate to pushpr.go's own
// diagnostic-logging block (mirroring the false-failure gate immediately
// below it) makes this test fail with "operator log line count = 1, want
// 2", while TestHandleSandboxEvent_RedeliveredLateExecutionComplete_
// RecordsFalseFailureOnce (which asserts the OPPOSITE for the counter)
// stays green -- proving the log line and the counter are independently
// gated exactly as documented.
func TestHandleSandboxEvent_ExecutionCompleteFailed_DiagnosticLoggedOnEveryRedelivery(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	turnStore := narvipg.NewTurnStore(pool)
	created := createProcessingTurn(ctx, t, turnStore, sessionID)

	// Capture BEFORE hydration -- see captureDefaultLoggerJSON's own doc
	// comment.
	logBuf := captureDefaultLoggerJSON(t)

	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	wantDiagnostic := &sandboxws.ExecutionCompleteDiagnostic{
		Message:     strPtr("The upstream provider rejected this request."),
		UnionMember: strPtr("APIError"),
		Model:       strPtr("anthropic/claude-sonnet-4-5"),
	}
	// Minted ONCE -- never a fresh executionCompleteRawWithDiagnostic call
	// per send, which would mint a DIFFERENT messageID and so a genuinely
	// distinct event instead of a redelivery of this one (mirrors
	// TestHandleSandboxEvent_RedeliveredLateExecutionComplete_
	// RecordsFalseFailureOnce's own identical precedent).
	raw := executionCompleteRawWithDiagnostic(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeFailed, wantDiagnostic)

	firstOutcome := sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  raw,
	})
	if !firstOutcome.Persisted {
		t.Error("first delivery: outcome.Persisted = false, want true")
	}
	waitUntil(t, 5*time.Second, func() bool {
		got, err := turnStore.Get(ctx, created.ID)
		return err == nil && got.Status == sqlcgen.TurnStatusFailed
	})
	if got := countLogEntriesForTest(t, logBuf, providerFailureDiagnosticLogMsg); got != 1 {
		t.Fatalf("operator log line count after the FIRST delivery = %d, want 1", got)
	}

	secondOutcome := sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "execution_complete",
		Gen:  1,
		Raw:  raw, // identical messageID -- a genuine wire-level redelivery.
	})
	if !secondOutcome.Persisted {
		t.Error("redelivery: outcome.Persisted = false, want true (still persisted -- appendRawEvent's own upsert always succeeds)")
	}

	// sendSandboxEventForTest only returns once cmd.Reply has fired, and
	// handleSandboxEvent sends that reply synchronously right after its
	// own transact commits (sandboxevent.go's own doc comment) -- so by
	// the time secondOutcome is in hand, whatever this redelivery did (or
	// correctly did not do) to the log has already happened. No extra
	// wait needed.
	if got := countLogEntriesForTest(t, logBuf, providerFailureDiagnosticLogMsg); got != 2 {
		t.Errorf("operator log line count after the REDELIVERED second delivery = %d, want 2 (logged unconditionally on every delivery, never gated on `inserted` the way turn_false_failure_total deliberately is)", got)
	}
}

// TestHandleSandboxEvent_PushComplete_CreatesPRArtifact proves: a real
// push_complete event, for a session whose creator has a real (encrypted)
// GitHub identity, calls SourceControl.CreatePR with the correct
// owner/repo (parsed from the session's own repo clone URL)/head/base/
// token, and records the result as a "pr"-typed artifact row.
//
// The fake's own defaultBranchName is deliberately set to something OTHER
// than "main" ("trunk") -- fix/setup-drift-and-pr-base: this proves
// CreatePRSpec.Base is the repo's REAL resolved default branch (via a real
// ResolveBranchSHA call, asserted below), not the hardcoded "main"
// placeholder this Step used to open every PR against regardless of a
// repo's actual configured default branch. A test that left
// defaultBranchName at its own "main" fallback could never catch a
// regression back to the old hardcoded literal -- the two values would
// coincidentally agree.
func TestHandleSandboxEvent_PushComplete_CreatesPRArtifact(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	userStore := narvipg.NewUserStore(pool)
	identityStore := narvipg.NewIdentityStore(pool)

	user, err := userStore.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: fmt.Sprintf("pr-test-%d@example.com", time.Now().UnixNano()),
		DisplayName:  "PR Test User",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	const plaintextToken = "gh-fake-oauth-token"
	encrypted, err := platform.EncryptToken(testTokenEncryptionKey, []byte(plaintextToken))
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	email := user.PrimaryEmail
	if _, err := identityStore.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:               user.ID,
		Provider:             sqlcgen.IdentityProviderGithub,
		ExternalID:           fmt.Sprintf("pr-test-external-%d", time.Now().UnixNano()),
		Email:                &email,
		EmailVerified:        true,
		LinkedVia:            sqlcgen.IdentityLinkedViaAdmin,
		AccessTokenEncrypted: encrypted,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}

	sessionID := createTestSessionWithRepos(ctx, t, pool, user.ID,
		"repo1", "https://github.com/acme/repo1.git", "feature-x")

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	const wantDefaultBranch = "trunk"
	sourceControl := &fakeSourceControl{
		nextRef:           ports.PRRef{Number: 42, URL: "https://github.com/acme/repo1/pull/42"},
		defaultBranchName: wantDefaultBranch,
	}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", sourceControl, testTokenEncryptionKey, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "push_complete",
		Gen:  1,
		Raw:  pushCompleteRaw(t, sessionID.String(), 1, "repo1", "feature-x", "abc123"),
	})

	waitUntil(t, 5*time.Second, func() bool {
		return sourceControl.callCount() == 1
	})

	// fix/setup-drift-and-pr-base: createPRBestEffort must resolve the
	// repo's real default branch (via ResolveBranchSHA, Branch: "") BEFORE
	// ever calling CreatePR -- proven here by asserting the ResolveBranchSHA
	// call itself, not just its downstream effect on CreatePRSpec.Base
	// below.
	if got := sourceControl.shaCallCount(); got != 1 {
		t.Fatalf("ResolveBranchSHA called %d times, want 1 (resolve the repo's real default branch before creating the PR)", got)
	}
	shaSpec := sourceControl.lastSHASpec()
	if shaSpec.Owner != "acme" || shaSpec.Repo != "repo1" {
		t.Errorf("ResolveBranchSHASpec.Owner/Repo = %q/%q, want acme/repo1", shaSpec.Owner, shaSpec.Repo)
	}
	if shaSpec.Branch != "" {
		t.Errorf("ResolveBranchSHASpec.Branch = %q, want empty (resolve the repo's OWN default branch, never a guess)", shaSpec.Branch)
	}
	if shaSpec.Token != plaintextToken {
		t.Errorf("ResolveBranchSHASpec.Token = %q, want the real decrypted token %q", shaSpec.Token, plaintextToken)
	}

	spec := sourceControl.lastSpec()
	if spec.Owner != "acme" || spec.Repo != "repo1" {
		t.Errorf("CreatePRSpec.Owner/Repo = %q/%q, want acme/repo1", spec.Owner, spec.Repo)
	}
	if spec.Head != "feature-x" {
		t.Errorf("CreatePRSpec.Head = %q, want %q", spec.Head, "feature-x")
	}
	if spec.Base != wantDefaultBranch {
		t.Errorf("CreatePRSpec.Base = %q, want %q (the repo's real resolved default branch, not a hardcoded placeholder)", spec.Base, wantDefaultBranch)
	}
	if spec.Token != plaintextToken {
		t.Errorf("CreatePRSpec.Token = %q, want the real decrypted token %q", spec.Token, plaintextToken)
	}

	artifactStore := narvipg.NewArtifactStore(pool)
	waitUntil(t, 5*time.Second, func() bool {
		rows, err := artifactStore.ListForSession(ctx, sessionID)
		return err == nil && len(rows) == 1
	})

	rows, err := artifactStore.ListForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("artifact count = %d, want 1", len(rows))
	}
	if rows[0].Type != sqlcgen.ArtifactTypePr {
		t.Errorf("artifact type = %s, want %s", rows[0].Type, sqlcgen.ArtifactTypePr)
	}
	if rows[0].Url != "https://github.com/acme/repo1/pull/42" {
		t.Errorf("artifact url = %q, want %q", rows[0].Url, "https://github.com/acme/repo1/pull/42")
	}
}

// TestHandleSandboxEvent_PushComplete_ResolveDefaultBranchFails_SkipsPRCreation
// is fix/setup-drift-and-pr-base's own regression test for
// resolvePRBaseBranch's error path: when the real GitHub API call that
// resolves a repo's default branch fails (rate limit, timeout, transient
// 5xx, ...), createPRBestEffort must skip THAT repo honestly (logged, no
// PR artifact) rather than falling back to guessing a base branch -- an
// otherwise-identical setup to
// TestHandleSandboxEvent_PushComplete_CreatesPRArtifact's own happy path,
// except the fake's nextSHAErr is set, proving CreatePR itself is NEVER
// reached when the base-branch resolution it now depends on fails first.
func TestHandleSandboxEvent_PushComplete_ResolveDefaultBranchFails_SkipsPRCreation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	userStore := narvipg.NewUserStore(pool)
	identityStore := narvipg.NewIdentityStore(pool)

	user, err := userStore.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: fmt.Sprintf("pr-resolve-base-fails-test-%d@example.com", time.Now().UnixNano()),
		DisplayName:  "PR Resolve Base Fails Test User",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	const plaintextToken = "gh-fake-oauth-token-resolve-base-fails"
	encrypted, err := platform.EncryptToken(testTokenEncryptionKey, []byte(plaintextToken))
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	email := user.PrimaryEmail
	if _, err := identityStore.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:               user.ID,
		Provider:             sqlcgen.IdentityProviderGithub,
		ExternalID:           fmt.Sprintf("pr-resolve-base-fails-test-external-%d", time.Now().UnixNano()),
		Email:                &email,
		EmailVerified:        true,
		LinkedVia:            sqlcgen.IdentityLinkedViaAdmin,
		AccessTokenEncrypted: encrypted,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}

	sessionID := createTestSessionWithRepos(ctx, t, pool, user.ID,
		"repo1", "https://github.com/acme/repo1.git", "feature-x")

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	// nextErr deliberately left unset: if createPRBestEffort wrongly called
	// CreatePR despite the ResolveBranchSHA failure below, it would
	// SUCCEED and record an artifact -- a silent false negative. Only
	// nextSHAErr is set, so CreatePR being reached at all is what this test
	// must catch, not merely CreatePR failing.
	sourceControl := &fakeSourceControl{
		nextRef:    ports.PRRef{Number: 42, URL: "https://github.com/acme/repo1/pull/42"},
		nextSHAErr: errors.New("fakeSourceControl: simulated GitHub API failure resolving default branch"),
	}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", sourceControl, testTokenEncryptionKey, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "push_complete",
		Gen:  1,
		Raw:  pushCompleteRaw(t, sessionID.String(), 1, "repo1", "feature-x", "abc123"),
	})

	waitUntil(t, 5*time.Second, func() bool {
		return sourceControl.shaCallCount() == 1
	})

	// Prove a negative (mirrors TestHandleSandboxEvent_PushComplete_
	// UnsupportedRepoHost_SkipsPRCreation's own identical precedent): give
	// createPRBestEffort's own post-commit-triggered goroutine every
	// reasonable chance to have already reached CreatePR before asserting
	// it did not.
	time.Sleep(300 * time.Millisecond)
	if got := sourceControl.callCount(); got != 0 {
		t.Errorf("CreatePR called %d times, want 0 (default-branch resolution failed; must skip this repo, never guess a base branch)", got)
	}

	artifactStore := narvipg.NewArtifactStore(pool)
	rows, err := artifactStore.ListForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("artifact count = %d, want 0 (no PR ever created when its base branch could not be resolved)", len(rows))
	}
}

// TestHandleSandboxEvent_PushComplete_NoCreatedBy_SkipsHonestly proves a
// session with no created_by user (§8.11's own "no bot fallback exists"
// gap) never calls CreatePR and never panics -- it simply has no PR
// artifact to show, logged, not silently swallowed as a bug.
func TestHandleSandboxEvent_PushComplete_NoCreatedBy_SkipsHonestly(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	sessionID := createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{},
		"repo1", "https://github.com/acme/repo1.git", "feature-x")

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	sourceControl := &fakeSourceControl{}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", sourceControl, testTokenEncryptionKey, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "push_complete",
		Gen:  1,
		Raw:  pushCompleteRaw(t, sessionID.String(), 1, "repo1", "feature-x", "abc123"),
	})

	time.Sleep(300 * time.Millisecond)
	if got := sourceControl.callCount(); got != 0 {
		t.Errorf("CreatePR called %d times, want 0 (no created_by user)", got)
	}

	artifactStore := narvipg.NewArtifactStore(pool)
	rows, err := artifactStore.ListForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("artifact count = %d, want 0", len(rows))
	}
}

// TestHandleSandboxEvent_PushComplete_ViewerCreator_SkipsPRCreation proves
// §13.2's own viewer guard (§13.3: "viewers never gain PR-reviewer
// attribution or git identity on session artifacts"): a session whose
// creator has a REAL, usable, encrypted GitHub identity/token -- otherwise
// an identical setup to TestHandleSandboxEvent_PushComplete_CreatesPRArtifact
// above, which proves the happy path succeeds with these exact same
// ingredients -- never calls CreatePR and never records a PR artifact when
// that creator's CURRENT role is viewer. This is the defense-in-depth half
// of the guard: domain/authz.Authorize already refuses a viewer at
// session-CREATION time (httpapi.CreateSession), so this test's own session
// is seeded directly via the store (bypassing that create-time gate
// entirely) to prove THIS second, independent check -- at PR-creation time
// -- catches it too, exactly as it must for a user demoted to viewer after
// already creating a session.
func TestHandleSandboxEvent_PushComplete_ViewerCreator_SkipsPRCreation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	userStore := narvipg.NewUserStore(pool)
	identityStore := narvipg.NewIdentityStore(pool)

	user, err := userStore.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: fmt.Sprintf("pr-viewer-test-%d@example.com", time.Now().UnixNano()),
		DisplayName:  "PR Viewer Test User",
		Role:         sqlcgen.UserRoleViewer,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	const plaintextToken = "gh-fake-oauth-token-viewer"
	encrypted, err := platform.EncryptToken(testTokenEncryptionKey, []byte(plaintextToken))
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	email := user.PrimaryEmail
	if _, err := identityStore.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:               user.ID,
		Provider:             sqlcgen.IdentityProviderGithub,
		ExternalID:           fmt.Sprintf("pr-viewer-test-external-%d", time.Now().UnixNano()),
		Email:                &email,
		EmailVerified:        true,
		LinkedVia:            sqlcgen.IdentityLinkedViaAdmin,
		AccessTokenEncrypted: encrypted,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}

	sessionID := createTestSessionWithRepos(ctx, t, pool, user.ID,
		"repo1", "https://github.com/acme/repo1.git", "feature-x")

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	sourceControl := &fakeSourceControl{nextRef: ports.PRRef{Number: 42, URL: "https://github.com/acme/repo1/pull/42"}}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", sourceControl, testTokenEncryptionKey, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "push_complete",
		Gen:  1,
		Raw:  pushCompleteRaw(t, sessionID.String(), 1, "repo1", "feature-x", "abc123"),
	})

	// A fixed sleep (not waitUntil-for-a-positive-condition), mirroring
	// TestHandleSandboxEvent_PushComplete_NoCreatedBy_SkipsHonestly's own
	// identical "prove a negative" precedent exactly: there is no
	// eventually-true condition to poll for here, only "this never
	// happens" -- so this test gives createPRBestEffort's own
	// post-commit-triggered goroutine every reasonable chance to have
	// already run (and wrongly called CreatePR) before asserting it did
	// not.
	time.Sleep(300 * time.Millisecond)
	if got := sourceControl.callCount(); got != 0 {
		t.Errorf("CreatePR called %d times, want 0 (session creator is a viewer)", got)
	}

	artifactStore := narvipg.NewArtifactStore(pool)
	rows, err := artifactStore.ListForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("artifact count = %d, want 0 (no PR ever created for a viewer-owned session)", len(rows))
	}
}

// TestHandleSandboxEvent_PushComplete_DisabledCreator_SkipsPRCreation is
// this Step's own THIRD fix-pass regression test for a confirmed
// re-review finding: creatorMayGetPRAttribution checked the session
// creator's Role (the viewer guard proven by
// TestHandleSandboxEvent_PushComplete_ViewerCreator_SkipsPRCreation
// above) but never checked creator.Disabled itself -- so an admin
// disabling a user's account while their session was still in flight
// left that disabled user's still-valid stored GitHub token free to be
// used for PR creation/attribution once the sandbox later emitted
// push_complete, even though every OTHER place this Step re-verifies a
// resolved actor's authority (slack/identity.go's authorizeResolvedActor,
// linear/identity.go's twin, auth/middleware.go's Authenticate) already
// denies a disabled user outright. This session's creator has an
// otherwise-eligible role (member, not viewer) -- proving this is
// SPECIFICALLY the Disabled check, not a re-test of the Role check above
// -- and a real, usable, encrypted GitHub identity/token, identical
// ingredients to TestHandleSandboxEvent_PushComplete_CreatesPRArtifact's
// own happy path, except disabled.
func TestHandleSandboxEvent_PushComplete_DisabledCreator_SkipsPRCreation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	userStore := narvipg.NewUserStore(pool)
	identityStore := narvipg.NewIdentityStore(pool)

	user, err := userStore.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: fmt.Sprintf("pr-disabled-test-%d@example.com", time.Now().UnixNano()),
		DisplayName:  "PR Disabled Test User",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// No UserStore mutation exists for Disabled today (only ListMembers'
	// own read exposure, httpapi/members.go) -- set it directly, mirroring
	// this Step's own established precedent for disabling a fixture user
	// where no store method exists yet (slack/identity_integration_test.go's
	// TestHandler_AppMention_CreateSessionDeniedForDisabledUser, linear/
	// identity_integration_test.go's TestWebhookHandler_Created_DeniedForDisabledUser).
	if _, err := pool.Exec(ctx, `UPDATE users SET disabled = true WHERE id = $1`, user.ID); err != nil {
		t.Fatalf("disable fixture user: %v", err)
	}

	const plaintextToken = "gh-fake-oauth-token-disabled"
	encrypted, err := platform.EncryptToken(testTokenEncryptionKey, []byte(plaintextToken))
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	email := user.PrimaryEmail
	if _, err := identityStore.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:               user.ID,
		Provider:             sqlcgen.IdentityProviderGithub,
		ExternalID:           fmt.Sprintf("pr-disabled-test-external-%d", time.Now().UnixNano()),
		Email:                &email,
		EmailVerified:        true,
		LinkedVia:            sqlcgen.IdentityLinkedViaAdmin,
		AccessTokenEncrypted: encrypted,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}

	sessionID := createTestSessionWithRepos(ctx, t, pool, user.ID,
		"repo1", "https://github.com/acme/repo1.git", "feature-x")

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	sourceControl := &fakeSourceControl{nextRef: ports.PRRef{Number: 42, URL: "https://github.com/acme/repo1/pull/42"}}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", sourceControl, testTokenEncryptionKey, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "push_complete",
		Gen:  1,
		Raw:  pushCompleteRaw(t, sessionID.String(), 1, "repo1", "feature-x", "abc123"),
	})

	// A fixed sleep (not waitUntil-for-a-positive-condition), mirroring
	// TestHandleSandboxEvent_PushComplete_ViewerCreator_SkipsPRCreation's
	// own identical "prove a negative" precedent exactly: there is no
	// eventually-true condition to poll for here, only "this never
	// happens" -- so this test gives createPRBestEffort's own
	// post-commit-triggered goroutine every reasonable chance to have
	// already run (and wrongly called CreatePR) before asserting it did
	// not.
	time.Sleep(300 * time.Millisecond)
	if got := sourceControl.callCount(); got != 0 {
		t.Errorf("CreatePR called %d times, want 0 (session creator is disabled)", got)
	}

	artifactStore := narvipg.NewArtifactStore(pool)
	rows, err := artifactStore.ListForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("artifact count = %d, want 0 (no PR ever created for a disabled creator's session)", len(rows))
	}
}

// TestHandleSandboxEvent_PushComplete_UnsupportedRepoHost_SkipsPRCreation is
// audit-remediation batch B3 round 2's own regression test for findings
// #2/#5 (MEDIUM/HIGH): createPRBestEffort used to derive owner/repo via
// reposource.ParseOwnerRepo and call a.sourceControl.CreatePR (a WRITE
// operation, the real GitHub-only adapter in production) with NO
// reposource.CheckRepoHost gate at all -- the third of what should have
// been a uniformly-guarded set of call sites. A session whose repo names a
// non-GitHub host (a GitLab URL passes reposource.ValidateRepoURL -- it
// accepts any https host) must now be rejected BEFORE ever deriving an
// owner/repo or calling CreatePR, exactly mirroring imageresolve.go's/
// imagebuild.Builder's own already-tested behavior -- otherwise, in
// production, this would open a REAL pull request against a
// coincidentally-matching, unrelated GitHub owner/repo using the session
// creator's real OAuth token.
func TestHandleSandboxEvent_PushComplete_UnsupportedRepoHost_SkipsPRCreation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	userStore := narvipg.NewUserStore(pool)
	identityStore := narvipg.NewIdentityStore(pool)

	user, err := userStore.Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: fmt.Sprintf("pr-unsupported-host-test-%d@example.com", time.Now().UnixNano()),
		DisplayName:  "PR Unsupported Host Test User",
		Role:         sqlcgen.UserRoleMember,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	const plaintextToken = "gh-fake-oauth-token-unsupported-host"
	encrypted, err := platform.EncryptToken(testTokenEncryptionKey, []byte(plaintextToken))
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	email := user.PrimaryEmail
	if _, err := identityStore.Create(ctx, sqlcgen.CreateIdentityParams{
		UserID:               user.ID,
		Provider:             sqlcgen.IdentityProviderGithub,
		ExternalID:           fmt.Sprintf("pr-unsupported-host-test-external-%d", time.Now().UnixNano()),
		Email:                &email,
		EmailVerified:        true,
		LinkedVia:            sqlcgen.IdentityLinkedViaAdmin,
		AccessTokenEncrypted: encrypted,
	}); err != nil {
		t.Fatalf("create identity: %v", err)
	}

	sessionID := createTestSessionWithRepos(ctx, t, pool, user.ID,
		"repo1", "https://gitlab.example.com/acme/repo1.git", "feature-x")

	sandboxStore := narvipg.NewSandboxStore(pool)
	if _, err := sandboxStore.Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	// A SourceControl that would fail the test outright if CreatePR were
	// ever actually invoked -- this test's whole point is that it must
	// NOT be, for a repo url naming a host other than github.com.
	sourceControl := &fakeSourceControl{nextErr: errors.New("fakeSourceControl: CreatePR must never be called for a non-GitHub host")}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", sourceControl, testTokenEncryptionKey, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}

	sendSandboxEventForTest(ctx, t, a, SandboxEvent{
		Type: "push_complete",
		Gen:  1,
		Raw:  pushCompleteRaw(t, sessionID.String(), 1, "repo1", "feature-x", "abc123"),
	})

	// Prove a negative (mirrors TestHandleSandboxEvent_PushComplete_
	// DisabledCreator_SkipsPRCreation's own identical precedent): give
	// createPRBestEffort's own post-commit-triggered goroutine every
	// reasonable chance to have already run (and wrongly called CreatePR)
	// before asserting it did not.
	time.Sleep(300 * time.Millisecond)
	if got := sourceControl.callCount(); got != 0 {
		t.Errorf("CreatePR called %d times, want 0 (unsupported repo-url host must deny before ever deriving owner/repo or calling CreatePR)", got)
	}

	artifactStore := narvipg.NewArtifactStore(pool)
	rows, err := artifactStore.ListForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("artifact count = %d, want 0 (no PR ever created for an unsupported repo-url host)", len(rows))
	}
}

// SuppressCreatePR makes this fake satisfy sessionactor's own
// stampedSuppressor, so a cycle whose persisted decision is shadow
// reaches a recording path here exactly as it does in production.
func (f *fakeSourceControl) SuppressCreatePR(_ context.Context, spec ports.CreatePRSpec) (ports.PRRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suppressCalls = append(f.suppressCalls, spec)
	return ports.PRRef{Number: 0, URL: "narvi-shadow://" + spec.Owner + "/" + spec.Repo}, nil
}

// suppressCallCount is the companion to callCount: how many PR creations
// were suppressed-and-recorded rather than performed.
func (f *fakeSourceControl) suppressCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.suppressCalls)
}
