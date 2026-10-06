package sessionguard_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/domain/sessionguard"
)

func usd(t *testing.T, s string) *sessionguard.MicroUSD {
	t.Helper()
	v, err := sessionguard.ParseMicroUSD(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return &v
}

var (
	sessionA = [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	sessionB = [16]byte{0xff}
	repoSrc  = sessionguard.CapSource{Kind: sessionguard.CapSourceRepo, Name: "acme/widgets", ID: "acme/widgets"}
	autoSrc  = sessionguard.CapSource{Kind: sessionguard.CapSourceAutomation, Name: "nightly", ID: "4f9c0a3e-0000-0000-0000-000000000001"}
)

// TestDecide_SpendCap is the cap's decision table: at or past the effective
// cap refuses, below it admits, for every origin -- the cap binds every
// turn regardless of who asked (§40.1's inversion of §24.6) -- and to the
// micro-dollar.
func TestDecide_SpendCap(t *testing.T) {
	t.Parallel()
	origins := []sessionguard.Origin{sessionguard.OriginPerson, sessionguard.OriginAutomatic, sessionguard.OriginDispatch}
	cases := []struct {
		name       string
		spent      string
		autoCap    string
		repoCap    string
		wantRefuse bool
		wantCap    string
		wantSource sessionguard.CapSource
	}{
		{name: "no cap admits any spend", spent: "1000000"},
		{name: "one micro-dollar below the cap admits", spent: "24.999999", repoCap: "25.00"},
		{name: "exactly at the cap refuses", spent: "25.000000", repoCap: "25.00", wantRefuse: true, wantCap: "25", wantSource: repoSrc},
		{name: "past the cap refuses", spent: "25.000001", repoCap: "25.00", wantRefuse: true, wantCap: "25", wantSource: repoSrc},
		{name: "nothing spent is below any cap", spent: "0", repoCap: "0.01"},
		{name: "the automation's cap wins below the repository's", spent: "5", autoCap: "5.00", repoCap: "50.00", wantRefuse: true, wantCap: "5", wantSource: autoSrc},
		{name: "the automation's cap wins above the repository's", spent: "30", autoCap: "40.00", repoCap: "25.00"},
		{name: "the repository's cap applies when the automation sets none", spent: "30", repoCap: "25.00", wantRefuse: true, wantCap: "25", wantSource: repoSrc},
	}
	for _, tc := range cases {
		for _, origin := range origins {
			t.Run(fmt.Sprintf("%s/%s", tc.name, origin), func(t *testing.T) {
				t.Parallel()
				f := sessionguard.Facts{SessionID: sessionA, SpentUSD: *usd(t, tc.spent), Turns: 4}
				if tc.autoCap != "" {
					f.AutomationCap, f.AutomationSource = usd(t, tc.autoCap), autoSrc
				}
				if tc.repoCap != "" {
					f.RepoCap, f.RepoSource = usd(t, tc.repoCap), repoSrc
				}
				admission, refusal := sessionguard.Decide(f, origin)
				if !tc.wantRefuse {
					if refusal != nil {
						t.Fatalf("refused: %v", refusal)
					}
					if !admission.Admits(sessionA) {
						t.Fatal("the admission does not admit its own session")
					}
					if admission.Admits(sessionB) {
						t.Fatal("the admission admits another session")
					}
					return
				}
				if refusal == nil {
					t.Fatal("admitted, want refused")
				}
				if admission.Admits(sessionA) {
					t.Fatal("a refusal came with an admission")
				}
				if refusal.Reason != sessionguard.ReasonSpendCap || refusal.Cap != *usd(t, tc.wantCap) || refusal.Spent != f.SpentUSD || refusal.Source != tc.wantSource || refusal.SessionID != sessionA || refusal.Turns != f.Turns {
					t.Fatalf("refusal = %+v", *refusal)
				}
				if !errors.Is(refusal, sessionguard.ErrSpendCapReached) {
					t.Fatal("the refusal does not unwrap to ErrSpendCapReached")
				}
				wrapped := fmt.Errorf("create turn: %w", refusal)
				got, ok := sessionguard.AsRefusal(wrapped)
				if !ok || got != refusal {
					t.Fatal("AsRefusal does not find the refusal through a wrap")
				}
			})
		}
	}
}

// TestAdmission_ZeroAndNewSession: the zero Admission admits nothing, not
// even the zero session id; AdmitNewSession admits its own session only.
func TestAdmission_ZeroAndNewSession(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		admission sessionguard.Admission
		session   [16]byte
		want      bool
	}{
		{"zero admits a session", sessionguard.Admission{}, sessionA, false},
		{"zero admits the zero id", sessionguard.Admission{}, [16]byte{}, false},
		{"new session admits itself", sessionguard.AdmitNewSession(sessionA), sessionA, true},
		{"new session admits no other", sessionguard.AdmitNewSession(sessionA), sessionB, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.admission.Admits(tc.session); got != tc.want {
				t.Fatalf("Admits = %v, want %v", got, tc.want)
			}
		})
	}
	if _, ok := sessionguard.AsRefusal(errors.New("other")); ok {
		t.Fatal("AsRefusal found a refusal in an unrelated error")
	}
}

