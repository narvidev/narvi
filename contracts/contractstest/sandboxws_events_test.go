package contractstest

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

var testTimestamp = time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

func TestSandboxEventsRoundTrip(t *testing.T) {
	sch := compileSchema(t, "sandbox-ws/v1/events.schema.json", "")

	t.Run("Ready", func(t *testing.T) {
		// AgentVersion/ImageDigest (§12.2 item 1's own runtime-fingerprint
		// gap) are given real, non-empty sample values deliberately -- a
		// zero-value "" would round-trip trivially (Go's own string zero
		// value marshals as a present, schema-valid empty string) without
		// ever proving a REAL value survives the marshal/validate/unmarshal
		// cycle intact.
		roundTrip(t, sch, sandboxws.Ready{
			Type:         "ready",
			MessageId:    "e1",
			SessionId:    testSessionID,
			Gen:          1,
			Timestamp:    testTimestamp,
			AgentVersion: "v1.4.2",
			ImageDigest:  "sha256:9f31c00abcdef",
		})
	})

	t.Run("Ready_Capabilities", func(t *testing.T) {
		// Technical plan §3.3, prompt receipts: capabilities is optional, and
		// so is each capability inside it; an agent that predates it sends
		// neither (Ready above).
		promptReceipt := true
		roundTrip(t, sch, sandboxws.Ready{
			Type:         "ready",
			MessageId:    "e1c",
			SessionId:    testSessionID,
			Gen:          1,
			Timestamp:    testTimestamp,
			AgentVersion: "v1.4.2",
			ImageDigest:  "sha256:9f31c00abcdef",
			Capabilities: &sandboxws.ReadyCapabilities{PromptReceipt: &promptReceipt},
		})
	})

	t.Run("Ready_Capabilities_MaxFrameBytes", func(t *testing.T) {
		// Technical plan §3.3, §6.1: the agent's read limit, stated on every
		// ready, with or without promptReceipt.
		maxFrameBytes := 32 << 20
		roundTrip(t, sch, sandboxws.Ready{
			Type:         "ready",
			MessageId:    "e1d",
			SessionId:    testSessionID,
			Gen:          1,
			Timestamp:    testTimestamp,
			AgentVersion: "v1.4.2",
			ImageDigest:  "sha256:9f31c00abcdef",
			Capabilities: &sandboxws.ReadyCapabilities{MaxFrameBytes: &maxFrameBytes},
		})
	})

	// A stated read limit is a positive byte count: the schema and the
	// generated decoder alike refuse zero or less, and the session actor
	// then reads the ready as stating nothing.
	t.Run("Ready_Capabilities_MaxFrameBytesBelowOneRejected", func(t *testing.T) {
		for _, value := range []string{"0", "-1"} {
			payload := []byte(`{"type":"ready","messageId":"e1e","sessionId":"` + testSessionID +
				`","gen":1,"timestamp":"2026-07-16T12:00:00Z","agentVersion":"v1","imageDigest":"d","capabilities":{"maxFrameBytes":` + value + `}}`)
			if err := validateJSON(t, sch, payload); err == nil {
				t.Fatalf("expected maxFrameBytes %s to fail validation, got nil error", value)
			}
			var event sandboxws.Ready
			if err := json.Unmarshal(payload, &event); err == nil {
				t.Fatalf("expected maxFrameBytes %s to fail Go unmarshal, got nil error", value)
			}
		}
	})

	t.Run("Ready_Capabilities_ReviewCheckout", func(t *testing.T) {
		// Technical plan §21.1, §30.4: an agent that runs the checkout
		// command says so on every ready, beside its other capabilities.
		reviewCheckout := true
		maxFrameBytes := 32 << 20
		roundTrip(t, sch, sandboxws.Ready{
			Type:         "ready",
			MessageId:    "e1f",
			SessionId:    testSessionID,
			Gen:          1,
			Timestamp:    testTimestamp,
			AgentVersion: "v1.4.2",
			ImageDigest:  "sha256:9f31c00abcdef",
			Capabilities: &sandboxws.ReadyCapabilities{MaxFrameBytes: &maxFrameBytes, ReviewCheckout: &reviewCheckout},
		})
	})

	t.Run("CheckoutResult", func(t *testing.T) {
		head := "0123456789abcdef0123456789abcdef01234567"
		tip := "89abcdef0123456789abcdef0123456789abcdef"
		failure := "fatal: could not read from remote repository"
		roundTrip(t, sch, sandboxws.CheckoutResult{
			Type:             "checkout_result",
			MessageId:        "checkout_result:m9",
			SessionId:        testSessionID,
			Gen:              1,
			CommandMessageId: "m9",
			Repos: []sandboxws.CheckoutResultReposElem{
				{Name: "widgets", Outcome: sandboxws.CheckoutResultReposElemOutcomeCheckedOut, HeadSha: &head, RefSha: &tip, Error: nil},
				{Name: "docs", Outcome: sandboxws.CheckoutResultReposElemOutcomeFetchFailed, HeadSha: nil, RefSha: nil, Error: &failure},
			},
		})
	})

	// checkout_result's messageId is deterministic: the control plane finds
	// a command's result by 'checkout_result:{commandMessageId}', so a result
	// under any other key is refused by the schema and the generated decoder
	// alike.
	t.Run("CheckoutResult_MessageIdWithoutPrefixRejected", func(t *testing.T) {
		payload := []byte(`{"type":"checkout_result","messageId":"m9","sessionId":"` + testSessionID +
			`","gen":1,"commandMessageId":"m9","repos":[{"name":"widgets","outcome":"busy","headSha":null,"refSha":null,"error":null}]}`)
		if err := validateJSON(t, sch, payload); err == nil {
			t.Fatal("expected a checkout_result messageId without its prefix to fail validation, got nil error")
		}
		var event sandboxws.CheckoutResult
		if err := json.Unmarshal(payload, &event); err == nil {
			t.Fatal("expected a checkout_result messageId without its prefix to fail Go unmarshal, got nil error")
		}
	})

	// outcome is an open enum for the compatibility grade (contracts/
	// manifest.json): a later value grades MINOR, and a reader treats an
	// unknown one as failed. This release's schema still knows exactly five.
	t.Run("CheckoutResult_UnknownOutcomeRejectedByThisRelease", func(t *testing.T) {
		payload := []byte(`{"type":"checkout_result","messageId":"checkout_result:m9","sessionId":"` + testSessionID +
			`","gen":1,"commandMessageId":"m9","repos":[{"name":"widgets","outcome":"moved","headSha":null,"refSha":null,"error":null}]}`)
		if err := validateJSON(t, sch, payload); err == nil {
			t.Fatal("expected an outcome outside this release's enum to fail validation, got nil error")
		}
	})

	t.Run("PromptReceived", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.PromptReceived{
			Type:            "prompt_received",
			MessageId:       "prompt_received:m1",
			SessionId:       testSessionID,
			Gen:             1,
			PromptMessageId: "m1",
			Duplicate:       true,
		})
	})

	// prompt_received's messageId is deterministic (technical plan §3.3,
	// prompt receipts): the control plane finds a prompt's receipt by
	// 'prompt_received:{promptMessageId}', so a receipt under any other key
	// is refused by the schema and by the generated decoder alike.
	t.Run("PromptReceived_MessageIdWithoutPrefixRejected", func(t *testing.T) {
		payload := []byte(`{"type":"prompt_received","messageId":"m1","sessionId":"` + testSessionID +
			`","gen":1,"promptMessageId":"m1","duplicate":false}`)
		if err := validateJSON(t, sch, payload); err == nil {
			t.Fatal("expected a prompt_received messageId without its prefix to fail validation, got nil error")
		}
		var event sandboxws.PromptReceived
		if err := json.Unmarshal(payload, &event); err == nil {
			t.Fatal("expected a prompt_received messageId without its prefix to fail Go unmarshal, got nil error")
		}
	})

	t.Run("Heartbeat", func(t *testing.T) {
		conversationID := "conv-123"
		bootPhase := "installing_deps"
		roundTrip(t, sch, sandboxws.Heartbeat{
			Type:           "heartbeat",
			MessageId:      "e2",
			SessionId:      testSessionID,
			Gen:            1,
			ConversationId: &conversationID,
			LastBootPhase:  &bootPhase,
			Timestamp:      testTimestamp,
		})
	})

	// Technical plan §35.2: the whole seconds the sandbox's provider stated
	// it will still let the sandbox run, on a ready and on a heartbeat.
	// Optional: an agent whose provider stated nothing sends neither
	// (Ready and Heartbeat above). Zero is a value, never an absence.
	t.Run("LifetimeRemainingSeconds", func(t *testing.T) {
		for _, remaining := range []int{5400, 0} {
			roundTrip(t, sch, sandboxws.Ready{
				Type:                     "ready",
				MessageId:                "e1l",
				SessionId:                testSessionID,
				Gen:                      1,
				Timestamp:                testTimestamp,
				AgentVersion:             "v1.4.2",
				ImageDigest:              "sha256:9f31c00abcdef",
				LifetimeRemainingSeconds: &remaining,
			})
			conversationID := "conv-123"
			roundTrip(t, sch, sandboxws.Heartbeat{
				Type:                     "heartbeat",
				MessageId:                "e2l",
				SessionId:                testSessionID,
				Gen:                      1,
				ConversationId:           &conversationID,
				LastBootPhase:            nil,
				Timestamp:                testTimestamp,
				LifetimeRemainingSeconds: &remaining,
			})
		}
		zero := 0
		data, err := json.Marshal(sandboxws.Heartbeat{Type: "heartbeat", MessageId: "e2z", SessionId: testSessionID, Gen: 1, Timestamp: testTimestamp, LifetimeRemainingSeconds: &zero})
		if err != nil || !bytes.Contains(data, []byte(`"lifetimeRemainingSeconds":0`)) {
			t.Fatalf("a heartbeat stating zero seconds marshals as %s (%v); want the key present with 0", data, err)
		}
	})

	// No minimum, deliberately: a negative value from a misbehaving agent
	// validates, and decodes through the generated types, which would
	// enforce a minimum; readers clamp it. (The control plane's own readers
	// never decode the key through the generated types:
	// internal/app/sessionactor/framekey.go.)
	t.Run("LifetimeRemainingSeconds_NegativeStillDecodes", func(t *testing.T) {
		ready := []byte(`{"type":"ready","messageId":"e1n","sessionId":"` + testSessionID +
			`","gen":1,"timestamp":"2026-07-16T12:00:00Z","agentVersion":"v1","imageDigest":"d","lifetimeRemainingSeconds":-5}`)
		heartbeat := []byte(`{"type":"heartbeat","messageId":"e2n","sessionId":"` + testSessionID +
			`","gen":1,"conversationId":null,"lastBootPhase":null,"timestamp":"2026-07-16T12:00:00Z","lifetimeRemainingSeconds":-5}`)
		for _, payload := range [][]byte{ready, heartbeat} {
			if err := validateJSON(t, sch, payload); err != nil {
				t.Fatalf("a negative lifetimeRemainingSeconds fails validation: %v\npayload: %s", err, payload)
			}
		}
		var readyEvent sandboxws.Ready
		if err := json.Unmarshal(ready, &readyEvent); err != nil || readyEvent.LifetimeRemainingSeconds == nil || *readyEvent.LifetimeRemainingSeconds != -5 {
			t.Fatalf("a ready with a negative lifetimeRemainingSeconds decodes to %v (%v); want -5", readyEvent.LifetimeRemainingSeconds, err)
		}
		var heartbeatEvent sandboxws.Heartbeat
		if err := json.Unmarshal(heartbeat, &heartbeatEvent); err != nil || heartbeatEvent.LifetimeRemainingSeconds == nil || *heartbeatEvent.LifetimeRemainingSeconds != -5 {
			t.Fatalf("a heartbeat with a negative lifetimeRemainingSeconds decodes to %v (%v); want -5", heartbeatEvent.LifetimeRemainingSeconds, err)
		}
	})

	t.Run("Heartbeat_BeforeFirstTurn", func(t *testing.T) {
		// Both conversationId and lastBootPhase are REQUIRED keys whose value
		// may be null (§6.1 nullability convention) -- this is not the
		// omission-means-fresh case (that's unique to Prompt.conversationId).
		roundTrip(t, sch, sandboxws.Heartbeat{
			Type:           "heartbeat",
			MessageId:      "e2b",
			SessionId:      testSessionID,
			Gen:            1,
			ConversationId: nil,
			LastBootPhase:  nil,
			Timestamp:      testTimestamp,
		})
	})

	t.Run("BootProgress", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.BootProgress{
			Type:      "boot_progress",
			MessageId: "e3",
			SessionId: testSessionID,
			Gen:       1,
			Phase:     "cloning_repos",
			Timestamp: testTimestamp,
		})
	})

	t.Run("Token", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.Token{
			Type:      "token",
			MessageId: "e4",
			SessionId: testSessionID,
			Gen:       1,
			Text:      "cumulative text so far",
		})
	})

	t.Run("ToolCall", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.ToolCall{
			Type:      "tool_call",
			MessageId: "e5",
			SessionId: testSessionID,
			Gen:       1,
			CallId:    "call-1",
			ToolName:  "read_file",
			Input:     sandboxws.ToolCallInput{"path": "main.go"},
		})
	})

	t.Run("ToolResult", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.ToolResult{
			Type:      "tool_result",
			MessageId: "e6",
			SessionId: testSessionID,
			Gen:       1,
			CallId:    "call-1",
			Output:    sandboxws.ToolResultOutput{"contents": "package main"},
			IsError:   false,
		})
	})

	t.Run("StepStart", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.StepStart{
			Type:      "step_start",
			MessageId: "e7",
			SessionId: testSessionID,
			Gen:       1,
			StepId:    "step-1",
		})
	})

	t.Run("StepFinish_FullCost", func(t *testing.T) {
		cached := 12
		usd := 0.0042
		roundTrip(t, sch, sandboxws.StepFinish{
			Type:      "step_finish",
			MessageId: "e8",
			SessionId: testSessionID,
			Gen:       1,
			StepId:    "step-1",
			Cost: sandboxws.StepFinishCost{
				Tokens: sandboxws.StepFinishCostTokens{
					Input:  1000,
					Output: 250,
					Cached: &cached,
				},
				Usd: &usd,
			},
		})
	})

	t.Run("StepFinish_MinimalCost", func(t *testing.T) {
		// cached and usd are optional; only tokens.input/tokens.output are
		// required (§6.1 nullability convention: this is the documented
		// exception, not the usual "required key, nullable value" shape).
		roundTrip(t, sch, sandboxws.StepFinish{
			Type:      "step_finish",
			MessageId: "e8b",
			SessionId: testSessionID,
			Gen:       1,
			StepId:    "step-1",
			Cost: sandboxws.StepFinishCost{
				Tokens: sandboxws.StepFinishCostTokens{
					Input:  10,
					Output: 5,
				},
			},
		})
	})

	t.Run("GitSync", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.GitSync{
			Type:      "git_sync",
			MessageId: "e9",
			SessionId: testSessionID,
			Gen:       1,
			Repo:      "narvi",
			Status:    sandboxws.GitSyncStatusCheckout,
			Branch:    "session/abc",
		})
	})

	// TestSandboxEventsGitSyncRepoRequired is this batch's own schema change
	// ("gitstate in-sandbox", §3.4/§14.1 design section 6): GitSync
	// now REQUIRES a "repo" field disambiguating which of a session's
	// (always-a-list, §3.4) repos a given stash/checkout/pop phase is
	// about. Omitting it must fail validation.
	t.Run("GitSync_MissingRepoRejected", func(t *testing.T) {
		payload := []byte(`{"type":"git_sync","messageId":"e9b","sessionId":"` + testSessionID +
			`","gen":1,"status":"checkout","branch":"session/abc"}`)
		if err := validateJSON(t, sch, payload); err == nil {
			t.Fatal("expected missing repo on git_sync to fail validation, got nil error")
		}
	})

	t.Run("Artifact", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.Artifact{
			Type:         "artifact",
			MessageId:    "e10",
			SessionId:    testSessionID,
			Gen:          1,
			ArtifactType: sandboxws.ArtifactArtifactTypePr,
			Url:          "https://github.com/narvidev/narvi/pull/42",
			Metadata:     sandboxws.ArtifactMetadata{"number": float64(42)},
		})
	})

	t.Run("ExecutionComplete", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.ExecutionComplete{
			Type:      "execution_complete",
			MessageId: "e11",
			SessionId: testSessionID,
			Gen:       1,
			AckId:     "execution_complete:e11",
			Outcome:   sandboxws.ExecutionCompleteOutcomeCompleted,
			Reason:    nil,
		})
	})

	t.Run("PushComplete", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.PushComplete{
			Type:      "push_complete",
			MessageId: "e12",
			SessionId: testSessionID,
			Gen:       1,
			AckId:     "push_complete:e12",
			Repos: []sandboxws.PushCompleteReposElem{
				{Name: "narvi", Branch: "session/abc", Sha: "deadbeef"},
			},
		})
	})

	t.Run("PushError", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.PushError{
			Type:      "push_error",
			MessageId: "e13",
			SessionId: testSessionID,
			Gen:       1,
			AckId:     "push_error:e13",
			Error:     "remote rejected non-fast-forward",
		})
	})

	t.Run("SessionTitle", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.SessionTitle{
			Type:      "session_title",
			MessageId: "e14",
			SessionId: testSessionID,
			Gen:       1,
			Title:     "Fix the failing test",
		})
	})

	t.Run("Warning", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.Warning{
			Type:      "warning",
			MessageId: "e15",
			SessionId: testSessionID,
			Gen:       1,
			Message:   "model catalog fallback in use",
		})
	})

	t.Run("Error", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.SandboxErrorEvent{
			Type:      "error",
			MessageId: "e16",
			SessionId: testSessionID,
			Gen:       1,
			AckId:     "error:e16",
			Message:   "opencode server crashed",
			Fatal:     true,
		})
	})

	t.Run("SnapshotReady", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.SnapshotReady{
			Type:       "snapshot_ready",
			MessageId:  "e17",
			SessionId:  testSessionID,
			Gen:        1,
			AckId:      "snapshot_ready:e17",
			SnapshotId: "snap-1",
		})
	})

	// Technical plan §35.5b: what the snapshotted sandbox holds, as its own
	// sandbox-agent reports it. Optional, and so is each member: an agent
	// that predates it sends none (SnapshotReady above), and one that
	// discovered no runtime version sends it null or not at all.
	t.Run("SnapshotReady_Provenance", func(t *testing.T) {
		protocol := "1.25.0"
		runtime := "1.14.19"
		for _, provenance := range []*sandboxws.SnapshotReadyProvenance{
			{AgentProtocol: &protocol, RuntimeVersion: &runtime},
			{AgentProtocol: &protocol, RuntimeVersion: nil},
			{},
		} {
			command := "m17"
			roundTrip(t, sch, sandboxws.SnapshotReady{
				Type:             "snapshot_ready",
				MessageId:        "e17p",
				SessionId:        testSessionID,
				Gen:              1,
				AckId:            "snapshot_ready:e17p",
				SnapshotId:       "snap-1",
				CommandMessageId: &command,
				Provenance:       provenance,
			})
		}
		payload := []byte(`{"type":"snapshot_ready","messageId":"e17q","sessionId":"` + testSessionID +
			`","gen":1,"ackId":"snapshot_ready:e17q","snapshotId":"snap-1","provenance":{"agentProtocol":"1.25.0","runtimeVersion":null}}`)
		if err := validateJSON(t, sch, payload); err != nil {
			t.Fatalf("an explicit null runtimeVersion fails validation: %v", err)
		}
		var event sandboxws.SnapshotReady
		if err := json.Unmarshal(payload, &event); err != nil || event.Provenance == nil || event.Provenance.RuntimeVersion != nil {
			t.Fatalf("an explicit null runtimeVersion decodes to %+v (%v); want a provenance with no runtime version", event.Provenance, err)
		}
	})

	// The provenance object is closed: a member this release does not name
	// is refused by the schema, so a later one is a contract change graded
	// like any other.
	t.Run("SnapshotReady_ProvenanceUnknownMemberRejected", func(t *testing.T) {
		payload := []byte(`{"type":"snapshot_ready","messageId":"e17r","sessionId":"` + testSessionID +
			`","gen":1,"ackId":"snapshot_ready:e17r","snapshotId":"snap-1","provenance":{"agentProtocol":"1.25.0","imageDigest":"sha256:0"}}`)
		if err := validateJSON(t, sch, payload); err == nil {
			t.Fatal("expected a provenance member this release does not name to fail validation, got nil error")
		}
	})

	t.Run("SubTaskStart", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.SubTaskStart{
			Type:            "sub_task_start",
			MessageId:       "e18",
			SessionId:       testSessionID,
			Gen:             1,
			SubTaskId:       "prt_subtask1",
			Label:           "Investigate flaky test",
			ParentMessageId: "msg_parent1",
		})
	})

	t.Run("SubTaskFinish", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.SubTaskFinish{
			Type:      "sub_task_finish",
			MessageId: "e19",
			SessionId: testSessionID,
			Gen:       1,
			AckId:     "sub_task_finish:e19",
			SubTaskId: "prt_subtask1",
			Outcome:   sandboxws.SubTaskFinishOutcomeCompleted,
		})
	})
}

