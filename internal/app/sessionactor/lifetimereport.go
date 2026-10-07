package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/platform"
)

// This file is the control plane's reader of the exact deadline a
// sandbox-agent reports (technical plan §35.2). The claim that spawns or
// restores a gen stamps a conservative estimate (lifetime.go); a ready or
// heartbeat that carries lifetimeRemainingSeconds -- the whole seconds the
// sandbox's provider stated it will still let the sandbox run, counted
// when the frame was written -- can only bring that deadline earlier, never
// later (TightenSandboxLifetimeDeadline). An agent that reports nothing,
// which is every agent until a provider states a deadline to its sandbox,
// leaves the estimate standing for the gen's whole life: nothing here
// depends on an agent-side change having shipped into a snapshot.

// reportedLifetimeRemaining returns the lifetimeRemainingSeconds a ready or
// heartbeat reports, in whole seconds within [0, ceiling], and false when
// it reports none.
//
// It reads the one key itself, leniently, never through the generated
// sandboxws types, and the ready's other readers decode the frame without
// it (decodeReady, framekey.go): whatever the key holds, the ready's
// promptReceipt and maxFrameBytes read as they would without it. A value
// the contract does not allow -- negative, fractional, larger than any
// lifetime -- is clamped: a fraction is rounded down and a negative value
// read as 0, both toward an earlier deadline; anything above ceiling,
// ProviderHardCap, is ceiling, which no kind's lifetime exceeds
// (platform.Timeouts.Validate). A key that is absent or null, or that does
// not decode as a number, reports nothing, and the estimate stands.
//
// The value is counted on the sandbox's clock, from the instant its
// provider stated, and the store adds it to the database's now(): the
// sandbox clock's offset from the provider's, and the frame's delivery
// time, both enter the deadline (technical plan §35.2).
func reportedLifetimeRemaining(raw json.RawMessage, ceiling time.Duration) (int32, bool) {
	var frame struct {
		LifetimeRemainingSeconds *json.Number `json:"lifetimeRemainingSeconds"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil || frame.LifetimeRemainingSeconds == nil {
		return 0, false
	}
	// The decoder only accepts a valid JSON number here, so the one error
	// left is a value out of float64's range, which ParseFloat returns as
	// an infinity, clamped below like any other value.
	seconds, err := strconv.ParseFloat(frame.LifetimeRemainingSeconds.String(), 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, false
	}
	limit := platform.DurationToSeconds(ceiling)
	switch {
	case seconds <= 0:
		return 0, true
	case seconds >= float64(limit):
		return limit, true
	default:
		return int32(math.Floor(seconds)), true
	}
}

// lifetimeTightening is what a ready or heartbeat brought a gen's deadline
// earlier with, carried out of handleSandboxEvent's transaction so it is
// logged only once that transaction has committed.
type lifetimeTightening struct {
	eventType        string
	gen              int32
	remainingSeconds int32
}

// tightenReportedLifetime runs, in the transaction that stores a ready or
// heartbeat of gen -- the live gen, which handleSandboxEvent's fence has
// already checked -- the report the event carries, if any. It returns the
// tightening when the deadline moved earlier, and nil when the event
// reports nothing or its report would not bring the deadline earlier.
func (a *Actor) tightenReportedLifetime(ctx context.Context, tx pgx.Tx, eventType string, raw json.RawMessage, gen int32) (*lifetimeTightening, error) {
	remaining, ok := reportedLifetimeRemaining(raw, a.timeouts.ProviderHardCap)
	if !ok {
		return nil, nil
	}
	tightened, err := a.stores.sandbox.WithTx(tx).TightenLifetimeDeadline(ctx, a.sessionID, gen, remaining)
	if err != nil {
		return nil, fmt.Errorf("sessionactor: record the reported lifetime deadline: %w", err)
	}
	if !tightened {
		return nil, nil
	}
	return &lifetimeTightening{eventType: eventType, gen: gen, remainingSeconds: remaining}, nil
}

// logLifetimeTightened records, after the event's transaction committed,
// that a sandbox-agent's report brought its gen's deadline earlier than the
// estimate: what an operator reads to tell the provider's stated deadline
// from the control plane's own assumption.
func (a *Actor) logLifetimeTightened(t *lifetimeTightening) {
	a.logger.Info(lifetimeTightenedLogMessage,
		"event_type", t.eventType, "gen", t.gen, "lifetime_remaining_seconds", t.remainingSeconds)
}

// lifetimeTightenedLogMessage is logLifetimeTightened's message.
const lifetimeTightenedLogMessage = "sessionactor: sandbox lifetime deadline brought earlier by the deadline its provider stated"
