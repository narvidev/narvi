// This file (credentialgate.go) refuses, at the dispatch gate, a turn whose
// model only a personal provider link could run, in a session that never
// resolves that link (technical plan §29.4): a pull request's review
// session, whose creator is merely the first linked person who triggered
// it, or a child session. Its sandbox is never delivered the link
// (httpapi.ProviderCredentialsDelivery, through the same
// credentialscope.Scope), so such a turn could only fail somewhere inside
// the agent runtime with a provider error that names nothing. It is refused
// before it is sent instead, with that reason named: the turn fails,
// never_started; its terminal event and a session warning open with
// providercredential.RefusalPersonalLinkOnly; a review attempt's check
// closes as not completed with the same reason; and a workflow run the
// turn belongs to escalates for a person instead of following an edge
// (workflowengine.OnTurnRefused). It never runs on the link, and never on
// another model in its place.
//
// Only the session creator's own link is known here, so only a model that
// link carries is refused by name. A model no credential this session
// resolves serves -- whoever else holds a link for it -- is dispatched as
// before and fails inside the agent runtime; it runs on nobody's link
// either way.
//
// Bounded twice. Within one round, only the turns pending when the round
// began are ever refused: a turn the refusal itself queued is neither
// refused nor dispatched in that round. Across rounds, a refusal queues
// nothing, because the workflow run escalates rather than re-firing the
// step.
//
// A turn that names no model is not this gate's business: the agent
// runtime picks its default among the credentials the sandbox holds, and
// the sandbox agent removes OpenCode's persisted auth store before every
// boot, so it holds only what was delivered on that boot, never a withheld
// link.

package sessionactor

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/credentialscope"
	"github.com/narvidev/narvi/internal/app/workflowengine"
	"github.com/narvidev/narvi/internal/domain/providercredential"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
	"github.com/narvidev/narvi/internal/domain/turn"
)

// personalLinkGate is what one planDispatch round needs to know to refuse
// a turn: the providers the session's resolution reaches, and those its
// creator's own link carries that the resolution withholds. Loaded at most
// once per round, and only when the turn about to be dispatched names a
// model.
type personalLinkGate struct {
	resolvable map[providercredential.Provider]bool
	personal   map[providercredential.Provider]bool
}

// loadPersonalLinkGate reads sessionRow's credential scope -- its
// github_pr_sessions membership included, read here, in this transaction --
// and, when the scope withholds its creator's link, which providers fall on
// each side. A read failure is returned, never taken as "nothing
// withheld": planDispatch then dispatches nothing this round.
func (a *Actor) loadPersonalLinkGate(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session) (personalLinkGate, error) {
	if a.stores.githubPRSession == nil || a.stores.providerCredential == nil {
		return personalLinkGate{}, nil
	}
	scope, err := credentialscope.Load(ctx, a.stores.githubPRSession.WithTx(tx), sessionRow)
	if err != nil {
		return personalLinkGate{}, fmt.Errorf("sessionactor: personal-link gate: %w", err)
	}
	if scope.UserID() != nil || scope.Origin.CreatedBy == "" {
		// The session resolves its creator's link, or has no creator:
		// nothing is withheld, so nothing can be refused for it.
		return personalLinkGate{}, nil
	}
	resolvable, personal, err := scope.Providers(ctx, a.stores.providerCredential.WithTx(tx))
	if err != nil {
		return personalLinkGate{}, fmt.Errorf("sessionactor: personal-link gate: %w", err)
	}
	return personalLinkGate{resolvable: resolvable, personal: personal}, nil
}

// refuseTurnFunc ends one pending turn as refused. refusePersonalLinkOnly
// in production; a parameter only so a test can have the refusal queue a
// turn of its own and prove the round never refuses that turn.
type refuseTurnFunc func(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session, turns []sqlcgen.Turn, target sqlcgen.Turn, provider providercredential.Provider, gen int, now time.Time) error

