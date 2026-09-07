//go:build integration

// Integration tests proving §31.6's own knowledge-retrieval GATE against a
// REAL Postgres instance: the tag/root overlap predicate, the shadow-epoch
// exclusion, and the contested-content exclusion all live in the SQL
// itself (queries/reviewverdicts.sql's own ListGatedArchDecisions/
// ListRecentArchDecisions), never at call sites -- these tests are what
// actually exercises that SQL rather than merely trusting its doc comment.
// Run via `make test-integration`.
package postgres_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// insertArchDecisionVerdict inserts one review_verdicts row carrying
// exactly one arch-decision, with the given tags/roots/shadow stamp --
// bypasses internal/app/reviewverdict.Insert entirely (this package
// cannot import it, and would not want to: these tests need to pin
// suppressedInShadow/tags/roots independently of whatever a real egress
// resolution or classification would compute) so the gate's own SQL can
// be tested against precisely-controlled rows.
//
// A nil tags/roots is normalized to the literal JSON "[]" before the
// INSERT -- naming arch_decision_tags/arch_decision_roots explicitly in
// this INSERT's own column list defeats their NOT NULL DEFAULT '[]'::jsonb
// (migrations/000113's own doc comment, and the exact trap a fix earlier
// on this branch closed for every OTHER hand-built InsertReviewVerdictParams
// literal): a bare Go nil []byte binds a genuine SQL NULL, which this
// NOT NULL column rejects outright, rather than silently falling back to
// its own default the way an omitted column would.
func insertArchDecisionVerdict(ctx context.Context, t *testing.T, store *narvipg.ReviewVerdictStore, repoFullName string, prNumber int32, headSHA string, tags, roots []byte, suppressedInShadow bool) sqlcgen.ReviewVerdict {
	t.Helper()
	if tags == nil {
		tags = []byte(`[]`)
	}
	if roots == nil {
		roots = []byte(`[]`)
	}
	row, err := store.Insert(ctx, sqlcgen.InsertReviewVerdictParams{
		RepoFullName:        repoFullName,
		PrNumber:            prNumber,
		HeadSha:             headSHA,
		RiskLevel:           "low",
		Premise:             "ok",
		BlastRadius:         []byte(`["database"]`), // deliberately DIFFERENT from arch_decision_tags -- proves the gate never reads this column
		FilesChanged:        1,
		TestsCoverage:       "adequate",
		DocsDrift:           "none",
		ProposedShippable:   "auto",
		Shippable:           "auto",
		DigestArchDecisions: []byte(`[{"decision":"d1","rejectedAlternative":"r1","conventionConformance":"c1"}]`),
		SuppressedInShadow:  suppressedInShadow,
		ArchDecisionTags:    tags,
		ArchDecisionRoots:   roots,
	})
	if err != nil {
		t.Fatalf("insert review_verdicts row: %v", err)
	}
	return row
}

// TestListGatedArchDecisions_OverlapMatches proves the gate admits a
// verdict whose OWN tags overlap the query's tags, and a DIFFERENT
// verdict whose roots overlap even with disjoint tags -- the "tags OR
// roots" union the SQL's own doc comment claims.
func TestListGatedArchDecisions_OverlapMatches(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := narvipg.NewReviewVerdictStore(pool)
	const repo = "acme/gate-overlap"

	insertArchDecisionVerdict(ctx, t, store, repo, 1, "sha-tag-overlap", []byte(`["database"]`), []byte(`["other"]`), false)
	insertArchDecisionVerdict(ctx, t, store, repo, 2, "sha-root-overlap", []byte(`["unrelated-tag"]`), []byte(`["internal"]`), false)
	insertArchDecisionVerdict(ctx, t, store, repo, 3, "sha-no-overlap", []byte(`["unrelated-tag"]`), []byte(`["unrelated-root"]`), false)

	cands, err := store.ListGatedArchDecisions(ctx, repo, 0, []string{"database"}, []string{"internal"}, 20)
	if err != nil {
		t.Fatalf("ListGatedArchDecisions: %v", err)
	}

	gotSHAs := map[string]bool{}
	for _, c := range cands {
		gotSHAs[c.HeadSHA] = true
	}
	if !gotSHAs["sha-tag-overlap"] {
		t.Error("ListGatedArchDecisions did not return the tag-overlap verdict")
	}
	if !gotSHAs["sha-root-overlap"] {
		t.Error("ListGatedArchDecisions did not return the root-overlap verdict")
	}
	if gotSHAs["sha-no-overlap"] {
		t.Error("ListGatedArchDecisions returned the no-overlap verdict, want it excluded")
	}
}

// TestListGatedArchDecisions_NeverReadsBlastRadius pins §21.2's own
// never-gate-on-the-verdict rule directly: every row in this test carries
// the IDENTICAL blast_radius ("database"), but only the one whose own
// SERVER-DERIVED arch_decision_tags overlaps the query is returned --
// proving the gate's own overlap predicate is keyed on arch_decision_tags/
// arch_decision_roots, never on blast_radius, which an attacker who can
// only ever influence the posted verdict (never the server-computed
// classification) cannot reach.
func TestListGatedArchDecisions_NeverReadsBlastRadius(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := narvipg.NewReviewVerdictStore(pool)
	const repo = "acme/gate-blast-radius-immune"

	// Both rows post the SAME blast_radius ("database", set unconditionally
	// by insertArchDecisionVerdict), but only this one's arch_decision_tags
	// overlaps the query.
	insertArchDecisionVerdict(ctx, t, store, repo, 1, "sha-real-tag-match", []byte(`["database"]`), []byte(`[]`), false)
	insertArchDecisionVerdict(ctx, t, store, repo, 2, "sha-blast-radius-only", []byte(`["unrelated"]`), []byte(`[]`), false)

	cands, err := store.ListGatedArchDecisions(ctx, repo, 0, []string{"database"}, nil, 20)
	if err != nil {
		t.Fatalf("ListGatedArchDecisions: %v", err)
	}
	if len(cands) != 1 || cands[0].HeadSHA != "sha-real-tag-match" {
		t.Fatalf("ListGatedArchDecisions returned %+v, want exactly the one verdict whose OWN arch_decision_tags overlaps -- a match keyed on blast_radius instead would incorrectly return both", cands)
	}
}

