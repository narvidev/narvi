# GitHub guide

Everything GitHub sends Narvi arrives at one route:

```json narvi-command
{"name": "GitHub webhook", "route": "POST /webhooks/github"}
```

HMAC-verified (`X-Hub-Signature-256`) over the raw body before anything
else happens, and deduplicated by GitHub's own `X-GitHub-Delivery`
header — a redelivery is acknowledged without being reprocessed.

**Three `X-GitHub-Event` values are ever acted on at all, not two:**
`issue_comment` (a comment on an issue OR a pull request — GitHub's own
API models a PR as a kind of issue; only the PR case is acted on),
`pull_request_review_comment` (a comment on a specific diff line), and
`pull_request` — easy to miss because none of its own real effects look
like a "comment", but it is `parseMention`'s third dispatched case
(`payload.go`), not a gap. A `pull_request` event only ever does
something for three of its own `action` values, each a structurally
different capability from the other two and from an @-mention:

- `"labeled"`, naming the configured re-review label — routed through
  the SAME `parseMention`/coalescing path as an @-mention (see
  "What triggers a session or a turn" below): starts a brand-new review
  session, or joins/re-triggers the existing one for that PR.
- `"synchronize"` — GitHub's own name for "new commits landed on this
  PR's head", i.e. what an operator would call a push. **This is the
  automatic re-review lane (`handlePullRequestSynchronize`, §24.1), and
  it is NOT ignored**: if a review session already exists for this PR it
  re-arms that session's own re-review debounce timer, literally
  continuing it — this is what "pushes" would have meant in an earlier,
  incorrect draft of this sentence that listed them as ignored. A
  `synchronize` event on a PR with no existing review session is a
  no-op, same as everything below. It never starts a session by itself.
- `"closed"` — merge gating (`handlePullRequestClosed`, §17.4/§17.5): a
  structurally separate capability that never touches a review session
  at all, in either direction. Listed here only because it is the third
  real, production-wired thing this event type does — not a review
  trigger, so it doesn't belong in "what triggers a session or a turn"
  below.

Every other `pull_request` action (`opened`, `reopened`, `edited`, an
unconfigured label, ...), and every other event type entirely (stars,
issue events on a genuine non-PR issue, reviews, checks, ...) is
genuinely acknowledged and silently ignored.

## What triggers a session or a turn

**@-mention the configured bot handle** in a PR comment or a diff-line
review comment:

```json narvi-command
{"name": "@-mention the bot in a PR comment or review comment (always classified target=review, deterministically — not the LLM's own judgment call)", "classifier": {"surface": "github", "target": "review"}}
```

**Apply the configured re-review label** to a pull request:

```json narvi-command
{"name": "Apply the configured re-review label to re-trigger a review", "classifier": {"surface": "github", "target": "review"}}
```

Both resolve to the exact same "mention" shape internally and are handed
to the same per-PR coalescing claim — a first trigger on a PR (either
kind) creates a brand-new review session; every later trigger on the SAME
PR (either kind, from anyone) **joins** it instead of creating a
duplicate. Unlike Slack/Linear's shadow-only classification (see
[slack.md](slack.md)/[linear.md](linear.md)), GitHub's own `target` is
**deterministically forced to `"review"`** before the LLM classifier ever
runs — being reached at all structurally means a PR was mentioned or
labeled, so there is no ambiguity for a model to resolve. The mention
regex is case-insensitive and rejects both a longer handle that merely
starts with yours (`@narvi` never matches `@narvi-bot-2`) and a team
mention sharing your handle as a prefix (`@narvi` never matches
`@narvi/maintainers`).

**Negative — always queued, never dropped, never conflicted.** Unlike
every other surface, GitHub-triggered turns use
`CreateTurnPolicy.AlwaysQueue`: N concurrent mentions on a PR that
already has a review session all succeed, producing exactly one session
and N turns, dispatched one at a time — never a `409` ([web.md](web.md)),
never a silently dropped message ([slack.md](slack.md)/
[linear.md](linear.md)). There is no way to jump the queue.

A third, REST-only re-trigger surface exists — see [web.md](web.md)'s own
`POST /api/sessions/{sessionID}/review/retrigger` — targeting an
already-known session directly rather than routing through this per-PR
mention claim.

## Other real `pull_request` actions (not review triggers)

Two more `pull_request` actions are genuinely acted on but never start,
join, or otherwise touch a review session — the intro section above
already lists these by name; this section is where each is actually
explained:

- `"closed"` (Step 48, §17.4/§17.5) — merge gating: the bot's own
  post-review merge decision runs here, entirely separate from review
  session creation/continuation.
- `"synchronize"` (Step 65, §24.1) — re-arms an *existing* review
  session's re-review debounce timer when new commits land on the PR's
  head. Never starts a session from nothing; a `synchronize` event on a
  PR with no review session is a no-op.

Neither is documented as a `narvi-command` block above: both are
webhook-body dispatch on the SAME `POST /webhooks/github` route every
other command on this page already uses, gated on `action`, not on a
separate route or a §18.4 classifier outcome — nothing in this
mechanism's own vocabulary (`route`/`classifier`) represents "this
specific action value on this specific event type", so this is
unavoidably prose, same as every other negative on this page.

