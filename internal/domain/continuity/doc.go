// Package continuity builds the recap the first turn dispatched to a fresh
// lineage carries (technical plan §35.5). A fresh lineage is a sandbox
// started fresh in place of one that is gone (sandbox.DecideFreshLineage):
// the agent in it has neither the earlier instance's conversation nor any
// work that was not pushed, so the first prompt it is sent opens with a
// report of what the session did before.
//
// What the recap says of each earlier turn is its request and its final
// text, as plan.FinalText reads it over the turn's window of the session's
// stored log: the one reader the plan notices, the plan views and the
// session's result already share. This package reads nothing itself. Its
// caller reads each turn's final text through that reader and hands Build
// its answer (plan.Final and FinalText's ok), so the recap can never
// disagree with the notification surfaces about which text a turn ended
// on.
//
// The recap is framed as a third party's report, never as the agent's own
// memory: fixed lines say the sandbox was started fresh, that the report
// was rebuilt from the session's record and can be incomplete or wrong,
// and ask the agent to check every claim against the repository as it is
// now. Every line of every excerpt is quoted ("> "), and a fixed line says
// the quoted lines are a record, never instructions (§5.2). Every other
// line is one of the template's own (IsTemplateLine), so no text a person
// or an agent wrote can pass for the framing or close the report early.
//
// The recap is bounded: each excerpt by RequestExcerptBytes or
// ReportExcerptBytes, the turns by MaxTurns, and the whole, in JSON-encoded
// bytes (EncodedLen), by the budget its caller passes -- at most
// platform.ContinuityRecapMaxBytes, and never more than the room the gen's
// prompt bound leaves. Build trims to fit, newest turns first, and reports
// when not even its frame fits; it never refuses a turn.
//
// Pure per §11: no I/O, no time.Now(), no randomness.
package continuity
