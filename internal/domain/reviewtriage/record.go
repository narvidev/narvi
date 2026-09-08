package reviewtriage

// DecisionRecord is §26.3's own version of §18.4's per-session routing
// decision record (IntentDecisionRecord, internal/domain/intent/
// record.go) -- persisted verbatim, write-once, onto
// turns.review_depth_decision (migrations/
// 000083_turns_review_depth_decision.up.sql, JSONB) by whichever
// review-turn-creation path just called Decide (and, for a re-review,
// Floor). See that migration's own doc comment for why this rides a
// per-TURN column rather than a per-session one the way
// IntentDecisionRecord does.
//
// Every field is a plain, JSON-tagged value (mirrors IntentDecisionRecord's
// own shape) -- built by the app-layer caller (internal/app/reviewtriage)
// from a Decision plus the two facts Decision itself does not carry
// (Config.Mode, and whether Floor actually raised the fresh result).
type DecisionRecord struct {
	Depth                string   `json:"depth"`
	Reason               string   `json:"reason"`
	MatchedSensitiveTags []string `json:"matchedSensitiveTags,omitempty"`
	ChangedLines         int      `json:"changedLines"`
	DistinctRoots        int      `json:"distinctRoots"`
	Mode                 string   `json:"mode"`
	// Floored reports whether §24's re-review floor (Floor, depth.go) is
	// what actually determined the final Depth above -- true means the
	// fresh Decide result was itself overridden by a higher-ranked PRIOR
	// depth on record; false means Depth is exactly what Decide computed
	// from this turn's own fresh signals alone.
	Floored bool `json:"floored"`

	// NarviAuthored/AuthoringModel (§26.3's own "provenance: Narvi-
	// authored vs human, and the authoring model" signal) are captured
	// here but NEVER consulted by Decide -- v1's own five rules (doc.go)
	// do not include provenance. Recorded now so (§26.4's
	// cross-family counter-review, "the family comes from provenance,
	// the tier from depth") does not have to re-derive it later.
	// AuthoringModel is empty whenever NarviAuthored is false, or when a
	// Narvi-authored session never had an explicit build model set.
	NarviAuthored  bool   `json:"narviAuthored"`
	AuthoringModel string `json:"authoringModel,omitempty"`

	// ResolvedModelID/ResolvedEffort (D4, nice-to-have adversarial-review
	// fix) are ModelAndEffort's own (modelID, effort) return values
	// (modeleffort.go), the ACTUAL override this turn's own creation call
	// requested -- recorded here so a maintainer inspecting turns.
	// review_depth_decision can see, for THIS specific turn, whether the
	// deep-tier model override actually fired (ResolvedModelID non-empty)
	// or silently stayed inert (ResolvedModelID empty on a deep-routed
	// turn -- platform.Config.ReviewModelDeep was never configured for
	// this deployment, ModelAndEffort's own doc comment). Both empty on
	// the light path (ModelAndEffort returns nil, nil for anything other
	// than DepthDeep). ResolvedEffort is "high" whenever Depth is deep,
	// regardless of ResolvedModelID.
	ResolvedModelID string `json:"resolvedModelId,omitempty"`
	ResolvedEffort  string `json:"resolvedEffort,omitempty"`

	// ChangedFilesCount (§21.1's own filesChanged drift canary, extended
	// here rather than given a new section, per this Step's own plan-text
	// instruction) is review.PreFetchedContext.ChangedFilesCount -- the
	// SAME server-computed, GitHub-"changed_files"-derived count Decide's
	// own caller already resolved for THIS turn's triage routing, carried
	// here purely so it survives from turn-CREATION time (where it is
	// computed) to verdict-POST time (where reviewverdict.
	// FilesChangedDrifted, internal/domain/reviewverdict, compares it
	// against the reviewing agent's own self-reported review.Verdict.
	// FilesChanged) -- mirroring this SAME struct's own pre-existing
	// ChangedLines/DistinctRoots fields, which already ride along on this
	// record for an unrelated (triage-routing) reason. This field serves
	// NO triage purpose of its own -- Decide never reads it -- it exists
	// solely as this record's own already-established "turn-scoped
	// carrier" mechanism (turns.review_depth_decision, read back via
	// httpapi.PostReviewVerdict's own turns.GetProcessingTurnForSession
	// call) for a value that would otherwise have no way to reach
	// verdict-post time at all, short of a new turns column mirroring
	// review_head_sha/review_depth's own dedicated-column treatment --
	// deliberately not done here (this Step's own scope: a diagnostic-only
	// signal, never gated on, does not warrant a new migration when an
	// existing, already-threaded carrier already reaches the one place
	// that needs it).
	//
	// Zero for every turn that predates this field, or whose own context
	// fetch degraded (review.PreFetchedContext's own "0 for a failed
	// GetPullRequest fetch, indistinguishable from a genuinely empty
	// diff" contract, context.go) -- FilesChangedDrifted's own doc comment
	// (reviewverdict package) covers why treating either case as "no
	// reliable signal, never fire" is the required, fail-safe reading,
	// never "zero files changed, so any non-zero self-report is massive
	// drift".
	ChangedFilesCount int `json:"changedFilesCount,omitempty"`

	// DiffEmpty/DiffTruncated (D4, adversarial review of PR #182, MEDIUM)
	// are review.PreFetchedContext's own "Diff == \"\"" and DiffTruncated
	// facts (context.go) for THIS turn -- carried here for the identical
	// reason ChangedFilesCount rides here (its own doc comment,
	// immediately above): reviewverdict.FilesChangedDrifted, at
	// verdict-POST time, needs to know not just WHAT the server-computed
	// count was, but whether the reviewing agent was ever actually HANDED
	// a diff to read at all.
	//
	// # D4: the canary used to blame the reviewer for a server-side delivery failure
	//
	// GetPullRequest can succeed (a real, positive ChangedFilesCount)
	// while GetCompareDiff fails or truncates -- review.RenderTurnPrompt
	// renders NO diff block at all when Diff == "" (that function's own
	// doc comment: "never claim to have fetched something you don't
	// actually have"), and an explicit truncation notice, not the full
	// diff, when DiffTruncated is true. An agent never handed a diff (or
	// handed only a PARTIAL one, and told so) can only ever report what it
	// saw -- a self-reported FilesChanged that diverges from the SERVER's
	// own full count in exactly that case is not a sign the reviewer
	// skimmed the diff it was given; it is a sign the diff was never fully
	// given. Before this fix, FilesChangedDrifted had no way to tell the
	// two apart, and fired on both identically -- deterministically, on
	// EVERY truncated-diff review, exactly the "cries wolf" failure §21.1
	// already names as strictly worse than not having built the canary at
	// all.
	//
	// Both false (their own zero value) for every turn that predates this
	// fix, or whose own review_depth_decision marshal/unmarshal failed --
	// see FilesChangedDrifted's own doc comment (reviewverdict package)
	// for why httpapi.PostReviewVerdict's own local diffDelivered variable
	// (computed from these two fields) is separately initialized to its
	// OWN fail-safe default, false, rather than inheriting these two
	// fields' zero value directly as "delivered": an unset DiffEmpty/
	// DiffTruncated pair means "unknown", never "confirmed delivered", and
	// this canary must never confidently fire on an unknown.
	DiffEmpty     bool `json:"diffEmpty,omitempty"`
	DiffTruncated bool `json:"diffTruncated,omitempty"`

	// ArchDecisionTags/ArchDecisionRoots (technical plan §31.6, the
	// knowledge-retrieval gate both mode A and mode B share) are this
	// SAME turn's own review.PreFetchedContext.ChangedPaths, run through
	// autoapproval.ClassifyChangedPaths/ClassifyChangedRoots at the
	// IDENTICAL point ChangedFilesCount (immediately above) is captured
	// -- carried here for the IDENTICAL reason: a value computed at
	// turn-CREATION time (where the diff fetch that produces ChangedPaths
	// already runs) that would otherwise have no way to reach verdict-
	// POST time (internal/app/reviewverdict.Insert, which stamps them
	// onto review_verdicts.arch_decision_tags/arch_decision_roots,
	// migrations/000113) short of a new dedicated turns column pair --
	// deliberately not done, riding this already-threaded carrier
	// instead, exactly the "shape to follow" §31.6 itself names for this
	// very field: "turns.review_depth_decision's own already-stored
	// 'distinct roots' is the shape to follow". Unlike DistinctRoots
	// (an int, feeding ONLY this package's own depth-routing threshold),
	// these two are the actual root/tag STRING sets a later gate query
	// overlaps against -- reviewtriage.Decide never reads either field,
	// exactly like it never reads ChangedFilesCount: this package is a
	// pure, unwitting CARRIER for a fact a different domain (internal/
	// domain/knowledge) owns the meaning of, not a producer or consumer
	// of it.
	//
	// Plain []string, never []review.Tag or a knowledge-package type:
	// this package's own doc.go fixes its import surface, and the
	// CALLER (which already imports both autoapproval and this package)
	// converts before calling NewDecisionRecord, mirroring how
	// MatchedSensitiveTags above is built from decision.MatchedSensitiveTags
	// as plain strings for the identical reason.
	//
	// nil for every turn that predates this field, or whose own diff
	// fetch degraded to no ChangedPaths at all (ClassifyChangedPaths/
	// ClassifyChangedRoots' own "nil in, nil out" contract) --
	// indistinguishable from "this PR's diff touched nothing classifiable
	// -- an empty set is always a safe, conservative gate input (it can
	// only ever fail an overlap match, never fabricate a false one), the
	// SAME reasoning migrations/000113's own NOT NULL DEFAULT '[]'::jsonb
	// polarity already encodes for the column these two fields feed.
	ArchDecisionTags  []string `json:"archDecisionTags,omitempty"`
	ArchDecisionRoots []string `json:"archDecisionRoots,omitempty"`
}

