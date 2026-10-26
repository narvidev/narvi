//go:build integration

package sessionactor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/narvidev/narvi/contracts/gen/go/sandboxws"
	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/continuity"
	plandomain "github.com/narvidev/narvi/internal/domain/plan"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/sandbox"
	"github.com/narvidev/narvi/internal/domain/turn"
	domainupload "github.com/narvidev/narvi/internal/domain/upload"
	"github.com/narvidev/narvi/internal/platform"
)

// This file proves technical plan §35.5's fresh lineage on the real actor
// and real Postgres (continuity.go): a claim that spawns fresh in place of
// an earlier gen of a session that had a turn dispatched is marked, by
// every path that spawns fresh, and nothing else is; it forgets the
// session's conversation id; the first prompt that reaches the marked gen
// -- a dispatch or a re-send -- opens with a recap of the session's
// earlier turns, built by the extractor the notification surfaces use, cut
// to the gen's prompt bound and stored with the turn; and the mark is
// cleared only by evidence that the prompt reached the sandbox.

// lineageCommander records every command it is asked to send. A snapshot
// command finds no connection, so the sandbox goes back to ready after
// each turn (triggerSnapshotBestEffort's revert) and no snapshot is ever
// recorded unless a test records one; a prompt fails with promptErr when
// it is set.
type lineageCommander struct {
	mu        sync.Mutex
	payloads  []json.RawMessage
	promptErr error
}

var _ ports.SandboxCommander = (*lineageCommander)(nil)

func (c *lineageCommander) SendCommand(_ string, payload json.RawMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads = append(c.payloads, append(json.RawMessage(nil), payload...))
	var head struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(payload, &head)
	switch head.Type {
	case "snapshot":
		return ports.ErrNoLiveSandboxConnection
	case "prompt":
		return c.promptErr
	}
	return nil
}

func (c *lineageCommander) setPromptErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.promptErr = err
}

// prompts returns every prompt command sent, in order.
func (c *lineageCommander) prompts(t *testing.T) []sentPrompt {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []sentPrompt
	for _, payload := range c.payloads {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &head); err != nil {
			t.Fatalf("malformed command payload %s: %v", payload, err)
		}
		if head.Type != "prompt" {
			continue
		}
		var p sandboxws.Prompt
		if err := json.Unmarshal(payload, &p); err != nil {
			t.Fatalf("prompt fails its contract: %v (%s)", err, payload)
		}
		out = append(out, sentPrompt{Prompt: p, raw: payload})
	}
	return out
}

// lastPrompt is the latest prompt sent; it fails the test when there is
// none.
func (c *lineageCommander) lastPrompt(t *testing.T) sentPrompt {
	t.Helper()
	prompts := c.prompts(t)
	if len(prompts) == 0 {
		t.Fatal("no prompt was sent")
	}
	return prompts[len(prompts)-1]
}

// lineageRig is one session on the real actor.
type lineageRig struct {
	pool      *pgxpool.Pool
	sessionID pgtype.UUID
	turns     *narvipg.TurnStore
	sandboxes *narvipg.SandboxStore
	provider  *fakeSpawnProvider
	commander *lineageCommander
	actor     *Actor
}

// lineageRigOptions shape a lineageRig. The zero value is a plain session
// whose sandbox is ready at gen 1, a provider that spawns and restores and
// never resumes, and the shipped timeouts.
type lineageRigOptions struct {
	// session creates the session; createTestSession when nil.
	session  func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID
	provider *fakeSpawnProvider
	// noSandbox leaves the session with no sandbox row.
	noSandbox bool
	// seed runs once the sandbox is created, before the actor is hydrated.
	seed func(ctx context.Context, t *testing.T, rig *lineageRig)
}

func newLineageRig(ctx context.Context, t *testing.T, opts lineageRigOptions) *lineageRig {
	t.Helper()
	pool := newTestPool(t)
	rig := &lineageRig{
		pool:      pool,
		turns:     narvipg.NewTurnStore(pool),
		sandboxes: narvipg.NewSandboxStore(pool),
		provider:  opts.provider,
		commander: &lineageCommander{},
	}
	if opts.session != nil {
		rig.sessionID = opts.session(ctx, t, pool)
	} else {
		rig.sessionID = createTestSession(ctx, t, pool)
	}
	if !opts.noSandbox {
		if _, err := rig.sandboxes.Create(ctx, rig.sessionID); err != nil {
			t.Fatalf("create sandbox: %v", err)
		}
		rig.setStatus(ctx, t, sqlcgen.SandboxStatusReady)
	}
	if rig.provider == nil {
		rig.provider = &fakeSpawnProvider{}
	}
	rig.provider.nextRef = ports.SandboxRef{ProviderID: "fresh-object"}
	rig.provider.nextRestoreRef = ports.SandboxRef{ProviderID: "restored-object"}
	if opts.seed != nil {
		opts.seed(ctx, t, rig)
	}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), nil, rig.commander, rig.provider, "http://localhost:8080", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	if rig.actor, err = r.GetOrSpawn(ctx, rig.sessionID); err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	return rig
}

func (r *lineageRig) sandbox(ctx context.Context, t *testing.T) sqlcgen.Sandbox {
	t.Helper()
	row, err := r.sandboxes.Get(ctx, r.sessionID)
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	return row
}

func (r *lineageRig) turn(ctx context.Context, t *testing.T, id pgtype.UUID) sqlcgen.Turn {
	t.Helper()
	row, err := r.turns.Get(ctx, id)
	if err != nil {
		t.Fatalf("get turn: %v", err)
	}
	return row
}

