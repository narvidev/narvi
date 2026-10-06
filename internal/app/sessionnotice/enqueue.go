// Package sessionnotice enqueues one already-rendered, human-readable
// notice about a session to the channel the session came from: its Slack
// thread, its Linear agent session, or its GitHub pull request, resolved
// from the session's spawn_source and its own reverse-lookup row. It is
// the one router for every notice of that shape -- a workflow step awaiting
// a decision or a run escalated (internal/app/workflowengine, technical
// plan §25.9), and the session guard's refusal (internal/app/turnguard,
// §40.1) -- each caller naming the three outbox kinds its notice travels
// under, so each keeps its own delivery classification.
//
// The payloads are the three plain shapes the existing notifiers already
// consume (slackapi.Payload, linearapi.Payload, githubapi.Payload). A
// 'web'- or 'mcp'-origin session enqueues nothing and logs nothing:
// neither has an external channel (the browser re-reads the session, and an
// MCP client polls its status or waits on it, §43.20). A bot-origin session
// missing its reverse-lookup row also enqueues nothing -- logged, never an
// error: a notice that cannot be routed must never undo or block the state
// change it reports, which commits in the same transaction.
package sessionnotice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/linearapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/platform"
)

// Stores is what Enqueue reads and writes, every store bound to the
// caller's transaction (store.WithTx(tx)), so the notice commits with the
// state change it reports (§5.1).
type Stores struct {
	SlackThreadSessions *postgres.SlackThreadSessionStore
	LinearAgentSessions *postgres.LinearAgentSessionStore
	GitHubPRSessions    *postgres.GitHubPRSessionStore
	Outbox              *postgres.OutboxStore
}

// Kinds are the outbox kinds a caller's notice travels under, one a
// channel.
type Kinds struct {
	Slack  ports.NotificationKind
	Linear ports.NotificationKind
	GitHub ports.NotificationKind
}

// Enqueue enqueues one outbox row carrying text to sessionRow's own
// channel, under the kind kinds names for it, and reports whether it did.
// No destination -- a 'web'- or 'mcp'-origin session, a source this binary
// has no channel for, a missing reverse-lookup row -- is (false, nil). Only
// a store failure is an error: a lookup that failed rather than found
// nothing, or the outbox insert.
func Enqueue(ctx context.Context, stores Stores, sessionRow sqlcgen.Session, kinds Kinds, text string) (bool, error) {
	_, enqueued, err := EnqueueEntry(ctx, stores, sessionRow, kinds, text)
	return enqueued, err
}

// EnqueueEntry is Enqueue that also returns the outbox row it created, for
// a caller that acts on the row in the same transaction (the session
// guard's held notice, internal/app/turnguard). The row is the zero value
// when nothing was enqueued.
func EnqueueEntry(ctx context.Context, stores Stores, sessionRow sqlcgen.Session, kinds Kinds, text string) (sqlcgen.Outbox, bool, error) {
	logger := platform.Logger(ctx)

	if sessionRow.SpawnSource == sqlcgen.SessionSpawnSourceWeb || sessionRow.SpawnSource == sqlcgen.SessionSpawnSourceMcp {
		return sqlcgen.Outbox{}, false, nil
	}

	var kind ports.NotificationKind
	var payload any

	switch sessionRow.SpawnSource {
	case sqlcgen.SessionSpawnSourceSlack:
		row, err := stores.SlackThreadSessions.GetBySessionID(ctx, sessionRow.ID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				logger.Warn("sessionnotice: slack-origin session has no slack_thread_sessions row; skipping", "kind", string(kinds.Slack))
				return sqlcgen.Outbox{}, false, nil
			}
			return sqlcgen.Outbox{}, false, fmt.Errorf("sessionnotice: get slack thread session: %w", err)
		}
		kind = kinds.Slack
		payload = slackapi.Payload{ChannelID: row.ChannelID, ThreadTS: row.ThreadTs, Text: text}

	case sqlcgen.SessionSpawnSourceLinear:
		row, err := stores.LinearAgentSessions.GetBySessionID(ctx, sessionRow.ID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				logger.Warn("sessionnotice: linear-origin session has no linear_agent_sessions row; skipping", "kind", string(kinds.Linear))
				return sqlcgen.Outbox{}, false, nil
			}
			return sqlcgen.Outbox{}, false, fmt.Errorf("sessionnotice: get linear agent session: %w", err)
		}
		kind = kinds.Linear
		payload = linearapi.Payload{AgentSessionID: row.AgentSessionID, OrganizationID: row.OrganizationID, Text: text, Success: true}

	case sqlcgen.SessionSpawnSourceGithub:
		row, err := stores.GitHubPRSessions.GetBySessionID(ctx, sessionRow.ID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				logger.Warn("sessionnotice: github-origin session has no github_pr_sessions row; skipping", "kind", string(kinds.GitHub))
				return sqlcgen.Outbox{}, false, nil
			}
			return sqlcgen.Outbox{}, false, fmt.Errorf("sessionnotice: get github pr session: %w", err)
		}
		owner, repo, ok := reposource.SplitFullName(row.RepoFullName)
		if !ok {
			logger.Warn("sessionnotice: could not split repo_full_name; skipping", "repo_full_name", row.RepoFullName, "kind", string(kinds.GitHub))
			return sqlcgen.Outbox{}, false, nil
		}
		kind = kinds.GitHub
		payload = githubapi.Payload{Owner: owner, Repo: repo, PRNumber: int(row.PrNumber), Text: text}

	default:
		// A source this binary has no channel for. Session.spawnSource is
		// an open enum (contracts/manifest.json's openEnums), so a newer
		// migration can add a value that an older binary, still serving
		// during a rolling deploy, reads here. With no channel to notify,
		// nothing is enqueued; the source is logged so the gap shows.
		logger.Warn("sessionnotice: unrecognized spawn_source; skipping", "spawn_source", string(sessionRow.SpawnSource))
		return sqlcgen.Outbox{}, false, nil
	}

	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return sqlcgen.Outbox{}, false, fmt.Errorf("sessionnotice: marshal notice payload: %w", err)
	}

	var correlationID *string
	if id, ok := platform.CorrelationIDFromContext(ctx); ok && id != "" {
		correlationID = &id
	}

	row, err := stores.Outbox.Create(ctx, sqlcgen.CreateOutboxEntryParams{
		SessionID:     sessionRow.ID,
		Kind:          string(kind),
		Payload:       rawPayload,
		CorrelationID: correlationID,
	})
	if err != nil {
		return sqlcgen.Outbox{}, false, fmt.Errorf("sessionnotice: create notice outbox entry: %w", err)
	}
	return row, true, nil
}
