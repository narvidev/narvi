// Package wsbridge implements sandbox-agent's half of the sandbox WS
// protocol (§6.1): dialing control-plane's own `wss://…/sessions/{id}/ws
// ?type=sandbox` endpoint, the ack/resend-on-reconnect guarantee for the 5
// critical event types, 30s heartbeats carrying the tracked boot phase,
// boot_progress translation, and inbound command dispatch (with per-message
// gen-fencing) to a pluggable CommandHandler.
//
// # The real, merged contract now has 6 critical types
//
// This package's own early implementation documented a then-real gap:
// technical plan §6.1's own prose already listed `sub_task_finish` as a 6th
// critical/ackable event type, but contracts/sandbox-ws/v1/events.schema.json
// had not yet been extended to match -- there was no
// contracts/gen/go/sandboxws.SubTaskStart or SubTaskFinish type to even
// construct. §7.1's own "Phasing" note assigned closing that gap to §7
// (the OpenCode adapter, this package's own sibling in
// internal/adapters/outbound/opencode) -- this work is what extends
// events.schema.json for real (SubTaskStart, SubTaskFinish, both added to
// the top-level `oneOf`) and updates the schema's own top-level description
// to correctly name all 6. This package's own ack protocol needed ZERO code
// changes to pick this up: SendCritical takes `msg any` and does not know or
// care which concrete type it is handed (see SendCritical's own doc
// comment) -- it already worked against ExecutionComplete, SandboxErrorEvent
// (named to avoid shadowing the builtin `error`), SnapshotReady,
// PushComplete, and PushError, and now equally against SubTaskFinish, the
// 6th real critical type, with no changes here at all.
//
// # Outbound ack-protocol buffer: the eviction policy is the single most
// important design decision in this package
//
// §6.1 states the guarantee tersely: "sender buffers (1000 events, evict
// oldest non-critical) and re-sends on reconnect until acked". Read
// literally, "evict oldest non-critical" is not merely a tie-breaking rule
// among equally-evictable candidates -- it is an explicit exemption: a
// critical (unacked) entry is NEVER evicted, full stop, regardless of how
// full the buffer gets. The 1000 cap (outboundBufferCap) is therefore a
// SOFT target enforced only by evicting non-critical entries; a non-critical
// (best-effort) entry is always safe to drop because the receiver's own
// upsert-by-messageId dedupe (§6.1) makes redelivery-or-not equally correct
// for it. A critical entry is exactly the opposite: it exists BECAUSE
// at-least-once delivery must be guaranteed (execution_complete/error/
// snapshot_ready/push_complete/push_error each close out state the control
// plane and UI treat as authoritative -- a turn's terminal outcome, a
// snapshot becoming restorable, a push's result). Evicting one to make room
// under memory pressure would silently convert a guaranteed-delivery
// protocol into a best-effort one at precisely the moment it matters most --
// exactly the failure class this whole ack protocol exists to prevent. So:
//
//   - If the buffer is under cap, or every existing entry the new one would
//     have to make room for is itself critical, nothing is evicted -- the
//     buffer is allowed to grow past 1000 in that (extreme, unlikely)
//     scenario rather than silently drop a guaranteed event.
//   - Otherwise, exactly the OLDEST non-critical entry is evicted to make
//     room -- oldest, not merely "some non-critical one", so eviction order
//     is deterministic and fair (a busy sender doesn't starve out
//     best-effort events sent early in a session in favor of ones sent
//     later).
//
// This decision lives in its own small, pure, deterministic function
// (evictionDecision, buffer.go) precisely so it can be exhaustively
// table-tested without any WS/network machinery in the loop.
//
// # The replay after a (re)connect: order, bound, and what can be evicted
//
// A best-effort entry leaves the buffer only by eviction, so a connection
// normally comes up with the buffer at its cap, and flushBuffer (run.go)
// replays all of it, in send order, before any live send is written and
// before commands are read. A send made while that replay runs is
// buffered, then HELD: it returns only once the replay has ended, and the
// replay itself writes its entry after every older one (enqueue,
// bridge.go). Three properties follow, each pinned by a test in
// bridge_test.go:
//
//   - Order. Nothing is written live ahead of an older buffered entry. A
//     live `token` frame overtaking an older, never-delivered frame of the
//     same text part would make the control plane store the older text
//     last, and the newest stored frame is what every reader shows.
//   - Bound. Each sending goroutine adds at most one entry per replay, so
//     a replay is one pass over what was buffered when the connection came
//     up plus at most one entry per sender. Commands and acks, read once
//     the replay ends, wait for that and no longer, however fast a sender
//     is. The hold is also the backpressure a live conn.Write gives when
//     the socket is full.
//   - Nothing unwritten is evicted by the replay. An add evicts only when
//     the buffer holds at least outboundBufferCap entries, and then only
//     the oldest best-effort one. The replay writes from its own copy of
//     the buffer, so an eviction cannot take an entry of the pass in
//     progress, and the one entry a held sender added is newer than all of
//     them, so it is evicted only if every older entry is critical.
//
// What eviction can drop, therefore: while disconnected, best-effort
// entries more than outboundBufferCap events old, never delivered -- the
// soft cap §6.1 states; while connected, entries every one of which was
// already written on this connection, by the replay or live; never a
// critical entry, which leaves the buffer only when its ack arrives.
//
// # Dispatch-by-type-field is this package's own necessary pattern
//
// contracts/gen/go/sandboxws has no generated discriminated-union wrapper
// type for commands.schema.json's own `oneOf` (go-jsonschema does not
// synthesize one) -- each command shape is its own independent Go struct
// with no shared marker beyond a `Type string` field. This package peeks
// that field via a tiny envelope struct to decide which concrete
// contracts/gen/go/sandboxws.{Prompt, Stop, Push, Snapshot, Shutdown, Ack,
// GitSyncComplete} to unmarshal the same raw bytes into. An unrecognized
// type value is logged and skipped, never fatal -- forward-compatible with
// a command type a future Step adds that this package doesn't know about
// yet.
//
// # Per-message gen-fencing
//
// commands.schema.json's own description states the invariant this package
// must enforce: "per-message gen-fencing (§3.2 'stale-gen inputs are
// rejected') does not rely solely on the connection-level X-Sandbox-Gen
// header". Every inbound command except ack (handled internally, never
// dispatched to CommandHandler at all) has its own Gen field checked
// against this Bridge's own session Gen before being acted on; a mismatch
// is logged and the command is silently ignored -- this applies to
// `shutdown` exactly as it does to the 5 business commands, since a stale
// connection's delayed shutdown must never tear down a bridge that has
// already moved on to a newer generation.
//
// # Prompt receipts and the dedup journal
//
// The control plane commits a turn Processing before it writes the turn's
// prompt to this connection, in one frame nothing acknowledges, so a prompt
// can be lost -- the control plane dies after its commit, or the frame goes
// with its socket -- leaving the turn to wait out its deadline on a sandbox
// that never had it. Technical plan §3.3's prompt receipts close that: once
// EnablePromptReceipts has opened this gen's prompt journal (promptjournal.go,
// in boot.Config.AgentStateDir), every ready advertises
// capabilities.promptReceipt, and the control plane, which records that per
// gen, asks such a gen for a receipt on each prompt (receiptRequested). On
// the gen's next reconnect it looks the receipt up, and when none is stored
// sends the same prompt again, with the same messageId. So this package, for
// every prompt, after checking its gen:
//
//   - journals its messageId, appended and fsynced, the first time it
//     arrives -- before anything else;
//   - answers it, when it asked, with a best-effort prompt_received event
//     whose messageId is 'prompt_received:{messageId}' -- a duplicate too,
//     with duplicate set, so a lost receipt is answered again by the next
//     copy;
//   - hands it to CommandHandler.HandlePrompt only the first time.
//
// A receipt is never sent for a prompt whose journal append failed: it
// would promise what a restarted process could not keep, so such a prompt
// neither runs nor is receipted. The failure breaks the journal for good:
// the agent stops advertising the capability, reports the prompt in a
// non-fatal, critical error event, and ends the connection, so the next
// ready tells the control plane, which records the gen as incapable and
// re-sends nothing more -- that turn ends at its deadline. A prompt that
// asked for no receipt still runs then, deduped in memory. The file holds
// complete lines only: a failed append is cut back off, and a tail a crash
// cut short is cut off at open, so no fragment glues onto a later line or,
// closed by one, reads as a prompt that ran.
// The receipt is not one of the critical types: each re-sent copy draws a
// fresh one, and a buffered one replays on reconnect like any best-effort
// event.
//
// The journal is keyed by session and gen (prompts-<sessionId>-<gen>.log)
// and outlives this process, not the gen: a container restart within the
// gen (a Kubernetes pod's default restartPolicy) keeps it, and a respawn,
// restore or resume is a new gen, which never reads an older gen's file --
// one that arrives with a snapshot or repo image is removed at open. Its
// directory must be this process's own and writable by no one else, as the
// credential cache's is (internal/sandboxagent/credentials): the agent
// runtime is a different, untrusted uid (§30.5). A journal that cannot be
// opened leaves the Bridge exactly as it was before receipts -- no
// capability, no receipt, no dedup -- and the control plane then never
// re-sends to this gen.
//
// # The largest message a connection reads
//
// Every connection reads messages of up to platform.MaxPromptFrameBytes,
// the largest prompt frame the control plane may send. The WebSocket
// library's default, 32 KiB, closed the connection on any longer prompt --
// a review's, with its pull request's diff inlined -- and the prompt was
// lost; with prompt receipts, every reconnect re-sent it, to be lost
// again.
//
// # Honest gaps this package documents rather than papers over
//
// New's own sandboxID parameter is the value internal/sandboxagent/boot.
// Config.SandboxID resolves (see its own doc comment for the full
// priority order): the real, control-plane-assigned sandbox identity
// (sandboxes.id) whenever a live SessionConfig is present -- populated via
// SessionConfig.SandboxId, the same NARVI_SESSION_CONFIG channel every
// other session-scoped value already travels on -- falling back to a
// NARVI_SANDBOX_ID env var override or "" only in the dev/CI-with-no-live-
// session case. A real production spawn therefore no longer sends an
// always-empty X-Sandbox-ID header, closing what used to be a real,
// production-blocking gap: internal/adapters/inbound/wshub/sandbox.go's
// own handshake step 4 rejects an empty X-Sandbox-ID with a fatal 401, so
// a sandbox booted on the OLD always-"" default could never complete its
// own handshake at all.
//
// What remains an honest, NOT-this-batch's-job gap: internal/adapters/
// inbound/wshub/sandbox.go's own step 4 verifies X-Sandbox-ID is merely
// PRESENT (non-empty) -- it never compares the header's VALUE against
// sandboxes.id server-side. The value sandbox-agent now sends is finally
// the real one, not always empty; deeper server-side verification of that
// value is a separate, deliberately unaddressed hardening step.
//
// Heartbeat.ConversationId was always nil before an OpenCode
// adapter existed; SetConversationID (bridge.go) lets
// cmd/sandbox-agent's own commandHandler record a real OpenCode
// conversation id once StartTurn returns one, which the heartbeat loop
// (run.go) now reports on every subsequent tick.
package wsbridge
