package slackapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/domain/framecut"
)

// TestEncodeDecodePlanActionValue_RoundTrip proves the button-value
// encoding used by every one of this Step's own approve/reject/
// request-changes buttons round-trips exactly, and that a malformed value
// (missing separator, empty half) is rejected rather than silently
// misparsed.
func TestEncodeDecodePlanActionValue_RoundTrip(t *testing.T) {
	tests := []struct {
		name      string
		planID    string
		sessionID string
	}{
		{name: "typical uuids", planID: "11111111-1111-1111-1111-111111111111", sessionID: "22222222-2222-2222-2222-222222222222"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			value := slackapi.EncodePlanActionValue(tc.planID, tc.sessionID)
			gotPlanID, gotSessionID, ok := slackapi.DecodePlanActionValue(value)
			if !ok {
				t.Fatalf("DecodePlanActionValue(%q) ok = false, want true", value)
			}
			if gotPlanID != tc.planID || gotSessionID != tc.sessionID {
				t.Errorf("DecodePlanActionValue(%q) = (%q, %q), want (%q, %q)", value, gotPlanID, gotSessionID, tc.planID, tc.sessionID)
			}
		})
	}

	malformed := []string{"", "no-separator-at-all", "|missing-plan-id", "missing-session-id|"}
	for _, v := range malformed {
		if _, _, ok := slackapi.DecodePlanActionValue(v); ok {
			t.Errorf("DecodePlanActionValue(%q) ok = true, want false", v)
		}
	}
}

// TestPostPlanApprovalMessage_Success proves the real Block Kit request
// shape (channel/thread_ts/blocks with 3 buttons carrying the encoded
// value) and that the message's own real channel+ts are read back from
// Slack's response.
func TestPostPlanApprovalMessage_Success(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C999", "ts": "1700000000.000100"})
	}))
	defer server.Close()

	client := slackapi.New(server.Client(), server.URL, "xoxb-test-token")

	channel, ts, err := client.PostPlanApprovalMessage(context.Background(), slackapi.PlanApprovalPayload{
		PlanID:    "plan-1",
		SessionID: "session-1",
		ChannelID: "C123",
		ThreadTS:  "1234.5678",
		Version:   2,
		Text:      "1. Do the thing\n2. Do the other thing",
	})
	if err != nil {
		t.Fatalf("PostPlanApprovalMessage() error = %v, want nil", err)
	}
	if channel != "C999" || ts != "1700000000.000100" {
		t.Errorf("(channel, ts) = (%q, %q), want (%q, %q)", channel, ts, "C999", "1700000000.000100")
	}
	if gotPath != "/chat.postMessage" {
		t.Errorf("request path = %q, want %q", gotPath, "/chat.postMessage")
	}
	if gotBody["channel"] != "C123" {
		t.Errorf("channel = %v, want %q", gotBody["channel"], "C123")
	}
	if gotBody["thread_ts"] != "1234.5678" {
		t.Errorf("thread_ts = %v, want %q", gotBody["thread_ts"], "1234.5678")
	}
	blocks, ok := gotBody["blocks"].([]any)
	if !ok || len(blocks) == 0 {
		t.Fatalf("blocks = %v, want a non-empty array", gotBody["blocks"])
	}
	// The last block must be the actions row with exactly 3 buttons, each
	// carrying the SAME encoded planId|sessionId value.
	lastBlock, ok := blocks[len(blocks)-1].(map[string]any)
	if !ok || lastBlock["type"] != "actions" {
		t.Fatalf("last block = %v, want an actions block", blocks[len(blocks)-1])
	}
	elements, ok := lastBlock["elements"].([]any)
	if !ok || len(elements) != 3 {
		t.Fatalf("actions elements = %v, want exactly 3 buttons", lastBlock["elements"])
	}
	wantValue := slackapi.EncodePlanActionValue("plan-1", "session-1")
	wantActionIDs := map[string]bool{slackapi.ActionApprovePlan: false, slackapi.ActionRejectPlan: false, slackapi.ActionRequestChangesPlan: false}
	for _, el := range elements {
		elem, ok := el.(map[string]any)
		if !ok {
			t.Fatalf("button element = %v, not an object", el)
		}
		if elem["value"] != wantValue {
			t.Errorf("button value = %v, want %q", elem["value"], wantValue)
		}
		actionID, _ := elem["action_id"].(string)
		if _, known := wantActionIDs[actionID]; !known {
			t.Errorf("unexpected action_id %q", actionID)
		}
		wantActionIDs[actionID] = true
	}
	for actionID, seen := range wantActionIDs {
		if !seen {
			t.Errorf("expected button with action_id %q was not present", actionID)
		}
	}
}

