package boot

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

// TestChownTree_HardLinkedEntriesFollowTheKernelsBound pins what the walk
// does with a non-directory entry that has more than one link, for each
// way the verdict on hard links can come out. The tree holds a file from
// outside the workspace hard-linked into it, a pair of hard links that are
// both inside, and a pair the runtime already owns, as one its own
// processes linked would be. Bounded, every entry is re-owned, the outside
// file with its inside name, since they are one inode: a kernel that
// refuses the runtime a link to a file it cannot read and write let it
// plant nothing it could not already change. Not bounded, no entry with
// more than one link is re-owned, the outside file keeps its owner, and
// one WARN counts the linked entries not already the runtime's, by their
// names relative to the root.
//
// Only an inconclusive probe reads fs.protected_hardlinks: a refusal or a
// link made decides alone, whatever the setting says.
func TestChownTree_HardLinkedEntriesFollowTheKernelsBound(t *testing.T) {
	inconclusive := probeResult{err: errors.New("the probe could not run")}
	tests := []struct {
		name    string
		probe   probeResult
		setting string
		readErr error
		// wantProbe is the probe outcome the verdict logs; wantRead, that
		// the setting was read and logged.
		wantProbe string
		wantRead  bool
		// reownLinked: entries with more than one link are re-owned.
		reownLinked bool
	}{
		{name: "the probe is refused, the setting unread", probe: probeResult{linkErr: unix.EPERM}, setting: "0", wantProbe: "refused", reownLinked: true},
		{name: "the probe links, the setting unread", probe: probeResult{}, setting: "1", wantProbe: "linked", reownLinked: false},
		{name: "the probe is inconclusive and protected_hardlinks is 1", probe: inconclusive, setting: "1", wantProbe: "inconclusive", wantRead: true, reownLinked: true},
		{name: "the probe is inconclusive and protected_hardlinks is 0", probe: inconclusive, setting: "0", wantProbe: "inconclusive", wantRead: true, reownLinked: false},
		{name: "the probe is inconclusive and protected_hardlinks cannot be read", probe: inconclusive, readErr: fs.ErrNotExist, wantProbe: "inconclusive", wantRead: true, reownLinked: false},
	}
	// linked are the entries with more than one link whose owner is not
	// the runtime's before the walk.
	linked := []string{"repo/from-outside", "repo/pair-a", "repo/sub/pair-b"}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			buildTree(t, root, []string{"repo/sub"}, []string{"repo/plain", "repo/pair-a", "repo/owned-a"})
			buildTree(t, outside, nil, []string{"secret"})
			secret := filepath.Join(outside, "secret")
			hardLink(t, secret, filepath.Join(root, "repo", "from-outside"))
			hardLink(t, filepath.Join(root, "repo", "pair-a"), filepath.Join(root, "repo", "sub", "pair-b"))
			hardLink(t, filepath.Join(root, "repo", "owned-a"), filepath.Join(root, "repo", "sub", "owned-b"))

			uid, gid := observableOwner(t, root)
			if err := os.Lchown(filepath.Join(root, "repo", "owned-a"), int(uid), int(gid)); err != nil {
				t.Fatalf("Lchown(owned-a): %v", err)
			}
			su, sg := ownerOf(t, secret)
			secretBefore := [2]uint32{su, sg}

			var reads int
			var logs bytes.Buffer
			opts := walkOptions{
				probe: func(int, int) probeResult { return tc.probe },
				protectedHardlinks: func() (string, error) {
					reads++
					return tc.setting, tc.readErr
				},
				logger: slog.New(slog.NewJSONHandler(&logs, nil)),
			}
			if err := chownTree(root, uid, gid, opts); err != nil {
				t.Fatalf("chownTree() error = %v, want nil", err)
			}

			reowned := func(rel string) bool {
				u, g := ownerOf(t, filepath.Join(root, rel))
				return u == uid && g == gid
			}
			for _, rel := range []string{".", "repo", "repo/sub", "repo/plain", "repo/owned-a", "repo/sub/owned-b"} {
				if !reowned(rel) {
					t.Errorf("%s was not re-owned -- a directory, or an entry with one link, is re-owned whatever the verdict", rel)
				}
			}
			for _, rel := range linked {
				if got := reowned(rel); got != tc.reownLinked {
					t.Errorf("%s (more than one link): re-owned = %t, want %t", rel, got, tc.reownLinked)
				}
			}
			u, g := ownerOf(t, secret)
			if got := [2]uint32{u, g} != secretBefore; got != tc.reownLinked {
				t.Errorf("the OUTSIDE file hard-linked into the tree: re-owned = %t (%d:%d -> %d:%d), want %t", got, secretBefore[0], secretBefore[1], u, g, tc.reownLinked)
			}
			if wantReads := map[bool]int{false: 0, true: 1}[tc.wantRead]; reads != wantReads {
				t.Errorf("fs.protected_hardlinks read %d times, want %d: only an inconclusive probe reads it", reads, wantReads)
			}

			records := decodeRecords(t, &logs)
			verdicts := recordsWhere(records, func(r map[string]any) bool { return r["reown_hard_linked"] != nil })
			if len(verdicts) != 1 {
				t.Fatalf("records stating the verdict = %v, want exactly one", verdicts)
			}
			v := verdicts[0]
			if v["level"] != "INFO" || v["reown_hard_linked"] != tc.reownLinked || v["probe"] != tc.wantProbe {
				t.Errorf("verdict record = %v, want INFO with reown_hard_linked=%t and probe=%q", v, tc.reownLinked, tc.wantProbe)
			}
			_, loggedValue := v["protected_hardlinks"]
			_, loggedErr := v["protected_hardlinks_error"]
			switch {
			case !tc.wantRead && (loggedValue || loggedErr):
				t.Errorf("verdict record = %v logs the setting, want it unread", v)
			case tc.wantRead && tc.readErr != nil && !loggedErr:
				t.Errorf("verdict record = %v, want the read's error logged", v)
			case tc.wantRead && tc.readErr == nil && v["protected_hardlinks"] != tc.setting:
				t.Errorf("verdict record = %v, want protected_hardlinks=%q", v, tc.setting)
			}

			warns := recordsWhere(records, func(r map[string]any) bool { return r["level"] == "WARN" })
			if tc.reownLinked {
				if len(warns) != 0 {
					t.Errorf("WARN records = %v, want none: every entry was re-owned", warns)
				}
				return
			}
			if len(warns) != 1 {
				t.Fatalf("WARN records = %v, want exactly one", warns)
			}
			if got := warns[0]["count"]; got != float64(len(linked)) {
				t.Errorf("WARN count = %v, want %d -- the linked entries not already the runtime's", got, len(linked))
			}
			if got := warns[0]["root"]; got != root {
				t.Errorf("WARN root = %v, want %q", got, root)
			}
			var sample []string
			for _, name := range warns[0]["sample"].([]any) {
				sample = append(sample, name.(string))
			}
			slices.Sort(sample)
			if !slices.Equal(sample, linked) {
				t.Errorf("WARN sample = %q, want %q, relative to the root", sample, linked)
			}
		})
	}
}

