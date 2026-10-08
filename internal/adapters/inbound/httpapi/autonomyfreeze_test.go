package httpapi

// This file unit-tests the autonomy freeze's pure mappers (technical plan
// §40.2): the freeze onto the wire, the autonomy.unfrozen audit detail,
// and the decision inbox's freeze fields -- no Postgres, no HTTP.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	"github.com/narvidev/narvi/internal/app/decisioninbox"
	domaindecisioninbox "github.com/narvidev/narvi/internal/domain/decisioninbox"
)

func TestAutonomyFreezeDTO(t *testing.T) {
	at := time.Date(2026, 10, 7, 9, 30, 0, 0, time.UTC)
	by := pgtype.UUID{Bytes: [16]byte{1, 2, 3}, Valid: true}
	name, reason := "Ada Admin", "an incident"
	for _, tc := range []struct {
		name string
		row  sqlcgen.GetAutonomyFreezeRow
		want string
	}{
		{"not frozen", sqlcgen.GetAutonomyFreezeRow{},
			`{"frozen":false,"frozenAt":null,"frozenByDisplayName":null,"frozenByUserId":null,"reason":null}`},
		{"frozen", sqlcgen.GetAutonomyFreezeRow{
			AutonomyFrozen: true, AutonomyFrozenAt: pgtype.Timestamptz{Time: at, Valid: true}, AutonomyFrozenBy: by,
			AutonomyFreezeReason: &reason, AutonomyFrozenByDisplayName: &name,
		}, `{"frozen":true,"frozenAt":"2026-10-07T09:30:00Z","frozenByDisplayName":"Ada Admin","frozenByUserId":"` + by.String() + `","reason":"an incident"}`},
		{"frozen by a deleted user", sqlcgen.GetAutonomyFreezeRow{
			AutonomyFrozen: true, AutonomyFrozenAt: pgtype.Timestamptz{Time: at, Valid: true}, AutonomyFreezeReason: &reason,
		}, `{"frozen":true,"frozenAt":"2026-10-07T09:30:00Z","frozenByDisplayName":null,"frozenByUserId":null,"reason":"an incident"}`},
		// A row read not frozen says nothing else, whatever its columns.
		{"not frozen, stray columns", sqlcgen.GetAutonomyFreezeRow{AutonomyFreezeReason: &reason, AutonomyFrozenBy: by},
			`{"frozen":false,"frozenAt":null,"frozenByDisplayName":null,"frozenByUserId":null,"reason":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(autonomyFreezeDTO(tc.row))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("autonomyFreezeDTO = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestUnfreezeAuditDetail(t *testing.T) {
	frozenAt := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	by := pgtype.UUID{Bytes: [16]byte{9}, Valid: true}
	reason := "a bad deploy"
	for _, tc := range []struct {
		name        string
		lifted      sqlcgen.UnfreezeAutonomyRow
		now         time.Time
		wantSeconds int64
		wantBy      any
	}{
		{"held ninety minutes", sqlcgen.UnfreezeAutonomyRow{FrozenAt: pgtype.Timestamptz{Time: frozenAt, Valid: true}, FrozenBy: by, Reason: &reason},
			frozenAt.Add(90*time.Minute + 400*time.Millisecond), 5400, by.String()},
		{"a clock behind the freeze never reads negative", sqlcgen.UnfreezeAutonomyRow{FrozenAt: pgtype.Timestamptz{Time: frozenAt, Valid: true}, Reason: &reason},
			frozenAt.Add(-time.Minute), 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := unfreezeAuditDetail(tc.lifted, tc.now)
			if d["frozen_seconds"] != tc.wantSeconds || d["frozen_by"] != tc.wantBy || d["reason"] != tc.lifted.Reason || d["frozen_at"] != tc.lifted.FrozenAt.Time {
				t.Errorf("unfreezeAuditDetail = %v, want frozen_seconds %d, frozen_by %v, the reason and frozen_at", d, tc.wantSeconds, tc.wantBy)
			}
		})
	}
}

// TestDecisionInboxResultToDTO_FreezeFields: the freeze, its unread flag,
// each item's held mark and the held workflow advances reach the wire --
// the advances as an empty list, never null, when there are none.
func TestDecisionInboxResultToDTO_FreezeFields(t *testing.T) {
	reason := "an incident"
	title := "the owner's build"
	heldAt := time.Date(2026, 10, 7, 9, 45, 0, 0, time.UTC)
	result := decisioninbox.Result{
		Items: []decisioninbox.Item{
			{Kind: domaindecisioninbox.KindReadyToMerge, RepoFullName: "acme/widgets", PRNumber: 1, HeldByFreeze: true},
			{Kind: domaindecisioninbox.KindReadyToMerge, RepoFullName: "acme/other", PRNumber: 2},
		},
		AutonomyFreeze: sqlcgen.GetAutonomyFreezeRow{AutonomyFrozen: true, AutonomyFrozenAt: pgtype.Timestamptz{Time: heldAt, Valid: true}, AutonomyFreezeReason: &reason},
		HeldWorkflowAdvances: []decisioninbox.HeldWorkflowAdvance{{
			WorkflowRunID: "run-1", SessionID: "session-1", SessionTitle: &title, WorkflowName: "build then test", HeldAt: heldAt,
		}},
	}
	dto := decisionInboxResultToDTO(result)
	if !dto.AutonomyFreeze.Frozen || dto.AutonomyFreeze.Reason == nil || *dto.AutonomyFreeze.Reason != reason || dto.AutonomyFreezeUnread {
		t.Errorf("freeze = %+v, unread %v; want the freeze, read", dto.AutonomyFreeze, dto.AutonomyFreezeUnread)
	}
	if !dto.Items[0].HeldByFreeze || dto.Items[1].HeldByFreeze {
		t.Errorf("held marks = %v, %v; want true, false", dto.Items[0].HeldByFreeze, dto.Items[1].HeldByFreeze)
	}
	if len(dto.HeldWorkflowAdvances) != 1 || dto.HeldWorkflowAdvances[0].WorkflowRunId != "run-1" || dto.HeldWorkflowAdvances[0].WorkflowName != "build then test" ||
		dto.HeldWorkflowAdvances[0].SessionTitle == nil || *dto.HeldWorkflowAdvances[0].SessionTitle != title || !dto.HeldWorkflowAdvances[0].HeldAt.Equal(heldAt) {
		t.Errorf("held advances = %+v, want the one run", dto.HeldWorkflowAdvances)
	}

	empty, err := json.Marshal(decisionInboxResultToDTO(decisioninbox.Result{AutonomyFreezeUnread: true}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"heldWorkflowAdvances":[]`, `"autonomyFreezeUnread":true`, `"autonomyFreeze":{"frozen":false,`} {
		if !strings.Contains(string(empty), want) {
			t.Errorf("an empty inbox's body %s lacks %s", empty, want)
		}
	}
}
