# Analyses

Dated, point-in-time studies that informed a plan change. Each one pins the
commit it was checked against, so a reader can tell what was true when it was
written and what has moved since.

These are **not normative**. `docs/TECHNICAL_PLAN.md` says what the system
must do and `docs/IMPLEMENTATION_PLAN.md` says in what order it gets built;
an analysis here only records how a conclusion was reached. Where the two
disagree, the plans win and the analysis is stale by definition.

That staleness is the point of keeping them rather than a reason not to. A
plan row states a decision; it rarely has room for the measurement, the
refuted alternative, or the thing that turned out to be false. Without that,
the first person to disagree with a row re-runs the whole investigation to
find out whether it was ever grounded.

## Convention

One file per study, named `YYYY-MM-DD-<subject>.md`, opening with the commit
it verified against and the limits of what it checked. An analysis that does
not say what it did **not** verify is the more dangerous kind: it reads as
exhaustive.

## Contents

| File | Subject |
|---|---|
| [`2026-09-15-background-agents-pr-732-734-737.md`](2026-09-15-background-agents-pr-732-734-737.md) | What release 732 and PRs 734-737 of the upstream project are worth taking into Narvi, and what must not be taken with them. Produced Steps 172-176 and four row amendments (PR #289). |
| [`2026-09-15-background-agents-doc-gaps.md`](2026-09-15-background-agents-doc-gaps.md) | What remained absent from the documentation after #289 — the follow-up inventory. Found the §26/§19 misreference in Step 176 (PR #290). |
