//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/domain/framecut"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves the storage guard and the plan notices against a real
// Postgres instance for frames the sandbox-agent cut on its way to the
// control plane (technical plan §6.1), driving every frame through
// Actor.Send as wshub's read loop does. Nothing produces a cut yet: the
// frames are built here as the cutter will write them, `cut` beside the
// text.

// cutTokenFrameRaw is tokenFrameRaw with a raw `cut` property, "" for none.
func cutTokenFrameRaw(t *testing.T, sessionID, partID, text string, cut json.RawMessage) json.RawMessage {
	t.Helper()
	frame := map[string]any{"type": "token", "messageId": partID, "sessionId": sessionID, "gen": 1, "text": text}
	if len(cut) > 0 {
		frame["cut"] = cut
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal token frame: %v", err)
	}
	return raw
}

// TestHandleSandboxEvent_TokenFrames_SharedCutVectors replays each case of
// the shared vector file (web/src/session/__tests__/fixtures/
// tokenCutFrames.json, which framecut, plan.FinalText and the web timeline
// read too) through Actor.Send while its turn is Processing: the rows
// stored are the ones the vector names, each stored row keeps its `cut`
// verbatim, and the turn's final text is the frame the vector names, with
// its cut. One session per case, one pool and one registry for all.
func TestHandleSandboxEvent_TokenFrames_SharedCutVectors(t *testing.T) {
	raw, err := os.ReadFile(tokenCutFramesFixture)
	if err != nil {
		t.Fatalf("read %s: %v", tokenCutFramesFixture, err)
	}
	var cases []struct {
		Name   string `json:"name"`
		Frames []struct {
			ID   int64           `json:"id"`
			Text string          `json:"text"`
			Cut  json.RawMessage `json:"cut"`
		} `json:"frames"`
		Want *struct {
			ID  int64         `json:"id"`
			Cut *framecut.Cut `json:"cut"`
		} `json:"want"`
		Stored []int64 `json:"stored"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("decode %s: %v", tokenCutFramesFixture, err)
	}

	ctx := context.Background()
	pool := newTestPool(t)
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })

	for _, tt := range cases {
		t.Run(tt.Name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}
			turn := createDispatchedProcessingTurn(ctx, t, pool, sessionID)
			a, err := r.GetOrSpawn(ctx, sessionID)
			if err != nil {
				t.Fatalf("GetOrSpawn: %v", err)
			}

			var wantStored []string
			for _, f := range tt.Frames {
				sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "token", Gen: 1, MessageID: tokenTestPartID, Raw: cutTokenFrameRaw(t, sessionID.String(), tokenTestPartID, f.Text, f.Cut)})
				for _, id := range tt.Stored {
					if id == f.ID {
						wantStored = append(wantStored, f.Text+"|"+string(normalizedCut(t, f.Cut)))
					}
				}
			}

			var gotStored []string
			for _, row := range listStoredTokenRows(ctx, t, pool, sessionID) {
				var payload struct {
					Cut json.RawMessage `json:"cut"`
				}
				if err := json.Unmarshal(row.payload, &payload); err != nil {
					t.Fatalf("decode stored payload: %v", err)
				}
				gotStored = append(gotStored, row.payloadText+"|"+string(normalizedCut(t, payload.Cut)))
			}
			if !reflect.DeepEqual(gotStored, wantStored) {
				t.Errorf("stored rows (text|cut) = %q, want %q", gotStored, wantStored)
			}

			final := a.planContentText(ctx, turn)
			if tt.Want == nil {
				if final != (plandomain.Final{Text: plandomain.ContentFallbackText}) {
					t.Errorf("planContentText = %+v, want the placeholder", final)
				}
				return
			}
			var wantText string
			for _, f := range tt.Frames {
				if f.ID == tt.Want.ID {
					wantText = f.Text
				}
			}
			if final.Text != wantText || !reflect.DeepEqual(final.Cut, tt.Want.Cut) {
				t.Errorf("planContentText = (%q, %+v), want frame %d (%q, %+v)", final.Text, final.Cut, tt.Want.ID, wantText, tt.Want.Cut)
			}
		})
	}
}

// normalizedCut re-encodes a raw `cut` property compactly, so the bytes
// Postgres stores (jsonb normalizes spacing and key order) and the bytes
// sent compare equal; empty for none.
func normalizedCut(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode cut %s: %v", raw, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode cut: %v", err)
	}
	return out
}

// TestCompleteProcessingTurn_PlanApproval_CutPlan_OffersNoApprove proves
// both plan-approval notices of a plan whose final text is a cut frame
// carry the cut: the chat one sets PlanApprovalPayload.Cut, so
// slackapi.PostPlanApprovalMessage posts no Approve button, and the issue
// tracker's text offers no approve keyword and gives the reason. A plan
// stored whole and then cut carries no cut, and is offered for approval.
func TestCompleteProcessingTurn_PlanApproval_CutPlan_OffersNoApprove(t *testing.T) {
	const whole = tokenTestFullText
	cut := framecut.Cut{Kept: len(tokenTestPrefix), Total: len(whole)}
	cutRaw, err := json.Marshal(cut)
	if err != nil {
		t.Fatal(err)
	}
	cutText := tokenTestPrefix + "\n[text cut at 10 of 47 bytes on its way from the sandbox]"
	if len(whole) != 47 || len(tokenTestPrefix) != 10 {
		t.Fatalf("fixture: whole %d bytes, prefix %d, the marker says 47 and 10", len(whole), len(tokenTestPrefix))
	}

	type sentFrame struct {
		text string
		cut  json.RawMessage
	}
	frameSets := []struct {
		name    string
		frames  []sentFrame
		wantCut *framecut.Cut
	}{
		{name: "the final text is cut", frames: []sentFrame{{"", nil}, {cutText, cutRaw}}, wantCut: &cut},
		{name: "stored whole, then cut", frames: []sentFrame{{"", nil}, {whole, nil}, {cutText, cutRaw}}},
	}
	origins := []struct {
		name  string
		setup func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID
		check func(t *testing.T, row sqlcgen.Outbox, wantCut *framecut.Cut)
	}{
		{
			name: "chat",
			setup: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
				sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceSlack)
				if _, ok, err := narvipg.NewSlackThreadSessionStore(pool).Claim(ctx, "C123", "1700000000.000100", sessionID); err != nil || !ok {
					t.Fatalf("claim slack thread session: ok=%v err=%v", ok, err)
				}
				return sessionID
			},
			check: func(t *testing.T, row sqlcgen.Outbox, wantCut *framecut.Cut) {
				var payload slackapi.PlanApprovalPayload
				if err := json.Unmarshal(row.Payload, &payload); err != nil {
					t.Fatalf("unmarshal slack plan approval payload: %v", err)
				}
				if !reflect.DeepEqual(payload.Cut, wantCut) {
					t.Errorf("payload cut = %+v, want %+v", payload.Cut, wantCut)
				}
				if wantText := map[bool]string{true: cutText, false: whole}[wantCut != nil]; payload.Text != wantText {
					t.Errorf("payload text = %q, want %q", payload.Text, wantText)
				}
				if wantCut == nil && strings.Contains(string(row.Payload), `"cut"`) {
					t.Errorf("a whole plan's payload %s carries a cut key", row.Payload)
				}
			},
		},
		{
			name: "issue tracker",
			setup: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
				sessionID := createTestSessionWithSpawnSource(ctx, t, pool, sqlcgen.SessionSpawnSourceLinear)
				agentSessions := narvipg.NewLinearAgentSessionStore(pool)
				if _, err := agentSessions.Claim(ctx, "agent-session-cut", "org-cut"); err != nil {
					t.Fatalf("claim linear agent session: %v", err)
				}
				if err := agentSessions.SetSessionID(ctx, "agent-session-cut", sessionID); err != nil {
					t.Fatalf("set linear agent session id: %v", err)
				}
				return sessionID
			},
			check: func(t *testing.T, row sqlcgen.Outbox, wantCut *framecut.Cut) {
				var payload linearapi.Payload
				if err := json.Unmarshal(row.Payload, &payload); err != nil {
					t.Fatalf("unmarshal linear payload: %v", err)
				}
				text := map[bool]string{true: cutText, false: whole}[wantCut != nil]
				if want := planApprovalLinearText(1, text, wantCut); payload.Text != want {
					t.Errorf("text = %q, want %q", payload.Text, want)
				}
				offersApprove := false
				for _, word := range strings.FieldsFunc(payload.Text, func(r rune) bool { return !unicode.IsLetter(r) }) {
					for _, keyword := range plandomain.ApproveKeywords {
						if word == keyword {
							offersApprove = true
						}
					}
				}
				if offersApprove != (wantCut == nil) {
					t.Errorf("text %q offers an approve keyword: %v, want %v", payload.Text, offersApprove, wantCut == nil)
				}
			},
		},
	}

	for _, fs := range frameSets {
		for _, origin := range origins {
			t.Run(fs.name+"/"+origin.name, func(t *testing.T) {
				ctx := context.Background()
				pool := newTestPool(t)
				sessionID := origin.setup(ctx, t, pool)
				if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
					t.Fatalf("create sandbox: %v", err)
				}
				createProcessingTurnWithPlanMode(ctx, t, narvipg.NewTurnStore(pool), sessionID, true, nil)

				r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, nil, nil, "", nil, nil, "", nil, false)
				if err != nil {
					t.Fatalf("NewRegistry: %v", err)
				}
				t.Cleanup(func() { _ = r.Shutdown() })
				a, err := r.GetOrSpawn(ctx, sessionID)
				if err != nil {
					t.Fatalf("GetOrSpawn: %v", err)
				}

				for _, f := range fs.frames {
					sendSandboxEventForTest(ctx, t, a, SandboxEvent{Type: "token", Gen: 1, MessageID: tokenTestPartID, Raw: cutTokenFrameRaw(t, sessionID.String(), tokenTestPartID, f.text, f.cut)})
				}
				sendSandboxEventForTest(ctx, t, a, SandboxEvent{
					Type: "execution_complete",
					Gen:  1,
					Raw:  executionCompleteRaw(t, sessionID.String(), 1, sandboxws.ExecutionCompleteOutcomeCompleted),
				})

				origin.check(t, getSoleOutboxRowForSession(ctx, t, pool, sessionID), fs.wantCut)
			})
		}
	}
}
