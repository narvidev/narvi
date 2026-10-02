//go:build integration

// This file proves technical plan §31.4's "Un-entitlement" for automation
// fan-out: a target whose repository an administrator revoked is refused
// at session creation like every other refusal (fanout.go's
// createFailedRun) -- a failed run with no session, never a stranded
// invocation. The revocation does not pause the automation itself, but each
// refused invocation counts a strike (§3.5), so three of them auto-pause it,
// and a restore does not resume it: that takes a person. In an invocation
// over several targets, the targets nobody revoked still run.
package automation_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
)

func TestPumpOnce_RevokedRepo_RecordsFailedRunNotStrandedInvocation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	auto, targets := f.createAutomation(t, "revoked repo fan-out test", 1)
	revocations := narvipg.NewRepoEntitlementRevocationStore(f.pool)
	if _, err := revocations.Revoke(ctx, "acme/"+targets[0].Name, pgtype.UUID{}, "frozen by an administrator"); err != nil {
		t.Fatalf("revoke %s: %v", targets[0].Name, err)
	}

	for i := 1; i <= domainautomation.AutoPauseThreshold; i++ {
		inv := f.createInvocation(t, auto.ID, targets)
		if err := f.engine.PumpOnce(ctx); err != nil {
			t.Fatalf("PumpOnce: %v", err)
		}

		runs := f.listRunsForInvocation(t, inv.ID)
		if len(runs) != 1 || runs[0].Status != sqlcgen.AutomationRunStatusFailed || runs[0].SessionID.Valid {
			t.Fatalf("invocation %d runs = %+v, want one failed run with no session", i, runs)
		}
		gotInv, err := f.invocations.Get(ctx, inv.ID)
		if err != nil {
			t.Fatalf("get invocation: %v", err)
		}
		if gotInv.Status != sqlcgen.AutomationInvocationStatusFailed {
			t.Fatalf("invocation %d status = %s, want failed, never stranded pending", i, gotInv.Status)
		}

		// The revocation itself pauses nothing: each refused invocation
		// counts one strike, and the third auto-pauses the automation.
		gotAuto, err := f.automations.Get(ctx, auto.ID)
		if err != nil {
			t.Fatalf("get automation: %v", err)
		}
		wantStatus := sqlcgen.AutomationStatusActive
		if i == domainautomation.AutoPauseThreshold {
			wantStatus = sqlcgen.AutomationStatusPaused
		}
		if gotAuto.Status != wantStatus || gotAuto.ConsecutiveFailures != int32(i) {
			t.Fatalf("after invocation %d: automation status = %s, consecutive failures = %d; want %s and %d", i, gotAuto.Status, gotAuto.ConsecutiveFailures, wantStatus, i)
		}
	}

	var denials int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'session.repo_entitlement_denied' AND detail_json->>'reason' = 'revoked'`).Scan(&denials); err != nil {
		t.Fatalf("count denial audit rows: %v", err)
	}
	if denials != domainautomation.AutoPauseThreshold {
		t.Errorf("revoked denial audit rows = %d, want one per invocation (%d)", denials, domainautomation.AutoPauseThreshold)
	}

	// A restore does not resume it: that takes a person, and until then an
	// invocation fans out nothing.
	if _, err := revocations.Restore(ctx, "acme/"+targets[0].Name); err != nil {
		t.Fatalf("restore: %v", err)
	}
	gotAuto, err := f.automations.Get(ctx, auto.ID)
	if err != nil {
		t.Fatalf("get automation: %v", err)
	}
	if gotAuto.Status != sqlcgen.AutomationStatusPaused {
		t.Errorf("automation status after the restore = %s, want still paused", gotAuto.Status)
	}
	next := f.createInvocation(t, auto.ID, targets)
	if err := f.engine.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	if runs := f.listRunsForInvocation(t, next.ID); len(runs) != 0 {
		t.Errorf("runs of an invocation of the paused automation = %d, want 0", len(runs))
	}
}

// TestPumpOnce_RevokedRepo_MixedInvocation_OtherTargetRunsAndAStrikeCounts:
// one invocation over two targets, one of whose repositories an
// administrator revoked. Entitlement is resolved per target: the revoked
// one gets a failed run with no session and one denial audit row, and the
// other still gets its session and runs -- a revoked target never aborts
// the rest of the fan-out, nor does an admitted one let it through. The
// invocation stays open while that run is live, and once it has finished
// the invocation fails, as any invocation with a failed run does, and
// counts one strike.
func TestPumpOnce_RevokedRepo_MixedInvocation_OtherTargetRunsAndAStrikeCounts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	auto, targets := f.createAutomation(t, "mixed revoked fan-out test", 2)
	revoked, admitted := targets[0], targets[1]
	if _, err := narvipg.NewRepoEntitlementRevocationStore(f.pool).Revoke(ctx, "acme/"+revoked.Name, pgtype.UUID{}, "frozen by an administrator"); err != nil {
		t.Fatalf("revoke %s: %v", revoked.Name, err)
	}
	inv := f.createInvocation(t, auto.ID, targets)
	if err := f.engine.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	runs := f.listRunsForInvocation(t, inv.ID)
	if len(runs) != 2 {
		t.Fatalf("got %d automation_runs rows, want 2: every target gets its own run, refused or not", len(runs))
	}
	var running sqlcgen.AutomationRun
	refused := 0
	for _, run := range runs {
		switch {
		case run.Status == sqlcgen.AutomationRunStatusFailed && !run.SessionID.Valid:
			refused++
		case run.SessionID.Valid && run.Status != sqlcgen.AutomationRunStatusFailed:
			running = run
		default:
			t.Fatalf("run %s = status %s, session %v; want one refused with no session and one with its session", run.ID.String(), run.Status, run.SessionID)
		}
	}
	if refused != 1 || !running.SessionID.Valid {
		t.Fatalf("refused runs = %d, admitted run = %v; want one each: the admitted target runs, the revoked one is refused", refused, running.ID)
	}
	session, err := narvipg.NewSessionStore(f.pool).Get(ctx, running.SessionID)
	if err != nil {
		t.Fatalf("get the admitted run's session: %v", err)
	}
	if !strings.Contains(string(session.Repos), "acme/"+admitted.Name) {
		t.Errorf("the admitted run's session repos = %s, want %s's", session.Repos, admitted.Name)
	}
	var denials int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'session.repo_entitlement_denied' AND detail_json->>'reason' = 'revoked'`).Scan(&denials); err != nil {
		t.Fatalf("count denial audit rows: %v", err)
	}
	if denials != 1 {
		t.Errorf("revoked denial audit rows = %d, want 1: the revoked target alone", denials)
	}
	gotInv, err := f.invocations.Get(ctx, inv.ID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if gotInv.ClosedAt.Valid {
		t.Fatalf("invocation closed as %s while its admitted run is live", gotInv.Status)
	}

	// The admitted run's turn finishes; the invocation closes failed and
	// counts one strike.
	f.setTurnStatus(t, running, sqlcgen.TurnStatusProcessing)
	if err := f.engine.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce (promote): %v", err)
	}
	f.setTurnStatus(t, running, sqlcgen.TurnStatusCompleted)
	if err := f.engine.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce (terminalize): %v", err)
	}
	if gotRun, err := f.runs.Get(ctx, running.ID); err != nil || gotRun.Status != sqlcgen.AutomationRunStatusSucceeded {
		t.Fatalf("admitted run = %v (err %v), want succeeded", gotRun.Status, err)
	}
	gotInv, err = f.invocations.Get(ctx, inv.ID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if gotInv.Status != sqlcgen.AutomationInvocationStatusFailed || !gotInv.FailureCountedAt.Valid {
		t.Fatalf("invocation status = %s, failure counted %v; want failed and counted: one of its runs was refused", gotInv.Status, gotInv.FailureCountedAt.Valid)
	}
	gotAuto, err := f.automations.Get(ctx, auto.ID)
	if err != nil {
		t.Fatalf("get automation: %v", err)
	}
	if gotAuto.ConsecutiveFailures != auto.ConsecutiveFailures+1 || gotAuto.Status != sqlcgen.AutomationStatusActive {
		t.Errorf("automation consecutive failures = %d, status %s; want %d and active: one strike, short of the auto-pause", gotAuto.ConsecutiveFailures, gotAuto.Status, auto.ConsecutiveFailures+1)
	}
}
