//go:build integration

// Integration tests for internal/app/digest.Pump (§21.3)
// against a real Postgres instance.
package digest_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/digest"
	"github.com/narvidev/narvi/internal/app/ports"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/platform"
)

// digestTestRig bundles every store internal/app/digest.Pump needs,
// backed by ONE shared Postgres pool (sharedpool_integration_test.go).
type digestTestRig struct {
	pool           *pgxpool.Pool
	sessions       *narvipg.SessionStore
	prSessions     *narvipg.GitHubPRSessionStore
	slackThreads   *narvipg.SlackThreadSessionStore
	reviewVerdicts *narvipg.ReviewVerdictStore
	outbox         *narvipg.OutboxStore
}

func newDigestTestRig(t *testing.T) *digestTestRig {
	t.Helper()
	pool := newTestPool(t)
	return &digestTestRig{
		pool:           pool,
		sessions:       narvipg.NewSessionStore(pool),
		prSessions:     narvipg.NewGitHubPRSessionStore(pool),
		slackThreads:   narvipg.NewSlackThreadSessionStore(pool),
		reviewVerdicts: narvipg.NewReviewVerdictStore(pool),
		outbox:         narvipg.NewOutboxStore(pool, false),
	}
}

func (rs *digestTestRig) deps() digest.Deps {
	return digest.Deps{
		Channels:  narvipg.NewDigestChannelStore(rs.pool),
		SendState: narvipg.NewDigestSendStateStore(rs.pool),
		Outbox:    rs.outbox,
		ReviewVerdict: appreviewverdict.Deps{
			ReviewVerdicts:       rs.reviewVerdicts,
			RepoSettings:         narvipg.NewRepoSettingsStore(rs.pool),
			ReviewFindings:       narvipg.NewReviewFindingStore(rs.pool),
			AutoApprovalOutcomes: narvipg.NewAutoApprovalOutcomeStore(rs.pool),
			Timeouts:             platform.DefaultTimeouts(),
		},
		Timeouts: platform.DefaultTimeouts(),
	}
}

// seedRepoWithSlackChannel creates a review session for repoFullName#prNumber
// threaded through a Slack channel (slack_thread_sessions, joined via
// github_pr_sessions -- exactly the existing association internal/app/
// digest's own channel discovery reuses, §21.3), plus a Shippable=auto
// review_verdicts row so the resulting digest has real content.
func (rs *digestTestRig) seedRepoWithSlackChannel(ctx context.Context, t *testing.T, repoFullName string, prNumber int32, channelID string) {
	t.Helper()

	session, err := rs.sessions.Create(ctx, sqlcgen.CreateSessionParams{SpawnSource: sqlcgen.SessionSpawnSourceGithub})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := rs.prSessions.EnsureRow(ctx, repoFullName, prNumber); err != nil {
		t.Fatalf("ensure github_pr_sessions row: %v", err)
	}
	if err := rs.prSessions.SetSessionID(ctx, repoFullName, prNumber, session.ID); err != nil {
		t.Fatalf("set github_pr_sessions session id: %v", err)
	}
	if _, ok, err := rs.slackThreads.Claim(ctx, channelID, "1234.5678", session.ID); err != nil || !ok {
		t.Fatalf("claim slack thread session: ok=%v err=%v", ok, err)
	}

	verdict := review.Verdict{
		RiskLevel:         review.RiskLevelLow,
		Premise:           review.PremiseStateOK,
		TestsCoverage:     review.TestsCoverageStateAdequate,
		DocsDrift:         review.DocsDriftStateNone,
		ProposedShippable: review.ProposedShippableAuto,
		FilesChanged:      3,
	}
	verdict.Shippable = review.ComputeShippable(verdict.RiskLevel, verdict.TestsCoverage, verdict.Premise, review.DescriptionAdequacyOK, review.CounterReviewDone)

	// §30.8: ListNonShadowReviewVerdictsInWindow (the digest's own
	// customer-consequential read, rollup.go) excludes any verdict whose
	// own repo has not been promoted past the live_egress_promoted_at
	// fence -- promote first so this fixture's own "the resulting digest
	// has real content" claim (this function's own doc comment above)
	// stays true.
	repoSettings := narvipg.NewRepoSettingsStore(rs.pool)
	if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("promote repo to live egress: %v", err)
	}
	if _, err := appreviewverdict.Insert(ctx, rs.reviewVerdicts, repoSettings, false, repoFullName, prNumber, "sha-digest", session.ID, verdict, reviewpost.Digest{Summary: "Test-seeded verdict."}, "", review.CounterReviewDone, reviewpost.FactCheckDone, 0, nil, nil); err != nil {
		t.Fatalf("seed review_verdicts row: %v", err)
	}
}

func (rs *digestTestRig) countSlackDigestOutboxRows(ctx context.Context, t *testing.T, channelID string) int {
	t.Helper()
	rows, err := rs.pool.Query(ctx, `SELECT payload FROM outbox WHERE kind = $1`, string(ports.NotificationKindSlackDigest))
	if err != nil {
		t.Fatalf("query outbox rows: %v", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("scan outbox payload: %v", err)
		}
		var p slackapi.DigestPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			t.Fatalf("unmarshal digest payload: %v", err)
		}
		if p.ChannelID == channelID {
			count++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox rows: %v", err)
	}
	return count
}

