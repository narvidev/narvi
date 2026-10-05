//go:build integration

package outboxworker_test

import (
	"context"
	"encoding/json"
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
	"github.com/narvidev/narvi/internal/platform"
)

// This file pins technical plan §40.2 at the outbox: the two kinds whose
// delivery is itself an automatic action -- the sentinel auto-fix's spawn
// and the description rewrite -- are held while autonomy is frozen,
// consuming nothing (the row pending, its attempt given back, due again
// after the recheck interval), and deliver once the freeze lifts; and the
// rows held so are left out of the lag gauge.

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
// attempts back at wantAttempts (the claim's attempt given back), its
// last_error naming the skip, and due again the recheck interval from the
// tick -- never delivered, to the world or to the ledger.
func assertHeldByTheFreeze(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id pgtype.UUID, tickAt time.Time, wantAttempts int32) {
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
	if row.LastError == nil || !strings.HasPrefix(*row.LastError, "skipped (frozen): ") {
		t.Errorf("last_error = %v, want it to start with %q", row.LastError, "skipped (frozen): ")
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
	freezeAutonomy(t, f.pool)

	for tick := 1; tick <= 2; tick++ {
		if tick == 2 {
			makeDue(t, f.pool, f.row.ID)
		}
		tickAt := time.Now()
		if err := f.builder.PumpOnce(ctx); err != nil {
			t.Fatalf("tick %d: PumpOnce: %v", tick, err)
		}
		assertHeldByTheFreeze(ctx, t, f.pool, f.row.ID, tickAt, 0)
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
// description rewrite reads and writes nothing -- live, or in a shadow
// repository, where it would otherwise go to the suppression ledger: the
// freeze is read before the shadow check, so a held row reaches neither.
func TestDescriptionAutofix_Frozen_RowHeld(t *testing.T) {
	ctx := context.Background()
	for i, tc := range []struct {
		name string
		live bool
	}{
		{name: "a live repository", live: true},
		{name: "a shadow repository", live: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			row, builder, sourceControl := descriptionAutofixFreezeRow(ctx, t, pool, "frozen-description-"+string(rune('a'+i)), 120+i, tc.live)
			freezeAutonomy(t, pool)

			tickAt := time.Now()
			if err := builder.PumpOnce(ctx); err != nil {
				t.Fatalf("PumpOnce: %v", err)
			}
			assertHeldByTheFreeze(ctx, t, pool, row.ID, tickAt, 0)
			if got := sourceControl.getPRBodyCallCount(); got != 0 {
				t.Errorf("GetPRBody calls = %d, want 0", got)
			}
			if got := sourceControl.updateCallCount(); got != 0 {
				t.Errorf("UpdatePRBody calls = %d, want 0", got)
			}
		})
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
