package outboxworker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/platform"
)

// TestClassification_CoversEveryKindDeclaredInSource reads the notification
// kinds out of ports/notifier.go itself, instead of comparing the
// classification table against a second list maintained by hand beside it.
//
// The hand-maintained list could not do what its own comment claimed. It
// said a new kind added to notifier.go without a matching entry here would
// fail -- but a kind added to notifier.go ALONE leaves both this list and
// the classification map untouched, so they still agree and the test stays
// green. Two copies of the same fact cannot check each other; only the
// source can.
//
// Go cannot enumerate a named string type's constants at runtime, so this
// parses the declaring file. That is the same technique internal/ops uses
// for the repo's other structural checks, and it fails for the event that
// actually happens: someone declares a twentieth kind.
func TestClassification_CoversEveryKindDeclaredInSource(t *testing.T) {
	declared := notificationKindsDeclaredInSource(t)
	if len(declared) < 19 {
		t.Fatalf("found only %d NotificationKind constants in the source; the parse is broken, not the table", len(declared))
	}

	for _, name := range declared {
		kind := ports.NotificationKind(name.value)
		if _, ok := notificationKindClassification[kind]; !ok {
			t.Errorf("%s (%q) is declared in internal/app/ports/notifier.go but has no egress classification.\n"+
				"    Every kind must be classified as one that reaches a customer or one that does not.\n"+
				"    Add it to notificationKindClassification and say which it is, and why, in its comment.",
				name.constName, name.value)
		}
	}

	// And the reverse: a classification for a kind nobody declares is dead
	// weight that outlives the thing it described.
	declaredValues := make(map[string]bool, len(declared))
	for _, d := range declared {
		declaredValues[d.value] = true
	}
	for kind := range notificationKindClassification {
		if !declaredValues[string(kind)] {
			t.Errorf("the classification table carries %q, which no NotificationKind constant declares any more", kind)
		}
	}
}

// TestRepeatability_CoversEveryKindDeclaredInSource is the same check for
// the repeatability table (repeatability.go): every kind the port declares
// says whether a second delivery adds to the first, and the table names no
// kind the port does not declare. A kind missing here would be read as not
// repeatable -- the safe direction -- but silently, so it is refused
// instead, here and by NewBuilder.
func TestRepeatability_CoversEveryKindDeclaredInSource(t *testing.T) {
	declared := notificationKindsDeclaredInSource(t)
	if len(declared) < 19 {
		t.Fatalf("found only %d NotificationKind constants in the source; the parse is broken, not the table", len(declared))
	}

	declaredValues := make(map[string]bool, len(declared))
	for _, name := range declared {
		declaredValues[name.value] = true
		if _, ok := notificationKindRepeatability[ports.NotificationKind(name.value)]; !ok {
			t.Errorf("%s (%q) is declared in internal/app/ports/notifier.go but has no repeatability classification.\n"+
				"    Say whether delivering it again after the remote end accepted it adds to its effect,\n"+
				"    in notificationKindRepeatability, and why, in its comment.",
				name.constName, name.value)
		}
	}
	for kind := range notificationKindRepeatability {
		if !declaredValues[string(kind)] {
			t.Errorf("the repeatability table carries %q, which no NotificationKind constant declares any more", kind)
		}
	}
}

// TestCheckRepeatability pins NewBuilder's refusal: a registered kind the
// repeatability table does not carry is named, and a map of classified
// kinds passes.
func TestCheckRepeatability(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kinds    []ports.NotificationKind
		wantMiss string
	}{
		{name: "every kind classified", kinds: []ports.NotificationKind{ports.NotificationKindSlack, ports.NotificationKindBlobDelete}},
		{name: "one kind unclassified", kinds: []ports.NotificationKind{ports.NotificationKindSlack, "never_classified_kind"}, wantMiss: "never_classified_kind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notifiers := make(map[ports.NotificationKind]ports.Notifier, len(tc.kinds))
			for _, kind := range tc.kinds {
				notifiers[kind] = nil
			}
			err := checkRepeatability(notifiers)
			if tc.wantMiss == "" {
				if err != nil {
					t.Fatalf("checkRepeatability = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantMiss) {
				t.Fatalf("checkRepeatability = %v, want a refusal naming %q", err, tc.wantMiss)
			}
		})
	}
}

// TestNewBuilder_RefusesAKindWithoutRepeatability pins that NewBuilder
// applies checkRepeatability to its finished map: a registered kind that
// has an egress classification but no repeatability one is refused.
func TestNewBuilder_RefusesAKindWithoutRepeatability(t *testing.T) {
	const kind = ports.NotificationKindBlobDelete
	saved, ok := notificationKindRepeatability[kind]
	if !ok {
		t.Fatalf("%q has no repeatability classification to remove", kind)
	}
	delete(notificationKindRepeatability, kind)
	t.Cleanup(func() { notificationKindRepeatability[kind] = saved })

	_, err := NewBuilder(nil, nil, map[ports.NotificationKind]ports.Notifier{kind: nil}, platform.DefaultTimeouts(), &platform.ShutdownState{})
	if err == nil || !strings.Contains(err.Error(), string(kind)) {
		t.Fatalf("NewBuilder = %v, want a refusal naming %q", err, kind)
	}
}

type declaredKind struct {
	constName string
	value     string
}

func notificationKindsDeclaredInSource(t *testing.T) []declaredKind {
	t.Helper()

	path := notifierSourcePath(t)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var out []declaredKind
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		ident, ok := spec.Type.(*ast.Ident)
		if !ok || ident.Name != "NotificationKind" {
			return true
		}
		for i, name := range spec.Names {
			if i >= len(spec.Values) {
				continue
			}
			lit, ok := spec.Values[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			out = append(out, declaredKind{constName: name.Name, value: lit.Value[1 : len(lit.Value)-1]})
		}
		return true
	})
	return out
}

func notifierSourcePath(t *testing.T) string {
	t.Helper()
	// Walk up to the module root, which is where internal/ lives.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "internal", "app", "ports", "notifier.go")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find internal/app/ports/notifier.go from the working directory")
		}
		dir = parent
	}
}
