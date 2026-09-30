package reviewtriage_test

import (
	"reflect"
	"testing"

	"github.com/narvidev/narvi/internal/domain/reviewtriage"
)

func TestExtractFileLines(t *testing.T) {
	tests := []struct {
		name string
		diff string
		want []reviewtriage.FileLines
	}{
		{name: "empty diff", diff: "", want: nil},
		{
			name: "modified file, one hunk with context",
			diff: "diff --git a/internal/a.go b/internal/a.go\n" +
				"index 1111111..2222222 100644\n" +
				"--- a/internal/a.go\n" +
				"+++ b/internal/a.go\n" +
				"@@ -1,3 +1,4 @@\n" +
				" package a\n" +
				"-var x = 1\n" +
				"+var x = 2\n" +
				"+var y = 3\n" +
				" \n",
			want: []reviewtriage.FileLines{{Path: "internal/a.go", Added: 2, Deleted: 1}},
		},
		{
			name: "content lines that read like file headers are counted as content",
			diff: "diff --git a/a.txt b/a.txt\n" +
				"--- a/a.txt\n" +
				"+++ b/a.txt\n" +
				"@@ -1,2 +1,2 @@\n" +
				"--- removed line starting with two dashes\n" +
				"+++ added line starting with two pluses\n" +
				"-plain removed\n" +
				"+plain added\n",
			want: []reviewtriage.FileLines{{Path: "a.txt", Added: 2, Deleted: 2}},
		},
		{
			name: "two hunks, omitted counts default to one, no-newline marker counts nothing",
			diff: "diff --git a/b.go b/b.go\n" +
				"--- a/b.go\n" +
				"+++ b/b.go\n" +
				"@@ -1 +1 @@\n" +
				"-old\n" +
				"+new\n" +
				"@@ -10,2 +10,3 @@ func f() {\n" +
				" a\n" +
				"+b\n" +
				" c\n" +
				"\\ No newline at end of file\n",
			want: []reviewtriage.FileLines{{Path: "b.go", Added: 2, Deleted: 1}},
		},
		{
			name: "added and deleted files",
			diff: "diff --git a/new.go b/new.go\n" +
				"new file mode 100644\n" +
				"--- /dev/null\n" +
				"+++ b/new.go\n" +
				"@@ -0,0 +1,2 @@\n" +
				"+line1\n" +
				"+line2\n" +
				"diff --git a/gone.go b/gone.go\n" +
				"deleted file mode 100644\n" +
				"--- a/gone.go\n" +
				"+++ /dev/null\n" +
				"@@ -1,3 +0,0 @@\n" +
				"-x\n" +
				"-y\n" +
				"-z\n",
			want: []reviewtriage.FileLines{
				{Path: "new.go", Added: 2},
				{Path: "gone.go", Deleted: 3},
			},
		},
		{
			name: "pure rename carries both paths and no lines",
			diff: "diff --git a/src/old.go b/test/new_test.go\n" +
				"similarity index 100%\n" +
				"rename from src/old.go\n" +
				"rename to test/new_test.go\n",
			want: []reviewtriage.FileLines{{Path: "test/new_test.go", OldPath: "src/old.go"}},
		},
		{
			name: "rename with a content change",
			diff: "diff --git a/src/old.go b/src/new.go\n" +
				"similarity index 90%\n" +
				"rename from src/old.go\n" +
				"rename to src/new.go\n" +
				"--- a/src/old.go\n" +
				"+++ b/src/new.go\n" +
				"@@ -1,1 +1,1 @@\n" +
				"-a\n" +
				"+b\n",
			want: []reviewtriage.FileLines{{Path: "src/new.go", OldPath: "src/old.go", Added: 1, Deleted: 1}},
		},
		{
			name: "binary and mode-only sections count zero lines",
			diff: "diff --git a/img.png b/img.png\n" +
				"index 1111111..2222222 100644\n" +
				"Binary files a/img.png and b/img.png differ\n" +
				"diff --git a/run.sh b/run.sh\n" +
				"old mode 100644\n" +
				"new mode 100755\n",
			want: []reviewtriage.FileLines{{Path: "img.png"}, {Path: "run.sh"}},
		},
		{
			name: "quoted non-ASCII path",
			diff: "diff --git \"a/caf\\303\\251.go\" \"b/caf\\303\\251.go\"\n" +
				"--- \"a/caf\\303\\251.go\"\n" +
				"+++ \"b/caf\\303\\251.go\"\n" +
				"@@ -1 +1 @@\n" +
				"-a\n" +
				"+b\n",
			want: []reviewtriage.FileLines{{Path: "café.go", Added: 1, Deleted: 1}},
		},
		{
			name: "a path with a space loses git's trailing tab",
			diff: "diff --git a/my dir/foo_test.go b/my dir/foo_test.go\n" +
				"--- a/my dir/foo_test.go\t\n" +
				"+++ b/my dir/foo_test.go\t\n" +
				"@@ -1 +1,2 @@\n" +
				" keep\n" +
				"+add\n" +
				"diff --git a/my dir/old name.go b/my dir/new_test.go\n" +
				"similarity index 80%\n" +
				"rename from my dir/old name.go\n" +
				"rename to my dir/new_test.go\n" +
				"--- a/my dir/old name.go\t\n" +
				"+++ b/my dir/new_test.go\n" +
				"@@ -1 +1 @@\n" +
				"-a\n" +
				"+b\n",
			want: []reviewtriage.FileLines{
				{Path: "my dir/foo_test.go", Added: 1},
				{Path: "my dir/new_test.go", OldPath: "my dir/old name.go", Added: 1, Deleted: 1},
			},
		},
		{
			name: "plain unified diff with no git header",
			diff: "--- a/one.txt\n" +
				"+++ b/one.txt\n" +
				"@@ -1 +1,2 @@\n" +
				" keep\n" +
				"+add\n" +
				"--- a/two.txt\n" +
				"+++ b/two.txt\n" +
				"@@ -1 +0,0 @@\n" +
				"-drop\n",
			want: []reviewtriage.FileLines{
				{Path: "one.txt", Added: 1},
				{Path: "two.txt", Deleted: 1},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reviewtriage.ExtractFileLines(tt.diff)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ExtractFileLines() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