// TestPumpOnce_DiscoversChannelAndEnqueuesDigest proves the end-to-end
// happy path: a repo with review activity threaded through a Slack
// channel gets exactly one digest_send_state row (status 'sent') and one
// matching slack_digest outbox row, with real rendered content.
func TestPumpOnce_DiscoversChannelAndEnqueuesDigest(t *testing.T) {
	rig := newDigestTestRig(t)
	ctx := context.Background()
	const repoFullName = "acme/digest-happy-path"
	const channelID = "C_DIGEST_HAPPY"

	rig.seedRepoWithSlackChannel(ctx, t, repoFullName, 1, channelID)

	pump := digest.New(rig.deps())
	now := time.Now()
	if err := pump.PumpOnce(ctx, now); err != nil {
		t.Fatalf("PumpOnce() error = %v, want nil", err)
	}

	sendState := narvipg.NewDigestSendStateStore(rig.pool)
	sendDate := pgtype.Date{Time: now.UTC().Truncate(24 * time.Hour), Valid: true}
	claimed, err := sendState.ClaimPending(ctx, sendDate, 10)
	if err != nil {
		t.Fatalf("claim pending (post-hoc check): %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("still-pending rows after PumpOnce = %d, want 0 (the row must already be 'sent', not left 'pending')", len(claimed))
	}

	if got := rig.countSlackDigestOutboxRows(ctx, t, channelID); got != 1 {
		t.Fatalf("slack_digest outbox rows for channel %q = %d, want 1", channelID, got)
	}
}

// TestPumpOnce_SecondTickSameDay_NoDuplicateSend proves the idempotent-
// seed half of the claim-before-act guarantee: a SECOND tick, later the
// same day, must never enqueue a second digest for a channel already
// sent to.
func TestPumpOnce_SecondTickSameDay_NoDuplicateSend(t *testing.T) {
	rig := newDigestTestRig(t)
	ctx := context.Background()
	const repoFullName = "acme/digest-second-tick"
	const channelID = "C_DIGEST_SECOND_TICK"

	rig.seedRepoWithSlackChannel(ctx, t, repoFullName, 2, channelID)

	pump := digest.New(rig.deps())
	// Both ticks are anchored to a fixed midday-UTC instant on today's
	// date rather than to the raw wall clock. PumpOnce keys its send
	// state on the UTC CALENDAR DAY (pump.go's own sendDate), so a real
	// clock reading within ten minutes of midnight puts the second tick
	// on the NEXT day, where a second send is correct behaviour rather
	// than the duplicate this test exists to catch -- i.e. the test would
	// fail on a schedule while the code under test is fine. Anchoring
	// backwards is safe for discovery: discoverChannelRepos derives only
	// a lower bound from now (since = now - lookback, channels.go), never
	// an upper one, so rows seeded at the real wall clock still match.
	today := time.Now().UTC()
	firstTick := time.Date(today.Year(), today.Month(), today.Day(), 12, 0, 0, 0, time.UTC)
	if err := pump.PumpOnce(ctx, firstTick); err != nil {
		t.Fatalf("PumpOnce() (first tick) error = %v, want nil", err)
	}
	// A second tick, minutes later the SAME calendar day.
	if err := pump.PumpOnce(ctx, firstTick.Add(10*time.Minute)); err != nil {
		t.Fatalf("PumpOnce() (second tick) error = %v, want nil", err)
	}

	if got := rig.countSlackDigestOutboxRows(ctx, t, channelID); got != 1 {
		t.Fatalf("slack_digest outbox rows for channel %q after TWO ticks = %d, want 1 (at-most-one send per channel per day)", channelID, got)
	}
}

// TestPumpOnce_ConcurrentTicks_ExactlyOneSendPerChannel is §21's own
// explicitly-pinned mutation test: "the digest's at-most-one-send under
// concurrent claims". N goroutines call PumpOnce concurrently for the
// SAME channel/day (simulating multiple control-plane pods ticking at
// once) -- the SELECT ... FOR UPDATE SKIP LOCKED claim
// (ClaimPendingDigestSendState) must guarantee exactly one of them
// actually claims and sends the row, never zero (a stuck 'pending' row)
// and never more than one (a duplicate send).
func TestPumpOnce_ConcurrentTicks_ExactlyOneSendPerChannel(t *testing.T) {
	rig := newDigestTestRig(t)
	ctx := context.Background()
	const repoFullName = "acme/digest-concurrent"
	const channelID = "C_DIGEST_CONCURRENT"
	const concurrentTicks = 8

	rig.seedRepoWithSlackChannel(ctx, t, repoFullName, 3, channelID)

	now := time.Now()
	var g errgroup.Group
	var wg sync.WaitGroup
	wg.Add(concurrentTicks)
	start := make(chan struct{})
	for i := 0; i < concurrentTicks; i++ {
		g.Go(func() error {
			pump := digest.New(rig.deps())
			wg.Done()
			<-start // every goroutine races PumpOnce as simultaneously as possible
			return pump.PumpOnce(ctx, now)
		})
	}
	wg.Wait() // every goroutine is constructed and ready before any of them starts racing
	close(start)

	if err := g.Wait(); err != nil {
		t.Fatalf("concurrent PumpOnce() calls returned an error = %v, want nil", err)
	}

	if got := rig.countSlackDigestOutboxRows(ctx, t, channelID); got != 1 {
		t.Fatalf("slack_digest outbox rows for channel %q after %d concurrent ticks = %d, want EXACTLY 1 (SKIP LOCKED must guarantee at-most-one send)", channelID, concurrentTicks, got)
	}

	// Also assert the row itself settled to 'sent', never stuck 'sending'
	// or 'pending' forever -- a claim that "won" but never resolved would
	// be as real a bug as a double-send.
	var status string
	if err := rig.pool.QueryRow(ctx, `SELECT status FROM digest_send_state WHERE channel_id = $1`, channelID).Scan(&status); err != nil {
		t.Fatalf("query digest_send_state status: %v", err)
	}
	if status != "sent" {
		t.Errorf("digest_send_state.status = %q, want %q", status, "sent")
	}
}
