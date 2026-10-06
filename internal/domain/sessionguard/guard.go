package sessionguard

import (
	"errors"
	"fmt"
	"time"
)

// Reason names why the guard refused a turn. It is also the turn's end
// reason when a queued turn is ended at dispatch (internal/domain/turn's
// EndReason values), the reason key of a refusal's HTTP body, and the
// reason label of session_guard_refused_total.
type Reason string

// ReasonSpendCap is the reason of a session that has spent its cap
// (technical plan §40.1).
const ReasonSpendCap Reason = "spend_cap"

// ErrSpendCapReached is what a ReasonSpendCap Refusal unwraps to, so a
// caller recognizes it with errors.Is.
var ErrSpendCapReached = errors.New("sessionguard: the session has reached its spend cap")

// Origin is who or what asked for the turn being decided.
type Origin int

const (
	// OriginPerson is a person's act through a human surface -- a prompt,
	// a plan approval, a workflow step decision -- and not one made
	// through an MCP client's grant.
	OriginPerson Origin = iota + 1
	// OriginAutomatic is every producer no person asked right then: a
	// workflow's own advance, the automatic re-review, an owed review
	// request's re-run, the release composition review; and a turn an MCP
	// client or a bot-attributed caller asked for.
	OriginAutomatic
	// OriginDispatch is a queued turn about to be dispatched: the second
	// check, made whoever created it.
	OriginDispatch
)

// String names the origin for logs.
func (o Origin) String() string {
	switch o {
	case OriginPerson:
		return "person"
	case OriginAutomatic:
		return "automatic"
	case OriginDispatch:
		return "dispatch"
	default:
		return fmt.Sprintf("Origin(%d)", int(o))
	}
}

// The kinds of thing a cap is set on.
const (
	CapSourceRepo       = "repo"
	CapSourceAutomation = "automation"
)

// CapSource is where a session's effective cap was set: a repository
// (Kind CapSourceRepo, Name and ID its owner/name) or an automation (Kind
// CapSourceAutomation, Name its name, ID its id).
type CapSource struct {
	Kind string
	Name string
	ID   string
}

// Facts is what the guard reads about one session, in the transaction that
// holds the session's row lock: what it has spent, and the caps that apply
// to it.
type Facts struct {
	// SessionID is the session the facts are about.
	SessionID [16]byte
	// ObservedAt is the database's own instant of the read.
	ObservedAt time.Time
	// SpentUSD is SUM(cost_usd) over the session's dispatched turns: a
	// lower bound of what it cost (technical plan §25.15).
	SpentUSD MicroUSD
	// AutomationCap is the session spend cap of the automation that created
	// the session, nil when none did or it sets none.
	AutomationCap    *MicroUSD
	AutomationSource CapSource
	// RepoCap is the strictest session spend cap among the session's
	// repositories, nil when none sets one.
	RepoCap    *MicroUSD
	RepoSource CapSource
	// Turns is how many turns the session has: every turn it was ever
	// admitted, whatever became of it. Turns are never deleted, and a turn
	// exists only once the guard admitted it, so the count grows exactly
	// when the session is admitted a turn -- it names the crossing a
	// refusal belongs to (WarningKey).
	Turns int64
}

// EffectiveCap is the cap the session is held to, and where it was set:
// the automation's own when it sets one -- even above its repository's,
// since both are written by an administrator alone -- and the strictest of
// its repositories' otherwise. nil when no cap applies.
func (f Facts) EffectiveCap() (*MicroUSD, CapSource) {
	if f.AutomationCap != nil {
		return f.AutomationCap, f.AutomationSource
	}
	if f.RepoCap != nil {
		return f.RepoCap, f.RepoSource
	}
	return nil, CapSource{}
}

