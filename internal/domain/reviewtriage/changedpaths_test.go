package reviewtriage_test

import (
	"reflect"
	"testing"

	"github.com/narvidev/narvi/internal/domain/reviewtriage"
)

func TestExtractChangedPaths(t *testing.T) {
	tests := []struct {
		name string
		diff string
		want []string
	}{
		{"empty diff", "", nil},
		{"no diff --git headers at all", "just some text\nwith no diff shape\n", nil},
		{
			"single file",
			"diff --git a/foo.go b/foo.go\n" +
				"index 111..222 100644\n" +
				"--- a/foo.go\n" +
				"+++ b/foo.go\n" +
				"@@ -1,2 +1,2 @@\n" +
				"-old\n" +
				"+new\n",
			[]string{"foo.go"},
		},
		{
			"multiple files, order preserved",
			"diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y\n" +
				"diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-x\n+y\n",
			[]string{"a.go", "b.go"},
		},
		{
			// Adversarial-review fix (D3, prior Step): a deleted file's
			// own "+++ /dev/null" header used to contribute NO path entry
			// at all -- this pins the fix instead: the paired
			// "--- a/<path>" side (this SAME file section's own
			// pre-change path) is now harvested.
			"deleted file's own pre-change path is harvested via +++ /dev/null pairing",
			"diff --git a/gone.go b/gone.go\n--- a/gone.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n",
			[]string{"gone.go"},
		},
		{
			// git (and GitHub's compare diff) ends a "---"/"+++" line with a
			// TAB when the path holds a space; the TAB is not part of the
			// name.
			"a path with a space loses git's trailing tab",
			"diff --git a/my dir/foo_test.go b/my dir/foo_test.go\n--- a/my dir/foo_test.go\t\n+++ b/my dir/foo_test.go\t\n@@ -1 +1 @@\n-x\n+y\n" +
				"diff --git a/my dir/gone.md b/my dir/gone.md\n--- a/my dir/gone.md\t\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n",
			[]string{"my dir/foo_test.go", "my dir/gone.md"},
		},
		{
			"duplicate header lines dedupe",
			"diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-x\n+y\n" +
				"diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -5 +5 @@\n-x\n+y\n",
			[]string{"a.go"},
		},
		{
			// Adversarial-review fix (D3, prior Step): a 100%-similarity
			// rename (no content change at all) emits neither a "---" nor
			// a "+++" line -- the ONLY place either path appears is
			// "rename from"/"rename to". Both the vacated old location and
			// the new one are real changes to the tree.
			"pure rename (100% similarity, no content change) harvests both paths",
			"diff --git a/old/widget.go b/new/widget.go\nsimilarity index 100%\nrename from old/widget.go\nrename to new/widget.go\n",
			[]string{"old/widget.go", "new/widget.go"},
		},
		{
			// A rename that ALSO changes content -- git still emits
			// rename from/to, AND a "---"/"+++" pair for the actual diff
			// hunks. Both paths still come through exactly once each,
			// deduped.
			"rename with content change harvests both paths, deduped",
			"diff --git a/old/widget.go b/new/widget.go\nsimilarity index 90%\nrename from old/widget.go\nrename to new/widget.go\nindex 111..222 100644\n--- a/old/widget.go\n+++ b/new/widget.go\n@@ -1 +1 @@\n-x\n+y\n",
			[]string{"old/widget.go", "new/widget.go"},
		},
		{
			// A deletion followed by an addition in the SAME diff must
			// never let the deletion's own pendingRemoved path leak
			// forward onto the addition -- the addition's own
			// "--- /dev/null" line must reset it first.
			"deletion immediately followed by an unrelated addition never cross-contaminates",
			"diff --git a/deleted.go b/deleted.go\n--- a/deleted.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n" +
				"diff --git a/added.go b/added.go\n--- /dev/null\n+++ b/added.go\n@@ -0,0 +1 @@\n+x\n",
			[]string{"deleted.go", "added.go"},
		},
		{
			// Adversarial-review fix (D2, adversarial review of PR #182,
			// BLOCKING): a BINARY file's own change carries no "---"/
			// "+++" pair at all -- "Binary files a/x and b/x differ" is
			// deliberately never parsed (D2's own doc comment: ambiguous
			// to split reliably) -- the path is instead recovered from
			// this section's own "diff --git a/<old> b/<new>" header, the
			// fallback ExtractChangedPaths now applies whenever a file
			// section produces no path any other way.
			"binary file change is harvested via the diff --git header fallback",
			"diff --git a/image.png b/image.png\nindex abc1234..def5678 100644\nBinary files a/image.png and b/image.png differ\n",
			[]string{"image.png"},
		},
		{
			// Adversarial-review fix (D2): a MODE-ONLY change (the
			// executable bit flipped, content unchanged) carries no
			// "---"/"+++" pair either -- "this script is now
			// world-executable, unjustified" is exactly the kind of
			// finding this gap used to silently blind the sensitive-glob
			// signal to.
			"mode-only change is harvested via the diff --git header fallback",
			"diff --git a/script.sh b/script.sh\nold mode 100644\nnew mode 100755\n",
			[]string{"script.sh"},
		},
		{
			// Adversarial-review fix (D2): a git-quoted non-ASCII path
			// (core.quotePath's own default C-style quoting, e.g.
			// `+++ "b/uni_caf\303\251.go"`) used to fail the OLD, unquoted-
			// only "+++ b/" prefix test entirely, contributing no path at
			// all for an ORDINARY modified file. The octal escapes below
			// (\303\251) decode to the UTF-8 bytes for "é" -- want is the
			// real, decoded filename.
			"git-quoted non-ASCII path is unquoted and harvested from the ordinary +++/--- pair",
			"diff --git \"a/uni_caf\\303\\251.go\" \"b/uni_caf\\303\\251.go\"\n" +
				"index 111..222 100644\n" +
				"--- \"a/uni_caf\\303\\251.go\"\n" +
				"+++ \"b/uni_caf\\303\\251.go\"\n" +
				"@@ -1 +1 @@\n" +
				"-old\n" +
				"+new\n",
			[]string{"uni_café.go"},
		},
		{
			// Adversarial-review fix (D2): the quoted form of the
			// "diff --git" header-fallback path itself (a BINARY file
			// whose own path also needs C-style quoting) -- exercises
			// parseDiffGitHeaderLine's own quoted-quoted branch directly,
			// not just the ordinary +++/--- extraction path the previous
			// case already covers.
			"binary file with a git-quoted non-ASCII path uses the quoted diff --git header fallback",
			"diff --git \"a/uni_caf\\303\\251.png\" \"b/uni_caf\\303\\251.png\"\n" +
				"index abc1234..def5678 100644\n" +
				"Binary files \"a/uni_caf\\303\\251.png\" and \"b/uni_caf\\303\\251.png\" differ\n",
			[]string{"uni_café.png"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reviewtriage.ExtractChangedPaths(tt.diff)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ExtractChangedPaths() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
