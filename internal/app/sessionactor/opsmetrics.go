// This file (opsmetrics.go) is §5.3's ("ops: dashboards, alerts,
// runbooks", §5.3) own instrumentation fix: before this Step, §5.3's own
// metric list -- "spawn latency, boot phase durations, liveness gaps,
// watchdog activations (and how many were false alarms -- target: ~0),
// outbox lag, orphan GC count" -- was only PARTIALLY real. outbox lag
// (outboxworker's outbox_lag_seconds) and orphan GC count (reconciler's
// orphans_reaped) existed; boot phase durations existed sandbox-agent-side
// (internal/sandboxagent/boot/telemetry.go, internal/sandboxagent/gitclone/
// telemetry.go). Spawn latency, liveness gaps, and watchdog activations
// did not exist anywhere in this codebase -- internal/platform/otel.go's
// own top comment names PR-11/12, PR-13/14, PR-24, PR-35 as where they
// were EXPECTED to land, and none of those landings actually happened; a
// repo-wide grep for "watchdog" + a metric instrument, or "spawn" +
// "latency"/"duration" + a metric instrument, returns nothing production
// code ever registers.
//
// A fourth gap is specific to this Step's own plan row ("false failures"):
// nothing anywhere counts a turn the control plane terminalized Failed
// that the sandbox later, genuinely, reports as having succeeded. See
// recordFalseFailureIfApplicable's own doc comment (pushpr.go) for the
// full semantics this instrument captures and why.
//
// This file constructs the five new instruments those four gaps need,
// exactly once, mirroring registry.go's own contractDriftDetected
// precedent: built by newOpsMetrics from the SAME otel.Meter(meterName)
// NewRegistry already resolves for contractDriftDetected, then threaded
// through to every Actor this Registry hydrates via hydrate.go, exactly
// like every other Actor-shared field.

package sessionactor

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// watchdogKind names which of the three watchdog-style named timers (§2:
// inactivity, connecting_deadline, liveness_check) drove a
// transitionSandboxToSuspect call (timerfired.go) -- or "" for
// recordSpawnFailure's own DISTINCT permanent-provider-error path
// (dispatch.go), which enters Suspect too but is not a watchdog silence:
// the provider already returned a definite, classified error, there is no
// "how long has it been quiet" gap to measure, and grouping it into the
// SAME activation/false-alarm instruments as a genuine liveness watchdog
// would blur exactly the "how many were false alarms" ratio §5.3 asks
// for. transitionSandboxToSuspect only records watchdog_activation_total/
// sandbox_liveness_gap_seconds when watchdog is non-empty.
type watchdogKind string

const (
	watchdogInactivity         watchdogKind = "inactivity"
	watchdogConnectingDeadline watchdogKind = "connecting_deadline"
	watchdogLivenessCheck      watchdogKind = "liveness_check"
)

