//go:build integration

// export_test.go bridges a small, deliberately narrow set of this
// package's own unexported symbols to its external (slack_test)
// integration test suite -- the standard Go "export_test.go" pattern.
//
// Needed for two tests. The first is textverdict_integration_test.go's own
// TestHandlePlanVerdict_UnauthorizedActor_DeniedByOwnAuthorizationCheck.
// That test proves handlePlanVerdict's own authorizeSessionAction(...,
// authz.ActionApprovePlan) call (this batch's own addition, "honour a
// typed plan verdict") independently denies an unauthorized-but-linked
// actor -- which CANNOT be exercised via a full HTTP-level (black-box)
// request: resolveOrClaimSession's own PRE-EXISTING
// authorizeExistingSessionReply gate (domain/authz.ActionPromptSession,
// handler.go) runs unconditionally for ANY reply on an already-mapped
// thread, BEFORE handleEvent's own plan-verdict check ever gets a chance
// to run, and today's authz matrix (authorize.go's own "row 2" comment)
// gives ActionPromptSession and ActionApprovePlan the IDENTICAL
// allow/allowIfOwned role sets -- so any actor a full HTTP request could
// ever get past that outer gate with would ALSO pass handlePlanVerdict's
// own inner one, making the inner denial branch unreachable (and its own
// removal undetectable) from black-box tests alone. This bridges JUST
// enough (handlePlanVerdict itself) to call it directly, bypassing the
// outer gate entirely -- never a general-purpose test-only API surface.
//
// The second is requestchangesrefusal_integration_test.go's
// TestRequestChangesRefusalText_KeysOnTheSentinel: which text the
// Request-changes modal shows for a failed create must follow the
// httpapi.ErrTurnAlreadyOpen sentinel, never the refusal's Message or
// Status. A real CreateTurnCore only ever pairs that sentinel with one
// message and one status, so no HTTP-level request can tell a check on the
// type from a check on the text; the test feeds the real refusal, and
// copies of it with the text or the type changed, to
// requestChangesRefusalText directly.
package slack

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	"github.com/narvidev/narvi/internal/platform"
)

// HandlePlanVerdictForTest bridges Deps.handlePlanVerdict (handler.go) --
// see this file's own top doc comment. A pure pass-through (never a
// second, drifted implementation of its own), returning handleEventResult's
// two EXPORTED fields as plain bools rather than naming that unexported
// type in this bridge's own exported signature. deps.SlackClient carries
// whatever shadowslack.Client the caller wired -- this package no longer
// constructs one of its own (§30.3), so there is no separate ack argument
// to bridge any more.
func (deps Deps) HandlePlanVerdictForTest(ctx context.Context, channel, key string, sessionID, planID pgtype.UUID, verdict string, actorUserID pgtype.UUID) (ok, releaseMessageClaim bool) {
	result := deps.handlePlanVerdict(ctx, platform.Logger(ctx), channel, key, sessionID, planID, verdict, actorUserID)
	return result.OK, result.ReleaseMessageClaim
}

// RequestChangesRefusalTextForTest bridges requestChangesRefusalText
// (interactive.go) -- see this file's own top doc comment. A pure
// pass-through.
func RequestChangesRefusalTextForTest(cerr *httpapi.CreateTurnError) string {
	return requestChangesRefusalText(cerr)
}
