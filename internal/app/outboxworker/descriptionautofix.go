package outboxworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/narvidev/narvi/internal/adapters/outbound/postgres"
	"github.com/narvidev/narvi/internal/app/ports"
	"github.com/narvidev/narvi/internal/domain/review"
	"github.com/narvidev/narvi/internal/domain/reviewpost"
	"github.com/narvidev/narvi/internal/platform"
)

// This file implements §26.2's own ("review digest: description
// adequacy + graduated remediation", §26.2) description-autofix notifier:
// ports.NotificationKindGitHubDescriptionAutofix's own real Deliver.
// Lives in internal/app/outboxworker, mirroring sentinelAutoFixNotifier's
// own identical placement (sentinelautofix.go) -- this is the "real
// outbound network/side-effect work, never synchronously in the HTTP
// request" layer every other Notifier in this package already lives at,
// and it needs BOTH a Postgres read (repoSettings/artifacts, to
// re-verify eligibility) AND a real outbound GitHub write
// (sourceControl), exactly the combination this package's own siblings
// already establish a home for.
//
// # Why this notifier, not httpapi/reviewverdict.go, decides eligibility
//
// §26.2/§5.2's own central requirement: "both the Narvi-authorship check
// and the flag check are enforced server-side at delivery time... never
// prompt-only, never trusting the agent to self-enforce". reviewverdict.go
// enqueues this Kind's own outbox row unconditionally whenever the agent
// proposed a body (DescriptionAutofixPayload's own doc comment) -- this
// Deliver method is the ONE place both checks actually run, fresh, every
// single delivery attempt (including every retry), mirroring
// githubapi.VerdictNotifier's own "compute the label-sync plan AT
// DELIVERY TIME, not at enqueue time" precedent one layer further: here,
// even WHETHER to write at all is deferred to delivery time, not merely
// the mechanics of the write.
//
// # Fail-safe direction
//
// Every genuinely CONFIRMED-negative outcome (a real repo_settings row
// with the flag off, or a confirmed absence of a platform-authored
// artifact for this PR) is a silent, logged no-op -- Deliver returns nil,
// and the outbox marks this row delivered, never retried. This is
// deliberate: retrying a confirmed "no" forever would never converge on
// anything different. A genuine, UNCERTAIN failure (a transient Postgres
// read error, a GitHub API failure) is instead returned as a real error,
// so the outbox's own existing backoff/retry/dead-letter machinery (§5.1)
// handles it -- never silently treated as though it were a confirmed "no"
// (which would permanently and silently drop a legitimate autofix on one
// transient hiccup) and never silently treated as though it were a
// confirmed "yes" (which would risk writing to a PR this notifier was
// never actually able to confirm was eligible). Every path in this file
// that is uncertain about eligibility fails toward "don't rewrite the
// description" -- see each branch below for the specific reasoning.
type descriptionAutofixNotifier struct {
	repoSettings  *postgres.RepoSettingsStore
	artifacts     *postgres.ArtifactStore
	sourceControl ports.SourceControl
	outbound      *platform.GitHubOutboundConfig
	timeouts      platform.Timeouts
}

var _ ports.Notifier = (*descriptionAutofixNotifier)(nil)

// NewDescriptionAutofixNotifier builds a ports.Notifier for
// ports.NotificationKindGitHubDescriptionAutofix -- called once by cmd/
// control-plane/main.go's own kind->Notifier map assembly, mirroring
// every other notifier constructor's own identical "called exactly once"
// precedent. sourceControl/outbound/timeouts are the SAME
// instances/values production wiring already constructs for every other
// GitHub-flavored notifier (e.g. githubapi.NewVerdictNotifier's own
// sourceControl/cfg.GitHubOutbound). outbound must be non-nil: the
// rewrite is made as the bot, so the notifier refuses to exist with
// GitHub outbound off (§12.5).
func NewDescriptionAutofixNotifier(
	repoSettings *postgres.RepoSettingsStore,
	artifacts *postgres.ArtifactStore,
	sourceControl ports.SourceControl,
	outbound *platform.GitHubOutboundConfig,
	timeouts platform.Timeouts,
) (ports.Notifier, error) {
	if err := platform.RequireGitHubOutbound(outbound, "outboxworker: new description autofix notifier"); err != nil {
		return nil, err
	}
	return &descriptionAutofixNotifier{
		repoSettings:  repoSettings,
		artifacts:     artifacts,
		sourceControl: sourceControl,
		outbound:      outbound,
		timeouts:      timeouts,
	}, nil
}

