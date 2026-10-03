package wsbridge

import "sync"

// outboundBufferCap is §6.1's own explicit figure ("sender buffers (1000
// events...)") -- a plain int, not a duration, matching
// internal/domain/sandbox's own precedent for CircuitBreakerWindow's
// companion "3" threshold (CircuitBreakerThreshold) living as a plain named
// constant rather than in platform.Timeouts. See doc.go for why this is a
// SOFT cap, enforced only by evicting non-critical entries.
const outboundBufferCap = 1000

// outboundEntry is one buffered, already-marshaled outbound message.
// payload is the bytes written to the wire, stored once at Send time so a
// resend never re-marshals (and can never re-marshal to something subtly
// different) -- except on a connection that reads less than payload, which
// is written Fit's cut of it instead: deterministic, so every such write
// of one entry to one bound is byte-identical too.
type outboundEntry struct {
	// ackID is "" for a non-critical (best-effort) entry -- only critical
	// entries are ever looked up/removed by ack().
	ackID    string
	critical bool
	payload  []byte
	// cutPath is the path, from the frame's top level, of the one string
	// SendBestEffort cut when the event was over platform.MaxEventFrameBytes
	// (the enqueue cap, doc.go's "What a connection writes"); nil for an
	// entry buffered whole. A connection that reads less is written a cut
	// of that same string (Fit), which keeps the total the first cut
	// recorded.
	cutPath []string
	// notice marks a warning this package raised itself about another
	// entry it could not write (Bridge.warnOnce). A notice is never itself
	// warned about: on a connection too small even for it, it is skipped.
	notice bool
	// seq is the entry's position in send order, assigned by add and never
	// reused: flushBuffer uses it to pick up exactly the entries added
	// while it was already writing, and eviction/ack never reorder it.
	seq uint64
}

// evictionDecision is the pure, deterministic policy backing the outbound
// buffer's soft cap (see doc.go for the full reasoning). Given the buffer's
// CURRENT contents (before newEntry is appended) and capacity, it decides
// which existing entry -- if any -- must be evicted to make room for
// newEntry.
//
// newEntry's own criticality does NOT affect this decision (a new critical
// entry is not privileged over a new non-critical one when it comes to
// whether SOME eviction happens) -- it is accepted as a parameter purely so
// the function's contract mirrors how it is actually invoked (buffer.add
// always has "the current contents plus a new entry" in hand); the current
// policy simply never needs to inspect it.
//
//   - If len(current) < capacity: no eviction needed. (evict=false)
//   - Else, scan current for the OLDEST entry with critical=false and evict
//     it. (evict=true, index=that entry's index)
//   - Else (every current entry is critical): nothing is evicted; the
//     buffer is allowed to grow past capacity rather than drop a
//     guaranteed-delivery event. (evict=false)
func evictionDecision(current []outboundEntry, newEntry outboundEntry, capacity int) (index int, evict bool) {
	_ = newEntry // see doc comment: the decision does not depend on the new entry's own criticality.

	if len(current) < capacity {
		return 0, false
	}

	for i, e := range current {
		if !e.critical {
			return i, true
		}
	}

	return 0, false
}

// outboundBuffer is the ack-protocol's own outbound buffer: every critical
// or best-effort entry ever sent is recorded here so it can be replayed, in
// original order, on the next (re)connect -- until a critical entry is
// acked (ack removes it permanently) or a non-critical entry is evicted
// under buffer pressure (evictionDecision above).
type outboundBuffer struct {
	mu      sync.Mutex
	entries []outboundEntry
	nextSeq uint64
}

func newOutboundBuffer() *outboundBuffer {
	return &outboundBuffer{}
}

// add appends entry, stamped with the next send-order seq, evicting one
// existing entry first if evictionDecision says to. It returns entry as
// buffered, its seq set, and the seq of the entry it evicted, if any, so
// the caller can forget what it knew of that entry (Bridge.warned).
func (b *outboundBuffer) add(entry outboundEntry) (added outboundEntry, evictedSeq uint64, evicted bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if idx, evict := evictionDecision(b.entries, entry, outboundBufferCap); evict {
		evictedSeq, evicted = b.entries[idx].seq, true
		b.entries = append(b.entries[:idx], b.entries[idx+1:]...)
	}
	entry.seq = b.nextSeq
	b.nextSeq++
	b.entries = append(b.entries, entry)
	return entry, evictedSeq, evicted
}

// ack permanently removes the critical entry matching ackID, if present,
// and returns its seq. Acking an unknown or already-removed ackID (e.g. a
// duplicate ack after a reconnect) is a silent no-op -- never an error.
func (b *outboundBuffer) ack(ackID string) (seq uint64, removed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for i, e := range b.entries {
		if e.critical && e.ackID == ackID {
			b.entries = append(b.entries[:i], b.entries[i+1:]...)
			return e.seq, true
		}
	}
	return 0, false
}

// snapshot returns a stable copy of every currently-buffered entry, in
// original order. A copy (not the live slice) so the caller can range over
// it while add/ack continue to run concurrently against the real buffer.
func (b *outboundBuffer) snapshot() []outboundEntry {
	return b.snapshotFrom(0)
}

// snapshotFrom is snapshot restricted to the entries whose seq is at least
// fromSeq -- still in original order, since seq only ever grows and
// eviction/ack only remove entries. flushBuffer (run.go) calls it with the
// seq just past the last entry it wrote, to pick up what was added while
// it was writing.
func (b *outboundBuffer) snapshotFrom(fromSeq uint64) []outboundEntry {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]outboundEntry, 0, len(b.entries))
	for _, e := range b.entries {
		if e.seq >= fromSeq {
			out = append(out, e)
		}
	}
	return out
}
