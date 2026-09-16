//go:build integration

// Integration test for §26.2's ("review digest: description adequacy +
// graduated remediation", §26.2) own description-autofix notifier
// (descriptionautofix.go), against a real Postgres instance -- gated
// behind the "integration" build tag, reusing this package's own
// newTestPool helper (builder_integration_test.go).
package outboxworker_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/platform"
)

// fakeDescriptionAutofixSourceControl is a minimal test-only
// ports.SourceControl -- narrowed to exactly the two methods
// descriptionAutofixNotifier's own Deliver calls (GetPRBody, UpdatePRBody)
// -- every other method returns a clear "not implemented" error,
// mirroring fakeSentinelAutoFixSourceControl's own identical precedent
// (sentinelautofix_integration_test.go).
//
// STATEFUL (adversarial-review fix, item 3's own explicit ask): UpdatePRBody
// now updates nextBody in place, so a SUBSEQUENT GetPRBody call -- exactly
// what a second Deliver call for the SAME PR does, whether a genuine retry
// or a later re-review's own delivery -- observes the body the FIRST
// UpdatePRBody call actually wrote, mirroring what a real GitHub PR does
// (a body PATCH is immediately visible to the next GET). Before this fix,
// nextBody/nextFound were fixed constants GetPRBody always returned
// regardless of any prior UpdatePRBody call, which made item 3's own bug
// (RenderAutofixBody not idempotent across repeated deliveries) impossible
// to even EXPRESS against this fake -- see
// TestDescriptionAutofixNotifier_RepeatedDelivery_IsIdempotent below, the
// test this statefulness exists to make possible.
type fakeDescriptionAutofixSourceControl struct {
	mu sync.Mutex

	getPRBodyCalls int
	nextBody       string
	nextFound      bool
	nextGetErr     error

	updateCalls   []ports.UpdatePRBodySpec
	nextUpdateErr error
}

var _ ports.SourceControl = (*fakeDescriptionAutofixSourceControl)(nil)

func (f *fakeDescriptionAutofixSourceControl) GetPRBody(context.Context, string, string, int, string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getPRBodyCalls++
	if f.nextGetErr != nil {
		return "", false, f.nextGetErr
	}
	return f.nextBody, f.nextFound, nil
}

func (f *fakeDescriptionAutofixSourceControl) getPRBodyCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getPRBodyCalls
}

func (f *fakeDescriptionAutofixSourceControl) UpdatePRBody(_ context.Context, spec ports.UpdatePRBodySpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls = append(f.updateCalls, spec)
	if f.nextUpdateErr == nil {
		// Stateful: a successful write is immediately visible to the NEXT
		// GetPRBody call, exactly like a real GitHub PATCH -- this file's
		// own top doc comment on this type explains why this is load-bearing
		// for item 3's own idempotency regression test.
		f.nextBody = spec.Body
		f.nextFound = true
	}
	return f.nextUpdateErr
}

func (f *fakeDescriptionAutofixSourceControl) updateCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.updateCalls)
}

func (f *fakeDescriptionAutofixSourceControl) lastUpdateSpec() ports.UpdatePRBodySpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.updateCalls[len(f.updateCalls)-1]
}

