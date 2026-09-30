//go:build integration

// Integration tests proving §30.8's own epoch discipline against a REAL
// Postgres instance: "stamp the effective egress mode onto every durable
// decision artifact, and suppress if the stamp OR the current flag says
// shadow -- monotone toward suppression, in both directions." The two
// tests below are a real flip between enqueue and delivery, exactly
// what a naive effect-time-only check cannot survive -- see this
// package's own shared TestMain/newTestPool (builder_integration_test.go,
// sharedpool_integration_test.go). Run via `make test-integration`.
package outboxworker_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// createShadowEpochTestSession creates a session naming EXACTLY one repo
// (sessions.repos, migrations/000018_session_repos.up.sql) -- the shape
// postgres.OutboxStore.Create's own §30.8 resolution requires to resolve
// per-repo (rather than falling back to egressmode.ResolvePlatform, see
// outbox_shadow.go's own doc comment). Mirrors internal/app/sessionactor's
// own createTestSessionWithRepos (pushpr_integration_test.go) -- duplicated
// rather than imported, that helper is unexported in a different package.
func createShadowEpochTestSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullName string) pgtype.UUID {
	t.Helper()
	// "branch" must be present (even as null) -- restdtos.
	// CreateSessionRequestReposElem's own generated UnmarshalJSON treats
	// a MISSING branch key as a required-field error (unlike an explicit
	// null), which singleSessionRepoFullName (outbox_shadow.go) then
	// silently reads as "this session names no single repo" -- verified
	// by omitting it here and watching every assertion below that
	// expects a per-repo resolution fail instead.
	reposJSON, err := json.Marshal([]map[string]any{
		{"name": repoFullName, "url": "https://github.com/" + repoFullName, "branch": nil},
	})
	if err != nil {
		t.Fatalf("marshal test repos: %v", err)
	}
	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
		Repos:       reposJSON,
	})
	if err != nil {
		t.Fatalf("create test session with repos: %v", err)
	}
	return session.ID
}

// TestPumpOnce_BornShadow_StaysShadowAfterPromotion is §30.8's own core
// claim, proven against real Postgres with a REAL flip in between: "a
// born-shadow row is terminally shadow, whatever the flag says at
// delivery" -- retries reaching ~35 minutes and documented indefinite
// backlogs mean a row pending across a shadow->live flip must never
// materialize as a real delivery. The row is enqueued while its own repo
// is shadow (the default, no repo_settings row at all); the repo is THEN
// promoted to live BEFORE PumpOnce ever runs. If Builder re-read the flag
// at delivery time instead of trusting its own enqueue-time stamp, this
// notifier would fire for real -- it must not.
// createMultiRepoSession builds a session naming SEVERAL repositories,
// which is a first-class shape in this system (multi-repo workspaces) and
// the one the per-repo stamp used to be blind to.
func createMultiRepoSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, repoFullNames ...string) pgtype.UUID {
	t.Helper()
	entries := make([]map[string]any, 0, len(repoFullNames))
	for _, n := range repoFullNames {
		entries = append(entries, map[string]any{"name": n, "url": "https://github.com/" + n, "branch": nil})
	}
	reposJSON, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal test repos: %v", err)
	}
	session, err := narvipg.NewSessionStore(pool).Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
		Repos:       reposJSON,
	})
	if err != nil {
		t.Fatalf("create multi-repo test session: %v", err)
	}
	return session.ID
}

