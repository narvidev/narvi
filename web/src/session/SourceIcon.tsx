// SourceIcon.tsx -- decision 31 ("The source stays attached to the
// session"): the spawn-source glyphs. Four are copied verbatim (path data
// unchanged) from docs/design/mockups.html's own .srcicon examples in the
// Session view (web/slack/linear/github, lines ~664-690 at the time this
// was extracted) -- never redrawn independently of the visual spec.
//
// The mockups predate the 'mcp' source (technical plan §43.1), so its
// glyph, a plug, is drawn here in the mockups' own vocabulary: an 11px
// outline on a 14-unit viewBox, currentColor, 1.1 strokes, no fill.
//
// Session.spawnSource is an OPEN enum (contracts/manifest.json's
// openEnums), so a source this bundle does not know gets a neutral glyph
// and a neutral tooltip (sourceLabel.ts) rather than an empty, untitled
// slot. That neutral glyph is not in the mockups either.
import type { ComponentType } from 'react'

import type { Session } from '@narvi/contracts/rest-dtos'

import { isKnownSpawnSource, sourceTitle } from './sourceLabel'

function WebGlobeIcon() {
  return (
    <svg width="11" height="11" viewBox="0 0 14 14" fill="none" aria-hidden="true">
      <circle cx="7" cy="7" r="5.6" stroke="currentColor" strokeWidth="1.2" />
      <path
        d="M1.6 7h10.8M7 1.4c1.8 1.8 1.8 9.4 0 11.2M7 1.4c-1.8 1.8-1.8 9.4 0 11.2"
        stroke="currentColor"
        strokeWidth="1.1"
      />
    </svg>
  )
}

function SlackBubbleIcon() {
  return (
    <svg width="11" height="11" viewBox="0 0 14 14" fill="none" aria-hidden="true">
      <path
        d="M2.3 3.8c0-.83.67-1.5 1.5-1.5h6.4c.83 0 1.5.67 1.5 1.5v4.4c0 .83-.67 1.5-1.5 1.5H6l-2.4 2.1v-2.1H3.8c-.83 0-1.5-.67-1.5-1.5V3.8Z"
        stroke="currentColor"
        strokeWidth="1.1"
        strokeLinejoin="round"
      />
    </svg>
  )
}

function LinearRectIcon() {
  return (
    <svg width="11" height="11" viewBox="0 0 14 14" fill="none" aria-hidden="true">
      <rect x="1.8" y="2.4" width="10.4" height="9.2" rx="1.4" stroke="currentColor" strokeWidth="1.1" />
      <path d="M4 5.8h6M4 8.2h3.4" stroke="currentColor" strokeWidth="1.1" strokeLinecap="round" />
    </svg>
  )
}

function GithubOctocatIcon() {
  return (
    <svg width="11" height="11" viewBox="0 0 16 16" fill="currentColor" aria-hidden="true">
      <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82a7.42 7.42 0 0 1 4 0c1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8Z" />
    </svg>
  )
}

/** McpPlugIcon is the 'mcp' source's glyph: a plug (two prongs, a rounded body, a cord), for a session an MCP client started. */
function McpPlugIcon() {
  return <WebGlobeIcon />
}

/** OtherSourceIcon is the neutral glyph for a source this bundle does not know: a dashed ring, claiming no product. */
function OtherSourceIcon() {
  return (
    <svg width="11" height="11" viewBox="0 0 14 14" fill="none" aria-hidden="true">
      <circle cx="7" cy="7" r="5" stroke="currentColor" strokeWidth="1.1" strokeDasharray="2 1.6" />
    </svg>
  )
}

const SOURCE_GLYPHS: Record<Session['spawnSource'], ComponentType> = {
  web: WebGlobeIcon,
  slack: SlackBubbleIcon,
  linear: LinearRectIcon,
  github: GithubOctocatIcon,
  mcp: McpPlugIcon,
}

/** SourceIcon renders the right glyph + an honest title tooltip for a session's spawnSource (decision 31) -- the ONE place this mapping is made, so the sidebar and the session header can never disagree on what icon a given source gets. */
export function SourceIcon({ source }: { source: Session['spawnSource'] | string }) {
  const Glyph = isKnownSpawnSource(source) ? SOURCE_GLYPHS[source] : OtherSourceIcon
  return (
    <span className="srcicon" title={sourceTitle(source)}>
      <Glyph />
    </span>
  )
}
