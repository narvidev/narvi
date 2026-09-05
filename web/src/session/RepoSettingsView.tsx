// RepoSettingsView.tsx -- the per-repository settings screen (§21, §26.7,
// §26.8, §4.1.2). SettingsView.tsx's own doc comment draws the line: that
// screen owns ORG-level configuration (Environments, Secrets, Members,
// Prompt templates); this one owns every PER-REPO row of repo_settings.
//
// # Eight endpoints, not one -- this is the whole shape of this screen
//
// GET /api/repos/{owner}/{repo}/settings returns every field below in one
// response, but there are SEVEN separately-gated PUT routes behind it, not
// one combined write (httpapi/reposettings.go's own doc comments on each
// handler):
//   - PUT .../settings            -- blockOnHighRisk + sentinelAutofixEnabled,
//     admin-only (BOTH authz.ActionConfigureBlockOnHighRisk AND
//     authz.ActionToggleSentinelAutoFix).
//   - PUT .../auto-approval-settings -- maxAutoApproveFilesChanged +
//     sensitiveBlastRadiusTags, maintainer+ (authz.ActionConfigureAutoApprove,
//     §13.3 row 5) -- the ONE write on this screen a maintainer is
//     authorized for on their own.
//   - PUT .../auto-merge, .../auto-retrigger-review, .../description-autofix,
//     .../review-depth, .../review-cost-budget -- each its own single
//     boolean/config group, each admin-only, each its own action.
//
// A single Save-everything button would force every field through the
// COMBINED endpoint's admin-only gate, locking a maintainer out of the one
// row they legitimately hold (auto-approval eligibility) -- see
// UpdateRepoSettingsRequest's own doc comment (contracts/rest/v1/
// dtos.schema.json) for why that field was deliberately kept off the
// shared request. This screen instead groups the form into one card per
// gate, each with its OWN Save button and its OWN role check
// (isAdmin/isMaintainerPlus below, mirroring SecretsPanel.tsx's own
// canManageScope precedent): a control the caller's role cannot use is
// disabled with an explanation, never hidden and never wired to an
// endpoint that would 403 -- the secrets-panel defect two screens ago
// (a maintainer offered a live form whose backing endpoint was admin-only,
// its resulting 403 reported as a duplicate-name error) is exactly the
// shape this file avoids repeating: every Save button here calls the
// ONE endpoint that actually owns its own fields, and every ApiError this
// screen surfaces renders the server's own message (MutationError below),
// never a fixed string asserting a cause the code did not determine.
//
// Every PUT call sends the COMPLETE desired state for the fields it owns
// (never a partial patch, UpdateRepoSettingsRequest's own doc comment) --
// each card loads its own initial value from the already-fetched
// RepoSettings, lets the operator edit, and sends the whole group back.
//
// # Two fields this screen deliberately does not offer a form for
//
// sessionsEnabled is not a field of RepoSettings at all -- §32.6 makes
// enrollment seed-manifest-only (a repo cannot acquire the "known repo"
// signal this screen's own GET requires until AFTER it is already
// enrolled, so a REST toggle here could never reach an unenrolled repo in
// the first place). SessionEnrollmentNote below says so in operator
// language rather than silently having no mention of enrollment anywhere
// on this screen.
//
// The three rwx_preview_* columns (§4.1.2: a dispatch key, a build
// endpoint template, an organization slug) are real repo_settings columns
// but carry NO REST surface at all today -- RepoSettings' own generated
// shape has no property for any of the three, and no PUT route exists to
// write them (internal/app/seed/doc.go's own "integrations" writeup: the
// seed manifest is, as of today, the only writer). Inventing a form for
// them here would mean inventing the missing endpoint too, which is out
// of scope for this screen -- and rwx_preview_dispatch_key specifically is
// a credential (it authorizes dispatching a build on the preview
// provider), so it must never be added to a GET response at all, mirroring
// SandboxSecret/ProviderCredential's own write-only, value-never-returned
// contract. PreviewLinksNote below says plainly that this configuration
// cannot be read or changed here, rather than fabricating a control that
// cannot work.
//
// # Rendering safety
//
// repoFullName (echoes what the caller typed, but is also GitHub's own
// naming), reviewDepthDeepPaths (operator-entered glob patterns), and
// sensitiveBlastRadiusTags (a closed enum on the wire, rendered defensively
// through the same path anyway in case a future server version ever sends
// an unrecognized value) all render through the plain-text T component
// below (truncateForDisplay), mirroring MembersPanel.tsx's own precedent
// exactly -- never dangerouslySetInnerHTML. The two multi-line/free-text
// EDITING controls (the deep-paths textarea, the numeric inputs) bind
// directly to a controlled input's `value`, which React never interprets
// as markup either way; T is reserved for the READ-ONLY summary this file
// renders alongside them, exported as RepoSettingsSummary for direct
// render-safety testing (see __tests__/repoSettingsRendering.test.tsx).
import { formatUsd } from './money'
import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'

import type { RepoSettings, ShadowLedgerEntry } from '@narvi/contracts/rest-dtos'

