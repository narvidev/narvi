//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	"github.com/narvidev/narvi/internal/adapters/inbound/auth"
	"github.com/narvidev/narvi/internal/adapters/inbound/httpapi"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/app/sessionactor"
	"github.com/narvidev/narvi/internal/platform"
)

// This file pins technical plan §43.21's rule for a plan revision queued
// behind an approved implementation, on real Postgres through a real
// session actor: where an ingress queues one -- CreateTurnForBot, the only
// caller of AlwaysQueue -- the revision waits, and never withdraws the
// authorization the running work holds. The approval is that
// authorization: plan v1 is approved (a terminal status) and the
// implementation turn was inserted by the approval itself; nothing the
// revision writes is read by that turn's dispatch, its completion or its
// delivery. So the implementation runs to its end and its branch is pushed
// and its pull request opened; only then is the revision dispatched, and
// when it completes it writes v2 awaiting approval while v1 stays approved.

// ofType returns every command of type typ the actor sent, in order.
func (c *deliveryCommander) ofType(t *testing.T, typ string) []json.RawMessage {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []json.RawMessage
	for _, p := range c.payloads {
		var env struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(p, &env); err != nil {
			t.Fatalf("decode command %s: %v", p, err)
		}
		if env.Type == typ {
			out = append(out, p)
		}
	}
	return out
}

// promptTexts is the text of every prompt the actor dispatched, in order.
func (c *deliveryCommander) promptTexts(t *testing.T) []string {
	t.Helper()
	var texts []string
	for _, raw := range c.ofType(t, "prompt") {
		var p sandboxws.Prompt
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode prompt %s: %v", raw, err)
		}
		texts = append(texts, p.Text)
	}
	return texts
}