func (f *fakeDescriptionAutofixSourceControl) CreatePR(context.Context, ports.CreatePRSpec) (ports.PRRef, error) {
	return ports.PRRef{}, errors.New("fakeDescriptionAutofixSourceControl: CreatePR not implemented")
}
func (f *fakeDescriptionAutofixSourceControl) ResolveBranchSHA(context.Context, ports.ResolveBranchSHASpec) (string, string, error) {
	return "", "", errors.New("fakeDescriptionAutofixSourceControl: ResolveBranchSHA not implemented")
}
func (f *fakeDescriptionAutofixSourceControl) IsAncestor(context.Context, ports.IsAncestorSpec) (bool, error) {
	return false, errors.New("fakeDescriptionAutofixSourceControl: IsAncestor not implemented")
}
func (f *fakeDescriptionAutofixSourceControl) ResolveContractsFingerprint(context.Context, ports.ResolveContractsFingerprintSpec) (string, bool, error) {
	return "", false, errors.New("fakeDescriptionAutofixSourceControl: ResolveContractsFingerprint not implemented")
}
func (f *fakeDescriptionAutofixSourceControl) CheckRepoAccess(context.Context, ports.CheckRepoAccessSpec) (bool, error) {
	return false, errors.New("fakeDescriptionAutofixSourceControl: CheckRepoAccess not implemented")
}
func (f *fakeDescriptionAutofixSourceControl) GetFileContent(context.Context, ports.GetFileContentSpec) (string, string, bool, error) {
	return "", "", false, errors.New("fakeDescriptionAutofixSourceControl: GetFileContent not implemented")
}
func (f *fakeDescriptionAutofixSourceControl) UpdateFileContent(context.Context, ports.UpdateFileContentSpec) (string, error) {
	return "", errors.New("fakeDescriptionAutofixSourceControl: UpdateFileContent not implemented")
}
func (f *fakeDescriptionAutofixSourceControl) RegisterPRStack(context.Context, ports.RegisterPRStackSpec) error {
	return errors.New("fakeDescriptionAutofixSourceControl: RegisterPRStack not implemented")
}
func (f *fakeDescriptionAutofixSourceControl) ListMergedBetween(context.Context, ports.ListMergedBetweenSpec) ([]ports.MergedPR, bool, error) {
	return nil, false, errors.New("fakeDescriptionAutofixSourceControl: ListMergedBetween not implemented")
}
func (f *fakeDescriptionAutofixSourceControl) CreateBranch(context.Context, ports.CreateBranchSpec) error {
	return errors.New("fakeDescriptionAutofixSourceControl: CreateBranch not implemented")
}
func (f *fakeDescriptionAutofixSourceControl) GetOpenPR(context.Context, string, string, int, string) (ports.OpenPR, bool, error) {
	return ports.OpenPR{}, false, errors.New("fakeDescriptionAutofixSourceControl: GetOpenPR not implemented")
}
func (f *fakeDescriptionAutofixSourceControl) ListOpenPRsForUser(context.Context, ports.ListOpenPRsForUserSpec) ([]ports.OpenPR, bool, error) {
	return nil, false, errors.New("fakeDescriptionAutofixSourceControl: ListOpenPRsForUser not implemented")
}
func (f *fakeDescriptionAutofixSourceControl) ResolveCodeOwners(context.Context, ports.ResolveCodeOwnersSpec) ([]ports.Owner, error) {
	return nil, errors.New("fakeDescriptionAutofixSourceControl: ResolveCodeOwners not implemented")
}
func (f *fakeDescriptionAutofixSourceControl) MergePR(context.Context, ports.MergePRSpec) (string, error) {
	return "", errors.New("fakeDescriptionAutofixSourceControl: MergePR not implemented")
}

// seedPlatformAuthoredPR creates a real session and a real "pr"-typed
// artifact row for it, at the deterministic
// "https://github.com/{owner}/{repo}/pull/{number}" URL -- mirrors
// internal/app/automerge's own seedEligiblePR precedent (worker_
// integration_test.go) for the identical "make isPlatformAuthored find a
// real row" need.
func seedPlatformAuthoredPR(ctx context.Context, t *testing.T, pool *pgxpool.Pool, owner, repo string, number int) {
	t.Helper()

	sessions := narvipg.NewSessionStore(pool)
	artifacts := narvipg.NewArtifactStore(pool)

	session, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create platform session: %v", err)
	}

	htmlURL := "https://github.com/" + owner + "/" + repo + "/pull/" + strconv.Itoa(number)
	if _, err := artifacts.Create(ctx, sqlcgen.CreateArtifactParams{
		SessionID: session.ID, Type: sqlcgen.ArtifactTypePr, Url: htmlURL, Metadata: []byte("{}"),
	}); err != nil {
		t.Fatalf("create pr artifact: %v", err)
	}
}

// descriptionAutofixPayload builds a marshaled ports.DescriptionAutofixPayload
// with DescriptionAdequacy defaulted to review.DescriptionAdequacyDrift (a
// qualifying value, so every EXISTING call site -- all of them exercising
// the flag/authorship checks, never the adequacy gate itself -- keeps
// clearing Deliver's own Check 0 exactly as before this field existed).
// descriptionAutofixPayloadWithAdequacy below is the adequacy-gate tests'
// own explicit-value sibling.
func descriptionAutofixPayload(t *testing.T, owner, repo string, number int, proposedBody string) []byte {
	t.Helper()
	return descriptionAutofixPayloadWithAdequacy(t, owner, repo, number, proposedBody, review.DescriptionAdequacyDrift)
}

