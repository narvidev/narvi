package boot

import (
	"bytes"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sync/errgroup"
)

// This file exercises outputTail directly (package boot, not boot_test) --
// it is unexported and has no external constructor, so a test needing
// newOutputTail(), Write's internal timing, or the afterCR field itself
// must live in-package. It covers the three Batch B4 review findings that
// specifically call for direct outputTail-level coverage (findings 1-3):
// Write's own algorithmic cost, concurrent Write safety, and the afterCR
// CRLF-split carry-over.

// TestIndexLineBoundary_WriteNewlineOnlyLinesScalesLinearly proves finding
// 1: indexLineBoundary must resolve each of a Write's k line boundaries in
// work proportional to that ONE line, not to however much of the buffer is
// still left to scan for '\r' -- a single Write of L newline-only lines
// (no '\r' anywhere, so nothing ever short-circuits a full remaining-buffer
// scan for it) must cost ~O(n), not ~O(k·n).
//
// The review measured the quadratic form before the fix: L=20,000 took
// 38.6ms, L=40,000 took 135.5ms and L=80,000 took 528.5ms.
//
// # What it proves, without a clock
//
// It counts what Write does instead of timing it, on two axes.
//
// Searching. The test fills outputTail.indexByte with a search that counts
// the bytes it examines: every byte up to and including the first match,
// or all of b when there is none. At every size, one Write of newline-only
// lines must examine between one and two bytes per input byte. The '\n'
// search alone has to see every input byte, so the lower bound proves the
// searches go through the counted seam at all. The upper bound is two
// passes over each line: the '\n' search to its terminator, and the '\r'
// search, bounded to p[:nl], over the same line short of it. That makes 21
// bytes for each 11-byte line here. Re-scanning the whole remainder for
// '\r' at every line examines about n²/22 bytes, over 2·10⁹ at L=20,000
// against a bound of 440,000.
//
// Copying. Write must read each line out of p, never copy the remainder
// on each boundary -- the shape of the original bug, which neither search
// count sees. The bytes one Write allocates (runtime.MemStats.TotalAlloc)
// must therefore grow with its input: quadrupling the input from 20,000
// to 80,000 lines may at most multiply them by eight. That is linear (4x)
// with 2x headroom. A per-boundary copy of the remainder multiplies them
// by about fifteen. The bound compares two sizes rather than fixing bytes
// per input byte, because the race detector raises the absolute figure
// about a hundredfold: sync.Pool, which the ANSI strip's regexp uses,
// drops items at random under -race. TotalAlloc is process-wide, so this
// test is not parallel.
//
// It does not prove anything about CPU spent outside both axes -- a loop
// over the remainder that neither searches through the seam nor allocates
// would not show here.
//
// The two earlier forms of this test timed Write. One asserted an absolute
// bound and one a ratio across input sizes. Both failed on busy machines
// with the implementation correct: 491ms against a 400ms bound, then
// 9.84x against an 8.0x ratio on CI.
func TestIndexLineBoundary_WriteNewlineOnlyLinesScalesLinearly(t *testing.T) {
	// Not parallel: the allocation bound reads runtime.MemStats.TotalAlloc,
	// which counts every goroutine's allocations, so it needs the process
	// to itself. A sequential top-level test runs while every parallel one
	// is still paused.

	const (
		// Each input byte is examined at least once, by the '\n' search,
		// and at most twice, once by each search.
		minExaminedPerByte = 1
		maxExaminedPerByte = 2

		// The allocation step: 4x the input may cost at most 8x the bytes.
		allocBaseLines   = 20_000
		allocLargeLines  = 80_000
		maxAllocStepGrow = 8
	)

	tests := []struct {
		name  string
		lines int
	}{
		{name: "one line", lines: 1},
		{name: "1,000 lines", lines: 1_000},
		{name: "20,000 lines", lines: allocBaseLines},
		{name: "40,000 lines", lines: 40_000},
		{name: "80,000 lines", lines: allocLargeLines},
	}

	allocated := make(map[int]uint64, len(tests))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := []byte(strings.Repeat("0123456789\n", tc.lines))

			examined := 0
			tail := newOutputTail()
			tail.indexByte = func(b []byte, c byte) int {
				i := bytes.IndexByte(b, c)
				if i < 0 {
					examined += len(b)
				} else {
					examined += i + 1
				}
				return i
			}

			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, err := tail.Write(p)
			runtime.ReadMemStats(&after)
			if err != nil {
				t.Fatalf("Write() error = %v, want nil", err)
			}
			allocated[tc.lines] = after.TotalAlloc - before.TotalAlloc

			if low := minExaminedPerByte * len(p); examined < low {
				t.Errorf("Write(%d newline-only lines, %d bytes) examined %d bytes through outputTail.indexByte, want at least %d "+
					"-- the '\\n' search must see every byte, so Write is searching some other way the count cannot see",
					tc.lines, len(p), examined, low)
			}
			if high := maxExaminedPerByte * len(p); examined > high {
				t.Errorf("Write(%d newline-only lines, %d bytes) examined %d bytes, want at most %d "+
					"(%d per input byte) -- O(k·n) regression?",
					tc.lines, len(p), examined, high, maxExaminedPerByte)
			}
			if got, want := len(tail.Lines()), min(tc.lines, hookOutputTailMaxLines); got != want {
				t.Errorf("Lines() kept %d lines, want %d -- the counts above must be over a Write that did its work",
					got, want)
			}
		})
	}

	base, large := allocated[allocBaseLines], allocated[allocLargeLines]
	if base == 0 {
		t.Fatalf("Write(%d lines) allocated nothing; the allocation step has no base to compare against", allocBaseLines)
	}
	if grew := float64(large) / float64(base); grew > maxAllocStepGrow {
		t.Errorf("Write allocated %d bytes for %d lines and %d bytes for %d lines (%.1fx for 4x the input), want at most %dx "+
			"-- is Write copying the remainder on each boundary?",
			base, allocBaseLines, large, allocLargeLines, grew, maxAllocStepGrow)
	}
}

