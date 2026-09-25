// Package mcpscope is the MCP surface's scope vocabulary (technical plan
// §43.17): the OAuth scope strings a user consents to when an MCP client
// asks for access, which tools each scope unlocks, and the hierarchy
// between them. A pure domain rule with no I/O, like authz's matrix -- and
// the one domain package internal/adapters/inbound/mcp may import
// (tools/lint/narvichecks/mcpimportban), because the adapter needs the
// rule to decide which tools a request may even see, and the rule can
// reach nothing else.
//
// Scopes only ever SUBTRACT: a granted scope never lets a call do more
// than its user's own role already allows (every tool still runs its REST
// twin's own authz check); it only decides whether a tool is visible and
// callable at all for this one authorization.
package mcpscope

import (
	"errors"
	"fmt"
	"strings"
)

// Scope is one OAuth scope string this deployment understands.
type Scope string

const (
	// Read covers every read-only tool.
	Read Scope = "mcp:read"
	// Write covers every state-changing tool, and implies Read. Declared
	// so the hierarchy is settled before any tool needs it; not
	// advertised, and refused at authorization, until a tool requires it
	// (Advertised).
	Write Scope = "mcp:write"
)

// Vocabulary is every scope this package declares, in canonical order.
var Vocabulary = []Scope{Read, Write}

// implied is the hierarchy as data: each scope maps to every scope it
// grants, itself included. A scope absent from this table implies
// nothing.
var implied = map[Scope][]Scope{
	Read:  {Read},
	Write: {Write, Read},
}

// Known reports whether s is in Vocabulary.
func Known(s Scope) bool {
	_, ok := implied[s]
	return ok
}

// Implies returns every scope s grants, itself included, in canonical
// order -- nil for a scope outside Vocabulary (including the empty
// string).
func Implies(s Scope) []Scope {
	got := implied[s]
	if got == nil {
		return nil
	}
	out := make([]Scope, 0, len(got))
	for _, v := range Vocabulary {
		for _, g := range got {
			if g == v {
				out = append(out, v)
			}
		}
	}
	return out
}

// Satisfies reports whether a grant holding granted may use something
// that requires required. It fails closed on every degenerate input: an
// empty or unknown required scope is satisfied by nothing (a tool whose
// scope was never set is hidden from everyone, not shown to everyone),
// and an unknown granted scope grants nothing.
func Satisfies(granted []Scope, required Scope) bool {
	if !Known(required) {
		return false
	}
	for _, g := range granted {
		for _, s := range implied[g] {
			if s == required {
				return true
			}
		}
	}
	return false
}

// Covers reports whether a credential holding held may be narrowed to
// requested: every requested scope is one held satisfies (Satisfies, so
// the hierarchy counts -- a Write holder may narrow to Read). This is the
// refresh grant's rule (technical plan §43.16): a refreshed token may
// only ever hold what the token it replaces held, or less. An empty
// requested set is covered by anything -- narrowing to nothing never
// widens -- and an unknown requested scope by nothing.
func Covers(held, requested []Scope) bool {
	for _, r := range requested {
		if !Satisfies(held, r) {
			return false
		}
	}
	return true
}

// Advertised returns the scopes a client may request from this build:
// exactly the known scopes at least one entry of required names, in
// canonical order, without duplicates. A scope nothing requires is never
// advertised, so no contract ever promises a scope nothing consumes.
func Advertised(required []Scope) []Scope {
	seen := make(map[Scope]bool, len(required))
	for _, r := range required {
		if Known(r) {
			seen[r] = true
		}
	}
	out := make([]Scope, 0, len(seen))
	for _, v := range Vocabulary {
		if seen[v] {
			out = append(out, v)
		}
	}
	return out
}

// ErrUnknownScope is wrapped by ParseRequested for a scope value outside
// the offered set.
var ErrUnknownScope = errors.New("mcpscope: scope not offered")

// ParseRequested parses an OAuth "scope" parameter (RFC 6749 section 3.3:
// space-delimited, case-sensitive) against offered, returning the
// requested scopes in canonical order without duplicates. An empty or
// all-whitespace value is a legitimate request for no scopes at all and
// returns an empty, non-nil slice. Any value not in offered -- unknown to
// this package, or known but not advertised by this build -- fails the
// whole parse with ErrUnknownScope.
func ParseRequested(raw string, offered []Scope) ([]Scope, error) {
	allowed := make(map[Scope]bool, len(offered))
	for _, o := range offered {
		allowed[o] = true
	}
	seen := map[Scope]bool{}
	for _, field := range strings.Fields(raw) {
		s := Scope(field)
		if !Known(s) || !allowed[s] {
			return nil, fmt.Errorf("%w: %q", ErrUnknownScope, field)
		}
		seen[s] = true
	}
	out := make([]Scope, 0, len(seen))
	for _, v := range Vocabulary {
		if seen[v] {
			out = append(out, v)
		}
	}
	return out, nil
}

// Strings converts scopes to their wire strings, preserving order; a nil
// input yields an empty, non-nil slice.
func Strings(scopes []Scope) []string {
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = string(s)
	}
	return out
}

// FromStrings converts stored scope strings back to Scopes, preserving
// order and keeping unknown values as-is (Satisfies ignores them).
func FromStrings(raw []string) []Scope {
	out := make([]Scope, len(raw))
	for i, s := range raw {
		out[i] = Scope(s)
	}
	return out
}

// Join renders scopes as an OAuth "scope" value (space-delimited).
func Join(scopes []Scope) string {
	return strings.Join(Strings(scopes), " ")
}