// pullRequestHTMLURL builds the SAME deterministic "https://github.com/
// {owner}/{repo}/pull/{number}" shape GitHub's own REST API always
// returns as a pull request's html_url (the exact value
// internal/app/sessionactor's own recordPRArtifact persists verbatim from
// a real CreatePR response, ports.PRRef.URL) -- constructed here rather
// than fetched via a THIRD real GitHub API call, since this notifier
// already needs to fetch the PR's own body separately (GetPRBody) and a
// dedicated fetch purely to resolve a deterministic, well-documented URL
// format would be pure overhead. A mismatch here (e.g. GitHub renaming a
// repo, changing case) can only ever make this function return a URL
// isPlatformAuthored's own lookup fails to match against an existing
// artifact row -- the SAFE direction (§26.2's own fail-safe requirement):
// a false "not platform-authored" skips the write, it can never
// manufacture a false "is platform-authored".
func pullRequestHTMLURL(owner, repo string, number int) string {
	return fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, number)
}

// isPlatformAuthored reports whether some Narvi session pushed and opened
// the pull request at htmlURL -- mirrors internal/app/decisioninbox's own
// identical isPlatformAuthored helper (aggregate.go) byte-for-byte in
// spirit: an artifacts row of type 'pr' exists for this exact URL
// (internal/app/sessionactor/pushpr.go's own recordPRArtifact, the ONE
// place such a row is ever written). err == nil means found (platform-
// authored); pgx.ErrNoRows means confirmed NOT platform-authored (a
// legitimate, common answer for a human-opened PR); any OTHER error is a
// genuine, uncertain read failure the caller must not treat as a
// confirmed negative.
func isPlatformAuthored(ctx context.Context, artifacts *postgres.ArtifactStore, htmlURL string) (authored bool, uncertain error) {
	_, err := artifacts.GetPRArtifactByURL(ctx, htmlURL)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return false, err
}