// refusePersonalLinkOnlyPending runs the gate over the turn planDispatch is
// about to dispatch (pendingID, turn.NextToDispatch's pick), refusing it
// when its model only a withheld link could run, then over the next one,
// until a turn passes or none is left. Only turns already pending when the
// round began are candidates: if the next pick is one this round's own
// refusal queued, the round ends with nothing to dispatch, and that turn
// waits for the next round, which gates it like any other. It returns the
// turns as they stand afterwards and the pending turn, if any, that passed.
func (a *Actor) refusePersonalLinkOnlyPending(
	ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session,
	turns []sqlcgen.Turn, pendingID pgtype.UUID, hasPending bool, gen int, now time.Time,
	refuse refuseTurnFunc,
) ([]sqlcgen.Turn, pgtype.UUID, bool, error) {
	roundPending := make(map[pgtype.UUID]bool, len(turns))
	for _, t := range turns {
		if turn.State(t.Status) == turn.StatePending {
			roundPending[t.ID] = true
		}
	}

	var gate *personalLinkGate
	for hasPending {
		if !roundPending[pendingID] {
			a.logger.Warn("sessionactor: a turn queued by this round's own refusal is held for the next round",
				"turn_id", pendingID.String())
			return turns, pgtype.UUID{}, false, nil
		}
		target, ok := findTurnByID(turns, pendingID)
		if !ok || target.ModelID == nil || *target.ModelID == "" {
			break
		}
		if gate == nil {
			loaded, err := a.loadPersonalLinkGate(ctx, tx, sessionRow)
			if err != nil {
				return nil, pgtype.UUID{}, false, err
			}
			gate = &loaded
		}
		provider, refused := providercredential.PersonalLinkOnly(*target.ModelID, gate.resolvable, gate.personal)
		if !refused {
			break
		}
		if err := refuse(ctx, tx, sessionRow, turns, target, provider, gen, now); err != nil {
			return nil, pgtype.UUID{}, false, err
		}
		delete(roundPending, target.ID)
		var err error
		if turns, err = a.stores.turn.WithTx(tx).ListForSession(ctx, a.sessionID); err != nil {
			return nil, pgtype.UUID{}, false, fmt.Errorf("sessionactor: list turns: %w", err)
		}
		pendingID, hasPending = turn.NextToDispatch(toQueueEntries(turns))
	}
	return turns, pendingID, hasPending, nil
}

// refusePersonalLinkOnly ends target, still pending, as a turn that never
// started: the machine's own Pending -> Failed edge (turn.TriggerAbandon),
// so the session's failure reason is never_started, the existing value for
// a turn given up on before it reached a sandbox. The named cause travels
// beside it: at the head of the synthetic terminal event's reason, in a
// session warning (the banner a session shows every warning in, the way
// this package names every other reason it originates), and on a review
// attempt's check. gen is the session's sandbox gen, 0 with no sandbox
// yet; the warning carries it like every other.
func (a *Actor) refusePersonalLinkOnly(ctx context.Context, tx pgx.Tx, sessionRow sqlcgen.Session, turns []sqlcgen.Turn, target sqlcgen.Turn, provider providercredential.Provider, gen int, now time.Time) error {
	from := turn.State(target.Status)
	to, err := turn.Transition(from, turn.TriggerAbandon)
	if err != nil {
		return fmt.Errorf("sessionactor: refuse turn %s: %w", target.ID.String(), err)
	}
	if _, err := a.turnWrites(tx).UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID:          target.ID,
		Status:      sqlcgen.TurnStatus(to),
		CompletedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}); err != nil {
		return fmt.Errorf("sessionactor: update turn status: %w", err)
	}

	reason := providercredential.PersonalLinkOnlyMessage(*target.ModelID, provider)

	// Never OnTurnCompleted: it would read the refusal as an implicit
	// "blocked" and follow any blocked edge the step wires, queueing the
	// same refused attempt again (refusal.go's doc comment).
	workflowengine.OnTurnRefused(ctx, workflowengine.Deps{
		Workflows:             a.stores.workflow.WithTx(tx),
		Turns:                 a.stores.turn.WithTx(tx),
		SlackThreadSessions:   a.stores.slackThreadSession.WithTx(tx),
		LinearAgentSessions:   a.stores.linearAgentSession.WithTx(tx),
		GitHubPRSessions:      a.stores.githubPRSession.WithTx(tx),
		Outbox:                a.stores.outbox.WithTx(tx),
		EpistemicCheckDefault: a.epistemicCheckDefault,
	}, sessionRow, target.ID, reason)

	if turn.RequiresSyntheticExecutionComplete(turn.TriggerAbandon) {
		if err := a.appendEvent(ctx, tx, "execution_complete", syntheticExecutionComplete(target.ID, from, reason)); err != nil {
			return err
		}
	}
	if err := a.recordSessionWarning(ctx, tx, gen, reason); err != nil {
		return err
	}

	failureReason, _ := turn.DeriveFailureReason(from, turn.TriggerAbandon)
	if err := a.enqueueOutboxNotification(ctx, tx, sessionRow, turn.TriggerAbandon, failureReason, target, nil, reviewcheck.NotAssessedPersonalLinkOnly); err != nil {
		return err
	}
	if err := a.persistDerivedSessionStatus(ctx, tx, summariesWithOverride(turns, target.ID, to, failureReason)); err != nil {
		return err
	}
	a.logger.Warn("sessionactor: turn refused: its model is available only through a personal provider link this session never runs on",
		"turn_id", target.ID.String(), "model", *target.ModelID, "provider", string(provider),
		"refusal", string(providercredential.RefusalPersonalLinkOnly))
	return nil
}
