// This file (users.go) implements §13.2's ("identities + full RBAC",
// §13.2) own auto-link algorithm need: "Fetch the actor's profile email
// from the provider API." -- a real users.info call against Slack's Web
// API, verified against Slack's own current documentation
// (docs.slack.dev/reference/methods/users.info) during this Step's
// investigation: GET/POST users.info?user=<id>, bot-token authenticated,
// requires the users:read.email OAuth scope to see profile.email at all
// (an app without that scope gets a 200/ok:true response with profile.email
// simply absent -- this client cannot distinguish that configuration gap
// from "this user genuinely has no email set" and does not try to; both
// surface as ok=false to GetUserEmail's own caller).
//
// §13.2 defines a verified Slack email: one Slack reports as confirmed,
// for a full member of this installation's own workspace. users.info's
// user object carries the facts that decide it (UserEmail below), auth.test
// names the workspace this client's bot token belongs to
// (installationteam.go), and UserEmail.Verified applies the definition.
//
// Called from internal/adapters/inbound/slack's own identity auto-linking
// (resolveSlackActor/resolveSlackActorSingleAttempt, identity.go), NEVER
// from the outbox delivery worker's own Notifier.Deliver path -- a
// distinct call site from this package's own chat.postMessage
// Client.Deliver, reached through the SAME single Client instance either
// way (§30.3's "one client per provider" -- see this package's own
// doc.go).

package slackapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// usersInfoResponse is the subset of Slack's real users.info response body
// this client reads. Slack's own universal envelope (Ok/Error) is shared
// with postMessageResponse (client.go) but kept as its own separate type
// here rather than reused -- the two responses carry structurally
// different payloads beyond that shared envelope, and duplicating this one
// small pair of fields is simpler than threading a generic envelope type
// through two otherwise-unrelated response shapes.
//
// Every flag decodes into a plain bool and team_id into a plain string, so
// a field the payload leaves out reads as false or "". For the two facts
// UserEmail.Verified requires -- is_email_confirmed and team_id -- an
// absent field therefore reads as "not confirmed" and "not this
// workspace": the email is not verified. For the flags that exclude a
// member (deleted, is_bot, is_restricted, is_ultra_restricted,
// is_stranger), an absent field excludes nothing.
type usersInfoResponse struct {
	Ok    bool   `json:"ok"`
	Error string `json:"error"`
	User  struct {
		TeamID            string `json:"team_id"`
		Deleted           bool   `json:"deleted"`
		IsBot             bool   `json:"is_bot"`
		IsRestricted      bool   `json:"is_restricted"`
		IsUltraRestricted bool   `json:"is_ultra_restricted"`
		IsStranger        bool   `json:"is_stranger"`
		IsEmailConfirmed  bool   `json:"is_email_confirmed"`
		Profile           struct {
			Email string `json:"email"`
		} `json:"profile"`
	} `json:"user"`
}

// UserEmail is what users.info reports about one Slack member's profile
// email, with the membership facts that decide whether Narvi may treat it
// as verified (Verified). Each field names the users.info user field it is
// decoded from; see usersInfoResponse for how an absent field reads.
type UserEmail struct {
	// Email is user.profile.email; "" when Slack shows this app none.
	Email string
	// EmailConfirmed is user.is_email_confirmed.
	EmailConfirmed bool
	// Deleted is user.deleted.
	Deleted bool
	// Bot is user.is_bot.
	Bot bool
	// Restricted is user.is_restricted: a guest account.
	Restricted bool
	// UltraRestricted is user.is_ultra_restricted: a single-channel guest.
	UltraRestricted bool
	// Stranger is user.is_stranger.
	Stranger bool
	// TeamID is user.team_id: the workspace the member belongs to. A
	// member reached through a channel shared with another workspace
	// carries that workspace's id.
	TeamID string
	// InstallationTeamID is auth.test's team_id for this client's bot
	// token: the workspace this installation belongs to. Set only when
	// Email is not empty.
	InstallationTeamID string
}

