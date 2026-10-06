//go:build integration

package sessionactor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/ports"
	domainreviewtriage "github.com/narvidev/narvi/internal/domain/reviewtriage"
	"github.com/narvidev/narvi/internal/platform"
)

// This file is technical plan §40.1's spend cap as the session actor's own
// automatic producers meet it: the automatic re-review and an owed review
// request's re-run, each refused, never failing anything and spending
// nothing a person would miss.

// spendOn stores a completed, dispatched turn of sessionID that cost usd.
func spendOn(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, usd string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO turns (session_id, status, dispatched_at, completed_at, cost_usd) VALUES ($1, 'completed', now(), now(), $2::numeric)`, sessionID, usd); err != nil {
		t.Fatalf("store the spend: %v", err)
	}
}

// guardRecorded counts sessionID's warnings saying it reached its spend cap
// and its code-host guard notices.
func guardRecorded(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) (warnings, notices int) {
	t.Helper()
	warnings = scalarInt(ctx, t, pool, `SELECT count(*) FROM events WHERE session_id = $1 AND type = 'warning' AND payload->>'message' LIKE 'This session has stopped taking new turns%'`, sessionID)
	notices = scalarInt(ctx, t, pool, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, sessionID, string(ports.NotificationKindGitHubSessionGuard))
	return warnings, notices
}

// costOnFetch is a review diff fetcher that, as the pull request is read
// -- with no transaction of the actor's open -- runs onRead first.
type costOnFetch struct {
	*fakeReviewDiffFetcher
	onRead func()
}

func (f *costOnFetch) GetPullRequest(ctx context.Context, owner, repo string, number int32, token string) (githubapi.PullRequest, error) {
	if f.onRead != nil {
		f.onRead()
		f.onRead = nil
	}
	return f.fakeReviewDiffFetcher.GetPullRequest(ctx, owner, repo, number, token)
}

// TestAutoRetrigger_AtSpendCap_DropsWithoutSpendingBudget: a debounce
// firing for a review session past its cap drops that firing -- no turn,
// none of the pull request's re-review budget spent, the pushed head kept
// as the target for the next push after a raise, the debounce deleted --
// and records the crossing's warning and its one notice. Refused before
// the code host is read; and, when a cost committed while it was read
// takes the session past its cap, refused again at the insert.
func TestAutoRetrigger_AtSpendCap_DropsWithoutSpendingBudget(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name        string
		spentBefore string
		duringFetch string
		wantPRReads int
	}{
		{name: "past the cap when it fires", spentBefore: "2.00", wantPRReads: 0},
		{name: "past the cap by a cost recorded while the pull request was read", spentBefore: "0.50", duringFetch: "1.50", wantPRReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			f := newAutoRetriggerFixture(ctx, t, pool)
			if _, err := f.repoSettings.UpsertAutoRetriggerReviewToggle(ctx, f.repoFullName, true); err != nil {
				t.Fatal(err)
			}
			setRepoCap(ctx, t, pool, f.repoFullName, "1.00")
			spendOn(ctx, t, pool, f.sessionID, tc.spentBefore)
			f.setPendingHeadSHA(ctx, t, "sha-pushed-past-the-cap")
			f.armDebounceTimer(ctx, t)
			before := f.getPRSession(ctx, t).AutoRetriggerCount

			inner := &fakeReviewDiffFetcher{nextHeadSHA: "sha-live", nextBaseRef: "main", nextDiff: oneLineReadableDiff}
			fetcher := &costOnFetch{fakeReviewDiffFetcher: inner}
			if tc.duringFetch != "" {
				fetcher.onRead = func() { spendOn(ctx, t, pool, f.sessionID, tc.duringFetch) }
			}
			r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false,
				RegistryOptions{ReviewDiffFetcher: fetcher, GitHubBotHandle: "narvi-bot", GitHubOutbound: platform.MustNewGitHubOutboundConfig("test-token"), ReviewSizeExclusions: domainreviewtriage.DefaultSizeExclusions()})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Shutdown() })
			fireDebounceTimer(ctx, t, r, f)

			reviews := scalarInt(ctx, t, pool, `SELECT count(*) FROM turns WHERE session_id = $1 AND is_review_attempt`, f.sessionID)
			if reviews != 0 {
				t.Fatalf("review turns = %d, want none past the cap", reviews)
			}
			row := f.getPRSession(ctx, t)
			if row.AutoRetriggerCount != before {
				t.Fatalf("auto_retrigger_count = %d, want %d: a refused firing spends no budget", row.AutoRetriggerCount, before)
			}
			if row.PendingRetriggerHeadSha == nil || *row.PendingRetriggerHeadSha != "sha-pushed-past-the-cap" {
				t.Fatalf("pending head = %v, want kept for the next push after a raise", row.PendingRetriggerHeadSha)
			}
			if inner.getPRCalls != tc.wantPRReads {
				t.Fatalf("pull request reads = %d, want %d", inner.getPRCalls, tc.wantPRReads)
			}
			if warnings, notices := guardRecorded(ctx, t, pool, f.sessionID); warnings != 1 || notices != 1 {
				t.Fatalf("guard warnings %d, notices %d; want one of each", warnings, notices)
			}
		})
	}
}

// TestOwedReviewRequest_AtSpendCap_DroppedAndRequesterTold: a person's
// request owed after its pull request moved, served when the session has
// since reached its spend cap, is dropped rather than re-run, and its
// requester is told once, why -- beside the crossing's own warning and
// notice. Nothing is inserted. Past the cap when the timer fires, it is
// refused before anything is done for it: the requester's authorization is
// not asked, the pull request is not read, and composing no prompt bumps no
// false-positive pattern's hit count. Past the cap by a cost recorded while
// phase 2 ran, it is refused again under the lock, at the insert.
func TestOwedReviewRequest_AtSpendCap_DroppedAndRequesterTold(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name            string
		spentBefore     string
		duringAuthorize string
		wantAsked       int
		wantPRReads     int
	}{
		{name: "past the cap when it fires", spentBefore: "1.25", wantAsked: 0, wantPRReads: 0},
		{name: "past the cap by a cost recorded while it was authorized", spentBefore: "0.25", duringAuthorize: "1.00", wantAsked: 1, wantPRReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(t)
			f := newContextFixture(ctx, t, pool, "acme/owed-at-cap", 860)
			setRepoCap(ctx, t, pool, f.repoFullName, "1.00")
			if _, err := pool.Exec(ctx, `INSERT INTO review_false_positive_patterns (repo_full_name, comment_id, comment_type, reason) VALUES ($1, 1, 'issue_comment', 'a known false positive')`, f.repoFullName); err != nil {
				t.Fatal(err)
			}
			seedOwedRequest(ctx, t, f, createRequester(ctx, t, pool, "at-cap"))
			spendOn(ctx, t, pool, f.sessionID, tc.spentBefore)
			auth := &fakeReviewRequestAuthorizer{allowed: true}
			if tc.duringAuthorize != "" {
				auth.onAsk = func() { spendOn(ctx, t, pool, f.sessionID, tc.duringAuthorize) }
			}
			rig := newOwedRig(ctx, t, pool, f.sessionID, nil, auth)

			pumpUntilOwedServed(ctx, t, rig, f)
			if runs := reRuns(ctx, t, f); len(runs) != 0 {
				t.Fatalf("re-runs = %d, want none past the cap", len(runs))
			}
			notices := dropNotices(ctx, t, f)
			if len(notices) != 1 || !strings.Contains(notices[0], "the Re-run review button") || !strings.Contains(notices[0], "spend cap") {
				t.Fatalf("drop notices = %q, want one telling the requester the session reached its spend cap", notices)
			}
			if warnings := dropWarnings(ctx, t, f); len(warnings) != 1 || !strings.Contains(warnings[0], "spend cap") {
				t.Fatalf("drop warnings = %q, want one saying why", warnings)
			}
			if warnings, notices := guardRecorded(ctx, t, pool, f.sessionID); warnings != 1 || notices != 1 {
				t.Fatalf("guard warnings %d, notices %d; want the crossing's one of each", warnings, notices)
			}
			if n := sentPrompts(t, rig.commander); n != 0 {
				t.Fatalf("prompts sent = %d, want none", n)
			}
			if n := len(auth.requests()); n != tc.wantAsked {
				t.Fatalf("authorizations asked = %d, want %d", n, tc.wantAsked)
			}
			rig.fetcher.mu.Lock()
			reads := rig.fetcher.getPRCalls
			rig.fetcher.mu.Unlock()
			if n := reads; n != tc.wantPRReads {
				t.Fatalf("pull request reads = %d, want %d", n, tc.wantPRReads)
			}
			if tc.wantPRReads == 0 {
				if hits := scalarInt(ctx, t, pool, `SELECT hit_count FROM review_false_positive_patterns WHERE repo_full_name = $1`, f.repoFullName); hits != 0 {
					t.Fatalf("false-positive hit count = %d, want 0: no prompt was composed for a request refused before it", hits)
				}
			}
		})
	}
}

// TestAutoRetrigger_FrozenAtSpendCap_TheFreezeSkipsFirst: a debounce firing
// while autonomy is frozen (technical plan §40.2) for a session that has
// also spent its cap is skipped by the freeze, which consumes nothing --
// the debounce re-armed to look again -- before the session guard is asked:
// no review turn, none of the budget spent, and no warning or notice of the
// cap. Once autonomy is unfrozen, the next firing meets the cap.
func TestAutoRetrigger_FrozenAtSpendCap_TheFreezeSkipsFirst(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newHoldFixture(ctx, t, pool, "acme/frozen-at-cap", 902, pgtype.UUID{})
	setRepoCap(ctx, t, pool, f.repoFullName, "1.00")
	spendOn(ctx, t, pool, f.sessionID, "2.00")
	f.armDebounce(ctx, t, time.Now())
	before, _, _, _ := f.debounce(ctx, t)
	freezeAutonomyForActorTest(ctx, t, pool)
	rig := newHoldRig(ctx, t, pool, f.sessionID, nil)

	if err := rig.actor.Send(ctx, TimerFired{Name: TimerReviewRetriggerDebounce}); err != nil {
		t.Fatalf("Send TimerFired: %v", err)
	}
	waitFrozenRearm(ctx, t, f, before.ArmedAt.Time)
	if n := f.reviewAttemptsOf(ctx, t, holdPushedHead); n != 0 {
		t.Fatalf("review attempts = %d, want none while frozen", n)
	}
	if pr := f.prSession(ctx, t); pr.AutoRetriggerCount != 0 {
		t.Fatalf("auto_retrigger_count = %d, want 0", pr.AutoRetriggerCount)
	}
	if warnings, notices := guardRecorded(ctx, t, pool, f.sessionID); warnings != 0 || notices != 0 {
		t.Fatalf("guard warnings %d, notices %d; want none: the freeze skipped the firing before the guard was asked", warnings, notices)
	}

	if _, err := narvipg.NewPlatformSettingsStore(pool).Unfreeze(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rig.actor.Send(ctx, TimerFired{Name: TimerReviewRetriggerDebounce}); err != nil {
		t.Fatalf("Send TimerFired: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		warnings, _ := guardRecorded(ctx, t, pool, f.sessionID)
		return warnings == 1
	})
	if n := f.reviewAttemptsOf(ctx, t, holdPushedHead); n != 0 {
		t.Fatalf("review attempts = %d, want none past the cap", n)
	}
}