// TestOutputTail_ZeroValueWrites proves the zero outputTail is ready to
// use, as it was before outputTail.indexByte existed: a nil indexByte
// means bytes.IndexByte. Write runs in os/exec's copy goroutine, so a nil
// search there would panic and take sandbox-agent down with it.
func TestOutputTail_ZeroValueWrites(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{name: "a newline-terminated line", input: "hello\n", want: []string{"hello"}},
		{name: "a carriage-return redraw", input: "10%\r20%\n", want: []string{"10%", "20%"}},
		{name: "a CRLF line", input: "windows\r\n", want: []string{"windows"}},
		{name: "a partial line", input: "no boundary yet", want: []string{"no boundary yet"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var tail outputTail
			n, err := tail.Write([]byte(tc.input))
			if err != nil || n != len(tc.input) {
				t.Fatalf("Write(%q) = (%d, %v), want (%d, nil)", tc.input, n, err, len(tc.input))
			}
			got := tail.Lines()
			if len(got) != len(tc.want) {
				t.Fatalf("Lines() = %q, want %q", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("Lines()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestOutputTail_ConcurrentWriteRaceSafe proves finding 2: outputTail's own
// doc comment claims it is "Safe for concurrent Write calls ... guarded by
// a single mutex" (matching runHook's real usage -- one shared *outputTail
// passed as BOTH supervisor.Spec.Stdout and Stderr, read by two independent
// OS-pipe-draining goroutines), yet nothing previously called Write
// concurrently from two goroutines with substantial, overlapping,
// non-trivial payloads. Run with `go test -race`: any future change that
// widens Write's critical section incorrectly, or introduces a second lock,
// must show up here as a data race, not slip through unexercised.
func TestOutputTail_ConcurrentWriteRaceSafe(t *testing.T) {
	tail := newOutputTail()

	const goroutines = 8
	const writesPerGoroutine = 200

	// errgroup.Group, not a bare `go` statement: §11's no-naked-goroutine
	// rule is lint-enforced (tools/lint/narvichecks) and applies to tests
	// too. Nothing here returns a real error -- the assertions are the race
	// detector and the invariants checked after Wait -- so every closure
	// returns nil and Wait's own error is ignored deliberately.
	var group errgroup.Group
	for g := 0; g < goroutines; g++ {
		group.Go(func() error {
			id := g
			for i := 0; i < writesPerGoroutine; i++ {
				// A substantial, varied, multi-line payload per Write (not a
				// single trivial byte) -- including a lone trailing '\r' on
				// some writes so afterCR is ALSO exercised under concurrency,
				// and a CRLF pair on others -- so this stresses the same
				// mutex-guarded state (t.lines, t.cur, t.afterCR) real
				// concurrent stdout/stderr draining would.
				// outputTail.Write never returns an error (documented on the
				// method itself), so every return value here is deliberately
				// discarded rather than asserted on -- errcheck requires the
				// discard be explicit.
				switch i % 4 {
				case 0:
					_, _ = fmt.Fprintf(tail, "goroutine-%d line-%d part-a\npart-b\n", id, i)
				case 1:
					_, _ = fmt.Fprintf(tail, "goroutine-%d progress-%d\r", id, i)
				case 2:
					_, _ = fmt.Fprintf(tail, "goroutine-%d crlf-%d\r\n", id, i)
				default:
					_, _ = tail.Write([]byte(strings.Repeat(strconv.Itoa(id), 37) + "\n"))
				}
			}
			return nil
		})
	}
	_ = group.Wait()

	// No assertion on exact content -- interleaving across goroutines is by
	// definition non-deterministic. The real assertions are (a) -race finds
	// nothing, and (b) the bound still holds under concurrent writers.
	lines := tail.Lines()
	if len(lines) > hookOutputTailMaxLines {
		t.Errorf("Lines() returned %d lines after concurrent writers, want at most %d (bound violated under concurrency)",
			len(lines), hookOutputTailMaxLines)
	}
}

// TestOutputTail_AfterCRCarriesOverSplitCRLF proves finding 3 directly at
// the outputTail level (the boot_test-level TestRunHooks_CRLFNotDoubled
// only ever exercises a CRLF pair delivered in a SINGLE Write call, so it
// cannot and does not exercise the afterCR carry-over branch at all): a
// genuine "\r\n" line ending split across two separate Write calls -- the
// '\r' arriving as the very last byte of one Write, the matching '\n'
// arriving as the very first byte of the next -- must still be captured as
// ONE line boundary, not doubled into an extra blank line.
func TestOutputTail_AfterCRCarriesOverSplitCRLF(t *testing.T) {
	tail := newOutputTail()

	// First Write ends in a lone, unpaired '\r' -- flushed immediately as a
	// line boundary, with afterCR left set so the NEXT Write's leading '\n'
	// (if any) is recognized as the second half of the same CRLF pair.
	if _, err := tail.Write([]byte("first-line\r")); err != nil {
		t.Fatalf("Write() error = %v, want nil", err)
	}
	if !tail.afterCR {
		t.Fatal("afterCR = false after a Write ending in a lone '\\r', want true")
	}

	// Second Write's leading '\n' is the split CRLF's second half: it must
	// be swallowed, not read as a further (empty) line boundary.
	if _, err := tail.Write([]byte("\nsecond-line\n")); err != nil {
		t.Fatalf("Write() error = %v, want nil", err)
	}
	if tail.afterCR {
		t.Error("afterCR = true after the following Write consumed it, want false")
	}

	want := []string{"first-line", "second-line"}
	got := tail.Lines()
	if len(got) != len(want) {
		t.Fatalf("Lines() = %v (%d lines), want exactly %v (%d lines) -- a split CRLF must not double into a blank line",
			got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Lines()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
