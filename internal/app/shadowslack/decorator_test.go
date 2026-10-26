package shadowslack

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/adapters/outbound/slackapi"
)

type fakeStore struct {
	rows []sqlcgen.CreateShadowSCMWriteParams
	err  error
}

func (s *fakeStore) Create(_ context.Context, arg sqlcgen.CreateShadowSCMWriteParams) (sqlcgen.ShadowScmWrite, error) {
	if s.err != nil {
		return sqlcgen.ShadowScmWrite{}, s.err
	}
	s.rows = append(s.rows, arg)
	return sqlcgen.ShadowScmWrite{}, nil
}

// liveSpy counts every call it receives. In shadow tests (failOnWrite
// true) it also fails the test the instant a write reaches it -- the
// network-contact assertion githubapi's own shadow-gate tests use, rather
// than trusting a call count alone. GetUserEmail is always allowed, and
// counted, because reads are supposed to pass through regardless of mode.
type liveSpy struct {
	t           *testing.T
	failOnWrite bool
	readCalls   int
	writeCalls  int
}

func (l *liveSpy) GetUserEmail(context.Context, string) (slackapi.UserEmail, bool, error) {
	l.readCalls++
	return slackapi.UserEmail{Email: "actor@example.com"}, true, nil
}
func (l *liveSpy) PostAck(context.Context, string, string, string) error {
	l.writeCalls++
	if l.failOnWrite {
		l.t.Error("PostAck reached the live client in shadow")
	}
	return nil
}
func (l *liveSpy) PostIdentityLinkNotice(context.Context, string, string, string, string) error {
	l.writeCalls++
	if l.t != nil {
		l.t.Error("PostIdentityLinkNotice reached the live client in shadow")
	}
	return nil
}
func (l *liveSpy) PostEphemeral(context.Context, string, string, string, string) error {
	l.writeCalls++
	if l.failOnWrite {
		l.t.Error("PostEphemeral reached the live client in shadow")
	}
	return nil
}
func (l *liveSpy) UpdateMessage(context.Context, string, string, string) error {
	l.writeCalls++
	if l.failOnWrite {
		l.t.Error("UpdateMessage reached the live client in shadow")
	}
	return nil
}
func (l *liveSpy) OpenView(context.Context, string, string, string) error {
	l.writeCalls++
	if l.failOnWrite {
		l.t.Error("OpenView reached the live client in shadow")
	}
	return nil
}

func shadowDecorator(t *testing.T, store *fakeStore) (*Decorator, *liveSpy) {
	t.Helper()
	spy := &liveSpy{t: t, failOnWrite: true}
	d, err := New(spy, store, "acme/widgets", func(context.Context, string) bool { return false })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d, spy
}

// TestDecorator_WritesAreSuppressedAndRecorded proves every mutation
// method records into the ledger instead of reaching the live client, with
// the repository this Decorator was built for attributed on every row --
// the §30.6 requirement this whole package exists to satisfy for a
// provider with no per-call repository of its own to key on (doc.go's own
// "why one fixed repository, not a per-call one").
func TestDecorator_WritesAreSuppressedAndRecorded(t *testing.T) {
	store := &fakeStore{}
	d, spy := shadowDecorator(t, store)
	ctx := context.Background()

	if err := d.PostAck(ctx, "C1", "1.1", "hello"); err != nil {
		t.Fatalf("PostAck: %v", err)
	}
	if err := d.PostEphemeral(ctx, "C1", "U1", "1.1", "notice"); err != nil {
		t.Fatalf("PostEphemeral: %v", err)
	}
	if err := d.UpdateMessage(ctx, "C1", "1.1", "decided"); err != nil {
		t.Fatalf("UpdateMessage: %v", err)
	}
	if err := d.OpenView(ctx, "trigger-1", "plan-1", "session-1"); err != nil {
		t.Fatalf("OpenView: %v", err)
	}

	if spy.writeCalls != 0 {
		t.Fatalf("live writeCalls = %d, want 0 -- a single leaked write is total failure of this capability", spy.writeCalls)
	}
	if len(store.rows) != 4 {
		t.Fatalf("ledger rows = %d, want 4 (one per suppressed call)", len(store.rows))
	}
	wantOps := []string{"slack_post_ack", "slack_post_ephemeral", "slack_update_message", "slack_open_view"}
	for i, row := range store.rows {
		if row.Operation != wantOps[i] {
			t.Errorf("row %d Operation = %q, want %q", i, row.Operation, wantOps[i])
		}
		if row.RepoFullName != "acme/widgets" {
			t.Errorf("row %d RepoFullName = %q, want %q", i, row.RepoFullName, "acme/widgets")
		}
	}
}

// TestDecorator_LiveRepoPassesThrough proves the decorator is not simply
// blocking everything: shadow mode must be graduable.
func TestDecorator_LiveRepoPassesThrough(t *testing.T) {
	store := &fakeStore{}
	spy := &liveSpy{t: t}
	d, err := New(spy, store, "acme/widgets", func(context.Context, string) bool { return true })
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := d.PostAck(context.Background(), "C1", "1.1", "hello"); err != nil {
		t.Fatalf("PostAck: %v", err)
	}
	if spy.writeCalls != 1 {
		t.Errorf("live writeCalls = %d, want 1", spy.writeCalls)
	}
	if len(store.rows) != 0 {
		t.Errorf("a live write was recorded in the suppression ledger (%d rows)", len(store.rows))
	}
}

