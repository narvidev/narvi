//go:build integration

// Integration tests proving §30.8's own review_verdicts exclusion against
// a REAL Postgres instance: "Shadow-era verdicts must never arm auto-merge
// after promotion... the exclusion lives in the query, never at call
// sites; promotion additionally sets a fence." Run via `make
// test-integration`.
package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// insertAutoApprovedVerdict inserts a Shippable='auto' review_verdicts
// row for (repoFullName, prNumber) stamped with the GIVEN
// suppressedInShadow value -- deliberately bypassing
// internal/app/reviewverdict.Insert's own egressmode.Resolve call (which
// would always compute a value consistent with repoFullName's CURRENT
// repo_settings row, making it impossible to construct the "verdict
// stamped shadow, repo since promoted" scenario these tests exist to
// prove) so the row's own stamp can be pinned independently of whatever
// repo_settings says at the moment of the INSERT.
func insertAutoApprovedVerdict(ctx context.Context, t *testing.T, store *narvipg.ReviewVerdictStore, repoFullName string, prNumber int32, headSHA string, suppressedInShadow bool) {
	t.Helper()
	if _, err := store.Insert(ctx, sqlcgen.InsertReviewVerdictParams{
		RepoFullName:       repoFullName,
		PrNumber:           prNumber,
		HeadSha:            headSHA,
		RiskLevel:          "low",
		Premise:            "ok",
		BlastRadius:        []byte("[]"),
		FilesChanged:       1,
		TestsCoverage:      "adequate",
		DocsDrift:          "none",
		ProposedShippable:  "auto",
		Shippable:          "auto",
		SuppressedInShadow: suppressedInShadow,
		ArchDecisionTags:   []byte(`[]`),
		ArchDecisionRoots:  []byte(`[]`),
	}); err != nil {
		t.Fatalf("insert review_verdicts row: %v", err)
	}
}