// TestWarningKey: one key per crossing -- the same for every refusal of
// the same session against the same cap from the same source while it is
// admitted nothing, whatever was spent meanwhile, and a new one when the
// cap or its source changes, or when the session has since been admitted a
// turn, even at a cap value it crossed before.
func TestWarningKey(t *testing.T) {
	t.Parallel()
	base := sessionguard.Refusal{Reason: sessionguard.ReasonSpendCap, SessionID: sessionA, Cap: 25_000_000, Spent: 25_000_000, Source: repoSrc, Turns: 7}
	same := base
	same.Spent = 31_400_000
	raised := base
	raised.Cap = 50_000_000
	moved := base
	moved.Source = autoSrc
	other := base
	other.SessionID = sessionB
	againAfterATurn := base
	againAfterATurn.Turns = 8
	againAfterATurn.Spent = 26_000_000

	if sessionguard.WarningKey(base) != sessionguard.WarningKey(same) {
		t.Fatal("two refusals of one crossing have different keys")
	}
	for name, r := range map[string]sessionguard.Refusal{"raised cap": raised, "moved cap": moved, "other session": other, "same cap, crossed again after a turn": againAfterATurn} {
		if sessionguard.WarningKey(base) == sessionguard.WarningKey(r) {
			t.Errorf("%s: the key did not change", name)
		}
	}
	if want := "urn:narvi:session-guard:spend_cap:01020304-0506-0708-090a-0b0c0d0e0f10:25000000:repo:acme/widgets:7"; sessionguard.WarningKey(base) != want {
		t.Fatalf("key = %q, want %q", sessionguard.WarningKey(base), want)
	}
}

// TestText_SpendCap: the refusal's text names the spend, the cap and where
// it was set, the overshoot a running turn can add, the lower bound, that
// the session has not failed, and the remedy -- and makes no claim that is
// false on some path that emits it: none that a turn reached the cap, which
// a cap set below the recorded spend never has, none that nothing failed,
// which a workflow step the guard stops contradicts, and none about a
// sandbox, which a session may not have.
func TestText_SpendCap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		source sessionguard.CapSource
		want   string
	}{
		{"repository", repoSrc, "set on repository acme/widgets"},
		{"automation", autoSrc, `set on the automation "nightly"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := sessionguard.Refusal{Reason: sessionguard.ReasonSpendCap, SessionID: sessionA, Cap: 1_000_000, Spent: 1_400_000, Source: tc.source}
			text := sessionguard.Text(r)
			for _, want := range []string{
				"it has spent $1.40, at or past its spend cap of $1.00",
				tc.want,
				"A turn already running when a session reaches its cap is left to finish and its cost is counted, so the spend shown can be past the cap",
				"lower bound of the bill",
				"The session itself has not failed",
				"An administrator can raise the cap",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("text %q does not contain %q", text, want)
				}
			}
			for _, claim := range []string{"The turn that reached the cap", "Nothing has failed", "sandbox", "by up to"} {
				if strings.Contains(text, claim) {
					t.Errorf("text %q claims %q, which is false on some path that emits it", text, claim)
				}
			}
			if r.Error() != text {
				t.Error("Error() is not Text()")
			}
		})
	}
}
