//go:build integration

package outboxworker_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/app/shadowoperator"
	"github.com/narvidev/narvi/internal/platform"
)

// This file pins technical plan §40.2 at the outbox: the two kinds whose
// delivery is itself an automatic action -- the sentinel auto-fix's spawn
// and the description rewrite -- are held while autonomy is frozen,
// consuming nothing (the row pending, its attempt given back, due again
// after the recheck interval), and deliver once the freeze lifts; a row of
// those kinds born in shadow still resolves into the ledger; the rows held
// are claimed in a lane of their own, so no notification waits behind
// them; and they are left out of the lag gauge.

// freezeAutonomy freezes autonomy on pool, and lifts it when the test ends
// if it is still frozen.
func freezeAutonomy(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	settings := narvipg.NewPlatformSettingsStore(pool)
	if _, err := settings.Freeze(context.Background(), pgtype.UUID{}, "an incident: hold every automatic action"); err != nil {
		t.Fatalf("freeze autonomy: %v", err)
	}
	t.Cleanup(func() { _, _ = settings.Unfreeze(context.Background()) })
}

func unfreezeAutonomy(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := narvipg.NewPlatformSettingsStore(pool).Unfreeze(context.Background()); err != nil {
		t.Fatalf("unfreeze autonomy: %v", err)
	}
}

// assertHeldByTheFreeze checks the row a frozen tick held: still pending,
// attempts back at wantAttempts (the claim's attempt given back), its run
// of shutdown interruptions left at wantInterruptions, its last_error
// naming the skip and its reason, and due again the recheck interval from
// the tick -- never delivered, to the world or to the ledger.
func assertHeldByTheFreeze(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id pgtype.UUID, tickAt time.Time, wantAttempts, wantInterruptions int32, reason string) {
	t.Helper()
	row, err := narvipg.NewOutboxStore(pool, false).Get(ctx, id)
	if err != nil {
		t.Fatalf("get outbox row: %v", err)
	}
	if row.Status != sqlcgen.OutboxStatusPending || row.DeliveredToLedger || row.DeliveredAt.Valid {
		t.Fatalf("row status %s, delivered to ledger %v, delivered at %v; want pending and undelivered", row.Status, row.DeliveredToLedger, row.DeliveredAt)
	}
	if row.Attempts != wantAttempts {
		t.Errorf("attempts = %d, want %d: a held delivery is not a counted attempt", row.Attempts, wantAttempts)
	}
	if row.ConsecutiveInterruptions != wantInterruptions {
		t.Errorf("consecutive_interruptions = %d, want %d: a hold is no shutdown interruption", row.ConsecutiveInterruptions, wantInterruptions)
	}
	if wantPrefix := "skipped (" + reason + "): "; row.LastError == nil || !strings.HasPrefix(*row.LastError, wantPrefix) {
		t.Errorf("last_error = %v, want it to start with %q", row.LastError, wantPrefix)
	}
	recheck := platform.DefaultTimeouts().AutonomyFreezeRecheckInterval
	if due := row.NextAttemptAt.Time; due.Before(tickAt.Add(recheck)) || due.After(time.Now().Add(recheck)) {
		t.Errorf("next_attempt_at = %v, want the recheck interval (%v) after the tick at %v", due, recheck, tickAt)
	}
}

// sentinelAutoFixFreezeFixture is one sentinel auto-fix outbox row, its
// claim and finding, and a Builder delivering its kind through the real
// notifier.
type sentinelAutoFixFreezeFixture struct {
	pool          *pgxpool.Pool
	builder       *outboxworker.Builder
	sourceControl *fakeSentinelAutoFixSourceControl
	sentinelFixes *narvipg.SentinelFixStore
	findings      *narvipg.ReviewFindingStore
	fix           sqlcgen.SentinelFix
	row           sqlcgen.Outbox
	repoFullName  string
	identityHash  string
}