// TestStepFinishCostTokensIsObjectNotNumber is the dedicated regression test
// called out explicitly in §6.1 and §9.1: "tokens is an object, not a
// number -- a number-vs-object mismatch here silently zeroes cost tracking,
// so pin it in the contract test." It asserts both directions: an
// object-shaped tokens payload is valid end to end, and a number-shaped one
// (what a naive/older OpenCode emitter might send) is rejected both by the
// JSON Schema and by the generated Go type.
func TestStepFinishCostTokensIsObjectNotNumber(t *testing.T) {
	sch := compileSchema(t, "sandbox-ws/v1/events.schema.json", "")

	validPayload := []byte(`{
		"type": "step_finish",
		"messageId": "e1",
		"sessionId": "` + testSessionID + `",
		"gen": 1,
		"stepId": "step-1",
		"cost": {"tokens": {"input": 100, "output": 50}}
	}`)

	t.Run("ObjectShapedTokensIsAccepted", func(t *testing.T) {
		if err := validateJSON(t, sch, validPayload); err != nil {
			t.Fatalf("expected object-shaped cost.tokens to validate, got: %v", err)
		}

		var event sandboxws.StepFinish
		if err := json.Unmarshal(validPayload, &event); err != nil {
			t.Fatalf("expected object-shaped cost.tokens to unmarshal, got: %v", err)
		}
		if event.Cost.Tokens.Input != 100 || event.Cost.Tokens.Output != 50 {
			t.Fatalf("unexpected decoded tokens: %#v", event.Cost.Tokens)
		}
	})

	// The regression this test exists to catch: cost.tokens sent as a bare
	// number (e.g. a total-token-count integer) instead of the
	// {input, output, cached?} object §6.1 mandates.
	numberShapedPayload := []byte(`{
		"type": "step_finish",
		"messageId": "e2",
		"sessionId": "` + testSessionID + `",
		"gen": 1,
		"stepId": "step-1",
		"cost": {"tokens": 150}
	}`)

	t.Run("NumberShapedTokensIsRejectedBySchema", func(t *testing.T) {
		if err := validateJSON(t, sch, numberShapedPayload); err == nil {
			t.Fatal("expected number-shaped cost.tokens to fail JSON Schema validation, got nil error")
		}
	})

	t.Run("NumberShapedTokensIsRejectedByGoUnmarshal", func(t *testing.T) {
		var event sandboxws.StepFinish
		if err := json.Unmarshal(numberShapedPayload, &event); err == nil {
			t.Fatal("expected number-shaped cost.tokens to fail Go unmarshal, got nil error")
		}
	})
}

