package sessionactor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/domain/reviewcheck"
	"github.com/narvidev/narvi/internal/domain/turn"
	"github.com/narvidev/narvi/internal/platform"
)

const (
	unitHead = "0123456789abcdef0123456789abcdef01234567"
	unitTip  = "89abcdef0123456789abcdef0123456789abcdef"
)

// TestReadyAdvertisesReviewCheckout pins how a ready's review-checkout
// capability is read (technical plan §21.1): only an explicit true counts,
// and a ready that fails its schema decode counts as not advertising it.
func TestReadyAdvertisesReviewCheckout(t *testing.T) {
	t.Parallel()

	const base = `"type":"ready","messageId":"r1","sessionId":"s","gen":3,"timestamp":"2026-10-01T12:00:00Z","agentVersion":"dev","imageDigest":"unknown"`
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{name: "advertised", raw: `{` + base + `,"capabilities":{"promptReceipt":true,"reviewCheckout":true}}`, want: true},
		{name: "advertised false", raw: `{` + base + `,"capabilities":{"reviewCheckout":false}}`, want: false},
		{name: "capabilities without it", raw: `{` + base + `,"capabilities":{"promptReceipt":true}}`, want: false},
		{name: "an agent that predates capabilities", raw: `{` + base + `}`, want: false},
		{name: "fails its schema decode", raw: `{"type":"ready","messageId":"r1","gen":3,"capabilities":{"reviewCheckout":true}}`, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := readyAdvertisesReviewCheckout(json.RawMessage(tc.raw)); got != tc.want {
				t.Errorf("readyAdvertisesReviewCheckout(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// TestReviewCheckoutCapable: the capability counts only for the gen it was
// recorded against, so no respawn, restore or resume inherits it.
func TestReviewCheckoutCapable(t *testing.T) {
	t.Parallel()

	gen := func(v int32) *int32 { return &v }
	for _, tc := range []struct {
		name string
		row  sqlcgen.Sandbox
		want bool
	}{
		{name: "recorded for the live gen", row: sqlcgen.Sandbox{Gen: 4, ReviewCheckoutGen: gen(4)}, want: true},
		{name: "recorded for an earlier gen", row: sqlcgen.Sandbox{Gen: 5, ReviewCheckoutGen: gen(4)}, want: false},
		{name: "never recorded", row: sqlcgen.Sandbox{Gen: 1}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := reviewCheckoutCapable(tc.row); got != tc.want {
				t.Errorf("reviewCheckoutCapable(%+v) = %v, want %v", tc.row, got, tc.want)
			}
		})
	}
}

// TestReviewCheckoutTargetFor pins which turns are checked out: a session
// that claims a pull request, a turn that recorded a full commit id, and a
// primary repo whose url names the claim's repository -- the ref
// SESSION_CONFIG gives the sandbox. Everything else dispatches as before.
func TestReviewCheckoutTargetFor(t *testing.T) {
	t.Parallel()

	claim := &sqlcgen.GithubPrSession{RepoFullName: "acme/widgets", PrNumber: 7}
	repos := func(url string) []byte {
		raw, err := json.Marshal([]map[string]any{{"name": "widgets", "url": url, "branch": nil}, {"name": "docs", "url": "https://github.com/acme/docs.git", "branch": "main"}})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	head := unitHead
	empty := ""
	upper := strings.ToUpper(unitHead)
	short := unitHead[:12]
	for _, tc := range []struct {
		name       string
		claim      *sqlcgen.GithubPrSession
		repos      []byte
		head       *string
		want       reviewCheckoutTarget
		wantOK     bool
		wantBadSHA bool
		wantErr    bool
	}{
		{name: "the base repository's review session", claim: claim, repos: repos("https://github.com/acme/widgets.git"), head: &head,
			want: reviewCheckoutTarget{repoName: "widgets", ref: "refs/pull/7/head", sha: unitHead}, wantOK: true},
		{name: "names the repository in another case", claim: claim, repos: repos("https://github.com/ACME/Widgets"), head: &head,
			want: reviewCheckoutTarget{repoName: "widgets", ref: "refs/pull/7/head", sha: unitHead}, wantOK: true},
		{name: "a legacy session whose spec names the fork", claim: claim, repos: repos("https://github.com/contributor/widgets.git"), head: &head},
		{name: "no claim", repos: repos("https://github.com/acme/widgets.git"), head: &head},
		{name: "no recorded head", claim: claim, repos: repos("https://github.com/acme/widgets.git")},
		{name: "an empty recorded head", claim: claim, repos: repos("https://github.com/acme/widgets.git"), head: &empty},
		{name: "a head that is not lowercase hex", claim: claim, repos: repos("https://github.com/acme/widgets.git"), head: &upper, wantBadSHA: true},
		{name: "an abbreviated head", claim: claim, repos: repos("https://github.com/acme/widgets.git"), head: &short, wantBadSHA: true},
		{name: "no repos", claim: claim, repos: []byte(`[]`), head: &head},
		{name: "repos that do not decode", claim: claim, repos: []byte(`{`), head: &head, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok, badSHA, err := reviewCheckoutTargetFor(tc.claim, tc.repos, tc.head)
			if (err != nil) != tc.wantErr || ok != tc.wantOK || badSHA != tc.wantBadSHA || got != tc.want {
				t.Errorf("reviewCheckoutTargetFor = %+v, ok %v, bad sha %v, err %v; want %+v, %v, %v, err %v",
					got, ok, badSHA, err, tc.want, tc.wantOK, tc.wantBadSHA, tc.wantErr)
			}
		})
	}
}

// TestDecodeCheckoutReply pins how a stored checkout_result is read: the
// entry for the turn's repo, its outcome read as text -- an open enum --
// and a reply that does not decode, or names no entry for the repo, read
// as failed, never as holding the head.
func TestDecodeCheckoutReply(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		raw  string
		want *turn.CheckoutReply
	}{
		{name: "none stored", raw: ``, want: nil},
		{name: "checked out", raw: `{"type":"checkout_result","repos":[{"name":"docs","outcome":"failed","headSha":null,"refSha":null,"error":"x"},{"name":"widgets","outcome":"checked_out","headSha":"` + unitHead + `","refSha":"` + unitTip + `","error":null}]}`,
			want: &turn.CheckoutReply{Outcome: turn.CheckoutCheckedOut, HeadSHA: unitHead, RefSHA: unitTip}},
		{name: "an outcome this binary does not know, kept as text", raw: `{"repos":[{"name":"widgets","outcome":"rewound","headSha":null,"refSha":null,"error":"?"}]}`,
			want: &turn.CheckoutReply{Outcome: "rewound", Error: "?"}},
		{name: "no entry for the repo", raw: `{"repos":[{"name":"docs","outcome":"checked_out","headSha":"` + unitHead + `","refSha":null,"error":null}]}`,
			want: &turn.CheckoutReply{Outcome: turn.CheckoutFailed, Error: `the sandbox's checkout_result names no repo "widgets"`}},
		{name: "does not decode", raw: `{"repos":"widgets"}`,
			want: &turn.CheckoutReply{Outcome: turn.CheckoutFailed, Error: "the sandbox's checkout_result does not decode"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := decodeCheckoutReply([]byte(tc.raw), "widgets")
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Errorf("decodeCheckoutReply = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestBuildCheckoutPayload: the command the actor sends passes the
// contract's own decoder -- the agent refuses one that does not -- and
// names the one repo, ref and commit asked for.
func TestBuildCheckoutPayload(t *testing.T) {
	t.Parallel()

	raw, err := BuildCheckoutPayload("3f0b7c9e-8f39-4a4b-9a3c-5d1b2c3e4f50", 6, "msg-1", "widgets", "refs/pull/7/head", unitHead)
	if err != nil {
		t.Fatal(err)
	}
	var cmd sandboxws.Checkout
	if err := json.Unmarshal(raw, &cmd); err != nil {
		t.Fatalf("the checkout command fails its contract: %v (%s)", err, raw)
	}
	if cmd.Type != "checkout" || cmd.MessageId != "msg-1" || cmd.Gen != 6 || len(cmd.Repos) != 1 ||
		cmd.Repos[0] != (sandboxws.CheckoutReposElem{Name: "widgets", Ref: "refs/pull/7/head", Sha: unitHead}) {
		t.Fatalf("command = %+v", cmd)
	}
}

// TestCheckoutRefusal pins what each refusal says: the cause, with the
// commit and the ref where they matter, the remedy in the session warning,
// and the review check's reason.
func TestCheckoutRefusal(t *testing.T) {
	t.Parallel()

	a := &Actor{timeouts: platform.DefaultTimeouts()}
	target := reviewCheckoutTarget{repoName: "widgets", ref: "refs/pull/7/head", sha: unitHead}
	for _, tc := range []struct {
		name        string
		verdict     turn.CheckoutVerdict
		reason      []string
		warning     []string
		notAssessed reviewcheck.NotAssessedReason
		counted     string
	}{
		{name: "an agent too old", verdict: turn.CheckoutVerdict{Refusal: turn.CheckoutRefusedUnsupported},
			reason:      []string{"cannot check out the commit this review was asked about"},
			warning:     []string{"Rebuild the sandbox image so it carries a current agent", "request the review again"},
			notAssessed: reviewcheck.NotAssessedReviewCheckoutUnsupported, counted: reviewCheckoutOutcomeUnsupported},
		{name: "no report", verdict: turn.CheckoutVerdict{Refusal: turn.CheckoutRefusedNoReport},
			reason:      []string{"did not report its checkout of " + unitHead + " within 15m0s"},
			warning:     []string{"This review was not run"},
			notAssessed: reviewcheck.NotAssessedReviewCheckoutFailed, counted: reviewCheckoutOutcomeNoReport},
		{name: "the head is gone", verdict: turn.CheckoutVerdict{Refusal: turn.CheckoutRefusedHeadAbsent, RefSHA: unitTip},
			reason:      []string{unitHead, "refs/pull/7/head", "whose tip is " + unitTip},
			warning:     []string{"Ask again for the pull request's head as it is now"},
			notAssessed: reviewcheck.NotAssessedReviewCheckoutFailed, counted: reviewCheckoutOutcomeHeadAbsent},
		{name: "a fetch that kept failing names the App", verdict: turn.CheckoutVerdict{Refusal: turn.CheckoutRefusedError, Outcome: turn.CheckoutFetchFailed, Error: "Authentication failed"},
			reason:      []string{"did not check out " + unitHead + " from refs/pull/7/head within 15m0s", "fetch_failed: Authentication failed"},
			warning:     []string{"install the App on that repository, with read access to its contents"},
			notAssessed: reviewcheck.NotAssessedReviewCheckoutFailed, counted: reviewCheckoutOutcomeError},
		{name: "a checkout that kept failing", verdict: turn.CheckoutVerdict{Refusal: turn.CheckoutRefusedError, Outcome: turn.CheckoutFailed, Error: "index.lock: File exists"},
			reason:      []string{"failed: index.lock: File exists"},
			warning:     []string{"This review was not run"},
			notAssessed: reviewcheck.NotAssessedReviewCheckoutFailed, counted: reviewCheckoutOutcomeError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := a.checkoutRefusal(target, tc.verdict)
			for _, want := range tc.reason {
				if !strings.Contains(got.reason, want) {
					t.Errorf("reason = %q, want it to contain %q", got.reason, want)
				}
			}
			for _, want := range tc.warning {
				if !strings.Contains(got.warning, want) {
					t.Errorf("warning = %q, want it to contain %q", got.warning, want)
				}
			}
			if got.notAssessed != tc.notAssessed || got.counted != tc.counted {
				t.Errorf("not assessed %q, counted %q; want %q, %q", got.notAssessed, got.counted, tc.notAssessed, tc.counted)
			}
		})
	}
}
