package automation_test

import (
	"errors"
	"testing"

	"github.com/narvidev/narvi/internal/domain/automation"
)

func TestValidateTriggerType(t *testing.T) {
	tests := []struct {
		name    string
		trigger automation.TriggerType
		wantErr bool
	}{
		{"manual", automation.TriggerTypeManual, false},
		{"cron", automation.TriggerTypeCron, false},
		{"github", automation.TriggerTypeGitHub, false},
		{"linear", automation.TriggerTypeLinear, false},
		{"webhook", automation.TriggerTypeWebhook, false},
		{"unknown", automation.TriggerType("bogus"), true},
		{"empty", automation.TriggerType(""), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := automation.ValidateTriggerType(tt.trigger)
			if tt.wantErr {
				if !errors.Is(err, automation.ErrUnknownTriggerType) {
					t.Fatalf("got %v, want ErrUnknownTriggerType", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateCronTriggerConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     automation.CronTriggerConfig
		wantErr error
	}{
		{"valid", automation.CronTriggerConfig{Schedule: "0 2 * * *"}, nil},
		{"empty schedule", automation.CronTriggerConfig{Schedule: ""}, automation.ErrEmptyCronSchedule},
		{"invalid schedule", automation.CronTriggerConfig{Schedule: "not a cron"}, automation.ErrCronFieldCount},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := automation.ValidateCronTriggerConfig(tt.cfg)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("got %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidateGitHubTriggerConfig(t *testing.T) {
	if err := automation.ValidateGitHubTriggerConfig(automation.GitHubTriggerConfig{Event: "pull_request"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	err := automation.ValidateGitHubTriggerConfig(automation.GitHubTriggerConfig{})
	if !errors.Is(err, automation.ErrEmptyGitHubEvent) {
		t.Fatalf("got %v, want ErrEmptyGitHubEvent", err)
	}
}

// TestValidateGitHubTriggerConfig_RejectsEventOutsideDispatchAllowlist is
// D10's own audit fix, pinned: before this fix, an Event value outside
// GitHubDispatchAllowlist (e.g. "release", a real GitHub event this
// dispatch path simply never subscribes to) was accepted at
// automation-create time and then NEVER matched anything at live dispatch
// time (ClassifyGitHubDispatch skips it there) -- silently dead forever,
// with no feedback to whoever configured it.
func TestValidateGitHubTriggerConfig_RejectsEventOutsideDispatchAllowlist(t *testing.T) {
	err := automation.ValidateGitHubTriggerConfig(automation.GitHubTriggerConfig{Event: "release"})
	if !errors.Is(err, automation.ErrGitHubEventNotDispatchable) {
		t.Fatalf("got %v, want ErrGitHubEventNotDispatchable", err)
	}
}

func TestMatchesGitHubTrigger(t *testing.T) {
	tests := []struct {
		name string
		cfg  automation.GitHubTriggerConfig
		in   automation.GitHubEventInput
		want bool
	}{
		{
			"event only, matches",
			automation.GitHubTriggerConfig{Event: "pull_request"},
			automation.GitHubEventInput{EventType: "pull_request", Action: "opened"},
			true,
		},
		{
			"event mismatch",
			automation.GitHubTriggerConfig{Event: "pull_request"},
			automation.GitHubEventInput{EventType: "issues", Action: "opened"},
			false,
		},
		{
			"event+action, matches",
			automation.GitHubTriggerConfig{Event: "pull_request", Action: "labeled"},
			automation.GitHubEventInput{EventType: "pull_request", Action: "labeled"},
			true,
		},
		{
			"event matches but action mismatch",
			automation.GitHubTriggerConfig{Event: "pull_request", Action: "labeled"},
			automation.GitHubEventInput{EventType: "pull_request", Action: "opened"},
			false,
		},
		{
			"label required and present",
			automation.GitHubTriggerConfig{Event: "pull_request", Label: "automation:run"},
			automation.GitHubEventInput{EventType: "pull_request", Labels: []string{"bug", "automation:run"}},
			true,
		},
		{
			"label required but absent",
			automation.GitHubTriggerConfig{Event: "pull_request", Label: "automation:run"},
			automation.GitHubEventInput{EventType: "pull_request", Labels: []string{"bug"}},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := automation.MatchesGitHubTrigger(tt.cfg, tt.in)
			if got != tt.want {
				t.Fatalf("MatchesGitHubTrigger() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateLinearTriggerConfig(t *testing.T) {
	if err := automation.ValidateLinearTriggerConfig(automation.LinearTriggerConfig{EventType: "Issue", OrganizationID: "org-1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	err := automation.ValidateLinearTriggerConfig(automation.LinearTriggerConfig{})
	if !errors.Is(err, automation.ErrEmptyLinearEventType) {
		t.Fatalf("got %v, want ErrEmptyLinearEventType", err)
	}
}

// TestValidateLinearTriggerConfig_RejectsEventTypeOutsideDispatchAllowlist
// mirrors TestValidateGitHubTriggerConfig_RejectsEventOutsideDispatchAllowlist,
// for Linear -- D10's own audit fix.
func TestValidateLinearTriggerConfig_RejectsEventTypeOutsideDispatchAllowlist(t *testing.T) {
	err := automation.ValidateLinearTriggerConfig(automation.LinearTriggerConfig{EventType: "AgentSessionEvent", OrganizationID: "org-1"})
	if !errors.Is(err, automation.ErrLinearEventNotDispatchable) {
		t.Fatalf("got %v, want ErrLinearEventNotDispatchable", err)
	}
}

// TestValidateLinearTriggerConfig_RequiresOrganizationID is W3's own
// required proof (confirmed HIGH, TENANT ISOLATION finding): an empty
// OrganizationID must be rejected at creation time, exactly like an empty
// EventType -- never silently accepted and left to fire for every
// installed workspace.
func TestValidateLinearTriggerConfig_RequiresOrganizationID(t *testing.T) {
	err := automation.ValidateLinearTriggerConfig(automation.LinearTriggerConfig{EventType: "Issue"})
	if !errors.Is(err, automation.ErrEmptyLinearOrganizationID) {
		t.Fatalf("got %v, want ErrEmptyLinearOrganizationID", err)
	}
}

// TestValidateLinearTriggerConfig_RejectsTeamFilterOnCommentEvent is W6's
// own required proof (confirmed MEDIUM finding: "a Linear Comment trigger
// with a team filter can never fire"): the impossible combination must be
// refused at creation time, not accepted and left silently dead.
func TestValidateLinearTriggerConfig_RejectsTeamFilterOnCommentEvent(t *testing.T) {
	err := automation.ValidateLinearTriggerConfig(automation.LinearTriggerConfig{EventType: "Comment", TeamKey: "ENG", OrganizationID: "org-1"})
	if !errors.Is(err, automation.ErrLinearTeamFilterNotSupported) {
		t.Fatalf("got %v, want ErrLinearTeamFilterNotSupported", err)
	}
	// An UNCONFIGURED team filter on the SAME event type is fine -- only a
	// non-empty TeamKey is the impossible combination.
	if err := automation.ValidateLinearTriggerConfig(automation.LinearTriggerConfig{EventType: "Comment", OrganizationID: "org-1"}); err != nil {
		t.Fatalf("unexpected error for an unconfigured team filter on Comment: %v", err)
	}
}

func TestMatchesLinearTrigger(t *testing.T) {
	tests := []struct {
		name string
		cfg  automation.LinearTriggerConfig
		in   automation.LinearEventInput
		want bool
	}{
		{
			"event type only, matches",
			automation.LinearTriggerConfig{EventType: "Issue", OrganizationID: "org-1"},
			automation.LinearEventInput{EventType: "Issue", Action: "create", OrganizationID: "org-1"},
			true,
		},
		{
			"event type mismatch",
			automation.LinearTriggerConfig{EventType: "Issue", OrganizationID: "org-1"},
			automation.LinearEventInput{EventType: "Comment", OrganizationID: "org-1"},
			false,
		},
		{
			"team filter matches",
			automation.LinearTriggerConfig{EventType: "Issue", TeamKey: "ENG", OrganizationID: "org-1"},
			automation.LinearEventInput{EventType: "Issue", TeamKey: "ENG", OrganizationID: "org-1"},
			true,
		},
		{
			"team filter mismatch",
			automation.LinearTriggerConfig{EventType: "Issue", TeamKey: "ENG", OrganizationID: "org-1"},
			automation.LinearEventInput{EventType: "Issue", TeamKey: "OPS", OrganizationID: "org-1"},
			false,
		},
		{
			// W3 audit fix's own required proof at the matcher level: an
			// otherwise fully-matching event from a DIFFERENT organization
			// must never match -- this is the tenant boundary itself.
			"organization mismatch, otherwise fully matching",
			automation.LinearTriggerConfig{EventType: "Issue", Action: "create", TeamKey: "ENG", OrganizationID: "org-1"},
			automation.LinearEventInput{EventType: "Issue", Action: "create", TeamKey: "ENG", OrganizationID: "org-2"},
			false,
		},
		{
			// Zero-value guard: an unconfigured (empty) cfg.OrganizationID
			// must never match, even against an event whose own
			// OrganizationID is ALSO empty -- an unconditional "" == ""
			// comparison would fail OPEN here.
			"both organization ids empty must not match",
			automation.LinearTriggerConfig{EventType: "Issue", OrganizationID: ""},
			automation.LinearEventInput{EventType: "Issue", OrganizationID: ""},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := automation.MatchesLinearTrigger(tt.cfg, tt.in)
			if got != tt.want {
				t.Fatalf("MatchesLinearTrigger() = %v, want %v", got, tt.want)
			}
		})
	}
}
