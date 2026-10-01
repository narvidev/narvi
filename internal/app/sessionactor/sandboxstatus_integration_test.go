//go:build integration

package sessionactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/platform"
)

// lockedBroadcaster records every broadcast payload under a mutex: the
// actor goroutine writes while the test reads, and work an actor runs
// after a command's reply may broadcast too.
type lockedBroadcaster struct {
	mu       sync.Mutex
	payloads []string
}

func (b *lockedBroadcaster) Broadcast(_ string, payload json.RawMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.payloads = append(b.payloads, string(payload))
}

// reports returns the sandbox_status payloads broadcast so far, as
// sandboxStatusReport reads them, in order.
func (b *lockedBroadcaster) reports() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, p := range b.payloads {
		if r := sandboxStatusReport(p); r != "" {
			out = append(out, r)
		}
	}
	return out
}

// storedSandboxStatus is one sandbox_status event as the events table
// holds it, with the message id of the event stored just before it.
type storedSandboxStatus struct {
	payload         string
	previousMessage string
}

// sandboxStatusEvents returns the session's sandbox_status events, oldest
// first.
func sandboxStatusEvents(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) []storedSandboxStatus {
	t.Helper()
	rows, err := narvipg.NewEventStore(pool).ListForSession(ctx, sessionID, 0, 1000)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var out []storedSandboxStatus
	previous := ""
	for _, row := range rows {
		if row.Type == SandboxStatusEventType {
			out = append(out, storedSandboxStatus{payload: string(row.Payload), previousMessage: previous})
		}
		previous = row.MessageID
	}
	return out
}

