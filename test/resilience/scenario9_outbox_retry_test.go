//go:build integration

// Resilience scenario #9 (§9.3, docs/IMPLEMENTATION_PLAN.md row 35):
// "Outbox: Slack API 500s for 10 min -> notification eventually
// delivered, no loss." See test/resilience/README.md's own #9 entry
// (updated by this same Step) for why this was previously "deferred to a
// later phase": neither the outbox delivery worker (internal/app/
// outboxworker) nor the Slack Notifier adapter (internal/adapters/
// outbound/slackapi) existed before Step 35 ("outbox delivery").
//
// This file drives a REAL internal/app/outboxworker.Builder against a
// REAL internal/adapters/outbound/slackapi.Client, pointed at a fake
// Slack-shaped httptest.Server that returns 500 for a scripted number of
// requests before recovering -- mirroring scenario3_slow_boot_test.go's
// own "short, test-scale platform.Timeouts overrides applied before
// constructing the thing under test" convention exactly, so a real
// 10-minute outage is compressed into a fast-running test without
// changing any of the actual claim/backoff/dead-letter LOGIC under test
// (domain/outbox.EvaluateBackoff and outboxworker.Builder.PumpOnce run
// completely unmodified against real, if short, durations).
//
// Unlike scenario7/scenario12, this scenario needs no sessionactor.Registry
// at all -- outboxworker.Builder is a standalone process-wide loop that
// only needs Harness.Pool (for a real *postgres.OutboxStore) and
// Harness.Timeouts (overridden here, exactly like scenario3 overrides a
// narrow subset of h.Timeouts for its own test only) -- so this file talks
// to newHarness's own pool directly rather than adding a new Harness
// method for it.
package resilience_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/outboxworker"
	"github.com/narvidev/narvi/internal/app/ports"
	domainoutbox "github.com/narvidev/narvi/internal/domain/outbox"
	"github.com/narvidev/narvi/internal/platform"
)

// flakySlackServer is a fake Slack-shaped chat.postMessage endpoint that
// returns HTTP 500 for the first failUntil requests, then a real Slack-
// shaped {"ok": true} success response for every request after that --
// the "Slack API 500s ... then recovers" half of this scenario's own
// wording. A plain atomic counter (not a mutex-guarded struct like the
// fakes elsewhere in this repo) is enough here: outboxworker.Builder's own
// per-tick delivery attempts are never concurrent with each other WITHIN
// one PumpOnce call for a single-row batch, and this test drives PumpOnce
// serially from one goroutine.
type flakySlackServer struct {
	requests  atomic.Int64
	failUntil int64
}