func descriptionAutofixPayloadWithAdequacy(t *testing.T, owner, repo string, number int, proposedBody string, adequacy review.DescriptionAdequacy) []byte {
	t.Helper()
	payload, err := json.Marshal(ports.DescriptionAutofixPayload{
		Owner:               owner,
		Repo:                repo,
		PRNumber:            number,
		DescriptionAdequacy: adequacy,
		ProposedBody:        proposedBody,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return payload
}

// TestDescriptionAutofixNotifier_FlagOff_NeverWrites is this Step's own
// central pin: "the server-side flag check refusing when the flag is
// off". No repo_settings row at all (the table's own established
// fail-closed-on-missing-row precedent) means the flag defaults to OFF --
// Deliver must return nil (a confirmed, non-retried no-op) and NEVER call
// GetPRBody/UpdatePRBody, even though the PR IS genuinely platform-
// authored.
func TestDescriptionAutofixNotifier_FlagOff_NeverWrites(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	repoSettings := narvipg.NewRepoSettingsStore(pool)
	artifacts := narvipg.NewArtifactStore(pool)

	owner, repo, number := "acme", "flagoff-repo", 101
	seedPlatformAuthoredPR(ctx, t, pool, owner, repo, number)
	// Deliberately NO repo_settings row for this repo at all.

	sourceControl := &fakeDescriptionAutofixSourceControl{nextFound: true, nextBody: "Original body."}
	notifier := outboxworker.NewDescriptionAutofixNotifier(repoSettings, artifacts, sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts())

	payload := descriptionAutofixPayload(t, owner, repo, number, "Proposed new body.")
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubDescriptionAutofix, Payload: payload}); err != nil {
		t.Fatalf("Deliver() error = %v, want nil (a confirmed flag-off is a silent no-op, never an error)", err)
	}

	if sourceControl.getPRBodyCallCount() != 0 {
		t.Errorf("GetPRBody called %d times, want 0 (flag off must short-circuit before any GitHub call)", sourceControl.getPRBodyCallCount())
	}
	if sourceControl.updateCallCount() != 0 {
		t.Errorf("UpdatePRBody called %d times, want 0", sourceControl.updateCallCount())
	}
}

// TestDescriptionAutofixNotifier_FlagExplicitlyOff_NeverWrites is the
// sibling of the test above with a REAL repo_settings row present, flag
// explicitly false (as opposed to the row being entirely absent) -- both
// must degrade identically.
func TestDescriptionAutofixNotifier_FlagExplicitlyOff_NeverWrites(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	repoSettings := narvipg.NewRepoSettingsStore(pool)
	artifacts := narvipg.NewArtifactStore(pool)

	owner, repo, number := "acme", "flagexplicitoff-repo", 102
	repoFullName := owner + "/" + repo
	seedPlatformAuthoredPR(ctx, t, pool, owner, repo, number)
	if _, err := repoSettings.UpsertDescriptionAutofixToggle(ctx, repoFullName, false); err != nil {
		t.Fatalf("upsert repo settings (flag off): %v", err)
	}

	sourceControl := &fakeDescriptionAutofixSourceControl{nextFound: true, nextBody: "Original body."}
	notifier := outboxworker.NewDescriptionAutofixNotifier(repoSettings, artifacts, sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts())

	payload := descriptionAutofixPayload(t, owner, repo, number, "Proposed new body.")
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubDescriptionAutofix, Payload: payload}); err != nil {
		t.Fatalf("Deliver() error = %v, want nil", err)
	}
	if sourceControl.updateCallCount() != 0 {
		t.Errorf("UpdatePRBody called %d times, want 0 (flag explicitly off)", sourceControl.updateCallCount())
	}
}

