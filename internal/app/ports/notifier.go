package ports

import (
	"context"
	"encoding/json"
)

// NotificationKind discriminates which outbound channel a Notification
// targets -- a thin, faithful mirror of the outbox table's own `kind TEXT`
// column (migrations/000010_outbox.up.sql), which this package's own
// doc.go note ("kind is kept TEXT -- the set of notifier kinds grows with
// the PR-32/33/34/35 ingress work") already anticipated. Declared as its
// own named string type (rather than a bare string) so a caller/
// implementation can never pass an arbitrary, untyped string where a kind
// is expected, mirroring how sqlcgen.SessionSpawnSource is its own named
// type over the sessions.spawn_source column rather than a bare string.
type NotificationKind string

const (
	// NotificationKindSlack routes to internal/adapters/outbound/slackapi
	// (a chat.postMessage reply into the session's own originating
	// thread).
	NotificationKindSlack NotificationKind = "slack"
	// NotificationKindLinear routes to internal/adapters/outbound/
	// linearapi (an outcome-shaped AgentActivity posted to the session's
	// own originating agent session).
	NotificationKindLinear NotificationKind = "linear"
	// NotificationKindGitHub routes to internal/adapters/outbound/githubapi
	// (an issue comment posted to the session's own originating PR).
	NotificationKindGitHub NotificationKind = "github"

	// NotificationKindSlackPlanApproval is §8.1's ("plan mode,
	// cross-channel", §8.1/§13.3) own addition -- routes to internal/app/
	// outboxworker's own Slack plan-approval notifier (wrapping
	// internal/adapters/outbound/slackapi), which posts the REAL
	// interactive Block Kit approval-request message (chat.postMessage,
	// numbered steps + Approve/Request changes/Reject buttons) and, on
	// success, persists the message's own channel+ts back onto the plans
	// row (PlanStore.SetSlackMessageRef) -- distinct from
	// NotificationKindSlack (a plain-text chat.postMessage reply) because
	// this one carries plan-specific content/buttons and has a real,
	// durable side effect (the persisted message ref) beyond delivery
	// itself.
	NotificationKindSlackPlanApproval NotificationKind = "slack_plan_approval"

	// NotificationKindSlackPlanDecided is §8.1's own addition -- routes
	// to the SAME Slack plan-approval notifier, but calls chat.update
	// (never chat.postMessage) against an EXISTING message (channel+ts
	// already known, carried in the payload itself -- see slackapi.
	// PlanDecidedPayload) to reflect a plan's final approved/rejected
	// outcome, whichever entry point (Slack itself, Linear, or the web
	// REST endpoints) actually rendered the decision -- §16.1/§13.3's own
	// "first verdict wins + notify the other channels". An audit-fix batch
	// (completeness/observability, internal/app/sessionactor/planrecord.go)
	// reuses this SAME kind for a third case: a plan SUPERSEDED by a newer
	// plan-mode turn's completion (rather than decided) -- chat.update's own
	// "omit blocks to strip them" behavior is exactly right there too, to
	// clear the now-stale Approve/Reject buttons.
	NotificationKindSlackPlanDecided NotificationKind = "slack_plan_decided"

	// NotificationKindLinearProgress is an audit-fix batch's own addition
	// (finding M16, "completeness", internal/adapters/outbound/linearapi/
	// doc.go): that package's own top comment explicitly deferred a
	// richer, asynchronous, retried "progressive" AgentActivity update
	// (the "progressive" half of §8.10) to "a future Step" once the real
	// Notifier/outbox consumer existed -- §5.1 shipped that consumer,
	// but nothing ever enqueued a mid-turn update through it until now.
	// Routes to the SAME internal/app/outboxworker linearNotifier already
	// registered under NotificationKindLinear (reused, not a second
	// client/credential-lookup path), but posts a "thought"-shaped
	// AgentActivity (linearapi.Client.CreateThoughtActivity) instead of an
	// outcome-shaped response/error one. A NEW, distinct Kind -- rather
	// than a payload-level discriminator squeezed onto
	// NotificationKindLinear -- mirroring NotificationKindSlackPlanApproval/
	// NotificationKindSlackPlanDecided's own established precedent just
	// above for "more than one shape needs to travel under one channel":
	// each of those is its own Kind with its own distinct payload struct,
	// even though both are delivered by the SAME planSlackNotifier
	// instance; linearNotifier's own Deliver now dispatches on
	// notification.Kind the exact same way planSlackNotifier's already
	// does.
	NotificationKindLinearProgress NotificationKind = "linear_progress"

	// NotificationKindGitHubVerdict is §8.2's own addition ("server-side
	// verdict", §8.2/§5.2/§21.2): routes to internal/adapters/outbound/
	// githubapi's own verdict notifier, which submits a FORMAL pull request
	// review (the "formal-review gate") or, deliberately, never a plain
	// PostIssueComment call, plus syncs the review:*-risk label vocabulary
	// (internal/domain/reviewpost.ComputeLabelSync) -- both in ONE Deliver
	// call, since a caller only ever posts a verdict as one coherent unit
	// (text + labels together), never independently. Distinct from
	// NotificationKindGitHub (the generic outcome-text comment every OTHER
	// github-origin turn used to post, now BLOCKED for a review session,
	// internal/app/sessionactor/outboxenqueue.go) -- see that Kind's own
	// doc comment for why a review session never uses it any more.
	NotificationKindGitHubVerdict NotificationKind = "github_verdict"

	// NotificationKindSentinelAutoFix is §8.2's own addition
	// ("sentinels + suggestions", §17.2): routes to internal/app/
	// outboxworker's own sentinel-auto-fix notifier, which atomically
	// claims its sentinel_fixes row and spawns a child session
	// (httpapi.CreateSessionOnTx, sentinelautofix.go) pre-loaded with the
	// origin diff and the specific coverage/doc-drift finding(s) that
	// triggered it -- see SentinelAutoFixPayload's own doc comment for
	// the payload shape, and reviewverdict.go for the ONE place this Kind
	// is ever enqueued (inside the same transaction as the triggering
	// verdict's own findings-upsert write).
	NotificationKindSentinelAutoFix NotificationKind = "sentinel_auto_fix"

	// NotificationKindHandoffSentinel is §14.4's own addition
	// ("handoff-readiness sentinel", §14.4): routes to internal/adapters/
	// outbound/githubapi's own handoff notifier, which posts the
	// sentinel's already-rendered summary as a plain issue comment
	// (PostIssueComment) AND syncs the fixed "handoff" label (AddLabels)
	// -- both in ONE Deliver call, mirroring NotificationKindGitHubVerdict's
	// own identical "text + labels together, one outbox row" precedent.
	// Distinct from that Kind: this sentinel never computes or posts a
	// risk verdict at all (§14.4: "alongside or INSTEAD OF a normal risk
	// verdict") -- see internal/app/sessionactor/handoffsentinel.go for
	// the ONE place this Kind is ever enqueued, inside the same
	// transaction as this PR's own idempotency claim
	// (internal/adapters/outbound/postgres.HandoffSentinelStore.Claim).
	NotificationKindHandoffSentinel NotificationKind = "handoff_sentinel"

	// NotificationKindReleaseManifest is §15's own addition ("release
	// PR review", §15.2): routes to internal/adapters/outbound/githubapi's
	// own release-manifest notifier, which posts the manifest check's
	// already-rendered comment (internal/domain/reviewpost.
	// RenderManifestComment) as a plain issue comment (PostIssueComment)
	// -- NEVER a formal review (the manifest check is "an audit, not a
	// risk verdict", §15.2's own words: it has no RiskLevel/Shippable of
	// its own to gate a formal-review event on, unlike
	// NotificationKindGitHubVerdict). Distinct from that Kind for exactly
	// the same reason NotificationKindHandoffSentinel already is (that
	// Kind's own doc comment): a mechanical, always-runs check that never
	// computes or posts a risk verdict. See internal/app/releasereview
	// for the ONE place this Kind is ever enqueued.
	NotificationKindReleaseManifest NotificationKind = "release_manifest"

	// NotificationKindSlackWorkflowDecision is §25.9's own addition
	// ("workflow HITL gate + circuit breaker", §25.9): a THIRD extension of
	// planSlackNotifier's own wrapper-per-provider pattern
	// (planslacknotifier.go, already extended twice: approval+decided) --
	// posts a single, already-rendered, human-readable notice (internal/app/
	// workflowengine's own enqueueWorkflowNotice, notify.go) for either of
	// TWO distinct §25.9 events: a workflow step reaching awaiting_decision
	// (HITLAfter -- "please decide: approve/reject/revise") or a run
	// escalating to needs_review (the circuit breaker tripping, or an
	// unrouted needs_fix/blocked outcome -- "this run now needs your
	// attention"), never repeated for the SAME escalation (§24.6's own
	// "never repeated" exemption, mirrored via workflow_runs.
	// needs_review_notified_at). Deliberately distinct from the plain
	// NotificationKindSlack (turn-completion outcomes only, enqueued
	// exclusively by internal/app/sessionactor's own outboxenqueue.go) even
	// though the wire payload shape is identical (slackapi.Payload, reused
	// verbatim, no new payload type) -- the SAME "a distinct Kind marks a
	// distinct triggering semantic even for structurally identical
	// delivery" precedent NotificationKindHandoffSentinel/
	// NotificationKindReleaseManifest already established alongside
	// NotificationKindGitHub.
	NotificationKindSlackWorkflowDecision NotificationKind = "slack_workflow_decision"
	// NotificationKindLinearWorkflowDecision is this SAME §25.9 addition's
	// Linear twin -- a THIRD extension of linearNotifier's own
	// wrapper-per-provider pattern (linearnotifier.go, already extended
	// twice: outcome+progress), reusing the plain linearapi.Payload shape
	// (Success always true) verbatim. See NotificationKindSlackWorkflowDecision's
	// own doc comment above for the full "why a distinct Kind" reasoning.
	NotificationKindLinearWorkflowDecision NotificationKind = "linear_workflow_decision"
	// NotificationKindGitHubWorkflowDecision is this SAME §25.9 addition's
	// GitHub twin -- reuses the EXISTING githubapi.BotNotifier instance
	// (cmd/control-plane/main.go), registered under a second Kind:
	// BotNotifier.Deliver never inspects notification.Kind at all, so this
	// needs no new githubapi code whatsoever, the most literal possible
	// "reuse the exact delivery mechanism, don't build a new delivery
	// path". See NotificationKindSlackWorkflowDecision's own doc comment
	// above for the full "why a distinct Kind" reasoning.
	NotificationKindGitHubWorkflowDecision NotificationKind = "github_workflow_decision"

	// NotificationKindRWXPreviewDispatch is §4.1's ("RWX provider +
	// previews", §4.1.2 point 2) own addition: routes to
	// internal/adapters/outbound/rwx's own ports.Notifier implementation,
	// which POSTs to RWX's real, documented Dispatches API
	// (https://cloud.rwx.com/mint/api/runs/dispatches) to trigger a
	// preview-app build at the pushed sha. Enqueued exactly once per
	// pushed repo whose per-repo preview setting is present
	// (internal/app/sessionactor/pushpr.go's own createPRBestEffort — the
	// ONE enqueue point, §4.1.2 point 1), in the SAME fresh transaction as
	// the companion NotificationKindGitHubPreviewLink row and the
	// session's first real "preview"-typed artifact row. Delivery is the
	// fast dispatch POST only — it never waits for RWX's own build to
	// finish (§4.1.2 point 2: "Delivery is the fast dispatch POST only; it
	// never waits for the build").
	NotificationKindRWXPreviewDispatch NotificationKind = "rwx_preview_dispatch"

	// NotificationKindGitHubPreviewLink is §4.1's own companion
	// addition (§4.1.2 point 3): routes to internal/adapters/outbound/
	// githubapi's own small notifier, which posts a `narvi/preview` commit
	// status (via a NEW CreateCommitStatus adapter capability — POST
	// /repos/{owner}/{repo}/statuses/{sha}) carrying the deterministic
	// "friendly" RWX preview URL. A commit status, not an issue comment or
	// a GitHub Deployment (§4.1.2 point 3's own reasoning: redelivery of
	// the same (context, sha) converges instead of duplicating, and a
	// preview that can die with RWX's own idle reaper should never
	// masquerade as a deployment environment). Enqueued alongside
	// NotificationKindRWXPreviewDispatch above, never independently.
	NotificationKindGitHubPreviewLink NotificationKind = "github_preview_link"

	// NotificationKindBlobDelete is §8.6's own outbox kind (§28.4):
	// enqueued whenever confirm's Stat-based verification fails (pending
	// -> failed) or the abandonment sweep reaps a stale pending row --
	// either way the object may half-exist in storage, and an external
	// delete is an outbound side effect that must survive a crash between
	// the status write and the delete (§5.1), the same reasoning behind
	// every other outbox kind. Routes to internal/adapters/outbound/
	// objstore's own small notifier, which calls BlobStore.Delete --
	// itself idempotent (deleting an already-absent key succeeds), so a
	// redelivered attempt after a transient failure is always safe to
	// retry with no dedup logic of its own. Payload is
	// {"key": "<the blob's own ports.BlobKey, as a plain string>"}.
	NotificationKindBlobDelete NotificationKind = "blob_delete"

	// NotificationKindSlackDigest is §21's own outbox kind (§21.3):
	// enqueued by internal/app/digest.Pump once per (date, channel), only
	// after that channel's own digest_send_state row has already been
	// atomically claimed (SELECT ... FOR UPDATE SKIP LOCKED) -- routes to
	// internal/app/outboxworker's own digest Slack notifier, a plain
	// chat.postMessage of internal/domain/digest.Render's own
	// deterministic, pre-rendered text (never a Block Kit interactive
	// message -- a digest has no buttons to click). Payload is
	// slackapi.DigestPayload{ChannelID, Text}.
	NotificationKindSlackDigest NotificationKind = "slack_digest"

	// NotificationKindGitHubDescriptionAutofix is §26.2's own addition
	// ("review digest: description adequacy + graduated remediation",
	// §26.2): routes to internal/app/outboxworker's own description-
	// autofix notifier, which re-verifies BOTH the Narvi-authorship of
	// the target PR (an artifacts row of type 'pr' exists for its own
	// URL, mirroring internal/app/decisioninbox's own isPlatformAuthored
	// precedent) AND this repo's own descriptionAutofix flag, fresh, at
	// delivery time -- never trusted from DescriptionAutofixPayload
	// itself (§5.2: "never prompt-only, never trusting the agent to
	// self-enforce") -- before composing (internal/domain/reviewpost.
	// RenderAutofixBody) and writing (ports.SourceControl.UpdatePRBody) a
	// new PR body, original preserved in a collapsed block. Either check
	// failing is a silent, logged no-op, never an error: §26.2 draws no
	// distinction between "not eligible" and "eligible but the write
	// itself failed" in terms of retry policy, but a CONFIRMED-negative
	// check (a real repo_settings row with the flag off, or a confirmed
	// absence of a platform-authored artifact) must never be retried
	// forever the way a genuine transient store error should be -- see
	// that notifier's own Deliver doc comment for the exact distinction.
	// See reviewverdict.go for the ONE place this Kind is ever enqueued
	// (inside the same transaction as the triggering verdict's own
	// review_verdicts insert), gated ONLY on Digest.ProposedBody being
	// non-blank -- enqueuing is never itself a decision that a write will
	// happen (DescriptionAutofixPayload's own doc comment).
	NotificationKindGitHubDescriptionAutofix NotificationKind = "github_description_autofix"

	// NotificationKindLinearDigest is §21's own Linear sibling of
	// NotificationKindSlackDigest above. UNLIKE every other Linear
	// notification this codebase sends, this one has no existing
	// AgentSession to post an AgentActivity into -- a digest is not a
	// reply to any one agent invocation, and linearapi.Client exposes
	// only AgentSession-scoped activity methods (CreateThoughtActivity/
	// CreateResponseActivity/CreateErrorActivity), never an organization-
	// level "post somewhere" capability. This kind is DELIBERATELY wired
	// (channel discovery + claim-before-act both cover Linear
	// organizations exactly like Slack channels) so the at-most-one-
	// send-per-day guarantee is proven for both providers, but its own
	// notifier (internal/app/outboxworker's own digestLinearNotifier)
	// always returns a clear, typed error -- surfacing through this
	// codebase's OWN existing outbox retry-then-dead-letter path
	// (§5.1) into the decision inbox's own admin-only needs_attention
	// row, never a silent no-op or a fabricated success. Real delivery
	// needs a genuinely new Linear API capability this Step's own brief
	// does not authorize inventing -- named here, not silently left a
	// gap; see internal/app/outboxworker/digestlinearnotifier.go's own
	// doc comment for the full "why".
	NotificationKindLinearDigest NotificationKind = "linear_digest"

	// NotificationKindGitHubReviewCheck is this codebase's own addition
	// for the review's GitHub-native result surface (§8.2/§21.1/§21.1b): routes
	// to internal/app/outboxworker's own review-check notifier, which
	// publishes/updates the narvi/review check run for a pull request --
	// see ReviewCheckPayload's own doc comment for the wire shape, and
	// internal/domain/reviewcheck for the pure state mapping and
	// supersession rule that notifier's own Deliver applies. Enqueued
	// from three places, one per internal/domain/reviewcheck.Phase this
	// Step actually wires a trigger for: internal/adapters/inbound/
	// github's own coalesce.go (PhaseQueued, "a pull request enters
	// scope"), internal/app/sessionactor's own dispatch.go
	// (PhaseRunning, a review turn dispatched) and reviewverdict.go/
	// outboxenqueue.go (PhaseTerminalAssessed/PhaseTerminalNotAssessed,
	// a review turn concluding with or without a posted verdict).
	// PhaseStale is a real, mapped, tested state (internal/domain/
	// reviewcheck's own doc comment on PhaseStale) with no live trigger
	// wired by this Step -- named as an explicit follow-up, never
	// silently assumed built.
	NotificationKindGitHubReviewCheck NotificationKind = "github_review_check"
)