// holds fails the test if cond stops holding at any point during d: for
// what must NOT happen after an asynchronous nudge (a dispatch the actor
// could otherwise make shortly after the call returned).
func holds(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !cond() {
			t.Fatalf("%s stopped holding", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// queuedRevisionRig is one plan-mode session, owned by a member with a
// linked code-host identity and a usable token, on a repository of its own
// armed live with an explicit branch, whose plan v1 awaits approval --
// served by a session actor with its own recording commander and source
// control, and an approve route built on that actor's registry.
type queuedRevisionRig struct {
	deliveryFixture
	plan     sqlcgen.Plan
	registry *sessionactor.Registry
	approve  *httptest.Server
}

func newQueuedRevisionRig(ctx context.Context, t *testing.T, rig testRig, sandboxReady bool) queuedRevisionRig {
	t.Helper()
	user, cookie := createUserWithRole(ctx, t, rig, sqlcgen.UserRoleMember)
	encrypted, err := platform.EncryptToken(deliveryTokenKey, []byte("gh-fake-oauth-token"))
	if err != nil {
		t.Fatalf("EncryptToken: %v", err)
	}
	if _, err := rig.pool.Exec(ctx, `UPDATE identities SET access_token_encrypted = $2 WHERE user_id = $1 AND provider = 'github'`, user.ID, encrypted); err != nil {
		t.Fatalf("store the creator's code-host token: %v", err)
	}
	repo := fmt.Sprintf("example/queued-revision-%d", time.Now().UnixNano())
	sess, err := rig.sessions.Create(ctx, sqlcgen.CreateSessionParams{
		SpawnSource: sqlcgen.SessionSpawnSourceWeb,
		CreatedBy:   user.ID,
		Repos:       []byte(`[{"name":"repo1","url":"https://github.com/` + repo + `.git","branch":"feature-x"}]`),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := narvipg.NewRepoSettingsStore(rig.pool).UpsertLiveEgressEnabled(ctx, repo, true); err != nil {
		t.Fatalf("arm live egress for %s: %v", repo, err)
	}
	if _, err := rig.sandboxes.Create(ctx, sess.ID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if sandboxReady {
		setSandboxReady(ctx, t, rig, sess.ID)
	}
	plan := seedAwaitingApprovalPlan(ctx, t, rig, sess.ID, 1)

	commander := &deliveryCommander{}
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

	router := chi.NewRouter()
	router.Route("/api/sessions", func(r chi.Router) {
		r.Use(auth.Middleware(rig.userSessions, rig.users))
		r.Post("/{sessionID}/plans/{planId}/approve", httpapi.ApprovePlan(rig.pool, rig.sessions, rig.turns, rig.plans, rig.events, rig.planDocuments, rig.participants, rig.outbox, rig.linearAgentSessions, rig.auditLog, registry, false))
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	return queuedRevisionRig{
		deliveryFixture: deliveryFixture{rig: rig, cookie: cookie, userID: user.ID, sessionID: sess.ID, actor: actor, commander: commander, scm: scm},
		plan:            plan,
		registry:        registry,
		approve:         server,
	}
}

func setSandboxReady(ctx context.Context, t *testing.T, rig testRig, sessionID pgtype.UUID) {
	t.Helper()
	if _, err := rig.sandboxes.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: sessionID, Status: sqlcgen.SandboxStatusReady}); err != nil {
		t.Fatalf("move sandbox to ready: %v", err)
	}
}

// approveByREST approves v1 through the real approve route, by the
// owner's cookie, and returns the implementation turn it queued.
func (r queuedRevisionRig) approveByREST(t *testing.T) pgtype.UUID {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, r.approve.URL+"/api/sessions/"+r.sessionID.String()+"/plans/"+r.plan.ID.String()+"/approve", bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: platform.AuthSessionCookieName, Value: r.cookie})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body planActionResponseForTest
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != http.StatusOK || body.TurnID == nil {
		t.Fatalf("approve: status %d body %+v (err %v), want 200 with the implementation turn", resp.StatusCode, body, err)
	}
	var id pgtype.UUID
	if err := id.Scan(*body.TurnID); err != nil {
		t.Fatal(err)
	}
	return id
}

// turnStatus reads one turn's status.
func (r queuedRevisionRig) turnStatus(ctx context.Context, t *testing.T, id pgtype.UUID) string {
	t.Helper()
	var s string
	if err := r.rig.pool.QueryRow(ctx, `SELECT status::text FROM turns WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("read turn %s: %v", id.String(), err)
	}
	return s
}

// planVersions reads the session's plans as "version status" lines.
func (r queuedRevisionRig) planVersions(ctx context.Context, t *testing.T) string {
	t.Helper()
	rows, err := r.rig.pool.Query(ctx, `SELECT version || ' ' || status::text FROM plans WHERE session_id = $1 ORDER BY version`, r.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

// snapshotReady answers the snapshot command the actor sent last, so the
// sandbox is ready again and the next queued turn can dispatch.
func (r queuedRevisionRig) snapshotReady(ctx context.Context, t *testing.T) {
	t.Helper()
	commands := r.commander.ofType(t, "snapshot")
	if len(commands) == 0 {
		t.Fatal("no snapshot command to answer")
	}
	var cmd sandboxws.Snapshot
	if err := json.Unmarshal(commands[len(commands)-1], &cmd); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	raw, err := json.Marshal(sandboxws.SnapshotReady{
		Type: "snapshot_ready", MessageId: id, SessionId: r.sessionID.String(), Gen: 1,
		AckId: "snapshot_ready:" + id, SnapshotId: "snap-" + id, CommandMessageId: &cmd.MessageId,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.send(ctx, t, "snapshot_ready", raw, id)
}

// TestRevisionQueuedBehindImplementation_LeavesItAuthorized: a revision
// queued through CreateTurnForBot while the approved implementation is
// processing, or while it is still queued behind a sandbox that is not
// ready, leaves it authorized: it is not cancelled, v1 stays approved, the
// implementation is dispatched first, completes, and its branch is pushed
// and its pull request opened -- whether the push reports back before the
// revision is dispatched or only after the revision has written v2. The
// revision is dispatched only after the implementation completed, and
// writes v2 awaiting approval beside v1, still approved.
func TestRevisionQueuedBehindImplementation_LeavesItAuthorized(t *testing.T) {
	rig := newTestRig(t)
	const feedback = "keep the env fallback"

	for _, tc := range []struct {
		name string
		// queuedBeforeDispatch queues the revision while the
		// implementation is still pending (the sandbox not ready yet), so
		// both wait in the queue together.
		queuedBeforeDispatch bool
		// pushAfterRevision delivers the implementation's push_complete
		// only once the revision has run and written v2.
		pushAfterRevision bool
	}{
		{name: "queued while the implementation runs, delivered before the revision"},
		{name: "queued while the implementation is still queued", queuedBeforeDispatch: true},
		{name: "queued while the implementation runs, its push reporting back after v2", pushAfterRevision: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := newQueuedRevisionRig(ctx, t, rig, !tc.queuedBeforeDispatch)

			implementation := r.approveByREST(t)
			if tc.queuedBeforeDispatch {
				holds(t, 300*time.Millisecond, "the implementation waiting for a sandbox", func() bool {
					return len(r.commander.ofType(t, "prompt")) == 0
				})
			} else {
				eventually(t, 10*time.Second, func() bool { return len(r.commander.ofType(t, "prompt")) == 1 })
				if got := r.turnStatus(ctx, t, implementation); got != "processing" {
					t.Fatalf("the implementation is %s after its dispatch, want processing", got)
				}
			}
			wantImplementation := "processing"
			if tc.queuedBeforeDispatch {
				wantImplementation = "pending"
			}

			// The revision, queued through the one ingress that queues.
			revision, err := httpapi.CreateTurnForBot(ctx, rig.pool, rig.sessions, rig.turns, rig.plans, nil, rig.auditLog, r.registry, r.sessionID, feedback, nil, true, false, r.userID, nil, nil, nil, nil, nil, nil, nil, nil, false)
			if err != nil {
				t.Fatalf("CreateTurnForBot: %v", err)
			}
			if !revision.PlanMode || revision.Status != sqlcgen.TurnStatusPending {
				t.Fatalf("the revision = plan_mode %v, %s; want a pending plan-mode turn", revision.PlanMode, revision.Status)
			}
			wantPrompts := 1
			if tc.queuedBeforeDispatch {
				wantPrompts = 0
			}
			holds(t, 300*time.Millisecond, "the queue after the revision was queued", func() bool {
				return r.turnStatus(ctx, t, implementation) == wantImplementation &&
					r.turnStatus(ctx, t, revision.ID) == "pending" &&
					len(r.commander.ofType(t, "prompt")) == wantPrompts
			})
			if got := r.planVersions(ctx, t); got != "1 approved" {
				t.Fatalf("plans after the revision was queued: %s, want v1 still approved", got)
			}

			if tc.queuedBeforeDispatch {
				// The sandbox comes up: the oldest pending turn -- the
				// implementation -- is dispatched, and the revision waits.
				setSandboxReady(ctx, t, rig, r.sessionID)
				if err := r.actor.Send(ctx, sessionactor.EnsureDispatched{}); err != nil {
					t.Fatalf("EnsureDispatched: %v", err)
				}
				eventually(t, 10*time.Second, func() bool { return len(r.commander.ofType(t, "prompt")) == 1 })
				if got := r.turnStatus(ctx, t, implementation); got != "processing" {
					t.Fatalf("the implementation is %s once the sandbox is ready, want processing", got)
				}
				if got := r.turnStatus(ctx, t, revision.ID); got != "pending" {
					t.Fatalf("the revision is %s once the sandbox is ready, want still pending behind the implementation", got)
				}
			}
			if texts := r.commander.promptTexts(t); len(texts) != 1 || !strings.Contains(texts[0], "Implement the plan you just proposed.") {
				t.Fatalf("dispatched prompts %q, want the implementation's alone", texts)
			}

			// The implementation completes. Its push is sent; the revision
			// waits for the sandbox's snapshot to finish.
			r.executionComplete(ctx, t)
			if got := r.turnStatus(ctx, t, implementation); got != "completed" {
				t.Fatalf("the implementation is %s after its execution_complete, want completed", got)
			}
			eventually(t, 10*time.Second, func() bool { return len(r.commander.ofType(t, "push")) == 1 })
			if got := r.turnStatus(ctx, t, revision.ID); got != "pending" || len(r.commander.ofType(t, "prompt")) != 1 {
				t.Fatalf("the revision is %s with %d prompt(s) dispatched while the sandbox snapshots, want pending and one", got, len(r.commander.ofType(t, "prompt")))
			}

			delivered := func() {
				t.Helper()
				r.pushComplete(ctx, t)
				eventually(t, 10*time.Second, func() bool { return r.scm.calls() == 1 && r.prArtifacts(ctx, t) == 1 })
			}
			if !tc.pushAfterRevision {
				delivered()
			}

			// The snapshot finishes: only now is the revision dispatched.
			r.snapshotReady(ctx, t)
			eventually(t, 10*time.Second, func() bool { return len(r.commander.ofType(t, "prompt")) == 2 })
			if texts := r.commander.promptTexts(t); !strings.Contains(texts[1], feedback) {
				t.Fatalf("second dispatched prompt %q, want the revision's", texts[1])
			}
			var implDone, revisionDispatched pgtype.Timestamptz
			if err := rig.pool.QueryRow(ctx, `SELECT (SELECT completed_at FROM turns WHERE id = $1), (SELECT dispatched_at FROM turns WHERE id = $2)`, implementation, revision.ID).Scan(&implDone, &revisionDispatched); err != nil {
				t.Fatal(err)
			}
			if !implDone.Valid || !revisionDispatched.Valid || revisionDispatched.Time.Before(implDone.Time) {
				t.Fatalf("the revision was dispatched at %v, the implementation completed at %v -- want the revision after", revisionDispatched.Time, implDone.Time)
			}

			// The revision completes: v2 awaits approval, v1 stays approved.
			r.executionComplete(ctx, t)
			if got := r.turnStatus(ctx, t, revision.ID); got != "completed" {
				t.Fatalf("the revision is %s after its execution_complete, want completed", got)
			}
			if got := r.planVersions(ctx, t); got != "1 approved, 2 awaiting_approval" {
				t.Fatalf("plans after the revision: %s, want v1 approved and v2 awaiting approval", got)
			}

			if tc.pushAfterRevision {
				delivered()
			}
			if got := r.turnStatus(ctx, t, implementation); got != "completed" || r.scm.calls() != 1 || r.prArtifacts(ctx, t) != 1 {
				t.Fatalf("the implementation %s with %d pull request(s) opened and %d recorded, want completed, one and one", got, r.scm.calls(), r.prArtifacts(ctx, t))
			}
			if got := r.planVersions(ctx, t); got != "1 approved, 2 awaiting_approval" {
				t.Fatalf("plans at the end: %s, want v1 approved and v2 awaiting approval", got)
			}
		})
	}
}
