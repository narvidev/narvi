package ops

import (
	"strings"
	"testing"
)

// TestDeferredDecisionsNameATrigger is the mechanism docs/DECISIONS.md's
// own deferred section promises: a deferral with no reopen condition is
// indistinguishable from an item nobody got round to, so CI refuses it.
func TestDeferredDecisionsNameATrigger(t *testing.T) {
	t.Parallel()

	decisions, err := LoadDeferredDecisions(repoRoot(t))
	if err != nil {
		t.Fatalf("LoadDeferredDecisions: %v", err)
	}

	if bad := CheckDeferredTriggers(decisions); len(bad) > 0 {
		t.Errorf("docs/DECISIONS.md: these deferred decisions name no condition that would reopen "+
			"them: %s\n\nA deferral is a decision and belongs in that table; a deferral with no "+
			"trigger is an item nobody got round to wearing a decision's clothes. Write the "+
			"condition someone could actually evaluate, not \"later\".", strings.Join(bad, ", "))
	}
}

// TestDeferredDecisionsTableIsFound guards the scanner itself. A parser
// keyed on a heading fails open the day the heading is renamed: it finds
// nothing, reports nothing, and the check above passes while verifying
// nothing -- this repository's own dominant defect, in the code written
// to prevent it.
func TestDeferredDecisionsTableIsFound(t *testing.T) {
	t.Parallel()

	decisions, err := LoadDeferredDecisions(repoRoot(t))
	if err != nil {
		t.Fatalf("LoadDeferredDecisions: %v", err)
	}
	if len(decisions) == 0 {
		t.Fatal("parsed zero deferred decisions: LoadDeferredDecisions must error rather than " +
			"return an empty slice, since an empty result reads as \"nothing deferred\"")
	}
	for _, d := range decisions {
		if d.Subject == "" {
			t.Errorf("a deferred row parsed with an empty subject: %+v", d)
		}
	}
}
