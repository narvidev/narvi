package sessionactor

import (
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/domain/turn"
)

// syntheticDispatchedKey is the field a synthetic execution_complete carries,
// set to true, when the turn it ends was dispatched
// (turn.SyntheticEndIsOfDispatchedTurn): the session's one turn in flight,
// whose events the log holds. The page ends the turn it is showing on such
// an end only (web/src/session/eventPayloads.ts, asTurnEnd). A synthetic end
// without it -- a turn ended from pending, queued behind a running one and
// cancelled by a stop, refused at the credential gate or abandoned on a
// refused spawn, or any synthetic end written before the key existed --
// ends nothing on the page: ending the page's turn on it ended another
// turn, the one still running.
//
// The key is a stamp of the dispatched case, not of the pending one, so
// that a synthetic end stored before it existed reads as it did before the
// page read synthetic ends at all: as no turn end. The page cannot match an
// end to its turn by id: no event of the log names a turn, and the prompt's
// messageId, which the turn records, is on no stored event but an optional
// receipt.
const syntheticDispatchedKey = "dispatched"

// syntheticExecutionComplete is the payload of the synthetic
// execution_complete the control plane writes when it ends turnID itself,
// from state from, for reason (technical plan §3.3: "Stop/failure paths
// emit a synthetic execution_complete event so clients always see one
// terminal event per turn"): {turn_id, synthetic, reason}, and
// syntheticDispatchedKey when the turn was dispatched.
func syntheticExecutionComplete(turnID pgtype.UUID, from turn.State, reason string) map[string]any {
	payload := map[string]any{
		"turn_id":   turnID.String(),
		"synthetic": true,
		"reason":    reason,
	}
	if turn.SyntheticEndIsOfDispatchedTurn(from) {
		payload[syntheticDispatchedKey] = true
	}
	return payload
}