// TestCreate_MultiRepoSession_SuppressedIfAnyRepoIsShadow pins §30.8's
// monotone-toward-suppression rule where it has teeth.
//
// A notification about work spanning two repositories is a customer-visible
// effect for BOTH, so it may go out only if both may. The first version of
// this code bailed out for any session not naming exactly one repository
// and fell back to the deployment-wide switch alone -- which on an ordinary
// deployment resolves LIVE. Adding a second repository to a session was
// therefore enough to deliver a suppressed repository's notifications: the
// per-repo flag bypassed by a fact about the session rather than about the
// flag.
func TestCreate_MultiRepoSession_SuppressedIfAnyRepoIsShadow(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)
	repoSettings := narvipg.NewRepoSettingsStore(pool)

	const promoted = "acme/multi-promoted"
	const suppressed = "acme/multi-suppressed"
	if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, promoted, true); err != nil {
		t.Fatalf("promote first repo: %v", err)
	}
	// The second repo is left unpromoted, which is the ordinary default.

	sessionID := createMultiRepoSession(ctx, t, pool, promoted, suppressed)
	payload, err := json.Marshal(map[string]any{"channel_id": "C1", "thread_ts": "1.1", "text": "hi"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	row, err := store.Create(ctx, sqlcgen.CreateOutboxEntryParams{
		SessionID: sessionID,
		Kind:      string(ports.NotificationKindSlack),
		Payload:   payload,
	})
	if err != nil {
		t.Fatalf("create outbox entry: %v", err)
	}
	if !row.SuppressedInShadow {
		t.Fatal("a session naming one promoted and one suppressed repository was stamped LIVE -- one suppressed repository must suppress the whole notification")
	}
}

// TestCreate_MultiRepoSession_LiveWhenEveryRepoIsPromoted proves the fix
// did not simply suppress everything multi-repo, which would be safe and
// useless.
func TestCreate_MultiRepoSession_LiveWhenEveryRepoIsPromoted(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)
	repoSettings := narvipg.NewRepoSettingsStore(pool)

	const a = "acme/multi-both-a"
	const b = "acme/multi-both-b"
	for _, n := range []string{a, b} {
		if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, n, true); err != nil {
			t.Fatalf("promote %s: %v", n, err)
		}
	}

	sessionID := createMultiRepoSession(ctx, t, pool, a, b)
	payload, err := json.Marshal(map[string]any{"channel_id": "C1", "thread_ts": "1.1", "text": "hi"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	row, err := store.Create(ctx, sqlcgen.CreateOutboxEntryParams{
		SessionID: sessionID,
		Kind:      string(ports.NotificationKindSlack),
		Payload:   payload,
	})
	if err != nil {
		t.Fatalf("create outbox entry: %v", err)
	}
	if row.SuppressedInShadow {
		t.Fatal("a session whose every repository is promoted was stamped SHADOW -- the fix must not suppress all multi-repo work")
	}
}

func TestPumpOnce_BornShadow_StaysShadowAfterPromotion(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	const repoFullName = "acme/shadow-outbox-born-shadow"

	sessionID := createShadowEpochTestSession(ctx, t, pool, repoFullName)

	payload, err := json.Marshal(map[string]any{"channel_id": "C1", "thread_ts": "1.1", "text": "hi"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	row, err := store.Create(ctx, sqlcgen.CreateOutboxEntryParams{
		SessionID: sessionID,
		Kind:      string(ports.NotificationKindSlack),
		Payload:   payload,
	})
	if err != nil {
		t.Fatalf("create outbox entry: %v", err)
	}
	if !row.SuppressedInShadow {
		t.Fatalf("SuppressedInShadow = false at enqueue, want true (repo has no repo_settings row -- §30.8's own onboarding default is shadow)")
	}

	// THE FLIP: promote the repo to live AFTER enqueue, BEFORE delivery.
	if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("promote repo to live egress: %v", err)
	}

	notifier := &fakeNotifier{}
	builder, err := outboxworker.NewBuilder(store, pool, map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindSlack: notifier,
	}, platform.DefaultTimeouts(), &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	if got := notifier.deliverCount(); got != 0 {
		t.Fatalf("deliverCount = %d, want 0 (a born-shadow row must never reach notifier.Deliver, even after its repo is promoted)", got)
	}

	got, err := store.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("get outbox entry: %v", err)
	}
	if got.Status != sqlcgen.OutboxStatusDelivered {
		t.Fatalf("Status = %q, want %q (delivered-to-ledger reuses the SAME terminal status, §30.6)", got.Status, sqlcgen.OutboxStatusDelivered)
	}
	if !got.DeliveredToLedger {
		t.Fatal("DeliveredToLedger = false, want true (this row's own terminal mark: delivered into the ledger, not the world)")
	}
	if !got.DeliveredAt.Valid {
		t.Fatal("DeliveredAt.Valid = false, want true")
	}
}

// TestPumpOnce_BornLive_SuppressedAfterDemotion is §30.8's own "suppress
// wins both ways" half: a row born live, whose own repo is demoted
// before delivery, must ALSO be suppressed -- the delivery-time recheck
// Builder.attempt performs only for a row whose frozen stamp still says
// live.
func TestPumpOnce_BornLive_SuppressedAfterDemotion(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	const repoFullName = "acme/shadow-outbox-born-live"

	sessionID := createShadowEpochTestSession(ctx, t, pool, repoFullName)

	if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("promote repo to live egress: %v", err)
	}

	payload, err := json.Marshal(map[string]any{"channel_id": "C1", "thread_ts": "1.1", "text": "hi"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	row, err := store.Create(ctx, sqlcgen.CreateOutboxEntryParams{
		SessionID: sessionID,
		Kind:      string(ports.NotificationKindSlack),
		Payload:   payload,
	})
	if err != nil {
		t.Fatalf("create outbox entry: %v", err)
	}
	if row.SuppressedInShadow {
		t.Fatalf("SuppressedInShadow = true at enqueue, want false (repo was already live)")
	}

	// THE FLIP: demote the repo AFTER enqueue, BEFORE delivery.
	if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repoFullName, false); err != nil {
		t.Fatalf("demote repo to shadow egress: %v", err)
	}

	notifier := &fakeNotifier{}
	builder, err := outboxworker.NewBuilder(store, pool, map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindSlack: notifier,
	}, platform.DefaultTimeouts(), &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	if got := notifier.deliverCount(); got != 0 {
		t.Fatalf("deliverCount = %d, want 0 (a demotion between enqueue and delivery must still suppress -- suppress wins both ways)", got)
	}

	got, err := store.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("get outbox entry: %v", err)
	}
	if !got.DeliveredToLedger {
		t.Fatal("DeliveredToLedger = false, want true")
	}
}

