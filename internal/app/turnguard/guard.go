// Package turnguard is the application half of the session guard
// (internal/domain/sessionguard, technical plan §40.1): it reads a
// session's facts in the caller's transaction, under the session's row
// lock, asks the domain's Decide, and records a refusal -- one persisted
// warning per crossing and, when that warning is new, one outbox notice.
//
// It is the only caller of sessionguard.Decide outside the domain package
// (a source scan beside postgres's turn_insert_test.go keeps it so), and so
// the only place an Admission for an existing session is minted. Every
// turn production code creates passes it: createTurnLocked and the plan
// approval (internal/adapters/inbound/httpapi), the workflow engine's
// advance and a person's revision (internal/app/workflowengine), the
// automatic re-review and an owed review request's re-run
// (internal/app/sessionactor), the release composition review
// (internal/app/releasereview); and the session actor asks it again for
// every queued turn about to be dispatched. The first turn of a session
// created in the same transaction is admitted by
// sessionguard.AdmitNewSession instead.
//
// The cap applies to every turn on the session regardless of who asked for
// it -- §40.1's inversion of §24.6: a person's prompt, a plan approval and
// the review button are refused like every automatic producer, and the
// human's remedy is the audited raise of the cap, never a bypass.
package turnguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/reposource"
	"github.com/narvidev/narvi/internal/domain/sessionguard"
	"github.com/narvidev/narvi/internal/platform"
)

// Stage is where a refusal was made: the stage attribute of
// session_guard_refused_total.
type Stage string

// The stages, one a site that asks the guard.
const (
	// StageCreate: createTurnLocked, every person's and bot's new turn.
	StageCreate Stage = "create"
	// StagePlanApproval: approving a plan, which creates its
	// implementation turn.
	StagePlanApproval Stage = "plan_approval"
	// StageWorkflowAdvance: the workflow engine's next attempt, by its own
	// advance or a person's decision.
	StageWorkflowAdvance Stage = "workflow_advance"
	// StageAutoRetrigger: the automatic re-review's debounce firing.
	StageAutoRetrigger Stage = "auto_retrigger"
	// StageOwedRequest: an owed review request's re-run.
	StageOwedRequest Stage = "owed_request"
	// StageComposition: the release composition review.
	StageComposition Stage = "composition"
	// StageDispatch: a queued turn about to be dispatched.
	StageDispatch Stage = "dispatch"
)

// meterName is this package's OTel meter.
const meterName = "narvi/turnguard"

// refusedTotal is session_guard_refused_total, resolved lazily: the guard
// is built in several places (the control plane's composition root, the
// session actor's registry, tests), and one instrument serves them all.
var refusedTotal = sync.OnceValue(func() metric.Int64Counter {
	c, err := otel.Meter(meterName).Int64Counter(
		"session_guard_refused_total",
		metric.WithDescription("Count of every turn the session guard refused (technical plan §40.1): a new turn not created, or a queued turn ended before its dispatch, because the session reached its spend cap. Tagged by reason (\"spend_cap\"), by stage -- \"create\" (a person's or bot's new turn, createTurnLocked), \"plan_approval\", \"workflow_advance\", \"auto_retrigger\", \"owed_request\", \"composition\" (the release composition review) and \"dispatch\" (a queued turn ended as it was about to be dispatched, counted once a dispatch evaluation however many queued turns it ended) -- and by the session's spawn_source. Every refusal of one crossing is counted, while the session records one warning and sends one notice for it. A guard read that failed is never counted: it is not a refusal."),
		metric.WithUnit("{refusal}"),
	)
	if err != nil {
		platform.Logger(context.Background()).Error("turnguard: construct session_guard_refused_total counter failed", "error", err)
	}
	return c
})

// countRefusal adds one to session_guard_refused_total.
func countRefusal(ctx context.Context, r *sessionguard.Refusal, stage Stage, spawnSource string) {
	c := refusedTotal()
	if c == nil {
		return
	}
	c.Add(ctx, 1, metric.WithAttributes(
		attribute.String("reason", string(r.Reason)),
		attribute.String("stage", string(stage)),
		attribute.String("spawn_source", spawnSource),
	))
}

// Guard reads a session's facts and decides; built once, on the pool, and
// bound to a caller's transaction for each use.
type Guard struct {
	pool        *pgxpool.Pool
	sessions    *postgres.SessionStore
	facts       *postgres.SessionGuardStore
	sandboxes   *postgres.SandboxStore
	events      *postgres.EventStore
	slack       *postgres.SlackThreadSessionStore
	linear      *postgres.LinearAgentSessionStore
	github      *postgres.GitHubPRSessionStore
	outbox      *postgres.OutboxStore
	broadcaster ports.EventBroadcaster
	// noticeHold is how long a refusal's notice is held for the caller's
	// own reply on the session's channel (AnsweredOnChannel).
	noticeHold time.Duration
}

// New builds a Guard on pool. broadcaster delivers the warning
// RecordRefusal records to the session's live subscribers; nil delivers it
// to nobody, and the warning is still stored. platformShadow is the
// deployment's shadow flag, which the outbox store needs to suppress a
// customer-visible notice in shadow (postgres.NewOutboxStore).
func New(pool *pgxpool.Pool, broadcaster ports.EventBroadcaster, platformShadow bool) *Guard {
	return &Guard{
		pool:        pool,
		sessions:    postgres.NewSessionStore(pool),
		facts:       postgres.NewSessionGuardStore(pool),
		sandboxes:   postgres.NewSandboxStore(pool),
		events:      postgres.NewEventStore(pool),
		slack:       postgres.NewSlackThreadSessionStore(pool),
		linear:      postgres.NewLinearAgentSessionStore(pool),
		github:      postgres.NewGitHubPRSessionStore(pool),
		outbox:      postgres.NewOutboxStore(pool, platformShadow),
		broadcaster: broadcaster,
		noticeHold:  platform.DefaultTimeouts().SessionGuardNoticeHold,
	}
}

