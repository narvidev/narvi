package wsbridge

import "time"

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

// BreakPromptJournalForTest closes the file of b's prompt journal under it,
// so every later append fails -- how a test makes a journal append fail
// deterministically. Call it after EnablePromptReceipts.
func BreakPromptJournalForTest(b *Bridge) {
	b.journal.close()
}

// AssertStateDirIsOursForTest is assertStateDirIsOurs, so a test can check
// a directory against an owner other than itself.
func AssertStateDirIsOursForTest(dir string, uid int) error {
	return assertStateDirIsOurs(dir, uid)
}

// PromptJournalFileNameForTest is promptJournalFileName.
func PromptJournalFileNameForTest(sessionID string, gen int) string {
	return promptJournalFileName(sessionID, gen)
}

// SetClockForTest replaces the clock every ready and heartbeat is written
// at (see Bridge.now) -- how a test moves time between two heartbeats
// without sleeping. Call it before Run; now is read on Run's goroutines.
func SetClockForTest(b *Bridge, now func() time.Time) {
	b.now = now
}