func (f *flakySlackServer) handler(w http.ResponseWriter, _ *http.Request) {
	n := f.requests.Add(1)
	w.Header().Set("Content-Type", "application/json")
	if n <= f.failUntil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"ok":false,"error":"internal_error"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func TestResilienceScenario9_Outbox_SlackAPI500sThenRecovers_EventuallyDeliveredNoLoss(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	// Short, test-scale overrides -- compresses the scenario's own
	// "10 min" outage into a fast-running test, mirroring scenario3's own
	// identical override convention. domain/outbox.MaxAttempts (10) is
	// UNCHANGED: this scenario's whole point is proving the row survives
	// several consecutive failures WITHOUT crossing that threshold, then
	// recovers -- weakening MaxAttempts itself would prove nothing.
	h.Timeouts.OutboxBackoffBase = 5 * time.Millisecond
	h.Timeouts.OutboxBackoffMax = 20 * time.Millisecond
	h.Timeouts.OutboxClaimDuration = 2 * time.Millisecond
	h.Timeouts.OutboxDeliveryTimeout = 5 * time.Second

	// Drive several ticks while the fake Slack server is still failing --
	// well under domain/outbox.MaxAttempts (10) -- confirming, via DIRECT
	// Postgres inspection after each tick, that the row is NEVER
	// dead-lettered while still within budget, and is retried (attempts
	// keeps advancing) rather than silently dropped. Declared here (rather
	// than immediately before the loop below) so flakySlackServer's own
	// failUntil can reference it directly instead of a second, easily-
	// drifting literal: exactly ticksWhileFailing real delivery attempts
	// happen before the recovery tick's own attempt (one per failing tick,
	// each tick claiming and attempting exactly the one seeded row), so
	// failUntil must equal ticksWhileFailing EXACTLY for the recovery
	// tick's own delivery attempt (request number ticksWhileFailing+1) to
	// be the fake server's first successful response -- one off in either
	// direction either dead-letters/short-circuits the failing loop or
	// makes the "recovery" tick observe another failure instead.
	const ticksWhileFailing = 5

	flaky := &flakySlackServer{failUntil: ticksWhileFailing}
	server := httptest.NewServer(http.HandlerFunc(flaky.handler))
	t.Cleanup(server.Close)

	// tickSettleBuffer is extra slack slept on top of h.Timeouts.
	// OutboxBackoffMax before each subsequent tick below. A real,
	// reproduced flake (second re-verification pass, Step 39) traced back
	// to this test's own bare time.Sleep(h.Timeouts.OutboxBackoffMax)
	// leaving ZERO margin: outboxworker.recordFailure computes NextRetryAt
	// from THIS process's own time.Now(), but ListDuePendingOutboxEntries'
	// own "next_attempt_at <= now()" check runs against h.Pool's real
	// Postgres server's OWN clock -- under full-suite -race load (this
	// whole package's tests, each spinning up a testcontainers Postgres,
	// running in parallel), ordinary scheduling jitter plus any nonzero
	// skew between the two clocks can make a tick's own claim land just
	// BEFORE the row is actually due, silently skipping that tick's
	// delivery attempt entirely (attempts/status unchanged, so the
	// still-pending/not-dead-lettered assertions inside the loop below
	// still pass trivially) -- which only ever surfaces as flaky.requests
	// under-counting relative to ticksWhileFailing, exactly the observed
	// "fake Slack server saw 4 requests, want at least 5" failure. 30ms is
	// a comfortable order of magnitude above realistic same-host clock
	// skew/jitter while keeping this scenario fast.
	const tickSettleBuffer = 30 * time.Millisecond

	slackNotifier := slackapi.New(server.Client(), server.URL, "test-bot-token")

	outboxStore := narvipg.NewOutboxStore(h.Pool, false)

	payload, err := json.Marshal(slackapi.Payload{
		ChannelID: "C-scenario9",
		ThreadTS:  "1700000000.000001",
		Text:      "Turn completed successfully.",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	row, err := outboxStore.Create(ctx, sqlcgen.CreateOutboxEntryParams{
		Kind:    string(ports.NotificationKindSlack),
		Payload: payload,
	})
	if err != nil {
		t.Fatalf("seed outbox entry: %v", err)
	}

	builder, err := outboxworker.NewBuilder(outboxStore, h.Pool, map[ports.NotificationKind]ports.Notifier{
		ports.NotificationKindSlack: slackNotifier,
	}, h.Timeouts, &platform.ShutdownState{})
	if err != nil {
		t.Fatalf("outboxworker.NewBuilder: %v", err)
	}

	for i := 0; i < ticksWhileFailing; i++ {
		if err := builder.PumpOnce(ctx); err != nil {
			t.Fatalf("PumpOnce (failing tick %d): %v", i, err)
		}
		time.Sleep(h.Timeouts.OutboxBackoffMax + tickSettleBuffer)

		got, err := outboxStore.Get(ctx, row.ID)
		if err != nil {
			t.Fatalf("get outbox entry (failing tick %d): %v", i, err)
		}
		if got.Status == sqlcgen.OutboxStatusDeadLetter {
			t.Fatalf("outbox entry dead-lettered after only %d attempts (want: still retrying, budget is %d)", got.Attempts, domainoutbox.MaxAttempts)
		}
		if got.Status != sqlcgen.OutboxStatusPending {
			t.Fatalf("Status after failing tick %d = %q, want %q (still pending, not yet delivered)", i, got.Status, sqlcgen.OutboxStatusPending)
		}
	}

	if n := flaky.requests.Load(); n < ticksWhileFailing {
		t.Fatalf("fake Slack server saw %d requests, want at least %d (one real delivery attempt per failing tick)", n, ticksWhileFailing)
	}

	// The fake Slack server now recovers (every request from here on
	// succeeds) -- wait past the currently-scheduled backoff, then pump
	// again: the notification must be delivered exactly once, with no
	// loss.
	time.Sleep(h.Timeouts.OutboxBackoffMax + tickSettleBuffer)
	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce (recovery tick): %v", err)
	}

	got, err := outboxStore.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("get outbox entry (after recovery): %v", err)
	}
	if got.Status != sqlcgen.OutboxStatusDelivered {
		t.Fatalf("Status after recovery = %q, want %q (eventually delivered, no loss)", got.Status, sqlcgen.OutboxStatusDelivered)
	}
	if !got.DeliveredAt.Valid {
		t.Fatal("DeliveredAt.Valid = false after successful delivery, want true")
	}

	// One more tick, well after delivery, must NOT re-deliver (no
	// duplicate delivery): the row is no longer 'pending', so
	// ListDuePendingOutboxEntries never selects it again.
	requestsBeforeExtraTick := flaky.requests.Load()
	if err := builder.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce (post-delivery tick): %v", err)
	}
	if flaky.requests.Load() != requestsBeforeExtraTick {
		t.Fatal("fake Slack server received an extra request after delivery -- notification delivered more than once")
	}
}