// TestDecideHardLinks pins the verdict for every probe outcome crossed
// with every reading of fs.protected_hardlinks. Only EPERM from the
// runtime's link means bounded, and only a link made means not; every
// other outcome -- another errno, which a live process of the runtime's
// uid can cause, a child that could not run, one that timed out -- leaves
// the verdict to the setting, which reads bounded at "1" alone.
func TestDecideHardLinks(t *testing.T) {
	probes := []struct {
		name   string
		result probeResult
		// decides is the verdict the probe gives alone; nil leaves it to
		// the setting.
		decides *bool
	}{
		{name: "EPERM", result: probeResult{linkErr: unix.EPERM}, decides: ptr(true)},
		{name: "linked", result: probeResult{}, decides: ptr(false)},
		{name: "EEXIST", result: probeResult{linkErr: unix.EEXIST}},
		{name: "EACCES", result: probeResult{linkErr: unix.EACCES}},
		{name: "exec failure", result: probeResult{err: &fs.PathError{Op: "fork/exec", Path: "/proc/self/exe", Err: unix.EACCES}}},
		{name: "timeout", result: probeResult{err: errors.New("no answer within 10s")}},
		// An EPERM beside a failure to run is no answer.
		{name: "EPERM beside a failure", result: probeResult{err: errors.New("could not run"), linkErr: unix.EPERM}},
	}
	settings := []struct {
		name  string
		value string
		err   error
	}{
		{name: "1", value: "1"},
		{name: "0", value: "0"},
		{name: "unreadable", err: fs.ErrNotExist},
	}
	for _, p := range probes {
		for _, s := range settings {
			t.Run(p.name+"/"+s.name, func(t *testing.T) {
				read := false
				v := decideHardLinks(p.result, func() (string, error) {
					read = true
					return s.value, s.err
				})
				want := s.err == nil && s.value == "1"
				if p.decides != nil {
					want = *p.decides
				}
				if v.bounded != want {
					t.Errorf("bounded = %t, want %t", v.bounded, want)
				}
				if read != (p.decides == nil) || v.settingRead != read {
					t.Errorf("setting read = %t (recorded %t), want %t: only an inconclusive probe reads it", read, v.settingRead, p.decides == nil)
				}
			})
		}
	}
}

