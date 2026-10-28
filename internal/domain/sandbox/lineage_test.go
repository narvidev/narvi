package sandbox_test

import (
	"testing"

	"github.com/narvidev/narvi/internal/domain/sandbox"
)

// freshSpawnAfterATurn is a fresh spawn that replaces a dead gen of a
// session that had a turn dispatched to it, with no snapshot refused,
// nothing rotating, nothing live and no mark pending: row 10, no_snapshot.
func freshSpawnAfterATurn() sandbox.LineageFacts {
	return sandbox.LineageFacts{Claim: sandbox.LineageClaimSpawn, PriorTurnDispatched: true}
}

// TestDecideFreshLineage covers every row of the decision's table
// (technical plan §35.5), in its order, each with the neighbour that pins
// where it stops: a row that does not mark wins over every fact that would
// otherwise mark, a pending mark is carried with the cause the claim's own
// facts give, and each cause row wins over the ones below it.
func TestDecideFreshLineage(t *testing.T) {
	t.Parallel()

	marked := func(cause sandbox.LineageCause) sandbox.LineageDecision {
		return sandbox.LineageDecision{Mark: true, Announce: true, Cause: cause}
	}
	carried := func(cause sandbox.LineageCause) sandbox.LineageDecision {
		return sandbox.LineageDecision{Mark: true, Carried: true, Cause: cause}
	}
	tests := []struct {
		name  string
		facts func(*sandbox.LineageFacts)
		want  sandbox.LineageDecision
	}{
		// Rows 1 and 2: a restore or a resume keeps the session's work,
		// whatever else is true of the claim -- but for row 0's restore.
		{name: "row 1: a restore", facts: func(f *sandbox.LineageFacts) { f.Claim = sandbox.LineageClaimRestore }},
		{name: "row 1: a rotation's restore", facts: func(f *sandbox.LineageFacts) {
			f.Claim, f.Rotating = sandbox.LineageClaimRestore, true
		}},
		{name: "row 1: a restore replacing a gen whose mark is pending", facts: func(f *sandbox.LineageFacts) {
			f.Claim, f.ReplacedMarkPending = sandbox.LineageClaimRestore, true
		}},
		{name: "row 2: a resume replacing a gen whose mark is pending, its snapshot unrecapped", facts: func(f *sandbox.LineageFacts) {
			f.Claim, f.ReplacedMarkPending, f.RestoredSnapshotUnrecapped = sandbox.LineageClaimResume, true, true
		}},
		{name: "a spawn's unrecapped-snapshot fact changes nothing", facts: func(f *sandbox.LineageFacts) {
			f.RestoredSnapshotUnrecapped = true
		}, want: marked(sandbox.LineageCauseNoSnapshot)},

		// Row 0: a restore of the fresh lineage's own snapshot, minted before
		// its recap arrived, carries the pending mark.
		{name: "row 0: a restore of an unrecapped snapshot replacing a pending mark", facts: func(f *sandbox.LineageFacts) {
			f.Claim, f.ReplacedMarkPending, f.RestoredSnapshotUnrecapped = sandbox.LineageClaimRestore, true, true
		}, want: carried(sandbox.LineageCauseUnrecappedSnapshot)},
		{name: "row 0: a rotation's restore of its own unrecapped snapshot", facts: func(f *sandbox.LineageFacts) {
			f.Claim, f.ReplacedMarkPending, f.RestoredSnapshotUnrecapped, f.Rotating = sandbox.LineageClaimRestore, true, true, true
		}, want: carried(sandbox.LineageCauseUnrecappedSnapshot)},
		{name: "row 0: an unrecapped snapshot restored again after the recap reached a later gen", facts: func(f *sandbox.LineageFacts) {
			f.Claim, f.RestoredSnapshotUnrecapped = sandbox.LineageClaimRestore, true
		}, want: sandbox.LineageDecision{Mark: true, Cause: sandbox.LineageCauseUnrecappedSnapshot}},
		{name: "row 2: a resume", facts: func(f *sandbox.LineageFacts) { f.Claim = sandbox.LineageClaimResume }},
		{name: "row 2: a resume replacing a gen whose mark is pending", facts: func(f *sandbox.LineageFacts) {
			f.Claim, f.ReplacedMarkPending = sandbox.LineageClaimResume, true
		}},
		{name: "no claim at all is never marked", facts: func(f *sandbox.LineageFacts) { f.Claim = 0 }},
		{name: "an unknown claim is never marked", facts: func(f *sandbox.LineageFacts) { f.Claim = sandbox.LineageClaimResume + 1 }},

		// Rows 3 and 4: nothing to lose.
		{name: "row 3: the session's first spawn", facts: func(f *sandbox.LineageFacts) { f.FirstGen = true }},
		{name: "row 3 before row 6: a first gen whose restore was refused", facts: func(f *sandbox.LineageFacts) {
			f.FirstGen, f.Refusal = true, sandbox.LineageRefusalCleared
		}},
		{name: "row 4: a fresh spawn before any turn was dispatched", facts: func(f *sandbox.LineageFacts) { f.PriorTurnDispatched = false }},
		{name: "row 4: a first gen stuck booting, force-respawned", facts: func(f *sandbox.LineageFacts) {
			f.PriorTurnDispatched, f.ReplacedLive = false, true
		}},
		{name: "row 4 before row 5: a pending mark with no turn dispatched", facts: func(f *sandbox.LineageFacts) {
			f.PriorTurnDispatched, f.ReplacedMarkPending = false, true
		}},

		// Row 5: the replaced gen's loss was announced and never recapped.
		{name: "row 5: carried, cause no_snapshot", facts: func(f *sandbox.LineageFacts) { f.ReplacedMarkPending = true },
			want: carried(sandbox.LineageCauseNoSnapshot)},
		{name: "row 5: carried, cause stuck_respawn", facts: func(f *sandbox.LineageFacts) {
			f.ReplacedMarkPending, f.ReplacedLive = true, true
		}, want: carried(sandbox.LineageCauseStuckRespawn)},
		{name: "row 5: carried, cause restore_refused", facts: func(f *sandbox.LineageFacts) {
			f.ReplacedMarkPending, f.Refusal = true, sandbox.LineageRefusalCleared
		}, want: carried(sandbox.LineageCauseRestoreRefused)},
		{name: "row 5: carried, cause restore_downgraded", facts: func(f *sandbox.LineageFacts) {
			f.ReplacedMarkPending, f.Refusal = true, sandbox.LineageRefusalKept
		}, want: carried(sandbox.LineageCauseRestoreDowngraded)},
		{name: "row 5: carried, cause rotation_no_snapshot", facts: func(f *sandbox.LineageFacts) {
			f.ReplacedMarkPending, f.Rotating = true, true
		}, want: carried(sandbox.LineageCauseRotationNoSnapshot)},

		// Rows 6 to 10: announced.
		{name: "row 6: a restore refused, its snapshot cleared", facts: func(f *sandbox.LineageFacts) { f.Refusal = sandbox.LineageRefusalCleared },
			want: marked(sandbox.LineageCauseRestoreRefused)},
		{name: "row 6 before row 8: a rotation whose restore was refused", facts: func(f *sandbox.LineageFacts) {
			f.Refusal, f.Rotating, f.ReplacedLive = sandbox.LineageRefusalCleared, true, true
		}, want: marked(sandbox.LineageCauseRestoreRefused)},
		{name: "row 7: a restore downgraded, its snapshot kept", facts: func(f *sandbox.LineageFacts) { f.Refusal = sandbox.LineageRefusalKept },
			want: marked(sandbox.LineageCauseRestoreDowngraded)},
		{name: "row 7 before row 8: a rotation whose restore was downgraded", facts: func(f *sandbox.LineageFacts) {
			f.Refusal, f.Rotating, f.ReplacedLive = sandbox.LineageRefusalKept, true, true
		}, want: marked(sandbox.LineageCauseRestoreDowngraded)},
		{name: "row 8: a rotation with no snapshot", facts: func(f *sandbox.LineageFacts) { f.Rotating = true },
			want: marked(sandbox.LineageCauseRotationNoSnapshot)},
		{name: "row 8 before row 9: a rotating gen still live", facts: func(f *sandbox.LineageFacts) {
			f.Rotating, f.ReplacedLive = true, true
		}, want: marked(sandbox.LineageCauseRotationNoSnapshot)},
		{name: "row 9: a live gen force-respawned", facts: func(f *sandbox.LineageFacts) { f.ReplacedLive = true },
			want: marked(sandbox.LineageCauseStuckRespawn)},
		{name: "row 10: a dead gen with no snapshot", want: marked(sandbox.LineageCauseNoSnapshot)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := freshSpawnAfterATurn()
			if tc.facts != nil {
				tc.facts(&f)
			}
			if got := sandbox.DecideFreshLineage(f); got != tc.want {
				t.Errorf("DecideFreshLineage(%+v) = %+v, want %+v", f, got, tc.want)
			}
		})
	}
}