// TestDescriptionAutofixNotifier_NotPlatformAuthored_NeverWrites is this
// Step's own other central pin: "the server-side authorship check
// refusing a human-authored PR". The flag is ON, but no artifacts row
// exists for this PR's own URL (a human-authored PR) -- Deliver must
// return nil and never call UpdatePRBody, even though the flag is armed.
func TestDescriptionAutofixNotifier_NotPlatformAuthored_NeverWrites(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	repoSettings := narvipg.NewRepoSettingsStore(pool)
	artifacts := narvipg.NewArtifactStore(pool)

	owner, repo, number := "acme", "notauthored-repo", 103
	repoFullName := owner + "/" + repo
	if _, err := repoSettings.UpsertDescriptionAutofixToggle(ctx, repoFullName, true); err != nil {
		t.Fatalf("upsert repo settings (flag on): %v", err)
	}
	// Deliberately NO artifact row -- this PR was opened by a human, not
	// a Narvi session.

	sourceControl := &fakeDescriptionAutofixSourceControl{nextFound: true, nextBody: "Original body."}
	notifier := outboxworker.NewDescriptionAutofixNotifier(repoSettings, artifacts, sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts())

	payload := descriptionAutofixPayload(t, owner, repo, number, "Proposed new body.")
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubDescriptionAutofix, Payload: payload}); err != nil {
		t.Fatalf("Deliver() error = %v, want nil (a confirmed non-platform-authored PR is a silent no-op, never an error)", err)
	}

	if sourceControl.getPRBodyCallCount() != 0 {
		t.Errorf("GetPRBody called %d times, want 0 (a human-authored PR must never reach the GitHub fetch step)", sourceControl.getPRBodyCallCount())
	}
	if sourceControl.updateCallCount() != 0 {
		t.Errorf("UpdatePRBody called %d times, want 0", sourceControl.updateCallCount())
	}
}

// TestDescriptionAutofixNotifier_AdequacyOK_NeverWrites is the
// adversarial-review fix's own delivery-time regression test for item 2
// (HIGH: "nothing gates the description rewrite on adequacy") -- Deliver's
// own THIRD check, defense-in-depth against httpapi's enqueue-time gate
// ever regressing: a payload carrying DescriptionAdequacy "ok" must be a
// silent, confirmed, never-retried no-op, even though BOTH the flag and
// authorship checks would otherwise pass.
func TestDescriptionAutofixNotifier_AdequacyOK_NeverWrites(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	repoSettings := narvipg.NewRepoSettingsStore(pool)
	artifacts := narvipg.NewArtifactStore(pool)

	owner, repo, number := "acme", "adequacyok-repo", 108
	repoFullName := owner + "/" + repo
	seedPlatformAuthoredPR(ctx, t, pool, owner, repo, number)
	if _, err := repoSettings.UpsertDescriptionAutofixToggle(ctx, repoFullName, true); err != nil {
		t.Fatalf("upsert repo settings (flag on): %v", err)
	}

	sourceControl := &fakeDescriptionAutofixSourceControl{nextFound: true, nextBody: "Original body."}
	notifier := outboxworker.NewDescriptionAutofixNotifier(repoSettings, artifacts, sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts())

	payload := descriptionAutofixPayloadWithAdequacy(t, owner, repo, number, "An unsolicited proposed rewrite.", review.DescriptionAdequacyOK)
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubDescriptionAutofix, Payload: payload}); err != nil {
		t.Fatalf("Deliver() error = %v, want nil (a confirmed adequacy=ok is a silent no-op, never an error)", err)
	}

	if sourceControl.getPRBodyCallCount() != 0 {
		t.Errorf("GetPRBody called %d times, want 0 (adequacy=ok must short-circuit before any GitHub call, even with the flag on and the PR platform-authored)", sourceControl.getPRBodyCallCount())
	}
	if sourceControl.updateCallCount() != 0 {
		t.Errorf("UpdatePRBody called %d times, want 0", sourceControl.updateCallCount())
	}
}

