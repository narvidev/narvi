package wsbridge

// SetFlushWriteHookForTest installs hook to run after flushBuffer writes
// each buffered entry (see Bridge.flushWriteHook) -- the one seam
// TestRun_LiveSendDuringReplayIsWrittenAfterEveryBufferedEntry needs to
// send a live event at a deterministic point mid-replay. Call it before
// Run; the hook runs on Run's own goroutine.
func SetFlushWriteHookForTest(b *Bridge, hook func()) {
	b.flushWriteHook = hook
}