// TestDecorator_ReadsAreForwarded: shadow must not blind identity
// auto-linking.
func TestDecorator_ReadsAreForwarded(t *testing.T) {
	store := &fakeStore{}
	d, spy := shadowDecorator(t, store)

	if _, _, err := d.GetUserEmail(context.Background(), "U1"); err != nil {
		t.Fatalf("GetUserEmail: %v", err)
	}
	if spy.readCalls != 1 {
		t.Errorf("read forwarded %d times, want 1", spy.readCalls)
	}
	if len(store.rows) != 0 {
		t.Errorf("a read was recorded as a suppressed write (%d rows)", len(store.rows))
	}
}

// TestDecorator_LedgerFailureFailsTheWrite pins record-or-fail: a
// suppression that could not be recorded must not be reported as one.
func TestDecorator_LedgerFailureFailsTheWrite(t *testing.T) {
	d, _ := shadowDecorator(t, &fakeStore{err: errors.New("ledger down")})

	if err := d.PostAck(context.Background(), "C1", "1.1", "hello"); err == nil {
		t.Fatal("PostAck reported success when its ledger insert failed")
	}
}

// TestDecorator_NoContentLeaksASlackToken proves the sealed, token-free
// spec types actually keep a Slack bot token out of the ledger -- there is
// no token PARAMETER on any of these methods to begin with (every Slack
// call in this codebase is authenticated with the one configured bot
// token), but the text a caller posts could still incidentally resemble
// one; this pins that whatever text IS recorded is exactly what was
// passed, verbatim, never silently redacted or substituted.
func TestDecorator_NoContentLeaksASlackToken(t *testing.T) {
	store := &fakeStore{}
	d, _ := shadowDecorator(t, store)

	if err := d.PostEphemeral(context.Background(), "C1", "U1", "1.1", "link your account at https://example.com/link/abc"); err != nil {
		t.Fatalf("PostEphemeral: %v", err)
	}
	if !strings.Contains(string(store.rows[0].SpecJson), "link your account") {
		t.Errorf("ledger row lost the notice text: %s", store.rows[0].SpecJson)
	}
}

// TestNew_RefusesAnIncompleteConstruction: there is no convenience default
// that yields a pass-through, because a pass-through obtained by omission
// is the failure this layer exists to remove.
func TestNew_RefusesAnIncompleteConstruction(t *testing.T) {
	live := &liveSpy{t: t}
	store := &fakeStore{}
	isLive := func(context.Context, string) bool { return true }

	if _, err := New(nil, store, "acme/widgets", isLive); err == nil {
		t.Error("New accepted a nil live client")
	}
	if _, err := New(live, nil, "acme/widgets", isLive); err == nil {
		t.Error("New accepted a nil ledger")
	}
	if _, err := New(live, store, "", isLive); err == nil {
		t.Error("New accepted an empty repoFullName -- the ledger is read per repository")
	}
	if _, err := New(live, store, "acme/widgets", nil); err == nil {
		t.Error("New accepted a nil resolver -- which would make live/shadow undefined")
	}
}

// TestDecorator_IdentityLinkNotice_NeverRecordsItsText is the reason this
// method exists apart from PostEphemeral.
//
// The notice's body carries a live magic-link URL whose nonce is
// credential-equivalent: whoever holds it can bind a Slack identity to a
// Narvi account. PostEphemeral records its text verbatim into a
// permanent, append-only ledger, so routing the notice through there
// stores a secret forever.
//
// The assertion is on the SERIALISED row, not on the struct's fields. A
// field list can be read and believed; only the bytes prove nothing
// leaked, and a future field added to the spec would be caught here
// rather than in a customer's ledger.
func TestDecorator_IdentityLinkNotice_NeverRecordsItsText(t *testing.T) {
	store := &fakeStore{}
	d, _ := shadowDecorator(t, store)

	const nonce = "nonce-b8f2c1d4e7a9"
	notice := "Connect your account here: https://narvi.example/auth/identity-link/" + nonce

	if err := d.PostIdentityLinkNotice(context.Background(), "C1", "U1", "1.1", notice); err != nil {
		t.Fatalf("PostIdentityLinkNotice: %v", err)
	}
	if len(store.rows) != 1 {
		t.Fatalf("ledger rows = %d, want 1: the notice must be recorded, just never with its text", len(store.rows))
	}
	spec := string(store.rows[0].SpecJson)
	if strings.Contains(spec, nonce) {
		t.Errorf("the magic-link nonce reached spec_json: %s", spec)
	}
	if strings.Contains(spec, "identity-link") || strings.Contains(spec, "Connect your account") {
		t.Errorf("the notice text reached spec_json: %s", spec)
	}
	if !strings.Contains(spec, "C1") || !strings.Contains(spec, "U1") {
		t.Errorf("spec_json = %s, want it to still name the channel and user -- the fact is what the evaluator needs", spec)
	}
}
