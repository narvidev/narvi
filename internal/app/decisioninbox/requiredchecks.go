package decisioninbox

import (
	"errors"

	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/autoapproval"
)

// This file is where a base branch's required checks (§21.2, "CI green
// means the required checks, not the checks that reported") become the
// fact the eligibility engine reads: autoapproval.EligibilityInput.
// RequiredChecks. Both of this package's eligibility call sites -- the
// read model (computeRealEligibility, aggregate.go) and the merge path
// (revalidateCore, revalidate.go, which the auto-merge worker shares) --
// read the requirements as the bot and build the fact here, from the same
// ports.OpenPR the CI read came from, so the inbox, a person's Merge click
// and the worker decide on the same requirements through the same engine.
//
// They differ only in WHEN they read, for a reason each states at its call
// site: the merge path reads before its probe, so a required check that is
// still running or failed is named in the refusal it returns; the read
// model shows no reason, and reads only once its probe has passed, like
// its other live reads.

// errNoBotCredential is the failed read of a deployment with GitHub
// outbound off (Deps.GitHubOutbound nil): there is no credential to read
// the requirements with.
var errNoBotCredential = errors.New("no bot credential to read the base branch's required checks with: GitHub outbound is off")

// requiredChecksSpec is the read of pr's base branch's requirements as the
// bot, or errNoBotCredential.
func requiredChecksSpec(deps Deps, pr ports.OpenPR) (ports.ListRequiredChecksSpec, error) {
	if deps.GitHubOutbound == nil {
		return ports.ListRequiredChecksSpec{}, errNoBotCredential
	}
	return ports.ListRequiredChecksSpec{Owner: pr.Owner, Repo: pr.Repo, Branch: pr.BaseRef, Token: deps.GitHubOutbound.BotToken()}, nil
}

// requiredChecksFact is the fact for one read of pr's base requirements:
// evaluated against the checks pr's own CI read listed at its head when
// the read succeeded, and the zero value -- "could not be read", which the
// engine refuses on -- when it failed. A failed read is never turned into
// "requires nothing".
func requiredChecksFact(required []ports.RequiredCheck, readErr error, pr ports.OpenPR) autoapproval.RequiredChecks {
	if readErr != nil {
		return autoapproval.RequiredChecks{}
	}
	return autoapproval.ReadRequiredChecks(toDomainRequiredChecks(required), toDomainHeadChecks(pr.HeadChecks), !pr.HeadChecksListDegraded)
}

// probeRequiredChecks is the stand-in a probe uses for fact: fact itself
// when the requirements were read, and "read, requiring nothing" when they
// were not -- the most lenient value, like the probes' assumed base SHA.
// An unread requirement can only refuse, never approve, so a probe that
// refuses with this stand-in refuses with the real fact too, on its own
// reason; and the final call, which gets the real fact, is the one that
// refuses on the failed read -- only once every other criterion has
// passed, so a transient failure never stands in for a pull request's
// real, lasting reason.
func probeRequiredChecks(fact autoapproval.RequiredChecks) autoapproval.RequiredChecks {
	if fact.Known {
		return fact
	}
	return autoapproval.ReadRequiredChecks(nil, nil, true)
}

func toDomainRequiredChecks(in []ports.RequiredCheck) []autoapproval.RequiredCheck {
	out := make([]autoapproval.RequiredCheck, 0, len(in))
	for _, c := range in {
		out = append(out, autoapproval.RequiredCheck{Name: c.Name, AppID: c.AppID})
	}
	return out
}

func toDomainHeadChecks(in []ports.HeadCheck) []autoapproval.HeadCheck {
	out := make([]autoapproval.HeadCheck, 0, len(in))
	for _, c := range in {
		out = append(out, autoapproval.HeadCheck{
			Name:   c.Name,
			Source: toDomainCheckSource(c.Source),
			AppID:  c.AppID,
			State:  toDomainCheckState(c.State),
		})
	}
	return out
}

// toDomainCheckSource maps a port source to the domain's. An unknown
// source maps to a commit status, which never satisfies a check tied to
// an App -- the conservative reading.
func toDomainCheckSource(s ports.HeadCheckSource) autoapproval.CheckSource {
	if s == ports.HeadCheckSourceCheckRun {
		return autoapproval.CheckSourceCheckRun
	}
	return autoapproval.CheckSourceStatus
}

// toDomainCheckState maps a port state to the domain's. An unknown state
// maps to failed, which never satisfies a requirement.
func toDomainCheckState(s ports.HeadCheckState) autoapproval.CheckState {
	switch s {
	case ports.HeadCheckStatePassed:
		return autoapproval.CheckStatePassed
	case ports.HeadCheckStatePending:
		return autoapproval.CheckStatePending
	default:
		return autoapproval.CheckStateFailed
	}
}