// TestChownTree_ProductionVerdictIsProbedAndLoggedOnce pins the walk
// production runs: with no seam injected it acts on the real probe's
// verdict, and computes and logs that verdict once per process and runtime
// identity, however many walks follow. On Linux as root the probe must
// answer; anywhere else it is inconclusive, and the setting decides.
func TestChownTree_ProductionVerdictIsProbedAndLoggedOnce(t *testing.T) {
	resetHardLinkVerdicts(t)
	uid, gid := probeIdentity()
	var logs bytes.Buffer
	opts := walkOptions{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	for range 3 {
		if err := chownTree(t.TempDir(), uid, gid, opts); err != nil {
			t.Fatalf("chownTree() error = %v, want nil", err)
		}
	}
	verdicts := recordsWhere(decodeRecords(t, &logs), func(r map[string]any) bool { return r["reown_hard_linked"] != nil })
	if len(verdicts) != 1 {
		t.Fatalf("records stating the verdict over three walks = %v, want exactly one", verdicts)
	}

	want := decideHardLinks(productionHardLinkProbe.run(int(uid), int(gid)), readProtectedHardlinks)
	outcome, _ := want.probe.outcome()
	if verdicts[0]["probe"] != outcome || verdicts[0]["reown_hard_linked"] != want.bounded {
		t.Errorf("verdict record = %v, want probe=%q and reown_hard_linked=%t, as the production probe and reader give", verdicts[0], outcome, want.bounded)
	}
	switch {
	case runtime.GOOS != "linux", os.Geteuid() != 0:
		if outcome != "inconclusive" {
			t.Errorf("probe outcome = %q, want inconclusive: the probe runs only on Linux, as root", outcome)
		}
	case outcome == "inconclusive":
		t.Errorf("probe outcome = inconclusive (%v), want an answer: on Linux as root, this binary answers the probe", verdicts[0]["probe_error"])
	}
}

// TestReadProtectedHardlinks pins the reader itself: the kernel's value,
// trimmed, on Linux, and an error anywhere else, or where /proc/sys/fs is
// masked as gVisor presents it, which the walk treats as a setting it
// could not read.
func TestReadProtectedHardlinks(t *testing.T) {
	value, err := readProtectedHardlinks()
	switch {
	case runtime.GOOS != "linux":
		if err == nil {
			t.Fatalf("readProtectedHardlinks() = %q, want an error: only Linux has the setting", value)
		}
	case maskedProtectedHardlinks() != "":
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("readProtectedHardlinks() = %q, %v, want ErrNotExist: %s says /proc/sys/fs is masked", value, err, maskedEnv)
		}
	case err != nil:
		t.Fatalf("readProtectedHardlinks() error = %v", err)
	case value != "0" && value != "1":
		t.Errorf("readProtectedHardlinks() = %q, want 0 or 1", value)
	}
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// resetHardLinkVerdicts empties the per-process verdict cache for the
// test, and restores it after.
func resetHardLinkVerdicts(t *testing.T) {
	t.Helper()
	hardLinkVerdicts.Lock()
	saved := hardLinkVerdicts.byRuntime
	hardLinkVerdicts.byRuntime = nil
	hardLinkVerdicts.Unlock()
	t.Cleanup(func() {
		hardLinkVerdicts.Lock()
		hardLinkVerdicts.byRuntime = saved
		hardLinkVerdicts.Unlock()
	})
}

// probeIdentity is a runtime identity the calling process may re-own to:
// 65534:65534 as root, for which the probe is a real one, and its own
// otherwise.
func probeIdentity() (uid, gid uint32) {
	if os.Geteuid() == 0 {
		return 65534, 65534
	}
	return uint32(os.Getuid()), uint32(os.Getgid())
}

// hardLink links newname to oldname's inode.
func hardLink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Link(oldname, newname); err != nil {
		t.Fatalf("Link(%s, %s): %v", oldname, newname, err)
	}
}

// decodeRecords parses the JSON records a slog.JSONHandler wrote to buf.
func decodeRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	dec := json.NewDecoder(buf)
	for dec.More() {
		var r map[string]any
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("decode log record: %v", err)
		}
		records = append(records, r)
	}
	return records
}

// recordsWhere returns the records keep accepts.
func recordsWhere(records []map[string]any, keep func(map[string]any) bool) []map[string]any {
	var out []map[string]any
	for _, r := range records {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}