func newSentinelAutoFixFreezeFixture(ctx context.Context, t *testing.T) *sentinelAutoFixFreezeFixture {
	t.Helper()
	pool := newTestPool(t)
	sessions := narvipg.NewSessionStore(pool)
	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	reviewFindings := narvipg.NewReviewFindingStore(pool)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	// A session naming no single repository: its outbox rows are born live
	// (the platform's mode), as TestSentinelAutoFixNotifier_SpawnsChildSessionAndUpdatesStores's are.
	const repoFullName = "acme/widgets"
	origin, err := sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create origin session: %v", err)
	}
	originSession := origin.ID
	fix, err := sentinelFixes.Claim(ctx, repoFullName, 91, originSession, "feature-fix-me")
	if err != nil {
		t.Fatalf("claim sentinel_fixes: %v", err)
	}
	const identityHash = "f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0"
	if _, err := reviewFindings.Upsert(ctx, sqlcgen.UpsertReviewFindingParams{
		RepoFullName: repoFullName, PrNumber: 91, IdentityHash: identityHash,
		Severity: "medium", FilePath: "internal/foo/bar.go", Description: "Missing test coverage.",
	}); err != nil {
		t.Fatalf("upsert review finding: %v", err)
	}

	sourceControl := &fakeSentinelAutoFixSourceControl{nextSHA: "deadbeef"}
	notifier := mustNotifier(outboxworker.NewSentinelAutoFixNotifier(pool, sessions, narvipg.NewTurnStore(pool), narvipg.NewEnvironmentStore(pool), narvipg.NewAuditLogStore(pool), registry, sentinelFixes, reviewFindings,
		sourceControl, platform.MustNewGitHubOutboundConfig("gh-fake-bot-token"), platform.DefaultTimeouts(), false, platform.RolloutModeOpen, narvipg.NewRepoSettingsStore(pool), narvipg.NewGitHubPRSessionStore(pool),
		func(context.Context, string) bool { return true }, narvipg.NewShadowSCMWriteStore(pool)))

	payload, err := json.Marshal(ports.SentinelAutoFixPayload{
		SentinelFixID: fix.ID.String(), RepoFullName: repoFullName, OriginPRNumber: 91,
		OriginReviewSessionID: originSession.String(), OriginHeadBranch: "feature-fix-me",
		RepoName: "widgets", RepoCloneURL: "https://github.com/acme/widgets.git",
		FindingIdentityHashes: []string{identityHash}, FindingDescriptions: []string{"Missing test coverage."},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	store := narvipg.NewOutboxStore(pool, false)
	row, err := store.Create(ctx, sqlcgen.CreateOutboxEntryParams{SessionID: originSession, Kind: string(ports.NotificationKindSentinelAutoFix), Payload: payload})
	if err != nil {
		t.Fatalf("enqueue the sentinel auto-fix: %v", err)
	}

	builder, err := outboxworker.NewBuilder(store, pool, map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindSentinelAutoFix: notifier,
	}, platform.DefaultTimeouts(), &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	return &sentinelAutoFixFreezeFixture{
		pool: pool, builder: builder, sourceControl: sourceControl, sentinelFixes: sentinelFixes, findings: reviewFindings,
		fix: fix, row: row, repoFullName: repoFullName, identityHash: identityHash,
	}
}

// fixSessions counts the sentinel auto-fix child sessions.
func (f *sentinelAutoFixFreezeFixture) fixSessions(ctx context.Context, t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE provenance_tag = 'sentinel_auto_fix'`).Scan(&n); err != nil {
		t.Fatalf("count fix sessions: %v", err)
	}
	return n
}

func (f *sentinelAutoFixFreezeFixture) findingStatus(ctx context.Context, t *testing.T) string {
	t.Helper()
	finding, err := f.findings.Get(ctx, f.repoFullName, 91, f.identityHash)
	if err != nil {
		t.Fatalf("get review finding: %v", err)
	}
	return finding.Status
}

// TestSentinelAutoFix_Frozen_RowHeldAttemptNotCounted: while autonomy is
// frozen, a due sentinel auto-fix delivery creates no branch and spawns no
// session; its claim stays pending with no child, its finding stays open,
// and the outbox row stays pending with its attempt given back, a skipped
// last_error, and its next attempt the recheck interval out. Two frozen
// ticks in a row consume nothing either.
func TestSentinelAutoFix_Frozen_RowHeldAttemptNotCounted(t *testing.T) {
	ctx := context.Background()
	f := newSentinelAutoFixFreezeFixture(ctx, t)
	setInterruptions(ctx, t, f.pool, f.row.ID, 2)
	freezeAutonomy(t, f.pool)

	for tick := 1; tick <= 2; tick++ {
		if tick == 2 {
			makeDue(t, f.pool, f.row.ID)
		}
		tickAt := time.Now()
		if err := f.builder.PumpOnce(ctx); err != nil {
			t.Fatalf("tick %d: PumpOnce: %v", tick, err)
		}
		assertHeldByTheFreeze(ctx, t, f.pool, f.row.ID, tickAt, 0, 2, "frozen")
	}
	if got := f.sourceControl.shaCallCount(); got != 0 {
		t.Errorf("ResolveBranchSHA calls = %d, want 0", got)
	}
	if got := f.sourceControl.createBranchCallCount(); got != 0 {
		t.Errorf("CreateBranch calls = %d, want 0", got)
	}
	if got := f.fixSessions(ctx, t); got != 0 {
		t.Errorf("fix sessions = %d, want 0", got)
	}
	fix, err := f.sentinelFixes.GetByID(ctx, f.fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if fix.FixChildSessionID.Valid || fix.Status != f.fix.Status {
		t.Errorf("claim = (status %q, child %v), want it untouched at %q with no child", fix.Status, fix.FixChildSessionID.Valid, f.fix.Status)
	}
	if got := f.findingStatus(ctx, t); got != "open" {
		t.Errorf("finding status = %q, want open", got)
	}
}

// TestSentinelAutoFix_Unfreeze_SpawnsOnce: the delivery the freeze held is
// still a candidate once it lifts: its next due tick spawns exactly one
// fix session, on one branch, counting one attempt.
func TestSentinelAutoFix_Unfreeze_SpawnsOnce(t *testing.T) {
	ctx := context.Background()
	f := newSentinelAutoFixFreezeFixture(ctx, t)
	freezeAutonomy(t, f.pool)
	if err := f.builder.PumpOnce(ctx); err != nil {
		t.Fatalf("frozen PumpOnce: %v", err)
	}

	unfreezeAutonomy(t, f.pool)
	makeDue(t, f.pool, f.row.ID)
	if err := f.builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce after the unfreeze: %v", err)
	}

	row, err := narvipg.NewOutboxStore(f.pool, false).Get(ctx, f.row.ID)
	if err != nil {
		t.Fatalf("get outbox row: %v", err)
	}
	if row.Status != sqlcgen.OutboxStatusDelivered || row.DeliveredToLedger {
		t.Fatalf("row status %s (to ledger %v), want delivered to the world", row.Status, row.DeliveredToLedger)
	}
	if row.Attempts != 1 {
		t.Errorf("attempts = %d, want 1: the held tick counted none", row.Attempts)
	}
	if got := f.fixSessions(ctx, t); got != 1 {
		t.Errorf("fix sessions = %d, want 1", got)
	}
	if got := f.sourceControl.createBranchCallCount(); got != 1 {
		t.Errorf("CreateBranch calls = %d, want 1", got)
	}
	fix, err := f.sentinelFixes.GetByID(ctx, f.fix.ID)
	if err != nil {
		t.Fatalf("get sentinel_fixes: %v", err)
	}
	if !fix.FixChildSessionID.Valid {
		t.Error("the claim has no child session after the unfreeze")
	}
	if got := f.findingStatus(ctx, t); got != "fix_pending" {
		t.Errorf("finding status = %q, want fix_pending", got)
	}
}

// descriptionAutofixFreezeRow enqueues a description rewrite of an opted-in,
// platform-authored pull request whose repository is live, or still in
// shadow when live is false, and returns the row with a Builder delivering
// its kind through the real notifier.
func descriptionAutofixFreezeRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repo string, number int, live bool) (sqlcgen.Outbox, *outboxworker.Builder, *fakeDescriptionAutofixSourceControl) {
	t.Helper()
	const owner = "acme"
	repoFullName := owner + "/" + repo
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	seedPlatformAuthoredPR(ctx, t, pool, owner, repo, number)
	if _, err := repoSettings.UpsertDescriptionAutofixToggle(ctx, repoFullName, true); err != nil {
		t.Fatalf("opt in to the description autofix: %v", err)
	}
	if live {
		if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
			t.Fatalf("promote to live egress: %v", err)
		}
	}
	sessionID := createShadowEpochTestSession(ctx, t, pool, repoFullName)
	store := narvipg.NewOutboxStore(pool, false)
	row, err := store.Create(ctx, sqlcgen.CreateOutboxEntryParams{
		SessionID: sessionID, Kind: string(ports.NotificationKindGitHubDescriptionAutofix),
		Payload: descriptionAutofixPayload(t, owner, repo, number, "This pull request retries the token refresh on transient failures."),
	})
	if err != nil {
		t.Fatalf("enqueue the description rewrite: %v", err)
	}
	if row.SuppressedInShadow == live {
		t.Fatalf("row born suppressed = %v, want %v", row.SuppressedInShadow, !live)
	}
	sourceControl := &fakeDescriptionAutofixSourceControl{nextFound: true, nextBody: "The body a person wrote."}
	notifier := mustNotifier(outboxworker.NewDescriptionAutofixNotifier(repoSettings, narvipg.NewArtifactStore(pool), sourceControl, platform.MustNewGitHubOutboundConfig("gh-fake-bot-token"), platform.DefaultTimeouts()))
	builder, err := outboxworker.NewBuilder(store, pool, map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindGitHubDescriptionAutofix: notifier,
	}, platform.DefaultTimeouts(), &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	return row, builder, sourceControl
}

// TestDescriptionAutofix_Frozen_RowHeld: while autonomy is frozen a due
// description rewrite of a live repository reads and writes nothing, and
// is held -- its attempt given back, its interruption run kept.
func TestDescriptionAutofix_Frozen_RowHeld(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	row, builder, sourceControl := descriptionAutofixFreezeRow(ctx, t, pool, "frozen-description", 120, true)
	setInterruptions(ctx, t, pool, row.ID, 1)
	freezeAutonomy(t, pool)

	tickAt := time.Now()
	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	assertHeldByTheFreeze(ctx, t, pool, row.ID, tickAt, 0, 1, "frozen")
	if got := sourceControl.getPRBodyCallCount(); got != 0 {
		t.Errorf("GetPRBody calls = %d, want 0", got)
	}
	if got := sourceControl.updateCallCount(); got != 0 {
		t.Errorf("UpdatePRBody calls = %d, want 0", got)
	}
}

// setInterruptions sets a row's run of shutdown interruptions, which a hold
// must leave as it is.
func setInterruptions(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id pgtype.UUID, n int32) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE outbox SET consecutive_interruptions = $2 WHERE id = $1`, id, n); err != nil {
		t.Fatalf("set the interruption run: %v", err)
	}
}