import {
  getRepoSettings,
  getShadowLedger,
  postActivateShadowLedger,
  putAutoApprovalSettings,
  putAutoMergeToggle,
  putAutoRetriggerReviewToggle,
  putDescriptionAutofixToggle,
  putRepoSettings,
  putReviewCostBudget,
  putReviewDepthConfig,
} from '../api/endpoints'
import { ApiError } from '../api/http'
import { repoSettingsQueryKeys, shadowLedgerQueryKeys } from '../api/queryKeys'
import { meQueryOptions } from '../auth/session'
import {
  BLAST_RADIUS_TAGS,
  type BlastRadiusTag,
  blastRadiusTagLabel,
  formatDeepPathsForTextarea,
  parseDeepPathsInput,
  parseOptionalPositiveInt,
  parseOptionalPositiveUsd,
  reconcileServerBackedField,
  serverBackedValuesEqual,
  type ServerBackedFieldState,
  REVIEW_DEPTH_MODES,
  reviewDepthModeLabel,
} from './repoSettingsFormat'
import { truncateForDisplay } from './textSafety'
import { GatekeeperAffordance } from '../ext/GatekeeperAffordance'
import { SlotOutlet } from '../ext/slots'

const MAX_FIELD_CHARS = 500

function T({ text }: { text: string }) {
  return <>{truncateForDisplay(text, MAX_FIELD_CHARS)}</>
}

function isAdmin(role: string | undefined): boolean {
  return role === 'admin'
}

function isMaintainerPlus(role: string | undefined): boolean {
  return role === 'admin' || role === 'maintainer'
}

/** MutationError renders a failed save's own server message, never a fixed string asserting a cause the code did not determine -- ApiError.message is already the server's own `error` string when it sent one (api/http.ts's own doc comment). */
function MutationError({ error }: { error: unknown }) {
  return (
    <p className="sidebar-notice" role="alert">
      {error instanceof ApiError ? <T text={error.message} /> : 'Save failed. Try again.'}
    </p>
  )
}

/** RoleGateNote explains, in operator language, why a card's own controls are disabled -- the server enforces this regardless of what this screen renders; this text only ever explains it. */
function RoleGateNote({ requiredRole }: { requiredRole: 'admin' | 'maintainer+' }) {
  return (
    <p className="ph" style={{ marginTop: 4 }}>
      {requiredRole === 'admin' ? 'Admin only.' : 'Maintainer or admin.'} Your role can see these values but not change them here -- enforced by the server, not merely hidden on this screen.
    </p>
  )
}

/**
 * RepoSettingsSummary renders every currently-saved field as a compact,
 * read-only overview above the editable cards -- exported for direct
 * render-safety testing. repoFullName/reviewDepthDeepPaths (operator/
 * GitHub-authored free text) and sensitiveBlastRadiusTags (a closed enum,
 * rendered defensively through the same T path) are the fields this test
 * exercises with adversarial content.
 */
export function RepoSettingsSummary({ settings }: { settings: RepoSettings }) {
  const tags = settings.sensitiveBlastRadiusTags
  const deepPaths = settings.reviewDepthDeepPaths

  const row = (label: string, value: React.ReactNode) => (
    <div style={{ display: 'flex', justifyContent: 'space-between', gap: 12, padding: '4px 0', fontSize: 'var(--text-sm)', borderBottom: '1px solid var(--line)' }}>
      <span style={{ color: 'var(--faint)' }}>{label}</span>
      <span style={{ textAlign: 'right', overflowWrap: 'anywhere', maxWidth: '65%' }}>{value}</span>
    </div>
  )

  return (
    <div className="panel">
      <h4>
        <T text={settings.repoFullName} />
      </h4>
      <p className="ph">Currently saved configuration, before any edit below is made.</p>
      {row('Block merge on high risk', settings.blockOnHighRisk ? 'on' : 'off')}
      {row('Sentinel auto-fix', settings.sentinelAutofixEnabled ? 'on' : 'off')}
      {row('Auto-merge', settings.autoMergeEnabled ? 'on' : 'off')}
      {row('Auto-retrigger review', settings.autoRetriggerReviewEnabled ? 'on' : 'off')}
      {row('Description autofix', settings.descriptionAutofixEnabled ? 'on' : 'off')}
      {row('Max auto-approve files changed', settings.maxAutoApproveFilesChanged ?? 'engine default')}
      {row(
        'Sensitive blast-radius tags',
        tags && tags.length > 0 ? (
          tags.map((t, i) => (
            <span key={`${t}-${i}`}>
              {i > 0 && ', '}
              <T text={blastRadiusTagLabel(t)} />
            </span>
          ))
        ) : (
          'engine default'
        ),
      )}
      {row('Review depth mode', settings.reviewDepthMode ? <T text={reviewDepthModeLabel(settings.reviewDepthMode)} /> : 'engine default')}
      {row(
        'Review depth · additional deep paths',
        deepPaths && deepPaths.length > 0 ? (
          deepPaths.map((p, i) => (
            <span key={`${p}-${i}`}>
              {i > 0 && ', '}
              <T text={p} />
            </span>
          ))
        ) : (
          'none configured'
        ),
      )}
      {row('Review cost budget · light', settings.reviewCostBudgetLightUsd !== null ? `$${settings.reviewCostBudgetLightUsd}` : 'engine default ($0.50)')}
      {row('Review cost budget · deep', settings.reviewCostBudgetDeepUsd !== null ? `$${settings.reviewCostBudgetDeepUsd}` : 'engine default ($5.00)')}
    </div>
  )
}

