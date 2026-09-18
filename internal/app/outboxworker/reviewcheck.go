package outboxworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/narvidev/narvi/internal/adapters/outbound/githubapi"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
	"github.com/narvidev/narvi/internal/platform"
)

// This file implements the review's own GitHub-native result surface
// (§8.2/§21.1/§21.1b) publisher: ports.
// NotificationKindGitHubReviewCheck's own real Deliver. The full identity
// design lives in three places, each with its own doc comment this one
// only summarizes:
//
//   - internal/domain/reviewcheck: the pure state mapping (Phase ->
//     GitHub status/conclusion, ComputeOutput) and the write-conflict
//     ordering (Supersedes).
//   - migrations/000132_review_check_runs.up.sql / internal/adapters/
//     outbound/postgres.ReviewCheckRunStore: the per-PR claim table and
//     its atomic Ensure+Lock+Update sequencing.
//   - internal/adapters/outbound/githubapi's own checkruns.go: the raw
//     GitHub Checks API calls.
//
// Deliver below is the ONE place all three compose: claim (Postgres
// only, lock held briefly) -> release the lock -> call GitHub (no
// transaction open, mirroring ports.Notifier.Deliver's own "always
// outside any tx" contract) -> record the result with a guarded,
// optimistic-concurrency write (no lock re-acquired -- see
// SetReviewCheckRunExternalID's own generated doc comment for the
// accepted residual this trades for never holding a Postgres transaction
// across a network call).
type reviewCheckNotifier struct {
	pool     *pgxpool.Pool
	store    *postgres.ReviewCheckRunStore
	adapter  *githubapi.Adapter
	botToken string
	// writerAppID (finding A2) is this deployment's own SELF-OBSERVED
	// writer App id -- the second half of "select by SHA and GitHub
	// App": a recovery list-check-runs read (below) adopts only a check
	// run whose own reported app.id matches this value, never merely
	// one sharing reviewcheck.CheckName.
	//
	// Deliberately NOT a caller-supplied config value (this type used to
	// take one, platform.Config.GitHubAppID) -- that field is §30.4's
	// OWN, entirely separate GitHub App: the read-only installation-
	// token-minting credential internal/adapters/outbound/githubapp.
	// Client uses for shadow-mode substitution, never the credential
	// n.botToken actually carries. A deployment's bot token is
	// documented (platform.Config's own GitHubBotToken doc comment) as
	// "a real GitHub personal access token OR a GitHub App installation
	// token, whichever the deploying operator provisions" -- there is no
	// config field anywhere in this codebase naming THAT credential's
	// own App id, and platform.Config.GitHubAppID asserting it anyway
	// was exactly this finding's own defect: comparing against an App id
	// that has nothing to do with the credential that actually creates
	// the check run, so the adoption filter this comparison backs could
	// never genuinely recognize a check run this notifier itself wrote.
	//
	// Self-learned instead: recordWriterAppID (below) captures the REAL
	// value GitHub itself reports, from this notifier's own successful
	// CreateCheckRun response -- the only way to know for certain what
	// identity a given credential's write is attributed to is to make
	// that credential perform a write and read back what GitHub says
	// about it. 0 means "not yet observed" -- see resolveOrCreateCheckRun's
	// own doc comment for how that state degrades (never adopts, always
	// creates -- safe, never a false-positive match).
	//
	// An atomic.Int64, but NOT the sole record any more (finding B2): a
	// bare in-process cache with no durable backing IS "a cache with
	// authority" (§5.1 forbids exactly this) the moment it alone decides
	// whether a crash-recovery adoption can succeed -- and a fresh
	// process (a restart, a new pod in a multi-pod fleet) is EXACTLY the
	// case adoption exists for, yet used to start back at "not yet
	// observed" every single time, unable to adopt its own in-flight run
	// until its own first successful create in that process's lifetime.
	// recordWriterAppID/observedWriterAppID (below) now also read/write
	// ReviewCheckRunStore's own durable row (migrations/
	// 000134_review_check_writer_app_id.up.sql) -- this field remains a
	// genuine, authority-free memoization on top of that (avoiding a DB
	// round trip on every call once a value is known), never the only
	// place the fact lives. Residual this does NOT close, stated
	// precisely rather than left implied: the very FIRST check-run
	// creation this deployment EVER makes, deployment-wide, still races
	// unrecovered (nothing has been persisted yet to fall back to) --
	// every subsequent crash, for any PR, recovers correctly once that
	// row exists.
	//
	// C7: a SECOND residual this durability does not close, stated for
	// the same reason -- a credential rotation that moves this
	// deployment's own bot token to a DIFFERENT GitHub App's installation
	// (not merely a new token for the SAME App) leaves this row naming
	// the OLD App id: nothing here observes a rotation happening, only a
	// SUCCESSFUL CreateCheckRun's own response, and a rotated credential
	// keeps succeeding, just attributed to a different App. Between the
	// rotation and this deployment's own next genuine CreateCheckRun (the
	// only event that re-teaches this value, recordWriterAppID above),
	// every cold process reads the STALE, pre-rotation App id back from
	// this durable row and may adopt (PATCH, via the NEW credential) a
	// check run GitHub attributes to the OLD App -- for the CORRECT pull
	// request (finding C1's own per-PR discriminator still applies), so
	// this is not the cross-PR collapse C1 fixes, but it is still
	// publishing under an App id this deployment's own CURRENT credential
	// does not actually own, and if GitHub's Checks API itself refuses a
	// cross-App update, every retry re-selects the SAME stale candidate
	// and fails the SAME way -- wedged, not merely delayed, until
	// something clears this row.
	//
	// Deliberately NOT auto-invalidated here: telling "a rotation to a
	// different App" apart from "a transient PATCH failure for any other
	// reason" from the adopt error alone is not reliable enough to act on
	// automatically without risking the opposite mistake (discarding a
	// good, durably-learned App id on an ordinary transient failure,
	// which would then race the deployment-wide unrecovered-first-create
	// window above on every such hiccup). Stated as an operator residual
	// instead: after rotating this deployment's own bot credential to a
	// DIFFERENT GitHub App, an operator clears this row (`DELETE FROM
	// review_check_writer_app_id;`) AND restarts every running process
	// (a durable-row delete alone does not reach an already-running
	// process's own in-process writerAppID atomic, checked first,
	// observedWriterAppID's own doc comment) -- every process then starts
	// back at "not yet observed" and self-relearns the new credential's
	// real App id from its own next successful create, the same safe
	// degradation the deployment-wide first-ever-create race above
	// already relies on.
	writerAppID atomic.Int64
}

