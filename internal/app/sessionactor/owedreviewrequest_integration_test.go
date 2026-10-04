//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	domainreviewtriage "github.com/narvidev/narvi/internal/domain/reviewtriage"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

// This file pins technical plan §24.9's owed human review requests on real
// Postgres, through the real actor: a person's review attempt (the label,
// the button, a mention) that waited behind another turn and met a moved
// pull request when it was dispatched ends context_moved, and its request
// is owed to its requester -- a row and a timer, in the dispatching
// transaction -- whose consumer re-runs it for the head the pull request
// has now, or drops it and tells its requester once.

// owedButtonText is the button lane's own sentence, as it records it.
const owedButtonText = "Manual re-review requested via the web review button."

// fakeReviewRequestAuthorizer is a scripted ports.ReviewRequestAuthorizer
// that records every request it is asked about.
type fakeReviewRequestAuthorizer struct {
	mu      sync.Mutex
	allowed bool
	err     error
	asked   []ports.ReviewRequest
	// onAsk, when set, runs as the consumer asks, with no transaction of
	// the actor's open: what a person may do meanwhile.
	onAsk func()
}

func (f *fakeReviewRequestAuthorizer) AuthorizeReviewRequest(_ context.Context, req ports.ReviewRequest) (bool, error) {
	f.mu.Lock()
	f.asked = append(f.asked, req)
	allowed, err, onAsk := f.allowed, f.err, f.onAsk
	f.mu.Unlock()
	if onAsk != nil {
		onAsk()
	}
	return allowed, err
}

func (f *fakeReviewRequestAuthorizer) requests() []ports.ReviewRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ports.ReviewRequest(nil), f.asked...)
}

// newOwedRig is newContextRig with authorizer wired as the registry's
// ReviewRequestAuthorizer.
func newOwedRig(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, reader *fakeReviewLiveReader, authorizer ports.ReviewRequestAuthorizer) *holdRig {
	t.Helper()
	rig := &holdRig{
		commander: &fakeSendCommander{},
		provider:  &fakeSpawnProvider{nextRef: ports.SandboxRef{ProviderID: "provider-owed"}},
		fetcher:   &fakeReviewDiffFetcher{nextHeadSHA: ctxMovedHead, nextBaseRef: "main", nextDiff: oneLineReadableDiff},
	}
	opts := RegistryOptions{
		ReviewDiffFetcher: rig.fetcher, GitHubBotHandle: "narvi-bot",
		GitHubOutbound:          platform.MustNewGitHubOutboundConfig("test-token"),
		ReviewSizeExclusions:    domainreviewtriage.DefaultSizeExclusions(),
		ReviewRequestAuthorizer: authorizer,
	}
	if reader != nil {
		opts.ReviewLiveReader = reader
	}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, rig.commander, rig.provider, "http://localhost:8080", nil, nil, "", nil, false, opts)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	rig.registry = r
	if rig.actor, err = r.GetOrSpawn(ctx, sessionID); err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	return rig
}

// createRequester creates the person a request is owed to.
func createRequester(ctx context.Context, t *testing.T, pool *pgxpool.Pool, label string) pgtype.UUID {
	t.Helper()
	user, err := narvipg.NewUserStore(pool).Create(ctx, sqlcgen.CreateUserParams{
		PrimaryEmail: fmt.Sprintf("owed-%s-%d@example.com", label, time.Now().UnixNano()),
		DisplayName:  "Requester " + label,
		Role:         sqlcgen.UserRoleMaintainer,
	})
	if err != nil {
		t.Fatalf("create the requester: %v", err)
	}
	return user.ID
}

// seedPersonsAttempt creates a pending review attempt of ctxRecordedHead a
// person asked for through trigger, as that lane records it -- the
// requester, the lane's own text, and moves, the moves in a row the request
// met before this turn -- created ten seconds ago, queued behind whatever
// turn is open.
func seedPersonsAttempt(ctx context.Context, t *testing.T, f *holdFixture, trigger string, requester pgtype.UUID, moves int32) sqlcgen.Turn {
	t.Helper()
	head := ctxRecordedHead
	prompt := owedButtonText + " Head: " + head
	text := owedButtonText
	// A turn no re-run carried records no moves.
	var carried *int32
	if moves > 0 {
		carried = &moves
	}
	created, err := f.turns.Create(ctx, sqlcgen.CreateTurnParams{
		SessionID: f.sessionID, Status: sqlcgen.TurnStatusPending, Prompt: &prompt,
		ReviewHeadSha: &head, ReviewVerdictContext: recordedContext(t, defaultRecordedContext()),
		IsReviewAttempt: true, RequestTrigger: &trigger, RequestedBy: requester, RequestText: &text, ContextMoves: carried,
	})
	if err != nil {
		t.Fatalf("create the person's review attempt: %v", err)
	}
	backdate(ctx, t, f, created.ID, 10)
	return created
}

// movedReader answers the pull request moved past ctxRecordedHead, to
// ctxMovedHead.
func movedReader(f *holdFixture) *fakeReviewLiveReader {
	reader := unmovedLiveReader(f.repoFullName, f.prNumber)
	reader.pr.HeadSHA = ctxMovedHead
	return reader
}

