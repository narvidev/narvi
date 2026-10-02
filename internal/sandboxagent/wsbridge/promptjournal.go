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

// errPromptJournalBroken is record's answer once an append has failed:
// the journal records nothing more for this process (promptJournal.broken).
var errPromptJournalBroken = errors.New("wsbridge: prompt journal: broken by an earlier failed append")

// errPromptNotJournaled is dispatchPrompt's answer when a prompt that asked
// for a receipt could not be journaled: dispatch returns it, which ends the
// connection, so the next ready tells the control plane that this agent no
// longer receipts prompts.
var errPromptNotJournaled = errors.New("wsbridge: a prompt that asked for a receipt could not be journaled; prompt receipts are now off")

// journalFile is what the journal needs of its file; *os.File is one. An
// interface so a test can fail an append part-way through.
type journalFile interface {
	Write([]byte) (int, error)
	Sync() error
	Truncate(size int64) error
	Close() error
}

// promptJournal is the agent's half of technical plan §3.3's prompt
// receipts (doc.go, "Prompt receipts and the dedup journal"): the set of
// prompt messageIds this session's gen has received, held in memory and
// appended, one JSON string per line, to a file of its own, fsynced after
// every append, so a copy of a prompt re-sent after a reconnect is never
// run twice -- across a restart of this process too.
//
// The file holds only complete lines, each ending in a newline: a torn
// tail is cut off at open, and an append that fails is cut back off, so
// nothing a failure leaves can glue onto the next line or turn into one.
// size is the length of those complete lines. After a failed append the
// journal is broken for good: record refuses every later append, and the
// Bridge stops advertising the capability (Bridge.promptReceiptsOn).
type promptJournal struct {
	mu     sync.Mutex
	file   journalFile
	size   int64
	seen   map[string]struct{}
	broken bool
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
// is only ever sent after the append's fsync has returned. It is cut off
// here, so it can neither glue onto the next append nor, closed by one,
// read as recorded to a later process.
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
	seen, size, err := loadPromptJournal(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("wsbridge: prompt journal: read %s: %w", path, err)
	}
	return &promptJournal{file: file, size: size, seen: seen}, nil
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
// a messageId, each -- and truncates a last line a crash cut short, fsynced,
// so the file holds complete lines only. It returns their ids and length.
func loadPromptJournal(file *os.File) (map[string]struct{}, int64, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, 0, err
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
		if err := file.Truncate(int64(len(complete))); err != nil {
			return nil, 0, err
		}
		if err := file.Sync(); err != nil {
			return nil, 0, err
		}
	}
	return seen, int64(len(complete)), nil
}

// has reports whether id is recorded, in memory or in the file.
func (j *promptJournal) has(id string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	_, ok := j.seen[id]
	return ok
}

// record appends id to the file and fsyncs it, then records it in memory.
// When either fails -- a full disk, an I/O error, a write cut short -- id is
// left unrecorded, in memory too, the file is cut back to its complete
// lines, and the journal is broken: this append and every later one return
// an error, and the caller decides what the prompt does (dispatchPrompt).
// Cutting back keeps a fragment from gluing onto a later line, which would
// then read as no line at all; breaking the journal keeps a later append
// from landing behind a fragment the cut could not remove.
func (j *promptJournal) record(id string) error {
	line, err := json.Marshal(id)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.broken {
		return errPromptJournalBroken
	}
	_, err = j.file.Write(line)
	if err == nil {
		err = j.file.Sync()
	}
	if err != nil {
		j.broken = true
		if cutErr := j.file.Truncate(j.size); cutErr != nil {
			err = errors.Join(err, fmt.Errorf("cut back to %d bytes: %w", j.size, cutErr))
		} else {
			_ = j.file.Sync()
		}
		return fmt.Errorf("wsbridge: prompt journal: append: %w", err)
	}
	j.size += int64(len(line))
	j.seen[id] = struct{}{}
	return nil
}

// isBroken reports whether an append has failed: the journal records
// nothing more, and the Bridge advertises no capability.
func (j *promptJournal) isBroken() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.broken
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

// promptReceiptsOn reports whether this Bridge advertises the
// prompt-receipt capability on its next ready: its journal is open and no
// append has failed.
func (b *Bridge) promptReceiptsOn() bool {
	return b.journal != nil && !b.journal.isBroken()
}

// dispatchPrompt handles one gen-checked prompt (dispatch.go). Without a
// journal it is handed to the handler, as it always was. With one:
//
//  1. Whether this gen has already received its messageId is read.
//  2. A messageId not received yet is journaled, appended and fsynced,
//     before anything else. A prompt that asked for none runs even when
//     that fails, deduped in memory for this process's life. When it fails
//     for a prompt that asked for a receipt, the prompt is neither run nor
//     receipted: a receipt for a prompt this agent could run again after a
//     restart would promise what it cannot keep. Instead the agent says so
//     and stops promising: the failed append has broken the journal, so no
//     later ready advertises the capability; a non-fatal error event names
//     the prompt and the failure, critical so a reconnect cannot lose it;
//     and the connection is ended (errPromptNotJournaled), so the ready of
//     the next one tells the control plane at once. That ready records this
//     gen as incapable: nothing is re-sent to it -- the lost prompt's turn
//     ends at its deadline, as an agent without receipts' would -- and no
//     later dispatch asks it for a receipt.
//  3. A prompt that asked for a receipt is answered with one, best-effort,
//     a duplicate too -- its messageId 'prompt_received:{messageId}', so
//     every copy is one stored row. This runs on the read loop, after the
//     buffer replay, so the send is never held.
//  4. A messageId not received before runs.
func (b *Bridge) dispatchPrompt(ctx context.Context, cmd sandboxws.Prompt) error {
	if b.journal == nil {
		b.handler.HandlePrompt(ctx, cmd)
		return nil
	}
	requested := cmd.ReceiptRequested != nil && *cmd.ReceiptRequested

	seen := b.journal.has(cmd.MessageId)
	if !seen {
		if err := b.journal.record(cmd.MessageId); err != nil {
			if requested {
				slog.Error("wsbridge: prompt not journaled; neither run nor receipted, prompt receipts now off, reconnecting",
					"messageId", cmd.MessageId, "error", err)
				b.reportPromptNotJournaled(ctx, cmd, err)
				return errPromptNotJournaled
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
		return nil
	}
	b.handler.HandlePrompt(ctx, cmd)
	return nil
}

// reportPromptNotJournaled tells the control plane, with a non-fatal error
// event, that the prompt cmd names was not run because it could not be
// journaled, and that this sandbox receipts no prompt from now on. The
// event is critical, so it is replayed on the next connection until acked,
// and its messageId is derived from the prompt's, so every report of one
// prompt is one stored row.
func (b *Bridge) reportPromptNotJournaled(ctx context.Context, cmd sandboxws.Prompt, cause error) {
	messageID := "prompt-not-journaled:" + cmd.MessageId
	event := sandboxws.SandboxErrorEvent{
		Type:      "error",
		MessageId: messageID,
		SessionId: b.sessionID,
		Gen:       b.sessionGen,
		AckId:     "error:" + messageID,
		Message: fmt.Sprintf("prompt %s was not run: this sandbox could not record it (%v), so it could not promise to run it only once. "+
			"This sandbox no longer receipts prompts, so the prompt is not sent again; its turn ends at its deadline unless it is stopped first.",
			cmd.MessageId, cause),
		Fatal: false,
	}
	if err := b.SendCritical(ctx, event, event.AckId); err != nil {
		slog.Warn("wsbridge: report an unjournaled prompt failed", "messageId", cmd.MessageId, "error", err)
	}
}