// opsMetrics bundles §5.3's five new OTel instruments, constructed
// exactly once per Registry (newOpsMetrics, called from NewRegistry) --
// mirroring internal/app/imagebuild's own buildTelemetry bundling
// precedent (telemetry.go) for a package that, like this one, already has
// a natural per-process constructor object (Registry/NewRegistry) to
// anchor eager construction to.
type opsMetrics struct {
	// spawnDuration is §5.3's "spawn latency": the wall-clock duration of
	// one real SandboxProvider.CreateSandbox/RestoreFromSnapshot/
	// ResumeSandbox call (dispatch.go's executeSpawn/executeRestore/
	// executeResume) -- the control-plane/adapter-side half of "boot"
	// internal/platform/otel.go's own top comment names ("Spawn latency in
	// particular belongs to the app/adapter layer (sessionactor or the
	// Modal provider), never to domain/sandbox"). Tagged by action
	// (spawn/restore/resume) and outcome (success/failure) -- both a
	// fixed, small enum, never session- or provider-id-scoped.
	spawnDuration metric.Float64Histogram

	// livenessGap is §5.3's "liveness gaps": how long a sandbox had shown
	// no sign of life by the moment a watchdog timer actually fired and
	// suspected it (EvaluateConnectingTimeout's own Elapsed /
	// EvaluateHeartbeatHealth's own Age, or the equivalent computed inline
	// for EvaluateInactivityTimeout, which returns neither) -- the
	// DURATION half of watchdog activity, complementing
	// watchdogActivation's own COUNT. Tagged by watchdog (the same three
	// values watchdogActivation uses).
	livenessGap metric.Float64Histogram

	// watchdogActivation is §5.3's "watchdog activations": incremented
	// every time transitionSandboxToSuspect (timerfired.go) runs for a
	// genuine watchdog-kind trigger (not recordSpawnFailure's permanent-
	// provider-error path -- see watchdogKind's own doc comment). Tagged
	// by watchdog.
	watchdogActivation metric.Int64Counter

	// watchdogFalseAlarm is §5.3's "(and how many were false alarms --
	// target: ~0)": incremented every time a Suspect sandbox recovers
	// DURING its own terminal_grace window (sandboxevent.go's own
	// Suspect-recovery-during-grace branch, §3.2's "any liveness signal
	// during grace returns to previous state") -- proof, after the fact,
	// that the watchdog that suspected it was WRONG: the sandbox was
	// alive the whole time. Tagged by recovered_from (the sandbox's own
	// pre_suspect_status -- ready/connecting/booting/spawning/
	// snapshotting, a fixed 5-value enum), NOT by watchdog: pre_suspect_
	// status alone cannot always distinguish inactivity from
	// liveness_check (both only ever fire from Ready, sandboxevent.go/
	// timerfired.go), and adding a persisted per-Suspect-episode watchdog
	// column just to disambiguate a false-alarm RATE this Step only needs
	// in aggregate (watchdogFalseAlarm / watchdogActivation, both summed
	// across every watchdog value) is more schema than this gap needs --
	// see this Step's own PR description for the full reasoning.
	watchdogFalseAlarm metric.Int64Counter

	// falseFailure is this Step's own plan-row-specific instrument ("false
	// failures"): incremented when a late, real, wire-level
	// execution_complete{outcome:completed} arrives for a session whose
	// own currently-derived state is already Failed with failure_reason =
	// timeout -- see recordFalseFailureIfApplicable's own doc comment
	// (pushpr.go) for the full semantics. Deliberately carries NO
	// attribute: the gate itself is already narrowed to exactly one
	// failure_reason value (timeout), so a label here would be constant,
	// not informative.
	falseFailure metric.Int64Counter

	// bootDuration/hookRerunDuration/gitFetchDuration/gitCheckoutDuration
	// (§33.3) are the four sandbox_agent_*_duration_seconds
	// histograms that used to be recorded INSIDE the ephemeral sandbox
	// process (internal/sandboxagent/boot/telemetry.go, internal/
	// sandboxagent/gitclone/telemetry.go, both deleted by this Step) and
	// are now recorded HERE instead, from the relayed best-effort
	// boot_timing sandbox-ws event (recordBootTiming, boottiming.go) --
	// see that file's own top comment for the full "ship the fact, record
	// centrally" reasoning (§33.3). Same instrument names and same
	// hand-tuned bucket slices as the deleted sandbox-side histograms.
	// Two DIFFERENT guards, because one check does not cover both: a
	// RENAME is caught by internal/ops's TestNoMetricDrift, which compares
	// these names against deploy/observability/{dashboards,alerts}; a
	// BUCKET-SHAPE change is not, since that check reads only the
	// registration call's name argument and has no notion of boundaries.
	// The slices are pinned by opsmetrics_buckets_test.go instead -- the
	// replacement for the bucket-shape test that was deleted along with
	// the sandbox-side files.
	bootDuration        metric.Float64Histogram
	hookRerunDuration   metric.Float64Histogram
	gitFetchDuration    metric.Float64Histogram
	gitCheckoutDuration metric.Float64Histogram

	// rolloutRefused is the Phase 6 audit's own fix for Finding 4:
	// session_rollout_refused_total (§32) was, before this fix,
	// incremented ONLY by httpapi.checkRolloutGate -- the session-creation-
	// time half of §32's own "fail-closed, twice" pair. The dispatch-time
	// half (this package's own refuseIfRolloutUnenrolled/
	// rolloutRefusalForDispatch, dispatch.go) refused spawns/restores/
	// resumes and turn dispatches identically, but never touched the
	// metric at all -- leaving one of the mechanism's two real enforcement
	// points invisible to the exact instrument §32.7/§32.9 point an
	// operator at. Registered under the SAME string name httpapi already
	// uses (rolloutgate.go) -- a metrics backend aggregates by instrument
	// name across meters, so this is genuinely the SAME counter from an
	// operator's own point of view, not a second, differently-named one --
	// tagged by the identical spawn_source attribute, and incremented
	// under the identical "genuine policy fact only, never a transient
	// read error" discipline checkRolloutGate already established (see
	// recordRolloutRefusal's own doc comment, below).
	rolloutRefused metric.Int64Counter

	// bootEvidenceFallback counts every sandbox moved Booting -> Ready by
	// §3.2's boot-evidence fallback (bootevidence.go): a generation that
	// showed no boot evidence, Booting past
	// platform.Timeouts.BootEvidenceFallback with its heartbeats still
	// arriving, whose null boot phase was then accepted as boot
	// completion. Only a sandbox-agent built before boot_timing existed
	// reaches it, so a non-zero rate says such agents are still booting --
	// most likely from an old snapshot lineage -- and each of those boots
	// held its first turn for the whole bound. Carries no attribute: which
	// sandbox is in the WARN line logged with it.
	bootEvidenceFallback metric.Int64Counter
}