func (r *lineageRig) conversationID(ctx context.Context, t *testing.T) *string {
	t.Helper()
	row, err := narvipg.NewSessionStore(r.pool).Get(ctx, r.sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	return row.OpencodeConversationID
}

func (r *lineageRig) setConversationID(ctx context.Context, t *testing.T, id string) {
	t.Helper()
	if _, err := narvipg.NewSessionStore(r.pool).UpdateConversationID(ctx, sqlcgen.UpdateSessionConversationIDParams{ID: r.sessionID, OpencodeConversationID: &id}); err != nil {
		t.Fatalf("record the conversation id: %v", err)
	}
}

// setStatus moves the sandbox to status, as its loss -- an idle stop, a
// failed liveness check through its grace -- would.
func (r *lineageRig) setStatus(ctx context.Context, t *testing.T, status sqlcgen.SandboxStatus) {
	t.Helper()
	if _, err := r.pool.Exec(ctx, `UPDATE sandboxes SET status = $2::sandbox_status WHERE session_id = $1`, r.sessionID, string(status)); err != nil {
		t.Fatalf("move the sandbox to %s: %v", status, err)
	}
}

// exec runs a seed statement against the session's sandbox row: $1 is the
// session.
func (r *lineageRig) exec(ctx context.Context, t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := r.pool.Exec(ctx, sql, append([]any{r.sessionID}, args...)...); err != nil {
		t.Fatalf("seed (%s): %v", sql, err)
	}
}

// mark reads the sandbox's gen and its fresh-lineage mark.
func (r *lineageRig) mark(ctx context.Context, t *testing.T) (gen int32, mark *int32) {
	t.Helper()
	row := r.sandbox(ctx, t)
	return row.Gen, row.FreshLineageGen
}

// assertMarked fails unless the sandbox is at gen, marked for it.
func (r *lineageRig) assertMarked(ctx context.Context, t *testing.T, gen int32) {
	t.Helper()
	if got, mark := r.mark(ctx, t); got != gen || mark == nil || *mark != gen {
		t.Fatalf("sandbox at gen %d marked %v, want gen %d marked a fresh lineage", got, mark, gen)
	}
}

// assertUnmarked fails unless the sandbox is at gen with no mark at all.
func (r *lineageRig) assertUnmarked(ctx context.Context, t *testing.T, gen int32) {
	t.Helper()
	if got, mark := r.mark(ctx, t); got != gen || mark != nil {
		t.Fatalf("sandbox at gen %d marked %v, want gen %d with no mark", got, mark, gen)
	}
}

// lineageStepStart is a step_start of gen, as wshub hands it to the actor.
func lineageStepStart(t *testing.T, sessionID pgtype.UUID, gen int) SandboxEvent {
	t.Helper()
	stepID := "step-" + uuid.NewString()
	raw, err := json.Marshal(sandboxws.StepStart{Type: "step_start", MessageId: "msg_" + stepID, SessionId: sessionID.String(), Gen: gen, StepId: stepID})
	if err != nil {
		t.Fatal(err)
	}
	return SandboxEvent{Type: "step_start", Gen: gen, MessageID: "msg_" + stepID, Raw: raw}
}

// lineageToken is one whole text part of gen, as one token frame.
func lineageToken(t *testing.T, sessionID pgtype.UUID, gen int, text string) SandboxEvent {
	t.Helper()
	partID := "part-" + uuid.NewString()
	raw, err := json.Marshal(sandboxws.Token{Type: "token", MessageId: partID, SessionId: sessionID.String(), Gen: gen, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return SandboxEvent{Type: "token", Gen: gen, MessageID: partID, Raw: raw}
}

// stream delivers, on gen, a step's start and then each of parts as a text
// part of its own, in order: the turn processing there streams them.
func (r *lineageRig) stream(ctx context.Context, t *testing.T, gen int, parts ...string) {
	t.Helper()
	deliver(ctx, t, r.actor, lineageStepStart(t, r.sessionID, gen))
	for _, text := range parts {
		deliver(ctx, t, r.actor, lineageToken(t, r.sessionID, gen, text))
	}
}

// queue creates a pending turn carrying request, arming the dispatch
// timer as every turn's creation does, and evaluates.
func (r *lineageRig) queue(ctx context.Context, t *testing.T, request string) sqlcgen.Turn {
	t.Helper()
	created := createTurnArmingDispatch(ctx, t, r.pool, r.sessionID, request)
	settle(ctx, t, r.actor)
	return created
}

// runTurn runs one turn on gen, ready: the turn is queued and sent,
// streams parts, and completes.
func (r *lineageRig) runTurn(ctx context.Context, t *testing.T, gen int, request string, parts ...string) sqlcgen.Turn {
	t.Helper()
	sent := len(r.commander.prompts(t))
	created := r.queue(ctx, t, request)
	if got := len(r.commander.prompts(t)); got != sent+1 {
		t.Fatalf("%d prompts sent, want %d: %q was dispatched", got, sent+1, request)
	}
	r.stream(ctx, t, gen, parts...)
	deliver(ctx, t, r.actor, completedEvent(gen))
	row := r.turn(ctx, t, created.ID)
	if row.Status != sqlcgen.TurnStatusCompleted {
		t.Fatalf("turn %q is %s, want completed", request, row.Status)
	}
	if got := r.sandbox(ctx, t).Status; got != sqlcgen.SandboxStatusReady {
		t.Fatalf("sandbox %s after the turn, want ready", got)
	}
	return row
}

// respawn queues request for the lost sandbox: the evaluation claims a new
// gen, and the provider's answer is recorded.
func (r *lineageRig) respawn(ctx context.Context, t *testing.T, request string) sqlcgen.Turn {
	t.Helper()
	created := r.queue(ctx, t, request)
	waitForConnecting(ctx, t, r.pool, r.sessionID)
	return created
}

// bootGen connects and boots gen, as its agent does: its ready -- one
// that advertises prompt receipts when receiptCapable --, its boot
// evidence and a null-phase heartbeat. The sandbox is ready, and the
// heartbeat's own evaluation has dispatched what it could.
func (r *lineageRig) bootGen(ctx context.Context, t *testing.T, gen int, receiptCapable bool) {
	t.Helper()
	deliver(ctx, t, r.actor, receiptReady(gen, receiptCapable))
	deliver(ctx, t, r.actor, bootTimingEvent("bt-"+uuid.NewString(), gen, "boot_duration", "false"))
	deliver(ctx, t, r.actor, nullPhaseHeartbeat("hb-"+uuid.NewString(), gen))
	if row := r.sandbox(ctx, t); row.Gen != int32(gen) || row.Status != sqlcgen.SandboxStatusReady {
		t.Fatalf("sandbox at gen %d %s after its boot, want gen %d ready", row.Gen, row.Status, gen)
	}
}

// twoTurnsThenLost runs two turns on gen 1 -- each one text part -- and
// then loses the sandbox, failed with nothing to restore.
func (r *lineageRig) twoTurnsThenLost(ctx context.Context, t *testing.T) (first, second sqlcgen.Turn) {
	t.Helper()
	first = r.runTurn(ctx, t, 1, "add the retry to the importer", "Added the retry, with a test; pushed to the branch.")
	second = r.runTurn(ctx, t, 1, "now make the backoff exponential", "Made the backoff exponential; not pushed yet.")
	r.setStatus(ctx, t, sqlcgen.SandboxStatusFailed)
	return first, second
}

// recapOf splits p's text into the recap it opens with and the request
// after it, failing unless the text is a whole recap, the separator and
// request, and every line of the recap is the template's own or a quoted
// excerpt line.
func recapOf(t *testing.T, p sentPrompt, request string) string {
	t.Helper()
	recap, ok := strings.CutSuffix(p.Text, continuity.Separator+request)
	if !ok || !strings.HasPrefix(recap, continuity.LineHeader+"\n") || !strings.HasSuffix(recap, "\n"+continuity.LineClosing) {
		t.Fatalf("prompt text (%d bytes) %q, want a recap, then the request %q", len(p.Text), shortened(p.Text), shortened(request))
	}
	for _, line := range strings.Split(recap, "\n") {
		if !continuity.IsTemplateLine(line) && line != continuity.LineEmptyQuote && !strings.HasPrefix(line, continuity.QuotePrefix) {
			t.Fatalf("recap line %q is neither the template's nor a quoted excerpt line", line)
		}
	}
	return recap
}

// shortened is s, or its head when it is long, for a failure message.
func shortened(s string) string {
	if len(s) <= 200 {
		return s
	}
	return s[:200] + "..."
}

// freshLineageCount is sandbox_fresh_lineage_total{cause, carried}.
func freshLineageCount(ctx context.Context, t *testing.T, cause sandbox.LineageCause, carried bool) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := otelReader.Collect(ctx, &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		if sm.Scope.Name != meterName {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "sandbox_fresh_lineage_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("sandbox_fresh_lineage_total data = %T, want metricdata.Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				c, _ := dp.Attributes.Value(attribute.Key("cause"))
				cr, _ := dp.Attributes.Value(attribute.Key("carried"))
				if c.AsString() == string(cause) && cr.AsBool() == carried {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// freshLineageCounts reads every cause and carried pair.
func freshLineageCounts(ctx context.Context, t *testing.T) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, cause := range []sandbox.LineageCause{sandbox.LineageCauseNoSnapshot, sandbox.LineageCauseRotationNoSnapshot,
		sandbox.LineageCauseRestoreRefused, sandbox.LineageCauseRestoreDowngraded, sandbox.LineageCauseStuckRespawn} {
		for _, carried := range []bool{false, true} {
			key := string(cause)
			if carried {
				key += "/carried"
			}
			out[key] = freshLineageCount(ctx, t, cause, carried)
		}
	}
	return out
}

// continuityRecapCount is turn_continuity_recap_total{outcome}.
func continuityRecapCount(ctx context.Context, t *testing.T, outcome string) int64 {
	t.Helper()
	return counterByAttrs(ctx, t, "turn_continuity_recap_total", map[string]string{"outcome": outcome})
}

// lockedLogCount counts the lines of l whose msg is msg.
func lockedLogCount(l *lockedLog, msg string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range bytes.Split(l.buf.Bytes(), []byte("\n")) {
		var e map[string]any
		if json.Unmarshal(line, &e) == nil && e["msg"] == msg {
			n++
		}
	}
	return n
}

// seedEarlierTurn records a turn of the rig's session dispatched to gen
// that ended status, before the sandbox was lost.
func (r *lineageRig) seedEarlierTurn(ctx context.Context, t *testing.T, gen int32, status sqlcgen.TurnStatus) {
	t.Helper()
	request := "the earlier request"
	created, err := r.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: r.sessionID, Status: status, Prompt: &request})
	if err != nil {
		t.Fatal(err)
	}
	var eventID int64
	if err := r.pool.QueryRow(ctx, `SELECT coalesce(max(id), 0) FROM events WHERE session_id = $1`, r.sessionID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	if _, err := r.turns.UpdateStatus(ctx, sqlcgen.UpdateTurnStatusParams{
		ID: created.ID, Status: status, DispatchedAt: now, CompletedAt: now, DispatchedSandboxGen: &gen, DispatchedEventID: &eventID,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestFreshLineage_EveryFreshPathIsMarked: every claim that spawns a
// sandbox fresh in place of an earlier gen of a session that had a turn
// dispatched marks the new gen, whichever path brought it there, with the
// cause the decision gives -- and forgets the session's conversation id.
// The line logged with it names the cause, and a session that runs a
// container daemon is named as never snapshotted.
func TestFreshLineage_EveryFreshPathIsMarked(t *testing.T) {
	dockerSession := func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
		return createTestSessionWithEnvironment(ctx, t, pool, createTestEnvironmentWithDocker(ctx, t, pool))
	}
	shadowSession := func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
		// A repository with no settings resolves shadow (§30.4).
		return createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "repo", "https://github.com/acme/repo.git", "main")
	}
	withSnapshot := func(id string) string {
		return `UPDATE sandboxes SET status = 'stopped', snapshot_id = '` + id + `' WHERE session_id = $1`
	}
	tests := []struct {
		name    string
		session func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID
		docker  bool
		// seed is the sandbox row at gen 1 as its loss left it.
		seed      []string
		earlier   sqlcgen.TurnStatus
		cause     sandbox.LineageCause
		snapshot  string // the snapshot the claim leaves recorded, "" for none
		stuckLive bool
	}{
		{name: "IdleFailureNoSnapshot", seed: []string{`UPDATE sandboxes SET status = 'failed' WHERE session_id = $1`},
			cause: sandbox.LineageCauseNoSnapshot},
		{name: "ContainerDaemonSession", session: dockerSession, docker: true,
			seed: []string{`UPDATE sandboxes SET status = 'stopped' WHERE session_id = $1`}, cause: sandbox.LineageCauseNoSnapshot},
		{name: "RotationNoSnapshot", seed: []string{`UPDATE sandboxes SET status = 'stopped', rotation_gen = gen, rotation_started_at = now() WHERE session_id = $1`},
			cause: sandbox.LineageCauseRotationNoSnapshot},
		{name: "ProvenanceRefused", seed: []string{withSnapshot("snap-other-major"),
			`UPDATE sandboxes SET snapshot_provenance_id = snapshot_id, snapshot_agent_protocol = '0.9.0', snapshot_minted_at = now() WHERE session_id = $1`},
			cause: sandbox.LineageCauseRestoreRefused},
		{name: "HooksBoundaryRefused", seed: []string{withSnapshot("snap-without-hooks"),
			`UPDATE sandboxes SET snapshot_provenance_id = snapshot_id, snapshot_agent_protocol = '1.28.0', snapshot_minted_at = now(),
				snapshot_skipped_repo_hooks_id = snapshot_id WHERE session_id = $1`},
			cause: sandbox.LineageCauseRestoreRefused},
		{name: "ContainerDaemonDowngrade", session: dockerSession, docker: true, seed: []string{withSnapshot("snap-before-docker")},
			cause: sandbox.LineageCauseRestoreDowngraded, snapshot: "snap-before-docker"},
		{name: "ShadowDowngrade", session: shadowSession, seed: []string{withSnapshot("snap-taken-live")},
			cause: sandbox.LineageCauseRestoreDowngraded, snapshot: "snap-taken-live"},
		{name: "StopRetirementNoSnapshot", seed: []string{`UPDATE sandboxes SET status = 'stopped', stop_retire_gen = gen WHERE session_id = $1`},
			earlier: sqlcgen.TurnStatusCancelled, cause: sandbox.LineageCauseNoSnapshot},
		{name: "StuckRestoreForceRespawned", seed: []string{`UPDATE sandboxes SET status = 'spawning', snapshot_id = 'snap-slow-restore',
				created_at = now() - interval '10 minutes', last_seen_at = now() - interval '10 minutes' WHERE session_id = $1`},
			cause: sandbox.LineageCauseStuckRespawn, snapshot: "snap-slow-restore", stuckLive: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			logs := captureDefaultLoggerLocked(t)
			counts := freshLineageCounts(ctx, t)
			earlier := tc.earlier
			if earlier == "" {
				earlier = sqlcgen.TurnStatusCompleted
			}
			rig := newLineageRig(ctx, t, lineageRigOptions{
				session:  tc.session,
				provider: &fakeSpawnProvider{dockerSupported: tc.docker},
				seed: func(ctx context.Context, t *testing.T, rig *lineageRig) {
					rig.seedEarlierTurn(ctx, t, 1, earlier)
					rig.setConversationID(ctx, t, "conv-of-gen-1")
					for _, sql := range tc.seed {
						rig.exec(ctx, t, sql)
					}
				},
			})

			rig.respawn(ctx, t, "carry on")
			if rig.provider.callCount() != 1 || rig.provider.restoreCallCount() != 0 {
				t.Fatalf("fresh spawns %d, restores %d; want one fresh spawn", rig.provider.callCount(), rig.provider.restoreCallCount())
			}
			rig.assertMarked(ctx, t, 2)
			if got := stringOrEmpty(rig.sandbox(ctx, t).SnapshotID); got != tc.snapshot {
				t.Fatalf("snapshot %q after the claim, want %q", got, tc.snapshot)
			}
			if id := rig.conversationID(ctx, t); id != nil {
				t.Fatalf("conversation id %q after the fresh spawn, want it forgotten", *id)
			}

			want := map[string]int64{string(tc.cause): 1}
			if got := countDelta(counts, freshLineageCounts(ctx, t)); len(got) != 1 || got[string(tc.cause)] != 1 {
				t.Fatalf("sandbox_fresh_lineage_total moved by %v, want %v", got, want)
			}
			line := logs.entry(t, freshLineageLogMessage)
			if line["cause"] != string(tc.cause) || line["carried"] != false || line["gen"] != float64(2) ||
				line["replaced_gen"] != float64(1) || line["docker_required"] != tc.docker || line["conversation_cleared"] != true {
				t.Fatalf("fresh lineage line = %v, want cause %s at gen 2 replacing gen 1, docker_required %v, the conversation cleared", line, tc.cause, tc.docker)
			}
		})
	}
}

// TestFreshLineage_SpawnFailureRetryCarriesTheMark: a marked gen whose
// spawn failed never received its recap, so the next fresh claim carries
// the mark to its own gen -- counted and logged as carried, never
// announced again.
func TestFreshLineage_SpawnFailureRetryCarriesTheMark(t *testing.T) {
	ctx := context.Background()
	logs := captureDefaultLoggerLocked(t)
	announced := freshLineageCount(ctx, t, sandbox.LineageCauseNoSnapshot, false)
	carried := freshLineageCount(ctx, t, sandbox.LineageCauseNoSnapshot, true)
	rig := newLineageRig(ctx, t, lineageRigOptions{
		provider: &fakeSpawnProvider{nextErr: &ports.ProviderError{Transient: false, Code: "INVALID_IMAGE", Op: ports.OpCreateSandbox, Err: errors.New("boom")}},
		seed: func(ctx context.Context, t *testing.T, rig *lineageRig) {
			rig.seedEarlierTurn(ctx, t, 1, sqlcgen.TurnStatusCompleted)
			rig.setStatus(ctx, t, sqlcgen.SandboxStatusFailed)
		},
	})

	rig.queue(ctx, t, "carry on")
	waitUntil(t, 5*time.Second, func() bool { return rig.sandbox(ctx, t).Status == sqlcgen.SandboxStatusSuspect })
	rig.assertMarked(ctx, t, 2)

	// The failed spawn's grace runs out; the provider is back.
	rig.provider.mu.Lock()
	rig.provider.nextErr = nil
	rig.provider.mu.Unlock()
	rig.setStatus(ctx, t, sqlcgen.SandboxStatusFailed)
	settle(ctx, t, rig.actor)
	waitForConnecting(ctx, t, rig.pool, rig.sessionID)

	rig.assertMarked(ctx, t, 3)
	if got := freshLineageCount(ctx, t, sandbox.LineageCauseNoSnapshot, false) - announced; got != 1 {
		t.Fatalf("sandbox_fresh_lineage_total{no_snapshot, announced} moved by %d, want 1: the loss is announced once", got)
	}
	if got := freshLineageCount(ctx, t, sandbox.LineageCauseNoSnapshot, true) - carried; got != 1 {
		t.Fatalf("sandbox_fresh_lineage_total{no_snapshot, carried} moved by %d, want 1", got)
	}
	if line := logs.entry(t, freshLineageCarriedLogMessage); line["gen"] != float64(3) || line["carried"] != true || line["level"] != "INFO" {
		t.Fatalf("carried line = %v, want gen 3, carried, at INFO", line)
	}
	if n := lockedLogCount(logs, freshLineageLogMessage); n != 1 {
		t.Fatalf("%d announced fresh-lineage lines, want 1", n)
	}
}

// TestFreshLineage_RestoresAndResumesAreNeverMarked: a restore -- of a
// compatible snapshot, of one whose provenance is unknown, of a rotation's
// own snapshot or of the one before it -- and a resume keep the agent's
// context, so none is marked or counted, the conversation id is kept, and
// the turn sent to the new gen carries no recap and names the conversation.
func TestFreshLineage_RestoresAndResumesAreNeverMarked(t *testing.T) {
	withSnapshot := `UPDATE sandboxes SET status = 'stopped', snapshot_id = 'snap-kept-context' WHERE session_id = $1`
	tests := []struct {
		name   string
		resume bool
		seed   []string
	}{
		{name: "Compatible", seed: []string{withSnapshot,
			`UPDATE sandboxes SET snapshot_provenance_id = snapshot_id, snapshot_agent_protocol = '1.28.0', snapshot_minted_at = now() WHERE session_id = $1`}},
		{name: "Unknown", seed: []string{withSnapshot}},
		{name: "RotationsOwnSnapshot", seed: []string{withSnapshot,
			`UPDATE sandboxes SET rotation_gen = gen, rotation_started_at = now() - interval '1 minute',
				snapshot_provenance_id = snapshot_id, snapshot_agent_protocol = '1.28.0', snapshot_minted_at = now() WHERE session_id = $1`}},
		{name: "RotationFallback", seed: []string{withSnapshot,
			`UPDATE sandboxes SET rotation_gen = gen, rotation_started_at = now(),
				snapshot_provenance_id = snapshot_id, snapshot_agent_protocol = '1.28.0', snapshot_minted_at = now() - interval '30 minutes' WHERE session_id = $1`}},
		{name: "Resume", resume: true, seed: []string{`UPDATE sandboxes SET status = 'stopped', provider_id = 'object-of-gen-1' WHERE session_id = $1`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			logs := captureDefaultLoggerLocked(t)
			counts := freshLineageCounts(ctx, t)
			rig := newLineageRig(ctx, t, lineageRigOptions{
				provider: &fakeSpawnProvider{resumeSupported: tc.resume},
				seed: func(ctx context.Context, t *testing.T, rig *lineageRig) {
					rig.seedEarlierTurn(ctx, t, 1, sqlcgen.TurnStatusCompleted)
					rig.setConversationID(ctx, t, "conv-of-gen-1")
					for _, sql := range tc.seed {
						rig.exec(ctx, t, sql)
					}
				},
			})

			rig.respawn(ctx, t, "carry on")
			if tc.resume {
				if rig.provider.resumeCallCount() != 1 || rig.provider.callCount()+rig.provider.restoreCallCount() != 0 {
					t.Fatalf("resumes %d, spawns %d, restores %d; want one resume", rig.provider.resumeCallCount(), rig.provider.callCount(), rig.provider.restoreCallCount())
				}
			} else if rig.provider.restoreCallCount() != 1 || rig.provider.callCount() != 0 {
				t.Fatalf("restores %d, fresh spawns %d; want one restore", rig.provider.restoreCallCount(), rig.provider.callCount())
			}
			rig.assertUnmarked(ctx, t, 2)
			if id := rig.conversationID(ctx, t); stringOrEmpty(id) != "conv-of-gen-1" {
				t.Fatalf("conversation id %v after the claim, want it kept", id)
			}
			if got := countDelta(counts, freshLineageCounts(ctx, t)); len(got) != 0 {
				t.Fatalf("sandbox_fresh_lineage_total moved by %v, want nothing", got)
			}
			if n := lockedLogCount(logs, freshLineageLogMessage) + lockedLogCount(logs, freshLineageCarriedLogMessage); n != 0 {
				t.Fatalf("%d fresh-lineage lines, want none", n)
			}

			rig.bootGen(ctx, t, 2, false)
			p := rig.commander.lastPrompt(t)
			if p.Text != "carry on" || stringOrEmpty(p.ConversationId) != "conv-of-gen-1" {
				t.Fatalf("prompt to gen 2: text %q, conversation %v; want the request alone, naming conv-of-gen-1", p.Text, p.ConversationId)
			}
		})
	}
}

// TestFreshLineage_FirstSpawnIsNeverMarked: a session's first gen -- no
// sandbox row, or one still pending -- has nothing to lose, so it is never
// marked, whatever its turns record, and its first prompt carries no recap.
func TestFreshLineage_FirstSpawnIsNeverMarked(t *testing.T) {
	for _, tc := range []struct {
		name      string
		noSandbox bool
	}{
		{"no sandbox row", true},
		// A pending row is the session's first gen whatever its turns say:
		// one is recorded as dispatched to it, so only the row's own state
		// keeps it unmarked.
		{"a sandbox row still pending", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			counts := freshLineageCounts(ctx, t)
			rig := newLineageRig(ctx, t, lineageRigOptions{
				noSandbox: tc.noSandbox,
				seed: func(ctx context.Context, t *testing.T, rig *lineageRig) {
					if tc.noSandbox {
						return
					}
					// Past the spawn cooldown, which a pending row is held to.
					rig.exec(ctx, t, `UPDATE sandboxes SET status = 'pending', created_at = now() - interval '10 minutes' WHERE session_id = $1`)
					rig.seedEarlierTurn(ctx, t, 1, sqlcgen.TurnStatusFailed)
				},
			})

			rig.respawn(ctx, t, "start the work")
			gen := rig.sandbox(ctx, t).Gen
			rig.assertUnmarked(ctx, t, gen)
			if got := countDelta(counts, freshLineageCounts(ctx, t)); len(got) != 0 {
				t.Fatalf("sandbox_fresh_lineage_total moved by %v, want nothing", got)
			}
			rig.bootGen(ctx, t, int(gen), false)
			if p := rig.commander.lastPrompt(t); p.Text != "start the work" {
				t.Fatalf("first prompt text %q, want the request alone", p.Text)
			}
		})
	}
}

// TestFreshLineage_NoTurnDispatchedBefore_NotMarked: a fresh spawn in
// place of a gen no turn was ever dispatched to -- a first spawn that
// failed, a first gen stuck starting, or one whose only turn never reached
// a sandbox -- has no context to lose, and is not marked.
func TestFreshLineage_NoTurnDispatchedBefore_NotMarked(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(ctx context.Context, t *testing.T, rig *lineageRig)
	}{
		{"a first spawn that failed", func(ctx context.Context, t *testing.T, rig *lineageRig) {
			rig.setStatus(ctx, t, sqlcgen.SandboxStatusFailed)
		}},
		{"a first gen stuck starting", func(ctx context.Context, t *testing.T, rig *lineageRig) {
			rig.exec(ctx, t, `UPDATE sandboxes SET status = 'spawning', created_at = now() - interval '10 minutes', last_seen_at = now() - interval '10 minutes' WHERE session_id = $1`)
		}},
		{"an earlier turn that never reached a sandbox", func(ctx context.Context, t *testing.T, rig *lineageRig) {
			request := "refused before it was sent"
			if _, err := rig.turns.Create(ctx, sqlcgen.CreateTurnParams{SessionID: rig.sessionID, Status: sqlcgen.TurnStatusFailed, Prompt: &request}); err != nil {
				t.Fatal(err)
			}
			rig.setStatus(ctx, t, sqlcgen.SandboxStatusStopped)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			counts := freshLineageCounts(ctx, t)
			rig := newLineageRig(ctx, t, lineageRigOptions{seed: tc.seed})

			rig.respawn(ctx, t, "start the work")
			if rig.provider.callCount() != 1 {
				t.Fatalf("fresh spawns %d, want 1", rig.provider.callCount())
			}
			rig.assertUnmarked(ctx, t, 2)
			if got := countDelta(counts, freshLineageCounts(ctx, t)); len(got) != 0 {
				t.Fatalf("sandbox_fresh_lineage_total moved by %v, want nothing", got)
			}
			rig.bootGen(ctx, t, 2, false)
			if p := rig.commander.lastPrompt(t); p.Text != "start the work" {
				t.Fatalf("prompt text %q, want the request alone", p.Text)
			}
		})
	}
}

// TestFreshLineage_FirstTurnAfterTheMarkCarriesTheRecap: the first turn
// sent to a fresh lineage opens with the recap of the session's earlier
// turns -- each request and what the earlier instance reported, quoted
// under the framing -- then the request; it names no conversation, the
// recap is stored with the turn for the gen it was built for, and the
// dispatch is counted attached.
func TestFreshLineage_FirstTurnAfterTheMarkCarriesTheRecap(t *testing.T) {
	ctx := context.Background()
	attached := continuityRecapCount(ctx, t, continuityRecapOutcomeAttached)
	rig := newLineageRig(ctx, t, lineageRigOptions{})
	rig.setConversationID(ctx, t, "conv-of-gen-1")
	rig.twoTurnsThenLost(ctx, t)
	third := rig.respawn(ctx, t, "and add a jitter")
	rig.assertMarked(ctx, t, 2)
	rig.bootGen(ctx, t, 2, false)

	p := rig.commander.lastPrompt(t)
	if p.Gen != 2 || p.ConversationId != nil {
		t.Fatalf("prompt to gen %d naming conversation %v, want gen 2 naming none", p.Gen, p.ConversationId)
	}
	recap := recapOf(t, p, "and add a jitter")
	for _, want := range []string{
		"\nTurn 1 (completed)\n" + continuity.LineRequest + "\n> add the retry to the importer\n" + continuity.LineReport + "\n> Added the retry, with a test; pushed to the branch.\n",
		"\nTurn 2 (completed)\n" + continuity.LineRequest + "\n> now make the backoff exponential\n" + continuity.LineReport + "\n> Made the backoff exponential; not pushed yet.\n",
	} {
		if !strings.Contains(recap, want) {
			t.Fatalf("recap %q, want it to hold %q", recap, want)
		}
	}
	if strings.Contains(recap, "Turn 3") || strings.Contains(recap, continuity.LineInterruptedNothing) || strings.Contains(recap, continuity.LineInterrupted) {
		t.Fatalf("recap %q reports the current turn, want only the earlier ones", recap)
	}
	stored := rig.turn(ctx, t, third.ID)
	if stringOrEmpty(stored.ContinuityRecap) != recap || stored.ContinuityRecapGen == nil || *stored.ContinuityRecapGen != 2 {
		t.Fatalf("turn's stored recap %v for gen %v, want the recap sent, for gen 2", stored.ContinuityRecap, stored.ContinuityRecapGen)
	}
	if got := continuityRecapCount(ctx, t, continuityRecapOutcomeAttached) - attached; got != 1 {
		t.Fatalf("turn_continuity_recap_total{attached} moved by %d, want 1", got)
	}
	rig.assertMarked(ctx, t, 2)
}

// TestFreshLineage_SecondTurnCarriesNone: once the recap has reached the
// gen, its mark is cleared, and the turn after carries no recap.
func TestFreshLineage_SecondTurnCarriesNone(t *testing.T) {
	ctx := context.Background()
	rig := newLineageRig(ctx, t, lineageRigOptions{})
	rig.twoTurnsThenLost(ctx, t)
	rig.respawn(ctx, t, "and add a jitter")
	rig.bootGen(ctx, t, 2, false)
	recapOf(t, rig.commander.lastPrompt(t), "and add a jitter")

	rig.stream(ctx, t, 2, "Added the jitter.")
	rig.assertUnmarked(ctx, t, 2)
	deliver(ctx, t, rig.actor, completedEvent(2))

	fourth := rig.queue(ctx, t, "and document it")
	if p := rig.commander.lastPrompt(t); p.Text != "and document it" {
		t.Fatalf("the second turn's prompt text %q, want the request alone", p.Text)
	}
	if stored := rig.turn(ctx, t, fourth.ID); stored.ContinuityRecap != nil || stored.ContinuityRecapGen != nil {
		t.Fatalf("the second turn stored a recap %v for gen %v, want none", stored.ContinuityRecap, stored.ContinuityRecapGen)
	}
}

// TestFreshLineage_RecapReportsEachEarlierTurnsFinalText: the recap
// reports each earlier turn's request and its final text exactly as
// ReadWindowFinal reads it over the turn's window -- the extractor the plan
// notices and the session result use: a turn's last text part, never its
// narration before it, and "No report was recorded." for a turn that
// streamed no text.
func TestFreshLineage_RecapReportsEachEarlierTurnsFinalText(t *testing.T) {
	ctx := context.Background()
	rig := newLineageRig(ctx, t, lineageRigOptions{})
	rig.runTurn(ctx, t, 1, "find why the import is slow", "Let me look at the importer first.", "The import is slow because each row opens a transaction; batched them.")
	rig.runTurn(ctx, t, 1, "run the benchmarks")
	rig.runTurn(ctx, t, 1, "summarize", "Batched rows: the import runs in a tenth of the time.")
	rig.setStatus(ctx, t, sqlcgen.SandboxStatusFailed)
	rig.respawn(ctx, t, "open the pull request")
	rig.bootGen(ctx, t, 2, false)
	recap := recapOf(t, rig.commander.lastPrompt(t), "open the pull request")

	// The recap the extractor's own answers give.
	sessionTurns, err := rig.turns.ListForSession(ctx, rig.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var in continuity.Input
	number := 0
	for _, st := range sessionTurns {
		if st.DispatchedEventID == nil || stringOrEmpty(st.Prompt) == "open the pull request" {
			continue
		}
		number++
		lower, upper, ok := TurnContentBounds(sessionTurns, st.ID)
		if !ok {
			t.Fatalf("turn %q has no window", stringOrEmpty(st.Prompt))
		}
		final, found, err := ReadWindowFinal(ctx, narvipg.NewEventStore(rig.pool), rig.sessionID, lower, upper)
		if err != nil {
			t.Fatal(err)
		}
		in.Turns = append(in.Turns, continuity.Turn{Number: number, Status: turn.State(st.Status), EndReason: stringOrEmpty(st.EndReason),
			Request: stringOrEmpty(st.Prompt), Report: final, Reported: found})
	}
	in.Earlier = len(in.Turns)
	want, ok := continuity.Build(in, platform.ContinuityRecapMaxBytes)
	if !ok || len(in.Turns) != 3 {
		t.Fatalf("expected recap: %d turns, ok %v", len(in.Turns), ok)
	}
	if in.Turns[0].Report != (plandomain.Final{Text: "The import is slow because each row opens a transaction; batched them."}) || in.Turns[1].Reported {
		t.Fatalf("the extractor read %+v and %+v, want the first turn's last part and nothing for the second", in.Turns[0].Report, in.Turns[1])
	}
	if recap != want.Text {
		t.Fatalf("recap:\n%s\nwant the extractor's:\n%s", recap, want.Text)
	}
	if strings.Contains(recap, "Let me look at the importer first.") {
		t.Fatalf("recap %q quotes a turn's narration, want only its final text", recap)
	}
	if !strings.Contains(recap, "\nTurn 2 (completed)\n"+continuity.LineRequest+"\n> run the benchmarks\n"+continuity.LineNoReport+"\n") {
		t.Fatalf("recap %q, want the second turn reported as having recorded nothing", recap)
	}
}

// TestFreshLineage_ReenqueuedTurnCarriesItsOwnEarlierAttempt: a turn in
// flight on the lost gen, re-sent to the fresh one, carries the recap too
// -- the earlier turns, and what its own attempt on the lost gen had
// reported, quoted as the earlier instance's -- and names no conversation.
func TestFreshLineage_ReenqueuedTurnCarriesItsOwnEarlierAttempt(t *testing.T) {
	ctx := context.Background()
	rig := newLineageRig(ctx, t, lineageRigOptions{})
	rig.setConversationID(ctx, t, "conv-of-gen-1")
	rig.runTurn(ctx, t, 1, "add the retry to the importer", "Added the retry.")
	inFlight := rig.queue(ctx, t, "migrate the importer to the new client")
	rig.stream(ctx, t, 1, "Halfway: the client is swapped in two of the five call sites.")
	firstMessageID := stringOrEmpty(rig.turn(ctx, t, inFlight.ID).DispatchedMessageID)

	// Gen 1 is lost with the turn in flight; the next evaluation spawns a
	// fresh gen, and its boot re-sends the turn.
	rig.setStatus(ctx, t, sqlcgen.SandboxStatusFailed)
	settle(ctx, t, rig.actor)
	waitForConnecting(ctx, t, rig.pool, rig.sessionID)
	rig.assertMarked(ctx, t, 2)
	rig.bootGen(ctx, t, 2, false)

	p := rig.commander.lastPrompt(t)
	if p.Gen != 2 || p.MessageId == firstMessageID || p.ConversationId != nil {
		t.Fatalf("re-sent prompt: gen %d, messageId %q (first %q), conversation %v; want gen 2, a new messageId, no conversation", p.Gen, p.MessageId, firstMessageID, p.ConversationId)
	}
	recap := recapOf(t, p, "migrate the importer to the new client")
	wantAttempt := "\n" + continuity.LineInterrupted + "\n> Halfway: the client is swapped in two of the five call sites.\n"
	if !strings.Contains(recap, wantAttempt) || !strings.Contains(recap, "\nTurn 1 (completed)\n") || !strings.Contains(recap, "> Added the retry.") {
		t.Fatalf("recap %q, want turn 1 and the attempt %q", recap, wantAttempt)
	}
	if strings.Contains(recap, "Turn 2") {
		t.Fatalf("recap %q reports the re-sent turn as an earlier one", recap)
	}
	stored := rig.turn(ctx, t, inFlight.ID)
	if stored.Status != sqlcgen.TurnStatusProcessing || stringOrEmpty(stored.ContinuityRecap) != recap || stored.ContinuityRecapGen == nil || *stored.ContinuityRecapGen != 2 {
		t.Fatalf("re-sent turn %s with recap for gen %v, want processing with the recap sent, for gen 2", stored.Status, stored.ContinuityRecapGen)
	}
}

// sandboxAgentPlaceholderTokens are the tokens the sandbox agent replaces
// with live values over a prompt's whole text, one per replacement of each
// of its render passes (cmd/sandbox-agent: renderVerdictToolPromptText,
// renderUploadToolPromptText, renderEpistemicOutcomeToolPromptText,
// renderReviewCostBudgetToolPromptText,
// renderCompositionFindingsToolPromptText).
var sandboxAgentPlaceholderTokens = []string{
	review.VerdictToolURLPlaceholder, review.VerdictToolBearerPlaceholder, review.VerdictToolGenPlaceholder,
	review.VerdictToolDispatchMessageIDPlaceholder,
	domainupload.BaseURLPlaceholder, domainupload.BearerPlaceholder, domainupload.GenPlaceholder,
	turn.EpistemicOutcomeToolURLPlaceholder, turn.EpistemicOutcomeToolBearerPlaceholder, turn.EpistemicOutcomeToolGenPlaceholder,
	review.ReviewCostBudgetToolURLPlaceholder,
	review.CompositionFindingsToolURLPlaceholder, review.CompositionFindingsToolBearerPlaceholder, review.CompositionFindingsToolGenPlaceholder,
}

// TestFreshLineage_RecapCarriesNoPlaceholderToken: the recap strips every
// placeholder token the sandbox agent substitutes from every excerpt it
// quotes -- an earlier request, an earlier report and the interrupted
// attempt -- like every other path that quotes text into a prompt, so the
// prompt the fresh gen is sent carries none of them for the agent to
// replace with live values.
func TestFreshLineage_RecapCarriesNoPlaceholderToken(t *testing.T) {
	ctx := context.Background()
	carrying := func(what string) string {
		var b strings.Builder
		for _, tok := range sandboxAgentPlaceholderTokens {
			b.WriteString(what + " mentions " + tok + ".\n")
		}
		return b.String()
	}
	rig := newLineageRig(ctx, t, lineageRigOptions{})
	rig.runTurn(ctx, t, 1, carrying("The request"), carrying("The report"))
	inFlight := rig.queue(ctx, t, "carry on with the review")
	rig.stream(ctx, t, 1, carrying("The attempt"))
	rig.setStatus(ctx, t, sqlcgen.SandboxStatusFailed)
	settle(ctx, t, rig.actor)
	waitForConnecting(ctx, t, rig.pool, rig.sessionID)
	rig.bootGen(ctx, t, 2, false)

	p := rig.commander.lastPrompt(t)
	recap := recapOf(t, p, "carry on with the review")
	for _, what := range []string{"> The request mentions .", "> The report mentions .", "> The attempt mentions ."} {
		if !strings.Contains(recap, what) {
			t.Errorf("recap:\n%s\nwant the excerpt %q kept, its token stripped", recap, what)
		}
	}
	for _, tok := range sandboxAgentPlaceholderTokens {
		if strings.Contains(p.Text, tok) {
			t.Errorf("the prompt to the fresh gen carries %s", tok)
		}
	}
	if stored := rig.turn(ctx, t, inFlight.ID); strings.Contains(stringOrEmpty(stored.ContinuityRecap), "{{") {
		t.Errorf("the stored recap carries a token: %q", shortened(stringOrEmpty(stored.ContinuityRecap)))
	}
}

// TestFreshLineage_RecapQuotesThePersonsRequest: a turn's stored prompt
// can open with the control plane's own instructions -- the
// structured-plan instruction on a plan-mode turn, the epistemic preamble
// on a build turn when the check is on, each longer than a request
// excerpt -- or wrap a lane's own text in the review context composed
// around it; the recap quotes the request the person made, never the
// instructions or the context.
func TestFreshLineage_RecapQuotesThePersonsRequest(t *testing.T) {
	const asked = "Design a retry for the CSV importer: at most 3 attempts, never retry a 4xx."
	for _, tc := range []struct {
		name, prompt, injected, requestText string
	}{
		{name: "a plan-mode turn", prompt: plandomain.MaybeInjectStructureInstruction(true, asked), injected: plandomain.RenderStructureInstruction()},
		{name: "a build turn with the epistemic check on", prompt: turn.MaybeInjectEpistemicPreamble(true, nil, false, asked), injected: turn.RenderEpistemicPreamble()},
		{name: "a lane's own text", prompt: "Review context composed around the request.\n" + asked, injected: "Review context composed around the request.", requestText: asked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rig := newLineageRig(ctx, t, lineageRigOptions{})
			earlier := rig.runTurn(ctx, t, 1, tc.prompt, "Proposed a retry with a capped backoff.")
			if tc.requestText != "" {
				if _, err := rig.pool.Exec(ctx, `UPDATE turns SET request_text = $2 WHERE id = $1`, earlier.ID, tc.requestText); err != nil {
					t.Fatal(err)
				}
			}
			rig.setStatus(ctx, t, sqlcgen.SandboxStatusFailed)
			rig.respawn(ctx, t, "add a test for the 4xx case")
			rig.bootGen(ctx, t, 2, false)

			recap := recapOf(t, rig.commander.lastPrompt(t), "add a test for the 4xx case")
			want := "\nTurn 1 (completed)\n" + continuity.LineRequest + "\n> " + asked + "\n" + continuity.LineReport + "\n"
			if !strings.Contains(recap, want) {
				t.Fatalf("recap:\n%s\nwant the person's request quoted whole: %q", recap, want)
			}
			first, _, _ := strings.Cut(tc.injected, "\n")
			if strings.Contains(recap, first) {
				t.Fatalf("recap quotes the injected instructions:\n%s", recap)
			}
		})
	}
}

// TestFreshLineage_ReenqueueToARestoredGen_NoRecap: a turn that carried a
// recap to a fresh gen, re-sent to a gen restored after it, carries none:
// the stored recap was built for the gen it was sent to, and the restored
// gen owes none.
func TestFreshLineage_ReenqueueToARestoredGen_NoRecap(t *testing.T) {
	ctx := context.Background()
	rig := newLineageRig(ctx, t, lineageRigOptions{})
	rig.twoTurnsThenLost(ctx, t)
	third := rig.respawn(ctx, t, "and add a jitter")
	rig.bootGen(ctx, t, 2, false)
	recapOf(t, rig.commander.lastPrompt(t), "and add a jitter")

	// Gen 2 is lost, with the turn in flight and nothing heard of it, and a
	// snapshot of it recorded: the next claim restores it.
	rig.exec(ctx, t, `UPDATE sandboxes SET status = 'failed', snapshot_id = 'snap-of-gen-2' WHERE session_id = $1`)
	settle(ctx, t, rig.actor)
	waitForConnecting(ctx, t, rig.pool, rig.sessionID)
	if rig.provider.restoreCallCount() != 1 {
		t.Fatalf("restores %d, want 1", rig.provider.restoreCallCount())
	}
	rig.assertUnmarked(ctx, t, 3)
	rig.bootGen(ctx, t, 3, false)

	p := rig.commander.lastPrompt(t)
	if p.Gen != 3 || p.Text != "and add a jitter" {
		t.Fatalf("re-sent prompt to gen %d: text %q, want gen 3 and the request alone", p.Gen, p.Text)
	}
	if stored := rig.turn(ctx, t, third.ID); stored.ContinuityRecapGen == nil || *stored.ContinuityRecapGen != 2 {
		t.Fatalf("stored recap for gen %v, want gen 2's kept: the record of what that gen was told", stored.ContinuityRecapGen)
	}
}

// TestFreshLineage_MarkCountsOnlyForItsGen: a mark counts only for the gen
// it names. A marked gen lost before any turn reached it and restored from
// a snapshot leaves a restored gen with no mark at all -- the claim's
// upsert cleared it -- and the turn sent there carries no recap.
func TestFreshLineage_MarkCountsOnlyForItsGen(t *testing.T) {
	ctx := context.Background()
	rig := newLineageRig(ctx, t, lineageRigOptions{})
	rig.twoTurnsThenLost(ctx, t)
	rig.respawn(ctx, t, "and add a jitter")
	rig.assertMarked(ctx, t, 2)

	rig.exec(ctx, t, `UPDATE sandboxes SET status = 'failed', snapshot_id = 'snap-restorable' WHERE session_id = $1`)
	settle(ctx, t, rig.actor)
	waitForConnecting(ctx, t, rig.pool, rig.sessionID)
	if rig.provider.restoreCallCount() != 1 {
		t.Fatalf("restores %d, want 1", rig.provider.restoreCallCount())
	}
	if _, mark := rig.mark(ctx, t); mark != nil {
		t.Fatalf("fresh_lineage_gen %d after the restore's claim, want it cleared", *mark)
	}
	rig.bootGen(ctx, t, 3, false)
	if p := rig.commander.lastPrompt(t); p.Gen != 3 || p.Text != "and add a jitter" {
		t.Fatalf("prompt to gen %d: text %q, want gen 3 and the request alone", p.Gen, p.Text)
	}
}

// TestFreshLineage_RecapNeverPushesAPromptOverItsGensBound: on a gen held
// to 32 KiB, a long request leaves the recap less room than its own
// budget; the recap is cut to that room, the prompt fits the gen's bound
// and is sent, never refused, and the dispatch is counted trimmed.
func TestFreshLineage_RecapNeverPushesAPromptOverItsGensBound(t *testing.T) {
	ctx := context.Background()
	trimmed := continuityRecapCount(ctx, t, continuityRecapOutcomeTrimmed)
	rig := newLineageRig(ctx, t, lineageRigOptions{})
	report := strings.Repeat("All of it went as planned, and the tests pass. ", 90) // ~4 KiB
	for i := 0; i < 5; i++ {
		rig.runTurn(ctx, t, 1, "the next part of the work", report)
	}
	rig.setStatus(ctx, t, sqlcgen.SandboxStatusFailed)
	request := strings.Repeat("Port the next module, keeping the behavior. ", 450) // ~20 KiB
	long := rig.respawn(ctx, t, request)
	rig.bootGen(ctx, t, 2, false)

	if stored := rig.turn(ctx, t, long.ID); stored.Status != sqlcgen.TurnStatusProcessing {
		t.Fatalf("turn %s, want processing: sent, never refused", stored.Status)
	}
	p := rig.commander.lastPrompt(t)
	if len(p.raw) > platform.DefaultFrameReadLimitBytes {
		t.Fatalf("prompt frame of %d bytes, over the gen's %d", len(p.raw), platform.DefaultFrameReadLimitBytes)
	}
	recap := recapOf(t, p, request)
	if continuity.EncodedLen(recap) >= platform.ContinuityRecapMaxBytes {
		t.Fatalf("recap of %d encoded bytes, want it cut below its own budget to the room the request left", continuity.EncodedLen(recap))
	}
	if got := continuityRecapCount(ctx, t, continuityRecapOutcomeTrimmed) - trimmed; got != 1 {
		t.Fatalf("turn_continuity_recap_total{trimmed} moved by %d, want 1", got)
	}
}

// TestFreshLineage_NoRoom_TheNextTurnCarriesTheRecap: a request that
// leaves no room for even the recap's frame is sent without it, logged and
// counted no_room; the mark stays, an event of that turn does not clear
// it, and the next turn carries the recap.
func TestFreshLineage_NoRoom_TheNextTurnCarriesTheRecap(t *testing.T) {
	ctx := context.Background()
	logs := captureDefaultLoggerLocked(t)
	noRoom := continuityRecapCount(ctx, t, continuityRecapOutcomeNoRoom)
	trimmed := continuityRecapCount(ctx, t, continuityRecapOutcomeTrimmed)
	rig := newLineageRig(ctx, t, lineageRigOptions{})
	rig.twoTurnsThenLost(ctx, t)
	request := strings.Repeat("x", platform.DefaultFrameReadLimitBytes-1200)
	full := rig.respawn(ctx, t, request)
	rig.bootGen(ctx, t, 2, false)

	if p := rig.commander.lastPrompt(t); p.Text != request {
		t.Fatalf("the full turn's prompt text is %d bytes, want the request alone, %d", len(p.Text), len(request))
	}
	if stored := rig.turn(ctx, t, full.ID); stored.Status != sqlcgen.TurnStatusProcessing || stored.ContinuityRecap != nil {
		t.Fatalf("full turn %s with recap %v, want processing with none", stored.Status, stored.ContinuityRecap)
	}
	if got := continuityRecapCount(ctx, t, continuityRecapOutcomeNoRoom) - noRoom; got != 1 {
		t.Fatalf("turn_continuity_recap_total{no_room} moved by %d, want 1", got)
	}
	if line := logs.entry(t, continuityRecapNoRoomLogMessage); line["level"] != "WARN" || line["gen"] != float64(2) {
		t.Fatalf("no_room line = %v, want WARN at gen 2", line)
	}
	rig.stream(ctx, t, 2, "Done.")
	rig.assertMarked(ctx, t, 2)
	deliver(ctx, t, rig.actor, completedEvent(2))

	// The next turn carries the recap, the full turn's request in it cut to
	// its excerpt's cap.
	rig.queue(ctx, t, "and add a jitter")
	recap := recapOf(t, rig.commander.lastPrompt(t), "and add a jitter")
	if !strings.Contains(recap, fmt.Sprintf("[Cut to its first %d of %d bytes.]", continuity.RequestExcerptBytes, len(request))) {
		t.Fatalf("recap %q, want the full turn's request cut to its excerpt's cap", recap)
	}
	if got := continuityRecapCount(ctx, t, continuityRecapOutcomeTrimmed) - trimmed; got != 1 {
		t.Fatalf("turn_continuity_recap_total{trimmed} moved by %d, want 1", got)
	}
}

// TestFreshLineage_MarkClearedOnlyWhenThePromptReachesTheSandbox: the mark
// stays through the dispatch's commit and its send, and is cleared only
// by evidence the gen stores of the recap's prompt: its prompt_received on
// a gen that receipts prompts, or the first event of the turn on one that
// does not -- never a frame of the boot or the connection, nor a send that
// found no connection, nor a turn stopped before it was sent.
func TestFreshLineage_MarkClearedOnlyWhenThePromptReachesTheSandbox(t *testing.T) {
	setUp := func(ctx context.Context, t *testing.T) *lineageRig {
		t.Helper()
		rig := newLineageRig(ctx, t, lineageRigOptions{})
		rig.twoTurnsThenLost(ctx, t)
		return rig
	}

	t.Run("ReceiptCapableGen_StaysUntilPromptReceived", func(t *testing.T) {
		ctx := context.Background()
		logs := captureDefaultLoggerLocked(t)
		delivered := continuityRecapCount(ctx, t, continuityRecapOutcomeDelivered)
		rig := setUp(ctx, t)
		third := rig.respawn(ctx, t, "and add a jitter")
		rig.bootGen(ctx, t, 2, true)
		p := rig.commander.lastPrompt(t)
		recapOf(t, p, "and add a jitter")
		if !p.asksReceipt(t) {
			t.Fatal("the recap's prompt asks for no receipt, want one from a capable gen")
		}
		rig.assertMarked(ctx, t, 2)
		deliver(ctx, t, rig.actor, receiptHeartbeat(2))
		deliver(ctx, t, rig.actor, promptReceivedEvent("another-prompt", 2, false))
		rig.assertMarked(ctx, t, 2)

		deliver(ctx, t, rig.actor, promptReceivedEvent(stringOrEmpty(rig.turn(ctx, t, third.ID).DispatchedMessageID), 2, false))
		rig.assertUnmarked(ctx, t, 2)
		if got := continuityRecapCount(ctx, t, continuityRecapOutcomeDelivered) - delivered; got != 1 {
			t.Fatalf("turn_continuity_recap_total{delivered} moved by %d, want 1", got)
		}
		if line := logs.entry(t, continuityRecapDeliveredLogMessage); line["event_type"] != "prompt_received" || line["gen"] != float64(2) {
			t.Fatalf("delivered line = %v, want the prompt_received at gen 2", line)
		}
	})

	t.Run("IncapableGen_ClearedByTheFirstTurnEvent", func(t *testing.T) {
		ctx := context.Background()
		rig := setUp(ctx, t)
		rig.respawn(ctx, t, "and add a jitter")
		rig.bootGen(ctx, t, 2, false)
		recapOf(t, rig.commander.lastPrompt(t), "and add a jitter")
		rig.assertMarked(ctx, t, 2)
		deliver(ctx, t, rig.actor, lineageStepStart(t, rig.sessionID, 2))
		rig.assertUnmarked(ctx, t, 2)
	})

	// The agent's own execution_complete is evidence too, cleared before
	// it completes the turn: a prompt the runtime ends at once sends
	// nothing else.
	t.Run("IncapableGen_ClearedByAnExecutionCompleteAlone", func(t *testing.T) {
		ctx := context.Background()
		rig := setUp(ctx, t)
		third := rig.respawn(ctx, t, "and add a jitter")
		rig.bootGen(ctx, t, 2, false)
		recapOf(t, rig.commander.lastPrompt(t), "and add a jitter")
		rig.assertMarked(ctx, t, 2)
		deliver(ctx, t, rig.actor, completedEvent(2))
		rig.assertUnmarked(ctx, t, 2)
		if stored := rig.turn(ctx, t, third.ID); stored.Status != sqlcgen.TurnStatusCompleted {
			t.Fatalf("recap turn %s after its execution_complete, want completed", stored.Status)
		}
		rig.queue(ctx, t, "and document it")
		if p := rig.commander.lastPrompt(t); p.Text != "and document it" {
			t.Fatalf("the next turn's prompt text %q, want the request alone: no second recap", shortened(p.Text))
		}
	})

	t.Run("BootFramesNeverClearIt", func(t *testing.T) {
		ctx := context.Background()
		rig := setUp(ctx, t)
		rig.respawn(ctx, t, "and add a jitter")
		rig.bootGen(ctx, t, 2, false)
		for _, frame := range []SandboxEvent{
			receiptReady(2, false),
			receiptHeartbeat(2),
			bootProgressEvent("bp-"+uuid.NewString(), 2, "setup"),
			gitSyncEvent("gs-"+uuid.NewString(), 2),
			bootTimingEvent("bt-"+uuid.NewString(), 2, "boot_duration", "false"),
			{Type: "warning", Gen: 2, MessageID: "w-" + uuid.NewString(), Raw: json.RawMessage(`{"type":"warning","messageId":"w","sessionId":"s","gen":2,"message":"a boot warning"}`)},
		} {
			deliver(ctx, t, rig.actor, frame)
			rig.assertMarked(ctx, t, 2)
		}
	})

	t.Run("UndeliveredSend_KeepsIt", func(t *testing.T) {
		ctx := context.Background()
		rig := setUp(ctx, t)
		rig.commander.setPromptErr(ports.ErrNoLiveSandboxConnection)
		third := rig.respawn(ctx, t, "and add a jitter")
		rig.bootGen(ctx, t, 2, false)
		recapOf(t, rig.commander.lastPrompt(t), "and add a jitter")
		if stored := rig.turn(ctx, t, third.ID); stored.Status != sqlcgen.TurnStatusFailed {
			t.Fatalf("turn %s after a send that found no connection, want failed", stored.Status)
		}
		rig.assertMarked(ctx, t, 2)

		rig.commander.setPromptErr(nil)
		rig.queue(ctx, t, "try again")
		recapOf(t, rig.commander.lastPrompt(t), "try again")
		rig.assertMarked(ctx, t, 2)
	})

	t.Run("StoppedBeforeSending_KeepsIt", func(t *testing.T) {
		ctx := context.Background()
		rig := setUp(ctx, t)
		third := rig.respawn(ctx, t, "and add a jitter")
		// A person's stop reaches the turn before its sandbox is up: it is
		// cancelled at the dispatch gate, never sent.
		if _, err := rig.pool.Exec(ctx, `UPDATE turns SET stop_requested_at = now() WHERE id = $1`, third.ID); err != nil {
			t.Fatal(err)
		}
		sent := len(rig.commander.prompts(t))
		rig.bootGen(ctx, t, 2, false)
		if got := len(rig.commander.prompts(t)); got != sent {
			t.Fatalf("%d prompts sent for a stopped turn, want none", got-sent)
		}
		if stored := rig.turn(ctx, t, third.ID); stored.Status != sqlcgen.TurnStatusCancelled {
			t.Fatalf("stopped turn %s, want cancelled", stored.Status)
		}
		rig.assertMarked(ctx, t, 2)

		rig.queue(ctx, t, "carry on instead")
		recapOf(t, rig.commander.lastPrompt(t), "carry on instead")
	})
}

// TestFreshLineage_RestoreAfterAFreshSpawnNamesTheSnapshotsConversation: a
// fresh spawn forgets the session's conversation but can keep the snapshot
// it did not restore -- a slow restore given up on, a restore downgraded
// -- and a later claim can restore that snapshot after all. The restored
// runtime holds the conversation the snapshot was minted under, recorded
// with it, and the restored gen's prompts name it: never none, and never
// one the fresh lineage opened. The restore is not marked and carries no
// recap. A snapshot whose conversation was not recorded leaves the
// session naming what it named, none here: nothing says what it holds.
func TestFreshLineage_RestoreAfterAFreshSpawnNamesTheSnapshotsConversation(t *testing.T) {
	const (
		snapshotID   = "snap-of-gen-1"
		conversation = "conv-of-gen-1"
	)
	stuckRestore := `UPDATE sandboxes SET status = 'spawning', snapshot_id = '` + snapshotID + `',
		created_at = now() - interval '10 minutes', last_seen_at = now() - interval '10 minutes' WHERE session_id = $1`
	recorded := `UPDATE sandboxes SET snapshot_conversation_id = '` + conversation + `', snapshot_conversation_recorded_id = snapshot_id WHERE session_id = $1`
	seedWith := func(sqls ...string) func(ctx context.Context, t *testing.T, rig *lineageRig) {
		return func(ctx context.Context, t *testing.T, rig *lineageRig) {
			rig.seedEarlierTurn(ctx, t, 1, sqlcgen.TurnStatusCompleted)
			rig.setConversationID(ctx, t, conversation)
			for _, sql := range sqls {
				rig.exec(ctx, t, sql)
			}
		}
	}
	// restoreGen3 loses gen 2 and lets the next evaluation restore the
	// kept snapshot as gen 3, then boots it.
	restoreGen3 := func(ctx context.Context, t *testing.T, rig *lineageRig) {
		t.Helper()
		rig.setStatus(ctx, t, sqlcgen.SandboxStatusFailed)
		settle(ctx, t, rig.actor)
		waitForConnecting(ctx, t, rig.pool, rig.sessionID)
		if rig.provider.restoreCallCount() != 1 || string(rig.provider.lastRestoreCall().snapshotID) != snapshotID {
			t.Fatalf("restores %d, want %s restored once", rig.provider.restoreCallCount(), snapshotID)
		}
		rig.assertUnmarked(ctx, t, 3)
	}

	t.Run("StuckRespawnThenRestore", func(t *testing.T) {
		ctx := context.Background()
		rig := newLineageRig(ctx, t, lineageRigOptions{
			provider: &fakeSpawnProvider{nextErr: &ports.ProviderError{Transient: false, Code: "INVALID_IMAGE", Op: ports.OpCreateSandbox, Err: errors.New("boom")}},
			seed:     seedWith(stuckRestore, recorded),
		})
		rig.queue(ctx, t, "carry on")
		waitUntil(t, 5*time.Second, func() bool { return rig.sandbox(ctx, t).Status == sqlcgen.SandboxStatusSuspect })
		rig.assertMarked(ctx, t, 2)
		if id := rig.conversationID(ctx, t); id != nil {
			t.Fatalf("conversation %q after the fresh spawn, want none", *id)
		}
		rig.provider.mu.Lock()
		rig.provider.nextErr = nil
		rig.provider.mu.Unlock()

		restoreGen3(ctx, t, rig)
		if id := rig.conversationID(ctx, t); stringOrEmpty(id) != conversation {
			t.Fatalf("the session names %v after the restore, want %s, the snapshot's", id, conversation)
		}
		rig.bootGen(ctx, t, 3, false)
		if p := rig.commander.lastPrompt(t); p.Gen != 3 || stringOrEmpty(p.ConversationId) != conversation || p.Text != "carry on" {
			t.Fatalf("prompt to gen %d names %v with text %q, want gen 3 naming %s with the request alone", p.Gen, p.ConversationId, shortened(p.Text), conversation)
		}
	})

	t.Run("RecapLostThenRestore", func(t *testing.T) {
		ctx := context.Background()
		rig := newLineageRig(ctx, t, lineageRigOptions{seed: seedWith(stuckRestore, recorded)})
		inFlight := rig.respawn(ctx, t, "carry on")
		rig.bootGen(ctx, t, 2, false)
		if p := rig.commander.lastPrompt(t); p.ConversationId != nil {
			t.Fatalf("the fresh gen's prompt names %q, want none", *p.ConversationId)
		}
		recapOf(t, rig.commander.lastPrompt(t), "carry on")

		// Gen 2 is lost before any event of the turn: the restore re-sends it.
		restoreGen3(ctx, t, rig)
		rig.bootGen(ctx, t, 3, false)
		p := rig.commander.lastPrompt(t)
		if p.Gen != 3 || stringOrEmpty(p.ConversationId) != conversation || p.Text != "carry on" {
			t.Fatalf("re-sent prompt to gen %d names %v with text %q, want gen 3 naming %s with the request alone", p.Gen, p.ConversationId, shortened(p.Text), conversation)
		}
		if stored := rig.turn(ctx, t, inFlight.ID); stored.Status != sqlcgen.TurnStatusProcessing || stringOrEmpty(stored.DispatchedMessageID) != p.MessageId {
			t.Fatalf("turn %s, want processing, re-sent to gen 3", stored.Status)
		}
	})

	t.Run("DowngradeThenRestore", func(t *testing.T) {
		ctx := context.Background()
		rig := newLineageRig(ctx, t, lineageRigOptions{
			session: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
				// A repository with no settings resolves shadow (§30.4).
				return createTestSessionWithRepos(ctx, t, pool, pgtype.UUID{}, "repo", "https://github.com/acme/repo.git", "main")
			},
			seed: seedWith(`UPDATE sandboxes SET status = 'stopped', snapshot_id = '`+snapshotID+`' WHERE session_id = $1`, recorded),
		})
		// The shadow session's restore is downgraded: gen 2 spawns fresh,
		// the snapshot kept, and runs a turn under a conversation of its own.
		rig.respawn(ctx, t, "carry on")
		if rig.provider.callCount() != 1 || rig.provider.restoreCallCount() != 0 {
			t.Fatalf("fresh spawns %d, restores %d; want the restore downgraded", rig.provider.callCount(), rig.provider.restoreCallCount())
		}
		rig.bootGen(ctx, t, 2, false)
		opened := "conv-of-gen-2"
		deliver(ctx, t, rig.actor, SandboxEvent{Type: "heartbeat", Gen: 2, MessageID: "hb-" + uuid.NewString(), ConversationID: &opened,
			Raw: json.RawMessage(`{"type":"heartbeat","messageId":"hb","sessionId":"s","gen":2,"conversationId":"conv-of-gen-2","lastBootPhase":null}`)})
		rig.stream(ctx, t, 2, "Carried on.")
		deliver(ctx, t, rig.actor, completedEvent(2))
		if id := rig.conversationID(ctx, t); stringOrEmpty(id) != opened {
			t.Fatalf("the session names %v, want %s, the fresh gen's", id, opened)
		}

		// The session leaves shadow mode, and gen 2 is lost with no snapshot
		// of its own: the kept snapshot is restored after all.
		if _, err := narvipg.NewRepoSettingsStore(rig.pool).UpsertLiveEgressEnabled(ctx, "acme/repo", true); err != nil {
			t.Fatal(err)
		}
		createTurnArmingDispatch(ctx, t, rig.pool, rig.sessionID, "and next")
		restoreGen3(ctx, t, rig)
		if id := rig.conversationID(ctx, t); stringOrEmpty(id) != conversation {
			t.Fatalf("the session names %v after the restore, want %s, the snapshot's", id, conversation)
		}
		rig.bootGen(ctx, t, 3, false)
		if p := rig.commander.lastPrompt(t); p.Gen != 3 || stringOrEmpty(p.ConversationId) != conversation || p.Text != "and next" {
			t.Fatalf("prompt to gen %d names %v with text %q, want gen 3 naming %s with the request alone", p.Gen, p.ConversationId, shortened(p.Text), conversation)
		}
	})

	t.Run("UnrecordedSnapshotNamesWhatTheSessionNames", func(t *testing.T) {
		ctx := context.Background()
		rig := newLineageRig(ctx, t, lineageRigOptions{
			provider: &fakeSpawnProvider{nextErr: &ports.ProviderError{Transient: false, Code: "INVALID_IMAGE", Op: ports.OpCreateSandbox, Err: errors.New("boom")}},
			seed:     seedWith(stuckRestore),
		})
		rig.queue(ctx, t, "carry on")
		waitUntil(t, 5*time.Second, func() bool { return rig.sandbox(ctx, t).Status == sqlcgen.SandboxStatusSuspect })
		rig.provider.mu.Lock()
		rig.provider.nextErr = nil
		rig.provider.mu.Unlock()
		restoreGen3(ctx, t, rig)
		if id := rig.conversationID(ctx, t); id != nil {
			t.Fatalf("the session names %q after restoring a snapshot with no record, want none, as the fresh spawn left it", *id)
		}
		rig.bootGen(ctx, t, 3, false)
		if p := rig.commander.lastPrompt(t); p.ConversationId != nil || p.Text != "carry on" {
			t.Fatalf("prompt names %v with text %q, want none and the request alone", p.ConversationId, shortened(p.Text))
		}
	})
}

