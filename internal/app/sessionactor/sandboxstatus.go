package sessionactor

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
)

// SandboxStatusEventType is the stored event this package appends when a
// transaction changes the session's sandbox status or generation
// (technical plan §3.2, §6.2). Its payload is the sandbox as the control
// plane now holds it, keyed under "sandbox" so a client reading the top
// level of every event for a sandbox-ws `gen` never mistakes it for one of
// the agent's own:
//
//	{"sandbox": {"gen": 2, "status": "ready"}}
//
// It reaches an open page the way every stored event does: broadcast after
// its transaction commits, and replayed by fetch_history, whose reply also
// carries the sandbox's current row (wshub's handleFetchHistory). A page
// therefore reads the boot from the status the server derives -- §3.2
// keeps a sandbox `booting` after the agent's `ready` until boot evidence
// and a null-phase heartbeat -- never from the agent's own events.
const SandboxStatusEventType = "sandbox_status"

// serverEventTypes are the event types the control plane writes into a
// session's event log itself and no sandbox-ws event has: this file's
// sandbox_status, imageresolve.go's image_decision, and the shadow-mode
// suppression record the postgres adapter writes (§30.6). They share the
// events.type namespace with the frames a sandbox sends, which
// handleSandboxEvent stores under the type each frame names. A page takes
// these as the server's own record -- a sandbox_status's gen and status
// move the rail -- so a frame of one of these types from the sandbox
// socket is dropped there, never stored (any process holding the sandbox
// token can open that socket). refusedSandboxFrame drops it, with the
// other frames a sandbox may not send: one without a type, and one shaped
// like the fetch_history reply whose sandbox row a page shows as the
// server's.
//
// This is a deny list of the control plane's own types, not an allow list
// of the sandbox-ws contract's: a newer sandbox-agent may send a type this
// binary does not know yet during a rollout, and that frame is still
// stored. A type written by the control plane that the contract also
// defines (execution_complete, warning) cannot be reserved: the agent
// sends it too. TestServerWrittenEventTypesAreReserved fails on any event
// type production code writes that is in neither the contract nor this
// set, and on an entry here that the contract defines or nothing writes.
var serverEventTypes = map[string]bool{
	SandboxStatusEventType:                   true,
	imageDecisionEventType:                   true,
	postgres.ShadowEgressSuppressedEventType: true,
}

// sandboxStatusKey is what the event reports, and what decides whether a
// write changed anything worth reporting.
type sandboxStatusKey struct {
	gen    int32
	status sqlcgen.SandboxStatus
}

func sandboxStatusKeyOf(row sqlcgen.Sandbox) sandboxStatusKey {
	return sandboxStatusKey{gen: row.Gen, status: row.Status}
}

// sandboxWriter is the one way this package writes a sandbox's status or
// generation: every write it makes is noted on the actor, so transact can
// append the sandbox_status event in the same transaction as the write
// (appendSandboxStatusIfChanged). TestSandboxStatusWritesGoThroughTheRecorder
// keeps every such write on it. The other SandboxStore methods change
// neither and are called on the store directly.
type sandboxWriter struct {
	a *Actor
	s *postgres.SandboxStore
}

// sandboxWrites returns the recorder for tx, the transaction transact
// opened.
func (a *Actor) sandboxWrites(tx pgx.Tx) sandboxWriter {
	return sandboxWriter{a: a, s: a.stores.sandbox.WithTx(tx)}
}

func (w sandboxWriter) note(row sqlcgen.Sandbox, err error) (sqlcgen.Sandbox, error) {
	if err == nil {
		key := sandboxStatusKeyOf(row)
		w.a.sandboxWritten = &key
	}
	return row, err
}

// UpdateStatus is postgres.SandboxStore.UpdateStatus, noted.
func (w sandboxWriter) UpdateStatus(ctx context.Context, arg sqlcgen.UpdateSandboxStatusParams) (sqlcgen.Sandbox, error) {
	return w.note(w.s.UpdateStatus(ctx, arg))
}

// UpdateStatusToSuspect is postgres.SandboxStore.UpdateStatusToSuspect, noted.
func (w sandboxWriter) UpdateStatusToSuspect(ctx context.Context, arg sqlcgen.UpdateSandboxStatusToSuspectParams) (sqlcgen.Sandbox, error) {
	return w.note(w.s.UpdateStatusToSuspect(ctx, arg))
}

// RecoverFromSuspect is postgres.SandboxStore.RecoverFromSuspect, noted.
func (w sandboxWriter) RecoverFromSuspect(ctx context.Context, arg sqlcgen.RecoverSandboxFromSuspectParams) (sqlcgen.Sandbox, error) {
	return w.note(w.s.RecoverFromSuspect(ctx, arg))
}

// UpsertForSpawn is postgres.SandboxStore.UpsertForSpawn, noted: it creates
// the row or bumps its generation, both a change a page has to see.
func (w sandboxWriter) UpsertForSpawn(ctx context.Context, arg sqlcgen.UpsertSandboxForSpawnParams) (sqlcgen.Sandbox, error) {
	return w.note(w.s.UpsertForSpawn(ctx, arg))
}

// appendSandboxStatusIfChanged appends the sandbox_status event inside tx
// when this transaction's last sandbox write left the sandbox at another
// generation or status than the actor's last commit did, and reports what
// that commit will leave. Only the transaction's last write is compared, so
// a transaction that passes through a status and back (live, suspect,
// stopped, in stop.go's retirement) reports where it ended, once; a write
// that changes neither -- the liveness bump every sandbox event makes --
// reports nothing.
func (a *Actor) appendSandboxStatusIfChanged(ctx context.Context, tx pgx.Tx) error {
	written := a.sandboxWritten
	if written == nil {
		return nil
	}
	if a.sandboxCommitted != nil && *a.sandboxCommitted == *written {
		return nil
	}
	if err := a.appendEvent(ctx, tx, SandboxStatusEventType, map[string]any{
		"sandbox": map[string]any{
			"gen":    written.gen,
			"status": string(written.status),
		},
	}); err != nil {
		return fmt.Errorf("sessionactor: append sandbox status event: %w", err)
	}
	return nil
}
