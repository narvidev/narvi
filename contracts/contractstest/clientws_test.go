package contractstest

import (
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/clientws"
)

// client-ws/v1/protocol.schema.json deliberately has no top-level oneOf
// (§6.2: "these 4 shapes are independent named payloads, not a discriminated
// union"), so each payload is validated against its own $defs entry.
const clientWSSchemaPath = "client-ws/v1/protocol.schema.json"

func TestSubscribeRequestRoundTrip(t *testing.T) {
	sch := compileSchema(t, clientWSSchemaPath, "#/$defs/SubscribeRequest")

	roundTrip(t, sch, clientws.SubscribeRequest{
		Token:    "ws-token-abc",
		ClientId: "client-1",
	})
}

func TestSubscribedPayloadRoundTrip(t *testing.T) {
	sch := compileSchema(t, clientWSSchemaPath, "#/$defs/SubscribedPayload")

	roundTrip(t, sch, clientws.SubscribedPayload{
		SessionId: testSessionID,
		State:     clientws.SubscribedPayloadState{"status": "active"},
		Events: []clientws.SubscribedPayloadEventsElem{
			{"type": "token", "text": "hello"},
		},
		Artifacts: []clientws.SubscribedPayloadArtifactsElem{
			{"artifactType": "pr", "url": "https://github.com/narvidev/narvi/pull/1"},
		},
		Participants: []clientws.SubscribedPayloadParticipantsElem{
			{"clientId": "client-1", "userId": "user-1"},
		},
	})
}

func TestFetchHistoryRequestRoundTrip(t *testing.T) {
	sch := compileSchema(t, clientWSSchemaPath, "#/$defs/FetchHistoryRequest")

	t.Run("WithCursorAndLimit", func(t *testing.T) {
		cursor := "cursor-abc"
		limit := 50
		roundTrip(t, sch, clientws.FetchHistoryRequest{
			SessionId: testSessionID,
			Cursor:    &cursor,
			Limit:     &limit,
		})
	})

	t.Run("NullCursorAndLimit", func(t *testing.T) {
		// null cursor means "start from the beginning/most recent"; null
		// limit means "use the server default page size" (§6.2).
		roundTrip(t, sch, clientws.FetchHistoryRequest{
			SessionId: testSessionID,
			Cursor:    nil,
			Limit:     nil,
		})
	})
}

func TestFetchHistoryResponseRoundTrip(t *testing.T) {
	sch := compileSchema(t, clientWSSchemaPath, "#/$defs/FetchHistoryResponse")

	t.Run("WithNextCursor", func(t *testing.T) {
		nextCursor := "cursor-def"
		roundTrip(t, sch, clientws.FetchHistoryResponse{
			Events: []clientws.FetchHistoryResponseEventsElem{
				{"type": "token", "text": "hello"},
			},
			NextCursor: &nextCursor,
		})
	})

	t.Run("NoMorePages", func(t *testing.T) {
		roundTrip(t, sch, clientws.FetchHistoryResponse{
			Events:     []clientws.FetchHistoryResponseEventsElem{},
			NextCursor: nil,
		})
	})

	// WithSandbox: every reply carries the sandbox as the control plane
	// holds it then, in state.sandbox's shape, so an open page learns a
	// status the server derived (technical plan §3.2) without resubscribing.
	t.Run("WithSandbox", func(t *testing.T) {
		roundTrip(t, sch, clientws.FetchHistoryResponse{
			Events: []clientws.FetchHistoryResponseEventsElem{
				// Numbers are float64, as encoding/json decodes them into an
				// interface{}, so the round trip compares equal.
				{"id": float64(7), "type": "sandbox_status", "payload": map[string]interface{}{"sandbox": map[string]interface{}{"gen": float64(2), "status": "ready"}}},
			},
			NextCursor: nil,
			Sandbox:    &clientws.FetchHistoryResponseSandbox{"gen": float64(2), "status": "ready", "lastSeenAt": nil},
		})
	})
}

// TestFetchHistoryResponseRequiresSandbox: the reply names the sandbox on
// every page, null included -- a reply without the key is not this
// contract's.
func TestFetchHistoryResponseRequiresSandbox(t *testing.T) {
	sch := compileSchema(t, clientWSSchemaPath, "#/$defs/FetchHistoryResponse")

	tests := []struct {
		name    string
		doc     string
		wantErr bool
	}{
		{name: "null sandbox", doc: `{"events":[],"nextCursor":null,"sandbox":null}`, wantErr: false},
		{name: "a sandbox", doc: `{"events":[],"nextCursor":null,"sandbox":{"gen":1,"status":"booting"}}`, wantErr: false},
		{name: "no sandbox key", doc: `{"events":[],"nextCursor":null}`, wantErr: true},
		{name: "a sandbox that is not an object", doc: `{"events":[],"nextCursor":null,"sandbox":"booting"}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateJSON(t, sch, []byte(tc.doc))
			if (err != nil) != tc.wantErr {
				t.Errorf("validate %s: err = %v, want error %v", tc.doc, err, tc.wantErr)
			}
		})
	}
}