function RiskPolicyCard({ owner, repo, settings, canEdit }: { owner: string; repo: string; settings: RepoSettings; canEdit: boolean }) {
  const queryClient = useQueryClient()
  const blockField = useServerBackedField(settings.blockOnHighRisk)
  const sentinelField = useServerBackedField(settings.sentinelAutofixEnabled)
  const repoFullName = `${owner}/${repo}`

  const mutation = useMutation({
    mutationFn: () => putRepoSettings(owner, repo, { blockOnHighRisk: blockField.value, sentinelAutofixEnabled: sentinelField.value }),
    onSuccess: (updated) => {
      blockField.set(updated.blockOnHighRisk)
      sentinelField.set(updated.sentinelAutofixEnabled)
      void queryClient.invalidateQueries({ queryKey: repoSettingsQueryKeys.detail(repoFullName) })
    },
  })

  return (
    <div className="panel">
      <h4>Merge &amp; risk policy</h4>
      <p className="ph">Both settings change what happens automatically on this repository's own pull requests, so both are admin-only and are saved together.</p>
      <label className="formrow" style={{ padding: '4px 0' }}>
        <input type="checkbox" checked={blockField.value} disabled={!canEdit || mutation.isPending} onChange={(e) => blockField.set(e.target.checked)} />
        Block merge when a review verdict comes back high risk
      </label>
      <label className="formrow" style={{ padding: '4px 0' }}>
        <input type="checkbox" checked={sentinelField.value} disabled={!canEdit || mutation.isPending} onChange={(e) => sentinelField.set(e.target.checked)} />
        Let coverage/doc-drift findings open their own auto-fix follow-up pull request
      </label>
      {(blockField.diverged || sentinelField.diverged) && <DivergedNotice />}
      {!canEdit && <RoleGateNote requiredRole="admin" />}
      {canEdit && (
        <div className="formrow">
          <button type="button" className="btn primary" disabled={mutation.isPending} onClick={() => mutation.mutate()}>
            {mutation.isPending ? 'Saving…' : 'Save'}
          </button>
        </div>
      )}
      {mutation.isError && <MutationError error={mutation.error} />}
    </div>
  )
}

function AutoMergeCard({ owner, repo, settings, canEdit }: { owner: string; repo: string; settings: RepoSettings; canEdit: boolean }) {
  const queryClient = useQueryClient()
  const enabledField = useServerBackedField(settings.autoMergeEnabled)
  const repoFullName = `${owner}/${repo}`

  const mutation = useMutation({
    mutationFn: () => putAutoMergeToggle(owner, repo, { enabled: enabledField.value }),
    onSuccess: (updated) => {
      enabledField.set(updated.autoMergeEnabled)
      void queryClient.invalidateQueries({ queryKey: repoSettingsQueryKeys.detail(repoFullName) })
    },
  })

  return (
    <div className="panel">
      <h4>Auto-merge</h4>
      <p className="ph">
        {settings.contradictionRateComputed && settings.contradictionRatePercent !== null
          ? `A human has since disagreed with the automated verdict on ${settings.contradictionRatePercent.toFixed(1)}% of this repository's own auto-approved pull requests, over ${settings.contradictionSampleSize} recorded outcome${settings.contradictionSampleSize === 1 ? '' : 's'} -- weigh this before arming the toggle below.`
          : // contradictionRateComputed false means only "no figure available".
            // It does not distinguish "nothing recorded yet" from a read that
            // failed, and the response carries nothing that would -- so this
            // says what is true (there is no figure) rather than asserting why.
            'No contradiction-rate figure is available for this repository, so there is nothing to weigh before arming this.'}
      </p>
      <label className="formrow" style={{ padding: '4px 0' }}>
        <input type="checkbox" checked={enabledField.value} disabled={!canEdit || mutation.isPending} onChange={(e) => enabledField.set(e.target.checked)} />
        Merge an auto-approved pull request unattended, instead of waiting for a 1-click human confirm
      </label>
      {enabledField.diverged && <DivergedNotice />}
      {!canEdit && <RoleGateNote requiredRole="admin" />}
      {canEdit && (
        <div className="formrow">
          <button type="button" className="btn primary" disabled={mutation.isPending} onClick={() => mutation.mutate()}>
            {mutation.isPending ? 'Saving…' : 'Save'}
          </button>
        </div>
      )}
      {mutation.isError && <MutationError error={mutation.error} />}
    </div>
  )
}

/** ShadowLedgerEntryRow renders one ShadowLedgerEntry as a table row -- operation/category/target are all attacker/customer-influenceable (a branch name, a file path, an HTTP path) so every one of them renders through T, never interpolated as markup. Exported for direct render-safety testing (see __tests__/shadowLedgerRendering.test.tsx) -- an entry with no sessionId renders no <Link> at all, the shape that test exercises (this codebase's own established "no precedent for unit-rendering a <Link>-bearing component outside the real app" convention, mirrored from decisionInboxRendering.test.tsx). */
export function ShadowLedgerEntryRow({ entry }: { entry: ShadowLedgerEntry }) {
  return (
    <tr>
      <td>
        <T text={entry.category} />
      </td>
      <td>
        <T text={entry.operation} />
      </td>
      <td>{entry.target ? <T text={entry.target} /> : <span style={{ color: 'var(--faint)' }}>—</span>}</td>
      <td>
        {entry.sessionId ? (
          <Link to="/session/$sessionId" params={{ sessionId: entry.sessionId }} style={{ textDecoration: 'none' }}>
            open session
          </Link>
        ) : (
          <span style={{ color: 'var(--faint)' }}>—</span>
        )}
      </td>
      <td style={{ color: 'var(--faint)', fontSize: 'var(--text-sm)' }}>{new Date(entry.createdAt).toLocaleString()}</td>
    </tr>
  )
}