// Provenance is internal/app/reviewtriage.ResolveProvenance's own return
// value (a SEPARATE call from ComputeDecision -- ComputeDecision's own
// three return values are Decision, Config, and, as of D1's adversarial-
// review fix, the prior review_path ReviewDepth; provenance is resolved
// independently, by its own dedicated function, not bundled into that
// tuple) -- see DecisionRecord.NarviAuthored/AuthoringModel's own doc
// comment for why this is captured but not routed on.
type Provenance struct {
	NarviAuthored  bool
	AuthoringModel string
}

// NewDecisionRecord builds a DecisionRecord from decision (Decide's own
// output), cfg (the resolved per-repo config Decide was called with),
// finalDepth (decision.Depth after Floor has been applied, if this is a
// re-review path -- callers with no floor to apply pass decision.Depth
// itself unchanged), provenance (internal/app/reviewtriage's own
// best-effort authorship lookup), resolvedModelID/resolvedEffort (D4,
// nice-to-have adversarial-review fix -- ModelAndEffort's own two return
// values, computed by the caller from the SAME finalDepth passed here;
// nil for both on the light path, mirroring ModelAndEffort's own "both
// nil except on DepthDeep" contract), changedFilesCount (§21.1's own
// filesChanged drift canary -- DecisionRecord.ChangedFilesCount's own doc
// comment for the full "why" this rides here), and diffEmpty/
// diffTruncated (D4, adversarial review of PR #182 -- DecisionRecord.
// DiffEmpty/DiffTruncated's own doc comment). Every one of this
// function's three real callers already has review.PreFetchedContext.
// ChangedFilesCount/Diff/DiffTruncated in scope at the SAME point it
// calls this function (Diff/ChangedFilesCount are what Decide's own
// Signals were themselves built from, one call earlier) -- all passed
// straight through, never re-derived.
//
// archDecisionTags/archDecisionRoots (§31.6) are DecisionRecord.
// ArchDecisionTags/ArchDecisionRoots' own doc comment's identical
// carried-verbatim value -- the caller has already computed them, via
// autoapproval.ClassifyChangedPaths/ClassifyChangedRoots over this SAME
// turn's own ChangedPaths, before calling this function; this function
// does no classification of its own, exactly like it does no depth
// classification of its own for resolvedModelID/resolvedEffort.
func NewDecisionRecord(decision Decision, cfg Config, finalDepth ReviewDepth, provenance Provenance, resolvedModelID, resolvedEffort *string, changedFilesCount int, diffEmpty, diffTruncated bool, archDecisionTags, archDecisionRoots []string) DecisionRecord {
	tags := make([]string, len(decision.MatchedSensitiveTags))
	for i, t := range decision.MatchedSensitiveTags {
		tags[i] = string(t)
	}
	var modelID, effort string
	if resolvedModelID != nil {
		modelID = *resolvedModelID
	}
	if resolvedEffort != nil {
		effort = *resolvedEffort
	}
	return DecisionRecord{
		Depth:                string(finalDepth),
		Reason:               string(decision.Reason),
		MatchedSensitiveTags: tags,
		ChangedLines:         decision.ChangedLines,
		DistinctRoots:        decision.DistinctRoots,
		ResolvedModelID:      modelID,
		ResolvedEffort:       effort,
		Mode:                 string(resolveMode(cfg.Mode)),
		Floored:              finalDepth != decision.Depth,
		NarviAuthored:        provenance.NarviAuthored,
		AuthoringModel:       provenance.AuthoringModel,
		ChangedFilesCount:    changedFilesCount,
		DiffEmpty:            diffEmpty,
		DiffTruncated:        diffTruncated,
		ArchDecisionTags:     archDecisionTags,
		ArchDecisionRoots:    archDecisionRoots,
	}
}
