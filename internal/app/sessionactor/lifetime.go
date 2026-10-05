package sessionactor

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/platform"
)

// sandboxLifetimeSeconds is the lifetime, in whole seconds, the claim that
// spawns or restores this session's sandbox stamps its deadline with
// (technical plan §35.2): the session kind's lifetime, from
// platform.Timeouts.SandboxLifetimeFor, the one place a kind's lifetime is
// decided. A pull request's review session -- one with a
// github_pr_sessions row, read in the claim's transaction -- is the review
// kind; every other session the default. Validate keeps every kind's
// lifetime positive and in whole seconds.
//
// A failed lookup is returned: the claim's transaction rolls back and the
// dispatch timer backs off, as for any other read the claim makes. The
// estimate is never guessed from the default kind on an error, since a
// longer default would put the deadline past the provider's own.
func (a *Actor) sandboxLifetimeSeconds(ctx context.Context, tx pgx.Tx) (*int32, sandbox.LifetimeKind, error) {
	review := false
	if a.stores.githubPRSession != nil {
		var err error
		if review, err = a.isReviewSession(ctx, a.stores.githubPRSession.WithTx(tx)); err != nil {
			return nil, "", err
		}
	}
	kind := sandbox.LifetimeKindFor(review)
	seconds := platform.DurationToSeconds(a.timeouts.SandboxLifetimeFor(kind))
	return &seconds, kind, nil
}

// lifetimeStamp is what a spawn or restore claim stamped on its new gen
// (technical plan §35.2), carried on its spawnPlan so it can be logged
// once the claim has committed.
type lifetimeStamp struct {
	claim    string // "spawn" or "restore"
	kind     sandbox.LifetimeKind
	gen      int32
	seconds  *int32
	deadline pgtype.Timestamptz
}

// newLifetimeStamp reads the stamp off row, the row the claim's upsert
// returned.
func newLifetimeStamp(claim string, kind sandbox.LifetimeKind, row sqlcgen.Sandbox) *lifetimeStamp {
	return &lifetimeStamp{claim: claim, kind: kind, gen: row.Gen, seconds: row.LifetimeSeconds, deadline: row.LifetimeDeadlineAt}
}

// logLifetimeDeadline records the deadline a committed spawn or restore
// claim stamped on its gen (technical plan §35.2) -- what an operator
// reads to tell a sandbox ended by its provider's lifetime from one that
// failed. executePlans calls it after planDispatch's transaction has
// committed, never inside it, so a claim that rolled back is never logged
// as claimed. A resume plan carries no stamp and logs nothing.
func (a *Actor) logLifetimeDeadline(plan *spawnPlan) {
	stamp := plan.lifetime
	if stamp == nil {
		return
	}
	var deadline any
	if stamp.deadline.Valid {
		deadline = stamp.deadline.Time
	}
	var seconds any
	if stamp.seconds != nil {
		seconds = *stamp.seconds
	}
	a.logger.Info(lifetimeClaimedLogMessage,
		"session_id", a.sessionID.String(), "claim", stamp.claim, "gen", stamp.gen,
		"lifetime_kind", string(stamp.kind), "lifetime_seconds", seconds, "lifetime_deadline_at", deadline)
}

// lifetimeClaimedLogMessage is logLifetimeDeadline's message.
const lifetimeClaimedLogMessage = "sessionactor: sandbox gen claimed with a lifetime deadline"