/**
 * ShadowLedgerCard renders §30.6's own shadow-operator surface: the
 * per-repo ledger of suppressed platform-shadow effects, the §30.1
 * LLM-spend line, and the "Activate" graduation gesture (§30.8). The
 * WHOLE card -- not just Activate -- is admin-only: unlike every other
 * card on this screen, this one's own GET is itself gated
 * (authz.ActionViewShadowLedger), because this ledger can hold a
 * customer repository's own file content at rest, beyond what any other
 * card here exposes. This mirrors MembersPanel.tsx's own "query anyway,
 * render a forbidden note on a real 403" pattern -- the server is the
 * authority, this screen only explains the verdict -- rather than
 * RiskPolicyCard/AutoMergeCard's own "always readable, only editing is
 * gated" shape immediately above, which does not apply here since even
 * READING this card is restricted.
 */
function ShadowLedgerCard({ owner, repo, role }: { owner: string; repo: string; role: string | undefined }) {
  const queryClient = useQueryClient()
  const repoFullName = `${owner}/${repo}`

  const query = useQuery({
    queryKey: shadowLedgerQueryKeys.detail(repoFullName),
    queryFn: ({ signal }) => getShadowLedger(owner, repo, signal),
    retry: false,
  })

  const mutation = useMutation({
    mutationFn: () => postActivateShadowLedger(owner, repo),
    onSuccess: (updated) => {
      queryClient.setQueryData(shadowLedgerQueryKeys.detail(repoFullName), updated)
      void queryClient.invalidateQueries({ queryKey: repoSettingsQueryKeys.detail(repoFullName) })
    },
  })

  const forbidden = query.isError && query.error instanceof ApiError && query.error.status === 403

  return (
    <div className="panel">
      <h4>Shadow ledger</h4>
      <p className="ph">
        What platform shadow mode suppressed on this repository&rsquo;s behalf -- every customer-visible write it would have made, and what running the agent actually cost. Admin-only: this is the one surface in the product that can
        hold a customer repository&rsquo;s own file content at rest, in full.
      </p>
      {forbidden && <p className="notavailable">Shadow ledger is admin-only. Your role cannot view this card -- enforced server-side (authz.ActionViewShadowLedger), not merely hidden here.</p>}
      {!forbidden && query.isPending && <p className="rail-empty">Loading…</p>}
      {!forbidden && query.isError && <p className="rail-empty">Couldn&rsquo;t load the shadow ledger.</p>}
      {!forbidden && query.isSuccess && (
        <>
          <div style={{ display: 'flex', justifyContent: 'space-between', gap: 12, padding: '4px 0', fontSize: 'var(--text-sm)', borderBottom: '1px solid var(--line)' }}>
            <span style={{ color: 'var(--faint)' }}>Egress mode</span>
            <span>
              {query.data.liveEgressEnabled ? 'Live' : 'Shadow'}
              {query.data.liveEgressPromotedAt ? ` · promoted ${new Date(query.data.liveEgressPromotedAt).toLocaleString()}` : ''}
            </span>
          </div>
          <div style={{ display: 'flex', justifyContent: 'space-between', gap: 12, padding: '4px 0', fontSize: 'var(--text-sm)', borderBottom: '1px solid var(--line)' }}>
            {/*
              formatUsd, not toFixed(2). A per-turn agent cost is routinely
              a fraction of a cent, and two decimals render a real charge as
              "$0.00" -- indistinguishable from free, on the one screen whose
              job is telling an evaluator what their evaluation cost. That is
              the exact drift money.ts exists to prevent, and this card had
              quietly reintroduced a fifth local formatter.
            */}
            <span style={{ color: 'var(--faint)' }}>LLM spend (turns that reported a cost)</span>
            <span>{query.data.llmSpendComputed && query.data.llmSpendUsd !== null ? formatUsd(query.data.llmSpendUsd) : 'no figure available yet'}</span>
          </div>
          <div style={{ display: 'flex', justifyContent: 'space-between', gap: 12, padding: '4px 0', fontSize: 'var(--text-sm)', borderBottom: '1px solid var(--line)' }}>
            <span style={{ color: 'var(--faint)' }}>Suppressed effects on record</span>
            <span>{query.data.totalCount}</span>
          </div>

          {query.data.categories.length > 0 && (
            <div style={{ padding: '8px 0' }}>
              {query.data.categories.map((c) => (
                <div key={c.label} style={{ display: 'flex', justifyContent: 'space-between', gap: 12, fontSize: 'var(--text-sm)', padding: '2px 0' }}>
                  <span>
                    <T text={c.label} />
                  </span>
                  <span>{c.count}</span>
                </div>
              ))}
            </div>
          )}

          {query.data.entries.length > 0 && (
            <div style={{ overflowX: 'auto' }}>
              <table className="sectable">
                <thead>
                  <tr>
                    <th>Category</th>
                    <th>Operation</th>
                    <th>Target</th>
                    <th>Session</th>
                    <th>When</th>
                  </tr>
                </thead>
                <tbody>
                  {query.data.entries.map((e, i) => (
                    <ShadowLedgerEntryRow key={`${e.source}-${e.sessionId ?? 'none'}-${i}`} entry={e} />
                  ))}
                </tbody>
              </table>
            </div>
          )}

          {query.data.pendingShadowEraCount > 0 && (
            <p className="sidebar-notice">
              {query.data.pendingShadowEraCount} suppressed effect{query.data.pendingShadowEraCount === 1 ? '' : 's'} still resolving into this ledger -- Activate is blocked until they settle.
            </p>
          )}
          {query.data.liveEgressEnabled ? (
            <p className="ph" style={{ marginTop: 8 }}>
              This repository is already live.
            </p>
          ) : (
            <>
              {!isAdmin(role) && <RoleGateNote requiredRole="admin" />}
              {isAdmin(role) && (
                <div className="formrow" style={{ marginTop: 8 }}>
                  <button type="button" className="btn primary" disabled={mutation.isPending || query.data.pendingShadowEraCount > 0} onClick={() => mutation.mutate()}>
                    {mutation.isPending ? 'Activating…' : 'Activate'}
                  </button>
                </div>
              )}
            </>
          )}
          {mutation.isError && <MutationError error={mutation.error} />}
        </>
      )}
    </div>
  )
}