// TestDescriptionAutofixNotifier_AdequacyZeroValue_NeverWrites proves the
// SAME gate fails safe on an older, pre-this-fix outbox row (or any other
// unrecognized DescriptionAdequacy value) -- the zero value is REJECTED,
// never treated as an implicit "proceed": Check 0 is an ALLOW-list
// ("drift"/"misleading" only), not a deny-list keyed off "ok" alone.
func TestDescriptionAutofixNotifier_AdequacyZeroValue_NeverWrites(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	repoSettings := narvipg.NewRepoSettingsStore(pool)
	artifacts := narvipg.NewArtifactStore(pool)

	owner, repo, number := "acme", "adequacyzero-repo", 109
	repoFullName := owner + "/" + repo
	seedPlatformAuthoredPR(ctx, t, pool, owner, repo, number)
	if _, err := repoSettings.UpsertDescriptionAutofixToggle(ctx, repoFullName, true); err != nil {
		t.Fatalf("upsert repo settings (flag on): %v", err)
	}

	sourceControl := &fakeDescriptionAutofixSourceControl{nextFound: true, nextBody: "Original body."}
	notifier := outboxworker.NewDescriptionAutofixNotifier(repoSettings, artifacts, sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts())

	// Marshal a payload with NO descriptionAdequacy key at all -- exactly
	// what an outbox row enqueued before this field existed would decode
	// to (Go's own JSON zero value for a missing string-typed field).
	payload := []byte(`{"owner":"acme","repo":"adequacyzero-repo","prNumber":109,"proposedBody":"Proposed."}`)
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubDescriptionAutofix, Payload: payload}); err != nil {
		t.Fatalf("Deliver() error = %v, want nil (a confirmed unrecognized adequacy value is a silent no-op, never an error)", err)
	}
	if sourceControl.updateCallCount() != 0 {
		t.Errorf("UpdatePRBody called %d times, want 0", sourceControl.updateCallCount())
	}
}

// TestDescriptionAutofixNotifier_FlagOnAndPlatformAuthored_WritesComposedBody
// is the happy path both checks passing: the PR's own body is rewritten
// via UpdatePRBody, with the freshly-fetched ORIGINAL body preserved
// alongside the proposed one (internal/domain/reviewpost.RenderAutofixBody).
func TestDescriptionAutofixNotifier_FlagOnAndPlatformAuthored_WritesComposedBody(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	repoSettings := narvipg.NewRepoSettingsStore(pool)
	artifacts := narvipg.NewArtifactStore(pool)

	owner, repo, number := "acme", "eligible-repo", 104
	repoFullName := owner + "/" + repo
	seedPlatformAuthoredPR(ctx, t, pool, owner, repo, number)
	if _, err := repoSettings.UpsertDescriptionAutofixToggle(ctx, repoFullName, true); err != nil {
		t.Fatalf("upsert repo settings (flag on): %v", err)
	}

	sourceControl := &fakeDescriptionAutofixSourceControl{nextFound: true, nextBody: "The CURRENT live body, freshly fetched."}
	notifier := outboxworker.NewDescriptionAutofixNotifier(repoSettings, artifacts, sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts())

	proposedBody := "This PR rewrites the auth token refresh path to retry on transient failures."
	payload := descriptionAutofixPayload(t, owner, repo, number, proposedBody)
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubDescriptionAutofix, Payload: payload}); err != nil {
		t.Fatalf("Deliver() error = %v, want nil", err)
	}

	if sourceControl.getPRBodyCallCount() != 1 {
		t.Fatalf("GetPRBody called %d times, want 1", sourceControl.getPRBodyCallCount())
	}
	if sourceControl.updateCallCount() != 1 {
		t.Fatalf("UpdatePRBody called %d times, want 1", sourceControl.updateCallCount())
	}

	spec := sourceControl.lastUpdateSpec()
	if spec.Owner != owner || spec.Repo != repo || spec.Number != number {
		t.Errorf("UpdatePRBodySpec owner/repo/number = %s/%s/%d, want %s/%s/%d", spec.Owner, spec.Repo, spec.Number, owner, repo, number)
	}
	if spec.Token != "gh-fake-bot-token" {
		t.Errorf("UpdatePRBodySpec.Token = %q, want the bot token (system-initiated, no human creator to attribute to)", spec.Token)
	}
	if !strings.Contains(spec.Body, proposedBody) {
		t.Errorf("UpdatePRBodySpec.Body missing the proposed text, got:\n%s", spec.Body)
	}
	if !strings.Contains(spec.Body, "The CURRENT live body, freshly fetched.") {
		t.Errorf("UpdatePRBodySpec.Body missing the freshly-fetched ORIGINAL body -- must be preserved in a collapsed block, got:\n%s", spec.Body)
	}
}

