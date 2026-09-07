//go:build integration

// Integration tests for §31.2/§31.6/§31.7's own "mode buffer" persistence
// half, through the real POST /sessions/{sessionID}/review/verdict
// surface (reviewverdict.go): review_verdicts.knowledge_mode/
// knowledge_influenced/arch_decision_tags/arch_decision_roots must all be
// forwarded verbatim from THIS session's own currently-processing turn --
// never recomputed, never left at their own zero value when the turn
// actually carried real stamps. reviewverdict_integration_test.go (this
// same package) already covers this endpoint's OTHER persisted columns
// (head_sha, risk_level, the digest_* columns); reviewverdictarchdecisions_
// integration_test.go (internal/adapters/outbound/postgres) already covers
// the SELECTOR half of this same feature (the gate query reading
// arch_decision_tags/roots back OUT of past verdicts) -- neither file
// asserts these four columns are ever actually WRITTEN by a real POST
// through this handler. An adversarial audit of this branch confirmed
// they can be silently reset to nil/""/false at appreviewverdict.Insert's
// own call site (reviewverdict.go) with this package's whole suite still
// reporting green.
package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/knowledge"
	"github.com/narvidev/narvi/internal/domain/reviewtriage"
)

// TestPostReviewVerdict_PersistsKnowledgeAndArchDecisionStamps is this
// Step's own end-to-end persistence proof. Each case seeds the posting
// session's own CURRENTLY-PROCESSING turn (turns.review_knowledge_mode/
// review_knowledge_decision/review_depth_decision) directly, exactly the
// way a real review-turn producer would have at turn-creation time (never
// fabricated some other way) -- mirrors TestPostReviewVerdict_
// PersistsReviewVerdictRow_WhenReviewHeadSHAKnown's own identical
// "seed via rig.turns.Create, then POST, then read back" shape, one
// column family further.
//
// knowledgeMode differs PER CASE ("mode_flip_test_a"/"mode_flip_test_b"),
// and is deliberately never knowledge.ModeA (the one value every real
// review-turn producer in this codebase actually writes today, internal/
// domain/knowledge/mode.go's own doc comment): PostReviewVerdict must
// forward THIS turn's own recorded stamp verbatim, never a value it
// re-derives on its own -- "the turn's mode, not the repository's current
// setting" (today there is no live per-repo setting to even diverge from,
// since the admin-facing switch does not exist yet, but the buffer's own
// entire justification is that a FUTURE flip must be attributable to
// whichever mode a given turn actually ran under, which requires this
// handler to be a faithful pass-through today, not a place that happens
// to agree with ModeA by coincidence). A handler that (the exact
// regression this test guards against) forwarded nil/""/false instead of
// reading the turn at all would produce a NULL knowledge_mode here --
// indistinguishable from correct only if this test asserted against
// knowledge.ModeA instead of these two distinct, arbitrary strings.
//
// knowledgeInfluenced is asserted BOTH ways in the SAME table test --
// true when the seeded turn's own InjectedRecord carries at least one id
// (its prompt actually carried an injected prior-decisions block), false
// when it carries none (an honestly empty record, the real shape
// FetchPriorArchDecisions returns for a repo with no history) -- a stamp
// that were hardcoded to always true, or always false, would still pass a
// test that only ever asserted one direction.
func TestPostReviewVerdict_PersistsKnowledgeAndArchDecisionStamps(t *testing.T) {
	wantArchTags := []string{"contracts", "postgres"}
	wantArchRoots := []string{"internal/domain", "internal/adapters"}
	triageRecordJSON, err := json.Marshal(reviewtriage.DecisionRecord{
		ArchDecisionTags:  wantArchTags,
		ArchDecisionRoots: wantArchRoots,
	})
	if err != nil {
		t.Fatalf("marshal seeded review_depth_decision: %v", err)
	}

	tests := []struct {
		name                    string
		knowledgeMode           string
		knowledgeDecision       knowledge.InjectedRecord
		wantKnowledgeInfluenced bool
	}{
		{
			name:          "turn's prompt carried an injected prior-decisions block -- influenced true",
			knowledgeMode: "mode_flip_test_a",
			knowledgeDecision: knowledge.InjectedRecord{
				IDs:           []string{"22222222-2222-2222-2222-222222222222:0"},
				ContentHashes: []string{"hash-case-true"},
			},
			wantKnowledgeInfluenced: true,
		},
		{
			name:                    "turn's prompt carried no injected block (an honestly empty repo history) -- influenced false",
			knowledgeMode:           "mode_flip_test_b",
			knowledgeDecision:       knowledge.InjectedRecord{},
			wantKnowledgeInfluenced: false,
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTestRig(t)
			ctx := context.Background()
			repoFullName := fmt.Sprintf("acme/verdict-knowledge-stamps-%d", i)
			prNumber := int32(300 + i)
			session := setupReviewSessionWithSandbox(ctx, t, rig, repoFullName, prNumber)

			reviewHeadSHA := fmt.Sprintf("sha-knowledge-stamps-%d", i)
			knowledgeDecisionJSON, err := json.Marshal(tc.knowledgeDecision)
			if err != nil {
				t.Fatalf("marshal seeded review_knowledge_decision: %v", err)
			}
			knowledgeMode := tc.knowledgeMode

			if _, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{
				SessionID:               session.ID,
				Status:                  sqlcgen.TurnStatusProcessing,
				ReviewHeadSha:           &reviewHeadSHA,
				ReviewDepthDecision:     triageRecordJSON,
				ReviewKnowledgeMode:     &knowledgeMode,
				ReviewKnowledgeDecision: knowledgeDecisionJSON,
			}); err != nil {
				t.Fatalf("seed processing turn with knowledge/arch-decision stamps: %v", err)
			}

			status, _ := postReviewVerdict(t, rig, session.ID.String(), "sandbox-bearer-token", "1", validVerdictRequestJSON())
			if status != http.StatusCreated {
				t.Fatalf("status = %d, want %d", status, http.StatusCreated)
			}

			var gotKnowledgeMode *string
			var gotKnowledgeInfluenced bool
			var gotArchTagsJSON, gotArchRootsJSON []byte
			if err := rig.pool.QueryRow(ctx,
				`SELECT knowledge_mode, knowledge_influenced, arch_decision_tags, arch_decision_roots FROM review_verdicts WHERE repo_full_name = $1 AND pr_number = $2`,
				repoFullName, prNumber,
			).Scan(&gotKnowledgeMode, &gotKnowledgeInfluenced, &gotArchTagsJSON, &gotArchRootsJSON); err != nil {
				t.Fatalf("query review_verdicts knowledge/arch-decision columns: %v", err)
			}

			if gotKnowledgeMode == nil || *gotKnowledgeMode != tc.knowledgeMode {
				t.Errorf("knowledge_mode = %v, want %q (the TURN's own stamp, forwarded verbatim -- never recomputed, never left NULL)", gotKnowledgeMode, tc.knowledgeMode)
			}
			if gotKnowledgeInfluenced != tc.wantKnowledgeInfluenced {
				t.Errorf("knowledge_influenced = %v, want %v", gotKnowledgeInfluenced, tc.wantKnowledgeInfluenced)
			}

			var gotArchTags, gotArchRoots []string
			if err := json.Unmarshal(gotArchTagsJSON, &gotArchTags); err != nil {
				t.Fatalf("unmarshal arch_decision_tags: %v", err)
			}
			if err := json.Unmarshal(gotArchRootsJSON, &gotArchRoots); err != nil {
				t.Fatalf("unmarshal arch_decision_roots: %v", err)
			}
			if !slices.Equal(gotArchTags, wantArchTags) {
				t.Errorf("arch_decision_tags = %v, want %v", gotArchTags, wantArchTags)
			}
			if !slices.Equal(gotArchRoots, wantArchRoots) {
				t.Errorf("arch_decision_roots = %v, want %v", gotArchRoots, wantArchRoots)
			}
		})
	}
}
