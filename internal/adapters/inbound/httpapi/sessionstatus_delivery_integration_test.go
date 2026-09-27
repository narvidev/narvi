//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/restdtos"
	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// This file is review round 2's O1 on real Postgres, driven through a real
// session actor and read through the real status route (technical plan
// §43.20): a successful turn's push and pull request run AFTER the turn's
// own terminal commit -- the push command once execution_complete has
// committed, the pull request only when that push's push_complete arrives
// -- and while they run the session must not read settled, because a pull
// request appears with no new input from anyone. It must not stick either:
// a failed, shadow, unsendable or lost push reads finished.

// deliveryTokenKey is an obviously-fake 32-byte AES-256-GCM key for the
// creator's GitHub token, never a real secret.
var deliveryTokenKey = []byte("0123456789abcdef0123456789abcdef")

// deliverySourceControl is the ports.SourceControl the actor opens pull
// requests through. CreatePR can be held open (entered/release) so a test
// reads the status while the pull request is being created. The embedded
// port is nil: a method this file does not expect the actor to call
// panics rather than answering something made up.
type deliverySourceControl struct {
	ports.SourceControl

	mu          sync.Mutex
	createCalls int
	entered     chan struct{}
	release     chan struct{}
	ref         ports.PRRef
}

func (f *deliverySourceControl) ResolveBranchSHA(context.Context, ports.ResolveBranchSHASpec) (string, string, error) {
	return "0123456789abcdef0123456789abcdef01234567", "main", nil
}

func (f *deliverySourceControl) CheckRepoAccess(context.Context, ports.CheckRepoAccessSpec) (bool, error) {
	return true, nil
}

func (f *deliverySourceControl) ResolveContractsFingerprint(context.Context, ports.ResolveContractsFingerprintSpec) (string, bool, error) {
	return "", false, nil
}

func (f *deliverySourceControl) RegisterPRStack(context.Context, ports.RegisterPRStackSpec) error {
	return nil
}

func (f *deliverySourceControl) CreatePR(ctx context.Context, _ ports.CreatePRSpec) (ports.PRRef, error) {
	f.mu.Lock()
	f.createCalls++
	entered, release := f.entered, f.release
	f.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return ports.PRRef{}, ctx.Err()
		}
	}
	return f.ref, nil
}

func (f *deliverySourceControl) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCalls
}

// deliveryCommander records the commands the actor sends the sandbox, and
// fails every send when err is set.
type deliveryCommander struct {
	mu       sync.Mutex
	payloads []json.RawMessage
	err      error
}

func (c *deliveryCommander) SendCommand(_ string, payload json.RawMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads = append(c.payloads, payload)
	return c.err
}

func (c *deliveryCommander) sent() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.payloads)
}

// deliveryFixture is one session whose single turn is processing on a
// sandbox, owned by a member with a linked GitHub identity and a usable
// token, on a repository of its own -- armed live unless the case says
// shadow -- and a session actor with its own commander and source control.
type deliveryFixture struct {
	rig       testRig
	cookie    string
	sessionID pgtype.UUID
	turnID    pgtype.UUID
	actor     *sessionactor.Actor
	commander *deliveryCommander
	scm       *deliverySourceControl
}

