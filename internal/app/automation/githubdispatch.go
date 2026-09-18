package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres/sqlcgen"
	domainautomation "github.com/narvidev/narvi/internal/domain/automation"
)

// githubTriggerConfigJSON is this package's OWN small, private, decode-only
// copy of the github trigger_config wire shape -- see
// cronTriggerConfigJSON's own doc comment (triggerpump.go) for why this is
// never shared with internal/adapters/inbound/httpapi's own
// githubTriggerConfigWire (an import cycle: this package already imports
// httpapi, for CreateSessionOnTx). Field names/json tags match that wire
// struct exactly, so a value httpapi wrote decodes here unchanged.
type githubTriggerConfigJSON struct {
	Event      string `json:"event"`
	Action     string `json:"action,omitempty"`
	Label      string `json:"label,omitempty"`
	Name       string `json:"name,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
}

func unmarshalGitHubTriggerConfig(raw []byte) (domainautomation.GitHubTriggerConfig, error) {
	var wire githubTriggerConfigJSON
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &wire); err != nil {
			return domainautomation.GitHubTriggerConfig{}, fmt.Errorf("automation: unmarshal github trigger config: %w", err)
		}
	}
	return domainautomation.GitHubTriggerConfig{
		Event:      wire.Event,
		Action:     wire.Action,
		Label:      wire.Label,
		Name:       wire.Name,
		Conclusion: wire.Conclusion,
	}, nil
}

// GitHubTriggerLister is the narrow slice of *postgres.AutomationStore
// DispatchGitHubWebhookEvent needs -- mirrors InvocationCreator's own
// "small, locally-defined interface so a unit/integration test can inject
// a fake with no real DB round trip, or one that deliberately panics to
// prove dispatch failure isolation" reasoning (invocationenqueue.go).
type GitHubTriggerLister interface {
	ListActiveGitHubAutomations(ctx context.Context) ([]sqlcgen.Automation, error)
}

// DispatchGitHubWebhookEvent is §8.4's own live-dispatch entry point
// for a real, already-claimed (deduplicated -- see this package's own
// doc.go) GitHub webhook delivery: called inline, synchronously, from
// internal/adapters/inbound/github's own handler.go, for EVERY delivery
// that handler processes -- an ADDITIONAL, independent consumer of the
// SAME delivery the @mention/merge-gate lanes already process, never a
// replacement for either (see that adapter's own doc comment on why a
// panic/error here must never suppress them).
//
// domainautomation.ClassifyGitHubDispatch(eventType) is checked first --
// see that function's own doc comment for the closed, typed allowlist it
// enforces; an eventType outside it returns immediately, before ever
// listing an automation.
//
// For each active, TriggerTypeGitHub automation whose own trigger_config
// (domainautomation.MatchesGitHubTrigger) matches in, this narrows that
// automation's own configured target repos to exactly the ones
// domainautomation.TargetMatchesGitHubEvent accepts (repo scoping, and --
// §8.4's own named trap -- branch-TIP scoping, never containment) and,
// only when at least one target survives that narrowing, calls
// CreateInvocation with that narrowed target list. One automation's own
// failure (a malformed trigger_config, a Postgres error) is isolated:
// logged, and does NOT abort evaluating the rest of the batch -- mirrors
// evaluateCronAutomation's own identical per-row isolation (triggerpump.go).
func DispatchGitHubWebhookEvent(ctx context.Context, logger *slog.Logger, automations GitHubTriggerLister, invocations InvocationCreator, eventType string, in domainautomation.GitHubEventInput) {
	if reason := domainautomation.ClassifyGitHubDispatch(eventType); reason != domainautomation.GitHubDispatchNotSkipped {
		logger.Debug("automation: github event not evaluated against any trigger", "event_type", eventType, "reason", string(reason))
		return
	}

	rows, err := automations.ListActiveGitHubAutomations(ctx)
	if err != nil {
		logger.Error("automation: list active github automations failed", "error", err)
		return
	}

	for _, row := range rows {
		dispatchOneGitHubAutomation(ctx, logger, invocations, row, eventType, in)
	}
}

func dispatchOneGitHubAutomation(ctx context.Context, logger *slog.Logger, invocations InvocationCreator, row sqlcgen.Automation, eventType string, in domainautomation.GitHubEventInput) {
	logger = logger.With("automation_id", row.ID.String())

	cfg, err := unmarshalGitHubTriggerConfig(row.TriggerConfig)
	if err != nil {
		logger.Error("automation: decode github trigger config failed", "error", err)
		return
	}
	if !domainautomation.MatchesGitHubTrigger(cfg, in) {
		return
	}

	targets, err := UnmarshalTargets(row.Repos)
	if err != nil {
		logger.Error("automation: decode automation repos for github dispatch failed", "error", err)
		return
	}

	matched := make([]domainautomation.Target, 0, len(targets))
	for _, target := range targets {
		if domainautomation.TargetMatchesGitHubEvent(target, in) {
			matched = append(matched, target)
		}
	}
	if len(matched) == 0 {
		// The trigger's own Event/Action/Label/Name/Conclusion filter
		// matched, but none of this automation's own configured target
		// repos/branches actually concern this event (wrong repo, or --
		// §8.4's own named trap -- a branch that merely contains the
		// commit rather than being its tip). Not an error: this
		// automation simply has nothing to do for this delivery.
		logger.Debug("automation: github trigger matched but no configured target repo/branch concerns this event", "event_type", eventType)
		return
	}

	if _, err := CreateInvocation(ctx, invocations, row.ID, matched); err != nil {
		logger.Error("automation: create invocation for github dispatch failed", "error", err, "event_type", eventType)
	}
}