// TestPostPlanApprovalMessage_APIError proves a non-ok Slack response
// surfaces as *DeliveryError, mirroring Deliver's own identical precedent.
func TestPostPlanApprovalMessage_APIError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "channel_not_found"})
	}))
	defer server.Close()

	client := slackapi.New(server.Client(), server.URL, "xoxb-test-token")
	_, _, err := client.PostPlanApprovalMessage(context.Background(), slackapi.PlanApprovalPayload{ChannelID: "C123", ThreadTS: "1.1"})
	if err == nil {
		t.Fatal("PostPlanApprovalMessage() error = nil, want non-nil")
	}
}

// TestPlanApprovalMessage_CutPlan_OffersNoApprove proves a plan whose text
// was cut on its way from the sandbox (payload.Cut, technical plan §6.1) is
// posted without the Approve & build button -- its approval would be
// refused (httpapi.ErrPlanCut) -- keeping Request changes and Reject, and
// that the context line gives the cut's reason in place of "Awaiting
// approval". A whole plan's message is unchanged: three buttons, Approve
// first, and the awaiting line.
func TestPlanApprovalMessage_CutPlan_OffersNoApprove(t *testing.T) {
	t.Parallel()

	cut := framecut.Cut{Kept: 4096, Total: 40960}
	tests := []struct {
		name        string
		cut         *framecut.Cut
		wantActions []string
		wantContext string
	}{
		{
			name:        "a cut plan",
			cut:         &cut,
			wantActions: []string{slackapi.ActionRequestChangesPlan, slackapi.ActionRejectPlan},
			wantContext: framecut.Reason(&cut),
		},
		{
			name:        "a cut the server could not read",
			cut:         &framecut.Malformed,
			wantActions: []string{slackapi.ActionRequestChangesPlan, slackapi.ActionRejectPlan},
			wantContext: framecut.Reason(&framecut.Malformed),
		},
		{
			name:        "a whole plan",
			wantActions: []string{slackapi.ActionApprovePlan, slackapi.ActionRequestChangesPlan, slackapi.ActionRejectPlan},
			wantContext: "Awaiting approval — first verdict wins, across Slack/Linear/web.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var gotBody map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C999", "ts": "1700000000.000100"})
			}))
			defer server.Close()

			client := slackapi.New(server.Client(), server.URL, "xoxb-test-token")
			if _, _, err := client.PostPlanApprovalMessage(context.Background(), slackapi.PlanApprovalPayload{
				PlanID: "plan-1", SessionID: "session-1", ChannelID: "C123", ThreadTS: "1234.5678", Version: 1,
				Text: "1. Add the migration\n[text cut at 20 of 40960 bytes on its way from the sandbox]",
				Cut:  tt.cut,
			}); err != nil {
				t.Fatalf("PostPlanApprovalMessage() error = %v", err)
			}

			blocks, _ := gotBody["blocks"].([]any)
			var gotActions []string
			var gotContext []string
			for _, b := range blocks {
				block, _ := b.(map[string]any)
				switch block["type"] {
				case "actions":
					elements, _ := block["elements"].([]any)
					for _, el := range elements {
						elem, _ := el.(map[string]any)
						actionID, _ := elem["action_id"].(string)
						gotActions = append(gotActions, actionID)
					}
				case "context":
					elements, _ := block["elements"].([]any)
					for _, el := range elements {
						elem, _ := el.(map[string]any)
						text, _ := elem["text"].(string)
						gotContext = append(gotContext, text)
					}
				}
			}
			if strings.Join(gotActions, ",") != strings.Join(tt.wantActions, ",") {
				t.Errorf("actions = %v, want %v", gotActions, tt.wantActions)
			}
			if len(gotContext) != 1 || gotContext[0] != tt.wantContext {
				t.Errorf("context = %q, want [%q]", gotContext, tt.wantContext)
			}
		})
	}
}