// newOpsMetrics constructs all five instruments against meter -- the SAME
// meter NewRegistry already resolves once via otel.Meter(meterName) for
// contractDriftDetected, passed in rather than re-resolved, so this
// package still calls otel.Meter exactly once per Registry construction
// (mirroring imagebuild.NewBuilder's own newBuildTelemetry(meter) call
// shape). Returns a wrapped error (never a partially-nil opsMetrics) on
// the first construction failure -- matching NewBuilder's own established
// error-returning convention for its own counters.
func newOpsMetrics(meter metric.Meter) (opsMetrics, error) {
	spawnDuration, err := meter.Float64Histogram(
		"sandbox_spawn_duration_seconds",
		metric.WithDescription("Wall-clock duration of one real ports.SandboxProvider.CreateSandbox/RestoreFromSnapshot/ResumeSandbox call (§5.3: spawn latency) -- the control-plane/adapter-side provider round trip, distinct from the sandbox-agent-side sandbox_agent_boot_duration_seconds (boot phase durations). Tagged by action (spawn/restore/resume) and outcome (success/failure)."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(spawnDurationBuckets...),
	)
	if err != nil {
		return opsMetrics{}, fmt.Errorf("sessionactor: construct sandbox_spawn_duration_seconds histogram: %w", err)
	}

	livenessGap, err := meter.Float64Histogram(
		"sandbox_liveness_gap_seconds",
		metric.WithDescription("How long a sandbox had shown no sign of life by the moment a watchdog timer (inactivity/connecting_deadline/liveness_check) suspected it (§5.3: liveness gaps) -- the duration half of watchdog_activation_total's own count. Tagged by watchdog."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(livenessGapBuckets...),
	)
	if err != nil {
		return opsMetrics{}, fmt.Errorf("sessionactor: construct sandbox_liveness_gap_seconds histogram: %w", err)
	}

	watchdogActivation, err := meter.Int64Counter(
		"watchdog_activation_total",
		metric.WithDescription("Count of every sandbox suspected (transitioned to Suspect) by a liveness/inactivity watchdog (§5.3: watchdog activations). Tagged by watchdog (inactivity/connecting_deadline/liveness_check); excludes recordSpawnFailure's own distinct permanent-provider-error path, which is not a watchdog silence."),
		metric.WithUnit("{activation}"),
	)
	if err != nil {
		return opsMetrics{}, fmt.Errorf("sessionactor: construct watchdog_activation_total counter: %w", err)
	}

	watchdogFalseAlarm, err := meter.Int64Counter(
		"watchdog_false_alarm_total",
		metric.WithDescription("Count of every Suspect sandbox that recovered during its own terminal_grace window (§3.2: 'any liveness signal during grace returns to previous state') -- proof the watchdog that suspected it was wrong (§5.3: watchdog activations '...and how many were false alarms -- target: ~0'). Tagged by recovered_from (the sandbox's own pre_suspect_status)."),
		metric.WithUnit("{alarm}"),
	)
	if err != nil {
		return opsMetrics{}, fmt.Errorf("sessionactor: construct watchdog_false_alarm_total counter: %w", err)
	}

	falseFailure, err := meter.Int64Counter(
		"turn_false_failure_total",
		metric.WithDescription("Count of every late, real execution_complete{outcome:completed} that arrived for a session already terminalized Failed with failure_reason=timeout (§9.3 scenario #4's own late-execution_complete class; §5.3's 'false failures' instrument) -- the control plane's own turn_deadline inference, contradicted after the fact by the sandbox genuinely finishing."),
		metric.WithUnit("{turn}"),
	)
	if err != nil {
		return opsMetrics{}, fmt.Errorf("sessionactor: construct turn_false_failure_total counter: %w", err)
	}

	bootDuration, err := meter.Float64Histogram(
		"sandbox_agent_boot_duration_seconds",
		metric.WithDescription("Wall-clock duration of one sandbox-agent boot-to-ready sequence (repo clone/sync through RunBoot's own hook/service startup) -- the total warm-boot latency §19.6's gating question needs as its own denominator when judging whether a hook rerun is materially eroding it. Recorded here from a relayed best-effort boot_timing event (§33.3); the wall-clock bracket itself is still measured sandbox-side, on the sandbox's own clock."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(bootDurationBuckets...),
	)
	if err != nil {
		return opsMetrics{}, fmt.Errorf("sessionactor: construct sandbox_agent_boot_duration_seconds histogram: %w", err)
	}

	hookRerunDuration, err := meter.Float64Histogram(
		"sandbox_agent_hook_rerun_duration_seconds",
		metric.WithDescription("Wall-clock duration of one sandbox-agent boot hook run (setup.sh/start.sh), including a workspaceMoved-triggered non-fatal setup.sh rerun under repo_image (§19.4/§19.5). Recorded here from a relayed best-effort boot_timing event (§33.3); the wall-clock bracket itself is still measured sandbox-side, on the sandbox's own clock."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(hookRerunDurationBuckets...),
	)
	if err != nil {
		return opsMetrics{}, fmt.Errorf("sessionactor: construct sandbox_agent_hook_rerun_duration_seconds histogram: %w", err)
	}

	gitFetchDuration, err := meter.Float64Histogram(
		"sandbox_agent_git_fetch_duration_seconds",
		metric.WithDescription("Wall-clock duration of SyncAll's own §19.3 boot-time fetch step (the default-branch and target-branch git fetch calls together) for one repo. Recorded here from a relayed best-effort boot_timing event (§33.3); the wall-clock bracket itself is still measured sandbox-side, on the sandbox's own clock."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(gitFetchDurationBuckets...),
	)
	if err != nil {
		return opsMetrics{}, fmt.Errorf("sessionactor: construct sandbox_agent_git_fetch_duration_seconds histogram: %w", err)
	}

	gitCheckoutDuration, err := meter.Float64Histogram(
		"sandbox_agent_git_checkout_duration_seconds",
		metric.WithDescription("Wall-clock duration of SyncAll's own checkoutBranch call (checkout onto the session branch, creating it from a resolved base if absent) for one repo -- excludes the fetch step and any stash push/pop, each its own separately-timed phase. Recorded here from a relayed best-effort boot_timing event (§33.3); the wall-clock bracket itself is still measured sandbox-side, on the sandbox's own clock."),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(gitCheckoutDurationBuckets...),
	)
	if err != nil {
		return opsMetrics{}, fmt.Errorf("sessionactor: construct sandbox_agent_git_checkout_duration_seconds histogram: %w", err)
	}

	rolloutRefused, err := meter.Int64Counter(
		"session_rollout_refused_total",
		metric.WithDescription("Count of every session-creation attempt, spawn/restore/resume attempt, or turn dispatch refused by §10's cohort-rollout gate (§32) because a named repo was not enrolled (repo_settings.sessions_enabled) -- the SAME instrument httpapi.checkRolloutGate registers (internal/adapters/inbound/httpapi/rolloutgate.go), incremented here too for this package's own two dispatch-time re-checks (refuseIfRolloutUnenrolled, rolloutRefusalForDispatch). Tagged by spawn_source. Only a genuine, DEMONSTRATED policy refusal increments this -- a refusal caused by a transient repo_settings read error never does, mirroring checkRolloutGate's own identical fail-closed-vs-terminal discipline (§32.5)."),
		metric.WithUnit("{refusal}"),
	)
	if err != nil {
		return opsMetrics{}, fmt.Errorf("sessionactor: construct session_rollout_refused_total counter: %w", err)
	}

	bootEvidenceFallback, err := meter.Int64Counter(
		"sandbox_boot_evidence_fallback_total",
		metric.WithDescription("Count of every sandbox moved Booting -> Ready by §3.2's boot-evidence fallback: a generation that showed no boot evidence, Booting past platform.Timeouts.BootEvidenceFallback with its heartbeats still arriving, whose null boot phase was then accepted as boot completion. Only a sandbox-agent built before boot_timing existed (2026-08-20), booting a repo with no service and no Docker, reaches it -- typically restored from an old snapshot lineage -- and each such boot held its first turn for the whole bound."),
		metric.WithUnit("{sandbox}"),
	)
	if err != nil {
		return opsMetrics{}, fmt.Errorf("sessionactor: construct sandbox_boot_evidence_fallback_total counter: %w", err)
	}

	return opsMetrics{
		spawnDuration:        spawnDuration,
		livenessGap:          livenessGap,
		watchdogActivation:   watchdogActivation,
		watchdogFalseAlarm:   watchdogFalseAlarm,
		falseFailure:         falseFailure,
		bootDuration:         bootDuration,
		hookRerunDuration:    hookRerunDuration,
		gitFetchDuration:     gitFetchDuration,
		gitCheckoutDuration:  gitCheckoutDuration,
		rolloutRefused:       rolloutRefused,
		bootEvidenceFallback: bootEvidenceFallback,
	}, nil
}

// recordBootEvidenceFallback increments sandbox_boot_evidence_fallback_total
// -- handleSandboxEvent's (sandboxevent.go) one call site, once the
// transaction that accepted a null boot phase through §3.2's fallback has
// committed.
func (a *Actor) recordBootEvidenceFallback(ctx context.Context) {
	if a.opsMetrics.bootEvidenceFallback == nil {
		return
	}
	a.opsMetrics.bootEvidenceFallback.Add(ctx, 1)
}

// spawnDurationBuckets mirrors internal/app/imagebuild's own
// buildDurationBuckets Finding-4 reasoning (concentrate resolution where
// duration is actually expected to live, not the OTel SDK's own
// millisecond-oriented default boundaries), scaled to a real provider
// spawn/restore/resume call's own expected range: sub-second to a few
// seconds typically, up to platform.Timeouts.ProviderHTTPClientTimeout's
// own ceiling (5 minutes) for a genuinely slow cold start.
var spawnDurationBuckets = []float64{
	0.1, 0.25, 0.5, 1, 2, 3, 5, 7.5, 10, 15, 20, 30, 45, 60, 90, 120, 180, 300,
}

// livenessGapBuckets covers the full range EITHER budget in
// domain/sandbox.LivenessConfig can produce: SteadyHeartbeatBudget
// (default 90s) up through FirstConnectBudget (default 240s), plus
// headroom for a config override or a genuinely pathological gap.
var livenessGapBuckets = []float64{
	10, 20, 30, 45, 60, 90, 120, 180, 240, 300, 450, 600, 900,
}

// bootDurationBuckets is byte-for-byte the deleted internal/sandboxagent/
// boot/telemetry.go's own bootDurationBuckets (§33.3: "same names, same
// hand-tuned bucket slices"): a total boot sequence -- even a warm one --
// includes network-bound git operations (fetch/checkout) and service
// readiness polling on top of the hook itself, so it is never sub-second
// the way a single warm hook rerun can be; the low end starts at 1s, and
// the upper end reaches 900s (15min) to still usefully bucket a genuinely
// cold BootModeBuild image-build boot.
var bootDurationBuckets = []float64{
	1, 2, 3, 5, 7.5, 10, 15, 20, 30, 45, 60, 90, 120, 180, 300, 600, 900,
}

// hookRerunDurationBuckets is byte-for-byte the deleted internal/
// sandboxagent/boot/telemetry.go's own hookRerunDurationBuckets: this
// instrument's own unit is seconds, not the OTel SDK's usual implicit
// milliseconds, so the SDK's own default boundaries would lump every
// reasonably-healthy (sub-5-second) rerun into a single [0, 5) bucket --
// these concentrate resolution in the sub-5s/sub-30s range a warm rerun is
// expected to live in, while still keeping coarse upper buckets for a
// genuinely cold BootModeBuild/BootModeFresh setup.sh run.
var hookRerunDurationBuckets = []float64{
	0.1, 0.25, 0.5, 0.75, 1, 1.5, 2, 3, 4, 5, 7.5, 10, 15, 20, 30, 60, 120, 300, 600,
}

// gitFetchDurationBuckets is byte-for-byte the deleted internal/
// sandboxagent/gitclone/telemetry.go's own gitFetchDurationBuckets:
// concentrates resolution in the expected sub-single-digit-second range for
// a warm repo_image fetch, while still reaching GitFetchStepTimeout's own
// 90s ceiling.
var gitFetchDurationBuckets = []float64{
	0.1, 0.25, 0.5, 1, 2, 3, 5, 7.5, 10, 15, 20, 30, 45, 60, 90, 120,
}

// gitCheckoutDurationBuckets is byte-for-byte the deleted internal/
// sandboxagent/gitclone/telemetry.go's own gitCheckoutDurationBuckets:
// shifted slightly lower than gitFetchDurationBuckets -- checkoutBranch is
// a local-only git operation (no network round trip), so it is typically
// faster still.
var gitCheckoutDurationBuckets = []float64{
	0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 7.5, 10, 15, 20, 30, 60,
}

// recordSpawnDuration records one real provider spawn/restore/resume
// call's own wall-clock duration -- executeSpawn/executeRestore/
// executeResume's (dispatch.go) one shared call site.
func (a *Actor) recordSpawnDuration(ctx context.Context, action string, seconds float64, failed bool) {
	if a.opsMetrics.spawnDuration == nil {
		// Defensive only: every real Actor is hydrated with a non-nil
		// opsMetrics (hydrate.go), but some tests construct an Actor
		// directly via a struct literal (mirroring commander/provider's
		// own documented "may be nil in tests" convention) without one.
		return
	}
	outcome := "success"
	if failed {
		outcome = "failure"
	}
	a.opsMetrics.spawnDuration.Record(ctx, seconds, metric.WithAttributes(
		attribute.String("action", action),
		attribute.String("outcome", outcome),
	))
}

// recordWatchdogActivation increments watchdog_activation_total and
// records gap into sandbox_liveness_gap_seconds, both tagged by watchdog
// -- transitionSandboxToSuspect's (timerfired.go) one shared call site,
// skipped entirely when watchdog == "" (recordSpawnFailure's own
// non-watchdog path -- see watchdogKind's own doc comment).
func (a *Actor) recordWatchdogActivation(ctx context.Context, watchdog watchdogKind, gap float64) {
	if watchdog == "" || a.opsMetrics.watchdogActivation == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("watchdog", string(watchdog)))
	a.opsMetrics.watchdogActivation.Add(ctx, 1, attrs)
	if a.opsMetrics.livenessGap != nil {
		a.opsMetrics.livenessGap.Record(ctx, gap, attrs)
	}
}

// recordWatchdogFalseAlarm increments watchdog_false_alarm_total, tagged
// by recoveredFrom (the sandbox's own pre_suspect_status) --
// handleSandboxEvent's (sandboxevent.go) own Suspect-recovery-during-grace
// success branch is this method's one call site.
func (a *Actor) recordWatchdogFalseAlarm(ctx context.Context, recoveredFrom string) {
	if a.opsMetrics.watchdogFalseAlarm == nil {
		return
	}
	a.opsMetrics.watchdogFalseAlarm.Add(ctx, 1, metric.WithAttributes(
		attribute.String("recovered_from", recoveredFrom),
	))
}

// recordFalseFailure increments turn_false_failure_total --
// recordFalseFailureIfApplicable's (pushpr.go) one call site, once it has
// already confirmed the session's own currently-derived state is Failed
// with failure_reason=timeout.
func (a *Actor) recordFalseFailure(ctx context.Context) {
	if a.opsMetrics.falseFailure == nil {
		return
	}
	a.opsMetrics.falseFailure.Add(ctx, 1)
}

// recordRolloutRefusal increments session_rollout_refused_total, tagged
// by spawnSource -- refuseIfRolloutUnenrolled's and
// rolloutRefusalForDispatch's own two call sites (dispatch.go), the
// dispatch-time half of §32's "fail-closed, twice" pair (Finding 4, Phase
// 6 audit). Callers must gate this on a GENUINE policy refusal
// themselves -- mirroring httpapi.recordRolloutRefusal's own identical
// contract (rolloutgate.go) exactly, never called at all for a refusal a
// transient repo_settings read error caused, since counting that here
// would make this metric lie to an operator about how many repos are
// actually being kept out by the cohort gate, the SAME reasoning
// checkRolloutGate's own doc comment already states in full.
func (a *Actor) recordRolloutRefusal(ctx context.Context, spawnSource string) {
	if a.opsMetrics.rolloutRefused == nil {
		return
	}
	a.opsMetrics.rolloutRefused.Add(ctx, 1, metric.WithAttributes(
		attribute.String("spawn_source", spawnSource),
	))
}
