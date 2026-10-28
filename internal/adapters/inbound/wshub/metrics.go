// This file (metrics.go) implements the client socket's access-check
// counter (technical plan §6.2, §13.3): every session read check the
// client socket makes, at subscribe and on an open socket, is counted by
// phase and outcome.

package wshub

import (
	"context"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/narvidev/narvi/internal/app/actorauthz"
)

// wshubMeterName is this package's OTel meter name (§5.3: one named meter
// per major subsystem).
const wshubMeterName = "narvi/wshub"

// The phase attribute's values: the check before the socket is registered
// (NewClientHandler's step 5b) and the periodic one on an open socket
// (recheckClientAccessLoop).
const (
	accessCheckPhaseSubscribe = "subscribe"
	accessCheckPhaseRecheck   = "recheck"
)

// clientWSAccessCheckTotal is resolved lazily, on first use, against
// whatever MeterProvider is registered globally by then -- the
// sync.OnceValue shape httpapi's cloudIdentityMintTotalCounter uses, for
// the same reason: this package has no constructor that runs after the
// process installs its provider.
var clientWSAccessCheckTotal = sync.OnceValue(newClientWSAccessCheckTotal)

func newClientWSAccessCheckTotal() metric.Int64Counter {
	c, err := otel.Meter(wshubMeterName).Int64Counter(
		"client_ws_access_check_total",
		metric.WithDescription("Count of every session read check the client socket makes (technical plan §6.2, §13.3): phase is subscribe (before the socket is registered) or recheck (on an open socket, every ClientWSAccessRecheckInterval); outcome is allowed, denied (the socket is closed 4001) or error (the check could not be made; the socket is closed 1011). A recheck cut short because the connection was already ending is not counted."),
		metric.WithUnit("{check}"),
	)
	if err != nil {
		// Only a malformed static instrument name can fail here; logged
		// rather than swallowed, as httpapi's counters do.
		slog.Error("wshub: construct client_ws_access_check_total counter failed", "error", err)
	}
	return c
}

// accessCheckOutcome is the outcome attribute for v. A value outside the
// three verdicts is counted as an error, the way the caller treats it.
func accessCheckOutcome(v actorauthz.LinkedActorVerdict) string {
	switch v {
	case actorauthz.LinkedActorAllowed:
		return "allowed"
	case actorauthz.LinkedActorDenied:
		return "denied"
	default:
		return "error"
	}
}

// recordAccessCheck counts one access check: phase is
// accessCheckPhaseSubscribe or accessCheckPhaseRecheck. No user or session
// id is attached (cardinality).
func recordAccessCheck(ctx context.Context, phase string, v actorauthz.LinkedActorVerdict) {
	clientWSAccessCheckTotal().Add(ctx, 1, metric.WithAttributes(
		attribute.String("phase", phase),
		attribute.String("outcome", accessCheckOutcome(v)),
	))
}