function AutoRetriggerReviewCard({ owner, repo, settings, canEdit }: { owner: string; repo: string; settings: RepoSettings; canEdit: boolean }) {
  const queryClient = useQueryClient()
  const enabledField = useServerBackedField(settings.autoRetriggerReviewEnabled)
  const repoFullName = `${owner}/${repo}`

  const mutation = useMutation({
    mutationFn: () => putAutoRetriggerReviewToggle(owner, repo, { enabled: enabledField.value }),
    onSuccess: (updated) => {
      enabledField.set(updated.autoRetriggerReviewEnabled)
      void queryClient.invalidateQueries({ queryKey: repoSettingsQueryKeys.detail(repoFullName) })
    },
  })

  return (
    <div className="panel">
      <h4>Auto-retrigger review on new commits</h4>
      <p className="ph">Once armed, a new commit pushed to a pull request with an existing review session enqueues a fresh review turn automatically, after a short quiet period, instead of waiting for a manual re-trigger. This never auto-approves anything on its own.</p>
      <label className="formrow" style={{ padding: '4px 0' }}>
        <input type="checkbox" checked={enabledField.value} disabled={!canEdit || mutation.isPending} onChange={(e) => enabledField.set(e.target.checked)} />
        Automatically re-review on new commits
      </label>
      {enabledField.diverged && <DivergedNotice />}
      {!canEdit && <RoleGateNote requiredRole="admin" />}
      {canEdit && (
        <div className="formrow">
          <button type="button" className="btn primary" disabled={mutation.isPending} onClick={() => mutation.mutate()}>
            {mutation.isPending ? 'Saving…' : 'Save'}
          </button>
        </div>
      )}
      {mutation.isError && <MutationError error={mutation.error} />}
    </div>
  )
}

function DescriptionAutofixCard({ owner, repo, settings, canEdit }: { owner: string; repo: string; settings: RepoSettings; canEdit: boolean }) {
  const queryClient = useQueryClient()
  const enabledField = useServerBackedField(settings.descriptionAutofixEnabled)
  const repoFullName = `${owner}/${repo}`

  const mutation = useMutation({
    mutationFn: () => putDescriptionAutofixToggle(owner, repo, { enabled: enabledField.value }),
    onSuccess: (updated) => {
      enabledField.set(updated.descriptionAutofixEnabled)
      void queryClient.invalidateQueries({ queryKey: repoSettingsQueryKeys.detail(repoFullName) })
    },
  })

  return (
    <div className="panel">
      <h4>Description autofix</h4>
      <p className="ph">
        Once armed, a Narvi-authored pull request whose description is flagged as drifted or misleading may have its body automatically rewritten (the original is preserved, collapsed). Only ever applies to Narvi-authored
        pull requests -- a human-authored one only ever gets a rendered suggestion, never a write.
      </p>
      <label className="formrow" style={{ padding: '4px 0' }}>
        <input type="checkbox" checked={enabledField.value} disabled={!canEdit || mutation.isPending} onChange={(e) => enabledField.set(e.target.checked)} />
        Automatically rewrite a drifted or misleading description
      </label>
      {enabledField.diverged && <DivergedNotice />}
      {!canEdit && <RoleGateNote requiredRole="admin" />}
      {canEdit && (
        <div className="formrow">
          <button type="button" className="btn primary" disabled={mutation.isPending} onClick={() => mutation.mutate()}>
            {mutation.isPending ? 'Saving…' : 'Save'}
          </button>
        </div>
      )}
      {mutation.isError && <MutationError error={mutation.error} />}
    </div>
  )
}

/**
 * useServerBackedField holds one editable field whose authoritative value
 * lives on the server.
 *
 * Every card on this screen used to seed its controls with plain
 * `useState(settings.X)`. That initial value is captured ONCE per mount, so
 * once the query refetched -- another admin changing the same repository,
 * another tab, or this screen's own invalidation after a sibling card saved --
 * the read-only summary at the top re-rendered to the new value while the
 * control below kept the old one. Pressing that card's Save then wrote the
 * stale value back, silently reverting the newer change, with the value being
 * overwritten visible a few lines above.
 *
 * The naive fix -- remount every card whenever the query returns -- trades that
 * for a worse one: it wipes whatever the operator is halfway through typing.
 * That direction currently works and must keep working (an unrelated card's
 * Save must never clear your in-progress edit).
 *
 * So: adopt a new server value only while the field is UNTOUCHED. Once the
 * operator edits it, their value stands, and `diverged` goes true so the card
 * can say that the stored value has moved underneath them. Saving makes the
 * server agree, which clears both flags -- comparison is by serialised value,
 * so arrays compare by content rather than identity.
 */