// Notification is what Notifier.Deliver needs to deliver ONE outbox entry
// -- Kind/Payload are a thin, faithful mirror of the outbox table's own
// `kind TEXT`/`payload JSONB` columns (migrations/000010_outbox.up.sql):
// Payload is deliberately kept as opaque json.RawMessage here, never a
// union of every possible provider-specific Go struct, so this port
// itself never needs to change shape as a fourth/fifth notifier kind is
// added later -- each concrete Notifier implementation (internal/adapters/
// outbound/{slackapi,linearapi,githubapi}) owns unmarshaling Payload into
// its OWN kind-specific shape, and is only ever asked to Deliver a
// Notification whose Kind it already knows how to handle (the delivery
// worker, internal/app/outboxworker, is what routes by Kind -- see that
// package's own doc comment for the kind->Notifier map this implies).
type Notification struct {
	Kind    NotificationKind
	Payload json.RawMessage

	// SuppressedInShadow is the row's ENQUEUE-TIME egress stamp
	// (TECHNICAL_PLAN.md §30.8's epoch discipline: "suppress if the stamp
	// OR the current flag says shadow -- monotone toward suppression, in
	// both directions").
	//
	// It is carried here because the delivery-time half of that rule is
	// applied by the outbox builder for SUPPRESS-classified kinds only. A
	// PASS-THROUGH kind reaches Deliver unconditionally, so a notifier
	// that must honour the stamp could otherwise read nothing but the
	// CURRENT flag -- and a row born shadow, delivered after the repo was
	// promoted, would then take the live path. §30.8 is explicit that a
	// born-shadow row "can only end in the ledger", whatever
	// repo_settings says by the time it is delivered.
	//
	// A notifier that ignores this field is not thereby safe; it is
	// simply one whose kind has no shadow-suppressible effect.
	SuppressedInShadow bool
}

