//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// runwaySeed is a sandbox row for the runway read, set on the database's
// clock: each *Ago and *FromNow is an offset from its now(), nil for NULL.
type runwaySeed struct {
	gen                int32
	status             string
	deadlineFromNow    *time.Duration
	deadlineGen        *int32
	lifetimeSeconds    *int32
	rotationGen        *int32
	rotationMessageID  *string
	rotationAgo        *time.Duration
	snapshotID         *string
	provenanceID       *string
	mintedAgo          *time.Duration
	snapshotStartAgo   *time.Duration
	lastSeenAgo        *time.Duration
	createdAgo         time.Duration
	deliveryStartedAgo *time.Duration
	pendingSnapshotID  *string
	preSuspectStatus   *string
	// startForAnother stamps the snapshot's start for another command
	// than the pending one; by default it is stamped for the pending one.
	startForAnother bool
	// heartbeats is how many heartbeats are counted since the latest
	// ready; nil for none recorded (a NULL count).
	heartbeats *int32
}

func durp(d time.Duration) *time.Duration { return &d }
func i32p(v int32) *int32                 { return &v }
func strpRunway(v string) *string         { return &v }

// seedRunwayRow writes s onto sessionID's sandbox row, creating it.
func seedRunwayRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, s runwaySeed) {
	t.Helper()
	if _, err := narvipg.NewSandboxStore(pool).Create(ctx, sessionID); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	secs := func(d *time.Duration) *float64 {
		if d == nil {
			return nil
		}
		v := d.Seconds()
		return &v
	}
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET gen = $2, status = $3::sandbox_status,
		lifetime_deadline_at = now() + make_interval(secs => $4::double precision), lifetime_deadline_gen = $5, lifetime_seconds = $6,
		rotation_gen = $7, rotation_started_at = now() - make_interval(secs => $8::double precision),
		snapshot_id = $9, snapshot_provenance_id = $10, snapshot_minted_at = now() - make_interval(secs => $11::double precision),
		snapshot_started_at = now() - make_interval(secs => $12::double precision),
		last_seen_at = now() - make_interval(secs => $13::double precision),
		created_at = now() - make_interval(secs => $14::double precision),
		pr_delivery_started_at = now() - make_interval(secs => $15::double precision),
		pending_snapshot_message_id = $16, rotation_message_id = $17, pre_suspect_status = $18::sandbox_status,
		snapshot_start_message_id = CASE WHEN $19::boolean THEN 'another-command' ELSE $16 END,
		heartbeat_ready_seq = CASE WHEN $20::integer IS NULL THEN NULL ELSE ready_seq END, heartbeats_since_ready = $20
		WHERE session_id = $1`,
		sessionID, s.gen, s.status, secs(s.deadlineFromNow), s.deadlineGen, s.lifetimeSeconds,
		s.rotationGen, secs(s.rotationAgo), s.snapshotID, s.provenanceID, secs(s.mintedAgo), secs(s.snapshotStartAgo),
		secs(s.lastSeenAgo), s.createdAgo.Seconds(), secs(s.deliveryStartedAgo), s.pendingSnapshotID, s.rotationMessageID, s.preSuspectStatus,
		s.startForAnother, s.heartbeats); err != nil {
		t.Fatalf("seed the sandbox: %v", err)
	}
}

// near reports whether got is within a second of want: both are measured
// on the database's clock, a statement apart.
func near(got int64, want time.Duration) bool {
	d := time.Duration(got) - want
	return d > -time.Second && d < time.Second
}

// TestGetSandboxRunway_ReadsEveryFactOnTheDatabaseClock pins the runway
// gate's one read (technical plan §35.3): the deadline counts only for the
// live gen it was stamped for, the lifetime only with it, a rotation only
// of the live gen, the snapshot in progress the rotation's own only while
// its pending command is the one the rotation sent, its snapshot taken only
// when recorded with its provenance at or after the rotation started, a
// snapshot's start when one is recorded for the pending command, the last
// sign of life (created_at when there is none yet), a suspect sandbox
// suspected mid-snapshot only when it was snapshotting, the replay read
// only once two heartbeats are counted since the latest ready, and a
// delivery's stamp with the database's now.
func TestGetSandboxRunway_ReadsEveryFactOnTheDatabaseClock(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSandboxStore(pool)

	base := func() runwaySeed {
		return runwaySeed{gen: 2, status: "ready", deadlineFromNow: durp(30 * time.Minute), deadlineGen: i32p(2), lifetimeSeconds: i32p(7200),
			lastSeenAgo: durp(10 * time.Second), createdAgo: time.Hour, heartbeats: i32p(2)}
	}
	tests := []struct {
		name  string
		seed  func(*runwaySeed)
		check func(t *testing.T, r sqlcgen.GetSandboxRunwayRow)
	}{
		{name: "the live gen's deadline and lifetime", seed: func(*runwaySeed) {}, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if r.Gen != 2 || r.Status != sqlcgen.SandboxStatusReady || !r.DeadlineKnown || !near(r.RunwayNanos, 30*time.Minute) || r.LifetimeSeconds != 7200 {
				t.Fatalf("%+v: want gen 2 ready, 30m of runway known, lifetime 7200", r)
			}
			if r.Rotating || r.RotationSnapshotPending || r.RotationSnapshotTaken || r.SnapshotStartKnown || r.SuspectedMidSnapshot || r.ReplayPending ||
				!near(r.SinceLastSeenNanos, 10*time.Second) || r.PrDeliveryStartedAt.Valid || !r.ObservedAt.Valid {
				t.Fatalf("%+v: want nothing rotating or snapshotting, seen 10s ago, no delivery, the database's now", r)
			}
		}},
		{name: "a deadline past is negative", seed: func(s *runwaySeed) { s.deadlineFromNow = durp(-time.Minute) }, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if !r.DeadlineKnown || !near(r.RunwayNanos, -time.Minute) {
				t.Fatalf("%+v: want a known runway of -1m", r)
			}
		}},
		{name: "another gen's deadline is unknown, and so is its lifetime", seed: func(s *runwaySeed) { s.deadlineGen = i32p(1) }, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if r.DeadlineKnown || r.LifetimeSeconds != 0 {
				t.Fatalf("%+v: want the deadline unknown and no lifetime", r)
			}
		}},
		{name: "a report-only deadline has no lifetime", seed: func(s *runwaySeed) { s.lifetimeSeconds = nil }, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if !r.DeadlineKnown || r.LifetimeSeconds != 0 {
				t.Fatalf("%+v: want the deadline known, lifetime 0 (none recorded)", r)
			}
		}},
		{name: "no deadline at all", seed: func(s *runwaySeed) { s.deadlineFromNow, s.deadlineGen, s.lifetimeSeconds = nil, nil, nil }, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if r.DeadlineKnown || r.RunwayNanos != 0 || r.LifetimeSeconds != 0 {
				t.Fatalf("%+v: want no deadline, no runway, no lifetime", r)
			}
		}},
		{name: "a rotation of the live gen, its snapshot taken after it started", seed: func(s *runwaySeed) {
			s.rotationGen, s.rotationAgo = i32p(2), durp(90*time.Second)
			s.snapshotID, s.provenanceID, s.mintedAgo = strpRunway("snap-rot"), strpRunway("snap-rot"), durp(30*time.Second)
		}, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if !r.Rotating || !near(r.SinceRotationNanos, 90*time.Second) || !r.RotationSnapshotTaken {
				t.Fatalf("%+v: want rotating for 90s, its snapshot taken", r)
			}
		}},
		{name: "a rotation whose snapshot predates it was not taken", seed: func(s *runwaySeed) {
			s.rotationGen, s.rotationAgo = i32p(2), durp(90*time.Second)
			s.snapshotID, s.provenanceID, s.mintedAgo = strpRunway("snap-old"), strpRunway("snap-old"), durp(time.Hour)
		}, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if !r.Rotating || r.RotationSnapshotTaken {
				t.Fatalf("%+v: want rotating, its snapshot not taken", r)
			}
		}},
		{name: "a snapshot whose provenance is another's was not taken", seed: func(s *runwaySeed) {
			s.rotationGen, s.rotationAgo = i32p(2), durp(90*time.Second)
			s.snapshotID, s.provenanceID, s.mintedAgo = strpRunway("snap-by-the-previous-binary"), strpRunway("snap-rot"), durp(30*time.Second)
		}, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if !r.Rotating || r.RotationSnapshotTaken {
				t.Fatalf("%+v: want rotating, its snapshot not taken (the row's provenance names another)", r)
			}
		}},
		{name: "a rotation of an older gen is no rotation", seed: func(s *runwaySeed) {
			s.rotationGen, s.rotationAgo = i32p(1), durp(90*time.Second)
			s.snapshotID, s.provenanceID, s.mintedAgo = strpRunway("snap-rot"), strpRunway("snap-rot"), durp(30*time.Second)
		}, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if r.Rotating || r.RotationSnapshotTaken {
				t.Fatalf("%+v: want not rotating", r)
			}
		}},
		{name: "the rotation's own snapshot in progress", seed: func(s *runwaySeed) {
			s.rotationGen, s.rotationMessageID, s.rotationAgo = i32p(2), strpRunway("cmd-rotation"), durp(30*time.Second)
			s.status, s.pendingSnapshotID, s.snapshotStartAgo = "snapshotting", strpRunway("cmd-rotation"), durp(30*time.Second)
		}, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if !r.Rotating || !r.RotationSnapshotPending {
				t.Fatalf("%+v: want rotating, its own snapshot pending", r)
			}
		}},
		{name: "another snapshot in progress on a rotating gen is not the rotation's", seed: func(s *runwaySeed) {
			s.rotationGen, s.rotationMessageID, s.rotationAgo = i32p(2), strpRunway("cmd-rotation"), durp(4*time.Minute)
			s.status, s.pendingSnapshotID, s.snapshotStartAgo = "snapshotting", strpRunway("cmd-post-turn"), durp(10*time.Second)
		}, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if !r.Rotating || r.RotationSnapshotPending {
				t.Fatalf("%+v: want rotating, the snapshot pending not its own", r)
			}
		}},
		{name: "the rotation's command, no longer snapshotting, is not pending", seed: func(s *runwaySeed) {
			s.rotationGen, s.rotationMessageID, s.rotationAgo = i32p(2), strpRunway("cmd-rotation"), durp(time.Minute)
			s.pendingSnapshotID = strpRunway("cmd-rotation")
		}, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if !r.Rotating || r.RotationSnapshotPending {
				t.Fatalf("%+v: want rotating, nothing pending on a ready row", r)
			}
		}},
		{name: "an older gen's rotation command is not the live gen's", seed: func(s *runwaySeed) {
			s.rotationGen, s.rotationMessageID, s.rotationAgo = i32p(1), strpRunway("cmd-rotation"), durp(time.Minute)
			s.status, s.pendingSnapshotID = "snapshotting", strpRunway("cmd-rotation")
		}, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if r.Rotating || r.RotationSnapshotPending {
				t.Fatalf("%+v: want neither rotating nor its snapshot pending", r)
			}
		}},
		{name: "suspected mid-snapshot", seed: func(s *runwaySeed) {
			s.status, s.preSuspectStatus, s.pendingSnapshotID = "suspect", strpRunway("snapshotting"), strpRunway("cmd")
		}, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if r.Status != sqlcgen.SandboxStatusSuspect || !r.SuspectedMidSnapshot {
				t.Fatalf("%+v: want suspect, suspected mid-snapshot", r)
			}
		}},
		{name: "suspected while ready is not mid-snapshot", seed: func(s *runwaySeed) {
			s.status, s.preSuspectStatus = "suspect", strpRunway("ready")
		}, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if r.Status != sqlcgen.SandboxStatusSuspect || r.SuspectedMidSnapshot {
				t.Fatalf("%+v: want suspect, not mid-snapshot", r)
			}
		}},
		{name: "a snapshot in progress, its start recorded", seed: func(s *runwaySeed) {
			s.status, s.pendingSnapshotID, s.snapshotStartAgo = "snapshotting", strpRunway("cmd"), durp(45*time.Second)
		}, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if r.Status != sqlcgen.SandboxStatusSnapshotting || !r.SnapshotStartKnown || !near(r.SinceSnapshotStartNanos, 45*time.Second) {
				t.Fatalf("%+v: want snapshotting for 45s", r)
			}
		}},
		{name: "a snapshot with no start", seed: func(s *runwaySeed) { s.status, s.pendingSnapshotID = "snapshotting", strpRunway("cmd") }, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if r.SnapshotStartKnown || r.SinceSnapshotStartNanos != 0 {
				t.Fatalf("%+v: want no start known", r)
			}
		}},
		{name: "a start stamped for another command times no other snapshot", seed: func(s *runwaySeed) {
			s.status, s.pendingSnapshotID, s.snapshotStartAgo, s.startForAnother = "snapshotting", strpRunway("cmd"), durp(6*time.Minute), true
		}, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if r.SnapshotStartKnown {
				t.Fatalf("%+v: want no start known for the pending command", r)
			}
		}},
		{name: "no heartbeat counted: the replay is pending", seed: func(s *runwaySeed) { s.heartbeats = nil }, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if !r.ReplayPending {
				t.Fatalf("%+v: want the replay pending", r)
			}
		}},
		{name: "one heartbeat since the ready: the replay is pending", seed: func(s *runwaySeed) { s.heartbeats = i32p(1) }, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if !r.ReplayPending {
				t.Fatalf("%+v: want the replay pending", r)
			}
		}},
		{name: "never seen: since its creation", seed: func(s *runwaySeed) { s.lastSeenAgo = nil }, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if !near(r.SinceLastSeenNanos, time.Hour) {
				t.Fatalf("%+v: want silent since its creation, an hour", r)
			}
		}},
		{name: "a delivery open", seed: func(s *runwaySeed) { s.deliveryStartedAgo = durp(2 * time.Minute) }, check: func(t *testing.T, r sqlcgen.GetSandboxRunwayRow) {
			if !r.PrDeliveryStartedAt.Valid || !near(int64(r.ObservedAt.Time.Sub(r.PrDeliveryStartedAt.Time)), 2*time.Minute) {
				t.Fatalf("%+v: want the delivery stamped 2m before the observation", r)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			s := base()
			tc.seed(&s)
			seedRunwayRow(ctx, t, pool, sessionID, s)
			r, err := store.Runway(ctx, sessionID)
			if err != nil {
				t.Fatalf("Runway: %v", err)
			}
			tc.check(t, r)
		})
	}
}

// TestStartSandboxRotation_RecordsTheRotationOfTheLiveGen: the rotation's
// start records the live gen, the database's now, the snapshot command as
// the rotation's and as the pending snapshot, and the snapshot's start; it
// is gen-fenced.
func TestStartSandboxRotation_RecordsTheRotationOfTheLiveGen(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSandboxStore(pool)
	sessionID := createTestSession(ctx, t, pool)
	seedRunwayRow(ctx, t, pool, sessionID, runwaySeed{gen: 3, status: "snapshotting", lastSeenAgo: durp(time.Second)})

	if started, err := store.StartRotation(ctx, sessionID, 2, "cmd-stale"); err != nil || started {
		t.Fatalf("StartRotation of gen 2 on gen 3 = %v, %v; want no row", started, err)
	}
	if started, err := store.StartRotation(ctx, sessionID, 3, "cmd-rotation"); err != nil || !started {
		t.Fatalf("StartRotation of the live gen = %v, %v; want it recorded", started, err)
	}
	row, err := store.Get(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if row.RotationGen == nil || *row.RotationGen != 3 || !row.RotationStartedAt.Valid || !row.SnapshotStartedAt.Valid ||
		row.PendingSnapshotMessageID == nil || *row.PendingSnapshotMessageID != "cmd-rotation" ||
		row.RotationMessageID == nil || *row.RotationMessageID != "cmd-rotation" ||
		row.SnapshotStartMessageID == nil || *row.SnapshotStartMessageID != "cmd-rotation" {
		t.Fatalf("row after the start: rotation %v (%v) started %v snapshot start %v pending %v; want gen 3, cmd-rotation, both starts, cmd-rotation pending",
			row.RotationGen, row.RotationMessageID, row.RotationStartedAt.Valid, row.SnapshotStartedAt.Valid, row.PendingSnapshotMessageID)
	}
	r, err := store.Runway(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Rotating || !r.RotationSnapshotPending || !near(r.SinceRotationNanos, 0) || !r.SnapshotStartKnown || !near(r.SinceSnapshotStartNanos, 0) {
		t.Fatalf("runway after the start: %+v; want rotating and snapshotting since now, its own snapshot pending", r)
	}
}

// TestStampSandboxSnapshotStart_OnlyAStartlessSnapshotOfTheLiveGen: a
// snapshot in progress with no start is given the database's now; one
// with a start keeps it, and nothing else is stamped.
func TestStampSandboxSnapshotStart_OnlyAStartlessSnapshotOfTheLiveGen(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSandboxStore(pool)
	tests := []struct {
		name      string
		seed      runwaySeed
		gen       int32
		want      bool
		wantStart bool
	}{
		{name: "a startless snapshot of the live gen", seed: runwaySeed{gen: 1, status: "snapshotting", pendingSnapshotID: strpRunway("c")}, gen: 1, want: true, wantStart: true},
		{name: "a snapshot that has its start", seed: runwaySeed{gen: 1, status: "snapshotting", snapshotStartAgo: durp(time.Minute)}, gen: 1, wantStart: true},
		{name: "another gen", seed: runwaySeed{gen: 2, status: "snapshotting"}, gen: 1},
		{name: "not snapshotting", seed: runwaySeed{gen: 1, status: "ready"}, gen: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := createTestSession(ctx, t, pool)
			seedRunwayRow(ctx, t, pool, sessionID, tc.seed)
			stamped, err := store.StampSnapshotStart(ctx, sessionID, tc.gen)
			if err != nil || stamped != tc.want {
				t.Fatalf("StampSnapshotStart = %v, %v; want %v", stamped, err, tc.want)
			}
			r, err := store.Runway(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if r.SnapshotStartKnown != tc.wantStart || (tc.seed.snapshotStartAgo != nil && !near(r.SinceSnapshotStartNanos, *tc.seed.snapshotStartAgo)) {
				t.Fatalf("start known %v (%v ago), want %v, an existing start kept", r.SnapshotStartKnown, time.Duration(r.SinceSnapshotStartNanos), tc.wantStart)
			}
		})
	}
}

// TestUpdateSandboxPendingSnapshotMessageID_StartsAndForgetsTheSnapshot:
// every snapshot start sets a pending command, and its start with it; the
// pending command cleared clears the start, and so does the snapshot's
// record.
func TestUpdateSandboxPendingSnapshotMessageID_StartsAndForgetsTheSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSandboxStore(pool)
	sessionID := createTestSession(ctx, t, pool)
	seedRunwayRow(ctx, t, pool, sessionID, runwaySeed{gen: 1, status: "snapshotting"})

	cmd := "cmd-1"
	row, err := store.UpdatePendingSnapshotMessageID(ctx, sqlcgen.UpdateSandboxPendingSnapshotMessageIDParams{SessionID: sessionID, PendingSnapshotMessageID: &cmd})
	if err != nil || !row.SnapshotStartedAt.Valid || row.PendingSnapshotMessageID == nil || *row.PendingSnapshotMessageID != cmd ||
		row.SnapshotStartMessageID == nil || *row.SnapshotStartMessageID != cmd {
		t.Fatalf("after setting the pending command: start %v (for %v) pending %v (%v); want all set, the start stamped for it",
			row.SnapshotStartedAt.Valid, row.SnapshotStartMessageID, row.PendingSnapshotMessageID, err)
	}
	row, err = store.UpdatePendingSnapshotMessageID(ctx, sqlcgen.UpdateSandboxPendingSnapshotMessageIDParams{SessionID: sessionID})
	if err != nil || row.SnapshotStartedAt.Valid || row.PendingSnapshotMessageID != nil || row.SnapshotStartMessageID != nil {
		t.Fatalf("after clearing the pending command: start %v (for %v) pending %v (%v); want all cleared", row.SnapshotStartedAt.Valid, row.SnapshotStartMessageID, row.PendingSnapshotMessageID, err)
	}
	if _, err := store.UpdatePendingSnapshotMessageID(ctx, sqlcgen.UpdateSandboxPendingSnapshotMessageIDParams{SessionID: sessionID, PendingSnapshotMessageID: &cmd}); err != nil {
		t.Fatal(err)
	}
	snapshot := "snap-1"
	row, err = store.UpdateSnapshotID(ctx, sqlcgen.UpdateSandboxSnapshotIDParams{SessionID: sessionID, SnapshotID: &snapshot})
	if err != nil || row.SnapshotStartedAt.Valid || row.PendingSnapshotMessageID != nil || row.SnapshotStartMessageID != nil {
		t.Fatalf("after the snapshot's record: start %v (for %v) pending %v (%v); want all cleared", row.SnapshotStartedAt.Valid, row.SnapshotStartMessageID, row.PendingSnapshotMessageID, err)
	}
}

// TestUpsertSandboxForSpawn_ClearsTheRotation: the claim that replaces a
// gen clears its rotation and any snapshot start -- hygiene, since a
// rotation counts only while it is the live gen's.
func TestUpsertSandboxForSpawn_ClearsTheRotation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSandboxStore(pool)
	sessionID := createTestSession(ctx, t, pool)
	seedRunwayRow(ctx, t, pool, sessionID, runwaySeed{gen: 1, status: "stopped", rotationGen: i32p(1), rotationMessageID: strpRunway("cmd-rotation"),
		rotationAgo: durp(time.Minute), snapshotStartAgo: durp(time.Minute), startForAnother: true, heartbeats: i32p(2)})

	hash := "token-hash-gen-2"
	row, err := store.UpsertForSpawn(ctx, sqlcgen.UpsertSandboxForSpawnParams{SessionID: sessionID, TokenHash: &hash, LifetimeSeconds: i32p(7200)})
	if err != nil {
		t.Fatalf("UpsertForSpawn: %v", err)
	}
	if row.Gen != 2 || row.RotationGen != nil || row.RotationMessageID != nil || row.RotationStartedAt.Valid || row.SnapshotStartedAt.Valid ||
		row.SnapshotStartMessageID != nil || row.HeartbeatReadySeq != nil || row.HeartbeatsSinceReady != nil {
		t.Fatalf("after the claim: gen %d rotation %v (%v) started %v snapshot start %v (for %v) heartbeats %v since ready %v; want gen 2 with none",
			row.Gen, row.RotationGen, row.RotationMessageID, row.RotationStartedAt.Valid, row.SnapshotStartedAt.Valid, row.SnapshotStartMessageID,
			row.HeartbeatsSinceReady, row.HeartbeatReadySeq)
	}
}

// TestStampSandboxSnapshotStart_AStartStampedForAnotherCommandIsReplaced:
// the previous binary starts a snapshot by setting the pending command
// alone, so the row can carry the start of an earlier snapshot (technical
// plan §35.3, round-2 F2). That start is not the pending snapshot's: the
// first look stamps one of its own, for its command, and the runway read
// then knows it.
func TestStampSandboxSnapshotStart_AStartStampedForAnotherCommandIsReplaced(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSandboxStore(pool)
	sessionID := createTestSession(ctx, t, pool)
	seedRunwayRow(ctx, t, pool, sessionID, runwaySeed{gen: 1, status: "snapshotting", pendingSnapshotID: strpRunway("post-turn-by-previous"),
		snapshotStartAgo: durp(6 * time.Minute), startForAnother: true, heartbeats: i32p(2)})

	stamped, err := store.StampSnapshotStart(ctx, sessionID, 1)
	if err != nil || !stamped {
		t.Fatalf("StampSnapshotStart = %v, %v; want the start replaced", stamped, err)
	}
	r, err := store.Runway(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !r.SnapshotStartKnown || !near(r.SinceSnapshotStartNanos, 0) {
		t.Fatalf("start known %v, %v ago; want a start of its own, now", r.SnapshotStartKnown, time.Duration(r.SinceSnapshotStartNanos))
	}
	row, err := store.Get(ctx, sessionID)
	if err != nil || row.SnapshotStartMessageID == nil || *row.SnapshotStartMessageID != "post-turn-by-previous" {
		t.Fatalf("start stamped for %v (%v), want post-turn-by-previous", row.SnapshotStartMessageID, err)
	}
	if again, err := store.StampSnapshotStart(ctx, sessionID, 1); err != nil || again {
		t.Fatalf("a second StampSnapshotStart = %v, %v; want nothing: the start is its own now", again, err)
	}
}

// TestCountSandboxHeartbeat_CountsTwoSinceTheReady: the runway read's
// replay_pending (technical plan §35.3, round-2 F1) holds until two
// heartbeats of the live gen are counted since its latest ready, whichever
// binary recorded that ready: a ready moves ready_seq, and the count
// starts again at the next heartbeat. Another gen's heartbeat counts
// nothing.
func TestCountSandboxHeartbeat_CountsTwoSinceTheReady(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	store := narvipg.NewSandboxStore(pool)
	sessionID := createTestSession(ctx, t, pool)
	seedRunwayRow(ctx, t, pool, sessionID, runwaySeed{gen: 3, status: "snapshotting", lastSeenAgo: durp(time.Second)})

	pending := func(want bool, when string) {
		t.Helper()
		r, err := store.Runway(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if r.ReplayPending != want {
			t.Fatalf("%s: replay pending %v, want %v", when, r.ReplayPending, want)
		}
	}
	heartbeat := func(gen int32) {
		t.Helper()
		if err := store.CountHeartbeat(ctx, sessionID, gen); err != nil {
			t.Fatalf("CountHeartbeat: %v", err)
		}
	}
	pending(true, "no heartbeat counted yet")
	heartbeat(3)
	pending(true, "one heartbeat")
	heartbeat(2)
	pending(true, "one heartbeat and another gen's")
	heartbeat(3)
	pending(false, "two heartbeats")
	heartbeat(3)
	pending(false, "three heartbeats")

	// A reconnect: this release's ready, then the previous binary's, which
	// moves ready_seq alone.
	if err := store.RecordReady(ctx, sessionID, 3, false, false, false, nil); err != nil {
		t.Fatal(err)
	}
	pending(true, "a ready since")
	heartbeat(3)
	pending(true, "one heartbeat since the ready")
	if _, err := pool.Exec(ctx, `UPDATE sandboxes SET ready_seq = ready_seq + 1 WHERE session_id = $1`, sessionID); err != nil {
		t.Fatal(err)
	}
	heartbeat(3)
	pending(true, "one heartbeat since a ready the previous binary recorded")
	heartbeat(3)
	pending(false, "two heartbeats since it")
}