function useServerBackedField<T>(serverValue: T): { value: T; set: (next: T) => void; dirty: boolean; diverged: boolean } {
  const [state, setState] = useState<ServerBackedFieldState<T>>({ value: serverValue, server: serverValue, dirty: false })

  if (!serverBackedValuesEqual(state.server, serverValue)) {
    // Deriving state from changed props during render, not in an effect: this
    // is React's own prescribed shape for it and avoids rendering the stale
    // value for a frame first. The decision itself lives in the pure
    // reconcileServerBackedField (repoSettingsFormat.ts) so it can be tested
    // without a DOM -- see its own tests for the three cases that matter.
    setState((prev) => reconcileServerBackedField(prev, serverValue))
  }

  return {
    value: state.value,
    set: (next: T) => setState((prev) => ({ ...prev, value: next, dirty: true })),
    dirty: state.dirty,
    diverged: state.dirty && !serverBackedValuesEqual(state.value, state.server),
  }
}

/** DivergedNotice warns that the stored value moved while this card was being edited -- see useServerBackedField's own doc comment for why saving anyway would revert someone else's change. */
function DivergedNotice() {
  return <p className="sidebar-notice">This setting changed on the server while you were editing it. Saving now will replace the stored value with what is shown here.</p>
}

function AutoApprovalCard({ owner, repo, settings, canEdit }: { owner: string; repo: string; settings: RepoSettings; canEdit: boolean }) {
  const queryClient = useQueryClient()
  const maxFilesField = useServerBackedField(settings.maxAutoApproveFilesChanged !== null ? String(settings.maxAutoApproveFilesChanged) : '')
  const tagsField = useServerBackedField<BlastRadiusTag[]>(settings.sensitiveBlastRadiusTags ?? [])
  const repoFullName = `${owner}/${repo}`
  const parsedMaxFiles = parseOptionalPositiveInt(maxFilesField.value)

  function toggleTag(tag: BlastRadiusTag) {
    const next = tagsField.value.includes(tag) ? tagsField.value.filter((t) => t !== tag) : [...tagsField.value, tag]
    tagsField.set(next)
  }

  const mutation = useMutation({
    mutationFn: () => {
      if (parsedMaxFiles === 'invalid') return Promise.reject(new Error('invalid diff-size ceiling'))
      return putAutoApprovalSettings(owner, repo, { maxAutoApproveFilesChanged: parsedMaxFiles, sensitiveBlastRadiusTags: tagsField.value.length > 0 ? tagsField.value : null })
    },
    onSuccess: (updated) => {
      maxFilesField.set(updated.maxAutoApproveFilesChanged !== null ? String(updated.maxAutoApproveFilesChanged) : '')
      tagsField.set(updated.sensitiveBlastRadiusTags ?? [])
      void queryClient.invalidateQueries({ queryKey: repoSettingsQueryKeys.detail(repoFullName) })
    },
  })

  return (
    <div className="panel">
      <h4>Auto-approval eligibility</h4>
      <p className="ph">Two of the criteria the automated approval engine checks, both against the server's own view of a pull request's diff -- never a reviewing model's own self-report.</p>
      <div className="formrow">
        <label htmlFor="max-files-changed">Diff-size ceiling, in changed files</label>
        <input id="max-files-changed" placeholder="engine default" value={maxFilesField.value} disabled={!canEdit || mutation.isPending} onChange={(e) => maxFilesField.set(e.target.value)} style={{ width: 140 }} />
      </div>
      {parsedMaxFiles === 'invalid' && <p className="sidebar-notice">Must be a whole, non-negative number of files, or blank to use the engine's own default.</p>}
      {/*
        This copy used to say the selection was "layered on top of the engine's
        own default list". It is not: LoadEligibilityConfig does
        `cfg.SensitiveTags = tags` (internal/app/reviewverdict/config.go) -- a
        wholesale assignment over DefaultSensitiveTags(), which is migrations,
        auth and contracts. So saving a list REPLACES those three. An operator
        who checked only "secrets" believing the save was additive would have
        silently dropped all three, on the gate that decides an unattended
        approval and, with auto-merge armed on this same screen, an unattended
        merge. Saying which tags are in force, and that this list is the whole
        of it, is the only honest framing.
      */}
      <p className="ph" style={{ margin: '8px 0 4px' }}>
        Sensitive paths that always block auto-approval. This selection is the <b>complete</b> list once you save it &mdash; it replaces the engine&rsquo;s defaults rather than adding to them, so keep migrations, auth
        and contracts checked unless you intend to drop them. Leave every box unchecked to fall back to those three defaults.
      </p>
      <div className="formrow" style={{ flexWrap: 'wrap' }}>
        {BLAST_RADIUS_TAGS.map((tag) => (
          <label key={tag} style={{ display: 'flex', alignItems: 'center', gap: 4 }}>
            <input type="checkbox" checked={tagsField.value.includes(tag)} disabled={!canEdit || mutation.isPending} onChange={() => toggleTag(tag)} />
            {blastRadiusTagLabel(tag)}
          </label>
        ))}
      </div>
      {(maxFilesField.diverged || tagsField.diverged) && <DivergedNotice />}
      {!canEdit && <RoleGateNote requiredRole="maintainer+" />}
      {canEdit && (
        <div className="formrow">
          <button type="button" className="btn primary" disabled={mutation.isPending || parsedMaxFiles === 'invalid'} onClick={() => mutation.mutate()}>
            {mutation.isPending ? 'Saving…' : 'Save'}
          </button>
        </div>
      )}
      {mutation.isError && <MutationError error={mutation.error} />}
    </div>
  )
}

function ReviewDepthCard({ owner, repo, settings, canEdit }: { owner: string; repo: string; settings: RepoSettings; canEdit: boolean }) {
  const queryClient = useQueryClient()
  const modeField = useServerBackedField(settings.reviewDepthMode ?? '')
  const deepPathsField = useServerBackedField(formatDeepPathsForTextarea(settings.reviewDepthDeepPaths))
  const repoFullName = `${owner}/${repo}`

  const mutation = useMutation({
    mutationFn: () => putReviewDepthConfig(owner, repo, { mode: modeField.value.trim().length > 0 ? modeField.value : null, deepPaths: parseDeepPathsInput(deepPathsField.value) }),
    onSuccess: (updated) => {
      modeField.set(updated.reviewDepthMode ?? '')
      deepPathsField.set(formatDeepPathsForTextarea(updated.reviewDepthDeepPaths))
      void queryClient.invalidateQueries({ queryKey: repoSettingsQueryKeys.detail(repoFullName) })
    },
  })

  return (
    <div className="panel">
      <h4>Review depth routing</h4>
      <p className="ph">Which review model/effort tier -- and how much an automated review is allowed to cost -- a pull request on this repository gets routed to.</p>
      <div className="formrow">
        <select className="sel-select" value={modeField.value} disabled={!canEdit || mutation.isPending} onChange={(e) => modeField.set(e.target.value)}>
          <option value="">engine default (auto)</option>
          {REVIEW_DEPTH_MODES.map((m) => (
            <option key={m} value={m}>
              {reviewDepthModeLabel(m)}
            </option>
          ))}
        </select>
      </div>
      <label htmlFor="deep-paths" style={{ display: 'block', fontSize: 'var(--text-sm)', margin: '8px 0 4px', color: 'var(--faint)' }}>
        Additional deep-routing path patterns, one per line -- layered on top of the engine's own fixed sensitive-path set, never replacing it
      </label>
      <textarea
        id="deep-paths"
        className="btn"
        style={{ width: '100%', minHeight: 90, textAlign: 'left', fontFamily: 'var(--mono)', fontSize: 'var(--text-sm)', resize: 'vertical' }}
        readOnly={!canEdit || mutation.isPending}
        value={deepPathsField.value}
        onChange={(e) => deepPathsField.set(e.target.value)}
        placeholder="e.g. internal/payments/**"
      />
      {(modeField.diverged || deepPathsField.diverged) && <DivergedNotice />}
      {!canEdit && <RoleGateNote requiredRole="admin" />}
      {canEdit && (
        <div className="formrow">
          <button type="button" className="btn primary" disabled={mutation.isPending} onClick={() => mutation.mutate()}>
            {mutation.isPending ? 'Saving…' : 'Save'}
          </button>
        </div>
      )}
      {mutation.isError && <MutationError error={mutation.error} />}
    </div>
  )
}

function ReviewCostBudgetCard({ owner, repo, settings, canEdit }: { owner: string; repo: string; settings: RepoSettings; canEdit: boolean }) {
  const queryClient = useQueryClient()
  const lightField = useServerBackedField(settings.reviewCostBudgetLightUsd !== null ? String(settings.reviewCostBudgetLightUsd) : '')
  const deepField = useServerBackedField(settings.reviewCostBudgetDeepUsd !== null ? String(settings.reviewCostBudgetDeepUsd) : '')
  const repoFullName = `${owner}/${repo}`
  const light = parseOptionalPositiveUsd(lightField.value)
  const deep = parseOptionalPositiveUsd(deepField.value)
  const invalid = light === 'invalid' || deep === 'invalid'

  const mutation = useMutation({
    mutationFn: () => {
      if (light === 'invalid' || deep === 'invalid') return Promise.reject(new Error('invalid cost budget'))
      return putReviewCostBudget(owner, repo, { lightUsd: light, deepUsd: deep })
    },
    onSuccess: (updated) => {
      lightField.set(updated.reviewCostBudgetLightUsd !== null ? String(updated.reviewCostBudgetLightUsd) : '')
      deepField.set(updated.reviewCostBudgetDeepUsd !== null ? String(updated.reviewCostBudgetDeepUsd) : '')
      void queryClient.invalidateQueries({ queryKey: repoSettingsQueryKeys.detail(repoFullName) })
    },
  })

  return (
    <div className="panel">
      <h4>Review cost budget</h4>
      <p className="ph">A per-review spending ceiling, checked before each optional pass (fact-check, counter-review) -- never the primary pass a verdict depends on, and never the architecture recap, which always runs regardless of cost.</p>
      <div className="formrow">
        <label htmlFor="budget-light">Light path, in USD</label>
        <input id="budget-light" placeholder="engine default ($0.50)" value={lightField.value} disabled={!canEdit || mutation.isPending} onChange={(e) => lightField.set(e.target.value)} style={{ width: 160 }} />
        <label htmlFor="budget-deep">Deep path, in USD</label>
        <input id="budget-deep" placeholder="engine default ($5.00)" value={deepField.value} disabled={!canEdit || mutation.isPending} onChange={(e) => deepField.set(e.target.value)} style={{ width: 160 }} />
      </div>
      {invalid && <p className="sidebar-notice">Must be a positive dollar amount, or blank to use the engine's own default -- an explicit zero is rejected because it would silently mean unlimited spend instead.</p>}
      {(lightField.diverged || deepField.diverged) && <DivergedNotice />}
      {!canEdit && <RoleGateNote requiredRole="admin" />}
      {canEdit && (
        <div className="formrow">
          <button type="button" className="btn primary" disabled={mutation.isPending || invalid} onClick={() => mutation.mutate()}>
            {mutation.isPending ? 'Saving…' : 'Save'}
          </button>
        </div>
      )}
      {mutation.isError && <MutationError error={mutation.error} />}
    </div>
  )
}

/** SessionEnrollmentNote -- sessionsEnabled is deliberately absent from this screen (and from RepoSettings' own wire shape entirely): see this file's own top doc comment for the full "why" (§32.6). */
function SessionEnrollmentNote() {
  return (
    <div className="panel">
      <h4>Session enrollment</h4>
      <p className="notavailable">
        Whether this repository is enrolled to receive automated sessions at all is not shown or changed on this screen. Enrollment comes from the deployment&rsquo;s own configuration, and turning it off there is also
        how a repository is rolled back &mdash; that takes effect immediately, refusing new sessions, sandbox restarts and further turns alike. It is deliberately not editable from a running screen: this one can only
        ever be reached for a repository that has already sent at least one pull-request session, so a toggle here would always arrive too late to be the control that matters.
      </p>
    </div>
  )
}

/** PreviewLinksNote -- the three rwx_preview_* columns have no REST surface today: see this file's own top doc comment for the full "why", including why the dispatch key specifically must never appear in a response. */
function PreviewLinksNote() {
  return (
    <div className="panel">
      <h4>PR preview links</h4>
      <p className="notavailable">
        A pull request can get an automatic preview-app link at its newest commit, configured per repository (a dispatch key, a build endpoint template, and an organization slug). That configuration is not readable or
        writable from this screen yet -- there is no way here to see whether preview links are set up for this repository, or to change it.
      </p>
    </div>
  )
}

export function RepoSettingsView() {
  const meQuery = useQuery(meQueryOptions)
  const role = meQuery.data?.role
  const [owner, setOwner] = useState('')
  const [repo, setRepo] = useState('')
  const enabled = owner.trim().length > 0 && repo.trim().length > 0
  const repoFullName = `${owner}/${repo}`

  const query = useQuery({
    queryKey: repoSettingsQueryKeys.detail(repoFullName),
    queryFn: ({ signal }) => getRepoSettings(owner, repo, signal),
    enabled,
    retry: false,
  })

  const notFound = query.isError && query.error instanceof ApiError && query.error.status === 404
  const forbidden = query.isError && query.error instanceof ApiError && query.error.status === 403
  const otherError = query.isError && !notFound && !forbidden

  return (
    <div className="app one">
      <section className="main">
        <div className="anav">
          <span style={{ fontWeight: 600, fontSize: 13 }}>Repository settings</span>
        </div>

        <div className="abody" style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
          <div className="panel">
            <h4>Repository</h4>
            <p className="ph">Every setting below is scoped to one repository at a time. There is no repository picker -- type the owner and name exactly as they appear on GitHub.</p>
            <div className="formrow">
              <input placeholder="owner" value={owner} onChange={(e) => setOwner(e.target.value)} />
              <input placeholder="repo" value={repo} onChange={(e) => setRepo(e.target.value)} />
            </div>
          </div>

          {!enabled && <p style={{ color: 'var(--faint)', fontSize: 'var(--text-sm)' }}>Enter a repository above to load its settings.</p>}
          {enabled && query.isPending && <p className="rail-empty">Loading…</p>}
          {notFound && <p className="notavailable">Repo not known to this deployment yet -- it has never sent Narvi a pull-request session.</p>}
          {forbidden && <p className="notavailable">Your role cannot view settings for this repository.</p>}
          {otherError && <p className="rail-empty">Couldn't load repository settings.</p>}

          {enabled && query.isSuccess && (
            <div key={repoFullName} style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
              <RepoSettingsSummary settings={query.data} />
              <RiskPolicyCard owner={owner} repo={repo} settings={query.data} canEdit={isAdmin(role)} />
              <AutoMergeCard owner={owner} repo={repo} settings={query.data} canEdit={isAdmin(role)} />
              <ShadowLedgerCard owner={owner} repo={repo} role={role} />
              <SlotOutlet id="repo-settings.governance" fallback={<GatekeeperAffordance />} />
              <AutoRetriggerReviewCard owner={owner} repo={repo} settings={query.data} canEdit={isAdmin(role)} />
              <DescriptionAutofixCard owner={owner} repo={repo} settings={query.data} canEdit={isAdmin(role)} />
              <AutoApprovalCard owner={owner} repo={repo} settings={query.data} canEdit={isMaintainerPlus(role)} />
              <ReviewDepthCard owner={owner} repo={repo} settings={query.data} canEdit={isAdmin(role)} />
              <ReviewCostBudgetCard owner={owner} repo={repo} settings={query.data} canEdit={isAdmin(role)} />
              <SessionEnrollmentNote />
              <PreviewLinksNote />
            </div>
          )}
        </div>
      </section>
    </div>
  )
}
