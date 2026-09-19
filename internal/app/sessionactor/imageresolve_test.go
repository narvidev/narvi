package sessionactor

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/narvidev/narvi/internal/domain/imagedecision"
)

// newCapturingLogger returns a *slog.Logger writing newline-delimited JSON
// into the returned *bytes.Buffer -- a unit-test-local equivalent of
// planrecord_integration_test.go's own captureDefaultLoggerJSON, but
// scoped to a single *slog.Logger value rather than slog.Default(), since
// validatedPersistReason (below) takes its logger as a plain parameter
// and needs no global state.
func newCapturingLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

// lastLogEntry decodes buf's own last non-empty JSON log line -- fails the
// test if there is none.
func lastLogEntry(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if len(bytes.TrimSpace(lines[i])) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(lines[i], &entry); err != nil {
			t.Fatalf("unmarshal log line %q: %v", lines[i], err)
		}
		return entry
	}
	t.Fatalf("no log line found; full log output:\n%s", buf.String())
	return nil
}

// TestValidatedPersistReason_ValidReasonPassesThroughSilently proves the
// overwhelmingly common case first: every real, in-vocabulary Reason
// decideImage/repoAccessAllowedForSpawn can actually produce passes
// through unchanged, with NOTHING logged -- this guard must never add
// noise to the ordinary path.
func TestValidatedPersistReason_ValidReasonPassesThroughSilently(t *testing.T) {
	t.Parallel()
	for _, reason := range imagedecision.All() {
		reason := reason
		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()
			logger, buf := newCapturingLogger()
			got := validatedPersistReason(logger, reason)
			if got != reason {
				t.Errorf("validatedPersistReason(%q) = %q, want unchanged", reason, got)
			}
			if buf.Len() != 0 {
				t.Errorf("validatedPersistReason(%q) logged something for a valid reason, want silence: %s", reason, buf.String())
			}
		})
	}
}

// TestValidatedPersistReason_InvalidReason_FallsBackAndLogsError is A1's
// own reproduction, at unit-test speed: a Reason value that is NOT one of
// imagedecision.All()'s own members (a future constant declared but never
// added to All(), or a manually-constructed value) must not reach the
// caller unchanged -- it must be substituted with
// imagedecision.ReasonUnrecognized (itself always a member of All(), so
// the caller's own subsequent ENUM-typed write can never fail on this
// value), and the substitution must be logged at ERROR, not Warn: A1's
// own finding is explicit that a programmer error silently dropping
// provenance is not a warning.
func TestValidatedPersistReason_InvalidReason_FallsBackAndLogsError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		reason imagedecision.Reason
	}{
		{name: "typo value, never declared anywhere", reason: imagedecision.Reason("not_a_real_reason")},
		{name: "zero value (empty string) -- never special-cased as valid", reason: imagedecision.Reason("")},
		{name: "ReasonNone -- a real constant, but deliberately excluded from All()", reason: imagedecision.ReasonNone},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger, buf := newCapturingLogger()
			got := validatedPersistReason(logger, tt.reason)
			if got != imagedecision.ReasonUnrecognized {
				t.Fatalf("validatedPersistReason(%q) = %q, want %q", tt.reason, got, imagedecision.ReasonUnrecognized)
			}
			entry := lastLogEntry(t, buf)
			if entry["level"] != "ERROR" {
				t.Errorf("validatedPersistReason(%q) logged at level %v, want ERROR", tt.reason, entry["level"])
			}
			if entry["reason"] != string(tt.reason) {
				t.Errorf("validatedPersistReason(%q) logged reason=%v, want the original invalid value so an operator can see what it actually was", tt.reason, entry["reason"])
			}
		})
	}
}

// TestValidatedPersistReason_FallbackIsAlwaysInVocabulary is the guard's
// own self-check: whatever it substitutes must itself always be a member
// of All(), or the exact failure this guard exists to prevent (an
// out-of-vocabulary value reaching the ENUM-typed column) simply moves one
// level down instead of being closed.
func TestValidatedPersistReason_FallbackIsAlwaysInVocabulary(t *testing.T) {
	t.Parallel()
	logger, _ := newCapturingLogger()
	got := validatedPersistReason(logger, imagedecision.Reason("bogus"))
	for _, r := range imagedecision.All() {
		if r == got {
			return
		}
	}
	t.Fatalf("validatedPersistReason's own fallback %q is not itself a member of imagedecision.All()", got)
}

// TestParticipantVisibleReason_CoarsensExactlyTheDisclosingReasons is A2's
// own audit fix, unit-tested directly: the three reasons that name
// admin-only session-creator account state (disabled, viewer role, no
// usable token -- all admin-only everywhere else in this product, per
// Settings -> Members) must be coarsened to ReasonRepoAccessDenied before
// they ever reach the "image_decision" event this codebase serves to
// every logged-in reader unconditionally. Every OTHER reason -- swept
// here over the COMPLETE vocabulary, not a hand-picked sample -- must
// pass through unchanged; this table asserts both directions so a future
// reason silently added to (or removed from) the coarsened set is caught
// here rather than discovered as a disclosure regression later.
func TestParticipantVisibleReason_CoarsensExactlyTheDisclosingReasons(t *testing.T) {
	t.Parallel()

	coarsened := map[imagedecision.Reason]bool{
		imagedecision.ReasonRepoAccessCreatorDisabled: true,
		imagedecision.ReasonRepoAccessCreatorViewer:   true,
		imagedecision.ReasonRepoAccessNoToken:         true,
	}

	all := imagedecision.All()
	gotCoarsened := 0
	for _, reason := range all {
		reason := reason
		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()
			got := participantVisibleReason(reason)
			if coarsened[reason] {
				if got != imagedecision.ReasonRepoAccessDenied {
					t.Errorf("participantVisibleReason(%q) = %q, want coarsened to %q (discloses admin-only creator account state otherwise)",
						reason, got, imagedecision.ReasonRepoAccessDenied)
				}
				return
			}
			if got != reason {
				t.Errorf("participantVisibleReason(%q) = %q, want unchanged (not one of the three disclosing reasons)", reason, got)
			}
		})
	}
	for _, reason := range all {
		if coarsened[reason] {
			gotCoarsened++
		}
	}
	if gotCoarsened != len(coarsened) {
		t.Fatalf("%d of the coarsened set's own %d reasons are actually in imagedecision.All() (a typo in this test's own map would hide silently otherwise)", gotCoarsened, len(coarsened))
	}

	// Zero value: the empty Reason is not in All() at all (decideImage
	// never produces it), but participantVisibleReason must still behave
	// like an ordinary "not one of the three" value -- pass through
	// unchanged, never panic, never accidentally match a coarsened case.
	if got := participantVisibleReason(imagedecision.Reason("")); got != imagedecision.Reason("") {
		t.Errorf(`participantVisibleReason("") = %q, want "" unchanged`, got)
	}
}