func newDeliveryFixture(ctx context.Context, t *testing.T, rig testRig, live, withBranch bool, sendErr error) deliveryFixture {
	t.Helper()
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	encrypted, err := platform.EncryptToken(deliveryTokenKey, []byte("gh-fake-oauth-token"))
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	if _, err := rig.pool.Exec(ctx, `UPDATE identities SET access_token_encrypted = $2 WHERE user_id = $1 AND provider = 'github'`, user.ID, encrypted); err != nil {
		t.Fatalf("store the creator's GitHub token: %v", err)
	}

	repo := fmt.Sprintf("example/status-delivery-%d", time.Now().UnixNano())
	branch := `null`
	if withBranch {
		branch = `"feature-x"`
	}
	sess, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
		CreatedBy:   user.ID,
		Repos:       []byte(`[{"name":"repo1","url":"https://github.com/` + repo + `.git","branch":` + branch + `}]`),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if live {
		if _, err := narvipg.NewRepoSettingsStore(rig.pool).UpsertLiveEgressEnabled(ctx, repo, true); err != nil {
			t.Fatalf("arm live egress for %s: %v", repo, err)
		}
	}
	if _, err := rig.sandboxes.Create(ctx, sess.ID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	processing, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: sess.ID, Status: sqlcgen.TurnStatusProcessing})
	if err != nil {
		t.Fatalf("create processing turn: %v", err)
	}

	commander := &deliveryCommander{err: sendErr}
	scm := &deliverySourceControl{ref: ports.PRRef{Number: 7, URL: "https://github.com/" + repo + "/pull/7"}}
	registry, err := sessionactor.NewRegistry(ctx, rig.pool, platform.DefaultTimeouts(), nil, commander, nil, "", scm, deliveryTokenKey, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = registry.Shutdown() })
	actor, err := registry.GetOrSpawn(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	return deliveryFixture{rig: rig, cookie: cookie, sessionID: sess.ID, turnID: processing.ID, actor: actor, commander: commander, scm: scm}
}

