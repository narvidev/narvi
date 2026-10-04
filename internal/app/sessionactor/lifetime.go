package sessionactor

import (
	"context"

	"github.com/jackc/pgx/v5"

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

// logLifetimeDeadline records the deadline a spawn or restore claim
// stamped on row, its new gen, in the claim's transaction (technical plan
// §35.2) -- what an operator reads to tell a sandbox ended by its
// provider's lifetime from one that failed.
func (a *Actor) logLifetimeDeadline(claim string, kind sandbox.LifetimeKind, row sqlcgen.Sandbox) {
	var deadline any
	if row.LifetimeDeadlineAt.Valid {
		deadline = row.LifetimeDeadlineAt.Time
	}
	var seconds any
	if row.LifetimeSeconds != nil {
		seconds = *row.LifetimeSeconds
	}
	a.logger.Info("sessionactor: sandbox gen claimed with a lifetime deadline",
		"session_id", a.sessionID.String(), "claim", claim, "gen", row.Gen,
		"lifetime_kind", string(kind), "lifetime_seconds", seconds, "lifetime_deadline_at", deadline)
}
