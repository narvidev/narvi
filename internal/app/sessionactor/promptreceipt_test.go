package sessionactor

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// TestReadyAdvertisesPromptReceipt pins how a ready's capability is read
// (technical plan §3.3, prompt receipts): only an explicit true counts, and
// a ready that fails its schema decode counts as not advertising it.
func TestReadyAdvertisesPromptReceipt(t *testing.T) {
	t.Parallel()

	const base = `"type":"ready","messageId":"r1","sessionId":"s","gen":3,"timestamp":"2026-10-01T12:00:00Z","agentVersion":"dev","imageDigest":"unknown"`
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{name: "advertised", raw: `{` + base + `,"capabilities":{"promptReceipt":true}}`, want: true},
		{name: "advertised false", raw: `{` + base + `,"capabilities":{"promptReceipt":false}}`, want: false},
		{name: "capabilities without it", raw: `{` + base + `,"capabilities":{}}`, want: false},
		{name: "an agent that predates capabilities", raw: `{` + base + `}`, want: false},
		{name: "fails its schema decode", raw: `{"type":"ready","messageId":"r1","gen":3,"capabilities":{"promptReceipt":true}}`, want: false},
		{name: "not JSON", raw: `not json`, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := readyAdvertisesPromptReceipt(json.RawMessage(tc.raw)); got != tc.want {
				t.Errorf("readyAdvertisesPromptReceipt(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestPromptReceiptCapable pins that the capability counts for the gen it
// was recorded against and no other: a respawn, restore or resume bumps the
// gen and inherits nothing.
func TestPromptReceiptCapable(t *testing.T) {
	t.Parallel()

	gen := func(g int32) *int32 { return &g }
	for _, tc := range []struct {
		name string
		row  sqlcgen.Sandbox
		want bool
	}{
		{name: "recorded for the live gen", row: sqlcgen.Sandbox{Gen: 4, PromptReceiptGen: gen(4)}, want: true},
		{name: "recorded for an earlier gen", row: sqlcgen.Sandbox{Gen: 5, PromptReceiptGen: gen(4)}, want: false},
		{name: "never recorded", row: sqlcgen.Sandbox{Gen: 4}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := promptReceiptCapable(tc.row); got != tc.want {
				t.Errorf("promptReceiptCapable(gen %d, prompt_receipt_gen %v) = %v, want %v", tc.row.Gen, tc.row.PromptReceiptGen, got, tc.want)
			}
		})
	}
}

// TestPromptResendFacts pins how the facts turn.PromptReconnectToAnswer
// decides on are read from the turn and sandbox rows: each departure from a
// capable, reconnected gen with a Processing turn whose own dispatch asked
// flips exactly its own fact.
func TestPromptResendFacts(t *testing.T) {
	t.Parallel()

	i32 := func(v int32) *int32 { return &v }
	str := func(v string) *string { return &v }
	capableSandbox := func() sqlcgen.Sandbox {
		return sqlcgen.Sandbox{Gen: 2, PromptReceiptGen: i32(2), ReadySeq: 5}
	}
	askedTurn := func() sqlcgen.Turn {
		return sqlcgen.Turn{
			Status:                    sqlcgen.TurnStatusProcessing,
			DispatchedMessageID:       str("m1"),
			ReceiptRequestedMessageID: str("m1"),
			ReceiptCheckedReadySeq:    i32(4),
		}
	}
	all := turn.PromptResendFacts{Processing: true, ReceiptRequested: true, GenCapable: true, ReconnectedSinceCheck: true}

	for _, tc := range []struct {
		name    string
		sandbox func(*sqlcgen.Sandbox)
		turn    func(*sqlcgen.Turn)
		want    func(*turn.PromptResendFacts)
	}{
		{name: "every fact holds"},
		{name: "not processing", turn: func(t *sqlcgen.Turn) { t.Status = sqlcgen.TurnStatusCompleted },
			want: func(f *turn.PromptResendFacts) { f.Processing = false }},
		{name: "a stale request: a later dispatch by a binary without receipts", turn: func(t *sqlcgen.Turn) { t.DispatchedMessageID = str("m2") },
			want: func(f *turn.PromptResendFacts) { f.ReceiptRequested = false }},
		{name: "no request", turn: func(t *sqlcgen.Turn) { t.ReceiptRequestedMessageID = nil },
			want: func(f *turn.PromptResendFacts) { f.ReceiptRequested = false }},
		{name: "never dispatched", turn: func(t *sqlcgen.Turn) { t.DispatchedMessageID = nil },
			want: func(f *turn.PromptResendFacts) { f.ReceiptRequested = false }},
		{name: "stop-flagged", turn: func(t *sqlcgen.Turn) { t.StopRequestedAt = pgtype.Timestamptz{Time: time.Unix(1, 0), Valid: true} },
			want: func(f *turn.PromptResendFacts) { f.StopRequested = true }},
		{name: "capability of an earlier gen", sandbox: func(s *sqlcgen.Sandbox) { s.Gen = 3 },
			want: func(f *turn.PromptResendFacts) { f.GenCapable = false }},
		{name: "no capability", sandbox: func(s *sqlcgen.Sandbox) { s.PromptReceiptGen = nil },
			want: func(f *turn.PromptResendFacts) { f.GenCapable = false }},
		{name: "a stop owes the gen its retirement", sandbox: func(s *sqlcgen.Sandbox) { s.StopRetireGen = i32(s.Gen) },
			want: func(f *turn.PromptResendFacts) { f.RetirementOwed = true }},
		{name: "no ready since the last check", sandbox: func(s *sqlcgen.Sandbox) { s.ReadySeq = 4 },
			want: func(f *turn.PromptResendFacts) { f.ReconnectedSinceCheck = false }},
		{name: "no check recorded", turn: func(t *sqlcgen.Turn) { t.ReceiptCheckedReadySeq = nil },
			want: func(f *turn.PromptResendFacts) { f.ReconnectedSinceCheck = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sb, tr, want := capableSandbox(), askedTurn(), all
			if tc.sandbox != nil {
				tc.sandbox(&sb)
			}
			if tc.turn != nil {
				tc.turn(&tr)
			}
			if tc.want != nil {
				tc.want(&want)
			}
			if got := promptResendFacts(sb, tr); got != want {
				t.Errorf("promptResendFacts() = %+v, want %+v", got, want)
			}
		})
	}
}