// TestDescriptionAutofix_Unfreeze_Rewrites: the rewrite the freeze held is
// written once the freeze lifts, on its next due tick, counting one
// attempt.
func TestDescriptionAutofix_Unfreeze_Rewrites(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	row, builder, sourceControl := descriptionAutofixFreezeRow(ctx, t, pool, "unfrozen-description", 130, true)
	freezeAutonomy(t, pool)
	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("frozen PumpOnce: %v", err)
	}

	unfreezeAutonomy(t, pool)
	makeDue(t, pool, row.ID)
	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce after the unfreeze: %v", err)
	}
	if got := sourceControl.updateCallCount(); got != 1 {
		t.Fatalf("UpdatePRBody calls = %d, want 1", got)
	}
	got, err := narvipg.NewOutboxStore(pool, false).Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("get outbox row: %v", err)
	}
	if got.Status != sqlcgen.OutboxStatusDelivered || got.Attempts != 1 {
		t.Errorf("row status %s, attempts %d; want delivered on its one counted attempt", got.Status, got.Attempts)
	}
}

// TestOutboxLag_FrozenHeldRowsExcluded: while autonomy is frozen a held
// row aging with the freeze is left out of outbox_lag_seconds -- a
// two-hour-old held row beside a fresh Slack row reads as the Slack row's
// age, not as a stuck outbox -- and once the freeze lifts it counts again.
func TestOutboxLag_FrozenHeldRowsExcluded(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)

	held := seedOutboxEntry(ctx, t, store, string(ports.NotificationKindSentinelAutoFix), map[string]any{"sentinel_fix_id": "never-delivered"})
	if _, err := pool.Exec(ctx, `UPDATE outbox SET created_at = now() - interval '2 hours' WHERE id = $1`, held.ID); err != nil {
		t.Fatalf("age the held row: %v", err)
	}
	slackNotifier := &fakeNotifier{}
	heldNotifier := &fakeNotifier{}
	builder, err := outboxworker.NewBuilder(store, pool, map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindSlack:           slackNotifier,
		ports.NotificationKindSentinelAutoFix: heldNotifier,
	}, platform.DefaultTimeouts(), &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	freezeAutonomy(t, pool)
	seedOutboxEntry(ctx, t, store, string(ports.NotificationKindSlack), map[string]any{"channel_id": "C1", "text": "hi"})

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	lag, found := readInt64Gauge(ctx, t, otelReader, "outbox_lag_seconds")
	if !found {
		t.Fatal("outbox_lag_seconds: no data point recorded")
	}
	if lag > 60 {
		t.Fatalf("outbox_lag_seconds = %d while frozen, want the fresh Slack row's age (under a minute): the held row is left out", lag)
	}
	if heldNotifier.deliverCount() != 0 || slackNotifier.deliverCount() != 1 {
		t.Fatalf("deliveries: held kind %d, Slack %d; want 0 and 1", heldNotifier.deliverCount(), slackNotifier.deliverCount())
	}

	unfreezeAutonomy(t, pool)
	makeDue(t, pool, held.ID)
	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce after the unfreeze: %v", err)
	}
	if lag, _ := readInt64Gauge(ctx, t, otelReader, "outbox_lag_seconds"); lag < 7000 {
		t.Fatalf("outbox_lag_seconds = %d after the unfreeze, want the two-hour-old row's age again", lag)
	}
}

