package httpapi

// This file (decisioninboxitemtodto_test.go) unit-tests
// decisionInboxItemToDTO directly (a white-box, package-internal test,
// mirroring planfollowupprivate_test.go's own identical "package httpapi,
// not httpapi_test" precedent) -- no Postgres, no HTTP round trip. Its
// one job is finding F3b (adversarial review): a previous version of
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