// TestSandboxEventsSubTaskId is the round-trip test for this batch's schema
// change (§6.1/§7.1 "Sub-task fan-out"): the six turn/tool/step-scoped event
// types -- token, tool_call, tool_result, step_start, step_finish,
// execution_complete -- now accept an OPTIONAL, nullable subTaskId. Absent
// (main lane, existing behavior unchanged) and a real string value (a
// sub-task's own lane) must both validate; session/connection-lifecycle
// event types (ready, heartbeat, boot_progress, git_sync, session_title,
// warning, snapshot_ready) are deliberately NOT touched by this change and
// have no such coverage here. sub_task_start/sub_task_finish's own
// subTaskId is a different field with a different meaning (the sub-task's
// own id, not "which sub-task this event happened under") and must stay
// REQUIRED, unchanged -- asserted at the bottom of this test.
func TestSandboxEventsSubTaskId(t *testing.T) {
	sch := compileSchema(t, "sandbox-ws/v1/events.schema.json", "")
	subTaskID := "prt_subtask1"

	t.Run("Token", func(t *testing.T) {
		t.Run("MainLane_Absent", func(t *testing.T) {
			roundTrip(t, sch, sandboxws.Token{
				Type: "token", MessageId: "st1", SessionId: testSessionID, Gen: 1,
				Text: "hello",
			})
		})
		t.Run("SubLane_Populated", func(t *testing.T) {
			roundTrip(t, sch, sandboxws.Token{
				Type: "token", MessageId: "st1b", SessionId: testSessionID, Gen: 1,
				Text: "hello", SubTaskId: sandboxws.TokenSubTaskId(&subTaskID),
			})
		})
	})

	t.Run("ToolCall", func(t *testing.T) {
		t.Run("MainLane_Absent", func(t *testing.T) {
			roundTrip(t, sch, sandboxws.ToolCall{
				Type: "tool_call", MessageId: "st2", SessionId: testSessionID, Gen: 1,
				CallId: "call-1", ToolName: "read_file", Input: sandboxws.ToolCallInput{"path": "main.go"},
			})
		})
		t.Run("SubLane_Populated", func(t *testing.T) {
			roundTrip(t, sch, sandboxws.ToolCall{
				Type: "tool_call", MessageId: "st2b", SessionId: testSessionID, Gen: 1,
				CallId: "call-1", ToolName: "read_file", Input: sandboxws.ToolCallInput{"path": "main.go"},
				SubTaskId: sandboxws.ToolCallSubTaskId(&subTaskID),
			})
		})
	})

	t.Run("ToolResult", func(t *testing.T) {
		t.Run("MainLane_Absent", func(t *testing.T) {
			roundTrip(t, sch, sandboxws.ToolResult{
				Type: "tool_result", MessageId: "st3", SessionId: testSessionID, Gen: 1,
				CallId: "call-1", Output: sandboxws.ToolResultOutput{"contents": "package main"}, IsError: false,
			})
		})
		t.Run("SubLane_Populated", func(t *testing.T) {
			roundTrip(t, sch, sandboxws.ToolResult{
				Type: "tool_result", MessageId: "st3b", SessionId: testSessionID, Gen: 1,
				CallId: "call-1", Output: sandboxws.ToolResultOutput{"contents": "package main"}, IsError: false,
				SubTaskId: sandboxws.ToolResultSubTaskId(&subTaskID),
			})
		})
	})

	t.Run("StepStart", func(t *testing.T) {
		t.Run("MainLane_Absent", func(t *testing.T) {
			roundTrip(t, sch, sandboxws.StepStart{
				Type: "step_start", MessageId: "st4", SessionId: testSessionID, Gen: 1, StepId: "step-1",
			})
		})
		t.Run("SubLane_Populated", func(t *testing.T) {
			roundTrip(t, sch, sandboxws.StepStart{
				Type: "step_start", MessageId: "st4b", SessionId: testSessionID, Gen: 1, StepId: "step-1",
				SubTaskId: sandboxws.StepStartSubTaskId(&subTaskID),
			})
		})
	})

	t.Run("StepFinish", func(t *testing.T) {
		t.Run("MainLane_Absent", func(t *testing.T) {
			roundTrip(t, sch, sandboxws.StepFinish{
				Type: "step_finish", MessageId: "st5", SessionId: testSessionID, Gen: 1, StepId: "step-1",
				Cost: sandboxws.StepFinishCost{Tokens: sandboxws.StepFinishCostTokens{Input: 10, Output: 5}},
			})
		})
		t.Run("SubLane_Populated", func(t *testing.T) {
			roundTrip(t, sch, sandboxws.StepFinish{
				Type: "step_finish", MessageId: "st5b", SessionId: testSessionID, Gen: 1, StepId: "step-1",
				Cost:      sandboxws.StepFinishCost{Tokens: sandboxws.StepFinishCostTokens{Input: 10, Output: 5}},
				SubTaskId: sandboxws.StepFinishSubTaskId(&subTaskID),
			})
		})
	})

	t.Run("ExecutionComplete", func(t *testing.T) {
		t.Run("MainLane_Absent", func(t *testing.T) {
			roundTrip(t, sch, sandboxws.ExecutionComplete{
				Type: "execution_complete", MessageId: "st6", SessionId: testSessionID, Gen: 1,
				AckId: "execution_complete:st6", Outcome: sandboxws.ExecutionCompleteOutcomeCompleted, Reason: nil,
			})
		})
		t.Run("SubLane_Populated", func(t *testing.T) {
			roundTrip(t, sch, sandboxws.ExecutionComplete{
				Type: "execution_complete", MessageId: "st6b", SessionId: testSessionID, Gen: 1,
				AckId: "execution_complete:st6b", Outcome: sandboxws.ExecutionCompleteOutcomeCompleted, Reason: nil,
				SubTaskId: sandboxws.ExecutionCompleteSubTaskId(&subTaskID),
			})
		})
	})

	// Explicit JSON null (distinct from the key being absent entirely, which
	// is what the Go-struct-based roundTrip subtests above exercise via
	// omitempty) must also validate for the nullable subTaskId type.
	t.Run("Token_ExplicitNullSubTaskId", func(t *testing.T) {
		payload := []byte(`{"type":"token","messageId":"st7","sessionId":"` + testSessionID + `","gen":1,"text":"hi","subTaskId":null}`)
		if err := validateJSON(t, sch, payload); err != nil {
			t.Fatalf("expected explicit null subTaskId to validate, got: %v", err)
		}
	})

	// sub_task_start/sub_task_finish's own subTaskId is UNCHANGED by this
	// batch -- still REQUIRED (a different meaning than the six fields
	// above: it's the sub-task's own id, not "which sub-task this event
	// happened under"). Omitting it must still fail validation.
	t.Run("SubTaskStart_MissingSubTaskIdStillRejected", func(t *testing.T) {
		payload := []byte(`{"type":"sub_task_start","messageId":"st8","sessionId":"` + testSessionID + `","gen":1,"label":"x","parentMessageId":"p1"}`)
		if err := validateJSON(t, sch, payload); err == nil {
			t.Fatal("expected missing subTaskId on sub_task_start to fail validation, got nil error")
		}
	})

	t.Run("SubTaskFinish_MissingSubTaskIdStillRejected", func(t *testing.T) {
		payload := []byte(`{"type":"sub_task_finish","messageId":"st9","sessionId":"` + testSessionID + `","gen":1,"ackId":"sub_task_finish:st9","outcome":"completed"}`)
		if err := validateJSON(t, sch, payload); err == nil {
			t.Fatal("expected missing subTaskId on sub_task_finish to fail validation, got nil error")
		}
	})
}