// TestFreeze_ShadowEraRowsResolveAndActivateSucceeds pins that the freeze
// never stops a person's shadow-to-live Activate. In a shadow repository,
// a sentinel auto-fix row and a description rewrite row, both born in
// shadow, can only end in the suppression ledger (§30.8), so their
// delivery starts nothing and the freeze does not hold them: a frozen tick
// records both in the ledger -- no branch, no session, no rewrite -- and
// Activate, which waits for every shadow-era row to settle, then succeeds
// while still frozen.
func TestFreeze_ShadowEraRowsResolveAndActivateSucceeds(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	const owner, repo = "acme", "shadow-era-activate"
	repoFullName := owner + "/" + repo
	sessions := narvipg.NewSessionStore(pool)
	sentinelFixes := narvipg.NewSentinelFixStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	store := narvipg.NewOutboxStore(pool, false)

	registry, err := sessionactor.NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })

	sessionID := createShadowEpochTestSession(ctx, t, pool, repoFullName)
	description, err := store.Create(ctx, sqlcgen.CreateOutboxEntryParams{
		SessionID: sessionID, Kind: string(ports.NotificationKindGitHubDescriptionAutofix),
		Payload: descriptionAutofixPayload(t, owner, repo, 140, "A description the shadow review proposed."),
	})
	if err != nil {
		t.Fatalf("enqueue the description rewrite: %v", err)
	}
	fix, err := sentinelFixes.Claim(ctx, repoFullName, 141, sessionID, "feature-fix-me")
	if err != nil {
		t.Fatalf("claim sentinel_fixes: %v", err)
	}
	payload, err := json.Marshal(ports.SentinelAutoFixPayload{
		SentinelFixID: fix.ID.String(), RepoFullName: repoFullName, OriginPRNumber: 141,
		OriginReviewSessionID: sessionID.String(), OriginHeadBranch: "feature-fix-me",
		RepoName: repo, RepoCloneURL: "https://github.com/" + repoFullName + ".git",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	sentinel, err := store.Create(ctx, sqlcgen.CreateOutboxEntryParams{SessionID: sessionID, Kind: string(ports.NotificationKindSentinelAutoFix), Payload: payload})
	if err != nil {
		t.Fatalf("enqueue the sentinel auto-fix: %v", err)
	}
	if !description.SuppressedInShadow || !sentinel.SuppressedInShadow {
		t.Fatalf("rows born suppressed = (%v, %v), want both born in shadow", description.SuppressedInShadow, sentinel.SuppressedInShadow)
	}

	descriptionSC := &fakeDescriptionAutofixSourceControl{nextFound: true, nextBody: "The body a person wrote."}
	sentinelSC := &fakeSentinelAutoFixSourceControl{nextSHA: "deadbeef"}
	builder, err := outboxworker.NewBuilder(store, pool, map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindGitHubDescriptionAutofix: mustNotifier(outboxworker.NewDescriptionAutofixNotifier(repoSettings, narvipg.NewArtifactStore(pool), descriptionSC, platform.MustNewGitHubOutboundConfig("gh-fake-bot-token"), platform.DefaultTimeouts())),
		ports.NotificationKindSentinelAutoFix: mustNotifier(outboxworker.NewSentinelAutoFixNotifier(pool, sessions, narvipg.NewTurnStore(pool), narvipg.NewEnvironmentStore(pool), narvipg.NewAuditLogStore(pool), registry, sentinelFixes, narvipg.NewReviewFindingStore(pool),
			sentinelSC, platform.MustNewGitHubOutboundConfig("gh-fake-bot-token"), platform.DefaultTimeouts(), false, platform.RolloutModeOpen, repoSettings, narvipg.NewGitHubPRSessionStore(pool),
			func(context.Context, string) bool { return true }, narvipg.NewShadowSCMWriteStore(pool))),
	}, platform.DefaultTimeouts(), &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	freezeAutonomy(t, pool)

	admin, err := narvipg.NewUserStore(pool).Create(ctx, sqlcgen.CreateUserParams{PrimaryEmail: "activator@example.com", DisplayName: "Activator", Role: sqlcgen.UserRoleAdmin})
	if err != nil {
		t.Fatalf("create the admin: %v", err)
	}
	reads := narvipg.NewShadowOperatorReadStore(pool)
	auditLog := narvipg.NewAuditLogStore(pool)
	_, err = shadowoperator.Activate(ctx, reads, repoSettings, auditLog, repoFullName, admin.ID)
	var unhandled *shadowoperator.ErrUnhandledShadowEraRows
	if !errors.As(err, &unhandled) || unhandled.Count != 2 {
		t.Fatalf("Activate before the tick = %v, want both shadow-era rows counted as unhandled", err)
	}

	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("frozen PumpOnce: %v", err)
	}
	for _, id := range []pgtype.UUID{description.ID, sentinel.ID} {
		got, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("get outbox row: %v", err)
		}
		if got.Status != sqlcgen.OutboxStatusDelivered || got.Attempts != 1 {
			t.Fatalf("row %s: status %s, attempts %d; want settled on its first attempt while frozen", got.Kind, got.Status, got.Attempts)
		}
	}
	if got := countLedgerWrites(ctx, t, pool, repoFullName, "sentinel_auto_fix"); got != 1 {
		t.Errorf("sentinel auto-fix ledger rows = %d, want 1", got)
	}
	if got, err := store.Get(ctx, description.ID); err != nil || !got.DeliveredToLedger {
		t.Errorf("the description rewrite was not delivered to the ledger (err %v)", err)
	}
	if descriptionSC.getPRBodyCallCount() != 0 || descriptionSC.updateCallCount() != 0 || sentinelSC.shaCallCount() != 0 || sentinelSC.createBranchCallCount() != 0 {
		t.Fatalf("calls to the code host while resolving shadow-era rows: GetPRBody %d, UpdatePRBody %d, ResolveBranchSHA %d, CreateBranch %d; want none",
			descriptionSC.getPRBodyCallCount(), descriptionSC.updateCallCount(), sentinelSC.shaCallCount(), sentinelSC.createBranchCallCount())
	}
	if fixRow, err := sentinelFixes.GetByID(ctx, fix.ID); err != nil || fixRow.FixChildSessionID.Valid {
		t.Fatalf("the shadow-era fix spawned a session (err %v)", err)
	}

	updated, err := shadowoperator.Activate(ctx, reads, repoSettings, auditLog, repoFullName, admin.ID)
	if err != nil {
		t.Fatalf("Activate while frozen, with every shadow-era row settled: %v", err)
	}
	if !updated.LiveEgressEnabled {
		t.Fatal("Activate did not promote the repository")
	}
}

