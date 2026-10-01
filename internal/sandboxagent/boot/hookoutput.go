package boot

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// hookOutputTailMaxLines is "on the order of 120 lines" (§19.5(a)) -- the
// bound outputTail keeps for one hook run's own combined stdout+stderr.
const hookOutputTailMaxLines = 120

// hookOutputTailMaxPendingBytes bounds t.cur, the not-yet-line-terminated
// partial line outputTail.Write is still accumulating. 64 KiB is far more
// than any real single output line (even a verbose stack-trace one-liner
// or a wide table row) needs, while still being a small, fixed
// contribution to sandbox-agent's own memory in the pathological case of a
// hook emitting output with no line boundary at all (e.g. a `\r`-driven
// npm/yarn/cargo/docker progress renderer that stalls, or a huge
// newline-free blob) -- see appendChunkLocked. Combined with
// hookOutputTailMaxLines, the worst-case footprint of one outputTail is
// bounded at roughly (hookOutputTailMaxLines+1) * hookOutputTailMaxPendingBytes
// (~7.75 MiB at these values), not the unbounded growth the pre-existing
// line-count-only cap actually allowed.
const hookOutputTailMaxPendingBytes = 64 * 1024

// hookOutputTruncatedLineFormat is the synthetic line appendChunkLocked
// injects into the tail whenever a single pending line would otherwise
// exceed hookOutputTailMaxPendingBytes -- marked distinctly so a reader (or
// grep) can tell "outputTail cut this line short" apart from the hook's
// own genuine output.
const hookOutputTruncatedLineFormat = "[[hookoutput: line truncated after %d bytes without a line break]]"

// ansiEscapePattern matches a CSI-style ANSI escape sequence (color codes,
// cursor movement, ...) -- e.g. "\x1b[31m", "\x1b[2K" -- so a hook's own
// colorized output (common for npm/yarn/cargo-style tooling) renders as
// plain, readable text in a boot log line rather than raw escape bytes.
var ansiEscapePattern = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")

// stripANSI removes every ANSI escape sequence ansiEscapePattern matches
// from s.
func stripANSI(s string) string {
	return ansiEscapePattern.ReplaceAllString(s, "")
}

// outputTail is a caller-held, bounded, line-oriented, ANSI-stripped
// ring buffer capturing the most recent hookOutputTailMaxLines lines of
// one hook run's own combined stdout+stderr (§19.5(a)). It implements
// io.Writer so it can be passed directly as BOTH supervisor.Spec.Stdout
// and Spec.Stderr (a single combined tail, mirroring how a terminal would
// interleave them) -- runHook constructs one PER hook run and holds it
// itself, independent of the bounded proc.Wait call: a timeout-triggered
// proc.Stop can therefore never lose a buffer that was never inside the
// cancelled operation to begin with (§19.5(a)'s own load-bearing
// requirement).
//
// Both the completed-line ring (t.lines, bounded by hookOutputTailMaxLines)
// and the pending partial line (t.cur, bounded by
// hookOutputTailMaxPendingBytes) are genuinely bounded -- a hook whose
// output contains no newline at all (a `\r`-driven progress renderer, or
// simply a huge blob) cannot grow this type's memory footprint without
// limit, which a line-count-only cap would allow.
//
// Safe for concurrent Write calls (a spawned process's stdout/stderr are
// two independent OS pipes, read concurrently by the exec package) --
// guarded by a single mutex, matching supervisor.Process's own
// "spawned-process output is inherently concurrent" precedent.
type outputTail struct {
	mu    sync.Mutex
	lines []string
	cur   strings.Builder

	// afterCR is true when the immediately preceding Write call ended in a
	// lone, unpaired '\r' that was already flushed as a line boundary (see
	// Write) -- so if the NEXT Write's first byte is '\n', it is the second
	// half of a genuine CRLF split across two Write calls and must be
	// swallowed rather than read as a further, empty line boundary.
	afterCR bool

	// indexByte is the byte search Write scans p with: bytes.IndexByte,
	// always, in production. It is a field so a test can count the bytes
	// one Write examines, which proves Write linear by an operation count
	// rather than by timing it (hookoutput_internal_test.go).
	indexByte func(b []byte, c byte) int
}

// newOutputTail returns a ready, empty outputTail.
func newOutputTail() *outputTail {
	return &outputTail{indexByte: bytes.IndexByte}
}

