package turn_test

import (
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/narvidev/narvi/internal/domain/turn"
)

func strptr(s string) *string { return &s }

// TestReadAgentEnd covers every row of the reader's table (technical plan
// §35.4), the first matching row deciding.
func TestReadAgentEnd(t *testing.T) {
	t.Parallel()
	legacy := strptr(turn.LegacyInterruptionReason)
	stopping := strptr("sandbox_stopping")
	ownEnd := turn.EndReading{}
	typed := turn.EndReading{Interrupted: true, Source: turn.InterruptionSourceTyped, Kind: "sandbox_stopping"}
	legacyText := turn.EndReading{Interrupted: true, Source: turn.InterruptionSourceLegacyText}
	tests := []struct {
		name string
		end  turn.AgentEnd
		want turn.EndReading
	}{
		// Row 1: a sub-task's end ends no turn.
		{name: "row 1: a sub-task's end with the key", end: turn.AgentEnd{Outcome: "cancelled", Reason: legacy, Interruption: stopping, SubTask: true}, want: ownEnd},
		{name: "row 1: a sub-task's end with the legacy text", end: turn.AgentEnd{Outcome: "cancelled", Reason: legacy, SubTask: true}, want: ownEnd},
		{name: "row 1: a sub-task's completed end with the key", end: turn.AgentEnd{Outcome: "completed", Interruption: stopping, SubTask: true}, want: ownEnd},
		// Row 2: a completed end is a completion, whatever key it carries.
		{name: "row 2: completed", end: turn.AgentEnd{Outcome: "completed"}, want: ownEnd},
		{name: "row 2: completed with the key", end: turn.AgentEnd{Outcome: "completed", Interruption: stopping}, want: turn.EndReading{KeyOnCompletion: true}},
		{name: "row 2: completed with a key this release does not know", end: turn.AgentEnd{Outcome: "completed", Interruption: strptr("later_value")}, want: turn.EndReading{KeyOnCompletion: true}},
		{name: "row 2: completed with an empty key", end: turn.AgentEnd{Outcome: "completed", Interruption: strptr("")}, want: ownEnd},
		{name: "row 2: completed with the legacy text", end: turn.AgentEnd{Outcome: "completed", Reason: legacy}, want: ownEnd},
		{name: "row 2: completed on a stopped turn with the key", end: turn.AgentEnd{Outcome: "completed", Interruption: stopping, StopRequested: true}, want: turn.EndReading{KeyOnCompletion: true}},
		// Row 3: a person's stop gets its own end.
		{name: "row 3: a stopped turn's end with the key", end: turn.AgentEnd{Outcome: "cancelled", Reason: legacy, Interruption: stopping, StopRequested: true}, want: ownEnd},
		{name: "row 3: a stopped turn's end with the legacy text", end: turn.AgentEnd{Outcome: "cancelled", Reason: legacy, StopRequested: true}, want: ownEnd},
		{name: "row 3: a stopped turn's failed end with the key", end: turn.AgentEnd{Outcome: "failed", Interruption: stopping, StopRequested: true}, want: ownEnd},
		// Row 4: the key is an interruption, whatever the reason.
		{name: "row 4: cancelled with the key", end: turn.AgentEnd{Outcome: "cancelled", Reason: legacy, Interruption: stopping}, want: typed},
		{name: "row 4: failed with the key", end: turn.AgentEnd{Outcome: "failed", Reason: strptr("opencode: APIError"), Interruption: stopping}, want: typed},
		{name: "row 4: the key with no reason", end: turn.AgentEnd{Outcome: "cancelled", Interruption: stopping}, want: typed},
		{name: "row 4: a key this release does not know", end: turn.AgentEnd{Outcome: "failed", Interruption: strptr("later_value")},
			want: turn.EndReading{Interrupted: true, Source: turn.InterruptionSourceTyped, Kind: "later_value"}},
		// Row 5: no key, the legacy text compared whole on a cancelled end.
		{name: "row 5: the legacy text", end: turn.AgentEnd{Outcome: "cancelled", Reason: legacy}, want: legacyText},
		{name: "row 5: the legacy text with an empty key", end: turn.AgentEnd{Outcome: "cancelled", Reason: legacy, Interruption: strptr("")}, want: legacyText},
		{name: "row 5 does not match: the legacy text on a failed end", end: turn.AgentEnd{Outcome: "failed", Reason: legacy}, want: ownEnd},
		// Row 6: anything else.
		{name: "row 6: cancelled with no reason (the runtime's own abort)", end: turn.AgentEnd{Outcome: "cancelled"}, want: ownEnd},
		{name: "row 6: cancelled, another reason", end: turn.AgentEnd{Outcome: "cancelled", Reason: strptr("stopped")}, want: ownEnd},
		{name: "row 6: failed", end: turn.AgentEnd{Outcome: "failed", Reason: strptr("opencode: turn produced no output")}, want: ownEnd},
		{name: "row 6: an outcome this release does not know", end: turn.AgentEnd{Outcome: "paused", Reason: legacy}, want: ownEnd},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := turn.ReadAgentEnd(tc.end); got != tc.want {
				t.Errorf("ReadAgentEnd(%+v) = %+v, want %+v", tc.end, got, tc.want)
			}
		})
	}
}

