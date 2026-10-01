package platform

// MaxPromptFrameBytes is the largest prompt command, encoded, that the
// control plane may write to a sandbox's WebSocket, and the read limit
// every sandbox-agent connection sets (technical plan §6.1). Both sides
// read this one constant: the agent calls Conn.SetReadLimit with it on
// every connection it dials (internal/sandboxagent/wsbridge), and the
// session actor measures every prompt frame before it is written -- a
// first dispatch, a re-enqueue and a receipt re-send alike
// (internal/app/sessionactor) -- and never sends one longer.
//
// Before this bound the agent kept the WebSocket library's default read
// limit, 32 KiB, so any prompt frame longer than that closed the agent's
// connection (StatusMessageTooBig) and was lost, silently: a review prompt
// inlines the pull request's whole diff, which made that common.
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
