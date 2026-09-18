package reviewverdict

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/reviewverdict"
)

// ErrAcceptanceStoreNotConfigured is returned by Accept/RevokeAcceptance/
// GetAcceptance/ListAcceptances below when store is nil (finding F9,
// adversarial review: Deps.Acceptances' own doc comment already promised
// "Accept/Revoke [degrade] to a plain error, never a silent no-op" --
// before this fix, only GetActiveAcceptance actually checked for nil;
// every other function here called straight through to a nil *postgres.
// ReviewVerdictAcceptanceStore and panicked on the nil-pointer field
// access inside it). Mirrors GetActiveAcceptance's own "a deployment/test
// wiring that never constructs this store" precedent, except a WRITE (or
// a read a caller cannot silently treat as absence, e.g. GetAcceptance's
// own use in RevokeAcceptance's failed-revoke diagnosis) has no honest
// ok=false to degrade to -- an error is the only honest answer.
var ErrAcceptanceStoreNotConfigured = errors.New("reviewverdict: review verdict acceptance store not configured")

// AcceptInput is Accept's own caller-supplied shape -- every field the
// accepting maintainer+ actually controls, or that the caller has
// already resolved server-side. Accept never re-fetches or re-derives
// any of Verdict's own fields; the caller is responsible for passing the
// SAME Record GetLatest just returned (mirrors Insert's own "the caller
// decides, this function only persists" contract, insert.go).
type AcceptInput struct {
	RepoFullName string
	PRNumber     int32
	// VerdictID/AttemptID/HeadSHA/Context are the accepted verdict's own
	// identity -- reviewverdict.Record.ID/AttemptID/HeadSHA/Context,
	// converted to this package's own driver-facing pgtype.UUID at the
	// httpapi boundary exactly like sessionID/attemptID already are on
	// Insert above. AttemptID may be the zero pgtype.UUID (Valid=false)
	// -- mirrors review_verdicts.attempt_id's own nullable precedent
	// (migrations/000130): a pre-amendment verdict recorded no attempt.
	VerdictID pgtype.UUID
	AttemptID pgtype.UUID
	HeadSHA   string
	Context   reviewverdict.Context
	// Reason is httpapi's own best-effort, no-I/O classification of which
	// waivable eligibility criterion this acceptance most likely
	// addresses -- NOT itself computed by calling autoapproval.
	// ComputeEligible (finding F8, adversarial review: this comment
	// previously claimed it was) -- display/audit only
	// (reviewverdict.Acceptance.Reason's own doc comment).
	Reason        string
	Justification string
	AcceptedBy    pgtype.UUID
}

// Accept records one review_verdict_acceptances row ("human acceptance
// of a verdict the engine refuses", §21.1b) -- APPEND-ONLY (this table's
// own migration doc comment): a repeat accept inserts a NEW row, never an
// UPDATE. Unlike an earlier version of this table (finding F2, adversarial
// review), a repeat accept for the SAME pull request now SUPERSEDES
// (revokes) any existing active row for that pull request FIRST --
// postgres.ReviewVerdictAcceptanceStore.Insert's own doc comment for why
// this is two sequential statements, not one combined atomic statement --
// so at most one row is ever active at a time
// (review_verdict_acceptances_one_active_idx), and revoking "the" active
// acceptance can never silently re-activate an earlier one left lying
// around.
func Accept(ctx context.Context, store *postgres.ReviewVerdictAcceptanceStore, in AcceptInput) (reviewverdict.Acceptance, error) {
	if store == nil {
		return reviewverdict.Acceptance{}, ErrAcceptanceStoreNotConfigured
	}
	ancestorChainJSON, err := marshalAncestorChain(in.Context.AncestorChain)
	if err != nil {
		return reviewverdict.Acceptance{}, fmt.Errorf("reviewverdict: accept: marshal ancestor chain: %w", err)
	}

	row, err := store.Insert(ctx, sqlcgen.InsertReviewVerdictAcceptanceParams{
		RepoFullName:  in.RepoFullName,
		PrNumber:      in.PRNumber,
		VerdictID:     in.VerdictID,
		AttemptID:     in.AttemptID,
		HeadSha:       in.HeadSHA,
		BaseRef:       nonEmptyStringPtr(in.Context.BaseRef),
		BaseSha:       nonEmptyStringPtr(in.Context.BaseSHA),
		AncestorChain: ancestorChainJSON,
		PolicyVersion: int32(in.Context.PolicyVersion),
		Reason:        in.Reason,
		Justification: in.Justification,
		AcceptedBy:    in.AcceptedBy,
	})
	if err != nil {
		return reviewverdict.Acceptance{}, err
	}
	return acceptanceFromRow(row), nil
}

