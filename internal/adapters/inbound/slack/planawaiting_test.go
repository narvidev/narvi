package slack

import (
	"strings"
	"testing"
	"unicode"

	"github.com/narvidev/narvi/internal/domain/framecut"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
)

// TestAckPlanAwaitingText_CutPlan_OffersNoApprove pins the reply to a
// message that neither decides nor revises a plan awaiting approval. For a
// plan whose text was cut on its way from the sandbox (technical plan
// §6.1), which cannot be approved, it names no approve keyword and no
// Approve button, gives the reason, and points to the revise prefix, the
// reject keywords and the Request changes and Reject buttons. For a whole
// plan it is unchanged.
func TestAckPlanAwaitingText_CutPlan_OffersNoApprove(t *testing.T) {
	t.Parallel()

	cut := framecut.Cut{Kept: 20, Total: 40960}
	tests := []struct {
		name        string
		cut         *framecut.Cut
		wantApprove bool
	}{
		{name: "a cut plan", cut: &cut},
		{name: "a cut the server could not read", cut: &framecut.Malformed},
		{name: "a whole plan", wantApprove: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			text := ackPlanAwaitingText(tt.cut)
			words := strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) })
			offers := false
			for _, w := range words {
				for _, k := range plandomain.ApproveKeywords {
					offers = offers || w == k
				}
				offers = offers || w == "Approve"
			}
			if offers != tt.wantApprove {
				t.Errorf("text %q offers Approve: %v, want %v", text, offers, tt.wantApprove)
			}
			if !strings.Contains(text, strings.Join(plandomain.RejectKeywords, "/")) || !strings.Contains(text, `"`+plandomain.RevisePrefix+`"`) {
				t.Errorf("text %q, want the reject keywords and the revise prefix", text)
			}
			if tt.cut != nil && !strings.Contains(text, framecut.Reason(tt.cut)) {
				t.Errorf("text %q, want the reason %q", text, framecut.Reason(tt.cut))
			}
		})
	}
}
