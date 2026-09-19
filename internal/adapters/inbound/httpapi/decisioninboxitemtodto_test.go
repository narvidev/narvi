package httpapi

// This file (decisioninboxitemtodto_test.go) unit-tests
// decisionInboxItemToDTO directly (a white-box, package-internal test,
// mirroring planfollowupprivate_test.go's own identical "package httpapi,
// not httpapi_test" precedent) -- no Postgres, no HTTP round trip. Its
// first job was finding F3b (adversarial review): a previous version of
// decisionInboxItemToDTO's own acceptance-rendering gate keyed on
// `it.AcceptanceJustification != ""` rather than `it.AcceptanceID != ""`
// -- an inference that only happened to be correct because httpapi.
// AcceptReviewVerdict's OWN separate required-justification check
// (decisioninbox.go) meant an Item's own AcceptanceJustification could
// never legitimately be empty while AcceptanceID was set. That coupling
// was never enforced BY this function, and nothing pinned it: deleting
// the required-justification check elsewhere left this gate silently
// wrong, with no test catching it. A full HTTP/Postgres round trip
// (acceptreviewverdict_integration_test.go) cannot construct the
// AcceptanceID-set/AcceptanceJustification-empty state this test needs
// to isolate -- the real accept-verdict endpoint always supplies a
// non-empty justification -- so this function is exercised directly,
// with a hand-built Item, instead.
//
// T2 (round 4, adversarial review) added a second job: the
// AcceptanceMergeable/AcceptanceMergeBlockedReason mapping (lines 209-213
// of decisioninbox.go, as of this fix) was the ENTIRE seam round 3's own
// fix passed through -- Item.AcceptanceMergeable computed server-side by
// aggregate.go, then mapped here onto the wire -- and had NO test of its
// own at any layer: a full HTTP/Postgres round trip exercises Build's own
// computation, but nothing pinned this FUNCTION's own mapping in
// isolation, so a one-line regression here (e.g. swapping the `!it.
// AcceptanceMergeable` guard for `it.AcceptanceMergeable`) would have
// silently reintroduced exactly the defect round 3 was fixing, with every
// existing test still green.

import (
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/app/decisioninbox"
	domaindecisioninbox "github.com/narvidev/narvi/internal/domain/decisioninbox"
)

// TestDecisionInboxItemToDTO_AcceptanceRendersOnIDNotJustification pins
// finding F3b directly: an Item whose own AcceptanceID is set but whose
// AcceptanceJustification is EMPTY (a shape the real accept-verdict
// endpoint's own required-justification check never actually produces,
// but that decisionInboxItemToDTO itself must not silently rely on) must
// still render the acceptance block on the wire -- acceptanceId non-nil,
// acceptedAt non-nil -- because AcceptanceID, not AcceptanceJustification,
// is the field aggregate.go's own buildPROpenItem sets under the
// Applicable/acceptanceContextStillFresh gate (Item.AcceptanceID's own
// doc comment): it is the TRUE existence signal.
func TestDecisionInboxItemToDTO_AcceptanceRendersOnIDNotJustification(t *testing.T) {
	t.Parallel()

	acceptedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	it := decisioninbox.Item{
		Kind:                    domaindecisioninbox.KindNeedsReview,
		RepoFullName:            "acme/widgets",
		PRNumber:                42,
		Provenance:              &domaindecisioninbox.Provenance{Kind: domaindecisioninbox.ProvenanceDirect},
		AcceptanceID:            "acceptance-1",
		AcceptanceJustification: "", // deliberately empty -- see this file's own doc comment
		AcceptedAt:              acceptedAt,
		AcceptedByUserID:        "user-1",
	}

	dto := decisionInboxItemToDTO(it)

	if dto.AcceptanceId == nil || *dto.AcceptanceId != "acceptance-1" {
		t.Errorf("AcceptanceId = %v, want a non-nil pointer to %q -- rendering must key on AcceptanceID, never on AcceptanceJustification's own emptiness", dto.AcceptanceId, "acceptance-1")
	}
	if dto.AcceptedAt == nil || !dto.AcceptedAt.Equal(acceptedAt) {
		t.Errorf("AcceptedAt = %v, want a non-nil pointer to %v", dto.AcceptedAt, acceptedAt)
	}
	if dto.AcceptedBy == nil || *dto.AcceptedBy != "user-1" {
		t.Errorf("AcceptedBy = %v, want a non-nil pointer to %q (finding F6, adversarial review)", dto.AcceptedBy, "user-1")
	}
	// AcceptanceJustification itself legitimately renders as a pointer to
	// the empty string here (never nil) -- it is STILL populated
	// unconditionally alongside the other three once the gate admits this
	// row; this test's own point is about what GATES rendering, not about
	// this one field's own value.
	if dto.AcceptanceJustification == nil {
		t.Error("AcceptanceJustification = nil, want a non-nil pointer (even to an empty string) once the acceptance block itself renders")
	}
}

