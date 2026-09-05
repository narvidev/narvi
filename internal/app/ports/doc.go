// Package ports declares the application's port interfaces — the
// boundary between the app layer and the outside world. Ports are pure
// contracts; adapters (internal/adapters/*) implement them, never the
// other way around (CLAUDE.md: "Don't couple a port to a single adapter.
// Interfaces in /internal/app/ports must hold for more than one
// implementation").
//
// This package is deliberately restricted to interface + value-type
// declarations: no I/O, no time.Now(), no randomness. It may import
// contracts/gen/go/* (generated wire-contract types — e.g.
// sessionconfig.SessionConfig, which CreateSpec carries) and the standard
// library only. It MUST NOT import internal/adapters/* — doing so would
// invert the dependency direction hexagonal architecture exists to
// enforce: adapters depend on ports, ports never depend on adapters.
//
// SandboxProvider (§4.1) is the first port implemented,
// against two adapters: internal/adapters/outbound/modal (real)
// and internal/adapters/outbound/rwx (a stub, later becoming a real
// second implementation — same interface, per §4.1.1's
// design). See sandboxprovider.go, capabilities.go, createspec.go,
// refs.go, and providererror.go for the interface and its supporting
// value types.
//
// AgentRuntime (§4.2, the OpenCode anti-corruption layer) is the SECOND
// port, added at §7, against internal/adapters/outbound/opencode
// (this Step, real) — CLAUDE.md's "the agent runtime... is expected to
// gain a second adapter" line applies to this interface exactly as it
// already did to SandboxProvider: nothing OpenCode-specific may leak into
// agentruntime.go's signature, even though OpenCode is (for now) its only
// implementation. See agentruntime.go for the interface, its AgentEvent/
// EventSink supporting types, and ClassifyAgentEvent (the shared "which
// wire event types are critical" classification every AgentRuntime
// adapter should reuse rather than reimplement).
//
// SandboxCommander (sandboxcommander.go) and SourceControl (sourcecontrol.go)
// are the THIRD and FOURTH ports, both added at §9.3 ("e2e happy
// path"): SandboxCommander lets app/sessionactor push an outbound command
// to a session's live sandbox WS connection (internal/adapters/inbound/
// wshub's own SandboxRegistry is the implementation) without importing
// wshub itself; SourceControl creates a pull request
// (internal/adapters/outbound/githubapi is the first real implementation,
// internal/adapters/outbound/gitlabapi remains an untouched stub for a
// future Step).
//
// Notifier (notifier.go) is the FIFTH port, added at §5.1 ("outbox
// delivery", §5.1/§5.4): a single Deliver(ctx, Notification) method,
// implemented by THREE real adapters (internal/adapters/outbound/
// slackapi, linearapi, githubapi) -- see notifier.go's own doc comment for
// why one interface with three implementations is the right shape here,
// even though (unlike SandboxProvider/SourceControl, where two adapters
// genuinely implement the same operation against different providers)
// each of these three is only ever asked to Deliver its own matching
// NotificationKind in practice, by internal/app/outboxworker's own
// kind->Notifier routing.
//
// LLM (llm.go) and IntentClassifier (intentclassifier.go) are the SIXTH
// and SEVENTH ports, both added at §8.3/§18: LLM is a
// genuinely reusable, provider-agnostic structured-output text-completion
// port (internal/adapters/outbound/llm's Anthropic adapter is the first
// real implementation this Step; a future internal/adapters/outbound/
// openai remains an untouched stub, PR-50) — nothing Anthropic- or
// OpenAI-specific may leak into either port's signature. IntentClassifier
// is the never-throw classification port internal/app/intentclassifier
// implements against LLM.
//
// BlobStore (blobstore.go, blobstoreerror.go) is the EIGHTH port, added at
// §8.6 ("uploads, blob storage & the in-sandbox download_file tool",
// §28): PresignPut/PresignGet/Stat/Delete against S3-compatible object
// storage, implemented by internal/adapters/outbound/objstore. Mirrors
// SandboxProvider/ProviderError's own "complete interface + typed
// {Transient, Code, Op, Err} error" shape exactly (§28.1), with its own
// BlobOp type (kept distinct from SandboxProvider's Op — see
// blobstoreerror.go's own doc comment for why) and its own typed
// not-found sentinel, ErrBlobNotFound.
//
// KnowledgeRanker (knowledgeranker.go) is the NINTH port, added at §34.7
// (docs/design/boundaries-design.md, section 2): orders the prior
// architecture decisions a server-derived gate has already admitted --
// Score returns one score per already-gated candidate and nothing else,
// so gate-then-rank (§31.6) is the shape of the signature rather than a
// convention an implementer has to remember. internal/domain/knowledge.
// RecencyRanker (public, no opinion) is the first implementation; a
// private hybrid ranker is expected to be the second, entering only
// through extension.Module.KnowledgeRanker.
//
// The remaining §4.3 ports — SessionStore/TurnStore/SandboxStore, Outbox,
// TimerScheduler, Clock — are out of scope for this Step and land in their
// own later Steps, each adding its own interface file here without
// touching any existing one.
package ports
