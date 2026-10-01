package wsbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
)

// promptJournalPrefix and promptJournalSuffix frame a journal file's name:
// prompts-<sessionId>-<gen>.log, one per session and gen.
const (
	promptJournalPrefix = "prompts-"
	promptJournalSuffix = ".log"
)

// promptJournal is the agent's half of technical plan §3.3's prompt
// receipts (doc.go, "Prompt receipts and the dedup journal"): the set of
// prompt messageIds this session's gen has received, held in memory and
// appended, one JSON string per line, to a file of its own, fsynced after
// every append, so a copy of a prompt re-sent after a reconnect is never
// run twice -- across a restart of this process too.
type promptJournal struct {
	mu   sync.Mutex
	file *os.File
	seen map[string]struct{}
}

// promptJournalFileName names the journal of sessionID's gen.
func promptJournalFileName(sessionID string, gen int) string {
	return promptJournalPrefix + sessionID + "-" + strconv.Itoa(gen) + promptJournalSuffix
}

// openPromptJournal opens -- creating it if absent -- the journal of
// sessionID's gen inside dir, and loads every messageId it records.
//
// dir must be this process's own and writable by no one else, checked as
// internal/sandboxagent/credentials checks its cache directory, for the
// same reason: its default lives under /tmp, where the agent runtime --
// prompt-injectable, a different uid (§30.5) -- can create a directory
// first, then plant a symlink at the journal's name, or edit the journal
// into running a prompt twice or never. Such a directory is a hard
// failure, never repaired.
//
// A journal file of any other session or gen in dir is never read, and is
// removed: one can only have arrived with a snapshot or a repo image, and a
// prompt of another gen says nothing about this one's.
//
// A last line a crash cut short -- an append the process died in before
// its fsync returned -- is read as not recorded, which is safe: a receipt
// is only ever sent after the append's fsync has returned. It is closed
// with a newline here, so the next append starts a line of its own.
func openPromptJournal(dir, sessionID string, gen int) (*promptJournal, error) {
	if sessionID == "" || strings.ContainsAny(sessionID, `/\`) || strings.Contains(sessionID, "..") {
		return nil, fmt.Errorf("wsbridge: prompt journal: session id %q cannot name a journal file", sessionID)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("wsbridge: prompt journal: create %s: %w", dir, err)
	}
	if err := assertStateDirIsOurs(dir, os.Getuid()); err != nil {
		return nil, err
	}

	name := promptJournalFileName(sessionID, gen)
	removeOtherPromptJournals(dir, name)

	path := filepath.Join(dir, name)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("wsbridge: prompt journal: open %s: %w", path, err)
	}
	seen, err := loadPromptJournal(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("wsbridge: prompt journal: read %s: %w", path, err)
	}
	return &promptJournal{file: file, seen: seen}, nil
}

// assertStateDirIsOurs fails unless dir is a real directory, owned by uid,
// with no group or other write bit -- the check
// internal/sandboxagent/credentials makes of its cache directory: write
// access for another uid is what lets it create, replace or symlink an
// entry. uid is a parameter so a test can name an owner other than itself.
func assertStateDirIsOurs(dir string, uid int) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("wsbridge: prompt journal: stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("wsbridge: prompt journal: %s is not a directory", dir)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("wsbridge: prompt journal: cannot read the ownership of %s", dir)
	}
	if uint64(stat.Uid) != uint64(uid) {
		return fmt.Errorf("wsbridge: prompt journal: %s is owned by uid %d, not this process (uid %d); refusing to keep the journal in a directory another user controls", dir, stat.Uid, uid)
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("wsbridge: prompt journal: %s has mode %#o; refusing to keep the journal in a directory group or others can write to", dir, perm)
	}
	return nil
}

// removeOtherPromptJournals removes, best effort, every journal file in dir
// but keep: another session's or another gen's.
func removeOtherPromptJournals(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		slog.Warn("wsbridge: prompt journal: list the state directory failed; other gens' journals left in place", "dir", dir, "error", err)
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == keep || !entry.Type().IsRegular() ||
			!strings.HasPrefix(name, promptJournalPrefix) || !strings.HasSuffix(name, promptJournalSuffix) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("wsbridge: prompt journal: remove another gen's journal failed", "file", name, "error", err)
		}
	}
}

// loadPromptJournal reads every complete line of file -- one JSON string,
// a messageId, each -- and closes a last line a crash cut short with a
// newline, fsynced, so the next append starts a line of its own.
func loadPromptJournal(file *os.File) (map[string]struct{}, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	var complete []byte
	if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
		complete = data[:i+1]
	}
	for _, line := range bytes.Split(complete, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var id string
		if err := json.Unmarshal(line, &id); err != nil || id == "" {
			continue
		}
		seen[id] = struct{}{}
	}
	if len(data) > len(complete) {
		if _, err := file.Write([]byte{'\n'}); err != nil {
			return nil, err
		}
		if err := file.Sync(); err != nil {
			return nil, err
		}
	}
	return seen, nil
}

// has reports whether id is recorded, in memory or in the file.
func (j *promptJournal) has(id string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	_, ok := j.seen[id]
	return ok
}

// record appends id to the file and fsyncs it, then records it in memory.
// When either fails, id is left unrecorded, in memory too, and the error
// returned: the caller decides whether the prompt runs anyway
// (dispatchPrompt). A line whose write landed but whose fsync failed may
// still be read by a later process of this gen, which then never runs that
// prompt -- the safe direction: a prompt run at most once.
func (j *promptJournal) record(id string) error {
	line, err := json.Marshal(id)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.file.Write(line); err != nil {
		return fmt.Errorf("wsbridge: prompt journal: append: %w", err)
	}
	if err := j.file.Sync(); err != nil {
		return fmt.Errorf("wsbridge: prompt journal: fsync: %w", err)
	}
	j.seen[id] = struct{}{}
	return nil
}

// remember records id in memory only: a prompt that asked for no receipt
// still runs when its append fails, and is then deduped for this process's
// life.
func (j *promptJournal) remember(id string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.seen[id] = struct{}{}
}

// close closes the journal's file.
func (j *promptJournal) close() {
	j.mu.Lock()
	defer j.mu.Unlock()
	_ = j.file.Close()
}

// EnablePromptReceipts opens this Bridge's prompt journal in dir
// (boot.Config.AgentStateDir), which turns on technical plan §3.3's prompt
// receipts for its gen: every ready advertises capabilities.promptReceipt,
// every prompt that sets receiptRequested is answered with a
// prompt_received event, a duplicate included, and every prompt messageId
// is run at most once, across a restart of this process within the gen
// too. Call it once, before Run.
//
// On an error the journal is not opened and the Bridge stays exactly as it
// was: no capability, no receipt, no dedup -- the safe direction, since the
// control plane then never re-sends a prompt to this gen. The error is
// logged here as well as returned.
func (b *Bridge) EnablePromptReceipts(dir string) error {
	journal, err := openPromptJournal(dir, b.sessionID, b.sessionGen)
	if err != nil {
		slog.Error("wsbridge: prompt receipts disabled: the prompt journal could not be opened", "dir", dir, "error", err)
		return err
	}
	b.journal = journal
	return nil
}

// dispatchPrompt handles one gen-checked prompt (dispatch.go). Without a
// journal it is handed to the handler, as it always was. With one:
//
//  1. Whether this gen has already received its messageId is read.
//  2. A messageId not received yet is journaled, appended and fsynced,
//     before anything else. When that fails for a prompt that asked for a
//     receipt, nothing more happens -- no receipt, no run: a receipt for a
//     prompt this agent could run again after a restart would promise what
//     it cannot keep. The control plane re-sends it after the gen's next
//     reconnect, or the turn ends at its deadline. A prompt that asked for
//     none runs anyway, deduped in memory for this process's life.
//  3. A prompt that asked for a receipt is answered with one, best-effort,
//     a duplicate too -- its messageId 'prompt_received:{messageId}', so
//     every copy is one stored row. This runs on the read loop, after the
//     buffer replay, so the send is never held.
//  4. A messageId not received before runs.
func (b *Bridge) dispatchPrompt(ctx context.Context, cmd sandboxws.Prompt) {
	if b.journal == nil {
		b.handler.HandlePrompt(ctx, cmd)
		return
	}
	requested := cmd.ReceiptRequested != nil && *cmd.ReceiptRequested

	seen := b.journal.has(cmd.MessageId)
	if !seen {
		if err := b.journal.record(cmd.MessageId); err != nil {
			if requested {
				slog.Error("wsbridge: prompt not journaled; neither run nor receipted, so the control plane may send it again",
					"messageId", cmd.MessageId, "error", err)
				return
			}
			slog.Warn("wsbridge: prompt not journaled; running it, deduped in memory only",
				"messageId", cmd.MessageId, "error", err)
			b.journal.remember(cmd.MessageId)
		}
	}

	if requested {
		receipt := sandboxws.PromptReceived{
			Type:            "prompt_received",
			MessageId:       "prompt_received:" + cmd.MessageId,
			SessionId:       b.sessionID,
			Gen:             b.sessionGen,
			PromptMessageId: cmd.MessageId,
			Duplicate:       seen,
		}
		if err := b.SendBestEffort(ctx, receipt); err != nil {
			slog.Warn("wsbridge: send prompt receipt failed", "messageId", cmd.MessageId, "error", err)
		}
	}

	if seen {
		slog.Info("wsbridge: prompt already received by this gen; not run again", "messageId", cmd.MessageId)
		return
	}
	b.handler.HandlePrompt(ctx, cmd)
}
