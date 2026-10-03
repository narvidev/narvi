package platform

// MaxPromptFrameBytes is the largest prompt command, encoded, that the
// control plane may write to a sandbox's WebSocket, and the read limit a
// sandbox agent built from this release sets on its connection (technical
// plan §6.1). Both sides read this one constant: that agent calls
// Conn.SetReadLimit with it on every connection it dials, and states it in
// every ready as capabilities.maxFrameBytes (internal/sandboxagent/wsbridge),
// and the session actor measures every prompt frame before it is written --
// a first dispatch, a re-enqueue and a receipt re-send alike
// (internal/app/sessionactor) -- and never sends one longer.
//
// It is the most a prompt is held to, never the least. Each gen's bound is
// the read limit its agent states in its latest ready, clamped to this
// constant; otherwise this constant when that ready advertised
// promptReceipt, since every agent that advertises it was built with this
// read limit; otherwise DefaultFrameReadLimitBytes (promptFrameBound,
// internal/app/sessionactor; technical plan §3.3). A prompt over its gen's
// bound fails its turn at dispatch, with both sizes named.
//
// Before this bound the agent kept the WebSocket library's default read
// limit, 32 KiB, so any prompt frame longer than that closed the agent's
// connection (StatusMessageTooBig) and was lost, silently: a review prompt
// inlines the pull request's whole diff, which made that common. An agent
// in an older snapshot or repo image keeps that default, and states
// nothing: its gen is held to DefaultFrameReadLimitBytes, so a larger
// prompt to it is refused at dispatch and named instead of written to be
// lost.
//
// 32 MiB, chosen from what the control plane can build. The largest
// prompts are a review's: its text carries the diff, which the GitHub
// adapter caps at 4 MiB (maxPRDiffResponseBytes), plus the pull request's
// title and body (GitHub caps a body at 65,536 characters) and the review's
// own instructions and context blocks. JSON encoding grows text: '<', '>'
// and '&' become a 6-byte \u escape, as do the control characters
// without a 2-byte escape and every byte of invalid UTF-8, so a
// worst-case diff encodes to 6 x 4 MiB = 24 MiB. A prompt typed through
// REST is bounded by its 1 MiB request body, so 6 MiB encoded. 32 MiB
// covers the worst-case diff with 8 MiB for everything else in the frame;
// a real diff, mostly plain code, encodes to a little more than its own
// size. The agent only allocates what a frame actually carries, so the
// bound costs nothing on ordinary frames. A prompt over it fails its turn
// at dispatch, with the sizes named, rather than being sent to be lost.
const MaxPromptFrameBytes = 32 << 20

// DefaultFrameReadLimitBytes is the WebSocket library's own read limit: the
// largest message a connection reads when nothing calls SetReadLimit on
// it. github.com/coder/websocket v1.8.15 sets defaultReadLimit = 32768
// (read.go:107) and stores it as limit+1 (read.go:116), so a message of
// exactly 32768 bytes reads, and one of 32769 fails with ErrMessageTooBig
// and closes the connection with StatusMessageTooBig.
//
// It is the read limit of a peer that states none, in either direction. A
// sandbox agent built before MaxPromptFrameBytes advertises no capability
// in its ready, so the control plane holds every prompt to its gen to this
// many bytes (technical plan §3.3, §6.1). A control plane built before
// MaxEventFrameBytes sends no MaxFrameBytesHeader, so an agent holds every
// write on that connection to this many bytes; and no critical event is
// ever larger, whatever the control plane states (wsbridge's SendCritical
// refuses one), so every control plane reads every critical event.
// TestDefaultFrameReadLimitBytes_IsTheLibraryDefault reads one byte past it
// on a fresh connection, so a library upgrade that moves the default fails
// there.
const DefaultFrameReadLimitBytes = 32768