// TestDecisionInboxItemToDTO_NoAcceptanceIDOmitsAcceptanceBlock is the
// mirror negative case: AcceptanceID empty (the common "never accepted"
// case) must omit the whole acceptance block, regardless of what the
// other three fields happen to hold (defense in depth -- buildPROpenItem
// never sets them independently, but this function must not assume that).
func TestDecisionInboxItemToDTO_NoAcceptanceIDOmitsAcceptanceBlock(t *testing.T) {
	t.Parallel()

	it := decisioninbox.Item{
		Kind:         domaindecisioninbox.KindNeedsReview,
		RepoFullName: "acme/widgets",
		PRNumber:     43,
		Provenance:   &domaindecisioninbox.Provenance{Kind: domaindecisioninbox.ProvenanceDirect},
		AcceptanceID: "",
	}

	dto := decisionInboxItemToDTO(it)

	if dto.AcceptanceId != nil {
		t.Errorf("AcceptanceId = %v, want nil when AcceptanceID is empty", *dto.AcceptanceId)
	}
	if dto.AcceptanceJustification != nil {
		t.Errorf("AcceptanceJustification = %v, want nil when AcceptanceID is empty", *dto.AcceptanceJustification)
	}
	if dto.AcceptedAt != nil {
		t.Errorf("AcceptedAt = %v, want nil when AcceptanceID is empty", *dto.AcceptedAt)
	}
	if dto.AcceptedBy != nil {
		t.Errorf("AcceptedBy = %v, want nil when AcceptanceID is empty", *dto.AcceptedBy)
	}
}

// TestDecisionInboxItemToDTO_AcceptanceMergeableTrueOmitsReason pins T2
// (round 4, adversarial review): an Item reporting AcceptanceMergeable=
// true must render acceptanceMergeable=true on the wire, and MUST leave
// acceptanceMergeBlockedReason absent -- decisioninbox.go's own gate
// (`!it.AcceptanceMergeable && it.AcceptanceMergeBlockedReason != ""`)
// never sets the DTO pointer at all when AcceptanceMergeable is true,
// mirroring Item.AcceptanceMergeBlockedReason's own doc comment ("empty
// when AcceptanceMergeable is true", pinned server-side by every
// TestBuild_AcceptanceMergeable-family test's own identical assertion --
// this test pins the SAME fact at the DTO-mapping seam those tests never
// reach).
//
// AcceptanceMergeBlockedReason is deliberately set to a NON-EMPTY string
// here even though AcceptanceMergeable is true -- a combination
// buildPROpenItem itself never actually produces (aggregate.go's own
// switch only ever sets the reason inside a case that leaves
// AcceptanceMergeable at its false zero value), but this MAPPING
// function must not silently depend on that upstream invariant holding:
// defense in depth, mirroring
// TestDecisionInboxItemToDTO_NoAcceptanceIDOmitsAcceptanceBlock's own
// identical "regardless of what the other fields happen to hold"
// discipline a few lines up. This also means the test is NOT vacuous --
// an empty-reason fixture would pass even if the mapping's own guard
// dropped the AcceptanceMergeable check entirely and rendered on
// reason-non-empty alone (a real mutation caught while writing this
// test, restored after confirming the failure).
//
// Mutation-test target: this is exactly the seam T2's own report names --
// see TestDecisionInboxItemToDTO_AcceptanceMergeableFalseRendersReason's
// own doc comment for the OTHER mutation this pair jointly catches.
func TestDecisionInboxItemToDTO_AcceptanceMergeableTrueOmitsReason(t *testing.T) {
	t.Parallel()

	it := decisioninbox.Item{
		Kind:                         domaindecisioninbox.KindNeedsReview,
		RepoFullName:                 "acme/widgets",
		PRNumber:                     44,
		Provenance:                   &domaindecisioninbox.Provenance{Kind: domaindecisioninbox.ProvenanceDirect},
		AcceptanceID:                 "acceptance-2",
		AcceptanceMergeable:          true,
		AcceptanceMergeBlockedReason: "a reason that must never surface once AcceptanceMergeable is true",
	}

	dto := decisionInboxItemToDTO(it)

	if dto.AcceptanceMergeable == nil || *dto.AcceptanceMergeable != true {
		t.Errorf("AcceptanceMergeable = %v, want a non-nil pointer to true", dto.AcceptanceMergeable)
	}
	if dto.AcceptanceMergeBlockedReason != nil {
		t.Errorf("AcceptanceMergeBlockedReason = %v, want nil (absent from the wire) when AcceptanceMergeable is true", *dto.AcceptanceMergeBlockedReason)
	}
}