// Verified reports whether Email is a verified email under §13.2's
// definition for Slack, the only kind Narvi links an identity on
// automatically: Slack reports it as confirmed, and the member is a full
// member of this installation's own workspace -- not deleted, not a bot,
// not a guest of either kind, not a stranger, and of the installation's
// own team. What Slack leaves unsaid fails closed: an absent
// is_email_confirmed or team_id, or an installation whose team auth.test
// did not name, is not verified.
func (u UserEmail) Verified() bool {
	return u.Email != "" &&
		u.EmailConfirmed &&
		!u.Deleted &&
		!u.Bot &&
		!u.Restricted &&
		!u.UltraRestricted &&
		!u.Stranger &&
		u.TeamID != "" &&
		u.TeamID == u.InstallationTeamID
}

// ErrSlackUserNotFound is Slack's own real users.info error code for a
// user id that doesn't exist (or is no longer visible to this app) --
// verified against Slack's own docs. This is the ONE users.info error this
// client treats as PERMANENT (wrapped via platform.Permanent by its own
// caller, internal/adapters/inbound/slack's resolveSlackActor, before
// identitylink.FetchEmailWithRetry's platform.Retry sees it) -- every
// other non-ok response (rate limiting, a transient 5xx, a network error)
// is left unwrapped, since §13.2 explicitly requires a fetch FAILURE to be
// retried, never treated as "this identity simply has no email".
var ErrSlackUserNotFound = errors.New("slackapi: user_not_found")

// GetUserEmail calls users.info for userID and returns what it reports
// about the member's profile email (UserEmail). ok=false, err=nil means
// Slack answered successfully but this user simply has no email visible
// to this app (either a genuinely unset profile field, or a missing
// users:read.email scope -- indistinguishable from this response alone,
// and both mean the SAME thing to this call's own caller: no email to
// match against, do not guess). When there is an email, GetUserEmail also
// sets UserEmail.InstallationTeamID, the installation's own workspace
// (installationteam.go: auth.test, asked alongside users.info on its own
// budget and cached per Client), so the result carries everything
// UserEmail.Verified reads.
//
// A non-nil error means a call itself failed -- ErrSlackUserNotFound for
// Slack's own "no such user" response (see that sentinel's own doc comment
// for why this is the one case NOT meant to be retried), any other error
// for everything else (network failure, an unexpected non-ok response, a
// malformed response body, auth.test failing, or ctx ending before
// auth.test has answered), all of which the caller should treat as
// retryable.
func (c *Client) GetUserEmail(ctx context.Context, userID string) (user UserEmail, ok bool, err error) {
	// Until the installation's workspace is known, ask for it alongside
	// users.info rather than after it: the lookup runs on its own budget
	// (installationteam.go), so this attempt still costs one call's latency.
	teamLookup := c.startTeamLookupIfUnknown()

	var parsed usersInfoResponse
	if err := c.getJSON(ctx, "users.info", url.Values{"user": {userID}}, &parsed); err != nil {
		return UserEmail{}, false, err
	}
	if !parsed.Ok {
		if parsed.Error == "user_not_found" {
			return UserEmail{}, false, ErrSlackUserNotFound
		}
		return UserEmail{}, false, fmt.Errorf("slackapi: users.info failed: %s", parsed.Error)
	}
	if parsed.User.Profile.Email == "" {
		return UserEmail{}, false, nil
	}

	installationTeamID, err := c.awaitInstallationTeam(ctx, teamLookup)
	if err != nil {
		return UserEmail{}, false, err
	}
	return UserEmail{
		Email:              parsed.User.Profile.Email,
		EmailConfirmed:     parsed.User.IsEmailConfirmed,
		Deleted:            parsed.User.Deleted,
		Bot:                parsed.User.IsBot,
		Restricted:         parsed.User.IsRestricted,
		UltraRestricted:    parsed.User.IsUltraRestricted,
		Stranger:           parsed.User.IsStranger,
		TeamID:             parsed.User.TeamID,
		InstallationTeamID: installationTeamID,
	}, true, nil
}

// getJSON calls the read-only Web API method with query, authenticated with
// this Client's bot token, and decodes the response body into out. Slack
// answers HTTP 200 with "ok": false for an API-level failure, so the caller
// reads out's own envelope; an error here is a request, read or decode
// failure.
func (c *Client) getJSON(ctx context.Context, method string, query url.Values, out any) error {
	reqURL := c.apiBaseURL + "/" + method
	if len(query) > 0 {
		reqURL += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("slackapi: build %s request: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.botToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("slackapi: %s request failed: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		return fmt.Errorf("slackapi: read %s response: %w", method, err)
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("slackapi: decode %s response: %w", method, err)
	}
	return nil
}
