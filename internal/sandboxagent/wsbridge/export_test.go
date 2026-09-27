package wsbridge

// SetFlushWriteHookForTest installs hook to run after flushBuffer writes
// each buffered entry (see Bridge.flushWriteHook). Call it before Run; the
// hook runs on Run's own goroutine, so it must not itself make a send that
// would be held behind the replay it is running inside.
func SetFlushWriteHookForTest(b *Bridge, hook func()) {
	b.flushWriteHook = hook
}

// SetEnqueueHeldHookForTest installs hook to run whenever a send has
// buffered its entry and is about to wait for a running replay (see
// Bridge.enqueueHeldHook) -- how a test learns, without sleeping, that a
// send it started is held. Call it before any send; the hook runs on the
// sending goroutine.
func SetEnqueueHeldHookForTest(b *Bridge, hook func()) {
	b.enqueueHeldHook = hook
}

// SetReplayCaughtUpHookForTest installs hook to run inside flushBuffer's
// final critical section, with connMu held, right after its last snapshot
// came back empty and before it publishes the connection (see
// Bridge.replayCaughtUpHook). The hook must not send synchronously: a send
// needs connMu. Call it before Run.
func SetReplayCaughtUpHookForTest(b *Bridge, hook func()) {
	b.replayCaughtUpHook = hook
}

// OutboundBufferCapForTest is outboundBufferCap, for a test that needs a
// connection to come up with the buffer exactly at its cap.
const OutboundBufferCapForTest = outboundBufferCap
