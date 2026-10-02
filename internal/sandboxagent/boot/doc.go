// Package boot implements sandbox-agent's own boot-sequence orchestration
// (§6.4, §5.3, §14.2): reading and validating process configuration
// (config.go), assembling the boot fingerprint §5.3 requires be logged
// first (fingerprint.go), running the hook policy from
// internal/domain/sandboxboot against real, disk-resident scripts using
// internal/sandboxagent/supervisor for execution (hooks.go), and
// RunBoot (runboot.go), the top-level per-repo dispatcher that
// chooses, per repo, between the multi-service manifest contract
// (internal/sandboxagent/services, §14.2) and this package's own
// setup.sh/start.sh hook contract (§6.4), falling back to the latter
// whenever a repo has no .narvi/services.yml. RunBoot (like RunHooks
// before it) takes its repo list as a plain []RepoInfo parameter; main.go populates it from cmd/sandbox-agent's own
// internal/sandboxagent/gitclone.CloneAll results whenever Config.
// SessionConfig is present, and passes nil (today's original no-op)
// otherwise. §6.1/§7 are expected to extend this package (or add
// sibling ones) further as the boot sequence grows.
//
// This also closes the "SESSION_CONFIG delivery" gap §6.4/§14.2
// repeatedly flagged as honest, undecided territory: config.go's Load()
// now additionally reads an OPTIONAL NARVI_SESSION_CONFIG env var carrying
// the full SESSION_CONFIG document as JSON, parsed into Config.
// SessionConfig (nil when the env var is absent -- a fully valid, correct
// state; dev/CI environments have no live session). When present, its own
// bootMode field is cross-checked against the separately-read
// NARVI_BOOT_MODE, a fail-fast *ModeMismatchError on disagreement --
// the same reconciliation shape as ports.CreateSpec.Validate's
// GenMismatchError. Load() also reads an optional
// NARVI_CREDENTIAL_CACHE_DIR (default /tmp/narvi-credentials, deliberately
// outside WorkspaceDir) consumed by the sibling
// internal/sandboxagent/credentials package's on-disk credential cache.
//
// This package is impure (env vars, disk stat calls, `git rev-parse`
// subprocesses, process supervision) and follows the same fail-fast,
// named-error convention as internal/platform/config.go -- but is a
// deliberately separate, small implementation: sandbox-agent's env vars
// are entirely disjoint from control-plane's own, and platform/config.go
// itself is out of scope for this Step.
//
// One further optional env var: NARVI_SANDBOX_ID (default
// "", an HONEST GAP -- see Config.SandboxID's own doc comment), consumed
// by the sibling internal/sandboxagent/wsbridge package as the sandbox WS
// connection's X-Sandbox-ID header value. Nothing else in this package
// changes: RunBoot, hook policy, and the fingerprint remain exactly as
// §6.4/§14.2 left them.
//
// And NARVI_AGENT_STATE_DIR (default /tmp/narvi-agent-state, beside the
// credential cache and likewise outside WorkspaceDir): where the sibling
// wsbridge package keeps its prompt journal, technical plan §3.3's prompt
// receipts -- see Config.AgentStateDir's own doc comment.
package boot
