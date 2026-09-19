// Package imagedecision (testdata, package "a") reproduces A1's own audit
// finding: a Reason constant declared and returned from a real call site,
// but never added to All(). want-checked below.
package imagedecision

type Reason string

const (
	ReasonSelected Reason = "selected"
	ReasonMissing  Reason = "missing" // want "ReasonMissing is declared as a Reason constant but is not listed in All"
	ReasonNone     Reason = "none"
)

func All() []Reason {
	return []Reason{
		ReasonSelected,
	}
}

// decide is a stand-in for imageresolve.go's own decideImage: a real call
// site returning ReasonMissing, exactly like the audit's own reproduction
// (a new Reason, returned from a real branch, never added to All()).
func decide(cold bool) Reason {
	if cold {
		return ReasonMissing
	}
	return ReasonSelected
}