// TestReadAgentEnd_TheKeyAlwaysDecides pins the order of the reader's two
// sources: the typed key decides whenever it is present, on any reason
// and any uncompleted outcome, a completed end stays a completion with it,
// and the legacy text decides only whole, without the key -- a reason one
// character away from it is the turn's own end.
func TestReadAgentEnd_TheKeyAlwaysDecides(t *testing.T) {
	t.Parallel()
	reasons := []*string{
		nil, strptr(""), strptr(turn.LegacyInterruptionReason), strptr("opencode: APIError"),
		strptr("opencode: turn produced no output"), strptr("stopped"),
		strptr("opencode: APIError (transient retry attempted and failed: backoff interrupted after 30s)"),
	}
	for _, key := range []string{"sandbox_stopping", "sandbox_lifetime", "x"} {
		for _, outcome := range []string{"failed", "cancelled"} {
			for _, reason := range reasons {
				end := turn.AgentEnd{Outcome: outcome, Reason: reason, Interruption: strptr(key)}
				want := turn.EndReading{Interrupted: true, Source: turn.InterruptionSourceTyped, Kind: key}
				if got := turn.ReadAgentEnd(end); got != want {
					t.Errorf("ReadAgentEnd(%s, %v, key %q) = %+v, want %+v: the key decides", outcome, derefOr(reason), key, got, want)
				}
			}
		}
		for _, reason := range reasons {
			end := turn.AgentEnd{Outcome: "completed", Reason: reason, Interruption: strptr(key)}
			if got := turn.ReadAgentEnd(end); got.Interrupted || !got.KeyOnCompletion {
				t.Errorf("ReadAgentEnd(completed, %v, key %q) = %+v, want a completion that carried the key", derefOr(reason), key, got)
			}
		}
	}

	for _, reason := range nearLegacyReasons() {
		end := turn.AgentEnd{Outcome: "cancelled", Reason: strptr(reason)}
		if got := turn.ReadAgentEnd(end); got != (turn.EndReading{}) {
			t.Errorf("ReadAgentEnd(cancelled, %q) = %+v, want the turn's own end: only the whole legacy text is an interruption", reason, got)
		}
	}
}

// nearLegacyReasons are reasons one character away from the legacy text
// -- each character changed, each dropped, one added at either end -- and
// two near misses: the text in another case, and the text without its
// runtime's prefix.
func nearLegacyReasons() []string {
	legacy := turn.LegacyInterruptionReason
	var out []string
	for i := range len(legacy) {
		changed := []byte(legacy)
		if changed[i] == 'x' {
			changed[i] = 'y'
		} else {
			changed[i] = 'x'
		}
		out = append(out, string(changed), legacy[:i]+legacy[i+1:])
	}
	return append(out, legacy+".", " "+legacy, strings.ToUpper(legacy), strings.TrimPrefix(legacy, "opencode: "))
}