// MaxEventFrameBytes is the largest agent event, encoded, the control plane
// reads on a sandbox's WebSocket, and the largest entry a sandbox agent
// keeps in its outbound buffer (technical plan §6.1). The control plane
// sets it as the connection's read limit (Conn.SetReadLimit) and states it
// to the agent in the handshake's MaxFrameBytesHeader response header
// (internal/adapters/inbound/wshub); an agent built from this release holds
// every write on that connection to what the header states, or to
// DefaultFrameReadLimitBytes when it states nothing, cutting a `token`,
// `tool_call` or `tool_result` frame that is over it
// (internal/sandboxagent/wsbridge), and cuts a best-effort event over this
// constant to it before buffering it.
//
// Before it the control plane read at the library's default, 32 KiB, and
// nothing bounded what an agent wrote: an event over 32 KiB closed the
// connection, the agent replayed it on every reconnect, and the sandbox's
// socket looped, about 320 reconnects a second, with nothing behind the
// frame ever arriving.
//
// 1 MiB, chosen from what the agent emits and what the control plane holds
// at once.
//
// What the agent emits. A text part, or a tool's input (a file the agent
// writes among them), is bounded by the model's output cap: about 256 KiB
// of text at a 64K-token cap and about 4 bytes a token, which ordinary
// prose and code grow to about 330 KiB once JSON-escaped (1.1 to 1.3
// times). 1 MiB covers that three times over, and a 128K-token cap of
// ordinary text. JSON escaping can grow text six-fold -- '<', '>', '&' and
// the control characters without a 2-byte escape each become \u00XX --
// and such a frame is cut, visibly (a marker line in the text and a `cut`
// property; a cut plan is never offered for approval), not lost. A tool's
// output is bounded by the agent runtime's own truncation, if any. This
// repository records none for the pinned runtime (opencode-ai 1.17.15,
// .github/workflows/ci.yml; defaultOpenCodeRuntimeVersion,
// internal/platform/config.go): no test or note here measures how much of
// a tool's output that binary passes on, and its adapter passes the
// output it is given on whole (translateToolResult,
// internal/adapters/outbound/opencode/translate.go). So nothing on this
// side bounds a tool's output but this constant: a `tool_result` over a
// connection's bound is cut to it, and one over this constant is cut at
// enqueue. Either way it is stored: every `tool_call` and `tool_result` a
// live turn sends is stored under a key of its own and broadcast
// (technical plan §6.1, "Stored tool events"; internal/domain/eventkey), so
// a tool's input and output reach the log, every subscribed browser's hub
// queue, history pages and transcript pages at up to this many bytes each
// -- whole when they fit the connection that carried them, cut, `cut` set,
// when they did not.
//
// What the control plane holds. One read per sandbox connection, of up to
// this many bytes; up to hubConnBufferSize (64) distinct broadcast
// payloads behind a slow browser connection, shared by every connection
// of that session (wshub.Hub.Broadcast queues one shared slice per
// payload), so at most 64 MiB -- a bound a turn reaches in practice now that
// tool results are broadcast, a run of large tool outputs filling a slow
// connection's queue where text parts alone rarely did; past it, the hub
// drops that connection's broadcasts, and the page, which takes a
// broadcast only as its cue to page the history from its cursor
// (web/src/ws/sessionStream.ts), reads what it missed on the next one; and
// one history page, whose read is
// bounded by FetchHistoryMaxReplyBytes, or is one event when the first
// alone is larger -- the store measures each event before it reads it
// (postgres.EventStore.ListPageForSession), so a page never holds events
// it does not send. The agent's buffer holds at most 1000
// best-effort entries (wsbridge's outboundBufferCap), each at most this
// many bytes once cut at enqueue, so 1000 MiB in the worst case and a few
// MiB in practice -- a pinned-runtime text part is two frames, one of
// them empty -- where nothing bounded an entry before; a critical entry
// is at most DefaultFrameReadLimitBytes.
//
// Not sized as MaxPromptFrameBytes was, to cover the six-fold worst case:
// that would be 4 MiB, a 4 GiB worst case in the agent's buffer and 256
// MiB behind each slow browser connection. A cut is a visible, non-fatal
// outcome where an oversize prompt was a refused turn.
const MaxEventFrameBytes = 1 << 20

// FetchHistoryMaxReplyBytes bounds one page of a session's event log, as
// a client WS fetch_history reply and as GET /api/sessions/{id}/events
// return it (technical plan §6.2, §6.3), and bounds what the page reads,
// not only what it sends. A page holds the longest run of events after its
// cursor, oldest first, whose sizes in the page sum to at most this, and
// always at least one event; it sets nextCursor after the last event it
// sent. The store measures each event in Postgres as it walks the page,
// stops at the first that does not fit, and reads only the events that do
// (postgres.EventStore.ListPageForSession), so the control plane holds at
// most this much of a page, or one event when the first alone is larger,
// and paging through a log reads each event once. The subscribe reply
// reads its events the same way, under its own, smaller budget
// (maxInitialReplayBytes, internal/adapters/inbound/wshub).
//
// 2 MiB. It is no smaller than MaxEventFrameBytes, so one maximal event
// always fits with room for ordinary ones, and an ordinary 500-event page,
// at about 2 KiB an event or less, is at most about 1 MiB and never cut. A
// turn's tool calls and results are stored too, each up to
// MaxEventFrameBytes, so a page of a session whose tools read or write
// large files stops at this budget well before its count, and its reader
// goes on from nextCursor.
// Without it a page could read 500 x MaxEventFrameBytes, 500 MiB, where
// the 32 KiB read limit before it kept the worst page to 16 MiB.
const FetchHistoryMaxReplyBytes = 2 << 20

// MaxFrameBytesHeader is the response header of the sandbox WebSocket
// handshake in which the control plane states the largest message it
// reads on that connection: MaxEventFrameBytes, in decimal (technical plan
// §6.1). A sandbox agent bounds what it writes on the connection by it,
// and by DefaultFrameReadLimitBytes when it is absent or not a positive
// integer -- a control plane built before it, during a rolling deploy or
// after a rollback, or a proxy that drops the header. It bounds what the
// agent writes, never what it reads.
const MaxFrameBytesHeader = "X-Max-Frame-Bytes"
