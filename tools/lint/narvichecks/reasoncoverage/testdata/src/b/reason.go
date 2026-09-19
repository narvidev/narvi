// Package imagedecision (testdata, package "b") is the compliant
// counterpart to package "a": every declared Reason constant is either
// listed in All() or named in the analyzer's own allowedMissing
// (ReasonNone, mirroring the real package's own deliberate exception) --
// the analyzer must stay silent here.
package imagedecision

type Reason string

const (
	ReasonSelected Reason = "selected"
	ReasonNoRepos  Reason = "no_repos"
	ReasonNone     Reason = "none"
)

func All() []Reason {
	return []Reason{
		ReasonSelected,
		ReasonNoRepos,
	}
}

func decide(cold bool) Reason {
	if cold {
		return ReasonNoRepos
	}
	return ReasonSelected
}