// TestReadAgentEnd_ElapsedSecondsTrap pins the trap technical plan §35.4
// names: a reason that embeds the time the turn ran is never read for it.
// A failed end, a cancelled end, and the legacy text with its seconds
// appended are all the turn's own end -- the reader compares the legacy
// text whole and reads no digit, substring or pattern of any reason, and
// it takes no diagnostic at all.
func TestReadAgentEnd_ElapsedSecondsTrap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		outcome string
		reason  string
	}{
		{name: "a failed end naming its elapsed seconds", outcome: "failed", reason: "opencode: APIError: request timed out after 3600 seconds"},
		{name: "a failed end naming a deadline and a status", outcome: "failed", reason: "opencode: APIError: 504 deadline exceeded after 7199.8s"},
		{name: "a cancelled end naming its elapsed seconds", outcome: "cancelled", reason: "opencode: turn canceled after 3599 seconds"},
		{name: "a cancelled end naming the sandbox's lifetime", outcome: "cancelled", reason: "sandbox lifetime of 7200s reached; turn context canceled before completion"},
		{name: "the legacy text with its seconds appended", outcome: "cancelled", reason: turn.LegacyInterruptionReason + " after 3600s"},
		{name: "the legacy text with its seconds in parentheses", outcome: "cancelled", reason: turn.LegacyInterruptionReason + " (elapsed 3600 seconds)"},
		{name: "the legacy text behind its seconds", outcome: "cancelled", reason: "after 3600s: " + turn.LegacyInterruptionReason},
		{name: "the legacy text with its seconds on a failed end", outcome: "failed", reason: turn.LegacyInterruptionReason + " after 3600s"},
		{name: "only digits", outcome: "cancelled", reason: "3600"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			end := turn.AgentEnd{Outcome: tc.outcome, Reason: strptr(tc.reason)}
			if got := turn.ReadAgentEnd(end); got != (turn.EndReading{}) {
				t.Errorf("ReadAgentEnd(%s, %q) = %+v, want the turn's own end", tc.outcome, tc.reason, got)
			}
		})
	}
}

func derefOr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// TestAttributeInterruption pins the attribution: the persisted deadline
// decides, never text. A stop at or past a known deadline is the
// sandbox's lifetime; any other stop is its provider's.
func TestAttributeInterruption(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		deadlineKnown bool
		runway        time.Duration
		want          turn.InterruptionReason
	}{
		{name: "past the deadline", deadlineKnown: true, runway: -time.Minute, want: turn.InterruptionReasonSandboxLifetime},
		{name: "at the deadline", deadlineKnown: true, runway: 0, want: turn.InterruptionReasonSandboxLifetime},
		{name: "just before the deadline", deadlineKnown: true, runway: time.Nanosecond, want: turn.InterruptionReasonSandboxStopped},
		{name: "well before the deadline", deadlineKnown: true, runway: time.Hour, want: turn.InterruptionReasonSandboxStopped},
		{name: "no deadline recorded", deadlineKnown: false, runway: -time.Hour, want: turn.InterruptionReasonSandboxStopped},
		{name: "no deadline recorded, zero runway", deadlineKnown: false, runway: 0, want: turn.InterruptionReasonSandboxStopped},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := turn.AttributeInterruption(tc.deadlineKnown, tc.runway); got != tc.want {
				t.Errorf("AttributeInterruption(%v, %v) = %q, want %q", tc.deadlineKnown, tc.runway, got, tc.want)
			}
		})
	}
}

// TestDecideSilentInterruption sweeps every combination of the facts: a
// silent loss is recorded only for a turn that ran on the lost gen, no
// stop flagged, not yet interrupted on that gen, whose gen's known
// deadline has passed; the deadline's label is given in every case.
func TestDecideSilentInterruption(t *testing.T) {
	t.Parallel()
	bools := []bool{false, true}
	runways := []time.Duration{-time.Hour, -time.Nanosecond, 0, time.Nanosecond, time.Hour}
	for _, onGen := range bools {
		for _, stop := range bools {
			for _, already := range bools {
				for _, known := range bools {
					for _, runway := range runways {
						f := turn.SilentLossFacts{OnGen: onGen, StopRequested: stop, AlreadyInterruptedOnGen: already, DeadlineKnown: known, Runway: runway}
						wantLabel := turn.SilentLossDeadlineUnknown
						if known {
							wantLabel = turn.SilentLossDeadlineAhead
							if runway <= 0 {
								wantLabel = turn.SilentLossDeadlinePassed
							}
						}
						wantRecord := onGen && !stop && !already && known && runway <= 0
						record, label := turn.DecideSilentInterruption(f)
						if record != wantRecord || label != wantLabel {
							t.Errorf("DecideSilentInterruption(%+v) = (%v, %q), want (%v, %q)", f, record, label, wantRecord, wantLabel)
						}
					}
				}
			}
		}
	}
}

// interruptionReasons are the reasons the texts are written for, and one
// this release does not know.
var interruptionReasons = []turn.InterruptionReason{
	turn.InterruptionReasonSandboxLifetime, turn.InterruptionReasonSandboxStopped, "a_later_reason", "",
}

