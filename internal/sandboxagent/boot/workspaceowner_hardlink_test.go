package boot

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// TestChownTree_HardLinkedEntriesFollowProtectedHardlinks pins what the walk
// does with a non-directory entry that has more than one link, for each
// fs.protected_hardlinks reading. The tree holds a file from outside the
// workspace hard-linked into it, a pair of hard links that are both
// inside, and a pair the runtime already owns, as one its own processes
// linked would be. At 1 every entry is re-owned, the outside file with
// its inside name, since they are one inode: a kernel that reads 1 let
// the runtime plant only a link to a file it could already read and
// write. At 0, or unread, no entry with more than one link is re-owned,
// the outside file keeps its owner, and one WARN counts the linked entries
// not already the runtime's, by their names relative to the root.
func TestChownTree_HardLinkedEntriesFollowProtectedHardlinks(t *testing.T) {
	setting := func(value string, err error) func() (string, error) {
		return func() (string, error) { return value, err }
	}
	tests := []struct {
		name    string
		setting func() (string, error)
		// reownLinked: entries with more than one link are re-owned.
		reownLinked bool
	}{
		{name: "protected_hardlinks is 1", setting: setting("1", nil), reownLinked: true},
		{name: "protected_hardlinks is 0", setting: setting("0", nil), reownLinked: false},
		{name: "protected_hardlinks cannot be read", setting: setting("", fs.ErrNotExist), reownLinked: false},
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

			var logs bytes.Buffer
			opts := walkOptions{protectedHardlinks: tc.setting, logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			if err := chownTree(root, uid, gid, opts); err != nil {
				t.Fatalf("chownTree() error = %v, want nil", err)
			}

			reowned := func(rel string) bool {
				u, g := ownerOf(t, filepath.Join(root, rel))
				return u == uid && g == gid
			}
			for _, rel := range []string{".", "repo", "repo/sub", "repo/plain", "repo/owned-a", "repo/sub/owned-b"} {
				if !reowned(rel) {
					t.Errorf("%s was not re-owned -- a directory, or an entry with one link, is re-owned whatever the setting", rel)
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

			records := decodeRecords(t, &logs)
			starts := recordsWhere(records, func(r map[string]any) bool { return r["reown_hard_linked"] != nil })
			if len(starts) != 1 || starts[0]["level"] != "INFO" || starts[0]["reown_hard_linked"] != tc.reownLinked {
				t.Errorf("records stating the setting = %v, want one INFO with reown_hard_linked=%t", starts, tc.reownLinked)
			} else if value, err := tc.setting(); err == nil && starts[0]["value"] != value {
				t.Errorf("logged value = %v, want %q", starts[0]["value"], value)
			} else if err != nil && starts[0]["error"] == nil {
				t.Errorf("record %v names no error, want the read's", starts[0])
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

// TestChownTree_ReadsTheKernelsSettingByDefault pins that the walk
// production runs reads fs.protected_hardlinks from the kernel, through
// readProtectedHardlinks, rather than assuming a value.
func TestChownTree_ReadsTheKernelsSettingByDefault(t *testing.T) {
	var logs bytes.Buffer
	if err := chownTree(t.TempDir(), uint32(os.Getuid()), uint32(os.Getgid()), walkOptions{logger: slog.New(slog.NewJSONHandler(&logs, nil))}); err != nil {
		t.Fatalf("chownTree() error = %v, want nil", err)
	}
	starts := recordsWhere(decodeRecords(t, &logs), func(r map[string]any) bool { return r["reown_hard_linked"] != nil })
	if len(starts) != 1 {
		t.Fatalf("records stating the setting = %v, want one", starts)
	}
	value, err := readProtectedHardlinks()
	if err != nil {
		if starts[0]["error"] == nil || starts[0]["reown_hard_linked"] != false {
			t.Errorf("record = %v, want the read's error and reown_hard_linked=false", starts[0])
		}
		return
	}
	if starts[0]["value"] != value || starts[0]["reown_hard_linked"] != (value == "1") {
		t.Errorf("record = %v, want value %q and reown_hard_linked=%t", starts[0], value, value == "1")
	}
}

// TestReadProtectedHardlinks pins the reader itself: the kernel's value,
// trimmed, on Linux, and an error anywhere else, which the walk treats as
// a setting it could not read.
func TestReadProtectedHardlinks(t *testing.T) {
	value, err := readProtectedHardlinks()
	if runtime.GOOS != "linux" {
		if err == nil {
			t.Fatalf("readProtectedHardlinks() = %q, want an error: only Linux has the setting", value)
		}
		return
	}
	if err != nil {
		t.Fatalf("readProtectedHardlinks() error = %v", err)
	}
	if value != "0" && value != "1" {
		t.Errorf("readProtectedHardlinks() = %q, want 0 or 1", value)
	}
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
