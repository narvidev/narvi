package reviewtriage

// ReviewDepth is the light/deep routing decision §26.3 defines -- a
// closed, two-value string type (mirroring internal/domain/review's own
// closed-enum-as-string-type convention, e.g. review.Shippable). The Go
// zero value ("") is deliberately not one of the two legal members,
// matching review/doc.go's own "fail-conservative policy for every
// closed enum" -- see Floor below for how an unrecognized/zero value is
// handled.
type ReviewDepth string

const (
	// DepthLight is the default, lower-scrutiny path: balanced-tier
	// model, no forced effort override -- exactly today's review
	// behavior (§26.9: "the light path's behavior remains exactly
	// today's review").
	DepthLight ReviewDepth = "light"
	// DepthDeep is the higher-scrutiny path: frontier-tier model, forced
	// high effort, and (§26.4, not built here) adversarial
	// counter-review + architecture recap.
	DepthDeep ReviewDepth = "deep"
)

// depthRank orders ReviewDepth for Floor's own max-of-two composition,
// mirroring internal/domain/review's own explicit-table-not-iota-order
// discipline (review/shippable.go's rank function, review/doc.go's own
// "Ranking is an explicit table, never iota order" section) -- an
// accidental reorder of the two consts above must never silently change
// this package's own policy.
var depthRank = map[ReviewDepth]int{
	DepthLight: 0,
	DepthDeep:  1,
}

// rank returns d's own position in depthRank -- an unrecognized/zero
// ReviewDepth (never legitimately produced by Decide, but a defensive
// case for a caller-supplied prior value read back from storage, e.g. a
// row written before this column existed) ranks with DepthLight, the
// LEAST conservative reading. This is deliberately the one place in this
// package that does NOT follow internal/domain/review's own "unrecognized
// ranks with the worst-known value" convention: an absent/garbled PRIOR
// depth means "we have no record this PR was ever routed deep", which is
// honestly indistinguishable from "it never was" -- there is no
// principled way to treat "unknown" as "deep" without inventing evidence
// that does not exist. Floor's own caller-facing contract (below) never
// depends on this defaulting toward deep; Decide's OWN fresh signals are
// what actually determine depth for a PR with no prior record at all.
func rank(d ReviewDepth) int {
	if r, ok := depthRank[d]; ok {
		return r
	}
	return 0
}

// Floor composes a freshly-computed depth with a PR's own previously
// recorded depth (§24's own re-review floor: "once deep, a PR stays
// deep, even if the delta itself would independently route light") --
// the more conservative (higher-ranked) of the two always wins, mirroring
// internal/domain/review's own maxShippable/max(rank) composition
// pattern (shippable.go) applied to this package's own two-value
// ReviewDepth instead. prior == "" (no prior verdict/depth on record --
// e.g. this PR's first-ever review) is the common, legitimate case: fresh
// alone decides, unaffected by an absent floor.
//
// # Precedence: an explicit always_light admin override outranks this floor (D9, adversarial-review fix)
//
// Floor itself has no awareness of WHY fresh is what it is -- it composes
// two ReviewDepth values, nothing more. But every caller applying it MUST
// skip this call entirely (using the fresh, unfloored Decision.Depth
// as-is) whenever that Decision's own Reason is ReasonAlwaysLightConfig
// (decide.go): a repo admin setting reviewDepth.mode=always_light is an
// explicit, deliberate cost-control choice ("always subtract to light"),
// and this floor is a history-based "always add rigor" mechanism -- the
// two are in direct, irreconcilable tension for exactly this one case,
// and the explicit admin choice must win. Before this fix, every one of
// this package's real callers applied Floor unconditionally, so a PR that
// had EVER gone deep once stayed deep on every subsequent auto-triggered
// push FOREVER, even after an admin flipped the repo to always_light --
// silently defeating the override, and (worse) persisting a self-
// contradictory decision record (mode: "always_light" alongside
// depth: "deep"). See internal/app/sessionactor/reviewretrigger.go's own
// handleReviewRetriggerDebounceTimer for where this precedence is now
// actually enforced, and internal/app/reviewtriage.ComputeDecision's own
// doc comment for how a caller obtains the prior depth this floor needs
// without a second, redundant Postgres read.
func Floor(fresh, prior ReviewDepth) ReviewDepth {
	if rank(prior) > rank(fresh) {
		return prior
	}
	return fresh
}

// NonFloorReasons are the decision reasons whose depth is never a later
// review's floor (§26.3: "a depth chosen because an input could not be
// read is never a floor"). A review routed deep only because its input
// could not be read says nothing about the change itself; flooring on it
// would make one transient read failure keep a pull request deep for
// good. The recorded reason means exactly that because Decide checks the
// unreadable-input rule after every other deep rule: a review that was
// also deep for a real reason records that reason instead, and floors. The floor's own read (internal/app/reviewtriage.ComputeDecision)
// passes this list to Postgres and skips every verdict whose producing
// turn's recorded reason is in it, flooring on the latest verdict before
// them instead -- so a pull request that went deep for a real reason
// before an unreadable review still stays deep after it.
//
// Returned fresh on every call.
func NonFloorReasons() []Reason {
	return []Reason{ReasonInputUnreadable}
}