// TestSandboxEventsArtifactStatus is the round-trip test for §8.6's
// ("uploads, blob storage & the in-sandbox download_file tool", §28.6)
// own additive schema change: the artifact event gains an OPTIONAL status
// (absent = "ready") and a nullable failureReason. Absent (every existing
// pr/preview producer, unchanged) and both real values must validate,
// mirroring TestSandboxEventsSubTaskId's own "absent/present" pairing
// immediately above for a different additive field on a different event
// type.
func TestSandboxEventsArtifactStatus(t *testing.T) {
	sch := compileSchema(t, "sandbox-ws/v1/events.schema.json", "")

	t.Run("StatusAbsent_DefaultsToReady", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.Artifact{
			Type:         "artifact",
			MessageId:    "a1",
			SessionId:    testSessionID,
			Gen:          1,
			ArtifactType: sandboxws.ArtifactArtifactTypeUpload,
			Url:          "/api/sessions/" + testSessionID + "/uploads/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/content",
			Metadata:     sandboxws.ArtifactMetadata{},
		})
	})

	t.Run("StatusReady_Explicit", func(t *testing.T) {
		ready := sandboxws.ArtifactStatusReady
		roundTrip(t, sch, sandboxws.Artifact{
			Type:         "artifact",
			MessageId:    "a2",
			SessionId:    testSessionID,
			Gen:          1,
			ArtifactType: sandboxws.ArtifactArtifactTypeUpload,
			Url:          "/api/sessions/" + testSessionID + "/uploads/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/content",
			Metadata:     sandboxws.ArtifactMetadata{},
			Status:       &ready,
		})
	})

	t.Run("StatusFailed_WithFailureReason", func(t *testing.T) {
		failed := sandboxws.ArtifactStatusFailed
		roundTrip(t, sch, sandboxws.Artifact{
			Type:          "artifact",
			MessageId:     "a3",
			SessionId:     testSessionID,
			Gen:           1,
			ArtifactType:  sandboxws.ArtifactArtifactTypeUpload,
			Url:           "/api/sessions/" + testSessionID + "/uploads/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/content",
			Metadata:      sandboxws.ArtifactMetadata{},
			Status:        &failed,
			FailureReason: &sandboxws.ArtifactFailureReason{Value: "verification_failed"},
		})
	})

	// Explicit JSON null failureReason (distinct from the key being absent
	// entirely, which the StatusAbsent/StatusReady subtests above exercise
	// via omitempty) must also validate.
	t.Run("Artifact_ExplicitNullFailureReason", func(t *testing.T) {
		payload := []byte(`{"type":"artifact","messageId":"a4","sessionId":"` + testSessionID + `","gen":1,"artifactType":"upload","url":"https://example.test/x","metadata":{},"status":"ready","failureReason":null}`)
		if err := validateJSON(t, sch, payload); err != nil {
			t.Fatalf("expected explicit null failureReason to validate, got: %v", err)
		}
	})

	// An unrecognized failureReason value must still be rejected -- this
	// field is a closed enum matching the Postgres artifact_failure_reason
	// type exactly, not an open string.
	t.Run("Artifact_UnknownFailureReasonRejected", func(t *testing.T) {
		payload := []byte(`{"type":"artifact","messageId":"a5","sessionId":"` + testSessionID + `","gen":1,"artifactType":"upload","url":"https://example.test/x","metadata":{},"status":"failed","failureReason":"not_a_real_reason"}`)
		if err := validateJSON(t, sch, payload); err == nil {
			t.Fatal("expected unknown failureReason value to fail validation, got nil error")
		}
	})

	// An unrecognized status value must still be rejected too.
	t.Run("Artifact_UnknownStatusRejected", func(t *testing.T) {
		payload := []byte(`{"type":"artifact","messageId":"a6","sessionId":"` + testSessionID + `","gen":1,"artifactType":"upload","url":"https://example.test/x","metadata":{},"status":"pending"}`)
		if err := validateJSON(t, sch, payload); err == nil {
			t.Fatal("expected status=\"pending\" (a valid Postgres artifact_status value the WIRE event never carries) to fail validation, got nil error")
		}
	})

	// pr/preview artifact events emitted before this Step (or by any
	// future producer that never sets these fields) must stay a valid,
	// unchanged shape -- the exact fixture the pre-existing "Artifact"
	// subtest in TestSandboxEventsRoundTrip above already covers; this
	// assertion just makes the "still valid with neither new field"
	// property explicit for THIS test's own reviewers.
	t.Run("PreExistingPRArtifactShapeStillValid", func(t *testing.T) {
		roundTrip(t, sch, sandboxws.Artifact{
			Type:         "artifact",
			MessageId:    "a7",
			SessionId:    testSessionID,
			Gen:          1,
			ArtifactType: sandboxws.ArtifactArtifactTypePr,
			Url:          "https://github.com/narvidev/narvi/pull/42",
			Metadata:     sandboxws.ArtifactMetadata{"number": float64(42)},
		})
	})
}
