// sourceRendering.test.tsx -- Session.spawnSource is an OPEN enum
// (contracts/manifest.json's openEnums), proven at the render boundary:
// a source this bundle does not know still draws a glyph with a tooltip
// in the source icon (sidebar, sessions list, header) and a visible tag in
// the session header, instead of an empty, untitled slot. Rendered with
// react-dom/server's renderToStaticMarkup, like timelineRendering.test.tsx.
import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'

import type { Session } from '@narvi/contracts/rest-dtos'

import { buildCostRollup } from '../costRollup'
import { SessionHeader } from '../SessionHeader'
import { SourceIcon } from '../SourceIcon'
import { buildTimelineModel } from '../timelineModel'

// A value the generated union does not name, as a newer server could send it.
const UNKNOWN_SOURCE = 'some_future_source' as Session['spawnSource']

function session(spawnSource: Session['spawnSource']): Session {
  return {
    id: 's1',
    title: 'A session',
    status: 'active',
    failureReason: null,
    archived: false,
    spawnSource,
    createdBy: null,
    createdAt: '2026-09-29T10:00:00Z',
    updatedAt: '2026-09-29T10:00:00Z',
    repos: [],
    sandboxStatus: null,
    buildModelId: null,
    buildEffort: null,
  }
}

function header(spawnSource: Session['spawnSource']): string {
  return renderToStaticMarkup(<SessionHeader session={session(spawnSource)} model={buildTimelineModel([])} cost={buildCostRollup([])} participants={[]} />)
}

describe('SourceIcon -- an unknown source', () => {
  it('draws the neutral glyph with a neutral tooltip', () => {
    const html = renderToStaticMarkup(<SourceIcon source={UNKNOWN_SOURCE} />)
    expect(html).toContain('title="Started from another source"')
    expect(html).toContain('<svg')
  })

  it('never borrows a known source\'s glyph', () => {
    const unknown = renderToStaticMarkup(<SourceIcon source={UNKNOWN_SOURCE} />)
    for (const known of ['web', 'slack', 'linear', 'github', 'mcp'] as const) {
      const html = renderToStaticMarkup(<SourceIcon source={known} />)
      expect(html.replace(/title="[^"]*"/, '')).not.toBe(unknown.replace(/title="[^"]*"/, ''))
    }
  })

  it('keeps a known source\'s tooltip unchanged', () => {
    expect(renderToStaticMarkup(<SourceIcon source="slack" />)).toContain('title="Started from Slack"')
  })
})

describe('SessionHeader -- an unknown source', () => {
  it('shows a neutral source tag, never an empty one', () => {
    const html = header(UNKNOWN_SOURCE)
    expect(html).toMatch(/<span class="srctag">.*<\/svg><\/span>other<\/span>/)
  })

  it('keeps a known source\'s tag unchanged', () => {
    expect(header('github')).toMatch(/<span class="srctag">.*<\/svg><\/span>GitHub<\/span>/)
  })
})

describe('the mcp source', () => {
  it('draws its own glyph with its own tooltip, never the neutral one', () => {
    const html = renderToStaticMarkup(<SourceIcon source="mcp" />)
    expect(html).toContain('title="Started from an MCP client"')
    expect(html).toContain('<svg')
    const unknown = renderToStaticMarkup(<SourceIcon source={UNKNOWN_SOURCE} />)
    expect(html.replace(/title="[^"]*"/, '')).not.toBe(unknown.replace(/title="[^"]*"/, ''))
  })

  it('shows MCP as the session header tag, never other', () => {
    expect(header('mcp')).toMatch(/<span class="srctag">.*<\/svg><\/span>MCP<\/span>/)
  })
})
