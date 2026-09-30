package autoapproval

import (
	"fmt"
	"sort"
	"strings"

	"github.com/narvidev/narvi/internal/domain/reviewcheck"
)

// This file is §21.2's "CI green means the required checks, not the checks
// that reported" amendment, as a pure rule. The CI read at the head
// (EligibilityInput.CIGreen) only sees the checks that have reported: a check
// the base branch requires but that has not started yet is simply absent, so
// that read can be green before the requirement is met. The required set is
// read from the base branch (its protection and its rulesets) by the caller;
// EvaluateRequiredChecks below says, for each required check, whether the
// head satisfies it.
//
// The required set is ADDED to the CI read, never substituted for it:
// ComputeEligible still refuses a head whose CI read is not green, so a
// failing check the base does not require keeps blocking. A base that
// requires nothing produces no shortfall, and eligibility is then exactly
// the CI read's answer.

// RequiredCheck is one check a base branch requires before a pull request
// into it may merge.
type RequiredCheck struct {
	// Name is the check run's name or the commit status's context.
	Name string
	// AppID names the App that must report the check. Zero names none: a
	// report from any source counts.
	AppID int64
}

// CheckSource is which of the code host's two check surfaces reported a
// HeadCheck.
type CheckSource string

const (
	// CheckSourceCheckRun is a check run, which carries the id of the App
	// that reported it.
	CheckSourceCheckRun CheckSource = "check_run"
	// CheckSourceStatus is a commit status, which carries no App id.
	CheckSourceStatus CheckSource = "status"
)

// CheckState is where a HeadCheck stands, as far as a requirement is
// concerned.
type CheckState string

const (
	// CheckStatePassed is a check that satisfies a requirement: a check run
	// concluded success, neutral or skipped, or a status in state success.
	CheckStatePassed CheckState = "passed"
	// CheckStatePending is a check that has reported but not concluded.
	CheckStatePending CheckState = "pending"
	// CheckStateFailed is every other outcome, an unrecognised one
	// included.
	CheckStateFailed CheckState = "failed"
)

// HeadCheck is one check reported at a pull request's current head.
type HeadCheck struct {
	Name   string
	Source CheckSource
	// AppID is the id of the App that reported a check run; zero for a
	// commit status, which reports none.
	AppID int64
	State CheckState
}

// ShortfallKind is why a required check is not satisfied at the head.
type ShortfallKind string

const (
	// ShortfallMissing is a required check with no report at the head from
	// a source that counts for it -- none at all, or only reports from an
	// App other than the one the base names.
	ShortfallMissing ShortfallKind = "missing"
	// ShortfallUnconfirmed is a required check not seen at the head while
	// the head's list of checks was not read in full: it may have reported
	// in the part that was not read.
	ShortfallUnconfirmed ShortfallKind = "unconfirmed"
	// ShortfallPending is a required check that has reported and not yet
	// concluded.
	ShortfallPending ShortfallKind = "pending"
	// ShortfallFailed is a required check with at least one report that
	// did not pass.
	ShortfallFailed ShortfallKind = "failed"
)

// RequiredCheckShortfall is one required check the head does not satisfy.
type RequiredCheckShortfall struct {
	Check RequiredCheck
	Kind  ShortfallKind
}

// RequiredChecks is the required-check fact eligibility reads, alongside
// the CI read. Build it with ReadRequiredChecks once the requirements were
// read; leave it at its zero value when they could not be.
type RequiredChecks struct {
	// Known is true once the base branch's requirements were read -- from
	// every source, a source the repository's plan does not offer counting
	// as read and declaring nothing. The zero value, false, is "could not
	// be read", and ComputeEligible refuses on it: a failed read never
	// falls back to the CI read alone, which would be the very gap this
	// fact exists to close.
	Known bool
	// Shortfalls is EvaluateRequiredChecks' answer, in its order. Empty
	// when the base requires nothing, or when the head satisfies all of
	// it.
	Shortfalls []RequiredCheckShortfall
}

// ReadRequiredChecks is the fact for requirements that were read: required
// evaluated against the checks reported at the head (EvaluateRequiredChecks).
func ReadRequiredChecks(required []RequiredCheck, head []HeadCheck, headComplete bool) RequiredChecks {
	return RequiredChecks{Known: true, Shortfalls: EvaluateRequiredChecks(required, head, headComplete)}
}

