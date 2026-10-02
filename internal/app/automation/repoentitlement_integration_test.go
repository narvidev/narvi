//go:build integration

// This file proves technical plan §31.4's "Un-entitlement" for automation
// fan-out: a target whose repository an administrator revoked is refused
// at session creation like every other refusal (fanout.go's
// createFailedRun) -- a failed run with no session, never a stranded
// invocation -- while the automation itself is not paused, and a target
// nobody revoked still runs.
package automation_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	narvipg "github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

func TestPumpOnce_RevokedRepo_RecordsFailedRunNotStrandedInvocation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	auto, targets := f.createAutomation(t, "revoked repo fan-out test", 2)
	revocations := narvipg.NewRepoEntitlementRevocationStore(f.pool)
	for _, target := range targets {
		if _, err := revocations.Revoke(ctx, "acme/"+target.Name, pgtype.UUID{}, "frozen by an administrator"); err != nil {
			t.Fatalf("revoke %s: %v", target.Name, err)
		}
	}
	inv := f.createInvocation(t, auto.ID, targets)

	if err := f.engine.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}

	runs := f.listRunsForInvocation(t, inv.ID)
	if len(runs) != 2 {
		t.Fatalf("got %d automation_runs rows, want 2 -- every target gets one recorded run, refused or not", len(runs))
	}
	for _, run := range runs {
		if run.Status != sqlcgen.AutomationRunStatusFailed || run.SessionID.Valid {
			t.Errorf("run %s = status %s, session %v; want failed with no session", run.ID.String(), run.Status, run.SessionID)
		}
	}
	gotInv, err := f.invocations.Get(ctx, inv.ID)
	if err != nil {
		t.Fatalf("get invocation: %v", err)
	}
	if gotInv.Status != sqlcgen.AutomationInvocationStatusFailed {
		t.Fatalf("invocation status = %s, want failed, never stranded pending", gotInv.Status)
	}
	var denials int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'session.repo_entitlement_denied' AND detail_json->>'reason' = 'revoked'`).Scan(&denials); err != nil {
		t.Fatalf("count denial audit rows: %v", err)
	}
	if denials != 2 {
		t.Errorf("revoked denial audit rows = %d, want one per target", denials)
	}

	// Not paused: the automation's status is unchanged, the failed
	// invocation counting one strike, and once one repository is restored
	// its next invocation runs that target.
	gotAuto, err := f.automations.Get(ctx, auto.ID)
	if err != nil {
		t.Fatalf("get automation: %v", err)
	}
	if gotAuto.Status != auto.Status {
		t.Errorf("automation status = %s, want %s: a revocation does not pause automations", gotAuto.Status, auto.Status)
	}
	if gotAuto.ConsecutiveFailures != auto.ConsecutiveFailures+1 {
		t.Errorf("consecutive failures = %d, want %d: the failed invocation counts one strike", gotAuto.ConsecutiveFailures, auto.ConsecutiveFailures+1)
	}
	if _, err := revocations.Restore(ctx, "acme/"+targets[1].Name); err != nil {
		t.Fatalf("restore: %v", err)
	}
	next := f.createInvocation(t, auto.ID, targets)
	if err := f.engine.PumpOnce(ctx); err != nil {
		t.Fatalf("PumpOnce: %v", err)
	}
	statuses := map[bool]int{}
	for _, run := range f.listRunsForInvocation(t, next.ID) {
		statuses[run.SessionID.Valid]++
	}
	if statuses[true] != 1 || statuses[false] != 1 {
		t.Errorf("runs of the next invocation with a session / without = %d / %d, want 1 / 1: the restored target runs, the revoked one is refused", statuses[true], statuses[false])
	}
}