// Refusal is the guard's answer for a turn the session may not take. It is
// an error, and unwraps to the sentinel of its reason (ErrSpendCapReached),
// so it travels through every caller's error chain and is recognized there
// with errors.Is, or read whole with AsRefusal.
type Refusal struct {
	Reason    Reason
	SessionID [16]byte
	// Cap and Spent are the effective cap and the recorded spend the
	// decision compared.
	Cap   MicroUSD
	Spent MicroUSD
	// Source is where Cap was set.
	Source CapSource
	// Turns is Facts.Turns at the refusal: the crossing it belongs to.
	Turns int64
}

// Error is the refusal's text: what every surface tells a person (Text).
func (r *Refusal) Error() string { return Text(*r) }

// Unwrap returns the sentinel of the refusal's reason.
func (r *Refusal) Unwrap() error {
	switch r.Reason {
	case ReasonSpendCap:
		return ErrSpendCapReached
	default:
		return nil
	}
}

// AsRefusal returns the Refusal err carries, if any.
func AsRefusal(err error) (*Refusal, bool) {
	var r *Refusal
	if errors.As(err, &r) && r != nil {
		return r, true
	}
	return nil, false
}

// Admission is the proof a turn of one session was admitted: the argument
// postgres.TurnStore.CreateAndArmDispatch requires. Its fields are
// unexported, so outside this package one is had only from Decide or
// AdmitNewSession; the zero value admits nothing.
type Admission struct {
	sessionID [16]byte
	ok        bool
}

// Admits reports whether a admits a turn of sessionID.
func (a Admission) Admits(sessionID [16]byte) bool {
	return a.ok && a.sessionID == sessionID
}

// Decide is the guard's decision for one turn of f's session. The cap is
// checked for every origin: a session whose recorded spend is at or past
// its effective cap is refused, compared in micro-dollars. Otherwise the
// turn is admitted.
func Decide(f Facts, origin Origin) (Admission, *Refusal) {
	_ = origin // the cap binds every origin; see the package doc.
	if limit, source := f.EffectiveCap(); limit != nil && f.SpentUSD >= *limit {
		return Admission{}, &Refusal{
			Reason:    ReasonSpendCap,
			SessionID: f.SessionID,
			Cap:       *limit,
			Spent:     f.SpentUSD,
			Source:    source,
			Turns:     f.Turns,
		}
	}
	return Admission{sessionID: f.SessionID, ok: true}, nil
}

// AdmitNewSession admits the first turn of sessionID, a session created in
// the same transaction that creates the turn: it has no turn yet, so it
// has spent nothing, and no cap above zero -- the only kind a cap can be --
// can refuse it. Only the session-creation transaction calls it
// (httpapi's CreateSessionOnTx), which a source scan keeps so.
func AdmitNewSession(sessionID [16]byte) Admission {
	return Admission{sessionID: sessionID, ok: true}
}

// WarningKey is the stable name of one crossing of the guard: the session,
// the reason, what the decision compared against -- for the cap, its value
// and where it was set -- and the session's turn count (Refusal.Turns).
//
// A crossing is the session refused after it was last admitted a turn,
// under one cap. Every refusal of the same crossing has the same key, so
// the warning it records (internal/app/turnguard derives its message id
// from this key) is stored once however many turns are refused: while the
// session is refused it is admitted nothing, so its turn count holds still,
// and the turn in flight, its re-sends, and the queued turns ended at
// dispatch were all counted before the crossing. Once the session is
// admitted a turn again -- the cap raised, cleared or moved, and a turn
// taken -- its count has grown, so the next refusal is a new crossing even
// at a cap value the session crossed before; and a cap changed to a new
// value or source is a new crossing at once. A cap moved away and back with
// no turn admitted in between is the same crossing: the session took
// nothing in between, and its warning still says what it spent.
func WarningKey(r Refusal) string {
	return fmt.Sprintf("urn:narvi:session-guard:%s:%s:%d:%s:%s:%d",
		r.Reason, formatSessionID(r.SessionID), int64(r.Cap), r.Source.Kind, r.Source.ID, r.Turns)
}

// formatSessionID prints id in the canonical 8-4-4-4-12 form.
func formatSessionID(id [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
}