// TestPlanApprovalPayload_WholePlanEncodesNoCut pins that a whole plan's
// outbox payload is byte-identical to one enqueued before cuts existed --
// no "cut" key -- and that a cut plan's carries it.
func TestPlanApprovalPayload_WholePlanEncodesNoCut(t *testing.T) {
	t.Parallel()

	whole, err := json.Marshal(slackapi.PlanApprovalPayload{PlanID: "p", SessionID: "s", ChannelID: "C", ThreadTS: "1.1", Version: 1, Text: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"plan_id":"p","session_id":"s","channel_id":"C","thread_ts":"1.1","version":1,"text":"t"}`; string(whole) != want {
		t.Errorf("whole plan payload = %s, want %s", whole, want)
	}
	cut, err := json.Marshal(slackapi.PlanApprovalPayload{PlanID: "p", Cut: &framecut.Cut{Kept: 1, Total: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cut), `"cut":{"kept":1,"total":2}`) {
		t.Errorf("cut plan payload = %s, want its cut", cut)
	}
}

// assumedMaxRawTextRunes mirrors blockkit.go's own unexported
// maxRawTextRunes constant -- kept here only so this test can position a
// Markdown construct precisely relative to the truncation cutoff. If
// blockkit.go's own constant ever changes, update this to match (the
// assertions below are otherwise generic and would still catch a
// real regression either way).
const assumedMaxRawTextRunes = 550

// TestPostPlanApprovalMessage_TruncationHappensOnRawMarkdown proves the
// audit-fix batch's "truncation tag-boundary safety" finding (LOW):
// truncateForSection now cuts payload.Text's RAW markdown BEFORE
// MarkdownToMrkdwn ever runs, not the already-converted mrkdwn -- so (a) a
// long plan whose raw Markdown link straddles the truncation cutoff never
// leaves a dangling, unterminated Slack "<url|label" tag fragment in what
// is actually posted, and (b) even the mathematical worst case for
// post-conversion growth (raw text saturated with literal "&" characters,
// which each expand to the 5-rune "&amp;" entity) still lands the FINAL
// converted text comfortably under Slack's own real 3000-character
// section-text limit.
func TestPostPlanApprovalMessage_TruncationHappensOnRawMarkdown(t *testing.T) {
	t.Parallel()

	// Position a valid link (which SHOULD survive intact) followed by a
	// second link engineered to straddle the truncation cutoff -- filler
	// sized so the cut lands 5 runes into the second link's own "[Click
	// here](...)" markdown, well before either its "]" or its closing ")".
	firstLink := "[Read more](https://example.com/full-link) "
	secondLink := "[Click here](https://example.com/dashboard/very/long/path)"
	fillerLen := assumedMaxRawTextRunes - len([]rune(firstLink)) - 5
	if fillerLen < 0 {
		t.Fatalf("test setup: assumedMaxRawTextRunes too small for firstLink (%d runes)", len([]rune(firstLink)))
	}

	tests := []struct {
		name string
		text string
	}{
		{
			name: "a raw Markdown link straddling the truncation cutoff never leaves a dangling tag, while an earlier complete link still converts normally",
			text: firstLink + strings.Repeat("x", fillerLen) + secondLink,
		},
		{
			name: "worst-case expansion (raw text saturated with literal & characters) still fits Slack's real section-text limit",
			text: strings.Repeat("&", 5000),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var gotBody map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
					t.Fatalf("decode request body: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C999", "ts": "1700000000.000100"})
			}))
			defer server.Close()

			client := slackapi.New(server.Client(), server.URL, "xoxb-test-token")
			_, _, err := client.PostPlanApprovalMessage(context.Background(), slackapi.PlanApprovalPayload{
				ChannelID: "C123",
				ThreadTS:  "1.1",
				Version:   1,
				Text:      tc.text,
			})
			if err != nil {
				t.Fatalf("PostPlanApprovalMessage() error = %v, want nil", err)
			}

			blocks, ok := gotBody["blocks"].([]any)
			if !ok || len(blocks) < 2 {
				t.Fatalf("blocks = %v, want at least 2 blocks", gotBody["blocks"])
			}
			contentBlock, ok := blocks[1].(map[string]any)
			if !ok {
				t.Fatalf("blocks[1] = %v, not an object", blocks[1])
			}
			textObj, ok := contentBlock["text"].(map[string]any)
			if !ok {
				t.Fatalf("blocks[1].text = %v, not an object", contentBlock["text"])
			}
			gotText, _ := textObj["text"].(string)

			// (b) Slack's own REAL section text-object limit is 3000
			// characters -- the final, actually-posted converted+truncated
			// text must comfortably fit under it regardless of how much the
			// raw input happened to expand during conversion.
			const slackRealSectionLimit = 3000
			if n := len([]rune(gotText)); n > slackRealSectionLimit {
				t.Errorf("final section text = %d runes, want <= %d (Slack's own real limit)", n, slackRealSectionLimit)
			}

			// (a) No dangling, unterminated Slack tag fragment: every "<" in
			// the final text must be part of a complete "<target|label>"
			// tag -- remove every well-formed tag and confirm nothing
			// shaped like a broken one remains.
			tagPattern := regexp.MustCompile(`<[^<>]*\|[^<>]*>`)
			remaining := tagPattern.ReplaceAllString(gotText, "")
			if strings.ContainsAny(remaining, "<>") {
				t.Errorf("final section text contains a dangling/unterminated tag fragment: %q", gotText)
			}
		})
	}

	// Sanity-check the straddling scenario actually exercises what it
	// claims to: the earlier, complete link must still have converted into
	// a real, well-formed Slack tag (proving truncation didn't just delete
	// it outright), and the straddled second link must NOT appear as any
	// kind of tag at all (complete or dangling).
	t.Run("earlier complete link converts normally alongside the straddled one", func(t *testing.T) {
		t.Parallel()

		var gotBody map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C999", "ts": "1700000000.000100"})
		}))
		defer server.Close()

		client := slackapi.New(server.Client(), server.URL, "xoxb-test-token")
		_, _, err := client.PostPlanApprovalMessage(context.Background(), slackapi.PlanApprovalPayload{
			ChannelID: "C123",
			ThreadTS:  "1.1",
			Version:   1,
			Text:      firstLink + strings.Repeat("x", fillerLen) + secondLink,
		})
		if err != nil {
			t.Fatalf("PostPlanApprovalMessage() error = %v, want nil", err)
		}
		blocks, ok := gotBody["blocks"].([]any)
		if !ok || len(blocks) < 2 {
			t.Fatalf("blocks = %v, want at least 2 blocks", gotBody["blocks"])
		}
		contentBlock, ok := blocks[1].(map[string]any)
		if !ok {
			t.Fatalf("blocks[1] = %v, not an object", blocks[1])
		}
		textObj, ok := contentBlock["text"].(map[string]any)
		if !ok {
			t.Fatalf("blocks[1].text = %v, not an object", contentBlock["text"])
		}
		gotText, _ := textObj["text"].(string)

		if !strings.Contains(gotText, "<https://example.com/full-link|Read more>") {
			t.Errorf("final section text = %q, want it to still contain the earlier complete link's converted tag", gotText)
		}
		if strings.Contains(gotText, "dashboard") {
			t.Errorf("final section text = %q, want the straddled second link's target never to appear at all (cut well before it)", gotText)
		}
	})
}

// TestUpdateMessage_Success proves chat.update's own request shape
// (channel/ts/text, deliberately NO blocks -- see UpdateMessage's own doc
// comment for why omitting blocks is what clears the buttons).
func TestUpdateMessage_Success(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat.update" {
			t.Errorf("request path = %q, want %q", r.URL.Path, "/chat.update")
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer server.Close()

	client := slackapi.New(server.Client(), server.URL, "xoxb-test-token")
	if err := client.UpdateMessage(context.Background(), "C123", "1234.5678", "Approved — implementation started."); err != nil {
		t.Fatalf("UpdateMessage() error = %v, want nil", err)
	}
	if gotBody["channel"] != "C123" || gotBody["ts"] != "1234.5678" {
		t.Errorf("(channel, ts) = (%v, %v), want (%q, %q)", gotBody["channel"], gotBody["ts"], "C123", "1234.5678")
	}
	if _, hasBlocks := gotBody["blocks"]; hasBlocks {
		t.Errorf("request body carries a blocks field, want none (omitting it is what clears the buttons)")
	}
}

// TestOpenView_Success proves views.open's own real request shape:
// trigger_id at the top level, and the view's own callback_id/
// private_metadata/single input block.
func TestOpenView_Success(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/views.open" {
			t.Errorf("request path = %q, want %q", r.URL.Path, "/views.open")
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer server.Close()

	client := slackapi.New(server.Client(), server.URL, "xoxb-test-token")
	if err := client.OpenView(context.Background(), "trigger-123", "plan-1", "session-1"); err != nil {
		t.Fatalf("OpenView() error = %v, want nil", err)
	}

	if gotBody["trigger_id"] != "trigger-123" {
		t.Errorf("trigger_id = %v, want %q", gotBody["trigger_id"], "trigger-123")
	}
	view, ok := gotBody["view"].(map[string]any)
	if !ok {
		t.Fatalf("view = %v, want an object", gotBody["view"])
	}
	if view["callback_id"] != slackapi.RequestChangesCallbackID {
		t.Errorf("view.callback_id = %v, want %q", view["callback_id"], slackapi.RequestChangesCallbackID)
	}
	wantMetadata := slackapi.EncodePlanActionValue("plan-1", "session-1")
	if view["private_metadata"] != wantMetadata {
		t.Errorf("view.private_metadata = %v, want %q", view["private_metadata"], wantMetadata)
	}
	blocks, ok := view["blocks"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("view.blocks = %v, want exactly 1 input block", view["blocks"])
	}
	inputBlock, ok := blocks[0].(map[string]any)
	if !ok || inputBlock["block_id"] != slackapi.RequestChangesBlockID {
		t.Fatalf("input block = %v, want block_id %q", blocks[0], slackapi.RequestChangesBlockID)
	}
	element, ok := inputBlock["element"].(map[string]any)
	if !ok || element["action_id"] != slackapi.RequestChangesActionID {
		t.Fatalf("input element = %v, want action_id %q", inputBlock["element"], slackapi.RequestChangesActionID)
	}
}
