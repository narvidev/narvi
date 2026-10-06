package outboxworker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	domainautonomy "github.com/narvidev/narvi/internal/domain/autonomy"
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

// TestRepeatability_EveryKindsValue pins every kind's repeatability, not
// only its presence: each value decides, when a shutdown cuts that kind's
// delivery short, whether the attempt is given back -- and a kind whose
// second delivery adds to the first must never get it back. Flipping any
// one value fails here, naming the kind and the reason it has that value.
func TestRepeatability_EveryKindsValue(t *testing.T) {
	type pinned struct {
		value  Repeatability
		reason string
	}
	want := map[ports.NotificationKind]pinned{
		ports.NotificationKindSlack:                    {NotRepeatable, "chat.postMessage: a repeat posts a second message"},
		ports.NotificationKindSlackPlanApproval:        {NotRepeatable, "chat.postMessage of the approval request: a second message"},
		ports.NotificationKindSlackWorkflowDecision:    {NotRepeatable, "chat.postMessage: a second message"},
		ports.NotificationKindSlackDigest:              {NotRepeatable, "chat.postMessage of the digest: a second message"},
		ports.NotificationKindSlackPlanDecided:         {Repeatable, "chat.update of a known message: the same text again"},
		ports.NotificationKindLinear:                   {NotRepeatable, "agentActivityCreate without an id: a second activity"},
		ports.NotificationKindLinearProgress:           {NotRepeatable, "agentActivityCreate without an id: a second thought"},
		ports.NotificationKindLinearWorkflowDecision:   {NotRepeatable, "agentActivityCreate without an id: a second activity"},
		ports.NotificationKindLinearDigest:             {Repeatable, "always returns its typed error, writes nothing"},
		ports.NotificationKindGitHub:                   {NotRepeatable, "BotNotifier's PostIssueComment: a second comment"},
		ports.NotificationKindGitHubWorkflowDecision:   {NotRepeatable, "BotNotifier's PostIssueComment: a second comment"},
		ports.NotificationKindGitHubVerdict:            {NotRepeatable, "CreateReview: a second formal review"},
		ports.NotificationKindHandoffSentinel:          {NotRepeatable, "ends in PostIssueComment: a second comment"},
		ports.NotificationKindReleaseManifest:          {NotRepeatable, "PostIssueComment: a second comment"},
		ports.NotificationKindGitHubPreviewLink:        {Repeatable, "a commit status per (context, sha) converges"},
		ports.NotificationKindGitHubDescriptionAutofix: {Repeatable, "RenderAutofixBody re-extracts the preserved original: the same body"},
		ports.NotificationKindGitHubReviewCheck:        {Repeatable, "finds and adopts its own run before creating one"},
		ports.NotificationKindSentinelAutoFix:          {Repeatable, "claim, branch and spawn in one transaction; a repeat finds the claim"},
		ports.NotificationKindRWXPreviewDispatch:       {NotRepeatable, "each dispatch starts a build"},
		ports.NotificationKindBlobDelete:               {Repeatable, "deleting an absent key succeeds"},
	}

	if len(notificationKindRepeatability) != len(want) {
		t.Errorf("the table classifies %d kinds, this test pins %d: pin the new kind's value here too", len(notificationKindRepeatability), len(want))
	}
	for kind, w := range want {
		got, ok := notificationKindRepeatability[kind]
		if !ok {
			t.Errorf("%q is missing from the repeatability table", kind)
			continue
		}
		if got != w.value {
			t.Errorf("%q repeatability = %d, want %d: %s", kind, got, w.value, w.reason)
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

// TestOutboxFreezeTable_EveryKindClassified is the same check for the
// autonomy freeze table (freeze.go, technical plan §40.2): every kind the
// port declares says whether its delivery is an automatic action the
// freeze holds, and the table names no kind the port does not declare.
func TestOutboxFreezeTable_EveryKindClassified(t *testing.T) {
	declared := notificationKindsDeclaredInSource(t)
	if len(declared) < 19 {
		t.Fatalf("found only %d NotificationKind constants in the source; the parse is broken, not the table", len(declared))
	}

	declaredValues := make(map[string]bool, len(declared))
	for _, name := range declared {
		declaredValues[name.value] = true
		if _, ok := notificationKindFreeze[ports.NotificationKind(name.value)]; !ok {
			t.Errorf("%s (%q) is declared in internal/app/ports/notifier.go but has no autonomy freeze classification.\n"+
				"    Say whether its delivery is itself an automatic action the freeze holds (§40.2),\n"+
				"    in notificationKindFreeze, and why.",
				name.constName, name.value)
		}
	}
	for kind := range notificationKindFreeze {
		if !declaredValues[string(kind)] {
			t.Errorf("the freeze table carries %q, which no NotificationKind constant declares any more", kind)
		}
	}
}

// TestOutboxFreezeTable_EveryKindsValue pins what the freeze does to every
// kind, not only its presence: exactly the sentinel auto-fix and the
// description rewrite hold, each counted under its own site, and every
// other kind delivers -- §40.2's "notifications about work already done
// still deliver". Flipping any one value fails here.
func TestOutboxFreezeTable_EveryKindsValue(t *testing.T) {
	holds := map[ports.NotificationKind]domainautonomy.Site{
		ports.NotificationKindSentinelAutoFix:          domainautonomy.SiteSentinelAutoFixSpawn,
		ports.NotificationKindGitHubDescriptionAutofix: domainautonomy.SiteDescriptionAutofix,
	}
	for _, kind := range allKindsForFreezeTest(t) {
		got := freezeOf(kind)
		if site, ok := holds[kind]; ok {
			if got.class != FreezeHolds || got.site != site {
				t.Errorf("%q = (class %d, site %q), want held under %q: its delivery is itself an automatic action", kind, got.class, got.site, site)
			}
			continue
		}
		if got.class != FreezeDelivers || got.site != "" {
			t.Errorf("%q = (class %d, site %q), want delivered with no site: a report of work already done is never held", kind, got.class, got.site)
		}
	}
}

// allKindsForFreezeTest is every kind the port declares, read from its
// source.
func allKindsForFreezeTest(t *testing.T) []ports.NotificationKind {
	t.Helper()
	var out []ports.NotificationKind
	for _, d := range notificationKindsDeclaredInSource(t) {
		out = append(out, ports.NotificationKind(d.value))
	}
	return out
}

// TestOutboxFreezeLanes_EveryDeclaredKindInOneLane pins the two lanes a
// frozen tick claims (Builder.claimBatch): heldKinds and deliveringKinds
// are equality lists, so together they must name every kind the port
// declares, each in exactly one lane -- a declared kind in neither would
// never be claimed while frozen, and one in both would be claimed twice.
func TestOutboxFreezeLanes_EveryDeclaredKindInOneLane(t *testing.T) {
	lane := map[string]string{}
	for _, l := range []struct {
		name  string
		kinds []string
	}{{"held", heldKinds()}, {"delivering", deliveringKinds()}} {
		for _, kind := range l.kinds {
			if prev, ok := lane[kind]; ok {
				t.Errorf("%q is in the %s lane and the %s lane", kind, prev, l.name)
			}
			lane[kind] = l.name
		}
	}
	for _, d := range notificationKindsDeclaredInSource(t) {
		if _, ok := lane[d.value]; !ok {
			t.Errorf("%s (%q) is in neither lane: a frozen tick would never claim it", d.constName, d.value)
		}
	}
	if got := heldKinds(); !slices.Equal(got, []string{"github_description_autofix", "sentinel_auto_fix"}) {
		t.Errorf("heldKinds() = %v, want the two kinds the freeze holds", got)
	}
}

// TestNewBuilder_RefusesAnUnclassifiedKind pins that NewBuilder applies
// classifyFreeze to its finished map: a registered kind with an egress and
// a repeatability classification but no freeze one is refused, never
// delivered unconsidered while frozen.
func TestNewBuilder_RefusesAnUnclassifiedKind(t *testing.T) {
	const kind = ports.NotificationKindSentinelAutoFix
	saved, ok := notificationKindFreeze[kind]
	if !ok {
		t.Fatalf("%q has no freeze classification to remove", kind)
	}
	delete(notificationKindFreeze, kind)
	t.Cleanup(func() { notificationKindFreeze[kind] = saved })

	_, err := NewBuilder(nil, nil, map[ports.NotificationKind]ports.Notifier{kind: nil}, platform.DefaultTimeouts(), &platform.ShutdownState{})
	if err == nil || !strings.Contains(err.Error(), string(kind)) || !strings.Contains(err.Error(), "autonomy freeze") {
		t.Fatalf("NewBuilder = %v, want a refusal naming %q and the freeze", err, kind)
	}
}

// TestLagSeconds pins what outbox_lag_seconds reads from one tick's
// claimed rows: the oldest row's age, zero for none -- and, while the
// autonomy freeze holds (§40.2), the oldest row of a kind that still
// delivers, so a held row aging with the freeze never reads as a stuck
// outbox.
func TestLagSeconds(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	row := func(kind ports.NotificationKind, age time.Duration) sqlcgen.Outbox {
		return sqlcgen.Outbox{Kind: string(kind), CreatedAt: pgtype.Timestamptz{Time: now.Add(-age), Valid: true}}
	}
	held := row(ports.NotificationKindSentinelAutoFix, 3*time.Hour)
	heldDescription := row(ports.NotificationKindGitHubDescriptionAutofix, 2*time.Hour)
	delivering := row(ports.NotificationKindSlack, 90*time.Second)
	for _, tc := range []struct {
		name    string
		claimed []sqlcgen.Outbox
		holding bool
		want    int64
	}{
		{name: "nothing claimed", want: 0},
		{name: "not frozen: the oldest row of any kind", claimed: []sqlcgen.Outbox{delivering, held, heldDescription}, want: 3 * 3600},
		{name: "frozen: held rows left out", claimed: []sqlcgen.Outbox{delivering, held, heldDescription}, holding: true, want: 90},
		{name: "frozen, only held rows: zero", claimed: []sqlcgen.Outbox{held, heldDescription}, holding: true, want: 0},
		{name: "a row created after now reads zero", claimed: []sqlcgen.Outbox{row(ports.NotificationKindSlack, -time.Minute)}, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lagSeconds(tc.claimed, tc.holding, now); got != tc.want {
				t.Fatalf("lagSeconds = %d, want %d", got, tc.want)
			}
		})
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
