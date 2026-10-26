// This file (installationteam.go) learns the workspace this client's bot
// token belongs to -- the installation's own workspace, which §13.2's
// definition of a verified Slack email compares each member's team_id
// against (UserEmail.Verified, users.go).
//
// One auth.test call answers it, and a bot token never changes workspace,
// so the first non-empty answer is kept for the Client's lifetime. Until
// then the call is made:
//
//   - on its own context, bounded by platform.Timeouts.
//     SlackInstallationTeamLookupTimeout and never derived from a caller's.
//     A profile-email attempt has a budget sized for one call
//     (IdentityEmailFetchTimeout, SlackInteractivityIdentityFetchTimeout);
//     a lookup bounded by it would be cut off at the very latency the
//     attempt tolerates, every attempt in turn, and never cached. An
//     attempt that stops waiting leaves the lookup running, so its answer
//     is there for the next attempt.
//   - once at a time: every caller waiting while a lookup is in flight
//     shares it (singleflight), so concurrent events cost one call.
//   - alongside users.info (GetUserEmail starts it before reading the
//     member), so an attempt costs one call's latency, not two.
//   - once at startup, best effort and without blocking boot
//     (WarmInstallationTeam), outside the development stage: the control
//     plane's serve() skips it there, where this repository's own targets
//     boot with placeholder credentials, and the lookup waits for the first
//     real event.
//
// A failed lookup is never silent: it logs a Warn and counts in
// slack_installation_team_lookup_failures_total. It also fails closed --
// with no workspace known, no Slack email is verified.

package slackapi

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/singleflight"
)

// meterName is this package's OTel meter name, after the "narvi/<package>"
// convention of internal/app/identitylink's own.
const meterName = "narvi/slackapi"

// teamLookupKey is the one singleflight key: a Client has one bot token,
// so one installation workspace to learn.
const teamLookupKey = "auth.test"

// teamLookupFailures counts auth.test lookups that did not learn the
// installation's workspace, labeled by reason: "request_failed" (the call
// failed or Slack answered ok=false) or "no_team" (Slack answered ok with no
// team_id). Package-level, like identitylink's emailFetchFailures: a Client
// is built by a plain constructor at several call sites, and otel.Meter is
// safe to call before the process's MeterProvider is set.
var teamLookupFailures = newTeamLookupFailuresCounter()

func newTeamLookupFailuresCounter() metric.Int64Counter {
	counter, err := otel.Meter(meterName).Int64Counter(
		"slack_installation_team_lookup_failures_total",
		metric.WithDescription("Count of auth.test lookups that did not learn the Slack installation's own workspace. While it is unknown, no Slack profile email reads as verified (§13.2), so no Slack identity links automatically. Labeled by reason: request_failed or no_team."),
		metric.WithUnit("{lookup}"),
	)
	if err != nil {
		// Unreachable for a well-formed constant name; panicking at
		// package load surfaces a broken build at once, as identitylink's
		// own counter does.
		panic(fmt.Sprintf("slackapi: construct slack_installation_team_lookup_failures_total counter: %v", err))
	}
	return counter
}

// authTestResponse is the subset of Slack's auth.test response this client
// reads: team_id, the workspace the calling token belongs to.
type authTestResponse struct {
	Ok     bool   `json:"ok"`
	Error  string `json:"error"`
	TeamID string `json:"team_id"`
}

// WithInstallationTeamLookupTimeout sets the budget of the auth.test
// lookup (platform.Timeouts.SlackInstallationTeamLookupTimeout) and
// returns c. New starts from platform.DefaultTimeouts()' value; call this
// once, at wiring, before the client is shared.
func (c *Client) WithInstallationTeamLookupTimeout(d time.Duration) *Client {
	c.teamLookupTimeout = d
	return c
}

// WarmInstallationTeam starts the auth.test lookup unless the workspace is
// already known, and returns at once: the lookup runs on its own bounded
// context, so a slow or failing Slack never holds up the caller (the
// control plane's boot). A failure is logged and counted like any other
// lookup's, and the next profile-email fetch asks again.
func (c *Client) WarmInstallationTeam() {
	c.startTeamLookupIfUnknown()
}

func (c *Client) cachedInstallationTeam() string {
	c.teamMu.Lock()
	defer c.teamMu.Unlock()
	return c.installationTeamID
}

// startTeamLookupIfUnknown returns the channel the in-flight (or a newly
// started) auth.test lookup answers on, or nil when the workspace is
// already known. The channel is buffered by singleflight, so a caller that
// never reads it leaks nothing.
func (c *Client) startTeamLookupIfUnknown() <-chan singleflight.Result {
	if c.cachedInstallationTeam() != "" {
		return nil
	}
	return c.teamLookups.DoChan(teamLookupKey, func() (any, error) {
		return c.lookupInstallationTeam()
	})
}

// awaitInstallationTeam returns the installation's workspace: the cached
// one, or the answer of lookup (started by the caller; one is started here
// when lookup is nil and none is known). It waits no longer than ctx -- the
// caller's own attempt -- and an attempt that stops waiting leaves the
// lookup running. "" with a nil error is an auth.test that named no team:
// nothing is verified.
func (c *Client) awaitInstallationTeam(ctx context.Context, lookup <-chan singleflight.Result) (string, error) {
	if team := c.cachedInstallationTeam(); team != "" {
		return team, nil
	}
	if lookup == nil {
		lookup = c.startTeamLookupIfUnknown()
		if lookup == nil {
			return c.cachedInstallationTeam(), nil
		}
	}
	select {
	case r := <-lookup:
		if r.Err != nil {
			return "", r.Err
		}
		team, _ := r.Val.(string)
		return team, nil
	case <-ctx.Done():
		return "", fmt.Errorf("slackapi: auth.test had not answered when the attempt ended: %w", ctx.Err())
	}
}

// lookupInstallationTeam makes the auth.test call on its own context,
// bounded by teamLookupTimeout and detached from every caller's, keeps a
// non-empty team_id, and logs and counts any lookup that learns none.
func (c *Client) lookupInstallationTeam() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.teamLookupTimeout)
	defer cancel()

	var parsed authTestResponse
	err := c.getJSON(ctx, "auth.test", nil, &parsed)
	if err == nil && !parsed.Ok {
		err = fmt.Errorf("slackapi: auth.test failed: %s", parsed.Error)
	}
	if err != nil {
		slog.Default().Warn("slackapi: could not learn the Slack installation's workspace; no Slack email reads as verified until it is learned", "error", err)
		teamLookupFailures.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", "request_failed")))
		return "", err
	}
	if parsed.TeamID == "" {
		slog.Default().Warn("slackapi: auth.test named no team for the bot token; no Slack email reads as verified until one is learned")
		teamLookupFailures.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", "no_team")))
		return "", nil
	}

	c.teamMu.Lock()
	c.installationTeamID = parsed.TeamID
	c.teamMu.Unlock()
	return parsed.TeamID, nil
}
