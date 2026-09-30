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
	// CheckSourceStatus is a commit status, which carries the account that
	// posted it rather than an App id.
	CheckSourceStatus CheckSource = "status"
)

// Poster is the kind of account that posted a commit status.
type Poster string

const (
	// PosterUnknown is a status whose poster could not be read. The zero
	// value, so a caller that forgets to say never makes a status count
	// for a check that names an App.
	PosterUnknown Poster = ""
	// PosterApp is a status an App posted, through its bot account. A check
	// run is always posted by an App.
	PosterApp Poster = "app"
	// PosterPerson is a status a person's own account posted.
	PosterPerson Poster = "person"
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
	// AppID is the App a report is attributed to: for a check run, the App
	// that reported it; for a commit status an App posted, that App when it
	// could be identified, and zero when it could not. Zero for every other
	// status.
	AppID int64
	// Poster is who posted a commit status: an App, a person, or unknown.
	// A check run is always PosterApp.
	Poster Poster
	// EarlierFromOthers is set on a commit status when an earlier status of
	// its name at the head came from another account, or from one that
	// could not be read, or when the earlier ones were not all read. The
	// code host rolls a name up to its latest status only, so an earlier
	// status hidden under this one may be the App's.
	EarlierFromOthers bool
	State             CheckState
}

// ShortfallKind is why a required check is not satisfied at the head.
type ShortfallKind string

const (
	// ShortfallMissing is a required check with no report at the head from
	// a source that counts for it -- none at all, or only reports from
	// another source (another App, or a person's commit status, for a
	// check that names an App).
	ShortfallMissing ShortfallKind = "missing"
	// ShortfallUnconfirmed is a required check not seen at the head while
	// the head's list of checks was not read in full: it may have reported
	// in the part that was not read.
	ShortfallUnconfirmed ShortfallKind = "unconfirmed"
	// ShortfallPosterUnknown is a required check naming an App, not
	// satisfied by any report that counts, beside a commit status of its
	// name whose poster could not be read: that status may be the App's.
	ShortfallPosterUnknown ShortfallKind = "poster_unknown"
	// ShortfallAppUnknown is a required check naming an App, not satisfied
	// by any report that counts, beside a commit status of its name that
	// an App posted but whose App could not be identified: it may be the
	// App named.
	ShortfallAppUnknown ShortfallKind = "app_unknown"
	// ShortfallReplaced is a required check naming an App, not satisfied by
	// any report that counts, whose name's latest commit status came from
	// another source over an earlier status that may be the App's
	// (HeadCheck.EarlierFromOthers): what the App reported cannot be read.
	ShortfallReplaced ShortfallKind = "replaced"
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
	// OtherSource is set on a missing check when a report of its name did
	// come in, from a source that does not count for it.
	OtherSource bool
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
// carrying its name and, when it names an App, attributed to that App (see
// countsFor): that App's check run, or a commit status that App posted.
// The check is satisfied only when at least one report counts and EVERY
// report that counts passed -- a check run and a commit status carrying the
// same required name must both pass, as GitHub requires; taking either one
// as enough would approve a failing check beside a passing status. A
// failed report outranks a pending one.
//
// A check run from another App never counts, nor does a commit status a
// person posted, nor one another App posted: a commit status counts for a
// check naming an App only when that App, identified by id, posted it.
// A status whose App could not be identified never counts either -- it may
// be any App -- and neither does one whose poster could not be read.
//
// headComplete is false when the head's list of checks was not read in
// full (its commit statuses beyond one page). A required check that is
// then not seen is reported unconfirmed, never missing, since it may sit
// in the part that was not read. A check naming an App that no report
// counts for is reported as could-not-be-confirmed, never missing, beside
// a status of its name whose poster could not be read (poster unknown),
// whose App could not be identified (App unknown), or that another source
// posted over an earlier status that may be the App's (replaced).
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
		otherSource, posterUnknown, appUnknown, replaced := false, false, false, false
		for _, report := range head {
			if report.Name != check.Name {
				continue
			}
			if !countsFor(check, report) {
				switch {
				case report.Source == CheckSourceStatus && report.Poster == PosterUnknown:
					posterUnknown = true
				case report.Source == CheckSourceStatus && report.Poster == PosterApp && report.AppID == 0:
					appUnknown = true
				default:
					otherSource = true
					if report.Source == CheckSourceStatus && report.EarlierFromOthers {
						replaced = true
					}
				}
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
		case counted == 0 && posterUnknown:
			shortfalls = append(shortfalls, RequiredCheckShortfall{Check: check, Kind: ShortfallPosterUnknown})
		case counted == 0 && appUnknown:
			shortfalls = append(shortfalls, RequiredCheckShortfall{Check: check, Kind: ShortfallAppUnknown})
		case counted == 0 && replaced:
			shortfalls = append(shortfalls, RequiredCheckShortfall{Check: check, Kind: ShortfallReplaced})
		case counted == 0 && !headComplete:
			shortfalls = append(shortfalls, RequiredCheckShortfall{Check: check, Kind: ShortfallUnconfirmed})
		case counted == 0:
			shortfalls = append(shortfalls, RequiredCheckShortfall{Check: check, Kind: ShortfallMissing, OtherSource: otherSource})
		case failed:
			shortfalls = append(shortfalls, RequiredCheckShortfall{Check: check, Kind: ShortfallFailed})
		case pending:
			shortfalls = append(shortfalls, RequiredCheckShortfall{Check: check, Kind: ShortfallPending})
		}
	}
	return shortfalls
}

// countsFor reports whether report, which carries check's name, is one of
// the reports that decide check. A check naming no App counts every report
// of its name. A check naming an App counts that App's check runs and the
// commit statuses that App posted, the App identified by id. A check run
// from another App, a status another App posted, a status whose App could
// not be identified, a status a person posted and a status whose poster is
// unknown never count.
func countsFor(check RequiredCheck, report HeadCheck) bool {
	if check.AppID == 0 {
		return true
	}
	if report.Source == CheckSourceCheckRun {
		return report.AppID == check.AppID
	}
	return report.Poster == PosterApp && report.AppID == check.AppID
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

// describeShortfall names the check -- with the App the base names, for
// every kind of shortfall, when one is named -- and what is wrong with it.
func describeShortfall(s RequiredCheckShortfall) string {
	name := fmt.Sprintf("%s%q", RequiredCheckReasonPrefix, s.Check.Name)
	if s.Check.AppID != 0 {
		name += fmt.Sprintf(" from the App the base branch names (App id %d)", s.Check.AppID)
	}
	switch s.Kind {
	case ShortfallMissing:
		if s.OtherSource {
			return name + " has not reported at the current head; a report of that name came from another source, which does not count"
		}
		return name + " has not reported at the current head"
	case ShortfallUnconfirmed:
		return name + " could not be confirmed at the current head (its commit statuses were not all read)"
	case ShortfallPosterUnknown:
		return name + " could not be confirmed at the current head (who posted its commit status could not be read)"
	case ShortfallAppUnknown:
		return name + " could not be confirmed at the current head (the App that posted its commit status could not be identified)"
	case ShortfallReplaced:
		return name + " could not be confirmed at the current head (its latest commit status came from another source, which does not count, over an earlier one that may be the App's)"
	case ShortfallPending:
		return name + " is still running at the current head"
	default:
		return name + " did not pass at the current head"
	}
}
