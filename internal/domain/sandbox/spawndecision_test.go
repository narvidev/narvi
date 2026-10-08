package sandbox_test

import (
	"strings"
	"testing"
	"time"

	"github.com/narvidev/narvi/internal/domain/sandbox"
)

// TestEvaluateSpawnDecision exercises EvaluateSpawnDecision's scenarios:
// restore/resume priority, the interrupted-spawn recovery case, the
// ready-without-websocket wait, cooldown bypass for failed/stopped, the
// in-memory skip guard, the persistent-resume sub-suite, the
// Spawning/Connecting/Booting "already booting" skip guard, and Suspect's
// own unconditional skip case (no staleness carve-out).
func TestEvaluateSpawnDecision(t *testing.T) {
	t.Parallel()

	cfg := sandbox.SpawnConfig{
		Cooldown:        30 * time.Second,
		ReadyWait:       60 * time.Second,
		SpawningTimeout: 120 * time.Second,
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name                     string
		state                    sandbox.SpawnState
		isSpawningInMemory       bool
		supportsPersistentResume bool
		wantKind                 sandbox.SpawnActionKind
		wantContains             string // substring expected in Reason/ProviderObjectID/SnapshotImageID
	}{
		{
			name: "restore when snapshot exists and sandbox is stopped",
			state: sandbox.SpawnState{
				Status: sandbox.StateStopped, CreatedAt: now.Add(-120 * time.Second),
				SnapshotImageID: "img-abc123",
			},
			wantKind:     sandbox.SpawnActionRestore,
			wantContains: "img-abc123",
		},
		{
			name: "restore when snapshot exists and sandbox is stale",
			state: sandbox.SpawnState{
				Status: sandbox.StateStale, CreatedAt: now.Add(-120 * time.Second),
				SnapshotImageID: "img-abc123",
			},
			wantKind: sandbox.SpawnActionRestore,
		},
		{
			name: "restore when snapshot exists and sandbox is failed",
			state: sandbox.SpawnState{
				Status: sandbox.StateFailed, CreatedAt: now.Add(-120 * time.Second),
				SnapshotImageID: "img-abc123",
			},
			wantKind: sandbox.SpawnActionRestore,
		},
		{
			name: "skip when already spawning",
			state: sandbox.SpawnState{
				Status: sandbox.StateSpawning, CreatedAt: now.Add(-5 * time.Second),
			},
			wantKind:     sandbox.SpawnActionSkip,
			wantContains: "spawning",
		},
		{
			name: "skip when connecting",
			state: sandbox.SpawnState{
				Status: sandbox.StateConnecting, CreatedAt: now.Add(-5 * time.Second),
			},
			wantKind: sandbox.SpawnActionSkip,
		},
		{
			name: "skip when booting (Narvi's own third boot phase)",
			state: sandbox.SpawnState{
				Status: sandbox.StateBooting, CreatedAt: now.Add(-5 * time.Second),
			},
			wantKind: sandbox.SpawnActionSkip,
		},
		{
			name: "skip when suspect, even long past every other window (Narvi's own addition, no TS equivalent)",
			state: sandbox.SpawnState{
				Status: sandbox.StateSuspect, CreatedAt: now.Add(-(cfg.SpawningTimeout + cfg.Cooldown + time.Hour)),
			},
			wantKind:     sandbox.SpawnActionSkip,
			wantContains: "suspect",
		},
		{
			// Before this row, a snapshotting sandbox past the cooldown fell
			// through to Spawn, which has no edge from snapshotting: every
			// dispatch evaluation of a turn queued behind a post-turn
			// snapshot failed and backed off until the snapshot ended.
			name: "skip when snapshotting, even long past every other window",
			state: sandbox.SpawnState{
				Status:     sandbox.StateSnapshotting,
				CreatedAt:  now.Add(-(cfg.SpawningTimeout + cfg.Cooldown + cfg.ReadyWait + time.Hour)),
				LastSeenAt: now.Add(-(cfg.SpawningTimeout + time.Hour)),
			},
			wantKind:     sandbox.SpawnActionSkip,
			wantContains: "snapshot in progress",
		},
		{
			name: "skip when snapshotting, even with a snapshot to restore and a provider object to resume",
			state: sandbox.SpawnState{
				Status: sandbox.StateSnapshotting, CreatedAt: now.Add(-time.Hour),
				SnapshotImageID: "img-abc123", ProviderObjectID: "provider-object-1",
			},
			supportsPersistentResume: true,
			wantKind:                 sandbox.SpawnActionSkip,
			wantContains:             "snapshot in progress",
		},
		{
			name: "spawn when stuck in spawning past the spawning timeout (recovers interrupted spawn)",
			state: sandbox.SpawnState{
				Status: sandbox.StateSpawning, CreatedAt: now.Add(-(cfg.SpawningTimeout + time.Second)),
			},
			wantKind: sandbox.SpawnActionSpawn,
		},
		{
			name: "spawn when stuck in connecting past the spawning timeout",
			state: sandbox.SpawnState{
				Status: sandbox.StateConnecting, CreatedAt: now.Add(-(cfg.SpawningTimeout + time.Second)),
			},
			wantKind: sandbox.SpawnActionSpawn,
		},
		{
			name: "spawn when stuck in booting past the spawning timeout",
			state: sandbox.SpawnState{
				Status: sandbox.StateBooting, CreatedAt: now.Add(-(cfg.SpawningTimeout + time.Second)),
			},
			wantKind: sandbox.SpawnActionSpawn,
		},
		{
			// Regression for the relaunch-orphaning bug: a healthy slow
			// boot that keeps posting boot-progress pings must stay
			// "booting" past createdAt+window, so it is NOT respawned
			// (which would rotate its identity and orphan it).
			name: "skip for a slow boot still pinging past the createdAt window (lastSeenAt fresh)",
			state: sandbox.SpawnState{
				Status:     sandbox.StateSpawning,
				CreatedAt:  now.Add(-(cfg.SpawningTimeout + 60*time.Second)), // 3 min ago
				LastSeenAt: now.Add(-10 * time.Second),                       // ping 10s ago
			},
			wantKind:     sandbox.SpawnActionSkip,
			wantContains: "spawning",
		},
		{
			// A genuinely stuck boot pinged early then went silent:
			// LastSeenAt stops advancing, so the guard releases after the
			// window exactly like the connecting-timeout watchdog (both
			// measure from max(CreatedAt, LastSeenAt)).
			name: "spawn for a boot whose last ping went silent past the window (recovery preserved)",
			state: sandbox.SpawnState{
				Status:     sandbox.StateConnecting,
				CreatedAt:  now.Add(-(cfg.SpawningTimeout + 60*time.Second)),
				LastSeenAt: now.Add(-(cfg.SpawningTimeout + time.Second)), // silent > window
			},
			wantKind: sandbox.SpawnActionSpawn,
		},
		{
			// Audit finding F3 regression: models the row EXACTLY as
			// postgres/queries/sandboxes.sql's own fixed UpsertSandboxForSpawn
			// leaves it right after a resume-style claim on a box that sat
			// terminal (Stopped) for far longer than SpawningTimeout --
			// CreatedAt/LastSeenAt both reset to "now" by that SAME upsert
			// call, not still reflecting however long the box sat idle
			// beforehand. A concurrent second actor reading this row
			// genuinely Skips (the guard's own "no-op for free" purpose),
			// which it would NOT do if LastSeenAt/CreatedAt still carried
			// the original, long-stale spawn time (see this package's
			// sibling postgres integration test for the query-level half
			// of this fix).
			name: "F3: resume-style claim resets last-sign-of-life, so a concurrent second read genuinely skips",
			state: sandbox.SpawnState{
				Status:     sandbox.StateSpawning,
				CreatedAt:  now,
				LastSeenAt: now,
			},
			wantKind:     sandbox.SpawnActionSkip,
			wantContains: "spawning",
		},
		{
			// The pre-fix counterpart of the case above: if a resume claim
			// left CreatedAt/LastSeenAt at their ORIGINAL, long-stale
			// values (as the query did before the F3 fix), the guard does
			// NOT skip -- this is the exact bug the fix closes.
			name: "F3 (pre-fix shape, documents the bug): stale last-sign-of-life on a just-claimed spawning row fails to skip",
			state: sandbox.SpawnState{
				Status:     sandbox.StateSpawning,
				CreatedAt:  now.Add(-10 * time.Minute),
				LastSeenAt: now.Add(-10 * time.Minute),
			},
			wantKind: sandbox.SpawnActionSpawn,
		},
		{
			name: "still skips a stale spawning when a spawn is in progress in-memory",
			state: sandbox.SpawnState{
				Status: sandbox.StateSpawning, CreatedAt: now.Add(-(cfg.SpawningTimeout + time.Second)),
			},
			isSpawningInMemory: true,
			wantKind:           sandbox.SpawnActionSkip,
		},
		{
			name: "skip when ready with active WebSocket",
			state: sandbox.SpawnState{
				Status: sandbox.StateReady, CreatedAt: now.Add(-120 * time.Second), HasActiveWebSocket: true,
			},
			wantKind:     sandbox.SpawnActionSkip,
			wantContains: "active WebSocket",
		},
		{
			name: "wait when ready without WebSocket but recent spawn",
			state: sandbox.SpawnState{
				Status: sandbox.StateReady, CreatedAt: now.Add(-30 * time.Second),
			},
			wantKind:     sandbox.SpawnActionWait,
			wantContains: "no WebSocket",
		},
		{
			name: "wait during cooldown period",
			state: sandbox.SpawnState{
				Status: sandbox.StatePending, CreatedAt: now.Add(-10 * time.Second),
			},
			wantKind:     sandbox.SpawnActionWait,
			wantContains: "waiting",
		},
		{
			name: "skip when isSpawningInMemory flag is set",
			state: sandbox.SpawnState{
				Status: sandbox.StatePending, CreatedAt: now.Add(-60 * time.Second),
			},
			isSpawningInMemory: true,
			wantKind:           sandbox.SpawnActionSkip,
			wantContains:       "in-memory flag",
		},
		{
			name: "spawn when all conditions pass",
			state: sandbox.SpawnState{
				Status: sandbox.StatePending, CreatedAt: now.Add(-60 * time.Second),
			},
			wantKind: sandbox.SpawnActionSpawn,
		},
		{
			name: "failed status bypasses cooldown",
			state: sandbox.SpawnState{
				Status: sandbox.StateFailed, CreatedAt: now.Add(-5 * time.Second),
			},
			wantKind: sandbox.SpawnActionSpawn,
		},
		{
			name: "stopped status bypasses cooldown",
			state: sandbox.SpawnState{
				Status: sandbox.StateStopped, CreatedAt: now.Add(-5 * time.Second),
			},
			wantKind: sandbox.SpawnActionSpawn,
		},

		// ---- Persistent resume (Daytona-style) ----
		{
			name: "resume when provider supports persistent resume and sandbox is stopped with providerObjectId",
			state: sandbox.SpawnState{
				Status: sandbox.StateStopped, CreatedAt: now.Add(-120 * time.Second),
				ProviderObjectID: "daytona-abc123",
			},
			supportsPersistentResume: true,
			wantKind:                 sandbox.SpawnActionResume,
			wantContains:             "daytona-abc123",
		},
		{
			name: "resume when provider supports persistent resume and sandbox is stale with providerObjectId",
			state: sandbox.SpawnState{
				Status: sandbox.StateStale, CreatedAt: now.Add(-120 * time.Second),
				ProviderObjectID: "daytona-abc123",
			},
			supportsPersistentResume: true,
			wantKind:                 sandbox.SpawnActionResume,
		},
		{
			name: "resume takes priority over restore when both available",
			state: sandbox.SpawnState{
				Status: sandbox.StateStopped, CreatedAt: now.Add(-120 * time.Second),
				ProviderObjectID: "daytona-abc123", SnapshotImageID: "img-abc123",
			},
			supportsPersistentResume: true,
			wantKind:                 sandbox.SpawnActionResume,
		},
		{
			name: "falls back to restore when supportsPersistentResume but no providerObjectId",
			state: sandbox.SpawnState{
				Status: sandbox.StateStopped, CreatedAt: now.Add(-120 * time.Second),
				SnapshotImageID: "img-abc123",
			},
			supportsPersistentResume: true,
			wantKind:                 sandbox.SpawnActionRestore,
		},
		{
			name: "falls back to spawn when supportsPersistentResume but no providerObjectId and no snapshot",
			state: sandbox.SpawnState{
				Status: sandbox.StateStopped, CreatedAt: now.Add(-120 * time.Second),
			},
			supportsPersistentResume: true,
			wantKind:                 sandbox.SpawnActionSpawn,
		},
		{
			name: "does not resume when supportsPersistentResume is false even with providerObjectId",
			state: sandbox.SpawnState{
				Status: sandbox.StateStopped, CreatedAt: now.Add(-120 * time.Second),
				ProviderObjectID: "daytona-abc123",
			},
			supportsPersistentResume: false,
			wantKind:                 sandbox.SpawnActionSpawn,
		},
		{
			// "failed" is not a resume-eligible status -- falls through to spawn.
			name: "does not resume for failed status even with providerObjectId",
			state: sandbox.SpawnState{
				Status: sandbox.StateFailed, CreatedAt: now.Add(-120 * time.Second),
				ProviderObjectID: "daytona-abc123",
			},
			supportsPersistentResume: true,
			wantKind:                 sandbox.SpawnActionSpawn,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := sandbox.EvaluateSpawnDecision(tc.state, cfg, now, tc.isSpawningInMemory, tc.supportsPersistentResume)
			if got.Kind != tc.wantKind {
				t.Fatalf("Kind = %s, want %s (full action: %+v)", got.Kind, tc.wantKind, got)
			}
			if tc.wantContains == "" {
				return
			}
			haystack := got.Reason + got.ProviderObjectID + got.SnapshotImageID
			if !strings.Contains(haystack, tc.wantContains) {
				t.Errorf("action %+v does not contain %q", got, tc.wantContains)
			}
		})
	}
}