// TestListLatestAutoApproved_ExcludesShadowStampedVerdict_EvenAfterPromotion
// is this Step's own core §30.8 claim about review_verdicts, proven with
// a real flip: a verdict recorded while its repo was shadow keeps its own
// suppressed_in_shadow=true stamp forever -- ListLatestAutoApproved
// (internal/app/automerge's own candidate-discovery query) must never
// return it, even after the SAME repo is later promoted to live. Without
// this, the automerge worker's own bounded-recent-verdicts scan would
// pick up a stale, never-really-reviewed verdict and act on it for real
// the moment a repo goes live.
func TestListLatestAutoApproved_ExcludesShadowStampedVerdict_EvenAfterPromotion(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	verdicts := narvipg.NewReviewVerdictStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	const repoFullName = "acme/shadow-verdict-born-shadow"
	const prNumber = int32(1)

	insertAutoApprovedVerdict(ctx, t, verdicts, repoFullName, prNumber, "sha-shadow-1", true)

	// THE FLIP: promote the repo AFTER the verdict was recorded.
	if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("promote repo to live egress: %v", err)
	}

	since := pgtype.Timestamptz{Time: time.Now().Add(-24 * time.Hour), Valid: true}
	candidates, err := verdicts.ListLatestAutoApproved(ctx, repoFullName, since, 20)
	if err != nil {
		t.Fatalf("ListLatestAutoApproved: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("ListLatestAutoApproved returned %d candidate(s), want 0 (the one verdict on record is stamped suppressed_in_shadow=true and must never become a candidate, whatever the repo's own CURRENT live_egress_enabled says)", len(candidates))
	}
}

// TestListLatestAutoApproved_ExcludesShadowStampedVerdict_PastThePromotionFence
// isolates the suppressed_in_shadow stamp check from the promotion-fence
// check above: the repo is promoted FIRST (so the fence is already
// satisfied by the time the verdict is recorded), and the verdict is
// still stamped suppressed_in_shadow=true -- a value the real
// reviewverdict.Insert could never actually produce for an
// already-promoted repo (its own egressmode.Resolve call would compute
// false), but exactly the shape a bug in that computation could produce.
// If the query relied on the fence alone, this row would incorrectly
// pass; the stamp check is what catches it here.
func TestListLatestAutoApproved_ExcludesShadowStampedVerdict_PastThePromotionFence(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	verdicts := narvipg.NewReviewVerdictStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	const repoFullName = "acme/shadow-verdict-stamp-isolated"
	const prNumber = int32(1)

	if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("promote repo to live egress: %v", err)
	}
	insertAutoApprovedVerdict(ctx, t, verdicts, repoFullName, prNumber, "sha-shadow-2", true)

	since := pgtype.Timestamptz{Time: time.Now().Add(-24 * time.Hour), Valid: true}
	candidates, err := verdicts.ListLatestAutoApproved(ctx, repoFullName, since, 20)
	if err != nil {
		t.Fatalf("ListLatestAutoApproved: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("ListLatestAutoApproved returned %d candidate(s), want 0 (this verdict is stamped suppressed_in_shadow=true and must be excluded by that stamp alone, even though it is created after the repo's own promotion fence)", len(candidates))
	}
}

// TestListLatestAutoApproved_ExcludesVerdictPredatingPromotionFence proves
// the independent, second half of the same guarantee: EVEN a verdict
// whose own suppressed_in_shadow stamp is false (it was genuinely live
// when recorded) must not count as a candidate if it predates this
// repo's own CURRENT live_egress_promoted_at fence -- e.g. a verdict from
// before a demote-then-repromote cycle. Belt and suspenders: this
// assertion would still hold even if the stamp above were somehow wrong.
func TestListLatestAutoApproved_ExcludesVerdictPredatingPromotionFence(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	verdicts := narvipg.NewReviewVerdictStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	const repoFullName = "acme/shadow-verdict-stale-promotion"
	const prNumber = int32(1)

	// A verdict recorded while genuinely live (suppressedInShadow=false),
	// BEFORE the demote/re-promote cycle below.
	insertAutoApprovedVerdict(ctx, t, verdicts, repoFullName, prNumber, "sha-live-1", false)

	// Demote, then re-promote -- migrations/
	// 000104_repo_settings_live_egress_promoted_at.up.sql's own doc
	// comment: a demotion clears the fence, and a later re-promotion sets
	// a FRESH one, strictly after the verdict above.
	if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repoFullName, false); err != nil {
		t.Fatalf("demote repo: %v", err)
	}
	if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("re-promote repo: %v", err)
	}

	since := pgtype.Timestamptz{Time: time.Now().Add(-24 * time.Hour), Valid: true}
	candidates, err := verdicts.ListLatestAutoApproved(ctx, repoFullName, since, 20)
	if err != nil {
		t.Fatalf("ListLatestAutoApproved: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("ListLatestAutoApproved returned %d candidate(s), want 0 (this verdict predates the CURRENT promotion fence, even though its own stamp says it was never suppressed)", len(candidates))
	}
}

// TestListLatestAutoApproved_IncludesLiveVerdictAfterItsOwnPromotion is
// this pair's own control case: a verdict genuinely recorded AFTER a
// real promotion, for a repo that has never been demoted since, DOES
// appear as a candidate -- proving the two exclusion tests above are
// really testing suppression, not merely a query that never returns
// anything.
func TestListLatestAutoApproved_IncludesLiveVerdictAfterItsOwnPromotion(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	verdicts := narvipg.NewReviewVerdictStore(pool)
	repoSettings := narvipg.NewRepoSettingsStore(pool)
	const repoFullName = "acme/shadow-verdict-genuinely-live"
	const prNumber = int32(1)

	if _, err := repoSettings.UpsertLiveEgressEnabled(ctx, repoFullName, true); err != nil {
		t.Fatalf("promote repo to live egress: %v", err)
	}
	insertAutoApprovedVerdict(ctx, t, verdicts, repoFullName, prNumber, "sha-live-2", false)

	since := pgtype.Timestamptz{Time: time.Now().Add(-24 * time.Hour), Valid: true}
	candidates, err := verdicts.ListLatestAutoApproved(ctx, repoFullName, since, 20)
	if err != nil {
		t.Fatalf("ListLatestAutoApproved: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("ListLatestAutoApproved returned %d candidate(s), want 1 (a verdict genuinely recorded after this repo's own promotion, with no demotion since, must count)", len(candidates))
	}
	if candidates[0].HeadSha != "sha-live-2" {
		t.Errorf("HeadSha = %q, want %q", candidates[0].HeadSha, "sha-live-2")
	}
}
