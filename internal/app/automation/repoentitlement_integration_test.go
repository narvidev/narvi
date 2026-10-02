//go:build integration

// This file proves technical plan §31.4's "Un-entitlement" for automation
// fan-out: a target whose repository an administrator revoked is refused
// at session creation like every other refusal (fanout.go's
// createFailedRun) -- a failed run with no session, never a stranded
// invocation. The revocation does not pause the automation itself, but each
// refused invocation counts a strike (§3.5), so three of them auto-pause it,
// and a restore does not resume it: that takes a person.
package automation_test

import (
	"context"
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