// TestDescriptionAutofixNotifier_PRNoLongerFound_NeverWrites proves a PR
// that has since been closed/deleted/made unreachable (GetPRBody's own
// found=false, err=nil) is a legitimate, silent no-op, never an error.
func TestDescriptionAutofixNotifier_PRNoLongerFound_NeverWrites(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	repoSettings := narvipg.NewRepoSettingsStore(pool)
	artifacts := narvipg.NewArtifactStore(pool)

	owner, repo, number := "acme", "gone-repo", 105
	repoFullName := owner + "/" + repo
	seedPlatformAuthoredPR(ctx, t, pool, owner, repo, number)
	if _, err := repoSettings.UpsertDescriptionAutofixToggle(ctx, repoFullName, true); err != nil {
		t.Fatalf("upsert repo settings (flag on): %v", err)
	}

	sourceControl := &fakeDescriptionAutofixSourceControl{nextFound: false}
	notifier := outboxworker.NewDescriptionAutofixNotifier(repoSettings, artifacts, sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts())

	payload := descriptionAutofixPayload(t, owner, repo, number, "Proposed new body.")
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubDescriptionAutofix, Payload: payload}); err != nil {
		t.Fatalf("Deliver() error = %v, want nil", err)
	}
	if sourceControl.updateCallCount() != 0 {
		t.Errorf("UpdatePRBody called %d times, want 0 (the PR was not found)", sourceControl.updateCallCount())
	}
}

// TestDescriptionAutofixNotifier_GetPRBodyFails_ReturnsErrorForRetry
// proves a genuine, uncertain GitHub API failure fetching the current
// body propagates as a real error (so the outbox retries later) rather
// than degrading to a silent no-op the way a CONFIRMED-negative check
// does.
func TestDescriptionAutofixNotifier_GetPRBodyFails_ReturnsErrorForRetry(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	repoSettings := narvipg.NewRepoSettingsStore(pool)
	artifacts := narvipg.NewArtifactStore(pool)

	owner, repo, number := "acme", "geterr-repo", 106
	repoFullName := owner + "/" + repo
	seedPlatformAuthoredPR(ctx, t, pool, owner, repo, number)
	if _, err := repoSettings.UpsertDescriptionAutofixToggle(ctx, repoFullName, true); err != nil {
		t.Fatalf("upsert repo settings (flag on): %v", err)
	}

	sourceControl := &fakeDescriptionAutofixSourceControl{nextGetErr: errors.New("simulated transient GitHub API failure")}
	notifier := outboxworker.NewDescriptionAutofixNotifier(repoSettings, artifacts, sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts())

	payload := descriptionAutofixPayload(t, owner, repo, number, "Proposed new body.")
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubDescriptionAutofix, Payload: payload}); err == nil {
		t.Fatal("Deliver() error = nil, want a real error when GetPRBody itself fails (a genuine, uncertain failure must be retried, never silently swallowed)")
	}
	if sourceControl.updateCallCount() != 0 {
		t.Errorf("UpdatePRBody called %d times, want 0 (never reached after GetPRBody fails)", sourceControl.updateCallCount())
	}
}

// TestDescriptionAutofixNotifier_RepoSettingsReadError_ReturnsErrorForRetry
// proves a genuine, uncertain Postgres read error checking the repo's own
// flag ALSO propagates as a real error, never silently treated as though
// the flag were confirmed off -- fault-injected via an already-rolled-
// back tx, mirroring internal/app/decisioninbox's own established
// TestBuild_CredentialResolutionErrorDegradesRatherThanRenderingNoGitHub
// precedent (aggregate_integration_test.go) for the identical technique.
func TestDescriptionAutofixNotifier_RepoSettingsReadError_ReturnsErrorForRetry(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	artifacts := narvipg.NewArtifactStore(pool)

	owner, repo, number := "acme", "repoerr-repo", 107
	seedPlatformAuthoredPR(ctx, t, pool, owner, repo, number)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx: %v", err)
	}
	brokenRepoSettings := narvipg.NewRepoSettingsStore(pool).WithTx(tx)

	sourceControl := &fakeDescriptionAutofixSourceControl{nextFound: true, nextBody: "Original body."}
	notifier := outboxworker.NewDescriptionAutofixNotifier(brokenRepoSettings, artifacts, sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts())

	payload := descriptionAutofixPayload(t, owner, repo, number, "Proposed new body.")
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubDescriptionAutofix, Payload: payload}); err == nil {
		t.Fatal("Deliver() error = nil, want a real error when the repo_settings read itself genuinely fails")
	}
	if sourceControl.updateCallCount() != 0 {
		t.Errorf("UpdatePRBody called %d times, want 0", sourceControl.updateCallCount())
	}
}