// TestDecideFreshLineage_Properties sweeps every combination of the facts
// and holds the decision to what §35.5 requires of it, whatever the row
// order: a resume never marks; a restore is marked exactly when its
// snapshot was minted before its fresh lineage's recap arrived, never
// announced, and carried exactly when the replaced gen's mark is pending;
// any other mark is a spawn of an existing session that had a turn
// dispatched before, every such spawn is marked, announced exactly when it
// is not carried, and carried exactly when the replaced gen's mark is
// pending; and a spawn's cause is the claim's own, the same whether the
// mark is carried or not.
func TestDecideFreshLineage_Properties(t *testing.T) {
	t.Parallel()

	claims := []sandbox.LineageClaim{0, sandbox.LineageClaimSpawn, sandbox.LineageClaimRestore, sandbox.LineageClaimResume}
	refusals := []sandbox.LineageRefusal{sandbox.LineageRefusalNone, sandbox.LineageRefusalCleared, sandbox.LineageRefusalKept}
	bools := []bool{false, true}
	visited := 0
	for _, claim := range claims {
		for _, firstGen := range bools {
			for _, prior := range bools {
				for _, pending := range bools {
					for _, refusal := range refusals {
						for _, rotating := range bools {
							for _, live := range bools {
								for _, unrecapped := range bools {
									visited++
									f := sandbox.LineageFacts{
										Claim: claim, FirstGen: firstGen, PriorTurnDispatched: prior, ReplacedMarkPending: pending,
										Refusal: refusal, Rotating: rotating, ReplacedLive: live, RestoredSnapshotUnrecapped: unrecapped,
									}
									d := sandbox.DecideFreshLineage(f)
									fail := func(why string) {
										t.Helper()
										t.Errorf("DecideFreshLineage(%+v) = %+v: %s", f, d, why)
									}
									freshLineage := claim == sandbox.LineageClaimSpawn && !firstGen && prior
									unrecappedRestore := claim == sandbox.LineageClaimRestore && unrecapped
									if claim == sandbox.LineageClaimResume && (d.Mark || d.Announce || d.Carried) {
										fail("a resume is marked")
									}
									if claim == sandbox.LineageClaimRestore && d.Mark != unrecappedRestore {
										fail("a restore is marked exactly when it restores an unrecapped snapshot")
									}
									if claim == sandbox.LineageClaimRestore && d.Announce {
										fail("a restore announces a loss")
									}
									if d.Mark != (freshLineage || unrecappedRestore) {
										fail("marked exactly when a spawn of an existing session follows a dispatched turn, or a restore restores an unrecapped snapshot")
									}
									if d.Announce && (!d.Mark || d.Carried) {
										fail("announced without a mark, or carried and announced again")
									}
									if d.Mark && claim == sandbox.LineageClaimSpawn && d.Announce == d.Carried {
										fail("a spawn's mark is either announced or carried")
									}
									if d.Carried != (d.Mark && pending) {
										fail("carried exactly when a mark replaces a gen whose own is pending")
									}
									if (d.Cause != sandbox.LineageCauseNone) != d.Mark {
										fail("a cause exactly when marked")
									}
									if d.Mark && claim == sandbox.LineageClaimSpawn {
										other := f
										other.ReplacedMarkPending = !pending
										if sandbox.DecideFreshLineage(other).Cause != d.Cause {
											fail("a carried mark takes another cause than an announced one")
										}
									}
									if (d.Cause == sandbox.LineageCauseUnrecappedSnapshot) != unrecappedRestore {
										fail("unrecapped_snapshot is the cause of exactly a restore of an unrecapped snapshot")
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if want := 4 * 2 * 2 * 2 * 3 * 2 * 2 * 2; visited != want {
		t.Fatalf("swept %d combinations, want %d: the sweep is broken", visited, want)
	}
}
