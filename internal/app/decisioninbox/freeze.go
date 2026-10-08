// This file (freeze.go) is the decision inbox's side of the autonomy freeze
// (technical plan §40.2, §16.1): one banner for every role while autonomy is
// frozen, a held mark on each ready_to_merge row whose automatic merge the
// freeze holds, and the workflow runs whose automatic advance it holds. The
// inbox only reads the freeze; nothing here is a site (internal/domain/
// autonomy), and nothing here gates a person's action -- a held row keeps
// its Merge button.

package decisioninbox

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	appreviewverdict "github.com/narvidev/narvi/internal/app/reviewverdict"
	"github.com/narvidev/narvi/internal/domain/authz"
	"github.com/narvidev/narvi/internal/domain/decisioninbox"
	"github.com/narvidev/narvi/internal/platform"
)

// maxHeldWorkflowAdvances bounds Result.HeldWorkflowAdvances, on §21.1's
// "bounded from day one" discipline (maxAttentionRowsPerSource's own
// bound): the oldest holds first.
const maxHeldWorkflowAdvances = 100

// HeldWorkflowAdvance is one workflow run whose automatic advance to its
// next step the autonomy freeze holds (§40.2, §25.9): its attempt finished
// with its outcome stored, the run still running with no live attempt. Once
// the freeze lifts, the releaser applies the advance exactly once; a
// person's stop of the session drops it and cancels the run instead.
type HeldWorkflowAdvance struct {
	WorkflowRunID string
	SessionID     string
	// SessionTitle is nil when the session has none.
	SessionTitle *string
	WorkflowName string
	HeldAt       time.Time
}

// readAutonomyFreeze reads the freeze in force once, for one load, with no
// cache (§5.1, §40.2). unread is true when it could not be read -- a nil
// store included -- and the row is then the zero value, which reads not
// frozen: the caller reports unread so that a failed read never renders as
// "not frozen". A missing row reads as not frozen, as every site reads it.
func readAutonomyFreeze(ctx context.Context, deps Deps) (freeze sqlcgen.GetAutonomyFreezeRow, unread bool) {
	if deps.PlatformSettings == nil {
		platform.Logger(ctx).Warn("decisioninbox: no platform settings store wired; the autonomy freeze is reported unread")
		return sqlcgen.GetAutonomyFreezeRow{}, true
	}
	row, err := deps.PlatformSettings.GetAutonomyFreeze(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return sqlcgen.GetAutonomyFreezeRow{}, false
	case err != nil:
		platform.Logger(ctx).Error("decisioninbox: read the autonomy freeze failed; reported unread, never as not frozen", "error", err)
		return sqlcgen.GetAutonomyFreezeRow{}, true
	default:
		return row, false
	}
}

// markHeldByFreeze sets Item.HeldByFreeze on each ready_to_merge row whose
// repository has auto-merge armed, when frozen is true: the rows whose
// automatic merge the freeze holds (§40.2: "ready_to_merge items remain
// listed, marked as held"). A row in a repository without auto-merge armed
// is not held by anything -- it waits on a person, frozen or not -- so it
// carries no mark. Whether a repository is armed is read once per
// repository per load (appreviewverdict.AutoMergeEnabled, which reads a
// failed read as not armed, the worker's own fail-closed rule).
func markHeldByFreeze(ctx context.Context, deps Deps, items []Item, frozen bool) {
	if !frozen {
		return
	}
	armed := map[string]bool{}
	for i := range items {
		if items[i].Kind != decisioninbox.KindReadyToMerge {
			continue
		}
		repo := items[i].RepoFullName
		isArmed, ok := armed[repo]
		if !ok {
			isArmed = appreviewverdict.AutoMergeEnabled(ctx, deps.ReviewVerdict, repo)
			armed[repo] = isArmed
		}
		items[i].HeldByFreeze = isArmed
	}
}

// buildHeldWorkflowAdvances lists the workflow runs whose automatic advance
// the freeze holds, on the sessions whose workflow steps the actor may
// decide (authz.ActionDecideWorkflowStep): every session for an
// administrator or maintainer, those they created or joined for a member,
// none for a viewer. Oldest first, at most maxHeldWorkflowAdvances; total
// is how many there are, so a list cut at the bound says so. The
// holds are read whatever this load read of the freeze: a hold outlives the
// freeze until the releaser applies it, within AutonomyFreezeRecheckInterval
// of the unfreeze, and is listed until then. Best-effort, as the plan and
// attention rows are: a read that fails is logged and lists none.
//
// The scope is enforced twice, on purpose: the query reads only the
// sessions the actor created or joined unless they may decide every
// session's steps, so a member's own holds are never crowded out of the
// bound by others'; and authz.Authorize decides each row, as it does each
// plan row, so the matrix stays the one authority. Either alone keeps a
// member to their own sessions.
func buildHeldWorkflowAdvances(ctx context.Context, deps Deps, actorUserID pgtype.UUID, actorRole authz.Role, logger *slog.Logger) (held []HeldWorkflowAdvance, total int) {
	if deps.Workflows == nil {
		return nil, 0
	}
	actor := authz.Actor{UserID: actorUserID.String(), Role: actorRole}
	if authz.Authorize(actor, authz.ActionDecideWorkflowStep, authz.Resource{OwnedOrJoined: true}) != nil {
		return nil, 0
	}
	everySession := authz.Authorize(actor, authz.ActionDecideWorkflowStep, authz.Resource{}) == nil

	rows, err := deps.Workflows.ListAdvanceHoldsForInbox(ctx, actorUserID, everySession, maxHeldWorkflowAdvances)
	if err != nil {
		logger.Error("decisioninbox: list held workflow advances failed", "error", err)
		return nil, 0
	}
	held = make([]HeldWorkflowAdvance, 0, len(rows))
	for _, row := range rows {
		total = int(row.Total)
		if authz.Authorize(actor, authz.ActionDecideWorkflowStep, authz.Resource{OwnedOrJoined: row.OwnedOrJoined}) != nil {
			continue
		}
		held = append(held, HeldWorkflowAdvance{
			WorkflowRunID: row.WorkflowRunID.String(),
			SessionID:     row.SessionID.String(),
			SessionTitle:  row.SessionTitle,
			WorkflowName:  row.WorkflowName,
			HeldAt:        row.HeldAt.Time,
		})
	}
	return held, total
}
