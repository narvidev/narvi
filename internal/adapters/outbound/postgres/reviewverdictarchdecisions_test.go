package postgres

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// TestCandidatesFromReviewVerdict covers the row -> []knowledge.Candidate
// explosion this file's own top doc comment describes: one row's
// digest_arch_decisions array becomes one Candidate per entry, sharing
// that row's own tags/roots/provenance; a malformed or empty JSON column
// degrades to zero candidates from that row, never an error.
func TestCandidatesFromReviewVerdict(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	baseRow := func() sqlcgen.ReviewVerdict {
		var id pgtype.UUID
		_ = id.Scan("11111111-1111-1111-1111-111111111111")
		return sqlcgen.ReviewVerdict{
			ID:           id,
			RepoFullName: "acme/widgets",
			PrNumber:     42,
			HeadSha:      "deadbeef",
			CreatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		}
	}

	tests := []struct {
		name string
		row  func() sqlcgen.ReviewVerdict
		want int // number of candidates expected
	}{
		{
			name: "two decisions in one row produce two candidates",
			row: func() sqlcgen.ReviewVerdict {
				r := baseRow()
				r.DigestArchDecisions = []byte(`[{"decision":"d1","rejectedAlternative":"r1","conventionConformance":"c1"},{"decision":"d2","rejectedAlternative":"r2","conventionConformance":"c2"}]`)
				r.ArchDecisionTags = []byte(`["database"]`)
				r.ArchDecisionRoots = []byte(`["internal"]`)
				r.KnowledgeInfluenced = true
				return r
			},
			want: 2,
		},
		{
			name: "empty digest_arch_decisions produces zero candidates",
			row: func() sqlcgen.ReviewVerdict {
				r := baseRow()
				r.DigestArchDecisions = []byte(`[]`)
				return r
			},
			want: 0,
		},
		{
			name: "nil digest_arch_decisions produces zero candidates",
			row: func() sqlcgen.ReviewVerdict {
				return baseRow()
			},
			want: 0,
		},
		{
			name: "malformed digest_arch_decisions degrades to zero candidates, never an error",
			row: func() sqlcgen.ReviewVerdict {
				r := baseRow()
				r.DigestArchDecisions = []byte(`not-json`)
				return r
			},
			want: 0,
		},
		{
			name: "malformed tags/roots degrade to nil, decisions still produced",
			row: func() sqlcgen.ReviewVerdict {
				r := baseRow()
				r.DigestArchDecisions = []byte(`[{"decision":"d1","rejectedAlternative":"r1","conventionConformance":"c1"}]`)
				r.ArchDecisionTags = []byte(`not-json`)
				r.ArchDecisionRoots = []byte(`not-json`)
				return r
			},
			want: 1,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := candidatesFromReviewVerdict(tt.row())
			if len(got) != tt.want {
				t.Fatalf("candidatesFromReviewVerdict() returned %d candidates, want %d (%+v)", len(got), tt.want, got)
			}
		})
	}
}

// TestCandidatesFromReviewVerdict_FieldsAndIDs pins the exact shape of a
// converted candidate: the "<verdict id>:<index>" ID convention
// (docs/design/boundaries-design.md, section 2.2), and every per-verdict
// field (RepoFullName/PRNumber/HeadSHA/Tags/Roots/CreatedAt/
// KnowledgeInfluenced) copied verbatim onto EVERY candidate the row
// produces.
//
// Mutation-verified: temporarily changing the ID format string from
// "%s:%d" to "%s-%d" made this test's exact-ID assertions fail; reverted
// after confirming the failure.
func TestCandidatesFromReviewVerdict_FieldsAndIDs(t *testing.T) {
	t.Parallel()

	var id pgtype.UUID
	if err := id.Scan("11111111-1111-1111-1111-111111111111"); err != nil {
		t.Fatalf("id.Scan: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	row := sqlcgen.ReviewVerdict{
		ID:                  id,
		RepoFullName:        "acme/widgets",
		PrNumber:            42,
		HeadSha:             "deadbeef",
		CreatedAt:           pgtype.Timestamptz{Time: now, Valid: true},
		DigestArchDecisions: []byte(`[{"decision":"d1","rejectedAlternative":"r1","conventionConformance":"c1"},{"decision":"d2","rejectedAlternative":"r2","conventionConformance":"c2"}]`),
		ArchDecisionTags:    []byte(`["database","auth"]`),
		ArchDecisionRoots:   []byte(`["internal"]`),
		KnowledgeInfluenced: true,
	}

	got := candidatesFromReviewVerdict(row)
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2", len(got))
	}

	wantIDs := []string{"11111111-1111-1111-1111-111111111111:0", "11111111-1111-1111-1111-111111111111:1"}
	for i, c := range got {
		if c.ID != wantIDs[i] {
			t.Errorf("candidate[%d].ID = %q, want %q", i, c.ID, wantIDs[i])
		}
		if c.VerdictID != "11111111-1111-1111-1111-111111111111" {
			t.Errorf("candidate[%d].VerdictID = %q, want the verdict's own id", i, c.VerdictID)
		}
		if c.RepoFullName != "acme/widgets" || c.PRNumber != 42 || c.HeadSHA != "deadbeef" {
			t.Errorf("candidate[%d] per-verdict fields not copied verbatim: %+v", i, c)
		}
		if !c.KnowledgeInfluenced {
			t.Errorf("candidate[%d].KnowledgeInfluenced = false, want true (copied from the row)", i)
		}
		if len(c.Tags) != 2 || len(c.Roots) != 1 {
			t.Errorf("candidate[%d] tags/roots not copied verbatim: tags=%v roots=%v", i, c.Tags, c.Roots)
		}
		if !c.CreatedAt.Equal(now) {
			t.Errorf("candidate[%d].CreatedAt = %v, want %v", i, c.CreatedAt, now)
		}
	}
	if got[0].Decision != "d1" || got[1].Decision != "d2" {
		t.Errorf("decisions not mapped in order: %+v", got)
	}
}