// owedRows reads the session's owed requests, oldest first.
func owedRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) []sqlcgen.OwedReviewRequest {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id, session_id, requested_by, trigger, request_text, is_review_attempt, context_moves, moved_turn_id, created_at
		FROM owed_review_requests WHERE session_id = $1 ORDER BY created_at, id`, sessionID)
	if err != nil {
		t.Fatalf("read the owed requests: %v", err)
	}
	var out []sqlcgen.OwedReviewRequest
	var scanErr error
	for rows.Next() {
		var o sqlcgen.OwedReviewRequest
		if scanErr = rows.Scan(&o.ID, &o.SessionID, &o.RequestedBy, &o.Trigger, &o.RequestText, &o.IsReviewAttempt, &o.ContextMoves, &o.MovedTurnID, &o.CreatedAt); scanErr != nil {
			break
		}
		out = append(out, o)
	}
	rows.Close()
	if scanErr != nil {
		t.Fatalf("scan an owed request: %v", scanErr)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the owed requests: %v", err)
	}
	return out
}

// owedTimer reads the session's owed_review_request timer: whether it is
// armed and due on the database's clock.
func owedTimer(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) (armed, due bool) {
	t.Helper()
	err := pool.QueryRow(ctx, `SELECT fires_at <= now() FROM session_timers WHERE session_id = $1 AND name = $2`, sessionID, TimerOwedReviewRequest).Scan(&due)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false
	}
	if err != nil {
		t.Fatalf("read the owed timer: %v", err)
	}
	return true, due
}

// assertOwed checks that attempt's end owed its request: one row naming
// it, with its requester, lane, text and moves, the timer due -- and the
// attempt's end, the row and the timer written by one transaction (equal
// xmin).
func assertOwed(ctx context.Context, t *testing.T, f *holdFixture, attempt sqlcgen.Turn, wantMoves int32) sqlcgen.OwedReviewRequest {
	t.Helper()
	rows := owedRows(ctx, t, f.pool, f.sessionID)
	if len(rows) != 1 {
		t.Fatalf("owed requests = %d, want 1", len(rows))
	}
	o := rows[0]
	if o.MovedTurnID != attempt.ID || o.RequestedBy != attempt.RequestedBy || o.Trigger != *attempt.RequestTrigger ||
		o.RequestText == nil || attempt.RequestText == nil || *o.RequestText != *attempt.RequestText || !o.IsReviewAttempt || o.ContextMoves != wantMoves {
		t.Fatalf("owed request = %+v, want the attempt %v's request (%s by %v) with %d moves", o, attempt.ID, *attempt.RequestTrigger, attempt.RequestedBy, wantMoves)
	}
	var sameTx, due bool
	if err := f.pool.QueryRow(ctx, `
		SELECT t.xmin::text = o.xmin::text AND o.xmin::text = st.xmin::text, st.fires_at <= now()
		FROM turns t
		JOIN owed_review_requests o ON o.moved_turn_id = t.id
		JOIN session_timers st ON st.session_id = t.session_id AND st.name = $2
		WHERE t.id = $1`, attempt.ID, TimerOwedReviewRequest).Scan(&sameTx, &due); err != nil {
		t.Fatalf("read the ended attempt, its owed request and the timer: %v", err)
	}
	if !sameTx || !due {
		t.Fatalf("owed: written with the attempt's end %v, timer due %v; want both", sameTx, due)
	}
	return o
}

// pumpUntilOwedServed runs real timer-pump ticks until the session owes
// nothing and its owed_review_request timer is gone.
func pumpUntilOwedServed(ctx context.Context, t *testing.T, rig *holdRig, f *holdFixture) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := rig.registry.PumpOnce(ctx); err != nil {
			t.Fatalf("PumpOnce: %v", err)
		}
		armed, _ := owedTimer(ctx, t, f.pool, f.sessionID)
		if !armed && len(owedRows(ctx, t, f.pool, f.sessionID)) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the owed request was never served")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// reRuns returns the session's review attempts of ctxMovedHead: the re-runs
// of a request owed.
func reRuns(ctx context.Context, t *testing.T, f *holdFixture) []sqlcgen.Turn {
	t.Helper()
	all, err := f.turns.ListForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var out []sqlcgen.Turn
	for _, tr := range all {
		if tr.IsReviewAttempt && tr.ReviewHeadSha != nil && *tr.ReviewHeadSha == ctxMovedHead {
			out = append(out, tr)
		}
	}
	return out
}

// dropNotices returns the bodies of the session's verdict-outbox notices.
func dropNotices(ctx context.Context, t *testing.T, f *holdFixture) []string {
	t.Helper()
	rows, err := f.pool.Query(ctx, `SELECT payload FROM outbox WHERE session_id = $1 AND kind = $2 ORDER BY created_at`, f.sessionID, string(ports.NotificationKindGitHubVerdict))
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	var raws [][]byte
	var scanErr error
	for rows.Next() {
		var raw []byte
		if scanErr = rows.Scan(&raw); scanErr != nil {
			break
		}
		raws = append(raws, raw)
	}
	rows.Close()
	if scanErr != nil {
		t.Fatalf("scan a notice: %v", scanErr)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, raw := range raws {
		var p struct {
			Body      string `json:"body"`
			RiskLevel string `json:"riskLevel"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode a notice: %v", err)
		}
		if p.RiskLevel != "" {
			t.Fatalf("a drop notice carries risk level %q: it would move the pull request's label", p.RiskLevel)
		}
		bodies = append(bodies, p.Body)
	}
	return bodies
}

// dropWarnings returns the session warnings that tell a dropped request's
// requester why, leaving aside every other warning the session records.
func dropWarnings(ctx context.Context, t *testing.T, f *holdFixture) []string {
	t.Helper()
	var out []string
	for _, w := range sessionWarnings(ctx, t, f.pool, f.sessionID) {
		if strings.Contains(w, "was not run") {
			out = append(out, w)
		}
	}
	return out
}

// mailboxBarrier returns once the actor has handled every command sent to
// it before: a frame with no type, which the actor drops and answers at
// once, writing nothing.
func mailboxBarrier(ctx context.Context, t *testing.T, a *Actor) {
	t.Helper()
	sendSandboxEventForTest(ctx, t, a, SandboxEvent{})
}