// NewReviewCheckNotifier builds a ports.Notifier for
// ports.NotificationKindGitHubReviewCheck.
func NewReviewCheckNotifier(pool *pgxpool.Pool, store *postgres.ReviewCheckRunStore, adapter *githubapi.Adapter, botToken string) ports.Notifier {
	return &reviewCheckNotifier{pool: pool, store: store, adapter: adapter, botToken: botToken}
}

var _ ports.Notifier = (*reviewCheckNotifier)(nil)

// recordWriterAppID stores id as this notifier's own self-observed
// writer App id (finding A2) -- called after every successful
// CreateCheckRun response. Never overwrites a real, already-observed
// value with 0 (id == 0 means the create response carried no app object
// at all -- should not happen for a genuine GitHub App installation
// token, but defended rather than trusted).
//
// Finding B2: also persists id durably via ReviewCheckRunStore.
// SetWriterAppID, so a FUTURE process (a restart, a new pod) can recover
// it without first re-observing it -- best-effort: a failure to persist
// is logged, never propagated as this call's own error, since the
// ORIGINAL CreateCheckRun already succeeded and this process's own
// in-process cache is already correct regardless of whether the durable
// write lands.
func (n *reviewCheckNotifier) recordWriterAppID(ctx context.Context, id int64) {
	if id == 0 {
		return
	}
	n.writerAppID.Store(id)
	if err := n.store.SetWriterAppID(ctx, id); err != nil {
		platform.Logger(ctx).Warn("outboxworker: reviewCheckNotifier: could not durably persist this deployment's own observed writer app id; a future fresh process will not recover it until its own next successful create",
			"app_id", id, "error", err)
	}
}

// observedWriterAppID returns this notifier's own self-observed writer
// App id, or 0 ("not yet observed" -- see writerAppID's own doc comment).
//
// Finding B2: checks the in-process cache FIRST (avoiding a DB round
// trip once a value is known), and only when THIS process has never
// observed one itself, falls back to ReviewCheckRunStore.GetWriterAppID
// -- the durable record ANY process, including an earlier incarnation of
// this same one before a restart, may have already written. A read
// failure degrades to 0 (never adopt, always create -- the same safe
// direction every other "not yet observed" path already degrades to),
// logged rather than propagated: this is a recovery optimization, never
// a correctness requirement of Deliver's own claim/publish sequence.
func (n *reviewCheckNotifier) observedWriterAppID(ctx context.Context) int64 {
	if id := n.writerAppID.Load(); id != 0 {
		return id
	}
	id, err := n.store.GetWriterAppID(ctx)
	if err != nil {
		platform.Logger(ctx).Warn("outboxworker: reviewCheckNotifier: could not read this deployment's own durably-observed writer app id; degrading to never-adopt for this call", "error", err)
		return 0
	}
	if id != 0 {
		n.writerAppID.Store(id)
	}
	return id
}