// countLedgerWrites counts the suppression ledger's rows of operation for
// repoFullName.
func countLedgerWrites(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullName, operation string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM shadow_scm_writes WHERE repo_full_name = $1 AND operation = $2`, repoFullName, operation).Scan(&n); err != nil {
		t.Fatalf("count ledger rows: %v", err)
	}
	return n
}

// TestOutbox_HeldRowsNeverStarveNotifications pins that the rows the
// freeze holds never keep a notification waiting, and never hide its lag.
// Twenty-five held rows due ahead of a Slack message in the one
// oldest-due-first order would take a whole batch; while the freeze holds
// -- set, or unreadable, which holds as well -- they are claimed in a lane
// of their own: the Slack message is delivered on the same tick, the held
// rows are held with their reason, and outbox_lag_seconds reads the Slack
// message's age, neither zero nor the held rows'.
func TestOutbox_HeldRowsNeverStarveNotifications(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		hold   func(t *testing.T, pool *pgxpool.Pool)
		reason string
	}{
		{name: "frozen", hold: freezeAutonomy, reason: "frozen"},
		{name: "the freeze unreadable", hold: func(t *testing.T, pool *pgxpool.Pool) {
			t.Helper()
			if _, err := pool.Exec(context.Background(), `ALTER TABLE platform_settings RENAME TO platform_settings_unreadable`); err != nil {
				t.Fatalf("make the freeze unreadable: %v", err)
			}
			t.Cleanup(func() {
				if _, err := pool.Exec(context.Background(), `ALTER TABLE platform_settings_unreadable RENAME TO platform_settings`); err != nil {
					t.Errorf("restore platform_settings: %v", err)
				}
			})
		}, reason: "freeze_unreadable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			store := narvipg.NewOutboxStore(pool, false)
			const heldRows = 25
			for i := 0; i < heldRows; i++ {
				seedOutboxEntry(ctx, t, store, string(ports.NotificationKindSentinelAutoFix), map[string]any{"sentinel_fix_id": "held"})
			}
			if _, err := pool.Exec(ctx, `UPDATE outbox SET next_attempt_at = now() - interval '1 hour', created_at = now() - interval '2 hours' WHERE kind = $1`, string(ports.NotificationKindSentinelAutoFix)); err != nil {
				t.Fatalf("age the held rows: %v", err)
			}
			slack := seedOutboxEntry(ctx, t, store, string(ports.NotificationKindSlack), map[string]any{"channel_id": "C1", "text": "hi"})
			if _, err := pool.Exec(ctx, `UPDATE outbox SET next_attempt_at = now() - interval '1 minute', created_at = now() - interval '30 minutes' WHERE id = $1`, slack.ID); err != nil {
				t.Fatalf("age the Slack row: %v", err)
			}
			slackNotifier, heldNotifier := &fakeNotifier{}, &fakeNotifier{}
			builder, err := outboxworker.NewBuilder(store, pool, map[ports.NotificationKind]ports.Notifier{
				ports.NotificationKindSlack:           slackNotifier,
				ports.NotificationKindSentinelAutoFix: heldNotifier,
			}, platform.DefaultTimeouts(), &platform.ShutdownState{})
			if err != nil {
				t.Fatalf("NewBuilder: %v", err)
			}
			tc.hold(t, pool)

			if err := builder.PumpOnce(ctx); err != nil {
				t.Fatalf("PumpOnce: %v", err)
			}
			if got := slackNotifier.deliverCount(); got != 1 {
				t.Fatalf("Slack deliveries = %d, want 1: a notification never waits behind held rows", got)
			}
			if got := heldNotifier.deliverCount(); got != 0 {
				t.Fatalf("held-kind deliveries = %d, want 0", got)
			}
			lag, found := readInt64Gauge(ctx, t, otelReader, "outbox_lag_seconds")
			if !found || lag < 29*60 || lag >= 60*60 {
				t.Fatalf("outbox_lag_seconds = %d (found %v), want the Slack message's age, about 30 minutes", lag, found)
			}
			var held, pending int
			if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE last_error LIKE $2 AND attempts = 0), count(*) FILTER (WHERE status = 'pending')
				FROM outbox WHERE kind = $1`, string(ports.NotificationKindSentinelAutoFix), "skipped ("+tc.reason+"): %").Scan(&held, &pending); err != nil {
				t.Fatalf("count held rows: %v", err)
			}
			if held != 20 || pending != heldRows {
				t.Fatalf("held rows = %d of %d pending, want a lane of 20 held with reason %s, every row still pending", held, pending, tc.reason)
			}
		})
	}
}