// TestDescriptionAutofixNotifier_RepeatedDelivery_IsIdempotent is item 3's
// own central regression test, at the notifier level (the unit-level
// counterpart, reviewpost.TestRenderAutofixBody_DoubleRenderEqualsSingleRender,
// pins the SAME property one layer down, in the pure rendering function
// alone): deliver the SAME outbox notification TWICE against the SAME
// (now-stateful, adversarial-review fix) fake source control -- exactly
// what a plain outbox retry after a PATCH whose response was lost looks
// like from Deliver's own point of view, GetPRBody's second call
// observing the body the first Deliver call's own UpdatePRBody already
// wrote. The SECOND delivery's own body must be BYTE-FOR-BYTE IDENTICAL
// to the first -- never a second, nested wrapper around the first
// delivery's own output, and never a body containing Narvi's own prior
// rewrite mislabeled as the "original".
func TestDescriptionAutofixNotifier_RepeatedDelivery_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	repoSettings := narvipg.NewRepoSettingsStore(pool)
	artifacts := narvipg.NewArtifactStore(pool)

	owner, repo, number := "acme", "repeat-delivery-repo", 110
	repoFullName := owner + "/" + repo
	seedPlatformAuthoredPR(ctx, t, pool, owner, repo, number)
	if _, err := repoSettings.UpsertDescriptionAutofixToggle(ctx, repoFullName, true); err != nil {
		t.Fatalf("upsert repo settings (flag on): %v", err)
	}

	realOriginal := "The real, human-authored original description."
	sourceControl := &fakeDescriptionAutofixSourceControl{nextFound: true, nextBody: realOriginal}
	notifier := outboxworker.NewDescriptionAutofixNotifier(repoSettings, artifacts, sourceControl, "gh-fake-bot-token", platform.DefaultTimeouts())

	proposedBody := "This PR rewrites the auth token refresh path to retry on transient failures."
	payload := descriptionAutofixPayload(t, owner, repo, number, proposedBody)

	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubDescriptionAutofix, Payload: payload}); err != nil {
		t.Fatalf("first Deliver() error = %v, want nil", err)
	}
	if sourceControl.updateCallCount() != 1 {
		t.Fatalf("UpdatePRBody called %d times after first delivery, want 1", sourceControl.updateCallCount())
	}
	firstBody := sourceControl.lastUpdateSpec().Body

	// Redeliver the IDENTICAL notification -- a plain outbox retry, never
	// a new verdict/payload.
	if err := notifier.Deliver(ctx, ports.Notification{Kind: ports.NotificationKindGitHubDescriptionAutofix, Payload: payload}); err != nil {
		t.Fatalf("second Deliver() error = %v, want nil", err)
	}
	if sourceControl.updateCallCount() != 2 {
		t.Fatalf("UpdatePRBody called %d times after second delivery, want 2", sourceControl.updateCallCount())
	}
	secondBody := sourceControl.lastUpdateSpec().Body

	if firstBody != secondBody {
		t.Errorf("second delivery's own body != first delivery's own body -- not idempotent:\nfirst:\n%s\n\nsecond:\n%s", firstBody, secondBody)
	}
	if !strings.Contains(secondBody, realOriginal) {
		t.Errorf("second delivery's own body lost the REAL original description, got:\n%s", secondBody)
	}
	if strings.Count(secondBody, proposedBody) != 1 {
		t.Errorf("second delivery's own body contains the proposed text %d times, want exactly 1 (no nested/duplicated content), got:\n%s", strings.Count(secondBody, proposedBody), secondBody)
	}
}