// toEmission converts a (possibly placeholder, possibly real)
// review_check_runs row into a reviewcheck.Emission -- a placeholder row
// (EnsureReviewCheckRunRow's own fresh insert: head_sha == "", phase ==
// "") converts to the Emission{} zero value, which reviewcheck.
// Supersedes already treats as "nothing published yet, always lose".
func toEmission(repoFullName string, prNumber int32, headSHA string, attemptID pgtype.UUID, attemptCreatedAt pgtype.Timestamptz, phase string, baseRef, baseSHA *string, policyVersion int32) reviewcheck.Emission {
	e := reviewcheck.Emission{
		RepoFullName:  repoFullName,
		PRNumber:      prNumber,
		HeadSHA:       headSHA,
		Phase:         reviewcheck.Phase(phase),
		PolicyVersion: int(policyVersion),
	}
	if attemptID.Valid {
		e.AttemptID = attemptID.String()
	}
	if attemptCreatedAt.Valid {
		e.AttemptCreatedAt = attemptCreatedAt.Time
	}
	if baseRef != nil {
		e.BaseRef = *baseRef
	}
	if baseSHA != nil {
		e.BaseSHA = *baseSHA
	}
	return e
}

// parseAttemptID converts payload's own plain-string AttemptID back to
// pgtype.UUID -- "" (a PhaseQueued emission, internal/domain/reviewcheck.
// Emission's own doc comment on AttemptID) converts to the zero value
// (Valid == false), a genuine SQL NULL.
func parseAttemptID(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if s == "" {
		return u, nil
	}
	if err := u.Scan(s); err != nil {
		return u, fmt.Errorf("outboxworker: reviewCheckNotifier: parse attempt id %q: %w", s, err)
	}
	return u, nil
}

func nonEmptyPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Deliver implements ports.Notifier. See this file's own top doc comment
// for the full claim -> call -> record sequence.
func (n *reviewCheckNotifier) Deliver(ctx context.Context, notification ports.Notification) error {
	logger := platform.Logger(ctx)

	var payload ports.ReviewCheckPayload
	if err := json.Unmarshal(notification.Payload, &payload); err != nil {
		return fmt.Errorf("outboxworker: reviewCheckNotifier: decode payload: %w", err)
	}
	if payload.HeadSHA == "" {
		// Mirrors httpapi.PostReviewVerdict's own "no review head sha on
		// record, skipping" precedent: nothing to create or update
		// against without a commit SHA. Logged, never retried -- a
		// redelivery of the IDENTICAL payload would reach the identical
		// conclusion every time.
		logger.Warn("outboxworker: reviewCheckNotifier: emission carries no head sha, skipping", "repo", payload.Owner+"/"+payload.Repo, "pr_number", payload.PRNumber)
		return nil
	}
	candidatePhase := reviewcheck.Phase(payload.Phase)
	if !candidatePhase.Valid() {
		logger.Error("outboxworker: reviewCheckNotifier: emission carries an unrecognized phase, refusing to publish", "phase", payload.Phase, "repo", payload.Owner+"/"+payload.Repo, "pr_number", payload.PRNumber)
		return nil
	}

	attemptID, err := parseAttemptID(payload.AttemptID)
	if err != nil {
		return err
	}
	candidate := reviewcheck.Emission{
		RepoFullName:     payload.Owner + "/" + payload.Repo,
		PRNumber:         int32(payload.PRNumber),
		HeadSHA:          payload.HeadSHA,
		AttemptID:        payload.AttemptID,
		AttemptCreatedAt: payload.AttemptCreatedAt,
		Phase:            candidatePhase,
		BaseRef:          payload.BaseRef,
		BaseSHA:          payload.BaseSHA,
		PolicyVersion:    payload.PolicyVersion,
	}

	// --- Claim: Postgres only, lock held briefly, never across the
	// GitHub call below (migrations/000132's own doc comment). ---
	tx, err := n.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("outboxworker: reviewCheckNotifier: begin claim tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	txStore := n.store.WithTx(tx)
	if err := txStore.EnsureRow(ctx, payload.Owner+"/"+payload.Repo, int32(payload.PRNumber)); err != nil {
		return fmt.Errorf("outboxworker: reviewCheckNotifier: ensure claim row: %w", err)
	}
	row, err := txStore.LockForUpdate(ctx, payload.Owner+"/"+payload.Repo, int32(payload.PRNumber))
	if err != nil {
		return fmt.Errorf("outboxworker: reviewCheckNotifier: lock claim row: %w", err)
	}
	current := toEmission(row.RepoFullName, row.PrNumber, row.HeadSha, row.AttemptID, row.AttemptCreatedAt, row.Phase, row.BaseRef, row.BaseSha, row.PolicyVersion)

	if !reviewcheck.Supersedes(current, candidate) {
		// Refused, not an error -- §21.1b: "an emission carries the
		// attempt and context it was produced for, and is REFUSED when
		// either has been superseded." Rolling back is safe: this
		// transaction has made no durable change (EnsureRow is
		// idempotent; the lock is released on rollback).
		logger.Info("outboxworker: reviewCheckNotifier: emission superseded, refusing to publish",
			"repo", candidate.RepoFullName, "pr_number", candidate.PRNumber,
			"candidate_attempt", candidate.AttemptID, "current_attempt", current.AttemptID,
			"candidate_phase", candidate.Phase, "current_phase", current.Phase)
		return nil
	}

	// newIdentityNeeded decides whether the EXISTING external check run
	// (if any) may be updated in place, or whether this emission must
	// open a fresh one. Two, and only two, reasons ever force a fresh
	// identity:
	//
	//  1. The head sha itself changed -- a check run is scoped to one
	//     commit (this package's own doc comment on why); the old
	//     external_id belongs to a SHA this row no longer targets.
	//  2. The CURRENT row is already terminal-shaped (Stale/
	//     TerminalAssessed/TerminalNotAssessed) AND candidate belongs to
	//     a DIFFERENT attempt -- "open a NEW check when a review
	//     restarts after a terminal result... never reopen a concluded
	//     one" (the brief's own identity rule). Reaching this branch
	//     already means Supersedes accepted candidate (above), so
	//     candidate is a genuinely newer attempt, not a stray/stale one.
	//
	// Neither condition fires for an ordinary same-attempt phase
	// progression (Running -> Terminal share one AttemptID -- PhaseQueued
	// carries NONE, by design: Emission.AttemptID's own doc comment,
	// "empty for PhaseQueued only... no attempt exists yet when a pull
	// request merely enters scope", so a queued row can never BE the
	// same attempt as anything and never reaches this branch as
	// `current` at all) or for a same-attempt PhaseStale flip: those
	// correctly keep updating the SAME external check run.
	newIdentityNeeded := row.HeadSha != "" && (row.HeadSha != candidate.HeadSHA ||
		(current.Phase.Terminal() && candidate.AttemptID != current.AttemptID))
	var existingExternalID *int64
	if newIdentityNeeded {
		if err := txStore.ClearExternalID(ctx, payload.Owner+"/"+payload.Repo, int32(payload.PRNumber)); err != nil {
			return fmt.Errorf("outboxworker: reviewCheckNotifier: clear stale external id: %w", err)
		}
	} else {
		existingExternalID = row.ExternalID
	}

	attemptCreatedAt := pgtype.Timestamptz{Time: candidate.AttemptCreatedAt, Valid: !candidate.AttemptCreatedAt.IsZero()}
	if _, err := txStore.UpdatePublished(ctx, payload.Owner+"/"+payload.Repo, int32(payload.PRNumber), candidate.HeadSHA, attemptID, attemptCreatedAt, string(candidate.Phase), nonEmptyPtr(candidate.BaseRef), nonEmptyPtr(candidate.BaseSHA), int32(candidate.PolicyVersion)); err != nil {
		return fmt.Errorf("outboxworker: reviewCheckNotifier: update published emission: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("outboxworker: reviewCheckNotifier: commit claim tx: %w", err)
	}
	committed = true

	// --- Call: GitHub, with no Postgres transaction open. ---
	output := reviewcheck.ComputeOutput(candidate.Phase)

	if existingExternalID != nil {
		if err := n.adapter.UpdateCheckRun(ctx, payload.Owner, payload.Repo, n.botToken, *existingExternalID, string(output.Status), string(output.Conclusion), output.Title, output.Summary); err != nil {
			return n.classifyAndWrap(logger, "update", err)
		}
		// finding A3: this call had no Postgres transaction open across
		// it (by design -- ports.Notifier.Deliver's own contract) and no
		// re-check that candidate is STILL the row's current emission
		// once the network call returns. A second Deliver call for the
		// SAME identity (a faster, concurrently-racing phase progression
		// -- both correctly claimed the row in order, Postgres's row
		// lock serializes THAT, but not the order their two independent
		// GitHub calls actually complete in) can finish its OWN,
		// genuinely newer write to GitHub BEFORE this call's slower one
		// lands, leaving GitHub showing THIS call's now-stale output even
		// though Postgres's own row has already moved on. See
		// guardAgainstSupersessionDuringCall's own doc comment for the
		// self-heal this performs.
		n.guardAgainstSupersessionDuringCall(ctx, logger, payload.Owner, payload.Repo, int32(payload.PRNumber), candidate, *existingExternalID)
		return nil
	}

	// No existing external id recorded locally: either this is genuinely
	// the first emission for this head sha, or an earlier delivery
	// attempt created the check run on GitHub but crashed OR is still
	// in flight (its own GitHub call has not yet reached SetExternalID).
	// The claim-row lock (LockForUpdate, above) serializes the two
	// attempts' own CLAIM decisions -- it cannot also serialize their
	// GitHub calls, which must run outside any transaction
	// (ports.Notifier.Deliver's own contract) -- so two attempts sharing
	// one head sha, racing closely enough, can each independently reach
	// this branch and each call CreateCheckRun below. Confirmed
	// empirically (reviewcheck_integration_test.go's own
	// TestReviewCheckNotifier_ConcurrentAttempts_ResolveToOneIdentity,
	// run repeated): the LOSING attempt's own check run becomes an
	// orphan nothing ever references again once its own SetExternalID
	// call (below) loses its guard -- Postgres's own claim row still
	// converges to exactly ONE identity regardless, which is what
	// §21.1b actually promises ("two ACTIVE identities... worse than
	// none" -- active meaning ones this system still treats as
	// current).
	//
	// Finding A7 (reassessed, corrected from an earlier "harmless"
	// characterization): the orphan is NOT harmless. Nothing in this
	// system ever writes to it again, so it never reaches
	// StatusCompleted -- it sits on the pull request's own Checks tab
	// PERMANENTLY queued or in_progress, a second, real, human-visible
	// "narvi/review" entry beside the one this system keeps correctly
	// updating. That is exactly the shape §21.1b's own "two active
	// identities... worse than none" warns against: a human (or GitHub's
	// own required-check evaluation, once a repository opts in, decision
	// 2) reading the PR sees a check that will never complete, with no
	// way to tell, from the PR alone, that it is a discarded duplicate
	// rather than a second, still-running assessment.
	//
	// C2 (corrected -- the prior text here misattributed this residual):
	// recovering via "select by SHA and GitHub App" (the brief's own
	// identity rule, finding A2's own self-learned writer App id, now
	// ALSO the per-PR reviewcheck.PRExternalID, finding C1 below) does
	// NOT narrow this specific orphan at all, whether or not this
	// process has already learned its own writer App id: BOTH attempts
	// reach this branch because NEITHER has recorded an external id
	// locally yet, which also means neither has CREATED the check run on
	// GitHub yet -- there is nothing yet on GitHub for either one's own
	// list call to find and adopt, known writer App id or not. The
	// known/not-yet-observed distinction changes the outcome of a
	// SEQUENTIAL recovery (a crash, then a LATER redelivery reaching this
	// branch after the original create has already landed on GitHub) --
	// it does nothing for a genuine concurrent race between two attempts
	// in flight at the same time, which is what this comment is actually
	// about. Closing the race itself would need either serializing the
	// two GitHub calls (which ports.Notifier.Deliver's own contract
	// forbids: no Postgres transaction may span a network call) or a
	// periodic reconciliation sweep that finds and completes/cleans up a
	// tracked PR's own orphaned check runs -- neither is built here;
	// named as a real, open follow-up rather than re-asserted as
	// harmless.
	//
	// Finding C1: this orphan is entirely SAME-PR (both racers are
	// delivering for the identical (repo, pr_number, head_sha)) and is
	// unrelated to, and not fixed by, reviewcheck.PRExternalID's own
	// per-PR scoping below -- that fixes a DIFFERENT collapse, two
	// DIFFERENT pull requests sharing one head commit adopting the SAME
	// run; this comment's own race is two deliveries for the SAME pull
	// request, which the per-PR discriminator does not, and is not meant
	// to, distinguish from each other.
	externalID, err := n.resolveOrCreateCheckRun(ctx, logger, payload.Owner, payload.Repo, candidate.HeadSHA, candidate.PRNumber, output)
	if err != nil {
		// C3 (fixed): resolveOrCreateCheckRun now classifies its own
		// failure at the exact point it occurred (list/adopt/create), so
		// the error returned here is already logged with the correct op
		// -- never re-labeled "create" when the real failure was an
		// adopt PATCH (or the preceding list read), which used to
		// misdiagnose an adopt-time permission failure as evidence the
		// CREATE endpoint itself lacks checks:write.
		return err
	}
	// finding A3: same guard as the plain-update branch above, for the
	// identical reason -- resolveOrCreateCheckRun's own network call(s)
	// also run with no transaction open.
	n.guardAgainstSupersessionDuringCall(ctx, logger, payload.Owner, payload.Repo, int32(payload.PRNumber), candidate, externalID)

	if _, err := n.store.SetExternalID(ctx, payload.Owner+"/"+payload.Repo, int32(payload.PRNumber), externalID, attemptID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Guard miss: a newer candidate reserved this row while the
			// GitHub call above was in flight. See
			// SetReviewCheckRunExternalID's own generated doc comment for
			// the accepted residual this represents -- this delivery is
			// done regardless; the newer candidate's own Deliver call
			// already has, or will get, its own correct external id.
			logger.Warn("outboxworker: reviewCheckNotifier: external id write lost the guard (superseded during the GitHub call); the just-created/updated check run is now unrecorded locally",
				"repo", candidate.RepoFullName, "pr_number", candidate.PRNumber, "external_id", externalID)
			return nil
		}
		return fmt.Errorf("outboxworker: reviewCheckNotifier: record external id: %w", err)
	}
	return nil
}

// resolveOrCreateCheckRun implements "select by SHA and GitHub App"
// (finding A2), narrowed to one PULL REQUEST (finding C1): it first lists
// GitHub's own check runs for headSHA and adopts (PATCHes) the one
// matching reviewcheck.CheckName, this deployment's own SELF-OBSERVED
// writer App id (finding A2 -- writerAppID's own doc comment; NEVER a
// merely-configured value that could name a different credential's App
// entirely), is NOT ALREADY CONCLUDED (finding A1), AND whose own
// external_id names prNumber's own reviewcheck.PRExternalID (finding
// C1), if any such run exists -- never merely the first name match. Only
// when no match is found does it create a brand-new check run.
//
// Finding C1: a GitHub check run is scoped to a commit SHA, never to a
// pull request -- ListCheckRunsForRef returns the IDENTICAL set of runs
// for every pull request sharing headSHA (the same branch opened against
// two bases; a fork PR beside a same-repo PR carrying the same commit).
// Before this fix, the predicate above stopped at (name, App id, head
// SHA, not-completed), none of which is scoped to a pull request at
// all -- resolveOrCreateCheckRun itself was never even given a PR number
// to scope by. Two such pull requests each emitting for the first time
// would each independently see nothing to adopt and each create their
// own run correctly, but the SECOND one to later reach a crash-recovery
// or cold-adoption read (this function, again, with no external id
// recorded locally) would find and adopt the FIRST pull request's own
// run: both pull requests' own review_check_runs rows would then name
// the SAME external_id, and every later emission from EITHER one
// overwrites what the OTHER's Checks tab shows -- reproduced against
// real Postgres (this Step's own report has the full before/after). The
// prExternalID comparison closes it: reviewcheck.PRExternalID(prNumber)
// is stamped onto every run this notifier creates (below) and is unique
// per PR within the (owner, repo) scope every call here already runs
// under, so a run created for a DIFFERENT pull request never matches.
//
// The status exclusion is the fix for a REAL, reproduced defect: the
// caller (Deliver, above) reaches this function with existingExternalID
// == nil in two shapes that used to look identical here but are not --
// genuinely nothing published yet for this head sha, OR this row's own
// external_id was JUST cleared because the CURRENT row was
// terminal-shaped and candidate is a newer attempt (newIdentityNeeded's
// own "open a NEW check when a review restarts after a terminal
// result... never reopen a concluded one" branch, Deliver above). Before
// this fix, THAT exact concluded run was still the ONE thing this list
// call would find (same name, same head sha, same App) and adopt --
// PATCHing a just-concluded, successful check run back to
// in_progress/action_required, precisely the "reopen a concluded check"
// the brief's own identity rule forbids. CheckRunSummary carried no
// status at all before this fix, so this predicate structurally COULD
// NOT exclude a concluded run no matter how newIdentityNeeded's own
// decision was written -- the fix had to reach the wire-level type, not
// just this function's own logic.
//
// C3 (fixed): every failure below is classified (n.classifyAndWrap) at
// the exact point it occurred -- "list", "adopt", or "create" -- rather
// than left for the caller to label uniformly as "create". An adopt-time
// PATCH failure is the one shape that used to be misdiagnosed hardest: it
// used to surface as "outboxworker: reviewCheckNotifier: create check
// run: ... permission denied", which reads as "the create endpoint lacks
// checks:write" even when a check run was already found (meaning the
// preceding list read succeeded) and only the PATCH itself failed -- the
// one diagnosis this feature most needs to be accurate about, since a
// missing checks:write is this feature's own expected first production
// state (classifyAndWrap's own doc comment).
func (n *reviewCheckNotifier) resolveOrCreateCheckRun(ctx context.Context, logger *slog.Logger, owner, repo, headSHA string, prNumber int32, output reviewcheck.Output) (int64, error) {
	prExternalID := reviewcheck.PRExternalID(prNumber)
	existing, err := n.adapter.ListCheckRunsForRef(ctx, owner, repo, headSHA, n.botToken)
	if err != nil {
		return 0, n.classifyAndWrap(logger, "list", err)
	}
	// observedAppID == 0 ("not yet observed", writerAppID's own doc
	// comment) means this process cannot yet tell ITS OWN check runs
	// apart from another app's identically-named one -- the safe
	// degradation is to never adopt (fall through to CreateCheckRun
	// below, which itself teaches this notifier its own app id for
	// every LATER call in this process's lifetime), never to guess.
	observedAppID := n.observedWriterAppID(ctx)
	if observedAppID != 0 {
		for _, run := range existing {
			if run.Name == reviewcheck.CheckName && run.AppID == observedAppID && run.HeadSHA == headSHA && run.Status != string(reviewcheck.StatusCompleted) && run.ExternalID == prExternalID {
				if err := n.adapter.UpdateCheckRun(ctx, owner, repo, n.botToken, run.ID, string(output.Status), string(output.Conclusion), output.Title, output.Summary); err != nil {
					return 0, n.classifyAndWrap(logger, "adopt", err)
				}
				return run.ID, nil
			}
		}
	}
	id, appID, err := n.adapter.CreateCheckRun(ctx, owner, repo, n.botToken, headSHA, reviewcheck.CheckName, string(output.Status), string(output.Conclusion), output.Title, output.Summary, prExternalID)
	if err != nil {
		return 0, n.classifyAndWrap(logger, "create", err)
	}
	n.recordWriterAppID(ctx, appID)
	return id, nil
}

// guardAgainstSupersessionDuringCall (finding A3, corrected by finding
// B1) re-checks, with NO Postgres transaction/lock held (a plain read,
// mirroring GetByRepoAndPRNumber's own "forward, non-locking read"
// shape), whether candidate is STILL this row's current emission now
// that the GitHub call that just published externalID's own output has
// returned. If it is, this was not a race -- nothing to do. If the row
// has moved on to a DIFFERENT attempt or phase for the SAME head sha
// AND row.ExternalID still names externalID -- the identity THIS call
// itself just published to -- the call this function follows was stale
// by the time it landed on GitHub (a concurrently-racing, genuinely
// newer Deliver call finished ITS OWN GitHub write first, to the SAME
// external check run) -- this self-heals by immediately re-publishing
// the row's own CURRENT truth to that SAME external check run, so
// GitHub converges to what Postgres already knows rather than being left
// showing this call's now-superseded output indefinitely. If the row has
// moved on to a DIFFERENT head sha, nothing here can correct that (a
// different head sha means a different external identity entirely,
// which the emission that changed it owns and publishes for itself) --
// logged, left alone. And if row.ExternalID no longer names externalID
// (finding B1) -- newIdentityNeeded (Deliver, above) already cleared it
// for a newer attempt at this SAME head sha, entirely within this call's
// own network-call window -- the self-heal above is not merely stale, it
// would be aimed at the WRONG run: the row has genuinely moved off this
// external identity, exactly like the different-head-sha case, and
// PATCHing externalID would reopen an already-concluded check run this
// row no longer claims (A1's own forbidden shape, reached through a path
// resolveOrCreateCheckRun's status predicate does not cover, since it
// never runs for an in-place update). Logged, left alone, same as the
// different-head-sha case -- C6 (fixed): row.ExternalID is nil in the
// MORE common of the two shapes here (the newer attempt's own create has
// not yet landed, or its own SetExternalID itself lost a guard to a
// still-newer write -- nothing has actually been recorded yet), and
// non-nil-but-different in the less common one (a fresh identity has
// already landed); the two are logged distinctly below rather than under
// one "already opened a fresh identity" message that only the second
// shape earns.
//
// Best-effort throughout: a failure reading the row, or a failure
// PATCHing the correction, is logged and swallowed, never propagated as
// this Deliver call's own error -- the ORIGINAL write already succeeded
// (that is what this function is verifying, after the fact), so failing
// the whole delivery over an inability to double-check or self-correct
// would turn a race this function is trying to narrow into a guaranteed
// redelivery of a call that already landed once. A single, un-recursed
// correction attempt: if a THIRD, still-newer write races even the
// correction itself (vanishingly rare -- it requires two supersessions
// within one Deliver call's own network-call window), the corrected
// state is itself stale by one more step; the next real phase transition
// for the PR (verdict-posting's own direct enqueue, if nothing else)
// still converges GitHub to the truth, the same way any other transient
// staleness in this system resolves.
func (n *reviewCheckNotifier) guardAgainstSupersessionDuringCall(ctx context.Context, logger *slog.Logger, owner, repo string, prNumber int32, candidate reviewcheck.Emission, externalID int64) {
	repoFullName := owner + "/" + repo
	row, err := n.store.GetByRepoAndPRNumber(ctx, repoFullName, prNumber)
	if err != nil {
		logger.Warn("outboxworker: reviewCheckNotifier: could not re-check for supersession during the GitHub call; leaving the just-published state as-is", "repo", repoFullName, "pr_number", prNumber, "error", err)
		return
	}
	current := toEmission(row.RepoFullName, row.PrNumber, row.HeadSha, row.AttemptID, row.AttemptCreatedAt, row.Phase, row.BaseRef, row.BaseSha, row.PolicyVersion)
	if current.AttemptID == candidate.AttemptID && current.Phase == candidate.Phase && current.HeadSHA == candidate.HeadSHA {
		return
	}
	if current.HeadSHA != candidate.HeadSHA {
		logger.Warn("outboxworker: reviewCheckNotifier: row moved to a different head sha during this call's own GitHub round trip; a fresh emission for the new head owns its own identity, nothing to correct here",
			"repo", repoFullName, "pr_number", prNumber, "candidate_head_sha", candidate.HeadSHA, "current_head_sha", current.HeadSHA)
		return
	}
	// finding B1: same head sha is NOT sufficient to conclude externalID
	// (the identity THIS call just published to) is still the row's own
	// current identity. newIdentityNeeded (Deliver, above) clears
	// external_id and opens a FRESH check run whenever the row was
	// already terminal-shaped and a genuinely newer attempt claims it --
	// reachable from inside this exact race window, since that claim
	// commits (and may even create its own new check run) entirely
	// between this call's own GitHub write returning and this read. When
	// that happens, row.ExternalID now names the NEW run the newer
	// attempt opened -- a DIFFERENT external id from externalID, even
	// though head sha is unchanged -- and self-correcting onto externalID
	// would PATCH the OLD, already-concluded run this row no longer
	// claims, reopening it (completed/success back to in_progress, with
	// conclusion dropped entirely -- checkRunRequest's own omitempty
	// behavior) while the row's own current truth lives on a run this
	// call never touches. Reproduced against real Postgres: without this
	// check, exactly that reopening happens. Skip the self-heal (best
	// effort, same as every other outcome this function reaches) rather
	// than write onto an identity this row has already moved off of --
	// the newer attempt's own Deliver call already published, or will
	// publish, its own correct output to its own run.
	// C6 (fixed): the two shapes below share a return, but not an actual
	// cause -- the prior single log line asserted "a newer emission
	// already opened a fresh identity" for BOTH, which is only true of
	// the second. row.ExternalID == nil is, in practice, the MORE common
	// of the two: newIdentityNeeded has cleared the column and the
	// genuinely newer attempt's own GitHub call (create-and-record) is
	// still in flight, or that attempt's own SetExternalID itself lost
	// its guard to a STILL-newer write -- either way nothing has been
	// durably recorded here yet, "already opened" overstates what is
	// actually known. Only the non-nil-mismatch case below is a fresh
	// identity that has genuinely already landed.
	if row.ExternalID == nil {
		logger.Warn("outboxworker: reviewCheckNotifier: row no longer records any external id for this head sha; a newer attempt's own identity claim is in flight (or itself lost its guard) -- nothing recorded yet to self-heal onto",
			"repo", repoFullName, "pr_number", prNumber, "external_id", externalID,
			"candidate_attempt", candidate.AttemptID, "current_attempt", current.AttemptID)
		return
	}
	if *row.ExternalID != externalID {
		logger.Warn("outboxworker: reviewCheckNotifier: row's own external id no longer names the run this call published to; a newer emission already opened and recorded a fresh identity for this head sha -- nothing to self-heal on the run this call wrote to",
			"repo", repoFullName, "pr_number", prNumber, "external_id", externalID, "row_external_id", *row.ExternalID,
			"candidate_attempt", candidate.AttemptID, "current_attempt", current.AttemptID)
		return
	}
	logger.Warn("outboxworker: reviewCheckNotifier: superseded during this call's own GitHub round trip; self-correcting the check run to the row's current truth",
		"repo", repoFullName, "pr_number", prNumber, "external_id", externalID,
		"candidate_attempt", candidate.AttemptID, "candidate_phase", candidate.Phase,
		"current_attempt", current.AttemptID, "current_phase", current.Phase)
	correctedOutput := reviewcheck.ComputeOutput(current.Phase)
	if err := n.adapter.UpdateCheckRun(ctx, owner, repo, n.botToken, externalID, string(correctedOutput.Status), string(correctedOutput.Conclusion), correctedOutput.Title, correctedOutput.Summary); err != nil {
		logger.Warn("outboxworker: reviewCheckNotifier: self-correction PATCH failed; the check run may still show stale output until a future emission republishes it", "repo", repoFullName, "pr_number", prNumber, "error", err)
	}
}

// classifyAndWrap surfaces "permission, rate-limit and transient
// failures distinctly" (the brief's own identity-rules requirement),
// logging which of the three this attempt's error is BEFORE wrapping and
// returning it -- the outbox's own existing retry/backoff/dead-letter
// path (§5.1) applies uniformly to whatever this returns, but an operator
// reading logs (or a future dead-letter reason column) can already tell
// a missing `checks:write` permission apart from a flaky network apart
// from GitHub's own rate limiting, without this classification
// collapsing all three into one indistinguishable "delivery failed". A
// missing `checks:write` scope is this feature's own expected FIRST
// production state (an installation this codebase does not itself grant
// the scope to), which is exactly why op (below) must name the real
// operation that failed -- C3's own fix, resolveOrCreateCheckRun's own
// doc comment -- rather than a caller-asserted label that may not match
// where the failure actually occurred.
func (n *reviewCheckNotifier) classifyAndWrap(logger *slog.Logger, op string, err error) error {
	var apiErr *githubapi.APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.RateLimited:
		logger.Warn("outboxworker: reviewCheckNotifier: rate-limited by GitHub", "op", op, "error", err)
	case errors.Is(err, ports.ErrPermissionDenied):
		logger.Error("outboxworker: reviewCheckNotifier: permission denied -- this installation likely lacks checks:write", "op", op, "error", err)
	case errors.Is(err, ports.ErrAuthenticationFailed):
		logger.Error("outboxworker: reviewCheckNotifier: authentication failed -- the configured bot credential was rejected", "op", op, "error", err)
	default:
		logger.Warn("outboxworker: reviewCheckNotifier: transient delivery failure", "op", op, "error", err)
	}
	return fmt.Errorf("outboxworker: reviewCheckNotifier: %s check run: %w", op, err)
}
