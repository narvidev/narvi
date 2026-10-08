// This file (reviewbaserepository.go) moves a pull request's review session
// onto its base repository at the one moment that is safe: when its next
// gen is about to boot (technical plan §21.1, §30.4).
//
// A review session clones the pull request's base repository and reads
// the head from the base's refs/pull/<number>/head (SESSION_CONFIG's ref,
// pullRequestRef). A session created before its spec named the base
// repository names its pull request's head repository -- for a pull
// request from a fork, the fork -- and its gens cloned the fork: their
// clone's origin is the fork's, which holds no pull ref. The checkout gate
// (reviewcheckout.go) checks a turn out only when the spec names the
// claim's repository, reading the spec as the one the live gen booted
// with, so the spec may change only when no gen holds the old one:
//
//   - the review_sessions_base_repository migration moved every such
//     session that held no live gen when it ran;
//   - this file moves the rest, and any a binary without it creates during
//     a rolling deploy, in the transaction that spawns or restores the
//     session's next gen, before that gen's SESSION_CONFIG is assembled,
//     so the gen boots on the base repository at the pull request's ref
//     and every reader of the spec -- the credential mint, the egress
//     mode, the rollout and entitlement re-checks this same spawn runs
//     next -- resolves the base from then on.
//
// A resume is never a moment to move: it delivers no new SESSION_CONFIG
// (ResumeSandbox takes no spec), so a resumed gen keeps the spec it booted
// with. Until its session moves, a legacy session dispatches as it did
// before its spec could name the base: the gate leaves its turns alone.

package sessionactor

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// moveReviewSessionToBaseRepository moves sessionRow onto the base
// repository its pull request's claim names, when its spec still names
// another (postgres.SessionStore.MoveReviewSessionToBaseRepository), and
// returns the row with the moved repos; any other session is returned as
// it is. It runs inside tryPlanSpawn's transaction, under the session's
// own lock, only for a spawn or a restore.
func (a *Actor) moveReviewSessionToBaseRepository(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session) (sqlcgen.Session, error) {
	repos, moved, err := a.stores.session.WithTx(tx).MoveReviewSessionToBaseRepository(ctx, sessionRow.ID)
	if err != nil {
		return sessionRow, fmt.Errorf("sessionactor: move the review session onto its base repository: %w", err)
	}
	if !moved {
		return sessionRow, nil
	}
	a.logger.Info("sessionactor: the review session's spec named its pull request's head repository; moved onto the base repository before its next gen boots",
		"from", string(sessionRow.Repos), "to", string(repos))
	sessionRow.Repos = repos
	return sessionRow, nil
}
