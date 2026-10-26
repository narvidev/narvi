package slackapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/narvidev/narvi/internal/app/ports"
)

// defaultAPIBaseURL is Slack's own real Web API base -- the ONLY place
// this literal appears in this package; production wiring
// (cmd/control-plane/main.go) passes it explicitly, mirroring
// internal/adapters/outbound/githubapi's own defaultAPIBaseURL/apiBaseURL
// constructor-parameter precedent exactly.
const defaultAPIBaseURL = "https://slack.com/api"

// maxResponseBodySize bounds how much of Slack's own chat.postMessage
// response body this client ever reads -- mirrors githubapi's own
// maxResponseBodySize/internal/adapters/inbound/slack's own
// maxAckResponseBodySize precedent exactly.
const maxResponseBodySize = 1 << 20 // 1 MiB

// Payload is the JSON shape this package expects to find in an outbox
// entry's own payload column for a ports.NotificationKindSlack row --
// enqueued by internal/app/sessionactor at turn-completion time (channel
// ID + thread timestamp from the session's own reverse-looked-up
// slack_thread_sessions row) with a short, human-readable outcome
// message.
type Payload struct {
	ChannelID string `json:"channel_id"`
	ThreadTS  string `json:"thread_ts"`
	Text      string `json:"text"`
}

// Client implements ports.Notifier against Slack's real chat.postMessage
// Web API endpoint.
type Client struct {
	httpClient *http.Client
	apiBaseURL string
	botToken   string

	// installationTeamID caches auth.test's team_id for botToken: the
	// workspace this installation belongs to, which a bot token never
	// changes (users.go's installationTeam). Empty until a call has learned
	// it; guarded by teamMu, which is never held across a request.
	teamMu             sync.Mutex
	installationTeamID string
}

// var _ ports.Notifier = (*Client)(nil) makes a Notifier signature drift a
// build error, not a runtime surprise.
var _ ports.Notifier = (*Client)(nil)

// New builds a Client. httpClient must be a real client: each Deliver
// call is bounded by its own caller-supplied context deadline,
// platform.Timeouts.OutboxDeliveryTimeout, not a package-level
// http.Client.Timeout.
//
// It used to default to http.DefaultClient when nil, and §30.2 names why
// that default had to go: New(nil, ...) in a new package got a working
// client that no egress layer above it could see. A nil httpClient now
// yields one whose transport refuses every request -- the omission is
// useless rather than dangerous, and the zero value fails closed,
// mirroring githubapi.New's own identical convention.
//
// apiBaseURL still defaults to defaultAPIBaseURL when empty; production
// wiring should still pass it explicitly, the same precedent
// githubapi.New and linearapi.New set for theirs.
func New(httpClient *http.Client, apiBaseURL, botToken string) *Client {
	if httpClient == nil {
		// Not http.DefaultClient. §30.2 calls that default an attractive
		// nuisance and removes it from all four outbound constructors:
		// New(nil, ...) in a new package used to produce a WORKING client
		// that no egress layer could see. It now produces one that can
		// make no request at all -- the omission is useless rather than
		// dangerous, and the zero value fails closed.
		//
		// This surface's own gate is a later Step's work; the default had
		// to go now regardless, because a gate installed later cannot
		// reach a client somebody already constructed around it.
		httpClient = &http.Client{Transport: refusingTransport{}}
	}
	if apiBaseURL == "" {
		apiBaseURL = defaultAPIBaseURL
	}
	return &Client{
		httpClient: httpClient,
		apiBaseURL: strings.TrimSuffix(apiBaseURL, "/"),
		botToken:   botToken,
	}
}

// postMessageRequest is the subset of Slack's real chat.postMessage
// request body this client needs for a plain, unthreaded post (the
// outbox's own Deliver, below) -- see postThreadMessageRequest
// (blockkit.go) for PostAck's own threaded sibling shape.
type postMessageRequest struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts"`
	Text     string `json:"text"`
}

// postMessageResponse is Slack Web API's own universal response envelope
// -- every method responds HTTP 200 even on an API-level failure, with
// "ok": false and an "error" code naming what went wrong.
type postMessageResponse struct {
	Ok    bool   `json:"ok"`
	Error string `json:"error"`
}

// DeliveryError is returned by Deliver for a non-ok Slack API response --
// a plain, structured error mirroring githubapi.CreatePRError/APIError's
// own precedent (no transient/permanent classification: outboxworker's
// own domain/outbox.EvaluateBackoff decision is driven by attempt count
// alone, not by inspecting this error's own shape).
type DeliveryError struct {
	SlackError string
}

func (e *DeliveryError) Error() string {
	return fmt.Sprintf("slackapi: chat.postMessage failed: %s", e.SlackError)
}

// Deliver implements ports.Notifier: decodes n.Payload as Payload and
// posts it via a real chat.postMessage call, authenticated with this
// Client's own bot token (Authorization: Bearer). n.Kind is not checked --
// this Client is only ever asked to Deliver ports.NotificationKindSlack
// rows in practice (the delivery worker's own kind->Notifier routing is
// what guarantees that; see ports.Notifier's own doc comment).
func (c *Client) Deliver(ctx context.Context, n ports.Notification) error {
	var payload Payload
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		return fmt.Errorf("slackapi: decode payload: %w", err)
	}

	reqBody, err := json.Marshal(postMessageRequest{
		Channel:  payload.ChannelID,
		ThreadTS: payload.ThreadTS,
		Text:     payload.Text,
	})
	if err != nil {
		return fmt.Errorf("slackapi: encode chat.postMessage request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBaseURL+"/chat.postMessage", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("slackapi: build chat.postMessage request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+c.botToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("slackapi: chat.postMessage request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodySize))
	if err != nil {
		return fmt.Errorf("slackapi: read chat.postMessage response: %w", err)
	}

	var parsed postMessageResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return fmt.Errorf("slackapi: decode chat.postMessage response: %w", err)
	}
	if !parsed.Ok {
		return &DeliveryError{SlackError: parsed.Error}
	}
	return nil
}

// refusingTransport answers every request with an error naming the cause,
// so a construction site built without a transport fails at its first call
// with something a reader can act on -- rather than silently reaching a
// customer's systems, which is what the removed default did.
type refusingTransport struct{}

func (refusingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("slackapi: this client was built with no HTTP client, so it can make no requests")
}

// PostIdentityLinkNotice is PostEphemeral. The two differ only in what the
// SHADOW decorator records for them (internal/app/shadowslack) -- a live
// send is a live send, and duplicating the transport here would be a
// second copy of it to keep in step.
func (c *Client) PostIdentityLinkNotice(ctx context.Context, channel, userID, threadTS, text string) error {
	return c.PostEphemeral(ctx, channel, userID, threadTS, text)
}
