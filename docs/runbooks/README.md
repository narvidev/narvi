# Runbooks

**On-call at 3am? Start at [`docs/ONCALL.md`](../ONCALL.md)** — the entry
point that tells you which of the entries below to open, and what to do
when none of them fit. This page is the index; that one is the triage
flow.

Operator runbooks for the failure modes Narvi's own dashboards/alerts
(`deploy/observability/`) surface, drawn from the resilience catalog
(`docs/TECHNICAL_PLAN.md` §9.3, indexed scenario-by-scenario in
`test/resilience/README.md`) plus the operational surfaces Steps 73a/74/76
added. Each entry below states the symptom an operator sees, the metric or
log line that confirms it, the remediation, and — where one exists — the
resilience scenario that reproduces it. **Fewer, real entries, not a
complete-looking table**: a failure mode this system cannot actually have
gets no entry here, and a gap in what's actually buildable (e.g. no
automated outbox dead-letter replay) is stated as a gap, not glossed over.

| Runbook | Alert(s) it backs | §9.3 scenario |
|---|---|---|
| [outbox-delivery.md](outbox-delivery.md) | `OutboxLagHigh`, `OutboxDeadLetterAny` | #9 (Slack API 500s) |
| [orphan-sandboxes.md](orphan-sandboxes.md) | `OrphanReapRateHigh` | #5 (concurrent spawns / orphan GC) |
| [slow-boot-and-spawn.md](slow-boot-and-spawn.md) | `BootDurationP95High`, `SpawnLatencyP95High` | #3 (slow boot) |
| [watchdog-false-alarms.md](watchdog-false-alarms.md) | `WatchdogFalseAlarmRateHigh` | none (see entry) |
| [turn-false-failures.md](turn-false-failures.md) | `TurnFalseFailureAny` | #4 (late `execution_complete`), adjacent not identical — see entry |
| [sandbox-capability-refusals.md](sandbox-capability-refusals.md) | none (no metric exists yet — see entry) | #17 (restore-with-docker) |
| [signing-key-rotation.md](signing-key-rotation.md) | none (routine admin procedure, not a failure) | n/a |
| [mcp-client-cutoff.md](mcp-client-cutoff.md) | none (routine admin procedure, not a failure) | n/a |

## Cross-references, not duplicated here

- **Cohort-rollout rollback** (Step 76, §32): the full operator runbook —
  enrolling/de-enrolling a repository, arming/disarming cohort mode
  platform-wide, and verifying a rollback took effect — already lives at
  `docs/TECHNICAL_PLAN.md` §32.9. That is the single source of truth for
  rollback; this directory does not restate it. `session_rollout_refused_total`
  (tagged `spawn_source`) is the metric §32.9 itself already names.
- **Docker/egress substrate refusals** (Step 74, §27.5/§27.6): see
  [sandbox-capability-refusals.md](sandbox-capability-refusals.md) below —
  short enough not to need its own cross-reference note, but distinct from
  rollout refusals above (a substrate refusal means the *provider* can't
  honor what the *Environment* requires; a rollout refusal means the repo
  simply isn't enrolled yet).
- **Signing-key rotation** (Step 73a, §27.3): see
  [signing-key-rotation.md](signing-key-rotation.md) below.
- **Cutting off an MCP client** (§43.13-§43.19): revoking one user's
  authorization, deleting or disabling a client, pausing a registration
  mechanism, revoking every authorization, and what each does on the
  client's next call -- see [mcp-client-cutoff.md](mcp-client-cutoff.md).

## Instruments these runbooks rely on

Every metric name inside a ```` ```json narvi-metrics ```` fenced block —
this file's own, below, plus one inside most of the runbook files this
directory contains — is checked against the real registered OTel
instruments by `internal/ops`'s own CI-enforced `TestNoRunbookMetricDrift`
(`go test ./internal/ops/...`, part of `make test`) — see
`internal/ops/docmetrics.go`'s own top comment for why, and
[`docs/guides/README.md`](../guides/README.md)'s own "Prose is not
machine-verified" section for the general discipline this mirrors. A
metric named INSIDE a fenced block that the code stops emitting fails
that build, not just this documentation — a metric mentioned only in
free-running prose elsewhere in this directory carries no such guarantee,
same as any other unchecked sentence.

```json narvi-metrics
{"metrics": ["session_rollout_refused_total"]}
```
