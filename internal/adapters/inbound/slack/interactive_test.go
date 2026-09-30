package slack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/platform"
)

// newSignedFormRequest builds a POST request whose body is
// application/x-www-form-urlencoded with a single "payload" field, signed
// exactly like a real Slack interactivity request (this file's own
// interactive.go doc comment: signature verification is IDENTICAL to the
// Events API ingress's own, over the raw body regardless of its own
// encoding) -- mirrors handler_test.go's own newSignedRequest/signRequest
// exactly, just over a form-encoded body instead of raw JSON.
func newSignedFormRequest(t *testing.T, secret string, ts int64, payload string) *http.Request {
	t.Helper()
	body := url.Values{"payload": {payload}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/slack/interactive", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signRequest(t, req, secret, ts, []byte(body))
	return req
}

// TestNewInteractivityHandler_SignatureVerification mirrors
// TestNewHandler_SignatureVerification (handler_test.go) exactly, over the
// form-encoded interactivity payload shape instead of the Events API's raw
// JSON body.
func TestNewInteractivityHandler_SignatureVerification(t *testing.T) {
	const secret = "test-signing-secret"
	const window = 5 * time.Minute
	now := time.Now()

	handler := NewInteractivityHandler(InteractiveDeps{
		SigningSecret: secret,
		Timeouts:      platform.Timeouts{WebhookTimestampFreshnessWindow: window},
	})

	payload := `{"type":"shortcut"}`

	tests := []struct {
		name       string
		mutate     func(req *http.Request)
		wantStatus int
	}{
		{
			name:       "valid signature and fresh timestamp accepted",
			mutate:     func(_ *http.Request) {},
			wantStatus: http.StatusOK,
		},
		{
			name: "tampered signature rejected",
			mutate: func(req *http.Request) {
				sig := req.Header.Get("X-Slack-Signature")
				last := sig[len(sig)-1]
				replacement := byte('0')
				if last == '0' {
					replacement = '1'
				}
				req.Header.Set("X-Slack-Signature", sig[:len(sig)-1]+string(replacement))
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "missing signature header rejected",
			mutate: func(req *http.Request) {
				req.Header.Del("X-Slack-Signature")
			},
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := newSignedFormRequest(t, secret, now.Unix(), payload)
			tc.mutate(req)

			rec := httptest.NewRecorder()
			handler(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestNewInteractivityHandler_ExpiredTimestampRejected is split out from
// the table above since it needs a DIFFERENT signed timestamp baked into
// the request from the start (the table's shared mutate-after-build shape
// doesn't fit a stale-timestamp case cleanly, mirroring handler_test.go's
// own "expired timestamp" case which likewise reconstructs the request).
func TestNewInteractivityHandler_ExpiredTimestampRejected(t *testing.T) {
	const secret = "test-signing-secret"
	const window = 5 * time.Minute
	now := time.Now()

	handler := NewInteractivityHandler(InteractiveDeps{
		SigningSecret: secret,
		Timeouts:      platform.Timeouts{WebhookTimestampFreshnessWindow: window},
	})

	staleTS := now.Add(-window).Add(-1 * time.Minute).Unix()
	req := newSignedFormRequest(t, secret, staleTS, `{"type":"shortcut"}`)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestNewInteractivityHandler_UnrecognizedType proves a payload type this
// handler doesn't understand degrades gracefully (200, never a crash/500)
// -- this file's own top doc comment's own explicit requirement.
func TestNewInteractivityHandler_UnrecognizedType(t *testing.T) {
	const secret = "test-signing-secret"
	handler := NewInteractivityHandler(InteractiveDeps{
		SigningSecret: secret,
		Timeouts:      platform.Timeouts{WebhookTimestampFreshnessWindow: 5 * time.Minute},
	})

	req := newSignedFormRequest(t, secret, time.Now().Unix(), `{"type":"shortcut","callback_id":"something_else"}`)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (unrecognized interaction types must degrade gracefully)", rec.Code, http.StatusOK)
	}
}

// TestNewInteractivityHandler_RequestChangesTriggersViewsOpen proves the
// request_changes_plan action calls views.open with the inbound
// trigger_id/private_metadata, responds fast (200), and does NO
// turn-creation work yet (that's view_submission's own job) -- this file's
// own doc comment's explicit sequencing.
func TestNewInteractivityHandler_RequestChangesTriggersViewsOpen(t *testing.T) {
	const secret = "test-signing-secret"

	var gotTriggerID string
	var gotPrivateMetadata string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			TriggerID string `json:"trigger_id"`
			View      struct {
				PrivateMetadata string `json:"private_metadata"`
			} `json:"view"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode views.open request: %v", err)
		}
		gotTriggerID = body.TriggerID
		gotPrivateMetadata = body.View.PrivateMetadata
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer server.Close()

	client := slackapi.New(server.Client(), server.URL, "xoxb-test-token")
	handler := NewInteractivityHandler(InteractiveDeps{
		SigningSecret: secret,
		SlackClient:   client,
		Timeouts:      platform.Timeouts{WebhookTimestampFreshnessWindow: 5 * time.Minute, SlackInteractivityAckTimeout: 5 * time.Second},
	})

	value := slackapi.EncodePlanActionValue("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
	payload := `{"type":"block_actions","trigger_id":"trigger-abc","channel":{"id":"C1"},"message":{"ts":"1.1"},"actions":[{"action_id":"` + slackapi.ActionRequestChangesPlan + `","value":"` + value + `"}]}`

	req := newSignedFormRequest(t, secret, time.Now().Unix(), payload)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body=%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if gotTriggerID != "trigger-abc" {
		t.Errorf("views.open trigger_id = %q, want %q", gotTriggerID, "trigger-abc")
	}
	if gotPrivateMetadata != value {
		t.Errorf("views.open view.private_metadata = %q, want %q", gotPrivateMetadata, value)
	}
}

// TestNewInteractivityHandler_ViewSubmission_EmptyFeedback_RespondsWithInlineError
// is the CONFIRMED MEDIUM audit finding's own regression test ("the
// pre-existing 'Request changes' modal ... still silently drops empty/
// whitespace-only feedback with zero user-visible signal"): a
// view_submission whose feedback text is empty, whitespace-only, or made up
// entirely of invisible zero-width runes must now get back a real
// response_action:"errors" body (Slack's own inline-validation-error
// mechanism, re-opening the modal) instead of the bare 200-with-no-body
// this handler used to write, which Slack renders as a silently-closed
// modal. This case never touches deps.Pool/deps.SlackClient at all (the
// blank-feedback check runs before either is needed), so this stays a
// plain unit test -- no Postgres/testcontainers required, unlike this
// package's other view_submission coverage
// (interactive_integration_test.go's own
// TestInteractivityHandler_ViewSubmission_CreatesRequestChangesTurn).
func TestNewInteractivityHandler_ViewSubmission_EmptyFeedback_RespondsWithInlineError(t *testing.T) {
	const secret = "test-signing-secret"

	tests := []struct {
		name     string
		feedback string
	}{
		{name: "empty string", feedback: ""},
		{name: "ascii whitespace only", feedback: "   "},
		{name: "tab/newline only", feedback: "\t\n"},
		// LOW audit fix regression case (zero-width runes not caught by a
		// bare strings.TrimSpace check): proves handleViewSubmission's own
		// blank check (now plandomain.IsBlankFeedback) also rejects
		// feedback made up ONLY of invisible zero-width runes.
		{name: "zero-width-space only (U+200B)", feedback: "\u200B\u200B"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewInteractivityHandler(InteractiveDeps{
				SigningSecret: secret,
				Timeouts:      platform.Timeouts{WebhookTimestampFreshnessWindow: 5 * time.Minute},
			})

			privateMetadata := slackapi.EncodePlanActionValue("11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222")
			viewSubmission := map[string]any{
				"type": "view_submission",
				"user": map[string]string{"id": "U0TEST"},
				"view": map[string]any{
					"callback_id":      slackapi.RequestChangesCallbackID,
					"private_metadata": privateMetadata,
					"state": map[string]any{
						"values": map[string]any{
							slackapi.RequestChangesBlockID: map[string]any{
								slackapi.RequestChangesActionID: map[string]any{
									"type":  "plain_text_input",
									"value": tc.feedback,
								},
							},
						},
					},
				},
			}
			raw, err := json.Marshal(viewSubmission)
			if err != nil {
				t.Fatalf("marshal view_submission payload: %v", err)
			}

			req := newSignedFormRequest(t, secret, time.Now().Unix(), string(raw))
			rec := httptest.NewRecorder()
			handler(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, http.StatusOK, rec.Body.String())
			}
			// writeViewSubmissionError must declare the answer JSON: Slack
			// reads response_action from an application/json body.
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want %q", got, "application/json")
			}

			var resp viewSubmissionErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response body: %v (body=%s)", err, rec.Body.String())
			}
			if resp.ResponseAction != "errors" {
				t.Errorf(`response_action = %q, want "errors" (body=%s) -- a bare 200 with no response_action silently closes the modal with zero signal to the submitter`, resp.ResponseAction, rec.Body.String())
			}
			if got := resp.Errors[slackapi.RequestChangesBlockID]; got != slackEmptyFeedbackErrorText {
				t.Errorf("errors[%q] = %q, want %q", slackapi.RequestChangesBlockID, got, slackEmptyFeedbackErrorText)
			}
		})
	}
}