// Deliver implements ports.Notifier: decodes n.Payload, re-verifies BOTH
// the Narvi-authorship of the target PR and this repo's own
// descriptionAutofix flag FRESH (never trusted from the payload for
// EITHER of those two -- DescriptionAutofixPayload's own doc comment),
// PLUS re-asserts the payload's own carried DescriptionAdequacy (a fact
// fixed at verdict time, never re-derivable from live state, so this
// third check re-checks the CARRIED value rather than looking anything up
// -- adversarial-review fix, §26.2's own follow-up) -- then, only
// if all three pass, re-fetches the PR's own CURRENT body, composes the
// new body (internal/domain/reviewpost.RenderAutofixBody), and writes it.
// See this file's own top doc comment for the full fail-safe direction
// every branch below follows.
func (n *descriptionAutofixNotifier) Deliver(ctx context.Context, notification ports.Notification) error {
	if notification.Kind != ports.NotificationKindGitHubDescriptionAutofix {
		return fmt.Errorf("outboxworker: descriptionAutofixNotifier: unrecognized notification kind %q", notification.Kind)
	}

	var payload ports.DescriptionAutofixPayload
	if err := json.Unmarshal(notification.Payload, &payload); err != nil {
		return fmt.Errorf("outboxworker: descriptionAutofixNotifier: decode payload: %w", err)
	}

	// Check 0: DescriptionAdequacy, re-asserted from the payload's own
	// CARRIED value (never a live re-derivation -- DescriptionAutofixPayload.
	// DescriptionAdequacy's own doc comment explains why none is possible
	// or needed for this particular fact). An ALLOW-list, not a deny-list:
	// only "drift"/"misleading" proceed -- "ok", the zero value (an older
	// outbox row enqueued before this field existed), and any other
	// unrecognized value all fail toward the SAME confirmed, silent,
	// never-retried no-op every other confirmed-negative check in this
	// file already returns. This is pure payload inspection, no I/O, so it
	// runs first, before either real check below.
	switch payload.DescriptionAdequacy {
	case review.DescriptionAdequacyDrift, review.DescriptionAdequacyMisleading:
	default:
		return nil
	}

	repoFullName := payload.Owner + "/" + payload.Repo

	// Check 1: this repo's own descriptionAutofix flag, fresh. A missing
	// row means "never configured" -- this table's own established
	// fail-closed-on-missing-row precedent (every sibling toggle already
	// treats a missing row as OFF, matching §24.5's "if the setting
	// cannot be read... treated as OFF"), a CONFIRMED negative, not an
	// uncertain one: skip, return nil (never retried; this package's own
	// Notifier implementations carry no logger of their own, matching
	// sentinelAutoFixNotifier's own identical "silent, idempotent no-op"
	// precedent for its own confirmed-negative case). A genuine OTHER
	// read error is uncertain -- propagated so the outbox retries.
	settings, err := n.repoSettings.Get(ctx, repoFullName)
	switch {
	case err == nil:
		if !settings.DescriptionAutofixEnabled {
			return nil
		}
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	default:
		return fmt.Errorf("outboxworker: descriptionAutofixNotifier: read repo settings: %w", err)
	}

	// Check 2: Narvi-authorship of the target PR, fresh. A confirmed
	// absence (pgx.ErrNoRows, folded into authored=false by
	// isPlatformAuthored) is a CONFIRMED negative -- skip, return nil. A
	// genuine read error surfaces as uncertain (err != nil,
	// authored=false) -- propagated so the outbox retries, never silently
	// treated as "not platform-authored".
	htmlURL := pullRequestHTMLURL(payload.Owner, payload.Repo, payload.PRNumber)
	authored, err := isPlatformAuthored(ctx, n.artifacts, htmlURL)
	if err != nil {
		return fmt.Errorf("outboxworker: descriptionAutofixNotifier: check platform authorship: %w", err)
	}
	if !authored {
		return nil
	}

	if n.sourceControl == nil {
		return errors.New("outboxworker: descriptionAutofixNotifier: no SourceControl configured")
	}

	getCtx, cancel := context.WithTimeout(ctx, n.timeouts.GitHubGetPRTimeout)
	originalBody, found, err := n.sourceControl.GetPRBody(getCtx, payload.Owner, payload.Repo, payload.PRNumber, n.outbound.BotToken())
	cancel()
	if err != nil {
		return fmt.Errorf("outboxworker: descriptionAutofixNotifier: get pr body: %w", err)
	}
	if !found {
		// The PR no longer exists/is no longer reachable -- a legitimate,
		// if uncommon, outcome (closed and deleted, or a permissions
		// change) never a reason to retry forever.
		return nil
	}

	newBody := reviewpost.RenderAutofixBody(originalBody, payload.ProposedBody)

	updateCtx, cancel := context.WithTimeout(ctx, n.timeouts.GitHubGetPRTimeout)
	err = n.sourceControl.UpdatePRBody(updateCtx, ports.UpdatePRBodySpec{
		Owner:  payload.Owner,
		Repo:   payload.Repo,
		Number: payload.PRNumber,
		Body:   newBody,
		Token:  n.outbound.BotToken(),
	})
	cancel()
	if err != nil {
		return fmt.Errorf("outboxworker: descriptionAutofixNotifier: update pr body: %w", err)
	}

	return nil
}