// sandboxStatusReport reads one sandbox_status payload as "gen/status",
// or "" when it is not one: the events table stores JSONB, which respaces
// what the actor marshaled, so payloads are compared decoded.
func sandboxStatusReport(payload string) string {
	var p struct {
		Sandbox *struct {
			Gen    *int    `json:"gen"`
			Status *string `json:"status"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil || p.Sandbox == nil || p.Sandbox.Gen == nil || p.Sandbox.Status == nil {
		return ""
	}
	return fmt.Sprintf("%d/%s", *p.Sandbox.Gen, *p.Sandbox.Status)
}

func sandboxStatusPayloads(events []storedSandboxStatus) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, sandboxStatusReport(e.payload))
	}
	return out
}

// newSandboxStatusActor runs seed (nil seeds nothing) before the actor
// hydrates, so hydration reads what it wrote, and returns the actor with
// its broadcaster.
func newSandboxStatusActor(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, seed func(*narvipg.SandboxStore) error) (*Actor, *lockedBroadcaster) {
	t.Helper()
	if seed != nil {
		if err := seed(narvipg.NewSandboxStore(pool)); err != nil {
			t.Fatalf("seed sandbox: %v", err)
		}
	}
	b := &lockedBroadcaster{}
	r, err := NewRegistry(ctx, pool, platform.DefaultTimeouts(), b, nil, nil, "", nil, nil, "", nil, false)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Shutdown() })
	a, err := r.GetOrSpawn(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetOrSpawn: %v", err)
	}
	return a, b
}

// TestSandboxStatusEvent_BootReportsTheServersStatus drives a boot the way
// a sandbox-agent sends it and reads what an open page receives (technical
// plan §3.2, §6.2): the agent's `ready` moves the sandbox to booting, not
// ready, and the event reporting ready follows only the null-phase
// heartbeat after boot evidence -- each stored right after the event that
// caused it, in its transaction, and broadcast. An event that changes no
// status reports nothing.
func TestSandboxStatusEvent_BootReportsTheServersStatus(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	sessionID := createTestSession(ctx, t, pool)
	a, broadcaster := newSandboxStatusActor(ctx, t, pool, sessionID, func(s *narvipg.SandboxStore) error {
		if _, err := s.Create(ctx, sessionID); err != nil {
			return err
		}
		_, err := s.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: sessionID, Status: sqlcgen.SandboxStatusConnecting})
		return err
	})

	const booting = "1/booting"
	const ready = "1/ready"
	steps := []struct {
		name    string
		event   SandboxEvent
		wantNew string
	}{
		{name: "the agent's ready moves the sandbox to booting, not ready", event: readyEvent("r", 1), wantNew: booting},
		{name: "a boot phase changes no status", event: bootProgressEvent("bp", 1, "clone"), wantNew: ""},
		{name: "a heartbeat still carrying a phase changes no status", event: phaseHeartbeat("hp", 1, "hooks"), wantNew: ""},
		{name: "the null-phase heartbeat after evidence marks it ready", event: nullPhaseHeartbeat("h1", 1), wantNew: ready},
		{name: "a heartbeat on a ready sandbox changes nothing", event: nullPhaseHeartbeat("h2", 1), wantNew: ""},
	}

	var want []string
	for _, step := range steps {
		if outcome := sendSandboxEventForTest(ctx, t, a, step.event); !outcome.Persisted {
			t.Fatalf("%s: %s %s not persisted", step.name, step.event.Type, step.event.MessageID)
		}
		if step.wantNew != "" {
			want = append(want, step.wantNew)
		}
		events := sandboxStatusEvents(ctx, t, pool, sessionID)
		if got := sandboxStatusPayloads(events); !slices.Equal(got, want) {
			t.Fatalf("%s: sandbox_status events = %v, want %v", step.name, got, want)
		}
		if step.wantNew == "" {
			continue
		}
		if cause := events[len(events)-1].previousMessage; cause != step.event.MessageID {
			t.Errorf("%s: the event stored before the report is %q, want the %s %q that caused it", step.name, cause, step.event.Type, step.event.MessageID)
		}
		if !slices.Contains(broadcaster.reports(), step.wantNew) {
			t.Errorf("%s: %s was stored but never broadcast to an open page", step.name, step.wantNew)
		}
	}
}

// TestSandboxStatusEvent_OnlyACommittedChangeIsReported pins the rule
// transact applies (sandboxstatus.go): one event per transaction that
// leaves the sandbox at another generation or status than the last commit
// did, reporting where it ended; nothing for a write that changes neither,
// for a passage through a status and back, or for a write that rolls back
// -- after which the same change, committed, is still reported.
func TestSandboxStatusEvent_OnlyACommittedChangeIsReported(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)

	type txFn = func(context.Context, pgx.Tx) error
	readyAtGen1 := func(sessionID pgtype.UUID) func(*narvipg.SandboxStore) error {
		return func(s *narvipg.SandboxStore) error {
			if _, err := s.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID}); err != nil {
				return err
			}
			_, err := s.UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: sessionID, Status: sqlcgen.SandboxStatusReady})
			return err
		}
	}
	spawningAtGen1 := func(sessionID pgtype.UUID) func(*narvipg.SandboxStore) error {
		return func(s *narvipg.SandboxStore) error {
			_, err := s.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID})
			return err
		}
	}
	noSandbox := func(pgtype.UUID) func(*narvipg.SandboxStore) error { return nil }
	setStatus := func(a *Actor, status sqlcgen.SandboxStatus) txFn {
		return func(ctx context.Context, tx pgx.Tx) error {
			_, err := a.sandboxWrites(tx).UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{SessionID: a.sessionID, Status: status})
			return err
		}
	}
	toSuspect := func(a *Actor) txFn {
		return func(ctx context.Context, tx pgx.Tx) error {
			pre := sqlcgen.SandboxStatusReady
			_, err := a.sandboxWrites(tx).UpdateStatusToSuspect(ctx, sqlcgen.UpdateSandboxStatusToSuspectParams{SessionID: a.sessionID, PreSuspectStatus: &pre})
			return err
		}
	}
	respawn := func(a *Actor) txFn {
		return func(ctx context.Context, tx pgx.Tx) error {
			_, err := a.sandboxWrites(tx).UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: a.sessionID})
			return err
		}
	}
	both := func(fns ...txFn) txFn {
		return func(ctx context.Context, tx pgx.Tx) error {
			for _, fn := range fns {
				if err := fn(ctx, tx); err != nil {
					return err
				}
			}
			return nil
		}
	}
	errRolledBack := errors.New("rolled back")

	tests := []struct {
		name string
		seed func(pgtype.UUID) func(*narvipg.SandboxStore) error
		// transacts returns one function per transact, run in order; want
		// lists the sandbox_status payloads the session holds afterwards.
		transacts func(a *Actor) []txFn
		want      []string
	}{
		{
			name:      "a status change",
			seed:      readyAtGen1,
			transacts: func(a *Actor) []txFn { return []txFn{setStatus(a, sqlcgen.SandboxStatusSnapshotting)} },
			want:      []string{"1/snapshotting"},
		},
		{
			name: "a write that changes neither generation nor status",
			seed: readyAtGen1,
			transacts: func(a *Actor) []txFn {
				return []txFn{func(ctx context.Context, tx pgx.Tx) error {
					_, err := a.sandboxWrites(tx).UpdateStatus(ctx, sqlcgen.UpdateSandboxStatusParams{
						SessionID:  a.sessionID,
						Status:     sqlcgen.SandboxStatusReady,
						LastSeenAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
					})
					return err
				}}
			},
			want: nil,
		},
		{
			name:      "a respawn: a new generation at the same status",
			seed:      spawningAtGen1,
			transacts: func(a *Actor) []txFn { return []txFn{respawn(a)} },
			want:      []string{"2/spawning"},
		},
		{
			name:      "the session's first sandbox",
			seed:      noSandbox,
			transacts: func(a *Actor) []txFn { return []txFn{respawn(a)} },
			want:      []string{"1/spawning"},
		},
		{
			name: "a passage through suspect and back to ready",
			seed: readyAtGen1,
			transacts: func(a *Actor) []txFn {
				return []txFn{both(toSuspect(a), func(ctx context.Context, tx pgx.Tx) error {
					_, err := a.sandboxWrites(tx).RecoverFromSuspect(ctx, sqlcgen.RecoverSandboxFromSuspectParams{
						SessionID:  a.sessionID,
						Status:     sqlcgen.SandboxStatusReady,
						LastSeenAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
					})
					return err
				})}
			},
			want: nil,
		},
		{
			name:      "several writes in one transaction report where it ended, once",
			seed:      readyAtGen1,
			transacts: func(a *Actor) []txFn { return []txFn{both(toSuspect(a), setStatus(a, sqlcgen.SandboxStatusStopped))} },
			want:      []string{"1/stopped"},
		},
		{
			name: "a rolled-back change, then the same change committed",
			seed: readyAtGen1,
			transacts: func(a *Actor) []txFn {
				return []txFn{
					both(setStatus(a, sqlcgen.SandboxStatusSnapshotting), func(context.Context, pgx.Tx) error { return errRolledBack }),
					setStatus(a, sqlcgen.SandboxStatusSnapshotting),
				}
			},
			want: []string{"1/snapshotting"},
		},
		{
			name: "a change, then a return to the status hydration read",
			seed: readyAtGen1,
			transacts: func(a *Actor) []txFn {
				return []txFn{setStatus(a, sqlcgen.SandboxStatusSnapshotting), setStatus(a, sqlcgen.SandboxStatusReady)}
			},
			want: []string{"1/snapshotting", "1/ready"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			a, broadcaster := newSandboxStatusActor(ctx, t, pool, sessionID, tc.seed(sessionID))
			// Direct transact calls, as TestActorTransact_BroadcastsOnlyAfterCommit
			// makes: the actor is idle, no command or timer of its own runs.
			for i, fn := range tc.transacts(a) {
				if err := a.transact(ctx, fn); err != nil && !errors.Is(err, errRolledBack) {
					t.Fatalf("transact %d: %v", i, err)
				}
			}
			if got := sandboxStatusPayloads(sandboxStatusEvents(ctx, t, pool, sessionID)); !slices.Equal(got, tc.want) {
				t.Errorf("sandbox_status events = %v, want %v", got, tc.want)
			}
			broadcast := broadcaster.reports()
			if !slices.Equal(broadcast, tc.want) {
				t.Errorf("sandbox_status broadcasts = %v, want %v", broadcast, tc.want)
			}
		})
	}
}