// GetActiveAcceptance fetches (repoFullName, prNumber)'s own LATEST
// non-revoked acceptance, if any -- ok=false (never an error) means no
// acceptance is currently active for this PR, a legitimate, common
// outcome (most PRs are never accepted at all) every real caller
// (internal/app/decisioninbox's revalidateCore) must treat as "no
// acceptance applies", exactly like reviewverdict.GetLatest's own
// identical ok=false convention for "no verdict posted at all".
//
// A row returned here (ok=true) may still be INAPPLICABLE to the PR's
// current verdict -- this function does not itself decide that; the
// caller compares the returned Acceptance's own VerdictID against the
// id of whatever reviewverdict.GetLatest just returned, via
// reviewverdict.Acceptance.Applicable.
//
// nil-safe: store == nil degrades to ok=false, err=nil, mirroring
// Deps.Acceptances' own doc comment ("a caller that doesn't need this
// rollup simply never wires its own store") -- a deployment/test wiring
// that never constructs a ReviewVerdictAcceptanceStore behaves exactly
// as though no acceptance had ever been recorded, never a panic or a
// spurious error on a feature it never opted into.
func GetActiveAcceptance(ctx context.Context, store *postgres.ReviewVerdictAcceptanceStore, repoFullName string, prNumber int32) (acceptance reviewverdict.Acceptance, ok bool, err error) {
	if store == nil {
		return reviewverdict.Acceptance{}, false, nil
	}
	row, err := store.GetActive(ctx, repoFullName, prNumber)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return reviewverdict.Acceptance{}, false, nil
		}
		return reviewverdict.Acceptance{}, false, err
	}
	return acceptanceFromRow(row), true, nil
}

// RevokeAcceptance records a maintainer+'s explicit revocation of
// acceptance id, SCOPED to repoFullName -- ok=false (never an error)
// means EITHER no acceptance with this id exists in this repo at all,
// OR it exists but is already revoked; the caller (httpapi) tells the
// two apart with a follow-up GetAcceptance (also repo-scoped) on this
// same ok=false path, mirroring postgres.ReviewVerdictAcceptanceStore.
// Revoke's own identical caller-side discipline.
func RevokeAcceptance(ctx context.Context, store *postgres.ReviewVerdictAcceptanceStore, id, revokedBy pgtype.UUID, repoFullName string) (acceptance reviewverdict.Acceptance, ok bool, err error) {
	if store == nil {
		return reviewverdict.Acceptance{}, false, ErrAcceptanceStoreNotConfigured
	}
	row, err := store.Revoke(ctx, id, revokedBy, repoFullName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return reviewverdict.Acceptance{}, false, nil
		}
		return reviewverdict.Acceptance{}, false, err
	}
	return acceptanceFromRow(row), true, nil
}

// GetAcceptance fetches one acceptance by id, SCOPED to repoFullName --
// ok=false (never an error) means no such acceptance exists in this
// repo.
func GetAcceptance(ctx context.Context, store *postgres.ReviewVerdictAcceptanceStore, id pgtype.UUID, repoFullName string) (acceptance reviewverdict.Acceptance, ok bool, err error) {
	if store == nil {
		return reviewverdict.Acceptance{}, false, ErrAcceptanceStoreNotConfigured
	}
	row, err := store.Get(ctx, id, repoFullName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return reviewverdict.Acceptance{}, false, nil
		}
		return reviewverdict.Acceptance{}, false, err
	}
	return acceptanceFromRow(row), true, nil
}

// ListAcceptances returns every acceptance (active or revoked) for
// (repoFullName, prNumber), newest-first, bounded by limit -- the audit
// view.
func ListAcceptances(ctx context.Context, store *postgres.ReviewVerdictAcceptanceStore, repoFullName string, prNumber, limit int32) ([]reviewverdict.Acceptance, error) {
	if store == nil {
		return nil, ErrAcceptanceStoreNotConfigured
	}
	rows, err := store.List(ctx, repoFullName, prNumber, limit)
	if err != nil {
		return nil, err
	}
	out := make([]reviewverdict.Acceptance, len(rows))
	for i, row := range rows {
		out[i] = acceptanceFromRow(row)
	}
	return out, nil
}

// acceptanceFromRow converts a freshly-inserted-or-read
// sqlcgen.ReviewVerdictAcceptance into the pure reviewverdict.Acceptance
// shape -- the one seam every caller of this file goes through, never
// hand-built ad hoc at each call site, mirroring recordFromRow's own
// identical role for review_verdicts (convert.go).
func acceptanceFromRow(row sqlcgen.ReviewVerdictAcceptance) reviewverdict.Acceptance {
	a := reviewverdict.Acceptance{
		ID:               pgUUIDToString(row.ID),
		RepoFullName:     row.RepoFullName,
		PRNumber:         row.PrNumber,
		VerdictID:        pgUUIDToString(row.VerdictID),
		AttemptID:        attemptIDFromRow(row.AttemptID),
		HeadSHA:          row.HeadSha,
		Reason:           row.Reason,
		Justification:    row.Justification,
		AcceptedByUserID: pgUUIDToString(row.AcceptedBy),
		AcceptedAt:       row.AcceptedAt.Time,
	}
	if row.BaseRef != nil {
		a.Context.BaseRef = *row.BaseRef
	}
	if row.BaseSha != nil {
		a.Context.BaseSHA = *row.BaseSha
	}
	a.Context.AncestorChain = unmarshalAncestorChain(row.AncestorChain)
	a.Context.PolicyVersion = int(row.PolicyVersion)
	if row.RevokedAt.Valid {
		t := row.RevokedAt.Time
		a.RevokedAt = &t
	}
	if row.RevokedBy.Valid {
		a.RevokedByUserID = pgUUIDToString(row.RevokedBy)
	}
	return a
}
