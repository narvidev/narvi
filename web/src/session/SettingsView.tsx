// SettingsView.tsx -- §12.2 item 5's own Settings screen:
// Environments, Secrets, Members & access, Prompt templates, plus the
// §27 enterprise-glue surfaces the row's own text calls out (cloud
// identity & cluster bindings, per-Environment docker/egress display,
// OpenCode config editor) folded into the Environments panel, since each
// of those sub-resources is itself keyed by environments.id.
//
// # Where this screen draws the line against per-repository settings
//
// The per-repository surface (RepoSettingsView.tsx, routes/repo-settings.tsx)
// owns the four automation toggles (auto_merge_enabled/
// auto_retrigger_review_enabled/sentinel_autofix_enabled/
// description_autofix_enabled), block_on_high_risk, review-depth/
// cost-budget config, and sensitive_blast_radius_tags (§21, §26.7, §26.8,
// §4.1.2) -- none of that lives here, including the per-repo auto-merge/
// auto-retrigger-review toggles. This screen owns ORG-LEVEL configuration
// only: Environments, Secrets (repo/environment/global scoped, but the
// SCOPE PICKER lives here, not a per-repo settings page), Members &
// access, Prompt templates.
//
// # A nav tab for every mockup entry, real content behind only some
//
// mockups.html's own Settings nav draws 8 entries (General, Environments,
// Secrets, Members & access, Integrations, Models, Prompt templates,
// Image builds). Two of those have no backing surface: Models/Image
// builds name no §-cited data model anywhere in this screen's own scope
// (§14.1, §14.2, §21, §24, §27), and building one would be inventing
// scope, not implementing it. General hosts the platform-wide settings,
// platform_settings' one row (technical plan §40.2): today the autonomy
// freeze card (AutonomyFreezePanel.tsx). Integrations (Slack/Linear/GitHub
// ingress, ChatGPT-account linking, cloud-identity signing-key rotation --
// §12.5, §29.3/§29.9, §27.3/§27.8) DOES have a real surface, on its own
// screen (IntegrationsPanel.tsx), because those surfaces share one shape
// -- connect, verify, show liveness, disconnect -- that nothing else on
// this Settings screen claims. All 8 tabs are still drawn (visual parity
// with the mockup, which the UI phase's own screenshot-review exit
// criterion requires), but the 2 without a real surface render an
// explicit, honest "not built here" notice naming where that surface
// actually lives -- never a fabricated panel.
import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'

import { meQueryOptions } from '../auth/session'
import { AutonomyFreezePanel } from './AutonomyFreezePanel'
import { EnvironmentsPanel } from './EnvironmentsPanel'
import { IntegrationsPanel } from './IntegrationsPanel'
import { MembersPanel } from './MembersPanel'
import { PromptTemplatesPanel } from './PromptTemplatesPanel'
import { SecretsPanel } from './SecretsPanel'

type SettingsTab = 'general' | 'environments' | 'secrets' | 'members' | 'integrations' | 'models' | 'prompt-templates' | 'image-builds'

const TABS: { id: SettingsTab; label: string }[] = [
  { id: 'general', label: 'General' },
  { id: 'environments', label: 'Environments' },
  { id: 'secrets', label: 'Secrets' },
  { id: 'members', label: 'Members & access' },
  { id: 'integrations', label: 'Integrations' },
  { id: 'models', label: 'Models' },
  { id: 'prompt-templates', label: 'Prompt templates' },
  { id: 'image-builds', label: 'Image builds' },
]

/**
 * NotBuiltYet is what a tab with no real surface behind it renders.
 *
 * It used to say "Not part of this Step." and then cite a row of the
 * project's own build schedule by filename. Both halves were written for a
 * developer reading the repository, not for the operator who actually
 * reaches this screen: an operator has no Step, and cannot open a planning
 * document being cited at them. The honest thing to tell them is what is
 * missing and whether it is coming.
 */
function NotBuiltYet({ note }: { note: string }) {
  return (
    <div className="panel">
      <p className="notavailable">
        <b>Nothing to configure here yet.</b> {note}
      </p>
    </div>
  )
}

/** GeneralTab is Settings → General: the platform-wide settings, the autonomy freeze among them (§40.2), every role reading it and an administrator changing it. */
function GeneralTab() {
  const meQuery = useQuery(meQueryOptions)
  return <AutonomyFreezePanel role={meQuery.data?.role} />
}

export function SettingsView() {
  const [tab, setTab] = useState<SettingsTab>('environments')

  return (
    <div className="app one">
      <section className="main">
        <div className="settings">
          <nav className="setnav" aria-label="Settings sections">
            {TABS.map((t) => (
              <button key={t.id} type="button" className={tab === t.id ? 'on' : ''} onClick={() => setTab(t.id)} aria-current={tab === t.id ? 'page' : undefined}>
                {t.label}
              </button>
            ))}
          </nav>
          <div className="setbody">
            {tab === 'general' && <GeneralTab />}
            {tab === 'environments' && <EnvironmentsPanel />}
            {tab === 'secrets' && <SecretsPanel />}
            {tab === 'members' && <MembersPanel />}
            {tab === 'integrations' && <IntegrationsPanel />}
            {tab === 'models' && <NotBuiltYet note="There is nothing to manage: the model catalogue is read from the configured providers, and you choose a model per session in the composer." />}
            {tab === 'prompt-templates' && <PromptTemplatesPanel />}
            {tab === 'image-builds' && <NotBuiltYet note="Image-build history for each environment — fingerprint, duration and retry backoff — is not recorded anywhere yet, so there is nothing to show." />}
          </div>
        </div>
      </section>
    </div>
  )
}
