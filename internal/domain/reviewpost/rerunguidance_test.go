package reviewpost_test

import (
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/domain/reviewpost"
)

// TestRerunGuidance_ContainsMention proves the rendered guidance embeds a
// literal "@handle" mention, surrounded by plain whitespace on both sides
// -- the shape internal/adapters/inbound/github's own compileMentionPattern
// regex requires (see that package's own rerunguidance_test.go for the
// real, executable proof against that actual regex -- this test only
// covers what this package alone can assert without importing an inbound
// adapter).
func TestRerunGuidance_ContainsMention(t *testing.T) {
	got := reviewpost.RerunGuidance("narvi-bot")

	if !strings.Contains(got, " @narvi-bot ") {
		t.Errorf("RerunGuidance(%q) = %q, want it to contain %q (a space-delimited mention)", "narvi-bot", got, " @narvi-bot ")
	}
}

// TestRerunGuidance_DifferentHandles proves the handle is actually
// interpolated, not a hardcoded literal.
func TestRerunGuidance_DifferentHandles(t *testing.T) {
	a := reviewpost.RerunGuidance("narvi-bot")
	b := reviewpost.RerunGuidance("other-bot")
	if a == b {
		t.Errorf("RerunGuidance produced identical text for different bot handles")
	}
	if !strings.Contains(b, "@other-bot") {
		t.Errorf("RerunGuidance(%q) = %q, want it to contain the given handle", "other-bot", b)
	}
}

// TestRerunGuidance_EmptyHandle_ButtonOnly proves a deployment with no bot
// handle -- GitHub outbound on, GitHub ingress off (§12.5), where
// platform.Load stores no handle -- gets the web button alone: never a
// bare "@" mention, and never the re-review label, since only the GitHub
// webhook listens for either.
func TestRerunGuidance_EmptyHandle_ButtonOnly(t *testing.T) {
	got := reviewpost.RerunGuidance("")

	if strings.Contains(got, "@") {
		t.Errorf("RerunGuidance(\"\") = %q, want no mention at all", got)
	}
	if strings.Contains(strings.ToLower(got), "label") {
		t.Errorf("RerunGuidance(\"\") = %q, want no re-review label: only the GitHub webhook detects one", got)
	}
	if !strings.Contains(got, "Re-run review button") {
		t.Errorf("RerunGuidance(\"\") = %q, want it to point at the web review screen's Re-run review button", got)
	}
}