// TestEvaluateSpawnDecision_ActsOnlyOverAnEdge cross-checks the decision
// against the machine's transition table (state.go), which the caller
// validates every write against: for every state, long past every window,
// with and without a snapshot, a provider object and the resume
// capability, a Spawn has a spawn or force-respawn edge, a Restore a
// restore edge and a Resume a resume edge out of that state. A decision
// with no edge is one the caller can only refuse as an error, every time
// it is evaluated -- what a snapshotting sandbox's Spawn was.
func TestEvaluateSpawnDecision_ActsOnlyOverAnEdge(t *testing.T) {
	t.Parallel()

	cfg := sandbox.SpawnConfig{Cooldown: 30 * time.Second, ReadyWait: 60 * time.Second, SpawningTimeout: 120 * time.Second}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	longAgo := now.Add(-time.Hour)
	const gen = 1

	for _, status := range allStates {
		for _, snapshot := range []string{"", "img-abc123"} {
			for _, providerObject := range []string{"", "provider-object-1"} {
				for _, resume := range []bool{false, true} {
					state := sandbox.SpawnState{
						Status: status, CreatedAt: longAgo, LastSeenAt: longAgo,
						SnapshotImageID: snapshot, ProviderObjectID: providerObject,
					}
					action := sandbox.EvaluateSpawnDecision(state, cfg, now, false, resume)
					var edges []sandbox.Trigger
					switch action.Kind {
					case sandbox.SpawnActionSpawn:
						edges = []sandbox.Trigger{sandbox.SpawnTrigger(gen + 1), sandbox.ForceRespawnTrigger(gen + 1)}
					case sandbox.SpawnActionRestore:
						edges = []sandbox.Trigger{sandbox.RestoreTrigger(gen + 1)}
					case sandbox.SpawnActionResume:
						edges = []sandbox.Trigger{sandbox.ResumeTrigger(gen + 1)}
					default:
						continue
					}
					legal := false
					for _, trig := range edges {
						if _, err := sandbox.Transition(status, gen, trig); err == nil {
							legal = true
						}
					}
					if !legal {
						t.Errorf("EvaluateSpawnDecision(%+v, resume=%v) = %s, but the machine has no such edge out of %s",
							state, resume, action.Kind, status)
					}
				}
			}
		}
	}
}

// TestSpawnActionKind_String proves every kind stringifies to its
// documented name, including the out-of-range fallback.
func TestSpawnActionKind_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind sandbox.SpawnActionKind
		want string
	}{
		{sandbox.SpawnActionSpawn, "spawn"},
		{sandbox.SpawnActionResume, "resume"},
		{sandbox.SpawnActionRestore, "restore"},
		{sandbox.SpawnActionSkip, "skip"},
		{sandbox.SpawnActionWait, "wait"},
		{sandbox.SpawnActionKind(999), "SpawnActionKind(999)"},
	}

	for _, tc := range tests {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("SpawnActionKind(%d).String() = %q, want %q", int(tc.kind), got, tc.want)
		}
	}
}