// TestReviewContext_APersonsMovedRequestIsOwedInTheDispatchingTransaction
// is the dispatch half of the exit's third sentence (technical plan
// §24.9), for each of a person's three lanes: a review attempt the label,
// the button or a mention asked for, queued behind a running turn, whose
// pull request moved while it waited, does not start when that turn ends.
// It ends context_moved, notifying nobody, and its request is owed: a row
// naming its requester, lane, text and one move, and the
// owed_review_request timer due at once -- all three written by the
// transaction that ends the attempt. The automatic lane is left as it is:
// no pending head, no debounce, no move counted on the pull request. The
// session's status reads scheduled, never settled.
func TestReviewContext_APersonsMovedRequestIsOwedInTheDispatchingTransaction(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	for i, trigger := range []string{turn.RequestTriggerLabel, turn.RequestTriggerButton, turn.RequestTriggerMention} {
		t.Run(trigger, func(t *testing.T) {
			f := newContextFixture(ctx, t, pool, fmt.Sprintf("acme/owed-dispatch-%d", i), int32(700+i))
			requester := createRequester(ctx, t, pool, trigger)
			seedRunningTurn(ctx, t, f)
			attempt := seedPersonsAttempt(ctx, t, f, trigger, requester, 0)
			auth := &fakeReviewRequestAuthorizer{allowed: true}
			rig := newOwedRig(ctx, t, pool, f.sessionID, movedReader(f), auth)

			endRunningTurn(ctx, t, f, rig)
			assertContextMoved(ctx, t, f, rig, attempt)
			assertOwed(ctx, t, f, attempt, 1)

			row := f.prSession(ctx, t)
			if row.PendingRetriggerHeadSha != nil || row.AutoRetriggerContextMoves != 0 || row.AutoRetriggerDroppedAt.Valid {
				t.Fatalf("pull request: pending %v moves %d dropped %v; want the automatic lane untouched", row.PendingRetriggerHeadSha, row.AutoRetriggerContextMoves, row.AutoRetriggerDroppedAt.Valid)
			}
			if _, _, _, ok := f.debounce(ctx, t); ok {
				t.Fatal("a person's moved request armed the automatic lane's debounce")
			}
			facts, err := narvipg.NewSessionStore(pool).ActivityFacts(ctx, f.sessionID, ReviewAutoRetriggerBudget)
			if err != nil {
				t.Fatal(err)
			}
			scheduled := false
			for _, name := range facts.ArmedTimerNames {
				if name == TimerOwedReviewRequest && TimerCountsAsScheduledWork(name, facts.ReviewRetriggerCanFire) {
					scheduled = true
				}
			}
			if !scheduled {
				t.Fatalf("armed timers %v: the owed request does not count as scheduled work", facts.ArmedTimerNames)
			}
			if n := len(auth.requests()); n != 0 {
				t.Fatalf("the authorizer was asked %d times before the consumer ran", n)
			}
		})
	}
}

