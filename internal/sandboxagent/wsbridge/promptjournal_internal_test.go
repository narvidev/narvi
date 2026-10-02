package wsbridge

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// faultyFile is a journal file whose next Write or Sync fails: a Write
// that fails writes the first cut bytes first, as a write cut short by a
// full disk or a file-size limit does.
type faultyFile struct {
	*os.File
	failWrite bool
	cut       int
	failSync  bool
}

func (f *faultyFile) Write(p []byte) (int, error) {
	if !f.failWrite {
		return f.File.Write(p)
	}
	n, err := f.File.Write(p[:min(f.cut, len(p))])
	if err != nil {
		return n, err
	}
	return n, syscall.EFBIG
}

func (f *faultyFile) Sync() error {
	if f.failSync {
		return syscall.EIO
	}
	return f.File.Sync()
}

func journalBytes(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, promptJournalFileName("sess", 3)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func openTestJournal(t *testing.T, dir string) *promptJournal {
	t.Helper()
	j, err := openPromptJournal(dir, "sess", 3)
	if err != nil {
		t.Fatalf("openPromptJournal: %v", err)
	}
	t.Cleanup(j.close)
	return j
}

// TestPromptJournal_FailedAppendIsCutBackAndBreaksTheJournal: an append
// that fails -- cut short mid-line, or written whole but not fsynced --
// leaves the file as it was before it, so nothing it wrote glues onto a
// later line, and breaks the journal, so no later append lands behind it.
// A later process of the same gen then reads exactly what was recorded,
// and records again.
func TestPromptJournal_FailedAppendIsCutBackAndBreaksTheJournal(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		fault faultyFile
	}{
		{name: "a write cut short", fault: faultyFile{failWrite: true, cut: 3}},
		{name: "a write whose fsync fails", fault: faultyFile{failSync: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			j := openTestJournal(t, dir)
			if err := j.record("first"); err != nil {
				t.Fatalf("record(first): %v", err)
			}

			fault := tc.fault
			fault.File = j.file.(*os.File)
			j.file = &fault
			if err := j.record("lost-prompt"); err == nil {
				t.Fatal("record(lost-prompt) = nil, want the append's failure")
			}
			if got, want := journalBytes(t, dir), "\"first\"\n"; got != want {
				t.Fatalf("journal after a failed append = %q, want %q: cut back to its complete lines", got, want)
			}
			if !j.isBroken() || j.has("lost-prompt") {
				t.Fatalf("broken = %v, has(lost-prompt) = %v; want broken and not recorded", j.isBroken(), j.has("lost-prompt"))
			}
			fault.failWrite, fault.failSync = false, false
			if err := j.record("after"); !errors.Is(err, errPromptJournalBroken) {
				t.Fatalf("record after a failure = %v, want errPromptJournalBroken", err)
			}
			if got, want := journalBytes(t, dir), "\"first\"\n"; got != want {
				t.Fatalf("journal after a refused append = %q, want %q", got, want)
			}

			// A later process of the same gen.
			again := openTestJournal(t, dir)
			if !again.has("first") || again.has("lost-prompt") || again.has("after") {
				t.Fatalf("reopened: has(first) = %v, has(lost-prompt) = %v, has(after) = %v; want true, false, false",
					again.has("first"), again.has("lost-prompt"), again.has("after"))
			}
			if err := again.record("ran-and-receipted"); err != nil {
				t.Fatalf("record on the reopened journal: %v", err)
			}
			if third := openTestJournal(t, dir); !third.has("ran-and-receipted") {
				t.Fatal("a prompt recorded after the failure is not read back: it glued onto a fragment")
			}
		})
	}
}

// TestPromptJournal_TornTailIsCutAtOpen: a last line without its newline
// -- a complete id or a fragment -- is not recorded, and is cut off at
// open, so a later process does not read it as recorded either.
func TestPromptJournal_TornTailIsCutAtOpen(t *testing.T) {
	t.Parallel()

	for _, tail := range []string{`"p2"`, `"p2`} {
		t.Run(tail, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, promptJournalFileName("sess", 3))
			if err := os.WriteFile(path, []byte("\"p1\"\n"+tail), 0o600); err != nil {
				t.Fatal(err)
			}
			first := openTestJournal(t, dir)
			if !first.has("p1") || first.has("p2") {
				t.Fatalf("has(p1) = %v, has(p2) = %v; want true, false", first.has("p1"), first.has("p2"))
			}
			if got, want := journalBytes(t, dir), "\"p1\"\n"; got != want {
				t.Fatalf("journal after open = %q, want %q: the torn tail cut off", got, want)
			}
			if second := openTestJournal(t, dir); second.has("p2") {
				t.Fatal("a later process reads the torn tail as recorded")
			}
		})
	}
}
