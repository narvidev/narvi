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
// It is the read limit of a peer that states none. A sandbox agent built
// before MaxPromptFrameBytes advertises no capability in its ready, so the
// control plane holds every prompt to its gen to this many bytes
// (technical plan §3.3, §6.1). TestDefaultFrameReadLimitBytes_IsTheLibraryDefault
// reads one byte past it on a fresh connection, so a library upgrade that
// moves the default fails there.
const DefaultFrameReadLimitBytes = 32768
