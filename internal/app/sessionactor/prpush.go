// This file (prpush.go) holds the ONE write that records a push to a pull
// request's head for §24's automatic re-review, and the session actor's
// own call to it when the session's own push moves that head (technical
// plan §43.20).
//
// A pull request's review session pushes its own work: its repos[].branch
// is the pull request's head ref (internal/adapters/inbound/github's
// handler builds it from the head), so a turn that commits moves that
// head when completeProcessingTurn's push lands. GitHub then delivers
// pull_request/synchronize for that push -- the server's own output coming
// back as input -- and that webhook arms review_retrigger_debounce on the
// SAME session, which inserts a review turn with no person acting. The
// status must not read settled in between, so the server records that
// work at the moment it creates its cause: in the transaction that
// persists the push's push_complete, before createPRBestEffort clears the
// delivery stamp, it performs the very write the webhook will perform. The
// webhook, when it lands, repeats it: the same head, the debounce re-armed
// from its own later instant -- §24's "2 minutes after the last push".

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/contracts/gen/go/sessionconfig"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/platform"
)

// RecordPullRequestPush is the one write that records headSHA as the new
// head of repoFullName#prNumber for §24's automatic re-review, in tx:
// github_pr_sessions.pending_retrigger_head_sha is set to headSHA, and the
// session's review_retrigger_debounce timer is armed (or re-armed)
// ReviewRetriggerDebounce after now -- unless a turn of that session was
// already created against headSHA (SessionHasTurnForReviewHead): a review
// of that head is then already queued, running or done, and a second one
// would review the same commits again. Both callers use it, so neither
// holds a copy of this rule: the pull_request/synchronize webhook
// (internal/adapters/inbound/github), and the session actor when its own
// push moves the pull request's head (recordOwnPullRequestPush below).
//
// It returns the session and whether the debounce was armed.
// pgx.ErrNoRows (unwrapped) means no review session backs that pull
// request -- no row, or a row whose session_id is still NULL -- and
// nothing was written.
func RecordPullRequestPush(ctx context.Context, tx pgx.Tx, prSessions *postgres.GitHubPRSessionStore, timers *postgres.TimerStore, timeouts platform.Timeouts,
	repoFullName string, prNumber int32, headSHA string, now time.Time,
) (sessionID pgtype.UUID, armed bool, err error) {
	row, err := prSessions.WithTx(tx).UpsertPendingRetriggerHeadSHA(ctx, repoFullName, prNumber, headSHA)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return pgtype.UUID{}, false, err
		}
		return pgtype.UUID{}, false, fmt.Errorf("upsert pending retrigger head sha: %w", err)
	}
	reviewed, err := prSessions.WithTx(tx).SessionHasTurnForReviewHead(ctx, row.SessionID, headSHA)
	if err != nil {
		return row.SessionID, false, fmt.Errorf("look up a turn for the pushed head: %w", err)
	}
	if reviewed {
		return row.SessionID, false, nil
	}
	// §24.2: the trailing-edge debounce -- the SAME upsert-on-UNIQUE(
	// session_id, name) every named timer uses, so a later push before it
	// fires pushes fires_at further out.
	if _, err := timers.WithTx(tx).Upsert(ctx, sqlcgen.UpsertSessionTimerParams{
		SessionID: row.SessionID,
		Name:      TimerReviewRetriggerDebounce,
		FiresAt:   pgtype.Timestamptz{Time: now.Add(timeouts.ReviewRetriggerDebounce), Valid: true},
	}); err != nil {
		return row.SessionID, false, fmt.Errorf("arm review_retrigger_debounce: %w", err)
	}
	return row.SessionID, true, nil
}

// recordOwnPullRequestPush runs in the transaction that persists a
// push_complete (handleSandboxEvent), for a first delivery only. When this
// session backs a pull request and the push moved that pull request's own
// head, it records the push through RecordPullRequestPush -- the write the
// pull_request/synchronize webhook will repeat -- so the automatic
// re-review the push causes is armed, and the status reads it as
// scheduled, from this commit on: before createPRBestEffort clears the
// delivery stamp, so no snapshot shows the session settled in between.
//
// A pushed repository moves the head only when it is the pull request's
// own repository (owner/repo of its clone URL equal to the pull request's
// repo_full_name) on the session's branch, which is the head ref. A fork
// pull request's head lives in the fork, which this never pre-arms: the
// webhook alone arms it if such a push reaches the head. The pushed sha is
// the one push_complete reports -- git's own head after the push -- and
// git reports it whether or not the push moved anything, so a head the
// server already knows (IsKnownPullRequestHead: the pending head, a posted
// verdict's head, a head one of the session's turns was created against)
// means the turn committed nothing new: no synchronize will come, and
// nothing is armed.
func (a *Actor) recordOwnPullRequestPush(ctx context.Context, tx pgx.Tx, raw json.RawMessage, now time.Time) error {
	if a.stores.githubPRSession == nil {
		return nil
	}
	prSession, err := a.stores.githubPRSession.WithTx(tx).GetBySessionID(ctx, a.sessionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("sessionactor: get github pr session for a completed push: %w", err)
	}
	var evt sandboxws.PushComplete
	if err := json.Unmarshal(raw, &evt); err != nil {
		a.logger.Warn("sessionactor: decode push_complete to record a pull request push failed", "error", err)
		return nil
	}
	sessionRow, err := a.stores.session.WithTx(tx).Get(ctx, a.sessionID)
	if err != nil {
		return fmt.Errorf("sessionactor: get session for a completed push: %w", err)
	}
	repos, err := reposFromJSON(sessionRow.Repos)
	if err != nil {
		a.logger.Warn("sessionactor: parse session repos to record a pull request push failed", "error", err)
		return nil
	}

	for _, pushed := range evt.Repos {
		if !pushTargetsPullRequestHead(prSession.RepoFullName, repos, pushed) {
			continue
		}
		known, err := a.stores.githubPRSession.WithTx(tx).IsKnownPullRequestHead(ctx, prSession.RepoFullName, prSession.PrNumber, a.sessionID, pushed.Sha)
		if err != nil {
			return fmt.Errorf("sessionactor: look up the pushed head: %w", err)
		}
		if known {
			continue
		}
		if _, _, err := RecordPullRequestPush(ctx, tx, a.stores.githubPRSession, a.stores.timer, a.timeouts, prSession.RepoFullName, prSession.PrNumber, pushed.Sha, now); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return fmt.Errorf("sessionactor: record the session's own pull request push: %w", err)
		}
	}
	return nil
}

// pushTargetsPullRequestHead reports whether pushed -- one repository of a
// push_complete -- went to the pull request prRepoFullName's own head: a
// repository of this session, on a GitHub URL whose owner/repo is the pull
// request's (not a fork), pushed on the session's own branch for it (the
// head ref), with a sha to record.
func pushTargetsPullRequestHead(prRepoFullName string, repos []sessionconfig.SessionConfigReposElem, pushed sandboxws.PushCompleteReposElem) bool {
	if pushed.Sha == "" {
		return false
	}
	for _, r := range repos {
		if r.Name != pushed.Name {
			continue
		}
		if r.Branch == nil || *r.Branch != pushed.Branch {
			return false
		}
		if reposource.CheckRepoHost(r.Url, ports.SupportedSourceControlHosts()...) != nil {
			return false
		}
		owner, repo, err := reposource.ParseOwnerRepo(r.Url)
		if err != nil {
			return false
		}
		return strings.EqualFold(owner+"/"+repo, prRepoFullName)
	}
	return false
}