// WithNoticeHold sets how long g holds a refusal's notice while the caller
// replies on the session's own channel (AnsweredOnChannel) -- the
// deployment's platform.Timeouts.SessionGuardNoticeHold -- and returns g.
// A Guard built by New holds it for the default.
func (g *Guard) WithNoticeHold(hold time.Duration) *Guard {
	if g != nil {
		g.noticeHold = hold
	}
	return g
}

// ErrNoGuard is the answer of a nil Guard: no admission without a guard.
var ErrNoGuard = errors.New("turnguard: no session guard is configured")

// Admit is the guard's decision for one turn of sessionID, read in tx: the
// caller's transaction, which must already hold the session's row lock
// (SessionStore.GetActorEpochForUpdate) -- every actor transaction takes it
// first, and every turn insert on an existing session takes it too, so a
// step cost committed before the lock is read here and one written after it
// waits. It reads the session row, resolves the repositories its clone
// URLs name, reads the facts (postgres.SessionGuardStore.Facts) and asks
// sessionguard.Decide. A refusal is counted under stage; a read that failed
// is returned as an error, never as a refusal and never as an admission.
// pgx.ErrNoRows when the session is gone.
func (g *Guard) Admit(ctx context.Context, tx pgx.Tx, sessionID pgtype.UUID, origin sessionguard.Origin, stage Stage) (sessionguard.Admission, *sessionguard.Refusal, error) {
	if g == nil {
		return sessionguard.Admission{}, nil, ErrNoGuard
	}
	sessionRow, err := g.sessions.WithTx(tx).Get(ctx, sessionID)
	if err != nil {
		return sessionguard.Admission{}, nil, fmt.Errorf("turnguard: read the session: %w", err)
	}
	facts, err := g.facts.WithTx(tx).Facts(ctx, sessionID, RepoFullNames(ctx, sessionRow))
	if err != nil {
		return sessionguard.Admission{}, nil, fmt.Errorf("turnguard: read the session's spend and caps: %w", err)
	}
	admission, refusal := sessionguard.Decide(facts, origin)
	if refusal != nil {
		countRefusal(ctx, refusal, stage, string(sessionRow.SpawnSource))
		platform.Logger(ctx).Info("turnguard: a turn was refused: the session has reached its spend cap",
			"session_id", sessionID.String(), "reason", string(refusal.Reason), "stage", string(stage), "origin", origin.String(),
			"spent_usd", refusal.Spent.String(), "cap_usd", refusal.Cap.String(),
			"cap_source", refusal.Source.Kind, "cap_source_name", refusal.Source.Name)
	}
	return admission, refusal, nil
}

// Admitter is the admission postgres.LockedTurnCreator.CreateLockedTurn
// runs in its own transaction, after the session's row lock: Admit for
// sessionID, with a refusal returned as its error.
func (g *Guard) Admitter(sessionID pgtype.UUID, origin sessionguard.Origin, stage Stage) postgres.TurnAdmit {
	return func(ctx context.Context, tx pgx.Tx) (sessionguard.Admission, error) {
		admission, refusal, err := g.Admit(ctx, tx, sessionID, origin, stage)
		if err != nil {
			return sessionguard.Admission{}, err
		}
		if refusal != nil {
			return sessionguard.Admission{}, refusal
		}
		return admission, nil
	}
}

// RepoFullNames is the owner/name of every repository sessionRow's clone
// URLs name, resolved as the rollout and entitlement re-checks resolve
// them (reposource.CheckRepoHost, then ParseOwnerRepo): a URL that resolves
// to no trusted owner/repo can match no cap and is skipped. A malformed
// repos column contributes no names, and the session's pull-request claims
// are still read beside them (GetSessionGuardFacts).
func RepoFullNames(ctx context.Context, sessionRow sqlcgen.Session) []string {
	var repos []struct {
		URL string `json:"url"`
	}
	if len(sessionRow.Repos) > 0 {
		if err := json.Unmarshal(sessionRow.Repos, &repos); err != nil {
			platform.Logger(ctx).Warn("turnguard: unmarshal session repos failed; reading the session's pull-request claims only",
				"session_id", sessionRow.ID.String(), "error", err)
			repos = nil
		}
	}
	names := make([]string, 0, len(repos))
	for _, r := range repos {
		if err := reposource.CheckRepoHost(r.URL, ports.SupportedSourceControlHosts()...); err != nil {
			continue
		}
		owner, name, err := reposource.ParseOwnerRepo(r.URL)
		if err != nil {
			continue
		}
		names = append(names, owner+"/"+name)
	}
	return names
}

// OriginForRequest is the origin of a turn a request asks for: a person's
// act when an authenticated person asked (actorUserID valid) and not
// through an MCP client's grant; automatic otherwise -- a bot-attributed
// caller, or an MCP-driven one.
func OriginForRequest(ctx context.Context, actorUserID pgtype.UUID) sessionguard.Origin {
	if !actorUserID.Valid {
		return sessionguard.OriginAutomatic
	}
	if _, ok := platform.MCPGrantFromContext(ctx); ok {
		return sessionguard.OriginAutomatic
	}
	return sessionguard.OriginPerson
}