// TestPumpOnce_PassThroughKind_DeliversEvenWhenBornShadow proves the
// OTHER half of the classification's own job: a PASS-THROUGH kind
// (§30.2 -- blob_delete, Narvi-internal storage hygiene) must run
// unconditionally, regardless of its own row's egress mode. Without
// this, the same suppress-wins logic that correctly protects a SUPPRESS
// kind would leak orphaned blobs forever in a shadow evaluation
// deployment -- the exact trap §30.2's own doc comment names.
func TestPumpOnce_PassThroughKind_DeliversEvenWhenBornShadow(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewOutboxStore(pool, false)
	const repoFullName = "acme/shadow-outbox-pass-through"

	sessionID := createShadowEpochTestSession(ctx, t, pool, repoFullName)
	// Deliberately no promotion: this repo stays shadow (the default) for
	// the whole test.

	payload, err := json.Marshal(map[string]any{"key": "blob-key-1"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	row, err := store.Create(ctx, sqlcgen.CreateOutboxEntryParams{
		SessionID: sessionID,
		Kind:      string(ports.NotificationKindBlobDelete),
		Payload:   payload,
	})
	if err != nil {
		t.Fatalf("create outbox entry: %v", err)
	}
	if !row.SuppressedInShadow {
		t.Fatalf("SuppressedInShadow = false, want true (test setup: this repo must actually be shadow for this assertion to mean anything)")
	}

	notifier := &fakeNotifier{}
	builder, err := outboxworker.NewBuilder(store, pool, map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindBlobDelete: notifier,
	}, platform.DefaultTimeouts(), &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("NewBuilder: %v", err)
	}
	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	if got := notifier.deliverCount(); got != 1 {
		t.Fatalf("deliverCount = %d, want 1 (blob_delete is PASS-THROUGH -- it must run even for a born-shadow row)", got)
	}

	got, err := store.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("get outbox entry: %v", err)
	}
	if got.DeliveredToLedger {
		t.Fatal("DeliveredToLedger = true, want false (a genuinely-delivered PASS-THROUGH row is not a ledger-terminal row)")
	}
}
