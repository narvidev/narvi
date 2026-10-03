package linear

import (
	"strings"
	"testing"
	"unicode"

	"github.com/narvidev/narvi/internal/domain/framecut"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
)

// TestPlanAwaitingApprovalReplyText_CutPlan_OffersNoApprove pins the
// notice posted for a reply that neither decides nor revises a plan
// awaiting approval. For a plan whose text was cut on its way from the
// sandbox (technical plan §6.1), which cannot be approved, it offers no
// approve keyword, gives the reason, and points to the revise prefix and
// the reject keywords. For a whole plan it is unchanged.
func TestPlanAwaitingApprovalReplyText_CutPlan_OffersNoApprove(t *testing.T) {
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
			text := planAwaitingApprovalReplyText(tt.cut)
			offers := false
			for _, w := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) }) {
				for _, k := range plandomain.ApproveKeywords {
					offers = offers || w == k
				}
			}
			if offers != tt.wantApprove {
				t.Errorf("text %q offers an approve keyword: %v, want %v", text, offers, tt.wantApprove)
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