## Text commands dispatched before mention parsing

Two exact-prefix text commands are recognized on an `issue_comment` or
`pull_request_review_comment` comment **before `parseMention` ever
runs** — dispatch-before-router, exactly like the merge-gating/
re-review checks above. Neither starts, joins, or prompts a review
session; both require the commenter's GitHub account to be linked and
authorized, via the identical `domain/authz.Authorize` gate every other
state-changing actor command on this surface uses.

- `false positive: <reason>` (Step 63, §22.2) teaches a repo-scoped
  false-positive review pattern.
- `arch recap wrong: <reason>` (Step 69, §26.5) contests the deep
  review path's own architecture-recap digest section.

A comment matching neither prefix falls through to the ordinary mention
pipeline below, completely unaffected — see "No slash commands, no
free-text prompting" under "Honest negatives" for what that means for
everything else.

## The GitHub App: install it on every repository Narvi reviews

A review session is read-only. Its sandbox clones the pull request's
head with a read-only installation token of Narvi's GitHub App
(`NARVI_GITHUB_APP_ID`/`NARVI_GITHUB_APP_PRIVATE_KEY`, technical plan
§30.4), never with the bot token or a person's own GitHub token; it never
pushes and never opens a pull request, and its output reaches GitHub only
through the review it posts. So the App must be installed, with read-only
permissions (`contents: read`, `metadata: read`), on every repository
Narvi reviews.

A pull request from a fork is cloned from the fork, so it needs the App
installed on the fork owner's account too — a stated limit (§30.4) until a
review session reads its pull request from the base repository (a planned
Step). When the App is missing on the repository a review clones, the
clone fails and the session shows a warning naming the repository and
that the App must be installed on it with read access.

A follow-up that asks a review session to change code (a later mention, a
web prompt) is answered in the session, but nothing it changes is pushed:
the session shows a warning saying so.

## Which model credentials a review runs on

A review session runs on the deployment's provider credentials (the
global, repository and environment keys an admin configures), never on a
person's own linked account, such as a ChatGPT link — not even the account
of the member whose mention opened the review (technical plan §29.4). The
counter-reviewer's opposing model is chosen among the deployment's
credentials too. A review whose model only a person's own link could run
is not run: it fails before it starts, its `narvi/review` check closes as
not completed and says the review's model is available only through a
person's own provider link, and the session names the model, its provider
and `personal_link_only`. An admin adding a deployment credential for that
provider, or the review using another model, lets the next review run.

## Review verdicts and decision inbox

Posting a review verdict, applying a suggested fix, and rebutting a
finding are REST-only actions a human takes on the web surface after a
GitHub-triggered review posts its results — see [web.md](web.md)'s own
"Review" and "Decision inbox" sections. GitHub ingress itself never
accepts a verdict/rebuttal/apply-suggestion command directly; it only
starts and continues the review session those actions operate on.

## Honest negatives

**A comment on a plain issue (not a PR) is never acted on**, even if it
mentions the bot by handle — `§8.2` is PR review only, and
`parseIssueComment` explicitly rejects the non-PR case before a mention
is ever even checked for.

**An unlinked GitHub account gets a reply — an unenrolled repo gets
none.** These look similar (a mention that "does nothing new") but are
structurally different refusals:

- A commenter whose GitHub account isn't linked to a Narvi identity
  (`ErrActorNotAuthorized`, `actor.Valid == false`) gets **one reply**
  per PR within `platform.Timeouts.GitHubActorNoticeTTL` (24h) explaining
  how to link an account — never a second reply spamming the same PR
  within that window for a repeat mention from the same unlinked
  account.
- Under cohort rollout (`NARVI_ROLLOUT_MODE=cohort`, [see the production
  checklist](../PRODUCTION_CHECKLIST.md)), a repo that is not enrolled
  refuses **with total silence on GitHub specifically** — no comment, no
  label, no status check, nothing an outside observer could distinguish
  from the bot simply not existing. This is deliberately stricter than
  every other refusal on this surface: an unenrolled repo gives a
  commenter no action to take (linking an account wouldn't help), so
  there is nothing honest to say beyond silence, and posting anything
  would itself be the exact customer-repository-visible egress cohort
  rollout exists to prevent before a repo is actually onboarded. Slack
  and Linear post an honest denial notice for the equivalent refusal
  instead — see `docs/TECHNICAL_PLAN.md` §32.5's own full per-channel
  table for why GitHub alone is silent.

**No free-text prompting; two exact-prefix commands, not zero.** A
GitHub PR comment is a recognized @-mention (starts or joins a review),
one of the two exact-prefix capture commands above ("Text commands
dispatched before mention parsing"), or it is nothing at all — there is
no way to send GitHub-ingress Narvi an arbitrary build-mode prompt the
way a Slack/Linear message can, and neither capture command is a
"prompt" in that sense (neither ever creates, joins, or advances a
review session — see that section for what each actually does); every
GitHub-originated turn is still review-shaped.

**No cancellation.** Identical limitation to [linear.md](linear.md)'s own
— there is no session/turn stop command anywhere in this codebase yet;
an in-flight review turn runs to completion or `platform.Timeouts.
TurnDeadline` (60 min), whichever comes first.