// EvaluateRequiredChecks reports every required check the head does not
// satisfy, sorted by name and then App id, and nil when it satisfies them
// all.
//
// For each required check, the reports that count are those at the head
// carrying its name and, when it names an App, reported by that App. A
// commit status carries no App id, so it never counts for a check that
// names one, and neither does a check run from another App: GitHub counts
// only the named App's report. The check is satisfied only when at least
// one report counts and EVERY report that counts passed -- a check run and
// a commit status carrying the same required name must both pass, as
// GitHub requires; taking either one as enough would approve a failing
// check beside a passing status. A failed report outranks a pending one.
//
// headComplete is false when the head's list of checks was not read in
// full. A required check naming no App that is then not seen is reported
// unconfirmed, never missing, since it may sit in the part that was not
// read. A check naming an App is still missing: what can go unread without
// the CI read itself reading degraded is the commit statuses (a truncated
// check-run list, or a failed GET, degrades the CI read, which refuses
// first), and a status never counts for a check that names an App.
//
// reviewcheck.CheckName (narvi/review) is taken out of the required set
// before anything else: it is the review this eligibility already reads
// (the verdict and its freshness), and the CI read already leaves it out by
// name. A base that requires it is not made ineligible by it here -- the
// verdict criteria decide what it stands for.
func EvaluateRequiredChecks(required []RequiredCheck, head []HeadCheck, headComplete bool) []RequiredCheckShortfall {
	var shortfalls []RequiredCheckShortfall
	for _, check := range normalizeRequired(required) {
		counted, failed, pending := 0, false, false
		for _, report := range head {
			if !countsFor(check, report) {
				continue
			}
			counted++
			switch report.State {
			case CheckStatePassed:
			case CheckStatePending:
				pending = true
			default:
				failed = true
			}
		}
		switch {
		case counted == 0 && !headComplete && check.AppID == 0:
			shortfalls = append(shortfalls, RequiredCheckShortfall{Check: check, Kind: ShortfallUnconfirmed})
		case counted == 0:
			shortfalls = append(shortfalls, RequiredCheckShortfall{Check: check, Kind: ShortfallMissing})
		case failed:
			shortfalls = append(shortfalls, RequiredCheckShortfall{Check: check, Kind: ShortfallFailed})
		case pending:
			shortfalls = append(shortfalls, RequiredCheckShortfall{Check: check, Kind: ShortfallPending})
		}
	}
	return shortfalls
}

// countsFor reports whether report is one of the reports that decide
// check: the same name, and, when check names an App, a check run from that
// App.
func countsFor(check RequiredCheck, report HeadCheck) bool {
	if report.Name != check.Name {
		return false
	}
	if check.AppID == 0 {
		return true
	}
	return report.Source == CheckSourceCheckRun && report.AppID == check.AppID
}

// normalizeRequired drops reviewcheck.CheckName and empty names, removes
// exact duplicates -- the two sources often declare the same check -- and
// sorts what remains by name and then App id, so a reason names checks in a
// stable order. A name required once with an App and once without keeps
// both: each is a requirement, and both must hold.
func normalizeRequired(required []RequiredCheck) []RequiredCheck {
	seen := make(map[RequiredCheck]bool, len(required))
	out := make([]RequiredCheck, 0, len(required))
	for _, check := range required {
		if check.Name == "" || check.Name == reviewcheck.CheckName || seen[check] {
			continue
		}
		seen[check] = true
		out = append(out, check)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].AppID < out[j].AppID
	})
	return out
}

// requiredChecksReason is the Reason ComputeEligible gives for shortfalls:
// every unmet required check, named, in EvaluateRequiredChecks' order.
// Unlike every other Reason, it is built rather than fixed, because the
// check is the useful part: "a required check has not reported" sends a
// person looking, "required check \"build\" has not reported" does not.
// Every such reason starts with RequiredCheckReasonPrefix.
func requiredChecksReason(shortfalls []RequiredCheckShortfall) Reason {
	parts := make([]string, 0, len(shortfalls))
	for _, s := range shortfalls {
		parts = append(parts, describeShortfall(s))
	}
	return Reason(strings.Join(parts, "; "))
}

// RequiredCheckReasonPrefix starts every Reason ComputeEligible gives for a
// required check the head does not satisfy, so a reader can tell that
// criterion apart without parsing the rest.
const RequiredCheckReasonPrefix = "required check "

func describeShortfall(s RequiredCheckShortfall) string {
	name := fmt.Sprintf("%s%q", RequiredCheckReasonPrefix, s.Check.Name)
	switch s.Kind {
	case ShortfallMissing:
		if s.Check.AppID != 0 {
			return fmt.Sprintf("%s has not reported at the current head from the App the base branch names (App id %d)", name, s.Check.AppID)
		}
		return name + " has not reported at the current head"
	case ShortfallUnconfirmed:
		return name + " could not be confirmed at the current head (its commit statuses were not all read)"
	case ShortfallPending:
		return name + " is still running at the current head"
	default:
		return name + " did not pass at the current head"
	}
}