// TestOwedReviewRequest_ReRunsForTheNewHeadOnThePersonsPath: the
// consumer serves a person's owed request (technical plan §24.9): it reads
// the pull request again, asks whether the requester may still have it
// run, and inserts the re-run -- a review attempt of the head the pull
// request has now, its prompt composed from the lane's own text and that
// head's diff, carrying the request's trigger, requester, text and moves --
// deleting the row and the timer in the same transaction, attributed to
// the requester in the audit log. The repository has not opted into the
// automatic re-review, and the budget is spent: neither decides a person's
// request, and no slot is spent.
func TestOwedReviewRequest_ReRunsForTheNewHeadOnThePersonsPath(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newContextFixture(ctx, t, pool, "acme/owed-rerun", 710)
	if _, err := narvipg.NewRepoSettingsStore(pool).UpsertAutoRetriggerReviewToggle(ctx, f.repoFullName, false); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE github_pr_sessions SET auto_retrigger_count = $2 WHERE session_id = $1`, f.sessionID, ReviewAutoRetriggerBudget); err != nil {
		t.Fatal(err)
	}
	requester := createRequester(ctx, t, pool, "rerun")
	seedRunningTurn(ctx, t, f)
	attempt := seedPersonsAttempt(ctx, t, f, turn.RequestTriggerButton, requester, 0)
	auth := &fakeReviewRequestAuthorizer{allowed: true}
	rig := newOwedRig(ctx, t, pool, f.sessionID, movedReader(f), auth)

	endRunningTurn(ctx, t, f, rig)
	assertContextMoved(ctx, t, f, rig, attempt)
	owed := assertOwed(ctx, t, f, attempt, 1)

	pumpUntilOwedServed(ctx, t, rig, f)
	runs := reRuns(ctx, t, f)
	if len(runs) != 1 {
		t.Fatalf("re-runs = %d, want 1", len(runs))
	}
	rerun := runs[0]
	if rerun.RequestTrigger == nil || *rerun.RequestTrigger != turn.RequestTriggerButton || rerun.RequestedBy != requester ||
		rerun.RequestText == nil || *rerun.RequestText != owedButtonText || rerun.ContextMoves == nil || *rerun.ContextMoves != 1 || !rerun.IsReviewAttempt {
		t.Fatalf("re-run = %+v, want the button's request by %v, its text, one move", rerun, requester)
	}
	if rerun.Prompt == nil || !strings.Contains(*rerun.Prompt, owedButtonText) || !strings.Contains(*rerun.Prompt, oneLineReadableDiff) {
		t.Fatalf("re-run prompt = %v, want the button's text and the new head's diff", rerun.Prompt)
	}
	if len(rerun.ReviewVerdictContext) == 0 {
		t.Fatal("the re-run recorded no context: its own move could not be checked")
	}
	asked := auth.requests()
	if len(asked) != 1 || asked[0].RequestedBy != requester.String() || asked[0].Trigger != turn.RequestTriggerButton ||
		asked[0].RepoFullName != f.repoFullName || asked[0].SessionID != f.sessionID.String() {
		t.Fatalf("authorizer asked %+v, want once, about the requester's button request on %s", asked, f.repoFullName)
	}
	var auditActor pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT actor_user_id FROM audit_log WHERE resource_type = 'turn' AND resource_id = $1 AND action = 'turn.create'`, rerun.ID.String()).Scan(&auditActor); err != nil || auditActor != requester {
		t.Fatalf("the re-run's audit row names %v (err %v), want the requester %v", auditActor, err, requester)
	}
	if row := f.prSession(ctx, t); row.AutoRetriggerCount != ReviewAutoRetriggerBudget || row.PendingRetriggerHeadSha != nil {
		t.Fatalf("pull request: count %d pending %v; want no slot spent and nothing pending", row.AutoRetriggerCount, row.PendingRetriggerHeadSha)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM outbox WHERE session_id = $1 AND kind = $2`, f.sessionID, string(ports.NotificationKindGitHubVerdict)); n != 0 {
		t.Fatalf("%d verdict notices for a request that was re-run", n)
	}
	_ = owed
}

// TestOwedReviewRequest_RequesterNoLongerAuthorizedIsDroppedAndToldOnce:
// the requester may no longer have the request run (technical plan §24.9).
// The consumer drops it, without reading the pull request: no re-run, the
// row and the timer gone, one notice
// on the pull request through the verdict outbox and one session warning,
// both saying why, written with the row's delete. A second delivery of the
// timer changes nothing: the requester is told once.
func TestOwedReviewRequest_RequesterNoLongerAuthorizedIsDroppedAndToldOnce(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newContextFixture(ctx, t, pool, "acme/owed-unauthorized", 720)
	requester := createRequester(ctx, t, pool, "unauthorized")
	seedRunningTurn(ctx, t, f)
	attempt := seedPersonsAttempt(ctx, t, f, turn.RequestTriggerLabel, requester, 0)
	auth := &fakeReviewRequestAuthorizer{allowed: false}
	rig := newOwedRig(ctx, t, pool, f.sessionID, movedReader(f), auth)

	endRunningTurn(ctx, t, f, rig)
	assertContextMoved(ctx, t, f, rig, attempt)
	owed := assertOwed(ctx, t, f, attempt, 1)
	fetchesBefore := rig.fetcher.prCalls()

	pumpUntilOwedServed(ctx, t, rig, f)
	if runs := reRuns(ctx, t, f); len(runs) != 0 {
		t.Fatalf("re-runs = %d, want none for a requester no longer authorized", len(runs))
	}
	if got := rig.fetcher.prCalls(); got != fetchesBefore {
		t.Fatalf("the pull request was read %d more times for a request that is dropped, want none", got-fetchesBefore)
	}
	notices := dropNotices(ctx, t, f)
	if len(notices) != 1 || !strings.Contains(notices[0], "the re-review label") || !strings.Contains(notices[0], "can no longer request reviews") {
		t.Fatalf("notices = %q, want one saying the label's request was dropped for its requester's access", notices)
	}
	warnings := dropWarnings(ctx, t, f)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "can no longer request reviews") {
		t.Fatalf("warnings = %q, want one saying why", warnings)
	}
	if n := len(auth.requests()); n != 1 {
		t.Fatalf("authorizer asked %d times, want 1", n)
	}

	// Told once: the timer delivered again finds nothing owed.
	if err := rig.actor.Send(ctx, TimerFired{Name: TimerOwedReviewRequest}); err != nil {
		t.Fatal(err)
	}
	mailboxBarrier(ctx, t, rig.actor)
	if n := len(dropNotices(ctx, t, f)); n != 1 {
		t.Fatalf("notices after a second delivery = %d, want still 1", n)
	}
	if n := len(dropWarnings(ctx, t, f)); n != 1 {
		t.Fatalf("warnings after a second delivery = %d, want still 1", n)
	}
	_ = owed
}

// TestOwedReviewRequest_AHumanRequestStopsAtTheBoundAndSaysSoOnce is the
// human half of the exit's fifth sentence (technical plan §24.9): a
// person's request that keeps meeting a moved context is re-run while its
// count of moves in a row is within ReviewContextMoveMaxConsecutive -- the
// re-run carrying the count on -- and the move that takes it past the
// bound drops it, telling its requester once, without asking the
// authorizer or reading the pull request.
func TestOwedReviewRequest_AHumanRequestStopsAtTheBoundAndSaysSoOnce(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	maxMoves := int32(platform.DefaultTimeouts().ReviewContextMoveMaxConsecutive)

	for i, tc := range []struct {
		name        string
		movesBefore int32
		rerun       bool
	}{
		{name: "the move that reaches the bound is re-run", movesBefore: maxMoves - 1, rerun: true},
		{name: "the move past the bound is dropped and told once", movesBefore: maxMoves},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newContextFixture(ctx, t, pool, fmt.Sprintf("acme/owed-bound-%d", i), int32(730+i))
			requester := createRequester(ctx, t, pool, fmt.Sprintf("bound-%d", i))
			seedRunningTurn(ctx, t, f)
			attempt := seedPersonsAttempt(ctx, t, f, turn.RequestTriggerButton, requester, tc.movesBefore)
			auth := &fakeReviewRequestAuthorizer{allowed: true}
			rig := newOwedRig(ctx, t, pool, f.sessionID, movedReader(f), auth)

			endRunningTurn(ctx, t, f, rig)
			assertContextMoved(ctx, t, f, rig, attempt)
			assertOwed(ctx, t, f, attempt, tc.movesBefore+1)
			fetchesBefore := rig.fetcher.prCalls()

			pumpUntilOwedServed(ctx, t, rig, f)
			runs := reRuns(ctx, t, f)
			notices := dropNotices(ctx, t, f)
			if tc.rerun {
				if len(runs) != 1 || runs[0].ContextMoves == nil || *runs[0].ContextMoves != tc.movesBefore+1 || len(notices) != 0 {
					t.Fatalf("re-runs %d (moves %v), notices %q; want one re-run carrying %d moves and no notice", len(runs), runs, notices, tc.movesBefore+1)
				}
				return
			}
			if len(runs) != 0 {
				t.Fatalf("re-runs = %d, want none past the bound", len(runs))
			}
			want := fmt.Sprintf("%d times in a row", tc.movesBefore+1)
			if len(notices) != 1 || !strings.Contains(notices[0], want) || !strings.Contains(notices[0], "Re-run review button") {
				t.Fatalf("notices = %q, want one naming the button and %q", notices, want)
			}
			if warnings := dropWarnings(ctx, t, f); len(warnings) != 1 || !strings.Contains(warnings[0], want) {
				t.Fatalf("warnings = %q, want one naming %q", warnings, want)
			}
			if n := len(auth.requests()); n != 0 {
				t.Fatalf("authorizer asked %d times for a request past the bound, want none", n)
			}
			if got := rig.fetcher.prCalls(); got != fetchesBefore {
				t.Fatalf("the pull request was read %d more times for a request past the bound, want none", got-fetchesBefore)
			}
			if err := rig.actor.Send(ctx, TimerFired{Name: TimerOwedReviewRequest}); err != nil {
				t.Fatal(err)
			}
			mailboxBarrier(ctx, t, rig.actor)
			if n := len(dropNotices(ctx, t, f)); n != 1 {
				t.Fatalf("notices after a second delivery = %d, want still 1", n)
			}
		})
	}
}

// seedOwedRequest owes a request on f's session directly, as the dispatch
// would: a person's button request whose attempt, a completed review turn
// created for it, moved -- with the owed_review_request timer due.
func seedOwedRequest(ctx context.Context, t *testing.T, f *holdFixture, requester pgtype.UUID) sqlcgen.OwedReviewRequest {
	t.Helper()
	moved, err := f.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: f.sessionID, Status: sqlcgen.TurnStatusFailed, IsReviewAttempt: true})
	if err != nil {
		t.Fatalf("create the moved attempt: %v", err)
	}
	backdate(ctx, t, f, moved.ID, 30)
	text := owedButtonText
	owed, err := narvipg.NewOwedReviewRequestStore(f.pool).Insert(ctx, sqlcgen.InsertOwedReviewRequestParams{
		SessionID: f.sessionID, RequestedBy: requester, Trigger: turn.RequestTriggerButton, RequestText: &text,
		IsReviewAttempt: true, ContextMoves: 1, MovedTurnID: moved.ID,
	})
	if err != nil {
		t.Fatalf("owe the request: %v", err)
	}
	if err := f.timers.ArmOwedReviewRequest(ctx, f.sessionID); err != nil {
		t.Fatalf("arm the owed timer: %v", err)
	}
	return owed
}

// TestReviewRetriggerHold_AnOwedHumanRequestHoldsTheAutomaticLane: while a
// person's request is owed, the automatic lane holds (technical plan
// §24.9's hold, its owed term) even with no turn of the session open: the
// debounce's firing inserts nothing, spends nothing, keeps the pushed head
// and re-arms the backstop out. A drop of the request releases the lane in
// its own transaction -- the held debounce due at once, written with the
// drop's notice -- and the next firing inserts the automatic review.
func TestReviewRetriggerHold_AnOwedHumanRequestHoldsTheAutomaticLane(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newHoldFixture(ctx, t, pool, "acme/owed-hold", 740, pgtype.UUID{})
	seedReadySandbox(ctx, t, pool, f.sessionID)
	requester := createRequester(ctx, t, pool, "hold")
	owed := seedOwedRequest(ctx, t, f, requester)
	if held, err := f.turns.ReviewRetriggerHeld(ctx, f.sessionID); err != nil || !held {
		t.Fatalf("ReviewRetriggerHeld with an owed request and no open turn = %v (err %v), want held", held, err)
	}
	// The consumer's first delivery waits: the timer moved out of reach.
	if _, err := pool.Exec(ctx, `UPDATE session_timers SET fires_at = now() + interval '1 hour' WHERE session_id = $1 AND name = $2`, f.sessionID, TimerOwedReviewRequest); err != nil {
		t.Fatal(err)
	}
	f.armDebounce(ctx, t, time.Now())
	auth := &fakeReviewRequestAuthorizer{allowed: false}
	rig := newOwedRig(ctx, t, pool, f.sessionID, nil, auth)
	rig.fetcher.nextHeadSHA = holdLiveHead

	fireHeldDebounce(ctx, t, rig, f)
	if n := f.automaticReviews(ctx, t); n != 0 {
		t.Fatalf("automatic reviews = %d while a person's request is owed, want 0", n)
	}
	if row := f.prSession(ctx, t); row.PendingRetriggerHeadSha == nil || *row.PendingRetriggerHeadSha != holdPushedHead || row.AutoRetriggerCount != 0 {
		t.Fatalf("held: pending %v count %d; want the pushed head kept and nothing spent", row.PendingRetriggerHeadSha, row.AutoRetriggerCount)
	}

	// The requester may no longer have it run: the drop releases the lane.
	if err := rig.actor.Send(ctx, TimerFired{Name: TimerOwedReviewRequest}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 5*time.Second, func() bool { return len(owedRows(ctx, t, pool, f.sessionID)) == 0 })
	var sameTx, due bool
	if err := pool.QueryRow(ctx, `
		SELECT o.xmin::text = st.xmin::text, st.fires_at <= now()
		FROM outbox o JOIN session_timers st ON st.session_id = o.session_id AND st.name = $2
		WHERE o.session_id = $1 AND o.kind = $3`, f.sessionID, TimerReviewRetriggerDebounce, string(ports.NotificationKindGitHubVerdict)).Scan(&sameTx, &due); err != nil {
		t.Fatalf("read the drop's notice and the debounce: %v", err)
	}
	if !sameTx || !due {
		t.Fatalf("after the drop: debounce written with the drop %v, due %v; want both", sameTx, due)
	}
	pumpUntilDebounceHandled(ctx, t, rig, f)
	if n := f.automaticReviews(ctx, t); n != 1 {
		t.Fatalf("automatic reviews after the drop = %d, want 1", n)
	}
	_ = owed
}

// TestOwedReviewRequest_AFailedReadKeepsTheRequestAndBacksOff: the
// consumer cannot read the pull request, or cannot evaluate the
// requester's authorization (technical plan §24.9). Neither is a verdict:
// the request is kept, nothing is re-run or told, and the timer backs off
// on the dispatch timer's schedule -- at least DispatchRetryBackoff out,
// armed_at not moved -- rather than coming back at the claim cadence.
func TestOwedReviewRequest_AFailedReadKeepsTheRequestAndBacksOff(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	backoff := platform.DefaultTimeouts().DispatchRetryBackoff

	for i, tc := range []struct {
		name  string
		setup func(rig *holdRig, auth *fakeReviewRequestAuthorizer)
	}{
		{name: "the pull request cannot be read", setup: func(rig *holdRig, _ *fakeReviewRequestAuthorizer) {
			rig.fetcher.nextPRErr = errors.New("code host unavailable")
		}},
		{name: "the authorization cannot be evaluated", setup: func(_ *holdRig, auth *fakeReviewRequestAuthorizer) {
			auth.err = errors.New("account lookup failed")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newContextFixture(ctx, t, pool, fmt.Sprintf("acme/owed-backoff-%d", i), int32(750+i))
			requester := createRequester(ctx, t, pool, fmt.Sprintf("backoff-%d", i))
			owed := seedOwedRequest(ctx, t, f, requester)
			var armedBefore pgtype.Timestamptz
			if err := pool.QueryRow(ctx, `SELECT armed_at FROM session_timers WHERE session_id = $1 AND name = $2`, f.sessionID, TimerOwedReviewRequest).Scan(&armedBefore); err != nil {
				t.Fatal(err)
			}
			auth := &fakeReviewRequestAuthorizer{allowed: true}
			rig := newOwedRig(ctx, t, pool, f.sessionID, nil, auth)
			tc.setup(rig, auth)

			if err := rig.registry.PumpOnce(ctx); err != nil {
				t.Fatal(err)
			}
			waitUntil(t, 5*time.Second, func() bool {
				var backedOff bool
				err := pool.QueryRow(ctx, `SELECT fires_at >= now() + make_interval(secs => $3) FROM session_timers WHERE session_id = $1 AND name = $2`,
					f.sessionID, TimerOwedReviewRequest, (backoff - 5*time.Second).Seconds()).Scan(&backedOff)
				return err == nil && backedOff
			})
			var armedAfter pgtype.Timestamptz
			if err := pool.QueryRow(ctx, `SELECT armed_at FROM session_timers WHERE session_id = $1 AND name = $2`, f.sessionID, TimerOwedReviewRequest).Scan(&armedAfter); err != nil {
				t.Fatal(err)
			}
			if !armedAfter.Time.Equal(armedBefore.Time) {
				t.Fatalf("the backoff moved armed_at from %v to %v: a backoff is not an arm", armedBefore.Time, armedAfter.Time)
			}
			if rows := owedRows(ctx, t, pool, f.sessionID); len(rows) != 1 || rows[0].ID != owed.ID {
				t.Fatalf("owed requests = %+v, want the request kept", rows)
			}
			if runs := reRuns(ctx, t, f); len(runs) != 0 {
				t.Fatalf("re-runs = %d, want none", len(runs))
			}
			if n := len(dropNotices(ctx, t, f)); n != 0 {
				t.Fatalf("notices = %d, want none: a failure is not a verdict", n)
			}
		})
	}
}

// TestOwedReviewRequest_AStopDropsTheRequestsItPredates: a person's stop
// drops every request the session owed when it was made (technical plan
// §24.9, §3.3), silently -- the stop is the answer. The stop timer drops
// them itself, in the transaction that disarms the owed timer with them, so
// nothing is left that no timer would ever serve; the owed timer's consumer
// applies the same rule first when it runs before the stop timer. A request
// owed after the stop was made stays, and the stop timer arms the owed
// timer again for it, which then re-runs it.
func TestOwedReviewRequest_AStopDropsTheRequestsItPredates(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	for i, tc := range []struct {
		name        string
		stopFirst   bool
		owedAfterIt bool
	}{
		{name: "the stop timer runs first", stopFirst: true},
		{name: "the owed timer runs first", stopFirst: false},
		{name: "a request owed after the stop stays", stopFirst: true, owedAfterIt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newContextFixture(ctx, t, pool, fmt.Sprintf("acme/owed-stop-%d", i), int32(760+i))
			requester := createRequester(ctx, t, pool, fmt.Sprintf("stop-%d", i))
			seedOwedRequest(ctx, t, f, requester)
			requestStop(ctx, t, pool, f.sessionID)
			var after sqlcgen.OwedReviewRequest
			if tc.owedAfterIt {
				after = seedOwedRequest(ctx, t, f, requester)
			}
			auth := &fakeReviewRequestAuthorizer{allowed: true}
			rig := newOwedRig(ctx, t, pool, f.sessionID, nil, auth)

			first := TimerStop
			if !tc.stopFirst {
				first = TimerOwedReviewRequest
			}
			if err := rig.actor.Send(ctx, TimerFired{Name: first}); err != nil {
				t.Fatal(err)
			}
			mailboxBarrier(ctx, t, rig.actor)

			rows := owedRows(ctx, t, pool, f.sessionID)
			armed, due := owedTimer(ctx, t, pool, f.sessionID)
			if n := len(dropNotices(ctx, t, f)); n != 0 {
				t.Fatalf("notices = %d, want none: a stop drops silently", n)
			}
			if runs := reRuns(ctx, t, f); len(runs) != 0 {
				t.Fatalf("re-runs = %d after the first delivery, want none of a request the stop predates", len(runs))
			}
			if !tc.owedAfterIt {
				if len(rows) != 0 || armed {
					t.Fatalf("after the %s timer alone: owed %d, timer armed %v; want nothing owed and no timer", first, len(rows), armed)
				}
				// Whatever is still armed is delivered: nothing re-runs.
				for range 3 {
					if err := rig.registry.PumpOnce(ctx); err != nil {
						t.Fatal(err)
					}
				}
				if err := rig.actor.Send(ctx, TimerFired{Name: TimerStop}); err != nil {
					t.Fatal(err)
				}
				mailboxBarrier(ctx, t, rig.actor)
				if runs := reRuns(ctx, t, f); len(runs) != 0 {
					t.Fatalf("re-runs = %d, want none", len(runs))
				}
				return
			}
			if len(rows) != 1 || rows[0].ID != after.ID || !armed || !due {
				t.Fatalf("after the stop timer: owed %+v, timer armed %v due %v; want the request owed after it kept, its timer due", rows, armed, due)
			}
			waitUntil(t, 5*time.Second, func() bool {
				if err := rig.registry.PumpOnce(ctx); err != nil {
					t.Fatal(err)
				}
				return len(reRuns(ctx, t, f)) == 1
			})
			if rows := owedRows(ctx, t, pool, f.sessionID); len(rows) != 0 {
				t.Fatalf("owed requests = %+v, want the one owed after the stop served", rows)
			}
		})
	}
}

// TestOwedReviewRequest_TheTimerStoreArmsThisKind pins the owed timer's
// two statements to its classified kind (technical plan §24.9, §43.20):
// ArmOwedReviewRequest inserts the row due at once and, armed again, moves
// it back to now and stamps armed_at, keeping created_at;
// BackOffOwedReviewRequest moves fires_at alone; neither touches another
// kind; the kind creates a turn and has its own handler.
func TestOwedReviewRequest_TheTimerStoreArmsThisKind(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newContextFixture(ctx, t, pool, "acme/owed-kind", 770)
	timeouts := platform.DefaultTimeouts()

	if n, err := f.timers.BackOffOwedReviewRequest(ctx, f.sessionID, pgtype.Timestamptz{Time: time.Now(), Valid: true}, timeouts.DispatchRetryBackoff, timeouts.DispatchRetryBackoffMax); err != nil || n != 0 {
		t.Fatalf("backing off a session with no owed timer: %d rows (err %v), want 0", n, err)
	}
	later := time.Now().Add(time.Hour)
	if _, err := f.timers.Upsert(ctx, sqlcgen.UpsertSessionTimerParams{SessionID: f.sessionID, Name: TimerTurnDeadline, FiresAt: pgtype.Timestamptz{Time: later, Valid: true}}); err != nil {
		t.Fatal(err)
	}
	if err := f.timers.ArmOwedReviewRequest(ctx, f.sessionID); err != nil {
		t.Fatalf("ArmOwedReviewRequest: %v", err)
	}
	first, err := f.timers.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: f.sessionID, Name: TimerOwedReviewRequest})
	if err != nil {
		t.Fatalf("the %q row: %v", TimerOwedReviewRequest, err)
	}
	if armed, due := owedTimer(ctx, t, pool, f.sessionID); !armed || !due {
		t.Fatalf("after the arm: armed %v due %v, want due at once", armed, due)
	}
	if n, err := f.timers.BackOffOwedReviewRequest(ctx, f.sessionID, first.CreatedAt, timeouts.DispatchRetryBackoff, timeouts.DispatchRetryBackoffMax); err != nil || n != 1 {
		t.Fatalf("BackOffOwedReviewRequest = %d rows (err %v), want 1", n, err)
	}
	backed, err := f.timers.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: f.sessionID, Name: TimerOwedReviewRequest})
	if err != nil {
		t.Fatal(err)
	}
	if !backed.ArmedAt.Time.Equal(first.ArmedAt.Time) || backed.FiresAt.Time.Before(first.ArmedAt.Time.Add(timeouts.DispatchRetryBackoff-time.Second)) {
		t.Fatalf("after the backoff: armed %v (was %v), fires %v; want armed_at kept and fires_at the backoff out", backed.ArmedAt.Time, first.ArmedAt.Time, backed.FiresAt.Time)
	}
	time.Sleep(10 * time.Millisecond)
	if err := f.timers.ArmOwedReviewRequest(ctx, f.sessionID); err != nil {
		t.Fatal(err)
	}
	again, err := f.timers.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: f.sessionID, Name: TimerOwedReviewRequest})
	if err != nil {
		t.Fatal(err)
	}
	if !again.CreatedAt.Time.Equal(first.CreatedAt.Time) || !again.ArmedAt.Time.After(first.ArmedAt.Time) {
		t.Fatalf("armed again: created %v (was %v), armed %v (was %v); want created_at kept and armed_at stamped", again.CreatedAt.Time, first.CreatedAt.Time, again.ArmedAt.Time, first.ArmedAt.Time)
	}
	if armed, due := owedTimer(ctx, t, pool, f.sessionID); !armed || !due {
		t.Fatalf("armed again: armed %v due %v, want due at once", armed, due)
	}
	other, err := f.timers.Get(ctx, sqlcgen.GetSessionTimerParams{SessionID: f.sessionID, Name: TimerTurnDeadline})
	if err != nil || !other.FiresAt.Time.Equal(later.Truncate(time.Microsecond)) {
		t.Fatalf("another kind: fires %v (err %v), want untouched at %v", other.FiresAt.Time, err, later)
	}
	if work, ok := ClassifyTimer(TimerOwedReviewRequest); !ok || work != TimerWorkCreatesTurn {
		t.Fatalf("ClassifyTimer(%q) = %v, %v; want the kind that creates a turn", TimerOwedReviewRequest, work, ok)
	}
	if !HasOwnTimerHandlerForTest(TimerOwedReviewRequest) {
		t.Fatal("the owed kind has no handler of its own: its arm would deliver it to the unknown-kind path")
	}
}

// builtInReviewStepRun attaches a live step attempt of the built-in review
// workflow to turnID, in a run of its own on the session.
func builtInReviewStepRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID, turnID pgtype.UUID) pgtype.UUID {
	t.Helper()
	var stepRun pgtype.UUID
	if err := pool.QueryRow(ctx, `
		WITH d AS (SELECT id, version FROM workflow_definitions WHERE is_built_in AND lane = 'review'),
		     s AS (SELECT sd.id FROM workflow_step_definitions sd JOIN d ON sd.workflow_definition_id = d.id ORDER BY sd.step_order LIMIT 1),
		     r AS (INSERT INTO workflow_runs (session_id, lane, workflow_definition_id, definition_version)
		           SELECT $1, 'review', d.id, d.version FROM d RETURNING id)
		INSERT INTO workflow_step_runs (workflow_run_id, step_definition_id, turn_id)
		SELECT r.id, s.id, $2 FROM r, s RETURNING id`, sessionID, turnID).Scan(&stepRun); err != nil {
		t.Fatalf("attach a workflow attempt: %v", err)
	}
	return stepRun
}

// TestOwedReviewRequest_TheReRunTakesOverTheMovedAttemptsWorkflowStep: a
// moved attempt the workflow engine tracked keeps its step attempt live
// while its request is owed (technical plan §24.9: the context_moved end
// runs no workflow hook); the re-run takes it over, and a drop ends it the
// way a failed attempt ends.
func TestOwedReviewRequest_TheReRunTakesOverTheMovedAttemptsWorkflowStep(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	for i, tc := range []struct {
		name    string
		allowed bool
	}{
		{name: "re-run", allowed: true},
		{name: "dropped", allowed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newContextFixture(ctx, t, pool, fmt.Sprintf("acme/owed-workflow-%d", i), int32(780+i))
			requester := createRequester(ctx, t, pool, fmt.Sprintf("workflow-%d", i))
			seedRunningTurn(ctx, t, f)
			attempt := seedPersonsAttempt(ctx, t, f, turn.RequestTriggerButton, requester, 0)
			stepRun := builtInReviewStepRun(ctx, t, pool, f.sessionID, attempt.ID)
			auth := &fakeReviewRequestAuthorizer{allowed: tc.allowed}
			rig := newOwedRig(ctx, t, pool, f.sessionID, movedReader(f), auth)

			endRunningTurn(ctx, t, f, rig)
			assertContextMoved(ctx, t, f, rig, attempt)
			var status string
			var attached pgtype.UUID
			read := func() {
				t.Helper()
				if err := pool.QueryRow(ctx, `SELECT status::text, turn_id FROM workflow_step_runs WHERE id = $1`, stepRun).Scan(&status, &attached); err != nil {
					t.Fatal(err)
				}
			}
			read()
			if status != "running" || attached != attempt.ID {
				t.Fatalf("while owed: step %s on %v, want running on the moved attempt", status, attached)
			}
			pumpUntilOwedServed(ctx, t, rig, f)
			read()
			if tc.allowed {
				runs := reRuns(ctx, t, f)
				if len(runs) != 1 || status != "running" || attached != runs[0].ID {
					t.Fatalf("after the re-run: step %s on %v, want running on the re-run", status, attached)
				}
				return
			}
			if status == "running" {
				t.Fatalf("after the drop: step %s on %v, want it ended", status, attached)
			}
		})
	}
}

// TestOwedReviewRequest_ServesEveryOwedRequestOldestFirst: a session owing
// two requests serves both, oldest first, one per delivery of its timer
// (technical plan §24.9): the consumer re-arms the timer due at once while
// more is owed, and deletes it with the last.
func TestOwedReviewRequest_ServesEveryOwedRequestOldestFirst(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newContextFixture(ctx, t, pool, "acme/owed-two", 790)
	first := seedOwedRequest(ctx, t, f, createRequester(ctx, t, pool, "two-first"))
	second := seedOwedRequest(ctx, t, f, createRequester(ctx, t, pool, "two-second"))
	auth := &fakeReviewRequestAuthorizer{allowed: true}
	rig := newOwedRig(ctx, t, pool, f.sessionID, nil, auth)

	if err := rig.actor.Send(ctx, TimerFired{Name: TimerOwedReviewRequest}); err != nil {
		t.Fatal(err)
	}
	mailboxBarrier(ctx, t, rig.actor)
	rows := owedRows(ctx, t, pool, f.sessionID)
	if len(rows) != 1 || rows[0].ID != second.ID {
		t.Fatalf("after one delivery: owed %+v, want the second request left", rows)
	}
	if armed, due := owedTimer(ctx, t, pool, f.sessionID); !armed || !due {
		t.Fatalf("after one delivery: timer armed %v due %v, want it due at once for the second", armed, due)
	}
	pumpUntilOwedServed(ctx, t, rig, f)
	asked := auth.requests()
	if len(asked) != 2 || asked[0].RequestedBy != first.RequestedBy.String() || asked[1].RequestedBy != second.RequestedBy.String() {
		t.Fatalf("authorizer asked %+v, want the first request's requester, then the second's", asked)
	}
	if runs := reRuns(ctx, t, f); len(runs) != 2 {
		t.Fatalf("re-runs = %d, want 2", len(runs))
	}
}

// TestOwedReviewRequest_AStopDuringTheReadDropsTheRequest: a person stops
// the session while the consumer, with no transaction open, asks whether
// the requester may still have the request run (technical plan §24.9,
// §3.3). The consumer's own transaction reads the stop and drops the
// request as the stop timer would, silently: nothing is re-run.
func TestOwedReviewRequest_AStopDuringTheReadDropsTheRequest(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	f := newContextFixture(ctx, t, pool, "acme/owed-stop-during", 795)
	seedOwedRequest(ctx, t, f, createRequester(ctx, t, pool, "stop-during"))
	auth := &fakeReviewRequestAuthorizer{allowed: true}
	auth.onAsk = func() { requestStop(ctx, t, pool, f.sessionID) }
	rig := newOwedRig(ctx, t, pool, f.sessionID, nil, auth)

	if err := rig.actor.Send(ctx, TimerFired{Name: TimerOwedReviewRequest}); err != nil {
		t.Fatal(err)
	}
	mailboxBarrier(ctx, t, rig.actor)
	if n := len(auth.requests()); n != 1 {
		t.Fatalf("authorizer asked %d times, want 1", n)
	}
	if runs := reRuns(ctx, t, f); len(runs) != 0 {
		t.Fatalf("re-runs = %d, want none of a request a stop made during the read predates", len(runs))
	}
	if rows := owedRows(ctx, t, pool, f.sessionID); len(rows) != 0 {
		t.Fatalf("owed requests = %d, want the request dropped", len(rows))
	}
	if armed, _ := owedTimer(ctx, t, pool, f.sessionID); armed {
		t.Fatal("the owed timer outlived the request the stop dropped")
	}
	if n := len(dropNotices(ctx, t, f)); n != 0 {
		t.Fatalf("notices = %d, want none: a stop drops silently", n)
	}
}