// Notifier is the port that delivers ONE outbound notification to an
// external channel (§4.3: "Notifier (Slack/Linear/GitHub comment
// delivery -- consumed via outbox only)"). Three real implementations
// exist, one per NotificationKind above (internal/adapters/outbound/
// slackapi, linearapi, githubapi) -- this interface exists specifically so
// each can satisfy the SAME contract (CLAUDE.md: "don't couple a port to a
// single adapter... interfaces in /internal/app/ports must hold for more
// than one implementation"), even though (unlike SourceControl/
// SandboxProvider, where two adapters implement the exact same operation
// against different providers) each of THESE three implementations is
// only ever asked to Deliver its own matching Kind in practice -- the
// delivery worker is what enforces that routing, not this interface
// itself, which is why Deliver takes no Kind-specific method of its own
// and instead threads Kind through the single Notification value: a
// uniform shape every implementation can be exercised through
// identically in tests (httptest.Server-based, mirroring every other
// outbound adapter's own existing test convention) and wired into the
// SAME kind->Notifier routing map in production (cmd/control-plane/
// main.go), without this port needing to know how many concrete kinds
// exist.
//
// Deliver is called EXACTLY ONCE per delivery attempt, always OUTSIDE any
// Postgres transaction (§5.1's own "a retry worker delivers with
// exponential backoff" -- a real outbound network call must never hold a
// transaction open, the same discipline every other real network call in
// this codebase already follows, e.g. SourceControl.CreatePR/
// SandboxProvider.CreateSandbox). A non-nil error means this attempt
// failed; the caller (internal/app/outboxworker) is what decides whether
// to retry (with backoff) or dead-letter -- this port never classifies its
// own errors as transient/permanent (mirroring SourceControl's own
// identical "no typed classification" precedent), since no caller needs
// one: outboxworker's own domain/outbox.EvaluateBackoff decision is driven
// purely by attempt count, not by inspecting the error's own shape.
type Notifier interface {
	Deliver(ctx context.Context, n Notification) error
}