// TestFreshLineage_ReceiptResendCarriesTheSameRecap: a receipt re-send of
// the recap's prompt, after the gen reconnects with no receipt stored,
// sends the same bytes -- the recap stored with the turn, not one rebuilt
// from a record that moved since.
func TestFreshLineage_ReceiptResendCarriesTheSameRecap(t *testing.T) {
	ctx := context.Background()
	rig := newLineageRig(ctx, t, lineageRigOptions{})
	first, _ := rig.twoTurnsThenLost(ctx, t)
	rig.respawn(ctx, t, "and add a jitter")
	rig.bootGen(ctx, t, 2, true)
	original := rig.commander.lastPrompt(t)
	recapOf(t, original, "and add a jitter")

	// The record moves: a recap built now would differ.
	if _, err := rig.pool.Exec(ctx, `UPDATE turns SET prompt = 'a request rewritten since' WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	deliver(ctx, t, rig.actor, receiptReady(2, true))
	prompts := rig.commander.prompts(t)
	resent := prompts[len(prompts)-1]
	if len(prompts) < 2 || resent.MessageId != original.MessageId {
		t.Fatalf("no receipt re-send of %s after the reconnect", original.MessageId)
	}
	if !bytes.Equal(resent.raw, original.raw) {
		t.Fatalf("re-sent prompt differs from the original:\n%s\n%s", resent.raw, original.raw)
	}
}

// TestFreshLineage_FirstPromptNamesNoConversation_TheHeartbeatRecordsTheNewOne:
// the fresh spawn forgets the session's conversation id, so the first
// prompt to the fresh gen names none; the agent opens one, its heartbeat
// records it, and the next prompt names that one.
func TestFreshLineage_FirstPromptNamesNoConversation_TheHeartbeatRecordsTheNewOne(t *testing.T) {
	ctx := context.Background()
	rig := newLineageRig(ctx, t, lineageRigOptions{})
	rig.setConversationID(ctx, t, "conv-of-gen-1")
	rig.runTurn(ctx, t, 1, "add the retry to the importer", "Added the retry.")
	if p := rig.commander.lastPrompt(t); stringOrEmpty(p.ConversationId) != "conv-of-gen-1" {
		t.Fatalf("gen 1's prompt names %v, want conv-of-gen-1", p.ConversationId)
	}
	rig.setStatus(ctx, t, sqlcgen.SandboxStatusFailed)
	rig.respawn(ctx, t, "and add a jitter")
	if id := rig.conversationID(ctx, t); id != nil {
		t.Fatalf("conversation id %q after the fresh spawn, want none", *id)
	}
	rig.bootGen(ctx, t, 2, false)
	if p := rig.commander.lastPrompt(t); p.ConversationId != nil {
		t.Fatalf("the fresh gen's first prompt names %q, want no conversation", *p.ConversationId)
	}

	opened := "conv-of-gen-2"
	deliver(ctx, t, rig.actor, SandboxEvent{Type: "heartbeat", Gen: 2, MessageID: "hb-" + uuid.NewString(), ConversationID: &opened,
		Raw: json.RawMessage(`{"type":"heartbeat","messageId":"hb","sessionId":"s","gen":2,"conversationId":"conv-of-gen-2","lastBootPhase":null}`)})
	if id := rig.conversationID(ctx, t); stringOrEmpty(id) != "conv-of-gen-2" {
		t.Fatalf("conversation id %v after the fresh gen's heartbeat, want conv-of-gen-2", id)
	}
	rig.stream(ctx, t, 2, "Added the jitter.")
	deliver(ctx, t, rig.actor, completedEvent(2))
	rig.queue(ctx, t, "and document it")
	if p := rig.commander.lastPrompt(t); stringOrEmpty(p.ConversationId) != "conv-of-gen-2" || p.Text != "and document it" {
		t.Fatalf("next prompt names %v with text %q, want conv-of-gen-2 and the request alone", p.ConversationId, p.Text)
	}
}