// TestDecisionInboxItemToDTO_AcceptanceMergeableFalseRendersReason is
// this pair's other half: AcceptanceMergeable=false WITH a non-empty
// reason must render BOTH -- acceptanceMergeable=false and
// acceptanceMergeBlockedReason=<the reason>, verbatim.
//
// Mutation-test target: swapping decisioninbox.go's own
// `if !it.AcceptanceMergeable && it.AcceptanceMergeBlockedReason != ""`
// guard for `if it.AcceptanceMergeable && ...` (inverting the sense of
// the first half) must turn THIS test's own non-nil-reason assertion
// into a failure (dto.AcceptanceMergeBlockedReason would read nil) while
// simultaneously turning
// TestDecisionInboxItemToDTO_AcceptanceMergeableTrueOmitsReason's own
// nil assertion into a failure the other way (a reason would render on a
// mergeable row) -- the two tests jointly pin both directions of the
// gate, exactly the seam a one-line regression here would silently pass
// through undetected before this fix.
func TestDecisionInboxItemToDTO_AcceptanceMergeableFalseRendersReason(t *testing.T) {
	t.Parallel()

	const reason = "this pull request has an open, unresolved review finding"
	it := decisioninbox.Item{
		Kind:                         domaindecisioninbox.KindNeedsReview,
		RepoFullName:                 "acme/widgets",
		PRNumber:                     45,
		Provenance:                   &domaindecisioninbox.Provenance{Kind: domaindecisioninbox.ProvenanceDirect},
		AcceptanceID:                 "acceptance-3",
		AcceptanceMergeable:          false,
		AcceptanceMergeBlockedReason: reason,
	}

	dto := decisionInboxItemToDTO(it)

	if dto.AcceptanceMergeable == nil || *dto.AcceptanceMergeable != false {
		t.Errorf("AcceptanceMergeable = %v, want a non-nil pointer to false", dto.AcceptanceMergeable)
	}
	if dto.AcceptanceMergeBlockedReason == nil || *dto.AcceptanceMergeBlockedReason != reason {
		t.Errorf("AcceptanceMergeBlockedReason = %v, want a non-nil pointer to %q", dto.AcceptanceMergeBlockedReason, reason)
	}
}

// TestDecisionInboxItemToDTO_AcceptanceMergeableFalseEmptyReasonOmitsField
// pins T5's own exact degenerate combination (round 4, adversarial
// review): AcceptanceMergeable=false with an EMPTY
// AcceptanceMergeBlockedReason despite a non-empty AcceptanceID --
// buildPROpenItem's own isHandoffPR/isReleaseCut branches (aggregate.go)
// each guard their AcceptanceMergeBlockedReason assignment on
// `acceptanceID != ""`, so neither can produce this exact combination as
// written; forced directly below so this test's own DTO-mapping coverage
// never depends on that guard actually firing.
// must render acceptanceMergeable=false while leaving
// acceptanceMergeBlockedReason ABSENT from the JSON entirely -- never a
// pointer to "". This is the exact shape that arrives client-side as
// `undefined`, not `null` (contracts/gen/ts/rest-dtos.ts's own
// `acceptanceMergeBlockedReason?: string | null`), which is what T5's own
// DecisionInboxView.tsx guard fix (`!= null`, catching both) exists to
// handle -- this test pins that the SERVER actually produces this
// combination, which is the premise T5's own fix depends on.
func TestDecisionInboxItemToDTO_AcceptanceMergeableFalseEmptyReasonOmitsField(t *testing.T) {
	t.Parallel()

	it := decisioninbox.Item{
		Kind:                         domaindecisioninbox.KindAwaitingApproval,
		RepoFullName:                 "acme/widgets",
		PRNumber:                     46,
		Provenance:                   &domaindecisioninbox.Provenance{Kind: domaindecisioninbox.ProvenanceDirect},
		IsHandoff:                    true,
		AcceptanceID:                 "acceptance-4",
		AcceptanceMergeable:          false,
		AcceptanceMergeBlockedReason: "this pull request is a handoff item, not an ordinary code-review merge decision",
	}
	// This test's own point is the EMPTY-reason combination specifically
	// -- overwrite the fixture's initial (correct, T6-fixed) reason with
	// the empty string a still-uncomputed field would carry, so this test
	// does not depend on buildPROpenItem's own T6 fix to construct its
	// input.
	it.AcceptanceMergeBlockedReason = ""

	dto := decisionInboxItemToDTO(it)

	if dto.AcceptanceMergeable == nil || *dto.AcceptanceMergeable != false {
		t.Errorf("AcceptanceMergeable = %v, want a non-nil pointer to false", dto.AcceptanceMergeable)
	}
	if dto.AcceptanceMergeBlockedReason != nil {
		t.Errorf("AcceptanceMergeBlockedReason = %v, want nil (absent from the JSON, arriving client-side as undefined) when the Go string is empty", *dto.AcceptanceMergeBlockedReason)
	}
}