// TestListGatedArchDecisions_ExcludesShadowEpoch proves the gate excludes
// a verdict stamped suppressed_in_shadow=true even though its own tags
// overlap the query -- the SAME in-query exclusion §30.8 requires
// elsewhere, extended here with no new stamp.
func TestListGatedArchDecisions_ExcludesShadowEpoch(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := narvipg.NewReviewVerdictStore(pool)
	const repo = "acme/gate-shadow-excluded"

	insertArchDecisionVerdict(ctx, t, store, repo, 1, "sha-shadow", []byte(`["database"]`), nil, true)

	cands, err := store.ListGatedArchDecisions(ctx, repo, 0, []string{"database"}, nil, 20)
	if err != nil {
		t.Fatalf("ListGatedArchDecisions: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("ListGatedArchDecisions returned %d candidate(s), want 0 (the one verdict on record is shadow-epoch)", len(cands))
	}
}

// TestListGatedArchDecisions_ExcludesContestedPR proves the gate excludes
// EVERY verdict for a PR that has ever had its arch-recap contested
// (review_digest_section_feedback) -- the per-(repo, pr, section)
// granularity this query's own doc comment names, deliberately coarser
// than a per-content-hash match.
func TestListGatedArchDecisions_ExcludesContestedPR(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := narvipg.NewReviewVerdictStore(pool)
	feedback := narvipg.NewReviewDigestSectionFeedbackStore(pool)
	const repo = "acme/gate-contested-excluded"
	const prNumber = int32(7)

	insertArchDecisionVerdict(ctx, t, store, repo, prNumber, "sha-contested", []byte(`["database"]`), nil, false)

	if _, _, err := feedback.Upsert(ctx, repo, prNumber, "arch_recap", "some-content-hash", "issue_comment", 1001, "this is wrong", pgtype.UUID{}); err != nil {
		t.Fatalf("seed contest: %v", err)
	}

	cands, err := store.ListGatedArchDecisions(ctx, repo, 0, []string{"database"}, nil, 20)
	if err != nil {
		t.Fatalf("ListGatedArchDecisions: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("ListGatedArchDecisions returned %d candidate(s), want 0 (this PR's own arch-recap has been contested)", len(cands))
	}

	// A DIFFERENT, never-contested PR in the SAME repo must still surface.
	insertArchDecisionVerdict(ctx, t, store, repo, prNumber+1, "sha-uncontested", []byte(`["database"]`), nil, false)
	cands, err = store.ListGatedArchDecisions(ctx, repo, 0, []string{"database"}, nil, 20)
	if err != nil {
		t.Fatalf("ListGatedArchDecisions (second call): %v", err)
	}
	if len(cands) != 1 || cands[0].HeadSHA != "sha-uncontested" {
		t.Fatalf("ListGatedArchDecisions returned %+v, want exactly the uncontested PR's own verdict", cands)
	}
}

// TestListRecentArchDecisions_IgnoresOverlapButKeepsExclusions proves the
// gate's own recency fallback (no tag/root predicate at all) still
// applies the SAME shadow-epoch and contested exclusions as
// ListGatedArchDecisions.
func TestListRecentArchDecisions_IgnoresOverlapButKeepsExclusions(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	store := narvipg.NewReviewVerdictStore(pool)
	feedback := narvipg.NewReviewDigestSectionFeedbackStore(pool)
	const repo = "acme/gate-recency-fallback"

	insertArchDecisionVerdict(ctx, t, store, repo, 1, "sha-unrelated-tags", []byte(`["totally-unrelated"]`), []byte(`["nowhere-near"]`), false)
	insertArchDecisionVerdict(ctx, t, store, repo, 2, "sha-shadow", []byte(`[]`), []byte(`[]`), true)
	insertArchDecisionVerdict(ctx, t, store, repo, 3, "sha-contested", []byte(`[]`), []byte(`[]`), false)
	if _, _, err := feedback.Upsert(ctx, repo, 3, "arch_recap", "hash", "issue_comment", 2002, "wrong", pgtype.UUID{}); err != nil {
		t.Fatalf("seed contest: %v", err)
	}

	cands, err := store.ListRecentArchDecisions(ctx, repo, 0, 20)
	if err != nil {
		t.Fatalf("ListRecentArchDecisions: %v", err)
	}
	if len(cands) != 1 || cands[0].HeadSHA != "sha-unrelated-tags" {
		t.Fatalf("ListRecentArchDecisions returned %+v, want exactly the one verdict that is neither shadow-epoch nor contested (its own mismatched tags/roots must not exclude it -- this query applies no overlap predicate at all)", cands)
	}
}