// indexLineBoundary reports the index and byte width of the first line
// boundary in p -- '\n' or '\r' (§19.5(a): carriage-return-driven progress
// output redraws a single line with no '\n' for the whole run, and must be
// captured as successive lines rather than one unbounded pending line). A
// genuine CRLF pair is reported as ONE boundary of width 2, never as two
// separate boundaries, so real Windows-style line endings are not doubled.
// Returns idx -1 if p contains no boundary at all.
//
// The '\r' search is bounded to p[:nl] (never the full remainder) whenever
// a '\n' was found: any '\r' that could possibly change the result must sit
// BEFORE nl (p[nl] is itself '\n', so it can never itself be that '\r'), so
// scanning past nl buys nothing but cost. This keeps a single call's own
// work proportional to the ONE line it resolves, not to however much of p
// is still left to process -- which is what keeps Write's own per-Write
// cost genuinely O(n) across all of a call's line boundaries, rather than
// re-scanning the whole shrinking remainder of p for '\r' on every one of a
// Write's k line boundaries (an O(k·n) blowup that only a newline-only
// buffer -- one with no '\r' anywhere to short-circuit the full scan -- was
// large enough to expose).
//
// indexByte is the byte search to scan with (outputTail.indexByte).
func indexLineBoundary(p []byte, indexByte func(b []byte, c byte) int) (idx, width int) {
	nl := indexByte(p, '\n')
	var cr int
	if nl >= 0 {
		cr = indexByte(p[:nl], '\r')
	} else {
		cr = indexByte(p, '\r')
	}
	if nl < 0 && cr < 0 {
		return -1, 0
	}
	if cr < 0 || (nl >= 0 && nl < cr) {
		return nl, 1
	}
	if cr+1 < len(p) && p[cr+1] == '\n' {
		return cr, 2
	}
	return cr, 1
}

// Write implements io.Writer: scans p directly for line boundaries ('\n',
// '\r', or a CRLF pair treated as one boundary -- see indexLineBoundary),
// appending each completed line (ANSI-stripped, oldest evicted once
// hookOutputTailMaxLines is exceeded) and carrying the trailing
// not-yet-terminated remainder forward in t.cur, itself bounded by
// hookOutputTailMaxPendingBytes (see appendChunkLocked). Never returns an
// error -- exactly like a bytes.Buffer's own Write, which this type
// otherwise generalizes on (a bounded, line-splitting variant instead of
// an unbounded byte buffer).
//
// Each completed line is read directly out of p (or the p-prefix plus
// whatever was already pending in t.cur), never by re-reading and
// rebuilding the whole remainder on every boundary found -- so a single
// Write containing k lines costs O(n) total, not O(k·n).
func (t *outputTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	n := len(p)

	if t.afterCR {
		t.afterCR = false
		if len(p) > 0 && p[0] == '\n' {
			p = p[1:]
		}
	}

	for len(p) > 0 {
		idx, width := indexLineBoundary(p, t.indexByte)
		if idx < 0 {
			break
		}
		if width == 1 && p[idx] == '\r' && idx+1 == len(p) {
			// A lone '\r' as the very last byte of this Write: flush it as
			// a line boundary now (a stalled progress-bar redraw should
			// not be held pending indefinitely), but remember it in case
			// the NEXT Write's first byte is the matching '\n' of a CRLF
			// pair split across two Write calls.
			t.appendChunkLocked(p[:idx])
			t.flushCurLocked()
			t.afterCR = true
			return n, nil
		}
		t.appendChunkLocked(p[:idx])
		t.flushCurLocked()
		p = p[idx+width:]
	}

	if len(p) > 0 {
		t.appendChunkLocked(p)
	}
	return n, nil
}

// appendChunkLocked appends b to t.cur, the pending partial line, flushing
// a synthetic hookOutputTruncatedLineFormat line (and resetting t.cur)
// each time the pending line would otherwise exceed
// hookOutputTailMaxPendingBytes -- so even a single Write of many
// newline-free megabytes is processed (and bounded) in one pass, rather
// than accumulating without limit. Caller must hold t.mu.
func (t *outputTail) appendChunkLocked(b []byte) {
	for len(b) > 0 {
		room := hookOutputTailMaxPendingBytes - t.cur.Len()
		if room <= 0 {
			t.appendLineLocked(fmt.Sprintf(hookOutputTruncatedLineFormat, hookOutputTailMaxPendingBytes))
			t.cur.Reset()
			room = hookOutputTailMaxPendingBytes
		}
		take := len(b)
		if take > room {
			take = room
		}
		t.cur.Write(b[:take])
		b = b[take:]
	}
}

// flushCurLocked appends the current pending line (t.cur) to t.lines as a
// completed line and resets t.cur. Caller must hold t.mu.
func (t *outputTail) flushCurLocked() {
	t.appendLineLocked(t.cur.String())
	t.cur.Reset()
}

// appendLineLocked appends one already-line-delimited line (ANSI stripped
// here, once, rather than per-Write-call) to t.lines, evicting the oldest
// entries once hookOutputTailMaxLines is exceeded. Caller must hold t.mu.
func (t *outputTail) appendLineLocked(line string) {
	t.lines = append(t.lines, stripANSI(line))
	if len(t.lines) > hookOutputTailMaxLines {
		t.lines = t.lines[len(t.lines)-hookOutputTailMaxLines:]
	}
}

// Lines returns a snapshot of the captured tail, oldest first, INCLUDING
// any partial (not-yet-line-terminated) trailing line a hook's own final,
// unflushed write left behind (e.g. a script whose last line of output has
// no trailing newline) -- bounded to the same hookOutputTailMaxLines cap.
func (t *outputTail) Lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()

	lines := make([]string, len(t.lines), len(t.lines)+1)
	copy(lines, t.lines)
	if t.cur.Len() > 0 {
		lines = append(lines, stripANSI(t.cur.String()))
		if len(lines) > hookOutputTailMaxLines {
			lines = lines[len(lines)-hookOutputTailMaxLines:]
		}
	}
	return lines
}
