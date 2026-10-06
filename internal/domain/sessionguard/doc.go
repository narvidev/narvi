// Package sessionguard is the one admission decision every new turn of a
// session passes before it is created, and every queued turn passes again
// before it is dispatched (technical plan §40.1): may this session take
// another turn?
//
// The decision has one reason today, the absolute spend cap (ReasonSpendCap):
// a session whose recorded spend is at or past its cap takes no new turn.
// The cap is the automation's own, for a session an automation created, when
// that automation sets one; the strictest cap among the session's
// repositories otherwise (Facts.EffectiveCap). The spend is SUM(cost_usd)
// over the session's dispatched turns, read from the rows that exist under
// the session-row lock, never a counter (internal/app/turnguard). The
// comparison is in integer micro-dollars (MicroUSD), so "at or past" is
// exact at the boundary.
//
// The cap applies to every turn on the session regardless of who asked for
// it: a person's prompt, a plan approval, a workflow step a person decides,
// the review button, every automatic producer. This inverts §24.6's rule
// on purpose. §24.6 exempts a person's manual re-trigger from the automatic
// re-review budget, because that budget exists to stop a loop and a person
// pressing a button is not one; a spend cap exists to stop money, and a cap
// a prompt can walk past is not a cap. The human's remedy is the audited
// raise of the cap, never a bypass. Origin is therefore carried for the
// record and for the controls that do distinguish who asked, never consulted
// by the cap.
//
// An Admission can be minted in exactly two ways: Decide, which only the
// application's guard (internal/app/turnguard) calls, and AdmitNewSession,
// which only the session-creation transaction calls for the first turn of
// a session it creates in that same transaction -- a session with no turn
// yet has spent nothing. postgres.TurnStore.CreateAndArmDispatch refuses to
// create a turn without an Admission for its session, and a source scan
// (internal/adapters/outbound/postgres's turn_insert_test.go) keeps both
// mints where they are, so no new path that creates a turn can skip the
// guard.
//
// What the cap cannot do, stated so nobody reads it as a hard ceiling on a
// bill: it refuses the next turn and never touches the one in flight, so the
// spend can pass the cap by up to that one turn's own cost; a step's cost
// that lands after its turn ended is counted nowhere, so the recorded spend
// is a lower bound (§25.15); a child session spawned during that turn has a
// cap of its own; and sandbox compute is not in cost_usd at all.
//
// The package is pure: no I/O, no clock, no randomness. Facts carries the
// database's own instant of the read.
package sessionguard