// TestInterruptionWarning_IsNeutral pins the warning a person reads: the
// turn has not failed, so the text carries no failure word and no
// duration, says the turn is still running, and names the cause the
// persisted deadline gave -- the stopped text for a reason this release
// does not know.
func TestInterruptionWarning_IsNeutral(t *testing.T) {
	t.Parallel()
	for _, r := range interruptionReasons {
		t.Run(string(r), func(t *testing.T) {
			t.Parallel()
			text := turn.InterruptionWarning(r)
			assertNeutral(t, "InterruptionWarning("+string(r)+")", text)
			if !strings.Contains(text, "The turn is still running") {
				t.Errorf("InterruptionWarning(%q) = %q, want it to say the turn is still running", r, text)
			}
			wantCause := "was stopped by its provider"
			if r == turn.InterruptionReasonSandboxLifetime {
				wantCause = "reached the end of its lifetime"
			}
			if !strings.Contains(text, wantCause) {
				t.Errorf("InterruptionWarning(%q) = %q, want it to say the sandbox %s", r, text, wantCause)
			}
		})
	}
	if turn.InterruptionWarning(turn.InterruptionReasonSandboxLifetime) == turn.InterruptionWarning(turn.InterruptionReasonSandboxStopped) {
		t.Error("the two reasons' warnings are the same text")
	}
}

// assertNeutral fails when text holds a failure word or any digit: neither
// the warning nor the context may say the turn failed, or carry a
// duration a classifier could read.
func assertNeutral(t *testing.T, what, text string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, word := range []string{"fail", "error", "cancel", "timed out", "timeout"} {
		if strings.Contains(lower, word) {
			t.Errorf("%s = %q holds %q", what, text, word)
		}
	}
	if strings.IndexFunc(text, unicode.IsDigit) >= 0 {
		t.Errorf("%s = %q holds a digit", what, text)
	}
}

// TestRenderInterruptedTurnContext pins the context a turn sent again
// after an interruption carries: framed by its fixed header and closing
// lines, every line one of the template's, no failure word and no
// duration, the cause the persisted deadline gave, and the prompt after it
// byte for byte.
func TestRenderInterruptedTurnContext(t *testing.T) {
	t.Parallel()
	for _, r := range interruptionReasons {
		t.Run(string(r), func(t *testing.T) {
			t.Parallel()
			context := turn.RenderInterruptedTurnContext(r)
			assertNeutral(t, "RenderInterruptedTurnContext("+string(r)+")", context)
			lines := strings.Split(context, "\n")
			if len(lines) != 5 || lines[0] != turn.InterruptionContextHeader || lines[len(lines)-1] != turn.InterruptionContextClosing {
				t.Fatalf("RenderInterruptedTurnContext(%q) = %q, want five lines between %q and %q", r, context, turn.InterruptionContextHeader, turn.InterruptionContextClosing)
			}
			if lines[2] != turn.InterruptionContextCheck || lines[3] != turn.InterruptionContextOutsideActions {
				t.Errorf("RenderInterruptedTurnContext(%q) = %q, want it to ask for a check of what the earlier attempt left, then of actions outside the sandbox", r, context)
			}
			wantCause := "was stopped by its provider"
			if r == turn.InterruptionReasonSandboxLifetime {
				wantCause = "reached the end of its lifetime"
			}
			if !strings.Contains(lines[1], wantCause) {
				t.Errorf("RenderInterruptedTurnContext(%q) line 2 = %q, want it to say the sandbox %s", r, lines[1], wantCause)
			}

			for _, prompt := range []string{"", "fix the build", "line one\n\nline two\n", turn.InterruptionContextClosing, "  leading and trailing  ", "[Narvi: report on this session's earlier work]\n...\n\nreview it"} {
				got := turn.PromptWithInterruptionContext(prompt, r)
				want := context + turn.InterruptionContextSeparator + prompt
				if got != want {
					t.Errorf("PromptWithInterruptionContext(%q, %q) = %q, want the context, the separator, then the prompt byte for byte", prompt, r, got)
				}
				if !strings.HasSuffix(got, turn.InterruptionContextClosing+turn.InterruptionContextSeparator+prompt) {
					t.Errorf("PromptWithInterruptionContext(%q, %q) = %q, want the prompt right after the closing line", prompt, r, got)
				}
			}
		})
	}
}