// send delivers one wire event to the actor, with its own message id (the
// actor's redelivery dedup keys on it), and waits for the commit's reply.
func (f deliveryFixture) send(ctx context.Context, t *testing.T, eventType string, raw json.RawMessage, messageID string) {
	t.Helper()
	reply := make(chan sessionactor.SandboxEventOutcome, 1)
	if err := f.actor.Send(ctx, sessionactor.SandboxEvent{Type: eventType, Gen: 1, MessageID: messageID, Raw: raw, Reply: reply}); err != nil {
		t.Fatalf("send %s: %v", eventType, err)
	}
	select {
	case outcome := <-reply:
		if !outcome.Persisted {
			t.Fatalf("%s was not persisted", eventType)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no reply to %s", eventType)
	}
}

func (f deliveryFixture) executionComplete(ctx context.Context, t *testing.T) {
	t.Helper()
	id := uuid.NewString()
	raw, err := json.Marshal(sandboxws.ExecutionComplete{
		Type: "execution_complete", MessageId: id, SessionId: f.sessionID.String(), Gen: 1,
		AckId: "execution_complete:" + id, Outcome: sandboxws.ExecutionCompleteOutcomeCompleted,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.send(ctx, t, "execution_complete", raw, id)
}

func (f deliveryFixture) pushComplete(ctx context.Context, t *testing.T) {
	t.Helper()
	id := uuid.NewString()
	raw, err := json.Marshal(sandboxws.PushComplete{
		Type: "push_complete", MessageId: id, SessionId: f.sessionID.String(), Gen: 1, AckId: "push_complete:" + id,
		Repos: []sandboxws.PushCompleteReposElem{{Name: "repo1", Branch: "feature-x", Sha: "0123456789abcdef0123456789abcdef01234567"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.send(ctx, t, "push_complete", raw, id)
}

func (f deliveryFixture) pushError(ctx context.Context, t *testing.T) {
	t.Helper()
	id := uuid.NewString()
	raw, err := json.Marshal(sandboxws.PushError{
		Type: "push_error", MessageId: id, SessionId: f.sessionID.String(), Gen: 1, AckId: "push_error:" + id,
		Error: "git push: remote rejected",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.send(ctx, t, "push_error", raw, id)
}

func (f deliveryFixture) status(t *testing.T) restdtos.SessionActivity {
	t.Helper()
	return getStatus(t, f.rig, f.sessionID, f.cookie)
}

func (f deliveryFixture) prArtifacts(ctx context.Context, t *testing.T) int {
	t.Helper()
	rows, err := f.rig.artifacts.ListForSession(ctx, f.sessionID)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	n := 0
	for _, row := range rows {
		if row.Type == sqlcgen.ArtifactTypePr {
			n++
		}
	}
	return n
}

func (f deliveryFixture) stampSet(ctx context.Context, t *testing.T) bool {
	t.Helper()
	var set bool
	if err := f.rig.pool.QueryRow(ctx, `SELECT pr_delivery_started_at IS NOT NULL FROM sandboxes WHERE session_id = $1`, f.sessionID).Scan(&set); err != nil {
		t.Fatalf("read the delivery stamp: %v", err)
	}
	return set
}

// untilFinished reads the status until it says finished, failing past the
// deadline; every read before that must be delivering (never another
// settled answer).
func (f deliveryFixture) untilFinished(t *testing.T, stage string) restdtos.SessionActivity {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := f.status(t)
		switch got.Activity {
		case restdtos.SessionActivityActivityFinished:
			return got
		case restdtos.SessionActivityActivityDelivering:
		default:
			t.Fatalf("%s: activity %q settled %v, want delivering until finished", stage, got.Activity, got.Settled)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: still delivering after 10s", stage)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func wantFinished(t *testing.T, stage string, got restdtos.SessionActivity) {
	t.Helper()
	if got.Activity != restdtos.SessionActivityActivityFinished || !got.Settled || got.SuggestedDelaySeconds != 300 {
		t.Fatalf("%s: activity %q settled %v delay %d, want finished, settled, 300", stage, got.Activity, got.Settled, got.SuggestedDelaySeconds)
	}
}

func wantDelivering(t *testing.T, stage string, got restdtos.SessionActivity, turnID pgtype.UUID) {
	t.Helper()
	if got.Activity != restdtos.SessionActivityActivityDelivering || got.Settled || got.SuggestedDelaySeconds != 5 {
		t.Fatalf("%s: activity %q settled %v delay %d, want delivering, not settled, 5", stage, got.Activity, got.Settled, got.SuggestedDelaySeconds)
	}
	if got.PendingTurns != 0 || got.InFlightTurn != nil || got.LastRun == nil || got.LastRun.TurnId != turnID.String() || got.LastRun.Outcome != restdtos.SessionActivityLastRunOutcomeCompleted {
		t.Fatalf("%s: pendingTurns %d inFlightTurn %+v lastRun %+v, want no turn queued or running and the turn completed", stage, got.PendingTurns, got.InFlightTurn, got.LastRun)
	}
}

// TestGetSessionStatus_DeliveryAfterACompletedTurn is the reviewers'
// reproduction of O1, and every way that delivery can end.
func TestGetSessionStatus_DeliveryAfterACompletedTurn(t *testing.T) {
	rig := newTestRig(t)
	ctx := context.Background()

	// execution_complete(completed) reads delivering -- not settled -- until
	// push_complete has arrived AND the pull request has been created and
	// recorded; only then finished, with the pull request already listed.
	t.Run("delivering until the pull request is recorded, then finished", func(t *testing.T) {
		f := newDeliveryFixture(ctx, t, rig, true, true, nil)
		if got := f.status(t); got.Activity != restdtos.SessionActivityActivityRunning || got.Settled {
			t.Fatalf("before execution_complete: activity %q settled %v, want running", got.Activity, got.Settled)
		}

		f.executionComplete(ctx, t)
		wantDelivering(t, "the turn completed, its push under way", f.status(t), f.turnID)
		eventually(t, 5*time.Second, func() bool { return f.commander.sent() == 1 })
		wantDelivering(t, "the push sent, no push_complete yet", f.status(t), f.turnID)
		if n := f.prArtifacts(ctx, t); n != 0 || f.scm.calls() != 0 {
			t.Fatalf("before push_complete: %d pull request artifacts, %d CreatePR calls, want none", n, f.scm.calls())
		}

		f.scm.mu.Lock()
		f.scm.entered, f.scm.release = make(chan struct{}), make(chan struct{})
		f.scm.mu.Unlock()
		f.pushComplete(ctx, t)
		select {
		case <-f.scm.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("CreatePR was never called")
		}
		wantDelivering(t, "the pull request being created", f.status(t), f.turnID)
		if n := f.prArtifacts(ctx, t); n != 0 {
			t.Fatalf("while CreatePR runs: %d pull request artifacts, want none yet", n)
		}
		close(f.scm.release)

		got := f.untilFinished(t, "the pull request created")
		wantFinished(t, "the pull request created", got)
		if n := f.prArtifacts(ctx, t); n != 1 {
			t.Fatalf("finished: %d pull request artifacts, want the one just created listed before the status said finished", n)
		}
		if f.stampSet(ctx, t) {
			t.Fatal("finished: the delivery stamp is still set")
		}
	})

	// A failed push reports push_error, and no pull request follows: the
	// session reads finished as soon as that event commits, not once the
	// window runs out.
	t.Run("a failed push reads finished as soon as push_error arrives", func(t *testing.T) {
		f := newDeliveryFixture(ctx, t, rig, true, true, nil)
		f.executionComplete(ctx, t)
		eventually(t, 5*time.Second, func() bool { return f.commander.sent() == 1 })
		wantDelivering(t, "the push sent", f.status(t), f.turnID)

		f.pushError(ctx, t)
		wantFinished(t, "push_error committed", f.status(t))
		if f.stampSet(ctx, t) || f.scm.calls() != 0 {
			t.Fatalf("after push_error: stamp set %v, %d CreatePR calls, want cleared and none", f.stampSet(ctx, t), f.scm.calls())
		}
	})

	// A shadow repository's push is suppressed at the source (§30.9): no
	// command is sent and nothing comes back, so there is nothing to wait
	// for -- finished at once, never delivering.
	t.Run("a shadow push reads finished at once", func(t *testing.T) {
		f := newDeliveryFixture(ctx, t, rig, false, true, nil)
		f.executionComplete(ctx, t)
		wantFinished(t, "shadow: the turn completed", f.status(t))
		if f.stampSet(ctx, t) {
			t.Fatal("shadow: a delivery stamp was set for a push that is never sent")
		}
		time.Sleep(200 * time.Millisecond)
		if f.commander.sent() != 0 {
			t.Fatalf("shadow: %d commands sent, want none", f.commander.sent())
		}
		wantFinished(t, "shadow: settled for good", f.status(t))
	})

	// A push that never reports back -- its sandbox gone, its push_complete
	// lost -- stops counting once MCPStatusDeliveryWindow has passed since
	// it began, so it cannot hold the session unsettled for good.
	t.Run("a push that never reports back reads finished once the window has passed", func(t *testing.T) {
		f := newDeliveryFixture(ctx, t, rig, true, true, nil)
		f.executionComplete(ctx, t)
		eventually(t, 5*time.Second, func() bool { return f.commander.sent() == 1 })
		wantDelivering(t, "the push sent", f.status(t), f.turnID)

		window := platform.DefaultTimeouts().MCPStatusDeliveryWindow
		if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET pr_delivery_started_at = now() - $2::interval WHERE session_id = $1`,
			f.sessionID, pgtype.Interval{Microseconds: (window - time.Second).Microseconds(), Valid: true}); err != nil {
			t.Fatal(err)
		}
		wantDelivering(t, "a second inside the window", f.status(t), f.turnID)
		if _, err := rig.pool.Exec(ctx, `UPDATE sandboxes SET pr_delivery_started_at = now() - $2::interval WHERE session_id = $1`,
			f.sessionID, pgtype.Interval{Microseconds: (window + time.Second).Microseconds(), Valid: true}); err != nil {
			t.Fatal(err)
		}
		wantFinished(t, "the window passed", f.status(t))
	})

	// The push command cannot reach the sandbox: nothing will come back,
	// so the delivery ends there rather than when the window runs out.
	t.Run("a push that cannot be sent reads finished without waiting for the window", func(t *testing.T) {
		f := newDeliveryFixture(ctx, t, rig, true, true, errors.New("sandbox connection gone"))
		f.executionComplete(ctx, t)
		eventually(t, 5*time.Second, func() bool { return f.commander.sent() == 1 && !f.stampSet(ctx, t) })
		wantFinished(t, "the send failed", f.status(t))
	})

	// No repository names a branch: the push sends nothing (the actor skips
	// every repo), so there is no delivery to report.
	t.Run("no repo with a branch reads finished at once", func(t *testing.T) {
		f := newDeliveryFixture(ctx, t, rig, true, false, nil)
		f.executionComplete(ctx, t)
		wantFinished(t, "no branch: the turn completed", f.status(t))
		if f.stampSet(ctx, t) {
			t.Fatal("no branch: a delivery stamp was set for a push that sends nothing")
		}
	})
}

// eventually polls cond until it holds, failing the test past timeout.
func eventually(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %v", timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
