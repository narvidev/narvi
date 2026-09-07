package knowledge

// ModeA is the fixed value every review turn is stamped with today
// (turns.review_knowledge_mode, migrations/000114; review_verdicts.
// knowledge_mode, migrations/000115) -- §31.2's own mode buffer. The
// admin-facing switch (repo_settings.review_knowledge_mode, "NULL = mode
// A") does not exist yet, so every repository runs mode A unconditionally
// for now; this constant is what that future switch will select
// ALONGSIDE a ModeB sibling this package does not define until it is
// needed -- "the column exists now, ahead of the switch, so a later
// change extends an already-proven buffer rather than retrofitting one
// onto live traffic" (migrations/000114's own doc comment).
const ModeA = "mode_a"
